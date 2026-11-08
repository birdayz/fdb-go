# FoundationDB Grafana datasource

Grafana backend plugin `birdayz-foundationdb-datasource` for FDB relational
schemas and OTEL_V1 stores. Design: `layers/otel/rfcs/grafana-datasource.md`.

This directory is a nested Go module (`fdb.dev/layers/otel/grafana`,
`replace fdb.dev => ../../..`) so the plugin SDK's dependencies stay out of the
root module and Bazel (`.bazelignore`, `# gazelle:exclude`).

## Build and test

```sh
just grafana-build   # mage + SDK targets → dist/gpx_foundationdb_{linux_amd64,linux_arm64,darwin_arm64}
just grafana-test    # go vet + go test inside this module only
```

`grafana-build` reads the executable name from `src/plugin.json` (frontend).

## Backend layout (`pkg/`)

- `main.go`: selects FDB API version 730 once, then `datasource.Manage`.
- `plugin/settings.go`: jsonData/secureJsonData parsing and validation.
- `plugin/fdb.go`: one FDB database per instance (cluster file or string,
  TLS with libfdb_c-style peer verification, PDC dialer).
- `plugin/datasource.go`: instance lifecycle, `QueryTypeMux`, tenants.
- `plugin/sql.go`, `macros.go`, `converters.go`: sqlds driver over read-only
  per-tenant pools (LRU of 32), macros in integer time units, converters.
- `plugin/trace.go`, `search.go`, `logs.go`, `links.go`: OTel frames.
- `plugin/resources.go`: tenants, attribute-keys, attribute-values, completion.
- `plugin/health.go`: open cluster, `SELECT 1`, OTel store layout.
- `plugin/metrics.go`: RFC §9 counters `fdb_datasource_*` on the SDK's
  Prometheus registry, labelled by datasource UID, query type and tenant:
  SQL scanned rows and bytes, pages and failures by SQLSTATE (every statement
  on the tenant's pool, health and completion included); OTel candidates,
  read and decoded bytes, trace truncations by reason, reader re-opens.

## Development stack and end-to-end tests

`docker-compose.yaml` runs Grafana (13.1.9; `GRAFANA_VERSION` overrides) with
`dist/` mounted, a single-node FDB 7.3.77 on tmpfs, `fdb-init` and a `seed` job
that writes the demo run `/OTEL/R_GRAFANA_DEV` through `otel.Exporter`
(`cmd/seed`: T0 with GenAI messages as deltas, T1 full-text, `SEED_TRACES`
traces each, default 200, spread over the last 6 hours; spans with events,
links and error statuses, typed attributes, GenAI messages, correlated logs).
`provisioning/` adds the datasource (cluster string `docker:docker@fdb:4500`, a
hostname coordinator the Go client and the plugin resolve) and the load dashboard.

```sh
just grafana-build && npm run build && CGO_ENABLED=0 go build -o bin/seed ./cmd/seed
docker compose up --build        # Grafana on :3000
tests/e2e.sh                     # Playwright against 13.1.9, then 12.0.0
```

`tests/e2e.sh` brings the stack up per Grafana version and runs the
`@grafana/plugin-e2e` suite in the pinned Playwright image (`PLAYWRIGHT=host`
uses the local install): Save & test, Explore SQL table, time series and typed
SQL, the read-only refusal, a trace by ID in TraceView, a search opening a
trace, span → logs and log → trace links, and the query and config editors.

## Load (R1)

`tests/load/run-load.mjs` keeps the provisioned dashboard `fdb-load` open in one
headless Chromium for 5 minutes after a 60 s idle baseline: 20 panels over the
last 6 hours, 14 SQL (12 time series with `$__timeGroup`, 2 tables) and 6 trace
searches (attribute, time, same-span, existential; T0 and T1). It samples the
plugin process, Grafana and fdbserver from `/proc` every 5 s, and FDB's
cumulative workload counters (`status json`) every 15 s.

Setup: the compose stack with `SEED_TRACES=5000` (20,000 spans and 10,714 logs
per tenant, two tenants), Grafana 13.1.9 at debug log level, FDB 7.3.77 one
process, memory engine on tmpfs, the plugin restarted before each run. Host: 24
cores, 62 GiB, shared with other work (1-minute load average 12 to 24 during the
runs), so latencies are upper bounds. One panel at a time on an idle plugin
takes 1.3 s median, 2.7 s max: every SQL time series scans every span in range.
Raw summaries: `tests/load/results/r1-{5s,30s}.json`.

| | 5 s refresh | 30 s refresh |
|---|---|---|
| Panel requests per minute | 236 | 40 |
| Completed / aborted by the next refresh | 725 / 456 | 200 / 0 |
| Completed request latency p50 / p95 / max | 1.4 / 3.7 / 4.7 s | 1.8 / 9.1 / 14.0 s |
| Plugin CPU, mean / p95 (100% = one core) | 718% / 842% | 167% / 653% |
| Plugin RSS, start → max (end) | 77 → 1,200 MiB (830) | 81 → 1,187 MiB (701) |
| Grafana server CPU mean, RSS max | 3.5%, 1,126 MiB | 1.4%, 703 MiB |
| fdbserver CPU mean / p95, RSS | 52% / 61%, 437 MiB | 14% / 48%, 437 MiB |
| FDB reads/s, keys read/s, MiB read/s | 33,868, 68,607, 18.6 | 7,009, 14,230, 3.9 |
| FDB transactions started/s | 24.9 | 17.1 |
| FDB performance limited by | workload | workload |

Idle (dashboard closed): plugin 0.2–0.3% CPU at 77 MiB RSS, fdbserver 1% CPU.
At a 5 s refresh this data size saturates the plugin: a refresh takes longer
than the interval, so 39% of the requests are cancelled by the next one (the
plugin logs `context canceled`; Grafana logs 499). The plugin's CPU is the
limit, not FDB: FDB reports `workload` as its limiter and fdbserver uses half a
core while the plugin uses seven. No tuning was applied.
