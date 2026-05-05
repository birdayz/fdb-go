# fdb-record-layer-go

[![CI](https://github.com/birdayz/fdb-record-layer-go/actions/workflows/ci.yml/badge.svg)](https://github.com/birdayz/fdb-record-layer-go/actions/workflows/ci.yml)
[![Test Report](https://img.shields.io/badge/test_report-latest-2980b9)](https://fdb-record-layer-go-reports.fsn1.your-objectstorage.com/reports/master/latest.html)

Go port of Apple's [FoundationDB Record Layer](https://github.com/FoundationDB/fdb-record-layer).
Wire-compatible with Java Record Layer 4.11.1.0 — Go and Java applications can read
and write the same data on a shared FDB cluster.

## Target versions

| Component | Version | Notes |
|-----------|---------|-------|
| **FoundationDB** | **7.3.75** | Client library + headers. Go bindings pinned to `release-7.3` branch. |
| **Java Record Layer** | **4.11.1.0** | Wire compatibility target. Conformance tests run against this version. |
| **Go** | **1.26.1** | Minimum Go version. |
| **Bazel** | **9.0.1** | Build system. Pinned in `.bazelversion`. |

FDB 8.0 is not yet released. When it ships, the Go bindings and client library should be upgraded together.

## Why

The Record Layer gives you structured records, secondary indexes, and transactional
schema evolution on top of FoundationDB's ordered key-value store. This port brings
that to Go without sacrificing interoperability with existing Java deployments.

## Performance

Includes a **pure Go FDB client** that speaks the FDB wire protocol directly — no CGo, no C library dependency.

Both clients run in the same process against the same FDB testcontainer, same keys. [`TestBenchmarkSanity`](pkg/fdbgo/bench/bench_test.go) verifies byte-identical results.

| Benchmark | fdb-go | Apple CGo | Diff |
|---|---:|---:|---|
| Get (100 B) | 60 us | 218 us | **3.6x** |
| Get (1 KB) | 61 us | 209 us | **3.4x** |
| Get (10 KB) | 69 us | 217 us | **3.1x** |
| GetRange (100 keys) | 92 us | 363 us | **3.9x** |
| Sustained read throughput | 430 MB/s | 191 MB/s | **2.3x** |
| Set + Commit | 1,008 us | 1,005 us | 1.0x |

With simulated network latency ([tc netem](pkg/fdbgo/bench/bench_test.go)):

| RTT | fdb-go | Apple CGo | Diff |
|---|---:|---:|---|
| 2 ms | 1,080 us | 2,726 us | **2.5x** |
| 10 ms | 5,254 us | 12,635 us | **2.4x** |
| 1,000 ms | 1,005 ms | 1,006 ms | 1.0x |

Reads 2-4x faster on localhost, **still 2.4x at 10 ms RTT**, converges to parity at extreme latency. Writes at parity. See [`PERFORMANCE.md`](pkg/fdbgo/bench/PERFORMANCE.md) for the analysis.

## Usage

```go
db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
    store, err := recordlayer.NewStoreBuilder().
        SetMetaDataProvider(metadata).
        SetContext(rtx).
        SetSubspace(keyspace).
        CreateOrOpen()
    if err != nil {
        return nil, err
    }

    return store.SaveRecord(order)
})
```

Type-safe access via generics:

```go
typed := recordlayer.NewTypedFDBRecordStore[*pb.Order](store)
order, err := typed.LoadRecord(ctx, primaryKey)
```

## SQL engine

Built-in SQL engine via Go's `database/sql` interface. Queries are optimized by a
Cascades-based query planner ported from Java's `fdb-relational-core`.

```go
import _ "github.com/birdayz/fdb-record-layer-go/pkg/relational/sqldriver"

db, _ := sql.Open("fdbsql", "fdbsql:///mydb?cluster_file=/etc/foundationdb/fdb.cluster&schema=main")

// DDL
db.Exec("CREATE DATABASE /mydb")
db.Exec(`CREATE SCHEMA TEMPLATE app_tmpl
    CREATE TABLE Users (id BIGINT NOT NULL, name STRING, email STRING, PRIMARY KEY (id))
    CREATE INDEX idx_email ON Users (email)`)
db.Exec("CREATE SCHEMA /mydb/main WITH TEMPLATE app_tmpl")

// DML
db.Exec("INSERT INTO Users (id, name, email) VALUES (1, 'Alice', 'alice@example.com')")
db.Exec("UPDATE Users SET name = 'Bob' WHERE id = 1")

// Queries — Cascades optimizer picks index scans, sort elimination, streaming aggregation
rows, _ := db.Query("SELECT name FROM Users WHERE email = 'alice@example.com'")
rows, _ = db.Query("SELECT name FROM Users ORDER BY id DESC")  // reverse PK scan
rows, _ = db.Query("SELECT email, COUNT(*) FROM Users GROUP BY email ORDER BY email ASC")
```

Supported SQL:
- SELECT with WHERE, ORDER BY (ASC/DESC), DISTINCT, GROUP BY, HAVING
- Aggregates: COUNT, SUM, MIN, MAX, AVG
- JOINs: INNER JOIN, comma-join (including self-joins)
- CTEs: WITH ... AS (SELECT ...) — including chained CTEs
- UNION ALL
- INSERT, UPDATE, DELETE
- CASE, COALESCE, CAST, arithmetic expressions
- Computed projections with aliases

ORDER BY requires a supporting index (no physical sort operator, matching Java's Cascades architecture).
Self-joins and CTE+JOINs correctly resolve alias-qualified column references.

## What works

Records, indexes, cursors, and all the plumbing needed to share data with Java:

- **CRUD** — save, load, delete, scan, existence checks, typed stores
- **Indexes** — VALUE, VERSION, RANK, COUNT, SUM, MIN_EVER, MAX_EVER, MAX_EVER_VERSION, COUNT_NOT_NULL, COUNT_UPDATES, PERMUTED_MIN, PERMUTED_MAX
- **Covering indexes** — KeyWithValueExpression (value columns stored in FDB value)
- **Index operations** — scan (BY_VALUE, BY_RANK, BY_GROUP), rebuild, online build (BY_RECORDS), state management (READABLE/WRITE_ONLY/DISABLED/READABLE_UNIQUE_PENDING)
- **Split records** — automatic chunking at 100KB, transparent reassembly
- **Record versioning** — 12-byte versions (10 global versionstamp + 2 local)
- **Cursors** — concat, map, filter, skip, limit, union, intersection, dedup, flatmap, chained, auto-continuing, fallback
- **Continuations** — cross-platform cursor resume tokens (record and index level)
- **Scan limits** — time, byte, and record scan limits
- **Transactions** — configurable retry with exponential backoff, commit hooks, conflict reporting
- **Schema evolution** — MetaDataValidator, MetaDataEvolutionValidator
- **Bulk operations** — DeleteAllRecords, DeleteRecordsWhere, record counting (atomic)
- **Aggregate functions** — EvaluateAggregateFunction (COUNT, SUM, MIN, MAX, RANK functions)
- **Store management** — format version 14, store locking (FORBID_RECORD_UPDATE, FULL_STORE), incarnation, header user fields
- **Key expressions** — Field, RecordType, Empty, Composite (Then), Nesting, FanOut, Grouping, FunctionKey, KeyWithValue, Version

## What doesn't (yet)

- TEXT index (full-text with tokenizers)
- BITMAP_VALUE, MULTIDIMENSIONAL, VECTOR, TIME_WINDOW_LEADERBOARD indexes
- Store state caching
- Timer/instrumentation
- Synthetic record types (JoinedRecordType, UnnestedRecordType)

Full gap analysis in [TODO.md](TODO.md).

## Conformance

Wire compatibility is verified by a conformance suite that runs both Go and Java
(Record Layer 4.11.1.0) against the same FDB instance, cross-validating reads and
writes bidirectionally.

### Wire format

All 10 keyspace constants match the Java implementation:

| Subspace | ID | Purpose |
|----------|----|---------|
| `StoreInfoKey` | 0 | Store header (format version, metadata) |
| `RecordKey` | 1 | Record data |
| `IndexKey` | 2 | Index entries |
| `IndexSecondarySpaceKey` | 3 | Secondary index data (RANK, PERMUTED) |
| `RecordCountKey` | 4 | Atomic record counts |
| `IndexStateSpaceKey` | 5 | Index lifecycle state |
| `IndexRangeSpaceKey` | 6 | Index build range tracking |
| `IndexUniquenessViolationsKey` | 7 | Deferred uniqueness violations |
| `RecordVersionKey` | 8 | Inline record versions |
| `IndexBuildSpaceKey` | 9 | Index build metadata |

Tuple encoding, split record layout, continuation token format, and index entry
structure are all verified against Java.

### Test coverage

315 conformance specs (Go↔Java cross-validation) and 838 unit/integration specs
against real FDB via testcontainers. **1153 total specs.**

| Area | Conformance specs |
|------|------------------:|
| CRUD + existence + isolation + conflicts | 49 |
| Multi-type records (Customer) | 12 |
| Split records | 9 |
| Scanning (forward, reverse, limits) | 12 |
| Continuation tokens (record + index level) | 6 |
| VALUE indexes (single, composite, fan-out, covering) | 22 |
| COUNT/SUM/MIN_EVER/MAX_EVER indexes | 38 |
| COUNT_NOT_NULL/COUNT_UPDATES/CLEAR_WHEN_ZERO | 12 |
| MAX_EVER_VERSION index | 7 |
| PERMUTED_MIN/MAX indexes | 10 |
| VERSION index | varies |
| RANK index | 14 |
| Record versioning | 4 |
| Record counting | 6 |
| RangeSet wire format | 4 |
| Store header (v1 + v2), index state, lifecycle | 25 |
| DeleteAllRecords / DeleteRecordsWhere | 11 |
| OnlineIndexer | 7 |
| RecordMetaData proto serialization | 21 |
| TypedRecord cross-language encoding | 11 |

## Building

Requires Bazel 9+ (via bazelisk) and Docker (for testcontainers).

```sh
just build      # compile + nogo lint (20 analyzers)
just test       # full test suite against real FDB
just gazelle    # regenerate BUILD files
just generate   # buf proto codegen
```

### Project layout

```
pkg/recordlayer/    Main implementation
gen/                Generated protobuf Go code
proto/apple/        Apple's original proto definitions
conformance/        Go↔Java cross-validation tests + Java conformance server
```

### Running specific tests

```sh
bazelisk test //pkg/recordlayer:recordlayer_test \
    --test_arg="--ginkgo.focus=CountIndex" --test_output=streamed
```

## License

See [LICENSE](LICENSE).
