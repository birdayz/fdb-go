# fdb-go — FoundationDB for Go

[![CI](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml/badge.svg)](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml)
[![Test Report](https://img.shields.io/badge/test_report-latest-2980b9)](https://fdb.dev/reports/latest.html)

**Use FoundationDB from Go — at the key-value, record, or SQL layer.**

fdb-go includes a native client with no cgo dependency, a Go port of Apple's
[Record Layer](https://github.com/FoundationDB/fdb-record-layer), and an embedded
`database/sql` engine. FoundationDB remains the database server; these libraries
run in your Go application. Use the client on its own or the layers above it.

Sharing record-store data with Java Record Layer 4.14.2.0 is a central goal,
with explicit [compatibility boundaries](docs/compatibility.md). This is not a
PostgreSQL-compatible SQL server or a replacement for the FoundationDB cluster.

This is an **unofficial, independent project**, not affiliated with, sponsored by,
or endorsed by Apple Inc. or the FoundationDB project. Apple and FoundationDB
names identify upstream projects and compatibility targets, not an endorsement.

[Get started](#getting-started) · [Examples](example/) ·
[Status](STATUS.md) · [Compatibility](docs/compatibility.md) ·
[Operator guide](docs/operations.md) · [Website](https://fdb.dev/)

## How it is built

This project is built with AI coding agents under human direction. A maintainer
sets priorities and review requirements. Agent instructions remain in the tree
(`CLAUDE.md`, `AGENTS.md`, `.claude/`).

The test infrastructure includes these external references and fault models:

- **Java conformance:** covered cases compare shared-store reads/writes and SQL
  behavior with Java Record Layer / Relational 4.14.2.0 ([`conformance/`](conformance/),
  [`pkg/relational/conformance/`](pkg/relational/conformance/)).
- **libfdb_c differential:** the pure-Go client runs the same operations as
  Apple's C client and is compared with it ([`pkg/fdbgo/bench/`](pkg/fdbgo/bench/)).
- **FoundationDB binding tester:** the official binding tester drives the Go
  bindings through a stack tester ([`cmd/fdb-stacktester/`](cmd/fdb-stacktester/)).
- **Deterministic simulation:** seeded operations and fault injection exercise
  an in-memory FDB model ([`pkg/dst/`](pkg/dst/),
  [`pkg/simfdb/`](pkg/simfdb/)).

## Choose a layer

| API | Use it for | Package |
|-----|------------|---------|
| **FoundationDB client** | Key-value transactions, range reads, and retries without cgo | [`pkg/fdbgo/fdb`](pkg/fdbgo/fdb) |
| **Record Layer** | Protobuf records, secondary indexes, schema metadata, and cursors | [`pkg/recordlayer`](pkg/recordlayer) |
| **SQL** | Queries through Go's `database/sql`, planned and executed in-process | [`pkg/relational/sqldriver`](pkg/relational/sqldriver) |

The SQL engine uses the Record Layer, which uses an FDB client. Record Layer and
SQL builds may select Apple's C client instead of the default pure-Go backend;
see [backend selection](#fdb-client).

## Status

**Pre-1.0; not declared production-ready.** Evaluate a pinned commit against your
schemas, queries, recovery requirements, and load. [STATUS.md](STATUS.md) describes
readiness and what the test lanes establish. These docs describe the development
tree, not the older v0.1.0 release. Read the [upgrade guide](docs/upgrade.md) before
opening existing data; older Go SQL storage is not automatically migrated.

## Getting started

Requires the Go toolchain specified in [go.mod](go.mod) and a running Docker daemon.
From a clone of this repository, build the CLI, start and configure a disposable
single-node cluster (`frl fdb up` prints its cluster-file path), then run the
complete SQL example and the record-store example:

```sh
go build -o frl ./cmd/frl &&
FDB_CLUSTER_FILE="$(./frl fdb up)" && export FDB_CLUSTER_FILE &&
go run ./example/sql &&
go run ./example
```

**Use a disposable cluster:** the SQL example recreates `/FRL/QUICKSTART` and
`QUICKSTART_TMPL`; the record-store example writes order `1001` under
`record_layer_demo`. Both programs return a nonzero exit status on failure.

- [`example/sql`](example/sql/main.go): domain/schema setup, parameterized writes,
  point queries, grouped aggregation, and checked row iteration.
- [`example/getting_started.go`](example/getting_started.go): protobuf metadata,
  saving records and typed loads inside transactions.

For an existing cluster, set `FDB_CLUSTER_FILE` to its cluster file instead of
starting one with `frl`. Do not run these destructive demos against data you need.
When finished with the disposable cluster:

```sh
./frl fdb down
```

## FDB client

The pure-Go client's API is modeled on Apple's Go binding. See the
[client documentation](pkg/fdbgo/README.md) for its supported surface and
limitations. A connection and transaction excerpt, inside an application function:

```go
import "fdb.dev/pkg/fdbgo/fdb"

if err := fdb.APIVersion(730); err != nil {
    return err
}
db, err := fdb.OpenDatabase(clusterFile)
if err != nil {
    return err
}
defer db.Close()
_, err = db.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
    tx.Set(fdb.Key("k"), []byte("v"))
    return tx.Get(fdb.Key("k")).Get()
})
return err
```

The callback may run again on a retryable error. Return read errors to the
transaction wrapper and keep external side effects outside the callback.

Record Layer and SQL backend selection is static per binary:

```sh
go build ./cmd/frl ./pkg/...  # pure-Go backend; no libfdb_c
CGO_ENABLED=1 go build -tags libfdbc ./...  # C compiler + matching libfdb_c headers/library
```

The build tag changes the transport, not the layers' storage format or compatibility
boundaries. Direct users of `pkg/fdbgo/fdb` always get the pure-Go client.

## Record Layer

With metadata, a keyspace, and a record configured (see the
[runnable example](example/getting_started.go)):

```go
_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
if err != nil {
    return err
}
```

For transactions, online index builds, schema evolution, backup, and observability,
see the [operator guide](docs/operations.md). The
[compatibility inventory](docs/compatibility.md) describes supported formats and
exceptions; a feature name alone is not an interoperability guarantee.

## SQL engine

The embedded `database/sql` engine uses a Cascades-based planner ported from
Java's relational layer. The [complete SQL example](example/sql/main.go) registers
a domain, creates a database and schema, inserts orders, and queries them.

Database paths have the form `/DOMAIN/DATABASE`. Register application domains
explicitly with `sqldriver.RegisterDomainIfNotExists("FRL")`. Unquoted SQL identifiers
fold to uppercase; DSN paths and schema names are case-sensitive. The example uses
`/FRL/QUICKSTART?schema=APP` and includes the required setup and error handling.

See [`FEATURE_MATRIX.md`](FEATURE_MATRIX.md) for the generated scenario inventory
and [`DIVERGENCES.md`](DIVERGENCES.md) for differences from Java. Tested features
are not promises about every combination of query shapes. SQL continuation tokens
are engine-private, not interchangeable between Go and Java.

## Compatibility and versions

| Component | Reference |
|-----------|-----------|
| **FoundationDB** | 7.3 protocol; client and test-cluster pin 7.3.77 |
| **Java Record Layer / Relational** | 4.14.2.0 |
| **Go toolchain** | [`go.mod`](go.mod) |
| **Bazel and dependencies** | [`.bazelversion`](.bazelversion), [`MODULE.bazel`](MODULE.bazel) |

The pure-Go client is not a multi-version client. FDB 8.0 compatibility is not
established. Before mixing Go and Java writers, check the
[compatibility matrix](docs/compatibility.md): collation, TEXT Unicode behavior,
vector layouts, synthetic types, and continuations need particular care.

## Evidence and performance

- [Client differentials](pkg/fdbgo/bench/) compare pure Go with `libfdb_c`.
- [Record Layer conformance](conformance/) exercises shared data with Java.
- [SQL conformance](pkg/relational/conformance/) compares query behavior with
  the Java relational engine.

A passing case establishes its tested scope, not universal parity. Read the
[CI run](https://github.com/birdayz/fdb-go/actions/workflows/ci.yml) for the exact
commit and executed lanes; a badge alone does not establish a full-suite pass.

Previous Go-vs-libfdb_c speedup and write-parity claims were withdrawn because the
measurements used obsolete GRV-cache semantics and mislabeled netem delay.
[Correction and benchmark methodology](pkg/fdbgo/bench/PERFORMANCE.md).
The separate historical R17 experiment below does not measure current master.

### Performance vs libfdb_c

R17 compared the **frozen R16 NEW patchset, not current master or the v0.2.0
cut**, with FDB 7.3.77's native client through CGo. Ratios are geometric means of
160 within-block NEW/CGo ratios per workload, not ratios of displayed medians.
The five endpoints were pre-registered; both 99% moving-block bootstrap
intervals (L8 and L16, 100,000 draws each) had to have upper bounds below 1.

| Measure | Workload | NEW / CGo | L8 99% CI | L16 99% CI |
|---|---|---:|---|---|
| CPU/transaction, startup through shutdown (E−A) | `batchset50` | 0.862469 | [0.850940, 0.874873] | [0.850373, 0.875916] |
| CPU/transaction, measured window (C−B) | `batchset50` | 0.788291 | [0.776788, 0.801047] | [0.776380, 0.801644] |
| Mean latency | `read1000` | 0.625737 | [0.603494, 0.650043] | [0.600815, 0.652733] |
| p99 latency | `read1000` | 0.507259 | [0.466695, 0.548761] | [0.463166, 0.549550] |
| CPU/transaction, startup through shutdown (E−A) | `read1000` | 0.908661 | [0.885677, 0.933150] | [0.883870, 0.934491] |

- One previously used Hetzner ccx43 instance (16 vCPUs), reimaged with Ubuntu
  24.04 and a fresh FDB 7.3.77 cluster. No independent host/fleet replication.
- 640 serial processes, one worker, `GOMAXPROCS=8`, Go 1.26.9, API 730.
  `read1000`: one Get/read-only transaction, 5,000 measured + 200 warmup;
  `batchset50`: 50 distinct 100-byte Sets/commit, 2,500 measured + 200 warmup.
- E−A includes startup, fixture, warmup, validation, CSV, forced GC/finalizers
  and shutdown; it excludes preceding identity hashing, final JSON and process
  exit. It is **not isolated steady-state CPU**. Deferred cleanup can cross the
  C−B window boundaries. Go heap counters do not measure the native C++ heap.
- NEW source: base `7922a2e5a8e3a2d6cf17dfee8b8134181c88b298` plus frozen patch
  SHA-256 `d0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85`;
  CGo source: `8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead`.
- Results archive SHA-256
  `4943768b532b2c57797c677f37b9381c10d922fbee81ef7821a3174a29d2e6f0`.
  [R17 tables](tools/clientbench/results/r17/TABLES.md) are a formatting-only copy
  from the retained experiment; named raw artifacts remain in the retained
  archive, not at the tables URL. [`tools/clientbench`](tools/clientbench/) (#822) is the
  harness; reproducing R17 requires its frozen sources, protocol and artifacts,
  not simply building current master.

The intervals depend on block-resampling assumptions on this one instance.
These results establish neither current-master performance nor general capacity,
universal Go-vs-C++ superiority, or source correctness.

## Building and contributing

A full clone was about 440 MiB during release preparation; history is retained.
`git clone --filter=blob:none https://github.com/birdayz/fdb-go.git` fetches
historical file contents on demand.

Development uses Bazel via bazelisk, `just`, and Docker for testcontainers:

```sh
just build      # compile + nogo lint
just test       # fast lane: unit + bounded integration tests
just test-full  # all Bazel test targets, including Java conformance + heavy suites
just gazelle    # regenerate BUILD files
just generate   # protobuf/code generation
```

The full lane runs targets at their default budgets, not every extended seed,
race, or active-fuzz configuration. See [STATUS.md](STATUS.md) for validation scope
and [CONTRIBUTING.md](CONTRIBUTING.md) to contribute. For questions and support
expectations, read [SUPPORT.md](SUPPORT.md). Report reproducible bugs through
[issues](https://github.com/birdayz/fdb-go/issues), and vulnerabilities according
to [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for upstream attribution.
