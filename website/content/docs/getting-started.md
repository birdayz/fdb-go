---
title: Getting started
description: "Install frl, start a local FoundationDB, and run the SQL and Record Layer examples."
weight: 1
---

You need Go ([version](https://github.com/birdayz/fdb-go/blob/master/go.mod)) and
Docker. cgo and `libfdb_c` are not needed.

## Install frl

```sh
curl -fsSL https://fdb.dev/install.sh | sh
```

`frl` is the project's CLI, a single static binary. The script installs it without
sudo. With Go instead: `go install fdb.dev/cmd/frl@latest`.

## Start FoundationDB

```sh
frl fdb up
```

Starts a single-node FoundationDB in Docker and writes its cluster file to
`~/.frl/frl-fdb.cluster`. If it fails, check that Docker is running.

## Run the examples

```sh
git clone https://github.com/birdayz/fdb-go.git && cd fdb-go
FDB_CLUSTER_FILE=~/.frl/frl-fdb.cluster go run ./example/sql
FDB_CLUSTER_FILE=~/.frl/frl-fdb.cluster go run ./example
```

The [SQL example](https://github.com/birdayz/fdb-go/tree/master/example/sql)
creates a schema, inserts rows, and queries them. The
[Record Layer example](https://github.com/birdayz/fdb-go/blob/master/example/getting_started.go)
saves and loads a record. Both recreate their data on every run, so they refuse
to run without `FDB_CLUSTER_FILE`.

## Use the client directly

Import `fdb.dev/pkg/fdbgo/fdb` for key-value transactions without the Record
Layer or SQL engine. See the [connection example](https://github.com/birdayz/fdb-go#fdb-client)
and [API notes](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/README.md).
Transaction functions can run more than once, so keep external side effects out
of them.

## Stop FoundationDB

```sh
frl fdb down
```

## Next

- [Status & compatibility](/docs/maturity/) before using an existing store or mixing Go and Java writers.
- [Backend selection](https://github.com/birdayz/fdb-go#fdb-client) to run the Record Layer and SQL engine on Apple's C client (`libfdbc` build tag).
