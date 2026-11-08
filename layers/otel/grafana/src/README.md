# FoundationDB datasource

Reads FoundationDB relational stores and OTEL_V1 trace and log stores straight
from the cluster: the plugin's backend embeds a pure-Go FoundationDB client, so
no server sits between Grafana and FDB. It never writes.

## Query types

- **SQL** over the tenant's schema, as a time series, table or logs. Macros
  work in integer nanoseconds: `$__timeFilter(col[, unit])`, `$__timeFrom()`,
  `$__timeTo()`, `$__timeGroup(col, interval)`, `$__timeGroupAlias(...)`,
  `$__time(col)`, `$__unixEpochNano{Filter,From,To}` and `$__hex(v)`. The
  editor completes macros, tables and columns, shows the executed SQL and runs
  `EXPLAIN`.
- **Trace** by ID, shown in the trace view.
- **Search** for traces with typed predicates `{scope, key, type, value}`:
  every predicate on one span (_same span_), each matched somewhere in the
  trace, logs included (_existential_), the newest traces with one attribute,
  or the newest traces in the time range. Keys and values autocomplete.
- **Logs** of a trace (optionally one span), or the newest logs with one attribute.

Each query names a tenant (schema); empty means the datasource default.
Template variables can list tenants, attribute keys, attribute values or the
first column of a SQL query.

## Configuration

- Cluster file path (default), or a cluster string for containers.
- TLS: off; as the cluster file and the `FDB_TLS_*` environment say; or
  certificates from the datasource settings.
- One database per datasource, a default tenant and an allow-list of tenants
  (names, or one prefix such as `T*`).
- Budgets for query time, transactions, SQL pages and rows, traces, search and
  logs, and FDB transaction tags so an operator can throttle Grafana's reads.

The plugin is unsigned: allow it with `allow_loading_unsigned_plugins`.
