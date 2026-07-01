# frl

Operator and developer CLI for the Go FoundationDB Record Layer. Separate
Go module so library consumers of `fdb.dev` don't inherit CLI deps.

See **[docs/operator-guide.md](docs/operator-guide.md)** for the full
wiring guide (Go + Java apps, both metadata paths). This README is a
terse command-surface reference.

Want to try it end-to-end against a live cluster in 5 steps? See
**[demo/README.md](demo/README.md)** — Docker FDB + schema bootstrap +
1 000-row seed + sample queries, copy-paste.

## Install

```sh
go install fdb.dev/cmd/frl@latest
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

### Data

```
frl record get <pk> [--type T]               # single record by PK (composite: 1,1)
frl record scan [--type T] [--reverse] [--limit N]  # newline-delimited JSON envelopes
frl record count [--type T] [-o json]        # via atomic count index
frl record put --type T '<json>' [--dry-run] --yes   # write (guarded)
frl record delete <pk> [--dry-run] --yes             # write (guarded)

frl index ls [--no-fdb] [-o json]            # name, type, state, record types
frl index describe <name> [-o json]          # full definition from metadata
frl index scan <name> [--reverse] [--limit N] # index entries as JSON envelopes
frl index build <name> --yes                 # online build, resumable (write)
frl index rebuild <name> --yes               # clear + build from scratch (write)
frl index set-state <name> <state> --yes     # READABLE/WRITE_ONLY/DISABLED (write)
```

### Store

```
frl store info [-o json]                     # DataStoreInfo header, no metadata needed
frl store dump [--subspace L] [--limit N]    # tuple-decoded forensic view; filter by subspace label
frl store lock <state> [--reason R] --yes    # header lock (write)
frl store unlock --yes                       # clear the lock (write)
frl store truncate --yes                     # delete EVERY record (double-gated)
```

### Metadata

```
frl meta get                                 # RecordMetaData as JSON
frl meta types ls [-o json]                  # record types + PK fields
frl meta types describe <name>               # PK, type key, proto msg, indexes

frl meta validate --file <f> [-o json]       # standalone .pb validation
frl meta evolve-check --old <f> --new <f> [-o json]  # MetaDataEvolutionValidator (CI-friendly)
frl meta diff <old> <new> [-o json]          # diff (text: +/-/~, json: sections.added/removed/changed)
frl meta apply --file <f> --yes              # validate + persist into FDBMetaDataStore (write)
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

## Flags (shared surface)

```
--context <name>          # on all store-touching commands
--meta-file <path>        # overrides Context.metadata for this call
--database + --schema     # relational addressing: keyspace + metadata from
                          #   the catalog (schema-pinned template version)
--cluster-file <path>     # overrides Context.cluster_file; chains with
                          #   `frl sql --cluster-file $(frl fdb up) …`
--keyspace-tuple <json>   # typed keyspace, e.g. '["myapp", 42, {"uuid": "…"}]'
--no-fdb                  # index ls only — metadata-only render
--reverse                 # record scan, index scan — walk in reverse PK / key order
--subspace <label>        # store dump only — limit to one subspace
-o|--output text|json     # structured-output commands (see below)
```

Contexts support the same addressing modes: set exactly one of
`keyspace_path`, `keyspace_tuple`, or `database` + `schema`.

## Structured output (`-o json`)

Fifteen commands emit machine-readable JSON on demand:

| Command | Payload |
|---|---|
| `store info` | full `DataStoreInfo` proto |
| `index ls` | array of `{name, type, state, record_types, last_modified_version}` |
| `index describe` | `{name, type, expression_fields, column_size, subspace_key, record_types, unique, clear_when_zero, added_version, last_modified_version, has_predicate, options}` |
| `meta types ls` | array of `{name, primary_key, since_version}` |
| `meta types describe` | `{name, primary_key, record_type_key, proto_message, proto_field_count, indexes, multi_type_indexes, universal_indexes}` |
| `meta validate` | `{file, valid}` |
| `meta evolve-check` | `{old, new, valid}` |
| `meta diff` | `{version?, record_types.{added,removed,changed}, indexes.{…}}` — added: `{name, detail}`, removed: names, changed: `{name, changes: [{field, old, new}]}` |
| `config view` | selected `Context` as protojson (snake_case; `-o yaml` is the default) |
| `config get-contexts` | array of `{name, active}` |
| `config current-context` | `{name}` |
| `keyspace resolve` | `{path, prefix_hex, prefix_len}` |
| `record count` | `{count, record_type}` |
| `tx read-version` | `{read_version}` |
| `version` | `{version, go_version, goos, goarch}` |

`record get` / `record scan` / `index scan` always emit newline-delimited
JSON envelopes — `-o` doesn't apply there (no competing text form):

| Command | Envelope |
|---|---|
| `record get` / `record scan` | `{"primary_key": "…", "record_type": "…", "record": { … }}` |
| `index scan` | `{"index": "…", "index_values": "…", "primary_key": "…", "value": "…"}` |

Proto field names are rendered in snake_case (via `UseProtoNames`) so
operators can grep / jq on keys that match their `.proto` source.

`meta get` uses `-o json|yaml` (protojson vs protoyaml); both render the
full `MetaData` message, yaml is easier to scan for large schemas.

Shape contract: success → typed object / array, error → non-zero exit +
message on stderr (never `{"valid": false}` at exit 0). Scripts branch
on exit code.

## Shell completions

cobra generates completion scripts on demand:

```sh
# bash (system-wide):
frl completion bash | sudo tee /etc/bash_completion.d/frl

# bash (per-user, lazy-loaded):
frl completion bash > ~/.local/share/bash-completion/completions/frl

# zsh:
frl completion zsh > "${fpath[1]}/_frl"

# fish:
frl completion fish > ~/.config/fish/completions/frl.fish
```

Tab-complete covers:
- noun-verb tree (cobra default)
- `--context` → context names from `~/.frl/config.yaml`
- `--output` → `text`/`json` (or `json`/`yaml` for `meta get`)
- `--type` → record type names from loaded metadata
- `--subspace` (store dump) → known subspace labels
- positional args on `config use-context`, `index describe`/`scan`,
  `meta types describe`

## Testing

- `go test ./internal/...` — the full suite: unit tests plus end-to-end
  tests against an FDB testcontainer (every read command, `sql`, and
  `meta catalog`). Without Docker the e2e tests skip with the repo's one
  allowed skip ("FDB not available (no Docker)"); unit tests always run.
- `bazelisk test //cmd/frl/...` — the same suite under Bazel; this is
  what CI runs via `bazelisk test //...`, so the e2e net gates merges.

## What's not yet wired

`config add-context`, Path B metadata for `meta get`/`meta types`/`index
describe` (file sources only until RFC-174 Slice 5), `frl status`. See
`rfcs/174-frl-cli-v2.md` at the repo root for the full v2 design, slice
plan, and review record.
