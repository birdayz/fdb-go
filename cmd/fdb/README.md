# fdb

CLI for FoundationDB, the Record Layer, and SQL. Run local development clusters,
work with protobuf records and indexes under `fdb rl`, and query relational
schemas with `fdb sql`.

**[CLI documentation](https://fdb.dev/docs/cli/)** is the canonical user guide:

- [Protobuf records and indexes](https://fdb.dev/docs/cli/protobuf/) — a complete
  annotated-schema tutorial, without an application plugin or metadata dumper.
- [SQL and catalog discovery](https://fdb.dev/docs/cli/sql/) — SQL queries and
  Record Layer inspection of the same store.
- [Connections and metadata](https://fdb.dev/docs/cli/metadata/) — contexts,
  descriptor sets, persisted metadata, and SQL catalog addressing.
- [Command reference](https://fdb.dev/docs/cli/reference/) — commands, flags,
  output formats, write guards, and shell completions.

## Build from a checkout

From the repository root, using the Go version in `go.mod`:

```sh
go build -o .tmpbuild/fdb ./cmd/fdb &&
export PATH="$PWD/.tmpbuild:$PATH" &&
fdb version
```

The renamed CLI has not had a tagged release yet. The installer and tagged Go
installation require a published root-module release containing `cmd/fdb`
(and matching `fdb` archives for the installer); use the checkout build above
until then. See the [installation guide](https://fdb.dev/docs/cli/#install).
The Bazel entry point is `just fdb <args>` (`bazelisk run //cmd/fdb -- <args>`).

## Start a local cluster

With Docker running:

```sh
fdb dev up &&
fdb tx read-version
```

`fdb dev up` initializes FoundationDB and activates a context. The default config
is `~/.fdb/config.yaml` (override with `FDB_CONFIG`); `fdb config path` prints its
location. Continue with the [protobuf](https://fdb.dev/docs/cli/protobuf/) or
[SQL](https://fdb.dev/docs/cli/sql/) tutorial.

Remove the development container and its data when finished:

```sh
fdb dev down
```

## Development

Run `just test` from the repository root for the standard CLI unit and bounded
integration coverage. Docker is required for integration tests. See `AGENTS.md`
for validation lanes. The SQL source assets remain in [demo/](demo/).
