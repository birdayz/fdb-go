# frl

Operator and developer CLI for the Go FoundationDB Record Layer. Separate
Go module so library consumers of `github.com/birdayz/fdb-record-layer-go`
don't inherit CLI deps.

See **[docs/operator-guide.md](docs/operator-guide.md)** for the full
wiring guide (Go + Java apps, both metadata paths). This README is a
terse command-surface reference.

## Install

```sh
go install github.com/birdayz/fdb-record-layer-go/cmd/frl@latest
```

Or build inside the repo:

```sh
just frl version                 # bazelisk run //cmd/frl -- version
go run ./cmd/frl version         # from the root of the repo
```

## First use

Scaffold a config file, edit it, select a context:

```sh
frl config init                    # writes ~/.frl/config.yaml with a template
$EDITOR ~/.frl/config.yaml         # fill in cluster_file, keyspace_path, metadata
frl config use-context local       # name the active context
frl store info                     # sanity check: cluster + keyspace reachable
```

The scaffold carries both metadata paths commented out:

```yaml
current_context: local
contexts:
  - name: local
    cluster_file: /etc/foundationdb/fdb.cluster
    keyspace_path: /myapp/orders
    metadata:
      meta_file: /etc/myapp/meta.pb         # Path A — meta.pb shipped alongside binaries
      # meta_store_keyspace: /myapp/_meta   # Path B — FDBMetaDataStore in FDB itself
```

See `docs/operator-guide.md` for how to produce `meta.pb` (Go: one-liner
via `recordlayer.WriteRecordMetaData`; Java: `meta.toProto().writeTo(out)`).

## Command surface

### Data (read-only)

```
frl record get <pk>                          # single record by PK
frl record scan [--type T] [--limit N]       # newline-delimited JSON envelopes
frl record count [--type T] [-o json]        # via atomic count index

frl index ls [--no-fdb] [-o json]            # name, type, state, record types
frl index describe <name>                    # full definition from metadata
frl index scan <name> [--reverse] [--limit N] # index entries as JSON envelopes
```

### Store

```
frl store info [-o json]                     # DataStoreInfo header, no metadata needed
frl store dump [--limit N]                   # tuple-decoded forensic view with subspace labels
```

### Metadata

```
frl meta get                                 # RecordMetaData as JSON
frl meta types ls [-o json]                  # record types + PK fields
frl meta types describe <name>               # PK, type key, proto msg, indexes

frl meta validate --file <f> [-o json]       # standalone .pb validation
frl meta evolve-check --old <f> --new <f> [-o json]  # MetaDataEvolutionValidator (CI-friendly)
frl meta diff <old> <new> [-o json]          # diff (text: +/-/~, json: sections.added/removed/changed)
```

### Context + navigation + escape

```
frl config init [--force]                    # scaffold a starter config.yaml
frl config path                              # print the effective config path
frl config use-context <name>
frl config current-context [-o json]
frl config get-contexts [-o json]
frl config view [--context <name>]
frl config schema                            # empty Config as JSON (field discovery)

frl keyspace resolve <path> [-o json]        # logical path → FDB byte prefix
frl tx read-version [-o json]                # current GRV (cluster smoke check)

frl version [--short] [-o json]              # binary + Go toolchain version
```

## Flags (current v1 surface)

```
--context <name>        # on all store-touching commands
--meta-file <path>      # overrides Context.metadata for this call
--no-fdb                # index ls only — metadata-only render
-o|--output text|json   # store info, index ls, meta types ls, config get-contexts
```

## Testing

- `go test ./internal/...` — unit tests (no FDB needed)
- `go test -tags=integration ./internal/cmd/...` — end-to-end against an
  FDB testcontainer, covers every read-only command
- `bazelisk test //cmd/frl/...` — Bazel-driven unit tests

## What's not yet wired

Writes (`record put`, `record delete`, `meta apply`, `store truncate` /
`destroy` / `lock`, `index build` / `rebuild` / `set-state`), `config
add-context`, `keyspace ls`/`tree` (FDB directory layer), `tx run`. See
the repo-root `TODO.md` section `## frl CLI` for the full design +
what's deferred and why.
