---
title: Getting Started
weight: 1
---

## Install

```sh
go get fdb.dev/pkg/fdbgo/fdb
```

The default build is the pure-Go client. No cgo, no C library, just a static binary. You only need a reachable FDB cluster and its cluster file.

## Connect and run a transaction

```go
import "fdb.dev/pkg/fdbgo/fdb"

fdb.MustAPIVersion(730)
db, err := fdb.OpenDatabase("/etc/foundationdb/fdb.cluster")
if err != nil {
	log.Fatal(err)
}

db.Transact(func(tx fdb.WritableTransaction) (any, error) {
	tx.Set(fdb.Key("greeting"), []byte("hello"))
	return tx.Get(fdb.Key("greeting")).MustGet(), nil
})
```

The `fdb` package mirrors Apple's Go binding, so existing FoundationDB code ports with minimal changes.

{{< callout type="info" >}}
  Record Layer and SQL apps can build with `CGO_ENABLED=1 go build -tags libfdbc` to run on Apple's libfdb_c client instead of the pure-Go one. Both read and write byte-identical records against the same cluster, so you can switch the tag and keep sharing data.
{{< /callout >}}

## Next

For structured records and SQL on top of the client, see the [Record Layer and SQL guides](https://github.com/birdayz/fdb-go) in the repository. Before depending on it in production, read [Maturity & Status](maturity).
