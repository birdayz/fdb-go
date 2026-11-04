---
title: Getting started
description: "Build fdb from current source, start a local FoundationDB, and run the SQL and Record Layer examples."
weight: 1
---

You need Go ([version](https://github.com/birdayz/fdb-go/blob/master/go.mod)) and
Docker. cgo and `libfdb_c` are not needed.

## Build the CLI from current source

`fdb` is the project's CLI for local development clusters, SQL, and Record Layer
operations (`fdb rl`). The renamed CLI has not had a tagged release yet; build
from a current checkout:

```sh
git clone https://github.com/birdayz/fdb-go.git &&
cd fdb-go &&
go build -o .tmpbuild/fdb ./cmd/fdb &&
export PATH="$PWD/.tmpbuild:$PATH" &&
fdb version
```

The installer and `go install fdb.dev/cmd/fdb@<tag>` become usable only after a
root-module release containing `cmd/fdb` is published; the installer additionally
requires that release's `fdb` binary archives. Existing older releases and
`@latest` do not provide this renamed CLI.

Configuration lives at `~/.fdb/config.yaml` (override with `FDB_CONFIG`);
`fdb config path` prints the effective path. See the
[CLI guide](/docs/cli/) for installation, connections, and commands.

To use the CLI without writing a Go application, follow the complete
[protobuf records and indexes](/docs/cli/protobuf/) or
[SQL and catalog discovery](/docs/cli/sql/) tutorial.

## Run the Go examples

```sh
FDB_CLUSTER_FILE="$(fdb dev up)" &&
export FDB_CLUSTER_FILE &&
go run ./example/sql &&
go run ./example
```

`fdb dev up` starts a single-node FoundationDB in Docker and prints the path of
its cluster file. If it fails, check that Docker is running.

The [SQL example](https://github.com/birdayz/fdb-go/tree/master/example/sql)
creates a schema, inserts rows, and queries them. The
[Record Layer example](https://github.com/birdayz/fdb-go/blob/master/example/getting_started.go)
saves and loads a record. Both recreate their data on every run, so they refuse
to run without `FDB_CLUSTER_FILE`.

## Use the client directly

Import `fdb.dev/pkg/fdbgo/fdb` for key-value transactions without the Record
Layer or SQL engine. See the [connection example](https://github.com/birdayz/fdb-go#native-go-client)
and [API notes](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/README.md).
Transaction functions can run more than once, so keep external side effects out
of them.

## Stop FoundationDB

```sh
fdb dev down
```

## Next

- [CLI command reference](/docs/cli/reference/) and [connections and metadata](/docs/cli/metadata/).
- [Status & compatibility](/docs/maturity/) before using an existing store or mixing Go and Java writers.
- [Backend selection](https://github.com/birdayz/fdb-go/blob/master/docs/operations.md#the-c-client-escape-hatch) to run the Record Layer and SQL engine on Apple's C client (`libfdbc` build tag).
