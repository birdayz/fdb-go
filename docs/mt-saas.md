# Running this engine as a multi-tenant SaaS

Operator guide for the deployment shape where **one SQL database path is one tenant** and every
tenant's SQL connection is the trust boundary. It is the companion to
[`docs/operations.md`](operations.md), which covers the single-deployment basics — connecting,
transaction limits, the online-index lifecycle, schema evolution, backup, observability hooks. This
page does not repeat those; it links to them and covers only what changes when there are N tenants
on one cluster.

Status claims about the codebase as a whole live in [`road-to-prod.md`](../road-to-prod.md), which
is the authority. This page is scoped to the multi-tenant question and carries a `file:line`
citation for every claim, because an operator guide is worthless unless every sentence in it is
checkable.

**Verified against:** Java `fdb-record-layer-core` **4.12.11.0** (the wire-compat spec), the
FoundationDB **7.3.77** client protocol, Go **1.26.x**. These are drift-guarded — this page is on
`pkg/docscheck`'s `livingDocs` list, so a version bump that leaves it behind fails the build.

**Read this first.** Three properties of this deployment shape are not negotiable and are not
enforced by the engine:

1. The application owns authentication, authorization, and the tenant → DSN mapping. There is no
   `GRANT`, no user model, no role — none of that exists in the grammar
   (`pkg/relational/core/parser/grammar/RelationalParser.g4:57-81`).
2. Untrusted SQL must never reach the driver. The scoping controls below are defense-in-depth on
   top of that rule, not a substitute for it.
3. Every per-statement quota ships effectively unlimited, and none of them is settable from a DSN.
   Arming them takes code (§3). A tenant fleet run without that code has no per-statement bound
   other than FDB's own 5-second transaction limit.

---

## 1. Tenancy model

### One database path per tenant, one subspace per database

A tenant is a SQL database path. The DSN is the ordinary one from
[`operations.md` §1](operations.md#1-connecting-to-a-cluster):

```
fdbsql:///t/<tenant-id>?cluster_file=/etc/foundationdb/fdb.cluster&schema=MAIN
```

`/t/<tenant-id>` is **a convention this guide recommends, not a driver feature.** `ParseDSN`
accepts any non-empty path and attaches no meaning to any segment
(`pkg/relational/sqldriver/dsn.go:120`). An empty path or a bare `/` is rejected at
`sql.Open` time (`pkg/relational/sqldriver/dsn.go:136`), which matters because several fail-closed
guards below key on "the session has a usable database path".

`schema=` has **no default**. Omit it and the session schema stays empty and metadata loading is a
silent no-op (`pkg/relational/sqldriver/driver.go:174`,
`pkg/relational/core/embedded/connection.go:507`). Always pass it. `MAIN` is only a convention.

The physical layout is one subspace per (database, schema) pair, and the prefix is the *plain tuple
encoding of the two strings*:

```go
// pkg/relational/core/keyspace/keyspace.go:50-58
func (k *RelationalKeyspace) SchemaSubspace(dbPath, schemaName string) (subspace.Subspace, error) {
	...
	return k.root.Sub(tuple.Tuple{dbPath, schemaName}), nil
}
```

The root is the empty subspace (`pkg/relational/sqldriver/driver.go:231`), so a tenant's key prefix
is literally `tuple.Pack(dbPath, schemaName)`.

**Keep tenant IDs short.** There is no directory layer and no integer interning — the decision is
explicit at `pkg/relational/core/keyspace/keyspace.go:9-13` ("uses plain tuple keys instead of
`DirectoryLayerDirectory`"). The tenant ID string is therefore embedded **verbatim in every single
key** of that tenant's records, index entries and record versions, forever, and the path is not
renameable. A 40-character tenant ID costs 40 bytes on every key in the tenant's data set.

Nothing in the layer bounds that. `ParseDBPath` validates structure only — leading `/`, no empty
segments (`pkg/relational/core/keyspace/keyspace.go:63-76`) — and the only length enforcement is
FDB's 10 000-byte key limit, applied **at commit**, not at write time
(`pkg/fdbgo/client/sizelimits.go:17`, `:10-12`). An absurd tenant ID therefore fails late, as a
commit error that does not name the path. Cap the length in your provisioning code.

### Why native FDB tenants are not used

FoundationDB has a native tenant feature and this repo implements a large part of it. It is still
the wrong tool for v1, for three independent reasons:

1. **The SQL layer cannot open one.** `Connector.Connect` goes `fdbclient.Open(...)` →
   `recordlayer.NewFDBDatabaseWithBackend(...)` (`pkg/relational/sqldriver/driver.go:211-215`).
   There is no DSN parameter and no code path. The record layer *can*
   (`pkg/recordlayer/database.go:195`, `NewFDBDatabaseFromTenant`, proven by
   `pkg/recordlayer/tenant_isolation_test.go:39-50`), but nothing outside tests calls it, and
   `database/sql` cannot reach it at all.
2. **The pure-Go tenant API is unauthenticated.** Tenant handles exist
   (`pkg/fdbgo/fdb/database.go:450`, `:473`, `:479`, `:491`, `:502`), but the authorization token
   option is a hard error, deliberately:

   ```go
   // pkg/fdbgo/fdb/options.go:389-392
   func (o goTransactionOptions) SetAuthorizationToken(_ string) error {
   	// Fails unsafe if ignored: the request would be sent UNAUTHENTICATED (auth
   	// bypass / wrong tenant scoping). The most dangerous silent no-op of the set.
   	return &UnsupportedOptionError{Option: "authorization_token"}
   }
   ```

   Pure-Go tenants therefore work only against a cluster with tenant authorization disabled. They
   are a namespacing mechanism there, not a security boundary.
3. **Adopting them forfeits the `libfdb_c` escape hatch.** The cgo backend deliberately carries no
   tenant operations — `pkg/fdbgo/fdb/backend.go:35-36`: "It deliberately still does NOT include
   tenant ops: those return concrete pure-Go handles a cgo backend cannot build and remain
   pure-Go-only in v1." There are zero tenant symbols under `pkg/fdbgo/libfdbc/`.

The exact state, stated plainly because the two halves are easy to conflate: **the pure-Go client
has tenants but no authorization; the `libfdb_c` client has authorization
(`pkg/fdbgo/libfdbc/options.go:105-106`) but no tenants.** Neither backend gives you authenticated
native tenants today. Subspace-per-database gives you isolation on both backends, which is why v1
uses it.

### Tenant enumeration is the application's job

The record layer cannot enumerate stores. There is no `ListStores`; a store is addressed only by a
caller-supplied subspace (`NewStoreBuilder().SetSubspace(ss)`). If a store is created outside the
SQL catalog, nothing in the codebase can find it again.

The SQL catalog *does* have `ListDatabases` (`pkg/relational/api/catalog.go:83`, FDB implementation
`pkg/relational/core/catalog/fdb_store_catalog.go:487`), but treat it as a debugging aid, not a
control-plane API: it covers only databases registered in the `/__SYS/CATALOG` store, and it
materialises the whole result in memory with the continuation argument discarded
(`fdb_store_catalog.go:484-486`). At a few thousand tenants that is a full catalog scan into a
slice on every call.

**Keep your own tenant registry.** The engine is not one.

---

## 2. Trust boundary

### What the application owns

Everything above SQL. There is no authentication, no authorization, no user or role model in the
engine: the grammar's complete statement set
(`pkg/relational/core/parser/grammar/RelationalParser.g4:41-49`, `:57-62`, `:74-81`) has no
`GRANT`, `REVOKE`, `CREATE USER` or `CREATE ROLE` — those words appear only in the reserved-keyword
list. `DatabaseMetaData.UserName` exists (`pkg/relational/api/database_metadata.go:76-77`) but is a
configured string that no decision consults. The code says so itself at
`pkg/relational/core/embedded/ddl.go:912`: "Java assumes authorization happens above the SQL
engine."

So: your service authenticates the caller, maps the caller to a tenant, and constructs the DSN.
Never let a tenant influence the database path, and never pass tenant-authored SQL to the driver.

### What the engine now enforces (post-#624)

**Catalog reads are session-scoped.** All four `INFORMATION_SCHEMA` views (`SCHEMATA`, `TABLES`,
`COLUMNS`, `INDEXES` — dispatched at `pkg/relational/core/embedded/system_tables.go:90-103`) route
through `listSessionSchemas`, which scopes to the connection's database and **fails closed** rather
than widening:

```go
// pkg/relational/core/embedded/system_tables.go:124-131
func (c *EmbeddedConnection) listSessionSchemas(txn api.Transaction) (api.ResultSet, error) {
	scope := scopePrefixOrEmpty(c.sess.DBPath)
	if scope == "" {
		return nil, api.NewError(api.ErrCodeInvalidPath,
			"connection has no usable database path; INFORMATION_SCHEMA cannot be scoped")
	}
	return c.sess.Catalog.ListSchemasInDatabase(txn, scope, nil)
}
```

A tenant's own `WHERE` clause was never a defence here and the code says why
(`system_tables.go:117-119`): row filtering runs *after* the rows are materialised, so before #624
the cluster had already been read by the time any predicate applied.

`SHOW DATABASES` is scoped the same way (`system_tables.go:411-431`), with an explicit
`WITH PREFIX` overriding it, read off the typed parse node rather than statement text. Matching is
at **path-segment granularity** — `/tenant-a` covers `/tenant-a/sub` but not `/tenant-abc`:

```go
// pkg/relational/core/embedded/system_tables.go:470-475
func databaseInPrefix(dbID, prefix string) bool {
	if scopePrefixOrEmpty(prefix) == "" {
		return false
	}
	return dbID == prefix || strings.HasPrefix(dbID, strings.TrimSuffix(prefix, "/")+"/")
}
```

Both `""` and `"/"` normalise to "no scope" (`scopePrefixOrEmpty`, `system_tables.go:447-452`), so
an unscopable session confers authority over nothing rather than over everything.

**`SHOW SCHEMA TEMPLATES` is deliberately still global**, and cannot be scoped without a catalog
wire-format change Java also reads — the reasoning is at `system_tables.go:520-533` and the
behaviour is pinned by `pkg/relational/sqldriver/mt_catalog_scope_fdb_test.go:262`. Templates are
cluster-global objects. Plan for tenants being able to see the *names* of every schema template on
the cluster, and do not encode anything sensitive in a template name.

### `RESTRICT_DDL_TO_SESSION_DATABASE` — set it on every tenant-facing connection

By default, DDL is **not** confined to the session's database: `parseSchemaIdentifier` accepts a
fully-qualified `/otherdb/SCHEMA` from any connection, matching Java, which assumes an authorization
layer above SQL. The Go-only connection option closes that for tenant-facing connections.

```go
// pkg/relational/api/options.go:148
OptRestrictDDLToSessionDatabase OptionName = "RESTRICT_DDL_TO_SESSION_DATABASE"
// pkg/relational/sqldriver/dsn.go:79
const RestrictDDLToSessionDatabaseParam = "restrict_ddl_to_session_database"
```

```go
db, err := sql.Open("fdbsql",
    "fdbsql:///t/"+tenantID+
        "?cluster_file=/etc/foundationdb/fdb.cluster"+
        "&schema=MAIN"+
        "&restrict_ddl_to_session_database=true")
```

What it covers, and both halves matter:

- **Cross-database DDL.** Enforced at the four resolution chokepoints, on the *already resolved*
  database path rather than on statement text: `CREATE DATABASE`
  (`pkg/relational/core/embedded/ddl.go:64`), `DROP DATABASE` (`:76`), `CREATE SCHEMA` (`:95`),
  `DROP SCHEMA` (`:124`), all through `checkDDLDatabaseScope` (`:920-932`). A violation is
  `api.CrossDatabaseDDLError` (`pkg/relational/api/ddl_scope_error.go:18`) at SQLSTATE **42501**
  (`ddl_scope_error.go:65`, code at `pkg/relational/api/errcode.go:91`). Scope is containment, not
  equality — a `/tenant-a` session may act on `/tenant-a/sub` (`ddl.go:916-919`).
- **Schema-template DDL.** `CREATE SCHEMA TEMPLATE` and `DROP SCHEMA TEMPLATE` are refused outright
  on a restricted connection (`ddl.go:136`, `:146` → `checkSchemaTemplateDDLAllowed`, `:956-961`),
  as `api.SchemaTemplateDDLRestrictedError` (`ddl_scope_error.go:45`), also 42501. Refusal rather
  than scoping is the only sound semantics: templates have no owning database, and a `DROP SCHEMA
  TEMPLATE` on a template another tenant's schemas were created from leaves those schemas
  unloadable.

Both are pinned end-to-end by `TestFDB_RestrictDDLToSessionDatabase`
(`pkg/relational/sqldriver/mt_ddl_scope_fdb_test.go:99`); the DSN-freeze paragraph below is pinned at
`:234` and the `/__SYS` refusal at `:294`.

Two details worth knowing before you deploy it:

- **A malformed boolean is an error, not "off".** `restrict_ddl_to_session_database=ture` fails at
  `sql.Open`/`OpenConnector` with SQLSTATE **22023** (`pkg/relational/sqldriver/dsn.go:103-113`),
  before FDB is touched (decode at `pkg/relational/sqldriver/driver.go:123-136`). A security flag
  that degrades silently on a typo is worse than none. The value is lower-cased and
  whitespace-trimmed first (`dsn.go:104`), so accepted true spellings are the bare flag, `1`, `t`,
  `true`, `yes`, `on` in any case; false is `0`, `f`, `false`, `no`, `off`; anything else is 22023.
- **The Connector's DSN is frozen.** `OpenConnector` deep-clones the parsed DSN before anything
  reads it (`driver.go:130`) and `DSN()` hands out a defensive copy (`driver.go:251`), so nothing
  outside the package can flip the restriction — or the `cluster_file` — between `OpenConnector` and
  the first `Connect`. To change a connection's configuration, open a new Connector.

There is no options API for this: `OpenConnector` takes a DSN string
(`pkg/relational/sqldriver/driver.go:123`) and the DSN is the only route through `database/sql`.
The option can also be set directly on a connection built with `embedded.New`
(`pkg/relational/core/embedded/connection.go:525`) via `SetOptions` (`:125`,
`api.NoOptions().With(api.OptRestrictDDLToSessionDatabase, true)`) — exported, but it bypasses
`database/sql` entirely, so treat the DSN as the operator surface.

**`CREATE DATABASE /__SYS/...` is refused unconditionally**, independent of the option
(`pkg/relational/core/ddl/create_database.go:38-41`), segment-granular so `/__SYSTEM_x` is still
allowed (`create_database.go:11-13`). Before #624 it succeeded, planting a database inside the
catalog's own keyspace.

### The honest summary

`RESTRICT_DDL_TO_SESSION_DATABASE` is defense-in-depth. It is not an authorization system: it has
one subject (the session's database path), no principals, no verbs, and it governs DDL only. DML
and SELECT are unaffected — a connection opened against `/t/a` reads and writes `/t/a` because that
is the path it was opened with, not because anything checked a permission. Untrusted SQL reaching
the driver is still a full compromise of that tenant's data. Keep the option on anyway; it turns a
class of application bugs from cross-tenant destruction into a 42501.

---

## 3. Quotas and fair-sharing

### The five per-statement limits, and their real defaults

| Option | Default | Default site | Scope |
|---|---|---|---|
| `api.OptMaxRows` (`MAX_ROWS`) | `math.MaxInt32` | `pkg/relational/api/options.go:198` | statement-wide |
| `api.OptExecutionScannedRowsLimit` | `math.MaxInt32` | `pkg/relational/api/options.go:213` | **per page** |
| `api.OptExecutionScannedBytesLimit` | `int64(math.MaxInt64)` | `pkg/relational/api/options.go:211` | **per page** |
| `api.OptExecutionTimeLimit` | `int64(0)` | `pkg/relational/api/options.go:212` | **per page** |
| `api.OptMaxStatementMemoryBytes` | absent from the defaults map → reads back `0` | `pkg/relational/api/options.go:43` | statement-wide |

Four of the five ship **effectively unlimited**. The exception is the time limit, and it runs the
other way: a 4-second per-page cap is armed unconditionally whether or not the option is set, and
the option can only *narrow* it, never raise it —

```go
// pkg/relational/core/embedded/cascades_generator.go:1775-1782
timeLimit := txPageTimeLimit                                            // 4s, :1256
if userMillis := optInt64(opts, api.OptExecutionTimeLimit, 0); userMillis > 0 {
	if ut := time.Duration(userMillis) * time.Millisecond; ut < timeLimit {
		timeLimit = ut
	}
}
```

The cap exists so FDB's 5-second hard wall is never reached (`cascades_generator.go:1252-1256`). It
is not a tenancy control: a statement that pages will spend 4 seconds *per page*, unbounded in
total, unless you also set a statement timeout.

What happens on breach differs per limit, and two of them do **not** produce an error:

- **`MAX_ROWS` truncates cleanly.** `io.EOF`, JDBC `setMaxRows` semantics, no error
  (`cascades_generator.go:1594-1600`). It is an egress cap, not a quota — the work is still done.
- **The scanned-rows / scanned-bytes limits paginate by default.** They only raise
  `ScanLimitReachedError` (SQLSTATE `54F01`, `pkg/relational/api/errcode.go:137`) when
  `SetFailOnScanLimitReached(true)` is also set (`pkg/relational/core/embedded/connection.go:88-91`).
  **Arming a scan limit without that flag is not a quota** — the statement silently pages on
  instead of failing, which is the opposite of what you wanted.
- **The time and memory limits do error.** Time → `54F01` (`cascades_generator.go:1848`); memory →
  `*recordlayer.MemoryLimitExceededError` (`pkg/recordlayer/execute_state.go:211`) mapped to
  `54F01` at `cascades_generator.go:2244-2247`.

### Setting them: not a DSN, not SQL — `conn.Raw`

**None of the five is settable from a DSN.** `ConnectionOptions()` decodes exactly one parameter,
`restrict_ddl_to_session_database` (`pkg/relational/sqldriver/dsn.go:86-96`); every other query
parameter is kept in the raw map and ignored (`dsn.go:84-85`). There is no `SET statement_timeout`
either, and the reason is recorded at `pkg/relational/core/embedded/connection.go:139-141` — the
grammar has no generic `SET <var> = <val>` rule. The SQL `OPTIONS(...)` clause carries an unrelated
set (`cascades_generator.go:772`).

The only route is `database/sql`'s escape hatch down to the driver connection:

```go
import (
	"fmt"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// configureTenantConn arms the per-statement governors on one pooled connection.
func configureTenantConn(driverConn any) error {
	ec, ok := driverConn.(*embedded.EmbeddedConnection)
	if !ok {
		return fmt.Errorf("not an embedded connection: %T", driverConn)
	}
	ec.SetOptions(api.NewOptionsBuilder().
		Set(api.OptMaxRows, int64(10_000)).                       // egress cap
		Set(api.OptExecutionScannedRowsLimit, int64(500_000)).    // per page
		Set(api.OptExecutionScannedBytesLimit, int64(64<<20)).    // per page
		Set(api.OptExecutionTimeLimit, int64(2_000)).             // millis, narrows the 4s cap
		Set(api.OptMaxStatementMemoryBytes, int64(256<<20)).      // statement-wide
		Build())
	ec.SetFailOnScanLimitReached(true) // without this the scan limits only paginate
	ec.SetStatementTimeout(30 * time.Second)
	ec.SetMaxResultBytes(32 << 20)
	return nil
}

conn, err := db.Conn(ctx)
if err != nil {
	return err
}
defer conn.Close()
if err := conn.Raw(configureTenantConn); err != nil {
	return err
}
// ... use conn (NOT db) for this tenant's statements ...
```

The setters are `SetOptions` (`connection.go:125`), `SetFailOnScanLimitReached` (`:132`),
`SetStatementTimeout` (`:143`), `SetMaxResultBytes` (`:154`).

**The footgun to design around: this configures ONE pooled connection.** `Connector.Connect`
installs only the DSN-decoded options (`pkg/relational/sqldriver/driver.go:173`), so a connection
the pool hands you later has *not* been through `configureTenantConn`. Route tenant work through a
helper that acquires a `*sql.Conn`, configures it, uses it, and closes it — do not issue queries on
the `*sql.DB` directly and assume the governors are on. `SetOptions` is also not safe to call
concurrently with execution on the same connection (`connection.go:121-123`).

### Statement timeout

`SetStatementTimeout` (`pkg/relational/core/embedded/connection.go:143`) takes a `time.Duration`,
defaults to **off** (`connection.go:96-102`), and bounds one whole `Execute` including all its
pages. Set it on every tenant connection: it is the only bound that spans pages, and without it the
4-second per-page cap composes into an unbounded statement. Breach surfaces as `54F01`,
distinguished from a caller's own context deadline by a cause tag
(`cascades_generator.go:1304`, `:2209`).

Note the semantics honestly: it is **per request, not per connection lifetime**. A continuation
resumed by a new request starts a fresh deadline (`connection.go:98-100`).

**Do not use `TRANSACTION_TIMEOUT`.** `api.OptTransactionTimeout`
(`pkg/relational/api/options.go:56`) is dead code — that declaration is its only occurrence in the
tree; nothing reads it. (It is unrelated to `ErrCodeTransactionTimeout` / `53F00`,
`pkg/relational/api/errcode.go:133`, which is live and is the mapping of FDB error 1031.) For
transaction-level bounds use the record-layer handle's `SetTransactionTimeout`, documented in
[`operations.md` §2](operations.md#2-transactions-retries-timeouts-cancellation).

### Concurrency: one `*sql.DB` per tenant

Give each tenant its own `*sql.DB` and cap it with `SetMaxOpenConns`. This is the **only** real
concurrency cap available today, and it is cheap: the driver keeps a process-global cache of
`*recordlayer.FDBDatabase` handles keyed by cluster-file path
(`pkg/relational/sqldriver/driver.go:73`, `:196`, doc at `:70-71`), so N tenant pools against one
cluster share **one** FDB handle, not N.

```go
db, err := sql.Open("fdbsql", tenantDSN)
if err != nil {
	return err
}
db.SetMaxOpenConns(8)   // the per-tenant concurrency cap
db.SetMaxIdleConns(2)
```

The cache is a `sync.Map` with no eviction (`driver.go:73`), bounded in practice by the number of
distinct cluster files — which is one. That is fine; do not read it as a per-tenant leak.

### Background work: `PriorityBatch`, record-layer only

FDB's batch GRV priority is available on the record-layer API —
`recordlayer.PriorityBatch` (`pkg/recordlayer/database.go:488`), applied via
`FDBRecordContext.SetTransactionPriority` (`:966-969`) or the runner config
(`pkg/recordlayer/runner.go:149`), landing as the GRV flag bit
`grvPriorityBatch` (`pkg/fdbgo/client/transaction.go:524`, `:3197-3198`).

**It is not reachable from the SQL layer** — there is no priority handling anywhere under
`pkg/relational/`. So: run tenant-facing SQL at default priority, and run bulk/background jobs
(reindexing, exports, GC) through the record-layer API with `PriorityBatch` set, so they yield to
interactive traffic under load.

### Transaction tags do not reach the wire — do not plan on server-side throttling

FoundationDB can throttle per transaction tag. **The pure-Go client cannot use it.**
`SetAutoThrottleTag` discards its argument (`pkg/fdbgo/fdb/options.go:238-240`), and while `SetTag`
does store the tag (`pkg/fdbgo/fdb/options.go:242` → `pkg/fdbgo/client/transaction.go:3182`), the
slice's only reader is client-local retry backoff (`transaction.go:3511`). Neither request carries
it: `CommitTransactionRequest.TagSet` is never assigned (`pkg/fdbgo/client/commitpath.go:448`), and
`buildGetReadVersionRequest` never sets `Tags` (`pkg/fdbgo/client/grv.go:1318-1325`). The wire
structs have the fields; they always serialize empty.

The `libfdb_c` backend does forward both (`pkg/fdbgo/libfdbc/options.go:62`), so this is a
divergence between the two builds, not a universal gap. On the default pure-Go build, **per-tenant
throttling must be implemented in your application** — `SetMaxOpenConns` plus the per-statement
governors above, not FDB tag throttling.

---

## 4. Observability

The hooks themselves are described in [`operations.md` §8](operations.md#8-observability-hooks).
What follows is only what a multi-tenant operator has to do differently.

### Retain the `*client.Database` handle at startup, or you get no metrics

`Metrics()` lives on `*client.Database` (`pkg/fdbgo/client/database.go:784`) and **only** there.
`fdb.Database` is a one-way wrapper — its `inner *client.Database` field is unexported with no
accessor (`pkg/fdbgo/fdb/database.go:94`, `:101`) — and `*recordlayer.FDBDatabase`
(`pkg/recordlayer/database.go:134`) has no `Metrics()` either. There is no way back down the stack.

So the handle has to be captured on the way up:

```go
cdb, err := client.OpenDatabase(ctx, clusterFile)   // *client.Database — keep this
if err != nil {
	return err
}
http.Handle("/metrics", fdbmetrics.Handler(cdb))    // Prometheus text exposition
rdb := recordlayer.NewFDBDatabase(fdb.WrapDatabase(cdb))
```

`fdbmetrics.Handler` (`pkg/fdbgo/fdbmetrics/fdbmetrics.go:36`) accepts any
`MetricsSource` (`fdbmetrics.go:29`) and pulls in no dependencies — it is deliberately not a
`prometheus.Collector` (`fdbmetrics.go:9-16`).

**The catch for a SQL-only deployment: a tenant service that uses only `database/sql` cannot get
client metrics at all.** The driver opens the handle itself
(`pkg/relational/sqldriver/driver.go:211`, `:215`) and stashes it in a package-private cache
(`driver.go:73`) with no getter. The one inversion is `sqldriver.RegisterBackend`
(`pkg/relational/sqldriver/driver.go:84`): build the `*client.Database` yourself, register the
wrapped record-layer database under a key, and open `fdbsql:///t/<id>?cluster_file=<key>`. Note its
doc frames it as a deterministic-simulation seam, not a metrics API — it works, but you are using
it off-label.

The same applies to record-layer counters. `StoreTimer` exists and mirrors Java's `FDBStoreTimer`
event set (`pkg/recordlayer/store_timer.go:99`, event set `:17-53`, `Snapshot()` at `:199`), but **nothing
exports it** — no production code calls `SetTimer` (`pkg/recordlayer/database.go:1023`), there is no
`fdbmetrics` equivalent for it, and the SQL path never surfaces an `FDBRecordContext` to install one
on. `fdbmetrics` covers client-level transaction counters only. Record-layer counters on the SQL
path are, today, not observable.

### Per-tenant plan logging

`PlanGenerationLogger` (`pkg/relational/core/embedded/plan_logging.go:78`) is installed **per
connection**, via the same `conn.Raw` route as the quotas — `SetPlanLogger`
(`pkg/relational/core/embedded/connection.go:165`). There is no DSN parameter, no context value and
no driver-level registration. That is convenient here: the tenant ID goes in the closure.

`PlanGenerationLogger` is an interface with one method and the package ships no func adapter, so
declare one and close over the tenant ID:

```go
type tenantPlanLogger struct{ tenantID string }

func (l tenantPlanLogger) LogPlanGeneration(ctx context.Context, info embedded.PlanGenerationInfo) {
	slog.InfoContext(ctx, "plan",
		"tenant", l.tenantID,                 // yours; the engine carries no tenant identity
		"sql", info.SQL,
		"plan_hash", info.PlanHash,
		"cache", info.Cache,
		"planning", info.PlanningDuration,
		"slow", info.SlowQuery)
}

_ = conn.Raw(func(driverConn any) error {
	ec := driverConn.(*embedded.EmbeddedConnection)
	ec.SetSlowQueryThresholdMicros(500_000)
	ec.SetPlanLogger(tenantPlanLogger{tenantID: tenantID})
	return nil
})
```

**It reports planning only.** The complete field set is `SQL`, `PlanHash`, `PlanExplain`,
`PlanningDuration`, `Cache`, `CacheNumEntries`, `SlowQuery`, `Err`
(`plan_logging.go:50-71`), the record is built entirely in `finish()` from the planning clock and
the plan tree, and the callback fires once per `Plan()` call. For what the statement then *did*,
see the next section.

Sampling and log volume are the handler's problem: the engine always emits
(`plan_logging.go:73-77`). At tenant-fleet scale, sample.

`SetPlanLogger` is not safe to call concurrently with planning on the same connection.

### Per-tenant execution statistics

`ExecutionStatsLogger` (`pkg/relational/core/embedded/execution_logging.go`) is the
post-execution counterpart, installed the same way — per connection, via `conn.Raw` →
`SetExecutionStatsLogger`. It is a **separate** interface from `PlanGenerationLogger`, not a second
method on it, because a cached plan is planned once and executed many times: the two records do not
pair up 1:1, and `PlanHash` is the join key between them.

One `ExecutionStats` is emitted per statement execution, carrying `SQL`, `PlanHash`,
`ExecutionDuration`, `RowsReturned`, `RowsAffected`, `RecordsScanned`, `BytesScanned`, `Pages`,
`Retries`, `SlowQuery` and `Err`. This is what per-tenant cost attribution needs: `RecordsScanned`
and `BytesScanned` are what the *cluster* served, which is not the same number as the rows the
tenant received — a filtered `LIMIT 1` over a million-row table returns one row and scans a
million.

```go
type tenantExecLogger struct{ tenantID string }

func (l tenantExecLogger) LogExecutionStats(ctx context.Context, s embedded.ExecutionStats) {
	slog.InfoContext(ctx, "query",
		"tenant", l.tenantID,               // yours; the engine carries no tenant identity
		"plan_hash", s.PlanHash,            // joins to the PlanGenerationInfo
		"duration", s.ExecutionDuration,
		"rows_returned", s.RowsReturned,
		"records_scanned", s.RecordsScanned, // the cluster-load number
		"bytes_scanned", s.BytesScanned,
		"retries", s.Retries,
		"slow", s.SlowQuery,
		"err", s.Err)
}

_ = conn.Raw(func(driverConn any) error {
	ec := driverConn.(*embedded.EmbeddedConnection)
	ec.SetExecutionStatsLogger(tenantExecLogger{tenantID: tenantID})
	return nil
})
```

Four properties matter operationally:

- **Failed statements still report.** A statement killed by `EXECUTION_SCANNED_ROWS_LIMIT` emits a
  record carrying `Err` *and* the counters it consumed on the way to being killed. The statements
  worth accounting for are disproportionately the ones that blew a budget, so a surface that only
  reported on success would go quiet exactly where it is needed.
- **Retried attempts are charged.** A page whose transaction conflicts and re-executes counts both
  attempts' scans. The cluster served both reads; billing one would understate the tenants worth
  knowing about. `Retries` tells you how much of the number that is.
- **`SlowQuery` now has two independent meanings.** `SetSlowQueryThresholdMicros` governs both:
  `PlanGenerationInfo.SlowQuery` says planning exceeded it, `ExecutionStats.SlowQuery` says
  execution did. One knob, and the pair tells you which half was slow.
- **DDL is not covered.** DDL routes through `execStatement` rather than the paged execution path,
  so it emits no execution record. SELECT and DML both do.

Same threading contract as `SetPlanLogger`: not safe to call concurrently with execution on the
same connection. Same sampling split too — the engine always emits, the handler decides volume.

Record-layer `StoreTimer` counters remain unexported on the SQL path (see above); `ExecutionStats`
is the SQL-layer surface, and it is a Go-only read-side extension. Java has no equivalent —
`fdb-relational-api`, `-jdbc` and `-grpc` carry no metric surface at all, and although
`fdb-relational-core` configures scan limits it never reads the consumed counts back. See
`rfcs/211-post-execution-query-stats.md` for the Java survey behind that claim.

### No `EXPLAIN ANALYZE`

`EXPLAIN` produces plan text without executing the statement — it returns a one-column `PLAN`
static result (`pkg/relational/core/embedded/cascades_generator.go:551`, `:572-575`). The grammar
takes an optional format clause and nothing else
(`pkg/relational/core/parser/grammar/RelationalParser.g4:717`, `:733-735`); `ANALYZE` is a lexer
token that appears in no parser rule (`RelationalLexer.g4:49`) and is not in `keywordsCanBeId`
(`RelationalParser.g4:1302-1305`), so `EXPLAIN ANALYZE ...` is a **syntax error** (`42601`,
`pkg/relational/core/parser/parser.go:387`), not an unsupported-feature message. Do not build a
tenant-facing "why is my query slow" tool on it.

---

## 5. TLS

### There is no DSN knob

`pkg/relational/sqldriver/dsn.go` mentions TLS nowhere, and the driver calls
`fdbclient.Open(clusterFile)` with no options parameter at all
(`pkg/relational/sqldriver/driver.go:211`, `pkg/internal/fdbclient/open_purego.go:23`). A SQL-driver
deployment configures TLS through exactly two surfaces:

1. The cluster file's `:tls` coordinator suffix (`pkg/fdbgo/client/database.go:126`). It is
   all-or-nothing — a partially-`:tls` coordinator list is rejected (`database.go:160`).
2. `FDB_TLS_CERTIFICATE_FILE` / `FDB_TLS_KEY_FILE` / `FDB_TLS_CA_FILE`, falling back to
   `cert.pem`/`key.pem` under `/etc/foundationdb` (`pkg/fdbgo/client/tls.go:15`, `:33-35`). Setting
   only one of cert/key is a hard error (`tls.go:40`).

The precedence rule is at `pkg/fdbgo/client/tls.go:62-68`. `client.WithTLSConfig`
(`pkg/fdbgo/client/options.go:61`) is the third surface, but it is unreachable from `database/sql` —
another reason a service that needs fine control opens the client itself and uses
`sqldriver.RegisterBackend`. Note the footgun: `WithTLSConfig(nil)` is a **no-op**, it does not
force plaintext (`options.go:58`).

### Certificates must be hostname-verifiable — and that means IP SANs

This is the single highest-value warning on this page, because a certificate set that works on
`libfdb_c` can hard-fail on the pure-Go client.

The pure-Go client does standard Go TLS verification. `InsecureSkipVerify` is never set anywhere in
`pkg/fdbgo`, and the dialer fills in `ServerName` from the dialed host when the caller left it
unset:

```go
// pkg/fdbgo/transport/conn.go:1159-1166
func upgradeTLS(conn net.Conn, addr string, cfg *tls.Config) (net.Conn, error) {
	cfg = cfg.Clone()
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			cfg.ServerName = host
		}
	}
```

**Coordinator hostnames are not enough.** After coordination, every connection to a proxy or storage
server is dialed at an address the cluster sent over the wire, and that address is always numeric:
`networkAddressString` builds `host:port` from the raw `types.IPAddress` bytes
(`pkg/fdbgo/client/endpoint.go:54-58`, `:71-88`), and that string is what gets dialed with the TLS
config (`pkg/fdbgo/client/database.go:573`). So `ServerName` becomes an IP literal for the majority
of connections regardless of what the cluster file says.

Consequence: **server certificates must carry IP SANs for every cluster process address.** Without
them the handshake fails closed at `conn.go:1170-1173` with Go's
`x509: cannot validate certificate for <ip> because it doesn't contain any IP SANs`.

This is a deliberate divergence from `libfdb_c`, which does not do hostname verification by default
— recorded at `rfcs/051-tls-wiring.md:140`: "`FDB_TLS_VERIFY_PEERS` rule DSL … Go uses standard CA +
SNI verification (the C++ `Check.Valid=1` default)." There is no knob to relax it short of supplying
your own `*tls.Config` with a `VerifyPeerCertificate`, which the SQL driver cannot do.

### Tokens and `verify_peers` are `libfdb_c`-only; tenants are pure-Go-only

Stated plainly, because these two facts together decide your build:

- **Authorization tokens.** Pure-Go refuses them loudly:
  `pkg/fdbgo/fdb/options.go:389-393` returns `&UnsupportedOptionError{Option: "authorization_token"}`
  (FDB `invalid_option`, 2007), with the rationale at `options.go:67-76` — a silently ignored token
  means the request goes out unauthenticated. `libfdb_c` forwards it 1:1
  (`pkg/fdbgo/libfdbc/options.go:105`).
- **`tls_verify_peers`.** No implementation in `pkg/fdbgo` at all; out of scope per
  `rfcs/051-tls-wiring.md:140`. Under `-tags libfdbc` it is reachable through the Apple binding's own
  network options, not through anything this repo exposes.
- **Native tenants.** Pure-Go only. `OpenTenant`/`CreateTenant`/`DeleteTenant` each downcast to the
  concrete pure-Go transaction with the comment "tenant ops are pure-Go only (out of RFC-109
  escape-hatch scope)" (`pkg/fdbgo/fdb/database.go:450`, `:479`, `:491`); `fdb.Tenant` embeds the
  concrete pure-Go `Database` (`pkg/fdbgo/fdb/tenant.go:11-14`); there is not one tenant symbol under
  `pkg/fdbgo/libfdbc/`.

**The mutual exclusion:** native FDB tenants and authorization tokens are the two halves of
FoundationDB's own tenant-isolation security model, and no single build of this port has both. Build
pure-Go and you get tenants with no token support and no `verify_peers`; build `-tags libfdbc` and
you get tokens and `verify_peers` but no tenants. That is a second, independent reason v1 isolates
tenants by subspace (§1) rather than by native tenants: subspace isolation works identically on both
builds and leaves the choice of backend free.

---

## 6. Backup and restore — what you actually have

**Cluster-granular only.** There is no backup/restore in the Go layer, and none in Java's record
layer either — see [`operations.md` §7](operations.md#7-backup--restore) for the `fdbbackup` /
`fdbrestore` commands. The pure-Go client has no `fdbbackup` protocol support: the backup error
codes exist as a string table (`pkg/fdbgo/fdb/error.go:344`) and nothing else; there is no backup
agent, no task bucket, no log-ranges handling. `frl` has no `export`, `import`, `backup` or
`restore` subcommand (the complete set is registered at `cmd/frl/internal/cmd/root.go:51-61`).

So a restore is a **whole-cluster** operation. Restoring one tenant to a point in time by restoring
the cluster means restoring every other tenant to that point in time too.

**Per-tenant restore is not built. It is buildable, and it is a real build — do not scope it as a
script.** The raw primitives are all exported: a tenant's data is one contiguous subspace,
`subspace.FDBRangeKeys()` gives its bounds (used exactly that way at
`cmd/frl/internal/cmd/store_dump.go:129`), `frl keyspace resolve` prints the prefix
(`cmd/frl/internal/cmd/keyspace.go:26`), and `GetRange`/`Set`/`ClearRange` are ordinary client calls
(`pkg/fdbgo/fdb/transaction.go:100`, `:209`, `:236`). Nothing in the layer copies a range, though —
the only whole-subspace bulk operation is destructive (`DeleteAllRecords`,
`pkg/recordlayer/store.go:888`).

Three things make the naive version wrong, and they are the actual content of the build:

- **No cross-transaction snapshot.** Each scan is one transaction, bounded by FDB's 5 s / 10 MB
  limits, so a store larger than one transaction is copied at drifting read versions. The only
  freeze available is `frl store lock forbid-record-update`
  (`cmd/frl/internal/cmd/store_write.go:52`).
- **Record versions cannot be restored.** `SaveRecordWithOptions` takes `(record, existenceCheck)`
  (`pkg/recordlayer/store.go:503`); there is no API to write a record's stored version. A
  reload mints new versions, which breaks VERSION-keyed indexes and version continuations.
- **The CLI is not a capture format.** `frl store dump` prints value *lengths*, not bytes
  (`cmd/frl/internal/cmd/store_dump.go:188`), and `frl record scan` emits three fields —
  `primary_key`, `record_type`, `record` (`cmd/frl/internal/cmd/record.go:186`) — with no record
  version, no index state and no store header. A scan/put round trip is lossy.

`formatVersionIncarnation` (`pkg/recordlayer/store.go:50`, "Incarnation counter for cross-cluster
migration") is not a migration tool; it is a header integer plus a key function
(`pkg/recordlayer/key_expression.go:1476`) that disambiguates versionstamps *after* a migration some
other system performed. It moves no data.

**If per-tenant point-in-time restore is a product promise, budget it as a project**: a
prefix-rewriting, snapshot-consistent copier with version fidelity. Do not promise it on the
strength of "the data is just a key range".

---

## 7. Operational gaps to know before you sell this

### Index builds are N CLI invocations

`frl index build <name>` builds **one index on one store**: the positional argument is the index
name (`cmd/frl/internal/cmd/index_write.go:36`, `:56`), the store is addressed by scalar
`--database`/`--schema`/`--keyspace-tuple` flags (`cmd/frl/internal/cmd/openstore.go:42-49`, all
`StringVar` — there is no slice form anywhere in `cmd/frl`), and the driver builds one indexer for
one target (`index_write.go:215`, single `BuildIndex` call at `:263`).

There is no orchestrator in the repo. Rolling one new index across N tenants is N invocations, or a
Go driver you write. If you write one, note the CLI does not expose everything the builder has —
`SetTargetIndexes` (`pkg/recordlayer/online_indexer.go:414`), `SetSourceIndex` (`:444`) and
`SetMutualIndexing` / `SetMutualIndexingBoundaries` (`:496`, `:505`, the closest thing to a
multi-worker distributed build) are Go-API only. Knobs that *are* exposed:
`--limit`, `--rps`, `--max-retries`, `--time-limit` (`index_write.go:79-82`); the semantics are in
[`operations.md` §4](operations.md#4-online-index-lifecycle), including the trap that
`SetRecordsPerSecond` only takes effect when `maxRetries > 0`.

### A new index on an existing tenant usually lands DISABLED

The default rebuild policy rebuilds inline at or below 200 records and leaves the index `Disabled`
above it (`pkg/recordlayer/store_builder.go:1125-1130`, Java's
`FDBRecordStoreBase.MAX_RECORDS_FOR_REBUILD`). `Disabled` means **not maintained by writes and not
scannable** until an `OnlineIndexer` run completes it — see
[`operations.md` §5](operations.md#5-index-state-transitions).

Getting a real record count requires a count source in the metadata. Without one,
`getRecordCountForRebuildPolicy` falls through to a single-record emptiness probe and reports
`math.MaxInt64` for any non-empty store (`pkg/recordlayer/store_builder.go:847`, `:876-884`), so
every non-trivial store takes the DISABLED arm.

**Correction to a claim that circulates in this repo's own notes: SQL schemas do not *always* take
that path.** They do *by default* — the relational metadata builder deliberately sets no
record-count key, because the stored template bytes must match Java's
(`pkg/relational/core/metadata/builder.go:489`), and that default is pinned by
`pkg/relational/sqldriver/evolution_added_index_gate_fdb_test.go:8`. But SQL *can* create a COUNT
index, two ways:

- explicitly — `CREATE INDEX <n> AS SELECT COUNT(*) FROM t GROUP BY g`
  (`pkg/relational/core/parser/grammar/RelationalParser.g4:172` →
  `pkg/relational/core/metadata/builder.go:1177`), pinned e2e by
  `pkg/relational/conformance/yamsql/testdata/aggregate_index_count_star.yaml:13`;
- implicitly — any grouped aggregate index drags in an auto-emitted `__GROUP_COUNT` companion
  (`builder.go:660`).

Relational primary keys are record-type-prefixed unless the template declares `INTERMINGLE TABLES`
(`builder.go:1232`, conditional at `:1239-1240`, mode threaded from `:128` into `buildPrimaryKeyExpression` at `:552`), which satisfies
`singleRecordTypeWithPrefixKey` (`pkg/recordlayer/store_builder.go:779`), and a *grouped* COUNT
index still qualifies as a count source — the rule is stated for the universal path at
`store_builder.go:900-903` and reached here through `snapshotRecordCountForRecordType` (`:913`), whose
`count(EMPTY)` operand is a grouping prefix of any grouped COUNT index
(`isGroupPrefix`, `pkg/recordlayer/aggregate_function.go:422`, tested for `COUNT` at `:261`). So a tenant whose schema happens
to carry a COUNT index can flip back to the inline arm — **and that is not automatically the better
outcome**: the inline rebuild runs inside the store-open transaction, against the 5 s / 10 MB
limits. Know which arm each tenant's schema takes; do not assume it is uniform across the fleet.

`MarkIndexDisabled` clears the index's data as it transitions (`pkg/recordlayer/index_state.go:226`,
`:236` → `clearIndexData`, `:456`), so a DISABLED index holds no stale entries — nothing can read a
wrong answer from it, it simply is not there.

### No `ALTER`, and the fan-out loop for the workaround does not ship

`ALTER` is a lexer token that no parser rule references
(`pkg/relational/core/parser/grammar/RelationalLexer.g4:47`; `ddlStatement` is
`createStatement | dropStatement | createTempFunction | dropTempFunction`,
`RelationalParser.g4:57-62`). `ALTER TABLE ...` is a **syntax error**, not an
unsupported-feature message.

Schema templates are immutable in practice: `CREATE SCHEMA TEMPLATE` of an existing name fails with
SQLSTATE **42F59** ("new version must be greater than current version",
`pkg/relational/core/ddl/save_schema_template.go:26-44`,
`pkg/relational/api/errcode.go:123`), and since the DDL visitor never sets a version and the builder
hardcodes 1 (`pkg/relational/core/metadata/builder.go:112`), a second CREATE always fails. There is
no `CREATE OR REPLACE` for templates — that exists only for temp functions
(`RelationalParser.g4:238`). Pinned by `pkg/relational/sqldriver/ddl_errors_probe_test.go:80`.

`RepairSchema` exists — `pkg/relational/api/catalog.go:76`, implemented at
`pkg/relational/core/catalog/fdb_store_catalog.go:472`: load the schema, load its template's latest
version, save the regenerated schema. It is a **Go catalog API, not SQL**; no grammar rule reaches
it (`pkg/relational/sqldriver/evolution_added_index_gate_fdb_test.go:56`). And it has **zero
non-test call sites** — no loop iterates `ListDatabases`/`ListSchemas` calling it. No fan-out ships; the
per-tenant Go-API loop is described as the status quo at `docs/rfc-schema-migration.md:306-311`.

So the fleet-migration story today is: bump the template version → call `RepairSchema` per tenant
through the Go API, in a loop you write → run `frl index build` per tenant per index, in a shell
loop you write. **Neither loop ships.** Plan for building both.

### No online index scrubber — a detection API, not a fleet tool

Java has `OnlineIndexScrubber` with `scrubDanglingIndexEntries()` and `scrubMissingIndexEntries()`
(Java source at tag 4.12.11.0,
`fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/OnlineIndexScrubber.java:43`,
`:92`, `:103`) — chunked, throttled, resumable, and repairing. **Go has no equivalent**, and says so
at `pkg/recordlayer/index_state.go:567` ("the scrubbing subspaces (Go has no index scrubbing)").
There is no `frl index scrub` (`grep scrub cmd/frl/` is empty) and no repair entry point anywhere.

What Go *does* have is a **detection-only** API: `FDBRecordStore.ValidateIndex`
(`pkg/recordlayer/index_validation.go:39`, Java's `StandardIndexMaintainer.validateEntries()`),
returning `MissingEntries` and `OrphanedEntries` (`:12-22`). Read its limits before you plan around
it:

- It **repairs nothing** — the file's entire exported surface is `ValidateIndex` and
  `IndexValidationResult.IsValid` (`:25`, `:39`). There is no repair entry point.
- It scans **all** records and **all** index entries into memory: the expected-entry map grows once
  per record (`:42-43`), and phase 2 materialises the entire index subspace in a single
  `GetRange(...).GetSliceWithError()` (`:82`) with no continuation and no scan limit. It is not
  bounded by FDB's 5 s / 10 MB transaction limits by construction — it will fail on a store that
  does not fit.
- It has **zero non-test call sites**, and no CLI exposes it.

Operator consequence: for a store of any real size there is **no usable way to detect, and no way at
all to repair, dangling or missing index entries** — per tenant or fleet-wide. If you suspect an
index is wrong, the remedy is a full rebuild.

### Format version: Go's default is not Java's

`SetFormatVersion` now exists on both builders (`pkg/recordlayer/store_builder.go:1219`,
`pkg/recordlayer/online_indexer.go:333`) and on the store (`pkg/recordlayer/store_api.go:84`). It is
a **ceiling, never a downgrade**, and it exists for exactly the rolling-upgrade case: pin every
instance to the OLD version so no upgraded instance starts writing a layout the not-yet-upgraded
ones cannot read (`store_builder.go:1209-1218`). An explicit `0` is an error, not "give me the
default" (rationale `:1204-1205`, enforced in `validateBuilder` at `:1337-1339`).

The defaults differ, and in a mixed Go/Java fleet that matters: **an unpinned Go store opens at
format version 14** (`store_builder.go:1196` → `formatVersionCurrent`,
`pkg/recordlayer/store.go:51-52`), **while Java's default for a new store is 7**
(`FormatVersion.java:215-216`, `CACHEABLE_STATE(7)` at `:133`; Java's *maximum supported* is also
14). A Go binary opening a tenant store upgrades that store's header in place unless you call
`SetFormatVersion` explicitly — and `frl index build` exposes no flag for it, so the CLI always runs
at its binary's newest. This divergence is booked in `DIVERGENCES.md`; treat it as a deployment
decision, not an implementation detail.

---

## 8. The tenant contract — what your tenants will hit

This is the list to put in front of anyone writing SQL against a tenant database. Every entry was
re-verified against master while writing this page, against the code and its committed pins rather
than against the prose that described them. That pass **refuted six claims** the previous revision
of `road-to-prod.md`'s watch-list asserted as live — the nullable-array wire divergence (fixed), the
LEFT-JOIN `isNullable` divergence (inverted), both client-side watch claims (fixed), projected
`EXISTS` (narrowed), and string functions as unsupported (Go supports them). `road-to-prod.md` was
corrected in the same pass, so the two pages now agree; the refutations are recorded here as well,
because a reader who has seen the old list needs to know which way each one moved.

### The silent ones — no error, wrong expectation

These are the dangerous entries, because nothing tells the tenant.

- **`DELETE`/`UPDATE … RETURNING` via `Exec` silently drops the returned values.** The grammar
  accepts `RETURNING` (`pkg/relational/core/parser/grammar/RelationalParser.g4:378`, `:455`) and the
  DML executes correctly with the right row count — the values simply never surface. Via `Query` you
  get `0A000 INSERT/UPDATE/DELETE return a row count, not rows — use Exec, not Query`
  (`pkg/relational/core/embedded/connection.go:653-655`). `INSERT … RETURNING` is `42601` (not in
  the grammar). Pinned: `pkg/relational/sqldriver/returning_clause_probe_test.go:49`, `:61`, `:75`.
- **`LIKE` does not treat a trailing newline as content.** `WHERE name LIKE 'abc'` matches the
  stored value `"abc\n"` — no DOTALL, and `$` matches before one final line terminator. Deliberate
  Java parity, derived against a live JDK. `_` and `%` do *not* match a line terminator. Pinned:
  `pkg/recordlayer/query/plan/cascades/values/like_match_test.go:120`, `:117`, `:118`, `:133`; the
  semantics are spelled out at
  `pkg/recordlayer/query/plan/cascades/values/like_match.go:23-31`.
- **NaN is a total order, not IEEE.** `WHERE (v/z) = (v/z)` returns **every** row, and NaN sorts
  above `+Infinity`. This matches PostgreSQL and Java's boxed comparison path (which is the path the
  SQL layer uses); it diverges from IEEE and from Java's direct `RelOpValue` path. Pinned:
  `pkg/relational/sqldriver/nan_comparison_semantics_test.go:100-107`, `:111-115`; derivation at
  `pkg/recordlayer/query/plan/cascades/values/coercion.go:26-46`.

### Type-system limits

- **No `DECIMAL` / `NUMERIC`.** They are lexer tokens with no place in `primitiveType`
  (`pkg/relational/core/parser/grammar/RelationalParser.g4:141-142`) and are not in
  `keywordsCanBeId`, so a tenant gets **`42601` syntax error** with a caret under the word — not a
  "feature unsupported" message. Java has no exact-decimal type either
  (`pkg/relational/conformance/yamsql/ansi_roster.go:32`). Pinned:
  `pkg/relational/core/parser/decimal_type_rejected_test.go`.

  **Money must use a scaled-integer convention**: store minor units in `BIGINT` and scale in the
  application. The numeric types that exist are `INTEGER` (int32), `BIGINT` (int64), `FLOAT`
  (float32) and `DOUBLE` (float64) (`pkg/relational/core/embedded/ddl.go:777-784`).
- **`isNullable` is uniformly nullable for scalar columns** — do not read a `NOT NULL` constraint out
  of result-set metadata. Scalar `NOT NULL` is not expressible in DDL at all (Java parity: `NOT NULL`
  is allowed only on ARRAY columns), and an ARRAY `NOT NULL` column is a flat repeated field, not
  proto `REQUIRED`, so no DDL-expressible column ever derives `ColumnNoNulls`.

  **This refuted watch-list entry 13**, which described `isNullable` reporting NOT NULL on the
  null-supplying leg of a LEFT JOIN. The test named there now pins the *opposite* — that the hole is
  unreachable — and fails if it re-arms:
  `pkg/relational/sqldriver/cross_leg_null_born_fdb_test.go:108-113`, header at `:18-27`; corroborated
  by `pkg/relational/sqldriver/aggregate_over_mixed_nesting_outer_join_fdb_test.go:154`.

### Cleanly rejected query shapes

Each of these fails fast with a code, which is the correct posture — but a tenant needs to know
before writing the query.

| Shape | SQLSTATE | Pin |
|---|---|---|
| derived table with a JOIN body | `0AF00` | `pkg/relational/sqldriver/explain_unplannable_fdb_test.go:77-80` |
| `EXISTS` inside an `OR` | `0A000` | `pkg/relational/conformance/yamsql/testdata/exists_with_or.yaml:36-45` |
| correlated scalar subquery inside `EXISTS` | `0A000` | `pkg/relational/sqldriver/exists_semantics_probe_test.go:153-159` |
| scalar subquery over a `FROM`-less `SELECT` | `0AF00` | `pkg/relational/conformance/yamsql/testdata/scalar_subquery_java.yaml:47-51` |
| correlated `EXISTS` over `GROUP BY` **with `HAVING`** | `0AF00` | `pkg/relational/conformance/yamsql/testdata/exists_with_aggregate.yaml:12-28` |
| `x IN (SELECT …)`, every position | `0AF00` | `pkg/relational/core/embedded/eval_predicate_map.go:145-152` |
| `COUNT(DISTINCT …)` | `0AF00` | `pkg/relational/conformance/yamsql/testdata/count_distinct.yaml:14-18` |
| `UNION` / `EXCEPT DISTINCT` (only `UNION ALL` is supported) | `0AF00` | `pkg/relational/core/embedded/logical_predicate.go:6296` |
| `EXCEPT` / `INTERSECT` (absent from the grammar) | `42601` | `pkg/relational/sqldriver/intersect_except_probe_test.go:43-46` |
| `NULLIF` | `0AF00` | `pkg/relational/conformance/yamsql/testdata/coalesce_nullif.yaml:26-28` |
| foreign keys / `CHECK` / column defaults (absent from the grammar) | `42601` | `pkg/relational/core/parser/grammar/RelationalParser.g4:154-156` |
| window functions (except vector K-NN `ROW_NUMBER() OVER (…)` in `QUALIFY`) | `0AF00` | `pkg/relational/conformance/yamsql/testdata/window_function_probes.yaml:15-22` |
| `ALTER` anything | `42601` | see §7 |

Two narrowings worth knowing, both **refuting** the corresponding watch-list phrasing:

- **A bare projected `EXISTS` works.** `SELECT id, EXISTS(...) FROM t` returns rows
  (`pkg/relational/sqldriver/projected_exists_round12_fdb_test.go:230-236`). Only an `EXISTS` buried
  inside another expression in the select list — `CASE WHEN EXISTS`, `EXISTS AND pred`,
  `EXISTS OR pred` — is rejected `0AF00`
  (`pkg/relational/core/query/expr/walk.go:111-113`, residuals pinned at
  `projected_exists_round12_fdb_test.go:183-206`).
- **A `HAVING`-less correlated `EXISTS` over `GROUP BY` works**, deliberately: grouping a non-empty
  row set yields at least one group and grouping an empty one yields none, so the grouping is
  semantics-preserving under `EXISTS`
  (`pkg/relational/core/embedded/logical_predicate.go:8606-8617`; supported half pinned with rows at
  `pkg/relational/sqldriver/exists_over_aggregate_fdb_test.go:110`).

### DDL surprises

- **`DROP SCHEMA IF EXISTS` ignores `IF EXISTS`** and errors on a missing schema:
  `42F51 schema /mydb/ghost does not exist`
  (`pkg/relational/core/catalog/fdb_store_catalog.go:416-417`,
  `pkg/relational/api/errcode.go:117`). This is deliberate replication of Java's bug — its
  `DdlVisitor.visitDropSchemaStatement` never reads `ctx.ifExists()` — and the code comment forbids
  "fixing" it (`pkg/relational/core/embedded/ddl.go:104-111`, `:127`). `DROP SCHEMA TEMPLATE
  IF EXISTS` and `DROP DATABASE IF EXISTS` **do** honor it. Pinned with all four legs:
  `pkg/relational/sqldriver/drop_schema_ifexists_conformance_probe_test.go:30`, `:39`, `:44`, `:50`.

  Consequence: **idempotent teardown scripts break on schemas.** Wrap the drop, or check first.

### Portability caveat: string functions are a Go-only extension

`UPPER`, `LOWER`, `SUBSTRING`, `TRIM`, `CONCAT`, `REPLACE`, `POSITION`, `REVERSE`, the `*_LENGTH`
family and the `L/RTRIM` pair all work here
(`pkg/recordlayer/query/plan/cascades/values/scalar_function_catalog.go:363-370`, rows pinned in
`pkg/relational/conformance/yamsql/testdata/string_functions.yaml`, real-FDB at
`pkg/relational/sqldriver/embedded_fdb_test.go:3866`). **This refutes the "unsupported on both
engines" claim for string functions** — Java's `BuiltInFunction` registry has no entries and rejects
the call form.

It is a read-side extension with zero wire impact, so it is safe. But a tenant query using `UPPER()`
**will not run against Java**. If cross-engine portability is part of what you sell, say so.

### Watches: the two things a tenant needs, both changed recently

- **A watch registers at the COMMITTED version**, not the read version, on the `fdb` facade a tenant
  uses. `Set(k, B); w = Watch(k)` stays pending until the next *external* change instead of firing on
  the transaction's own write (`pkg/fdbgo/fdb/transaction.go:457-459`,
  `pkg/fdbgo/client/transaction.go:2221-2226`), matching `libfdb_c`. Pinned by the cross-client
  differential `pkg/fdbgo/bench/differential_watch_test.go:266-272`. The low-level synchronous
  `client.Transaction.Watch` still registers at the read version, deliberately — it blocks until the
  watch fires, so it structurally cannot wait for a commit (`pkg/fdbgo/client/readpath.go:1150-1155`).
- **There IS a `too_many_watches` cap.** 10 000 concurrent watches per `Database` by default,
  raisable to 1 000 000 via `SetMaxWatches`, past which registration fails `1032`
  (`pkg/fdbgo/client/database.go:381-386`, `:388-401`, charged at
  `pkg/fdbgo/client/readpath.go:1236-1238`). Pinned: `pkg/fdbgo/client/watch_limit_test.go:10-14`.
  Budget watches per tenant against it — this is a shared, per-process resource, so one tenant can
  exhaust it for the rest.

**Both of these refute `road-to-prod.md`'s client-side operational list**, which recorded
"watches register at read version, not commit-gated; no `too_many_watches` limit".

### Not a tenant-contract item any more

**The nullable-array wire divergence is fixed.** Watch-list entry 14 described Go writing a plain
repeated field where Java wraps nullable arrays, with `[]` and NULL collapsing — and called it "the
one OPEN wire-compat divergence on the hard line". All of that is false on master. The wrapper is
emitted (`pkg/relational/core/metadata/builder.go:930-937`, `wrapperFor` at `:1070-1090`, contract at
`:760-762`) and `[]` and NULL are distinct on read-back
(`pkg/relational/sqldriver/array_literal_insert_fdb_test.go:143-156`, `:200-207`).

The proof that closes the *hard line* is the cross-engine one: `conformance/rfc204_template_golden_test.go`
asserts `live Java catalog bytes == committed golden bytes == Go catalog bytes` on the stored
schema-template, with `nullable_scalar_array` (`:88`) and `nullable_array_of_struct` (`:98`) among the
templates and Java-blessed goldens committed at `conformance/testdata/rfc204/`. That is what would go
red if Go's wrapper shape drifted from Java's. (The Go-internal byte-equal pin at
`array_literal_insert_fdb_test.go:217`, `:277-282` is a *different* guarantee — it holds the writer to
the descriptor's declared shape. Its golden is built from the same descriptor, so it cannot by
construction detect a Go-vs-Java shape divergence; `:200-207` is the pin that fails if the wrapper
stops being emitted.)

The one residual is the wrapper's *type name* — Go derives it deterministically where Java mints a
UUID — which is wire-valid because every Java reader checks the descriptor's shape, never its name
(`builder.go:1058-1067`), and the goldens normalize that one token on both sides (`:25-33`).
