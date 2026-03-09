# fdb-record-layer-go

Go port of Apple's [FoundationDB Record Layer](https://github.com/FoundationDB/fdb-record-layer).
Wire-compatible with the Java implementation — Go and Java applications can read and write
the same data on a shared FDB cluster.

```
go get github.com/birdayz/fdb-record-layer-go
```

## Why

The Record Layer gives you structured records, secondary indexes, and transactional
schema evolution on top of FoundationDB's ordered key-value store. This port brings
that to Go without sacrificing interoperability with existing Java deployments.

## Usage

```go
db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
    store, err := recordlayer.NewFDBRecordStoreBuilder().
        SetMetaData(metadata).
        SetContext(rtx).
        SetKeySpacePath(path).
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

## What works

Records, indexes, and all the plumbing needed to share data with Java:

- **CRUD** — save, load, delete, scan, existence checks, typed stores
- **Indexes** — VALUE, COUNT, SUM; scan, rebuild, online build (BY_RECORDS)
- **Split records** — automatic chunking at 100KB, transparent reassembly
- **Record versioning** — 12-byte versions (10 global versionstamp + 2 local)
- **Continuations** — cross-platform cursor resume tokens
- **Transactions** — configurable retry, commit hooks, conflict reporting

## What doesn't (yet)

- Most index types (RANK, TEXT, MIN_EVER, MAX_EVER, BITMAP, VECTOR, ...)
- Schema evolution validation (`MetaDataEvolutionValidator`)
- Bulk conditional delete (`deleteRecordsWhereAsync`)
- Index build progress tracking / crash resume
- Store state caching, timer instrumentation

Full gap analysis in [TODO.md](TODO.md).

## Conformance

Wire compatibility is verified by a conformance suite that runs both Go and Java
against the same FDB instance, cross-validating reads and writes bidirectionally.

### Wire format

All 10 keyspace constants match the Java implementation:

| Subspace | ID | Purpose |
|----------|----|---------|
| `StoreInfoKey` | 0 | Store header (format version, metadata) |
| `RecordKey` | 1 | Record data |
| `IndexKey` | 2 | Index entries |
| `IndexSecondarySpaceKey` | 3 | Secondary index data |
| `RecordCountKey` | 4 | Atomic record counts |
| `IndexStateSpaceKey` | 5 | Index lifecycle state |
| `IndexRangeSpaceKey` | 6 | Index build range tracking |
| `IndexUniquenessViolationsKey` | 7 | Deferred uniqueness violations |
| `RecordVersionKey` | 8 | Inline record versions |
| `IndexBuildSpaceKey` | 9 | Index build metadata |

Tuple encoding, split record layout, continuation token format, and index entry
structure are all verified against Java. Details in [reports/subspace_wire_compat.md](reports/subspace_wire_compat.md).

### Test coverage

149 conformance specs (Go↔Java cross-validation) across 18 test files,
plus 460 unit/integration specs against real FDB via testcontainers.

| Area | Conformance specs |
|------|------------------:|
| CRUD + existence checks | 49 |
| Multi-type records | 15 |
| Split records | 10 |
| Scanning (forward, reverse, limits) | 18 |
| Continuation tokens | 3 |
| Indexes (VALUE, COUNT, fan-out, composite, rebuild) | 25 |
| Record versioning | 4 |
| Record counting | 6 |
| Isolation + conflicts | 17 |
| Delete operations | 8 |

Open conformance gaps tracked in [TODO.md](TODO.md): SUM index, RangeSet wire
format, DeleteAllRecords, store header, index state persistence, FormerIndex.

See [reports/feature_completeness.md](reports/feature_completeness.md) for a
method-by-method comparison against Java's `FDBRecordStore`.

## Contributing

### Building

Requires Bazel 8+ (via bazelisk) and Docker (for testcontainers).

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
conformance/        Go↔Java cross-validation tests
reports/            Audit reports (wire compat, coverage, completeness)
```

### Running specific tests

```sh
bazelisk test //pkg/recordlayer:recordlayer_test \
    --test_arg="--ginkgo.focus=CountIndex" --test_output=streamed
```

## License

See [LICENSE](LICENSE).
