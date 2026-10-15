---
title: Getting started
description: "Build fdb-go from source and run the SQL and Record Layer examples against a local FoundationDB cluster."
weight: 1
---

## Prerequisites

- Go, at the version in [`go.mod`](https://github.com/birdayz/fdb-go/blob/master/go.mod)
- Docker, for the local FoundationDB cluster

cgo and `libfdb_c` are not needed.

## Get the source

```sh
git clone https://github.com/birdayz/fdb-go.git
cd fdb-go
```

## Run the SQL example

```sh
go build -o frl ./cmd/frl
export FDB_CLUSTER_FILE="$(./frl fdb up)"
go run ./example/sql
```

`frl fdb up` starts a single-node FoundationDB cluster in Docker and prints its
cluster-file path. The examples delete and recreate their data, so they refuse to
run unless `FDB_CLUSTER_FILE` is set.

Source: [`example/sql`](https://github.com/birdayz/fdb-go/tree/master/example/sql).

## Run the Record Layer example

```sh
go run ./example
```

It saves and loads a demo order (ID 1001) on the same local cluster. Source:
[`example/getting_started.go`](https://github.com/birdayz/fdb-go/blob/master/example/getting_started.go).

## Use the client directly

Import `fdb.dev/pkg/fdbgo/fdb` for key-value transactions without the Record
Layer or SQL engine. See the [connection example](https://github.com/birdayz/fdb-go#fdb-client)
and [API notes](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/README.md).
Transaction functions can run more than once, so keep external side effects out
of them.

## Stop the cluster

```sh
./frl fdb down
```

If `frl fdb up` fails, check that Docker is running.

## Next

- [Status & compatibility](/docs/maturity/) before using an existing store or mixing Go and Java writers.
- [Backend selection](https://github.com/birdayz/fdb-go#fdb-client) to run the Record Layer and SQL engine on Apple's C client (`libfdbc` build tag).
