# Grafana datasource for FDB relational and OTEL_V1 stores

**Status:** draft, nothing implemented. **Base:** PR #1035. Decisions in §12
are proposals; say no to the ones you don't want.

## 1. What we build

One Grafana **backend** plugin, in Go, that talks to FoundationDB directly:
pure-Go client, `CGO_ENABLED=0`, no libfdb_c, no server in between. Two paths
share one FDB client per datasource instance:

- **SQL.** Our `database/sql` driver (`fdbsql`) under `grafana/sqlds/v4`.
  Tables, time series and logs over any relational schema store.
- **OTel.** A new read-only `otel.Reader`. Trace by ID into Grafana's
  TraceView, trace search over the typed API we already have, and logs.

No TraceQL, no Tempo API. If someone needs Grafana's Traces Drilldown app,
that is a separate server later (P6).

## 2. What we don't build, and why

- **Postgres wire protocol + Grafana's Postgres datasource.** Our dialect is
  not Postgres, its macros emit Postgres SQL, and pg-wire is a project of its
  own. No.
- **Traces through SQL.** The span projections are incomplete by design
  (events and links drop bytes and long-string attributes and the dropped
  counts; docs/compatibility.md, writer contract). The exact data is the OTLP
  envelope plus, for GenAI layouts, message reconstruction. Both are Go code.
  ClickHouse builds trace frames in SQL with `arrayMap` over maps
  (`clickhouse-datasource` `sqlGenerator.ts`); we can't. No.
- **The Exporter as the reader.** It ensures templates, databases and schemas
  and publishes `STORE_META`. A dashboard never writes. No.
- **The relational gRPC server** (`fdbsql://host:port`). Not implemented. No.
- **Tempo API first.** Needs a TraceQL parser (Tempo is AGPL, so we can't
  vendor theirs) and a server. Later, maybe.

## 3. Process and handlers

- Grafana starts the plugin binary (`gpx_<id>_<os>_<arch>`) and talks gRPC via
  `hashicorp/go-plugin`. One process, one instance per datasource
  (`datasource.Manage`). A settings change replaces the instance; the old one's
  `Dispose` closes its `*sql.DB` pools and FDB client.
- `fdb.APIVersion(730)` once, in `main`.
- `QueryData` is a `QueryTypeMux`: `sql` → sqlds; `trace`, `traceSearch`,
  `logs` → ours. `CheckHealth`: open the cluster, `SELECT 1`, and in OTel mode
  read `STORE_META("layout")`; report the step and the FDB/SQLSTATE code that
  failed. `CallResource`: sqlds' completion routes plus `/tenants`,
  `/attribute-keys`, `/attribute-values`.

## 4. Settings

| Field | Notes |
|---|---|
| `clusterFilePath` | Default. The client writes coordinator changes back to it. |
| `clusterString` | For containers and Cloud. Coordinator changes stay in memory, so it goes stale when every coordinator it names has moved. |
| `tlsMode`, CA, `tlsVerifyPeers`; cert and key in `secureJsonData` | FDB certs rarely carry IP SANs, so: `InsecureSkipVerify` plus `VerifyConnection` from the verify-peers policy, as `client.WithTLSConfig` documents for libfdb_c parity. |
| `enableSecureSocksProxy` | PDC dialer passed as `fdb.WithDialFunc`. |
| `database` | One relational database per datasource; OTel: `/OTEL/R_<RUN>`. |
| `defaultSchema`, `allowedSchemas` | Tenant default and allow-list (names or one prefix like `T*`). Outside it: refused before any read. |
| `otelMode` | `auto` (OTel query types if the schema has OTEL_V1's tables), `on`, `off`. |
| budgets, `transactionTags` | §9. Tags let an operator throttle Grafana's reads in FDB. |

The tenant is the schema (`T<n>`), picked per query from a template variable
fed by `/tenants`.

## 5. Read-only

A dashboard must not write. Today:

- sqlds only calls `QueryContext`, and the driver refuses update plans there
  (INSERT, UPDATE, DELETE, DDL, transaction statements) with 02F01 before
  executing them, and multi-statement batches with 0A000.
- `isolation_level_snapshot=true` (DSN) refuses everything but SELECT and
  EXPLAIN of a SELECT at planning (`planOne`, `snapshotAdmits`), on the parse
  tree, not the text.
- Hole: an auto-commit SELECT commits its shared transaction, so a store-header
  upgrade done by the open is persisted. No SQL-level test pins the snapshot
  refusal either.

P0 adds `read_only=true`: the same refusal, cancel instead of commit for every
transaction of the connection, and `BeginTx(TxOptions{ReadOnly: true})`
accepted because it is now enforced. The `otel.Reader` creates nothing,
publishes nothing, builds stores with `SetSkipPossiblyRebuild(true)`, and
cancels every transaction.

Tenant isolation is Grafana's datasource permissions and nothing else; FDB has
no per-schema ACL. Answered (`TestReadOnly_SelectReadsOnlyItsOwnSchemasRows`):
a SELECT on a connection bound to `T1` cannot read `T2`'s rows or another
database's (42703), but `INFORMATION_SCHEMA` lists every schema, table and
column of the database. So `allowedSchemas` holds for rows as long as the
plugin, never the SQL, picks the schema, and the plugin filters the tenant and
completion lists by it. Tenant names are not secret from a datasource's users.

## 6. SQL

Macros follow the Postgres datasource's names but work in integer
nanoseconds, OTEL_V1's unit (`start_ns`, `effective_ns`, ...):

| Macro | Expands to |
|---|---|
| `$__timeFilter(col[, unit])` | `col >= <from> AND col < <to>` (`ns` default; `us`, `ms`, `s`) |
| `$__timeFrom([unit])`, `$__timeTo([unit])` | integer literal |
| `$__timeGroup(col, interval[, unit])` | `(col / step) * step` |
| `$__timeGroupAlias(...)`, `$__time(col[, unit])` | same, in ns, `AS "time"` |
| `$__unixEpochNano{Filter,From,To}` | aliases, for Postgres muscle memory |
| `$__hex(v)` | `X'<v>'` after checking even-length hex |

`col` must be a (dotted) identifier; units and intervals are validated. A macro
never splices free text.

Converters: a BIGINT column named `time`/`timestamp` becomes `time.Time` (so
long-to-wide works); BYTES becomes lowercase hex (capped at 4 KiB); ARRAY and
STRUCT become JSON. Without the BYTES converter, identifiers render as binary
garbage.

Facts to live with:

- `GROUP BY` on an expression is Go-only; Java 4.14.2.0 refuses it over
  unordered input. These dashboards don't port to Java.
- A time series over raw spans scans every row in range; there are no rollups.
  §9's budgets bound it, and the editor shows the SQL and `EXPLAIN`.
- Completion uses `INFORMATION_SCHEMA` (a Go extension, scoped to the
  connection's database).
- `sqldriver.NewConnector(db, dsn)` (P0) hands the driver the instance's FDB
  database. The plugin does not use `RegisterBackend`, the process-global test
  seam.

## 7. Traces and logs

```go
func OpenReader(ctx context.Context, db *recordlayer.FDBDatabase, database, schema string, cfg ReaderConfig) (*Reader, error)
func (r *Reader) TraceOTLP(ctx context.Context, traceID string, b TraceBudget) ([]*tracepb.ResourceSpans, QueryResult, error)
func (r *Reader) QuerySameSpan(ctx context.Context, q SameSpanQuery) (SpanQueryPage, error)
func (r *Reader) QueryTraceExistential(ctx context.Context, q TraceExistentialQuery) (TraceExistentialResult, error)
func (r *Reader) Query(ctx context.Context, a Attribute) (QueryResult, error)
func (r *Reader) Time(ctx context.Context, tr TimeRange) (QueryResult, error)
func (r *Reader) QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]LogRecord, error)
func (r *Reader) QueryLogsByAttribute(ctx context.Context, a Attribute, limit int) ([]LogRecord, error)
func (r *Reader) AttributeKeys(ctx context.Context, scope, typ, prefix string, limit int) ([]string, bool, error)
func (r *Reader) AttributeValues(ctx context.Context, a Attribute, prefix string, limit int) ([]string, bool, error)
```

All of this exists today, spread over `SpanReader`, the Exporter and the log
and message stores. The facade puts it behind an open that writes nothing
(`OpenTenantStoreCompiled`, then `STORE_META("layout")`, `ownership` error if
absent). `TraceOTLP` replaces one `LoadSpanOTLP` per span with one scan of the
trace's SPANS rows. Reads use the GRV cache (≤100 ms old); the plugin never
writes, so that only means a span committed in the last 100 ms can be missing.

**Trace frame.** TraceView reads one frame per trace with
`preferredVisualisationType = "trace"`. We emit exactly Tempo's field set
(`grafana-tempo-datasource` `trace_transform.go`): `traceID`, `spanID`,
`parentSpanID`, `operationName`, `serviceName`, `serviceNamespace`, `kind`,
`statusCode` (2 = error), `statusMessage`, `instrumentationLibrary{Name,Version}`,
`traceState`, `serviceTags`, `startTime`/`duration` (ms), `logs` (events),
`references` (links), `tags`. Two deviations: bytes values show as base64
(Tempo shows `""`), and dropped counts and schema URLs show as `otel.*` tags
instead of vanishing. GenAI messages are restored under a 64 MiB trace budget;
past it, manifests plus a frame notice.

**Search.** Typed predicates `{scope, key, type, value}`, string/int/double/bool,
canonical values; bytes and JSON are not queryable. Modes: `sameSpan` (all
predicates on one span; predicate zero drives the scan), `existential` (span,
log and message clauses each matched somewhere in the trace), `attribute`
(newest K by one attribute), `time` (newest K in range). The result follows
Tempo's search frame. Root service, name and duration need each hit's rows
(no `parent_span_id` index), so they're on for ≤20 hits. No regex, ranges or
negation: the API doesn't have them, and adding them is engine work.

**Logs.** Grafana's `log-lines` frame: `timestamp` (effective time), `body`
(string, else JSON), `severity` (text, else from the OTLP number ranges), `id`
(`event_id`), `labels`, plus `traceId`/`spanId`. SQL can produce the same
frame with `format = logs`.

**Links.** Grafana's built-in trace-to-logs only knows Loki, Splunk, Elastic
and a few others; a third-party datasource gets nothing. It doesn't matter:
TraceView turns any field-config link on the trace frame into a span link. So
`spanID` carries internal links to our own `logs` query (per span and per
trace), log rows link back to `trace`, and a SQL result's `trace_id` column
gets the same link through sqlds' `ResponseMutator`.

## 8. Attribute-key autocomplete

There is no key catalog. Keys live only in the fan-out attribute indexes,
keyed `(scope, attr_key, value, ...)`. `AttributeKeys` skip-scans: first entry
at or after `pack(scope, last_key)+1`, limit 1, repeat. One range read per
distinct key per scope and type, run in parallel, capped (1,000 keys, 2 s),
cached 5 minutes per tenant. Values the same way under `(scope, key)`, capped
at 100. Long strings (digest-indexed) are not offered.

When configurable attribute indexes land (migration RFC §8):

- their index options name scope, key and type, so the metadata *is* the
  catalog: no data read;
- D12: the plugin never evolves a template, opens stores above template
  version 1, and re-resolves on `StaleMetaDataVersionError`;
- D13: keep Go honouring `allowedForQuery=false`; the plugin's SQL depends on
  it;
- D15: keep `index_all_attributes: true`; with `false`, search and autocomplete
  cover only configured keys.

A key-catalog table written by the Exporter would make autocomplete one range
read, at the cost of a write per new key and a template change. Not worth it.

## 9. Budgets and pooling

| Limit | Default |
|---|---|
| Query timeout | 30 s (sqlds deadline; same for trace and log calls) |
| FDB transaction timeout | 5 s |
| SQL page | 10,000 scanned rows / 16 MiB (`execution_scanned_*_limit`) |
| SQL rows | 100,000 |
| SQL statement memory | 256 MiB (`max_statement_memory_bytes`) |
| Trace | 10,000 spans / 64 MiB; partial result plus notice |
| Search | K = 20 (max 1,000); 20,000 candidates; 4 MiB read; typed budget error, never a silently truncated answer |
| Logs | 1,000 rows |

sqlds keeps one `*sql.DB` per tenant (8 open, 4 idle) and never evicts, so
we put an LRU over pools (32 schemas). A pooled connection pays the
schema-witness check on checkout: 0.82 ms vs 0.59 ms p50 pinned for a point
query (benchmark doc, round 3). sqlds can't pin; we eat it. SQL takes a fresh
read version per statement; no GRV cache for SQL in v1.

Dashboards auto-refresh. Transaction tags plus FDB tag throttling, the
budgets above, and Prometheus counters (scanned rows, pages, SQLSTATE,
candidates, bytes) are the defence.

## 10. Build, ship, test

**Nested Go module** at `layers/otel/grafana` (`replace fdb.dev => ../../..`).
The sdk and sqlds pull ~250 modules, 156 of them new, and through MVS would
bump otel to v1.47.0 and protobuf to v1.36.12 for every package in the root
module and in Bazel's `go_deps`. No. The root-module pieces (`otel.Reader`,
`sqldriver.NewConnector`, `read_only`) stay root-module code under `just test`.

Backend: mage with the sdk's targets, linux amd64/arm64. Frontend:
`@grafana/create-plugin` layout, `@grafana/*` 13.1.x, Node ≥ 24, npm (no
rules_js here, so it stays out of Bazel). Recipes: `just grafana-build`,
`grafana-test`, `grafana-e2e`. `plugin.json`: backend, metrics, logs, tracing,
alerting; Grafana ≥ 12.0.

Ship unsigned for self-hosted (`allow_loading_unsigned_plugins`). Private
signature when a fixed deployment wants it (~1 day). Grafana Cloud means
catalog review plus PDC to reach FDB; that's a later call.

Tests:

- **Root module:** `Reader` opens and writes nothing; `TraceOTLP` equals
  `LoadSpanOTLP` per span (message layouts included); budgets; skip scan
  against a seeded store, plus an empty-store vacuity check. `read_only`
  refuses DML/DDL/SHOW and batches, admits SELECT/EXPLAIN, persists no header
  upgrade; `TxOptions{ReadOnly}` only with it. Pin the existing
  `isolation_level_snapshot` refusal too.
- **Plugin Go:** macros (every unit, malformed input, injection attempts),
  converters, a golden trace frame covering every OTLP field against Tempo's
  field list, log frame, links, settings and TLS parsing.
- **Plugin + FDB:** seed with `otel.Exporter` (events, links, status, logs,
  both message layouts), call `QueryData` directly for every query type,
  check frames, links, notices, tenant switching and the read-only refusal.
- **E2E:** compose with FDB and Grafana 13.1.x plus the oldest supported
  version; Playwright: save & test, SQL table and time series, a trace in
  TraceView, span → logs, log → trace.

Every new test gets mutation evidence before commit.

## 11. Plan

| Phase | What | Days |
|---|---|---|
| P0 | `otel.Reader`, `sqldriver.NewConnector`, `read_only`, tests | 5–7 |
| P1 | plugin skeleton, settings, FDB lifecycle (path/string/TLS/PDC), health, sqlds driver, macros, converters, tenants and pools, completion | 6–8 |
| P2 | trace frame, search table, log frame, links, stale-metadata retry, FDB integration tests | 6–8 |
| P3 | config and query editors, autocomplete, variables | 7–10 |
| P4 | builds, provisioning, compose, Playwright, CI, docs | 4–6 |
| P5 (opt.) | signing, PDC verification, catalog submission | 2–3 + review |
| P6 (opt.) | Tempo-compatible server with a TraceQL subset (`{ span.k = v && ... }`) | 15–25 |

P0–P4: 28–39 engineer-days. P0 can start now; P1 and P2 need it; P3 needs
P1's query model.

Risks, short:

- **R1** The planner, hash aggregation and sorts run inside Grafana's host.
  Memory and page bounds, process metrics, a 20-panel load check.
- **R2** Dashboard load on FDB. Tags, throttling, budgets; rollups are a
  metrics-tables problem, not this one.
- **R3** Cross-schema reads (§5): rows are isolated, names are not. Filter the
  lists; if tenant names must be secret, one datasource per tenant.
- **R4** sdk and Grafana churn. Pin versions; e2e on two Grafana versions.
  TraceView's and the log frame's field sets are the stable contracts.
- **R5** Go-only SQL in dashboards (§6). Documented; don't pretend otherwise.
- **R6** Users expecting Tempo search (regex, ranges, TraceQL). The editor
  offers what the API answers. P6 is the answer if it matters.

## 12. Decisions

| ID | Proposal |
|---|---|
| GD1 | ID `birdayz-foundationdb-datasource`, "FoundationDB", Apache-2.0, Grafana ≥ 12.0. Not OTel-branded: it serves plain relational stores too. |
| GD2 | Nested module, mage + npm, `just grafana-*`. Not the root module. |
| GD3 | sqlds/v4. Our own `database/sql` loop (~300 lines) would pin connections, but then we own frames, long-to-wide and retries. |
| GD4 | `read_only` DSN option; `isolation_level_snapshot=true` until it lands. |
| GD5 | Cluster-file path (default) or cluster string; TLS from settings with libfdb_c-style peer checks; optional PDC. |
| GD6 | One datasource per database, tenant per query, `allowedSchemas`. One per tenant only if tenant names must be secret (R3). |
| GD7 | `otel.Reader`. Not SQL, not the Exporter (§2). |
| GD8 | Typed predicates only; no TraceQL in v1. |
| GD9 | Restore GenAI messages under the 64 MiB trace budget; manifests plus notice past it. |
| GD10 | Skip-scan autocomplete with caps and cache; configured indexes as the catalog later; no key-catalog table. |
| GD11 | §9's budgets. |
| GD12 | Unsigned self-hosted first; signing and Cloud later. |
| GD13 | Tempo API: P6, only if Drilldown or existing Tempo dashboards are needed. |
| GD14 | No SQL GRV cache in v1. |
| GD15 | Lives in this repository until the root APIs it needs are released. |

**Sources read:** `grafana-plugin-sdk-go` v0.298.0, `sqlds/v4` v4.2.7,
`grafana/grafana` `03106fa29ab5`, `grafana-tempo-datasource` `daf10cae2285`,
`clickhouse-datasource` v4.23.4, `grafana-postgresql-datasource`
`8273a799d095`. Grafana core no longer ships Tempo, Jaeger or Zipkin; their
frame code lives in the external plugins.
