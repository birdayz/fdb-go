---
title: fdb CLI
description: "Use fdb to run local FoundationDB, work with protobuf records and indexes, and query SQL schemas."
weight: 2
---

`fdb` is the command-line tool for FoundationDB, the Record Layer, and SQL.
It starts local development clusters, reads and writes protobuf records,
manages indexes, and runs SQL without an application server or cgo.

{{< cards >}}
  {{< card link="protobuf" title="Protobuf records and indexes" icon="code" subtitle="Compile an annotated schema, work with records, and inspect indexes." >}}
  {{< card link="sql" title="SQL and catalog discovery" icon="terminal" subtitle="Create a SQL schema, query rows, and inspect its Record Layer store." >}}
  {{< card link="metadata" title="Connections and metadata" icon="cog" subtitle="Configure clusters, contexts, descriptor sets, and persisted metadata." >}}
  {{< card link="reference" title="Command reference" icon="book-open" subtitle="Commands, flags, output formats, write guards, and completions." >}}
{{< /cards >}}

## Install

The renamed CLI has not had a tagged release yet. Build from a current checkout
with the Go version in
[`go.mod`](https://github.com/birdayz/fdb-go/blob/master/go.mod):

```sh
git clone https://github.com/birdayz/fdb-go.git &&
cd fdb-go &&
go build -o .tmpbuild/fdb ./cmd/fdb &&
export PATH="$PWD/.tmpbuild:$PATH" &&
fdb version &&
fdb --help
```

The release installer (`curl -fsSL https://fdb.dev/install.sh | sh`) and
`go install fdb.dev/cmd/fdb@<tag>` require a root-module release containing
`cmd/fdb`; the installer also needs that release's published `fdb` archives.
Those release artifacts are not available yet. Existing older releases and
`@latest` cannot supply this renamed CLI; use the checkout build above.

## Start a local cluster

With Docker running:

```sh
fdb dev up &&
fdb tx read-version &&
fdb dev status
```

`fdb dev up` starts and initializes a single-node FoundationDB container, writes
its cluster file, and activates a CLI context. Progress goes to stderr; stdout
is the cluster-file path. You do not need to create a config file first.

The container publishes its port on `127.0.0.1`. It is an unauthenticated,
throwaway development database, not a production deployment or an isolation
boundary against local users. A local Docker socket is required; remote Docker
daemons and clients in other containers are not supported by this helper.

For a cluster you already run, see [connections and contexts](/docs/cli/metadata/#connect-to-a-cluster).

## Choose a workflow

- **Native protobuf records:** follow the [Record Layer tutorial](/docs/cli/protobuf/).
  Give the CLI an annotated protobuf descriptor set; no application plugin or
  metadata dumper is required.
- **SQL:** follow the [SQL tutorial](/docs/cli/sql/). SQL schemas use the
  relational catalog, not a descriptor-set flag.
- **An existing application store:** select its cluster, keyspace, and
  [metadata source](/docs/cli/metadata/#choose-a-metadata-source).

## Command groups

| Command | Purpose |
|---|---|
| `fdb rl` | Record Layer records, indexes, stores, metadata, statistics, keyspaces, and status |
| `fdb sql` | Interactive SQL, one-shot statements, and SQL scripts |
| `fdb tx` | Transaction utilities such as `read-version` |
| `fdb dev` | Local FoundationDB development containers |
| `fdb config` | Config files and named contexts |
| `fdb version` | Binary and Go build information |
| `fdb completion` | Shell-completion scripts |

Record Layer operations live under `fdb rl`, including relational catalog
discovery at `fdb rl meta catalog`. Use `fdb <command> --help` for help on a
command, or read the [full reference](/docs/cli/reference/).

## Local files and cleanup

Configuration defaults to `~/.fdb/config.yaml`; `FDB_CONFIG` selects another
path. `fdb config path` prints the path in use. Interactive SQL history lives
at `~/.fdb/sql_history`.

When you are finished with the default development cluster:

```sh
fdb dev down
```

This removes the container and its data. It leaves the CLI context and cluster
file in place. For a named instance, pass the same `--name` used at startup.
