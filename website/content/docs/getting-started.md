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

`fdbclient.Backend` reports which client a binary carries (`"pure-go"` / `"libfdb_c"`).

## Next

Record-layer and SQL usage, the operator guide, and runnable examples live in the
[repository](https://github.com/birdayz/fdb-go). Before depending on it in production, read
[Maturity & Status](maturity).
