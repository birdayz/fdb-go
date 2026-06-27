---
title: Getting Started
weight: 1
---

## Install

```sh
go get fdb.dev/pkg/recordlayer
```

The default build uses the **pure-Go FoundationDB client** — no cgo, no C library, a static
binary. You only need a reachable FDB cluster and its cluster file.

{{< callout type="info" >}}
  Want Apple's C client instead? Build with `CGO_ENABLED=1 go build -tags libfdbc`. Both
  backends read and write byte-identical records against the same cluster.
{{< /callout >}}

## Connect

```go
import "fdb.dev/pkg/fdbgo/fdbclient"

db, err := fdbclient.Open("/etc/foundationdb/fdb.cluster")
if err != nil {
    log.Fatal(err)
}
```

## Your first record

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

## Next steps

{{< cards >}}
  {{< card link="/docs/record-layer" title="Record Layer" subtitle="Indexes, versions, continuations." >}}
  {{< card link="/docs/sql" title="SQL Engine" subtitle="Query with database/sql." >}}
{{< /cards >}}
