---
title: Record Layer
weight: 3
---

The Record Layer stores structured Protocol Buffer records with secondary indexes, record versions,
and transactional schema evolution on top of FoundationDB. Its on-disk format is **byte-identical to
Java Record Layer 4.12.11.0** — Go and Java applications read and write the same records on a shared
cluster.

## Save and load

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

## What it covers

- **Secondary indexes** — value, count, sum, and other index types; maintained transactionally.
- **Record versions** — stored inline at the `pk + -1` suffix (format version ≥ 6).
- **Split records** — values over 100 KB are chunked across suffixes; unsplit at suffix 0.
- **Continuations** — proto-wrapped cursor tokens (magic `6773487359078157740`), resumable scans.
- **Schema evolution** — add fields and indexes; online index builds backfill existing data.

Every one of these is exercised against the Java reference by the conformance suite — the wire format
is the project's hard line.

## Operations

For running it in production — connecting, online index builds, schema migration, backup, and
observability — see the [operator guide](https://github.com/birdayz/fdb-go/blob/master/docs/operations.md).
