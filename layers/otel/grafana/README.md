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
