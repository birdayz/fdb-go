# Operator guide

Practical guidance for running `fdb-record-layer-go` in production. This covers connecting to a
cluster, transaction behaviour and limits, the online-index lifecycle, schema evolution,
backup/restore, and the observability hooks. It is **pre-1.0** — see `RELEASE.md` for the stability
policy and `PRODUCTION_READINESS.md` for the current readiness snapshot.

The one hard line: **the FDB wire format is byte-compatible with Java `fdb-record-layer-core`
4.12.11.0** in every release. Go, C, and Java apps can share one cluster and read each other's data.

---

## 1. Connecting to a cluster

### SQL driver (`database/sql`)

Register is automatic on import; the driver name is `fdbsql`. The DSN mirrors Java's JDBC URL minus
the `jdbc:` prefix:

```
fdbsql:///<database-path>                                  # default cluster file
fdbsql:///<database-path>?cluster_file=/etc/foundationdb/fdb.cluster&schema=<name>
```

```go
import _ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"

db, err := sql.Open("fdbsql", "fdbsql:///myapp?cluster_file=/etc/foundationdb/fdb.cluster&schema=app")
```

Cluster-file resolution order: the DSN `cluster_file` param → the `FDB_CLUSTER_FILE` environment
variable → FDB's default file. The remote form `fdbsql://host:port/...` is **not implemented**
(returns an unsupported-operation error) — the driver is in-process. Opened clients are cached
per cluster-file path for the process lifetime.

### Record-store API

Pure-Go client (the default). `fdb.OpenDatabase` is always the pure-Go client and needs the API
version selected first:

```go
fdb.APIVersion(730)                                          // required before any open
db, _ := fdb.OpenDatabase("/etc/foundationdb/fdb.cluster")   // or fdb.OpenDefault()
rdb := recordlayer.NewFDBDatabase(db)
```

`recordlayer.NewFDBDatabaseFactory().GetDatabase(clusterFile)` caches by cluster file (mirrors Java
`FDBDatabaseFactory`) and routes through the backend seam below.

### The C-client escape hatch

The client is **pure Go by default** (no cgo). To run against Apple's `libfdb_c` instead — e.g. the
battle-tested C client on a bet-the-company write path — open through the **backend seam**
(`fdbclient.Open` picks the backend by build tag) and build with the tag:

```go
backend, _ := fdbclient.Open(clusterFile)            // pure-Go OR libfdb_c, per build tag
rdb := recordlayer.NewFDBDatabaseWithBackend(backend)
```

```sh
go build ./...                       # default → pure-Go
CGO_ENABLED=1 go build -tags libfdbc ./...   # → libfdb_c
```

The plain `fdb.OpenDatabase` form above is *not* backend-selected; the switch lives in
`fdbclient.Open`. This is a **build-time** choice (the libfdb_c network thread is once-per-process),
not a runtime switch. Both backends are wire-compatible; a cross-client byte-identical differential
gates every PR (`nightly-libfdbc.yml`). The SQL driver and `FDBDatabaseFactory` route through this
seam too, so `-tags libfdbc` switches them as well.

---

## 2. Transactions: retries, timeouts, cancellation

Run record-store work via `rdb.Run` / `rdb.RunRead` (the record layer's retrying transaction
methods; the closure gets an `*FDBRecordContext`):

```go
_, err := rdb.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) { ... })
// read-only: rdb.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) { ... })
```

(`Run`/`RunRead` dispatch to the lower-level `fdb.Database` methods `Transact`/`ReadTransact` and the
ctx-bounded `TransactCtx`/`ReadTransactCtx`, which live on the `fdb.Database` handle, not on `rdb`.)

- **The `ctx` deadline bounds the retry loop and backoff.** A cancelled/expired ctx aborts retries
  promptly — this is the cancellation mechanism.
- **The default retry limit is unlimited**, matching `libfdb_c` (`RETRY_LIMIT = -1`). The `ctx`
  deadline is the bound; set one for any unattended work.
- The commit RPC itself runs detached from late ctx cancellation, so a cancel mid-commit can't tear
  a write in half (ambiguous-write safety, RFC-090).

Tune transaction defaults on the underlying `fdb.Database` handle (`db.Options()`), applied to every
transaction it runs:

| Option | Effect |
|---|---|
| `SetTransactionTimeout(ms)` | hard per-tx wall-clock cap (`0` = disabled, the libfdb_c default) |
| `SetTransactionRetryLimit(n)` | cap retries (`-1` unlimited, `0` no retries) |
| `SetTransactionMaxRetryDelay(ms)` | cap exponential backoff |
| `SetTransactionSizeLimit(bytes)` | fail a tx that exceeds the byte budget |

Per-transaction overrides are available via `tx.Options()`. `transaction_timed_out` (1031) is
**not** retryable — it surfaces to the caller.

---

## 3. Transaction limits

FoundationDB enforces hard limits the layer is built around:

| Limit | Value | How the layer copes |
|---|---|---|
| Transaction duration | 5 s | use cursors + continuations for long scans; `transaction_too_old` (1007) is retryable |
| Value size | 100 KB | records are **split** into 100 KB chunks (unsplit at PK suffix `0`, chunks at `1+`, version at `-1`) |
| Transaction size | 10 MB | `transaction_too_large` (2101) — split the work; the online indexer auto-lowers its batch limit on 2101 |
| Key size | ~10 KB | keep primary keys / indexed values small |

Long reads must use a cursor with a `TimeScanLimiter` and carry the continuation across
transactions; the SQL engine does this automatically via paginated execution.

---

## 4. Online index lifecycle

Build a new index on existing data without blocking writes, via `OnlineIndexerBuilder`:

```go
idxer, _ := recordlayer.NewOnlineIndexerBuilder().
    SetDatabase(rdb).
    SetMetaData(md).
    SetIndex(myIndex).
    SetSubspace(ss).
    SetLimit(100).                       // records per transaction (default 100)
    SetMaxRetries(3).                     // REQUIRED for SetRecordsPerSecond to take effect (see below)
    SetRecordsPerSecond(10000).          // inter-tx rate limit (only applied when maxRetries > 0)
    SetProgressLogIntervalMillis(10000). // log "Indexer: Built Range" at most every 10s (default -1 = off)
    SetLogger(slog.Default()).
    Build()

scanned, err := idxer.BuildIndex(ctx)    // returns records scanned
```

Key knobs (defaults in parentheses):

| Setter | Effect |
|---|---|
| `SetLimit(n)` (100) | records per build transaction; auto-lowered on transient `too_large`/`too_old` errors |
| `SetRecordsPerSecond(n)` (10000) | inter-transaction rate limit; `0` = unlimited. **Only takes effect when `SetMaxRetries(n>0)` is also set** — the rps wait lives on the retry/throttle path (default `maxRetries=0` ⇒ no throttling). Use `SetEnforcedPostTransactionDelay` for an unconditional delay. |
| `SetEnforcedPostTransactionDelay(ms)` (0) | fixed delay between ranges, **instead of** the rate limit; applied unconditionally (no retries needed) |
| `SetTimeLimit(d)` (unlimited) | abort with `TimeLimitExceededError` after the wall-clock budget |
| `SetMaxRetries(n)` (0) | retries per range on transient errors; also gates `SetRecordsPerSecond` throttling |
| `SetProgressLogIntervalMillis(n)` (-1) | `<0` off · `0` every range · `>0` throttle the progress log |
| `SetLogger(l)` (`slog.Default()`) | where progress events go (INFO) |
| `SetMarkReadable(bool)` (true) | mark the index readable when the build completes |
| `AddTargetIndex` / `SetMutualIndexing` | multi-target and concurrent (mutual) builds |

A build resumes from its persisted progress if interrupted.

---

## 5. Index-state transitions

An index is always in one of four states:

| State | Maintained on writes? | Used by queries? |
|---|---|---|
| `Disabled` | no | no |
| `WriteOnly` | yes | no |
| `Readable` | yes | yes |
| `ReadableUniquePending` | yes | yes (unique constraint still settling) |

The build path drives `Disabled`/new → `ClearAndMarkIndexWriteOnly` → (build) → `MarkIndexReadable`.
`MarkIndexReadable` verifies the index is fully built and free of uniqueness violations (else a
`RecordIndexUniquenessViolationError`). Querying a non-scannable index returns
`IndexNotReadableError`. Inspect state with `store.GetIndexState(name)` / `IsIndexReadable(name)`.

A freshly-added index in metadata is detected on store open. Under the **default** rebuild policy
(`DefaultIndexRebuildPolicy`): a small store (≤ ~200 records) rebuilds the index inline to
`Readable`; a larger store leaves it **`Disabled`** — writes do **not** maintain it until an
`OnlineIndexer` build runs (`Disabled` → `ClearAndMarkIndexWriteOnly` → build → `Readable`). To have
writes maintain a new index *during* backfill instead, opt into `WriteOnlyIfTooLargePolicy` via
`SetIndexRebuildPolicy` so the large-store case lands `WriteOnly`.

---

## 6. Schema-evolution safety

The store header records a **format version** (current `14`); it is upgraded automatically on open
and rejects a future version it doesn't understand (`UnsupportedFormatVersionError`). There is no
manual format-version setter — it tracks the code.

Governed by the wire-compat hard line:

- **Safe:** adding a protobuf field (unknown fields round-trip, so older readers preserve them);
  adding an index (default policy: `Disabled` until an online build → `Readable`, or `WriteOnly`
  during backfill with `WriteOnlyIfTooLargePolicy` — see §5); adding a record type.
- **Unsafe (breaks shared-cluster compat):** anything that changes key encoding, the record/index/
  version/continuation **format**, primary-key structure, or the split layout. Don't.

When in doubt, consult `DIVERGENCES.md` and confirm the change is covered by the conformance +
cross-engine differential + binding-stress gates.

---

## 7. Backup & restore

There is **no backup/restore in the Go layer** — and there is none in Java's record layer either;
it is an FDB-cluster concern. Use FoundationDB's own tools:

```sh
fdbbackup start  -d <backup-url> -C /etc/foundationdb/fdb.cluster
fdbrestore start -r <backup-url> -C /etc/foundationdb/fdb.cluster
```

Because the record layer's data is ordinary FDB key/values under the store's subspace, a
cluster-level backup captures it consistently.

---

## 8. Observability hooks

**Metrics.** The low-level pure-Go client (`*client.Database`) carries `ClientMetrics` (the `libfdb_c`
`DatabaseContext` transaction-metrics subset, RFC-097): commits started/completed, per-code retry
counters (conflicts 1020, maybe-committed 1021, too-old 1007, future-version 1009, throttled, …),
GRV cache hits, and latency sketches. `Metrics()` lives on `*client.Database` (a `MetricsSource`),
**not** on `fdb.Database` or `*recordlayer.FDBDatabase`. So keep the `*client.Database` handle when
you want metrics:

```go
cdb, _ := client.OpenDatabase(ctx, clusterFile)        // *client.Database — has Metrics()
http.Handle("/metrics", fdbmetrics.Handler(cdb))       // zero-dep Prometheus text exposition
rdb := recordlayer.NewFDBDatabase(fdb.WrapDatabase(cdb))
```

`fdbmetrics.Handler` accepts any `MetricsSource` (`interface{ Metrics() client.ClientMetricsSnapshot }`).

**Logs.** Diagnostics route through the standard `log/slog`. Apps set their own handler with
`slog.SetDefault(...)` (no record-layer logging API to learn), or pass a per-handle logger with the
client's `WithLogger(...)` option. Serious (panic-recovery) events log at Error; per-code retry
events at Debug, `commit_unknown_result` at Warn.

**Online-index progress.** `OnlineIndexerBuilder.SetProgressLogIntervalMillis(...)` +
`SetLogger(...)` emit a throttled `"Indexer: Built Range"` INFO event per range (off by default).

**Query planning.** Install a `PlanGenerationLogger` (RFC-034) via the connection to receive one
`PlanGenerationInfo` per `Plan()` call — cache hit/miss, plan hash, EXPLAIN text, planning
duration, and a slow-query flag — for plan-cache and slow-query observability.
