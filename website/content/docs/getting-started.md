---
title: Getting started
description: "Build fdb-go from source and run its SQL and record-store examples against a disposable local FoundationDB cluster."
weight: 1
---

Use a source checkout for this walkthrough. The website describes the development
tree; the older v0.1.0 release does not match these APIs and storage behavior.

## Prerequisites

- Git and the Go toolchain required by [`go.mod`](https://github.com/birdayz/fdb-go/blob/master/go.mod).
- Docker running locally, for the disposable single-node FoundationDB cluster.
- A shell that supports the commands below, such as Bash or Zsh.

The default backend is pure Go: you do not need cgo or a local `libfdb_c`
installation. You do need a running FoundationDB server.

## Check out the source

```sh
git clone https://github.com/birdayz/fdb-go.git
cd fdb-go
git rev-parse HEAD
```

Record that commit hash with your results. For repeatable evaluation, check out
a specific reviewed commit rather than following a moving branch.

## Run the SQL example

**Use a disposable cluster.** The SQL example recreates `/FRL/QUICKSTART` and
its schema template. Do not point it at a store you need to keep.

```sh
go build -o frl ./cmd/frl
FDB_CLUSTER_FILE="$(./frl fdb up)" || exit 1
export FDB_CLUSTER_FILE
go run ./example/sql
```

`frl fdb up` starts and configures the local cluster and prints the cluster-file
path. Exporting it lets the example connect to that cluster. The
[SQL example source](https://github.com/birdayz/fdb-go/tree/master/example/sql)
contains the complete setup, parameterized writes, queries, and error handling.
The [repository quickstart](https://github.com/birdayz/fdb-go#getting-started)
is the corresponding source-level guide.

## Try the Record Layer

With the same disposable cluster running:

```sh
go run ./example
```

The [record-store example](https://github.com/birdayz/fdb-go/blob/master/example/getting_started.go)
shows metadata setup and saving and loading records. It overwrites
its demo order with ID 1001; use only the disposable cluster from this walkthrough.

## Clean up

When finished, remove the local cluster:

```sh
./frl fdb down
```

This is a local development setup, not a production deployment recipe. If startup
fails, check Docker availability and the CLI's error output; do not substitute a
container-internal address into a host-side cluster file.

## Use the client on its own

For key-value transactions without the Record Layer or SQL engine, import
`fdb.dev/pkg/fdbgo/fdb`. See the [client connection example](https://github.com/birdayz/fdb-go#fdb-client)
and [client API notes](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/README.md).
Transaction callbacks can be retried; avoid external side effects inside them and
return read errors to the transaction wrapper.

## Before using existing data

Read [status and compatibility](/docs/maturity/), the
[compatibility boundaries](https://github.com/birdayz/fdb-go/blob/master/docs/compatibility.md),
and [upgrade guidance](https://github.com/birdayz/fdb-go/blob/master/docs/upgrade.md).
Go and Java interoperability depends on the exact formats and features used.

Record Layer and SQL builds can select Apple's C client with the `libfdbc` build
tag and cgo enabled. Direct users of `pkg/fdbgo/fdb` always use the pure-Go client.
See [backend selection](https://github.com/birdayz/fdb-go#fdb-client) for the build
requirements; changing the client backend does not remove higher-layer
compatibility boundaries.
