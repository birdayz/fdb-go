# fdb-go — FoundationDB for Go

[![CI](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml/badge.svg)](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml)
[![Test Report](https://img.shields.io/badge/test_report-latest-2980b9)](https://fdb-record-layer-go-reports.fsn1.your-objectstorage.com/reports/master/latest.html)

Go port of Apple's [FoundationDB Record Layer](https://github.com/FoundationDB/fdb-record-layer).
Wire-compatible with Java Record Layer 4.12.11.0 — Go and Java applications can read
and write the same data on a shared FDB cluster.

## Status

**Pre-1.0. Not yet declared production-ready — pin a commit and run the suites below
before relying on it.** Maturity varies by layer:

| Layer | Maturity | Notes |
|-------|----------|-------|
| **Record store** (CRUD, indexes, versions, continuations, split records) | **Most mature** | Wire-compatibility is the project's hard line, exercised by the Java conformance + binding-stress suites. This is the part to trust first. |
| **Cascades SQL engine** | **Usable, evolving** | Wide SQL surface (see below) validated by a cross-engine differential harness, but still has open correctness items — consult the conformance report and `TODO.md` before depending on a given query shape. |
| **Pure-Go FDB client** (`pkg/fdbgo`) | **Youngest** | Reimplements the FDB wire protocol from scratch (RYW, retries, `commit_unknown_result`). Validated against libfdb_c via the binding tester. It is the default backend; if you'd rather link Apple's C client, the `CGO_ENABLED=1 ... -tags libfdbc` build flag swaps it in — both read/write byte-identical records (see the build commands below). |

Before production use: pin a commit, run the conformance + differential + stress
suites against your workload, and review `PRODUCTION_READINESS.md` /
`TODO-production.md` for the current gap list. Report issues per `SECURITY.md`.
For running it — connecting, transactions, online index builds, schema evolution,
backup, and observability — see the [operator guide](docs/operations.md).

## Target versions

| Component | Version | Notes |
|-----------|---------|-------|
| **FoundationDB** | **7.3.77** | Client library + headers. Go bindings pinned to `release-7.3` branch. |
| **Java Record Layer** | **4.12.11.0** | Wire compatibility target. Conformance tests run against this version. |
| **Go** | **1.26.4** | Minimum Go version (kept current with stdlib security patches; `govulncheck` CI gates this). |
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

## FDB client backend (pure-Go vs libfdb_c)

The record layer runs on either of two wire-compatible FDB clients; a **build tag** picks
one (there is no runtime flag — the choice is static per binary). Application code is
backend-agnostic and opens through `fdbclient.Open`:

```go
import "fdb.dev/pkg/fdbgo/fdbclient"

db, _ := fdbclient.Open(clusterFile)            // backend-agnostic
rl := recordlayer.NewFDBDatabaseWithBackend(db)
```

```sh
go build ./...                        # default: the from-scratch pure-Go client (no cgo, no libfdb_c)
CGO_ENABLED=1 go build -tags libfdbc  # Apple's libfdb_c client (the escape hatch)
```

Exactly one client is linked, so a default build never pulls in cgo or the C library;
`fdbclient.Backend` (`"pure-go"` / `"libfdb_c"`) reports which one a binary carries. Both
clients read and write byte-identical records, index entries, and continuations against the
same cluster — proven by a cross-backend differential suite — so you can flip the tag and
keep sharing data (with each other, and with Java/C apps). This is the same idiom the
standard library uses for its `netgo`/`netcgo` resolver split and the sqlite ecosystem uses
to swap mattn/go-sqlite3 (cgo) for modernc.org/sqlite (pure-Go).

## SQL engine

Built-in SQL engine via Go's `database/sql` interface. Queries are optimized by a
Cascades-based query planner ported from Java's `fdb-relational-core`.

```go
import _ "fdb.dev/pkg/relational/sqldriver"

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

Supported SQL (the authoritative, tested surface is the yamsql scenarios under
`pkg/relational/conformance/yamsql/testdata/` + `DIVERGENCES.md` **at HEAD**; the list
below is a hand summary of those, not a separately-maintained source of truth). For an
exhaustive, auto-generated inventory of every scenario by feature area, see
[`FEATURE_MATRIX.md`](FEATURE_MATRIX.md) (regenerated by `just feature-matrix`):
- SELECT with WHERE, ORDER BY (ASC/DESC, including mixed directions), DISTINCT,
  GROUP BY, HAVING, LIMIT / OFFSET
- Aggregates: COUNT, SUM, MIN, MAX, AVG
- JOINs: INNER, comma-join / self-join, and LEFT / RIGHT / FULL OUTER JOIN
  (outer joins are a Go-only read-side extension — Java's SQL layer has none; wire
  compat is unaffected)
- Subqueries in WHERE: EXISTS / NOT EXISTS, IN (SELECT ...), and correlated scalar
  subqueries (Go-only read-side extensions)
- CTEs: WITH ... AS (SELECT ...), including chained CTEs
- UNION ALL
- INSERT, UPDATE, DELETE
- CASE, COALESCE, CAST, arithmetic, scalar functions (e.g. UPPER, LOWER)
- Computed projections with aliases

ORDER BY works without an index via a Go-only bounded in-memory sort
(`RecordQueryInMemorySortPlan`, beyond Java's index-only Cascades); a supporting
index/PK avoids the sort, and an unbounded ORDER BY without LIMIT is capped to
avoid OOM. Self-joins and CTE+JOINs correctly resolve alias-qualified columns.

Not yet supported in the SQL engine:
- A plain CTE referenced inside a UNION branch (recursive CTEs, which use UNION
  internally, do work)
- `IN (SELECT ...)` in DML WHERE (rejected; rewrite as a correlated `EXISTS`)
- General window functions (matching Java — only `ROW_NUMBER() ... QUALIFY` for
  vector K-NN search exists; see TODO.md)
- Synthetic record types (JoinedRecordType, UnnestedRecordType)

## What works

Records, indexes, cursors, and all the plumbing needed to share data with Java:

- **CRUD** — save, load, delete, scan, existence checks, typed stores
- **Indexes** — VALUE, VERSION, RANK, COUNT, SUM, MIN_EVER, MAX_EVER, MAX_EVER_VERSION, COUNT_NOT_NULL, COUNT_UPDATES, PERMUTED_MIN, PERMUTED_MAX, TEXT, BITMAP_VALUE, MULTIDIMENSIONAL, TIME_WINDOW_LEADERBOARD, VECTOR (HNSW)
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
- **Instrumentation** — StoreTimer with timed events and counters matching Java's FDBStoreTimer
- **Store state caching** — FDBRecordStoreStateCache interface with PassThroughStoreStateCache default

## What doesn't (yet)

- Synthetic record types (JoinedRecordType, UnnestedRecordType)

Full gap analysis in [TODO.md](TODO.md).

## Conformance

Wire compatibility is verified by a conformance suite that runs both Go and Java
(Record Layer 4.12.11.0) against the same FDB instance, cross-validating reads and
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

434 conformance specs (Go↔Java cross-validation), 5320 Go test functions, and 2702
Ginkgo specs against real FDB via testcontainers. **8000+ total test entry points.**
1579-entry SQL corpus runs through the Go engine with zero failures.

| Area | Conformance specs |
|------|------------------:|
| CRUD + existence + isolation + conflicts | 49 |
| Multi-type records (Customer) | 15 |
| Split records | 10 |
| Scanning (forward, reverse, limits, tuple ordering) | 13 |
| Continuation tokens (record + index level) | 6 |
| VALUE indexes (single, composite, fan-out, covering) | 22 |
| COUNT/SUM/MIN_EVER/MAX_EVER indexes | 38 |
| COUNT_NOT_NULL/COUNT_UPDATES/CLEAR_WHEN_ZERO | 12 |
| MAX_EVER_VERSION index | 7 |
| PERMUTED_MIN/MAX indexes | 10 |
| RANK index | 14 |
| TEXT index | 12 |
| BITMAP_VALUE index | 6 |
| MULTIDIMENSIONAL index | 15 |
| VECTOR index (HNSW) | 18 |
| TIME_WINDOW_LEADERBOARD index | 11 |
| Record versioning | 4 |
| Record counting | 6 |
| RangeSet wire format | 4 |
| Store header (v1 + v2), index state, lifecycle | 28 |
| DeleteAllRecords / DeleteRecordsWhere | 10 |
| OnlineIndexer | 7 |
| RecordMetaData proto serialization | 21 |
| TypedRecord cross-language encoding | 11 |

## Getting started

```sh
# 1. Start FoundationDB (Docker)
docker run -d --name fdb -p 4500:4500 foundationdb/foundationdb:7.3.77

# 2. Get the cluster file
docker exec fdb cat /var/fdb/fdb.cluster > /tmp/fdb.cluster

# 3. Use from Go
go get fdb.dev/pkg/relational/sqldriver
```

```go
package main

import (
    "database/sql"
    "fmt"
    _ "fdb.dev/pkg/relational/sqldriver"
)

func main() {
    db, _ := sql.Open("fdbsql", "fdbsql:///myapp?cluster_file=/tmp/fdb.cluster&schema=main")
    db.Exec("CREATE DATABASE /myapp")
    db.Exec(`CREATE SCHEMA TEMPLATE app CREATE TABLE Users (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))`)
    db.Exec("CREATE SCHEMA /myapp/main WITH TEMPLATE app")

    db.Exec("INSERT INTO Users VALUES (1, 'Alice'), (2, 'Bob')")

    rows, _ := db.Query("SELECT id, name FROM Users ORDER BY id")
    for rows.Next() {
        var id int64; var name string
        rows.Scan(&id, &name)
        fmt.Printf("%d: %s\n", id, name)
    }
}
```

Runnable, CI-compiled examples live under `example/`:
- [`example/sql`](example/sql/main.go) — the SQL path above, fleshed out (DDL, parameterized
  INSERT, point query, `GROUP BY` aggregate over an index). `go run ./example/sql`.
- [`example/getting_started.go`](example/getting_started.go) — the lower-level record-store API
  (metadata, typed `SaveRecord`/`loadRecord`, index scans).

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
pkg/recordlayer/        Record Layer implementation (CRUD, indexes, cursors, schema)
pkg/relational/         SQL engine (parser, Cascades optimizer, executor, database/sql driver)
pkg/fdbgo/              Pure Go FDB client (wire protocol, no CGo)
gen/                    Generated protobuf Go code
proto/apple/            Apple's original proto definitions
conformance/            Go↔Java cross-validation tests + Java conformance server
```

### Running specific tests

```sh
bazelisk test //pkg/recordlayer:recordlayer_test \
    --test_arg="--ginkgo.focus=CountIndex" --test_output=streamed
```

## License

See [LICENSE](LICENSE).
