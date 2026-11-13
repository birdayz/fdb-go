---
title: Command reference
description: "Commands, flags, configuration, output formats, and shell completions for the fdb CLI."
weight: 5
---

`fdb` manages local FoundationDB development databases, runs SQL, and inspects
and updates Record Layer stores. The CLI is pre-1.0; commands, flags, and
output formats may change.

For setup, see the [CLI overview](/docs/cli/). For schema and store wiring, see
[Metadata and store addressing](/docs/cli/metadata/),
[Protobuf schemas](/docs/cli/protobuf/), and [SQL](/docs/cli/sql/).

## Command groups

| Root command | Purpose |
|---|---|
| `fdb rl` | Record Layer operations: `record`, `index`, `store`, `meta`, `stats`, `keyspace`, and `status`. Relational catalog operations are under `fdb rl meta catalog`. |
| `fdb sql` | Interactive SQL, a statement with `-c`, a file with `-f`, or input from stdin. |
| `fdb tx` | Transaction utilities. |
| `fdb dev` | Local FoundationDB development databases in Docker. |
| `fdb config` | Local configuration and context selection. |
| `fdb version` | Binary and Go toolchain details. |
| `fdb completion` | Shell completion scripts. |

Use `fdb <command> --help` or `fdb help <command>` for command help; `-h` is the
short flag form. Flags are command-specific, not global: put them after the
command that accepts them.

## Configuration and addressing

The default config path is `~/.fdb/config.yaml`. Set `FDB_CONFIG` to use another
path. `fdb config path` prints the effective path, even before the file exists.

```sh
fdb config init
$EDITOR ~/.fdb/config.yaml
fdb config use-context local
fdb rl store info                 # inspect an existing store's header
```

A context for a protobuf descriptor set can look like this:

```yaml
current_context: local
contexts:
  - name: local
    cluster_file: /etc/foundationdb/fdb.cluster
    keyspace_path: /myapp/orders
    metadata:
      descriptor_set:
        path: /etc/myapp/schema.binpb
        records_file: myapp/records.proto
        # metadata_version: 7  # optional explicit override
```

Choose one addressing mode in each context:

- `keyspace_path`: a slash-separated path packed as a tuple of strings.
- `keyspace_tuple`: a typed tuple, written as a YAML array.
- `database` and `schema`: a relational database URI and schema name. The
  relational catalog supplies both the store keyspace and its schema-pinned
  template metadata.

For a non-relational store, choose one source under `metadata`:
`descriptor_set`, `meta_store_keyspace` for metadata persisted in an
`FDBMetaDataStore`, or `meta_file` for a complete serialized `MetaData.pb`.

### Shared addressing flags

Record and index commands, store commands, `rl meta get`, `rl meta types`, and
`rl status` share these addressing flags:

| Flag | Meaning |
|---|---|
| `--context <name>` | Select a context instead of `current_context`. |
| `--cluster-file <path>` | Override the context's `cluster_file`. An empty effective value uses the client's default cluster-file discovery. |
| `--database <uri>` and `--schema <name>` | Address one relational store. Supply both; they replace the context's keyspace and metadata with catalog addressing. |
| `--keyspace-tuple <json>` | Override the keyspace with a typed tuple, such as `'["myapp", 42, {"uuid": "00112233-4455-6677-8899-aabbccddeeff"}]'`. Cannot be combined with `--database`/`--schema`. |

`--context` and `--cluster-file` are also available on SQL, transaction,
catalog, statistics, and metadata-apply commands. Their other addressing flags
are listed in their own sections below. `dev up --context` has a different
meaning: it names the context to write and activate.

Typed tuples accept strings, signed integers, fractional numbers,
`{"uuid": "…"}`, and `{"bytes_hex": "…"}`. Each tagged object has exactly one
key. Boolean, null, and nested-array elements are not supported. Config tuple
numbers pass through floating-point representation; avoid config integers
outside the exact range of ±2^53.

Relational database paths, schema names, and template names follow SQL identifier
rules: unquoted names fold to uppercase; SQL double quotes preserve case.
`/FRL` is the upstream relational domain. A relational `/FRL/...` URI is not the
same addressing mode as a slash-separated Record Layer `keyspace_path`.

### SQL and statistics context defaults

SQL and `rl stats` can inherit `database` and `schema` from the selected
relational context:

- Without `--database`, the context supplies its database and, unless
  `--schema` is explicit, its schema.
- An explicit `--database` never inherits the context's schema, even when it
  names the same database. Supply `--schema` if needed.
- An explicit `--schema` without `--database` keeps the context's database and
  replaces only its schema.

SQL requires an effective database; its default schema is optional. Single-schema
statistics commands require both an effective database and schema.
`stats collect --all-schemas` can inherit the context's database but ignores its
implicit schema. It refuses any explicit `--schema`, including `--schema ''`.

### Metadata-source flags

These flags are accepted only by metadata-aware commands: record and index
operations, `rl store create`/`lock`/`unlock`/`truncate`, `rl meta get`,
`rl meta types ls`/`describe`, and `rl status`.

| Flag | Meaning |
|---|---|
| `--descriptor-set <path>` | Load a compiled protobuf `FileDescriptorSet`, including imports, and derive metadata from Record Layer annotations. |
| `--records-file <path>` | The records schema's import path inside `--descriptor-set`. Required when more than one file declares a Record Layer union. |
| `--meta-version <N>` | Optional non-negative int32 override of the descriptor input's top-level metadata version. |
| `--meta-file <path>` | Load a complete serialized `MetaData.pb` instead of deriving metadata from annotations. Overrides the context's metadata source. |

`--records-file` and `--meta-version` require `--descriptor-set` on the same
invocation. `--descriptor-set` and `--meta-file` are mutually exclusive. Neither
can be combined with `--database`/`--schema` or a relational context, because the
catalog is already the metadata source.

Compile the descriptor set in your protobuf module, then inspect it:

```sh
buf build . --as-file-descriptor-set -o schema.binpb
fdb rl meta get --descriptor-set schema.binpb --records-file myapp/records.proto
```

The CLI reads the compiled set; it does not invoke Buf or `protoc`. It builds
metadata from primary-key, index, and schema options without an application
plugin or metadata dumper. Omit `--records-file` only when exactly one file
declares a Record Layer union. Missing record definitions or primary keys fail
normal Record Layer validation. See [Protobuf schemas](/docs/cli/protobuf/) for
`protoc` compilation and annotation details.

Omit `--meta-version` or the context's `metadata_version` to use the builder's
annotation-derived version. An override changes only the top-level metadata
version; index versions remain derived. It cannot recover application evolution
history or programmatic indexes: descriptor input reads declarations, not
application code or the target store's metadata.

Descriptor-input flags are **not** accepted by SQL, statistics, catalog commands,
metadata-free `rl store info`/`dump`, or the artifact commands
`rl meta validate`, `evolve-check`, `diff`, and `apply`. Those artifact commands
still require complete serialized `MetaData.pb` files, not descriptor sets.
Use `fdb rl meta get --descriptor-set schema.binpb` to validate and display
annotated descriptor input.

## Write confirmation

Record Layer mutations normally require either `--yes` or an interactive
confirmation. Non-interactive stdin without `--yes` is refused. This applies to
record puts/deletes, index builds/rebuilds/state changes, store
creation/locks/unlocks, metadata apply, catalog repair, and statistics clear.

- Record `put` and `delete` support `--dry-run` without confirmation.
- Store `truncate` always requires `--yes`; an interactive terminal additionally
  requires you to type the store address back.
- Statistics collection persists statistics without a confirmation prompt.
- SQL writes and DDL execute without these Record Layer confirmation guards.

Record Layer writes refuse a target that overlaps the relational catalog.
Relational template evolution uses the versioned Go metadata/catalog APIs,
then catalog repair and index builds—not `rl meta apply` or unsupported
`ALTER SCHEMA TEMPLATE` SQL. See [schema evolution](/docs/cli/metadata/#schema-evolution).
Record Layer record writes also bypass SQL-level constraints not encoded in
Record Layer metadata; use SQL when those constraints must be enforced.

## Record Layer commands

### Record commands

```text
fdb rl record get <primary-key> [--type T]
fdb rl record scan [--type T] [--reverse] [--limit N]
fdb rl record count [--type T] [-o text|json]
fdb rl record put --type T '<json>' [--dry-run] [--yes]
fdb rl record delete <primary-key> [--type T] [--dry-run] [--yes]
```

All record commands accept the shared addressing and metadata-source flags.

| Command | Behavior and output |
|---|---|
| `get` | Load one record by primary key. A missing record is an error. Emit one JSON envelope. |
| `scan` | Scan in primary-key order, optionally restricted by record type. `--reverse` reverses that order. `--limit` defaults to `100`; `0` means unlimited. Emit newline-delimited JSON envelopes. |
| `count` | Read maintained counts, not a full record scan. Text output is a bare integer. |
| `put` | Parse the JSON as the required `--type`, validate it, and save it with transactional index maintenance and uniqueness checks. Overwrite a record with the same primary key. Both real writes and dry runs emit the saved or would-be-saved JSON envelope. |
| `delete` | Delete a record and its index entries transactionally. `--dry-run` reports existence without deleting. Deleting an already-absent record succeeds and prints `already absent`. |

Primary keys use comma-separated tuple elements. Each element parses as a signed
64-bit integer if possible, otherwise as a string. Use the `primary_key` printed
by a scan rather than guessing a record-type prefix. With `get` or `delete`,
`--type T` prepends that type's key when its primary-key expression has a
record-type prefix; then supply only the logical key, not an already-prefixed
one. This syntax cannot address string elements containing commas, and does not
round-trip binary, UUID, nested, or other typed primary-key elements. Integers
above the signed int64 range are parsed as strings.

Store-wide `count` uses a `record_count_key` if configured, otherwise a universal
`COUNT` index. Per-type counts use a record-type count key, a `COUNT` index on
that type, or a universal `COUNT` index grouped by record type. If no suitable
count source exists, the command errors rather than scanning.

### Indexes

```text
fdb rl index ls [--no-fdb] [-o text|json]
fdb rl index describe <name> [-o text|json]
fdb rl index scan <name> [--reverse] [--limit N]
fdb rl index build <name> [--limit N] [--rps N] [--max-retries N] [--time-limit D] [--yes]
fdb rl index rebuild <name> [--limit N] [--rps N] [--max-retries N] [--yes]
fdb rl index set-state <name> <state> [--yes]
```

All index commands accept shared addressing and metadata-source flags.

| Command | Behavior |
|---|---|
| `ls` | Show name, index type, state, record types, and last-modified metadata version. `--no-fdb` reads file metadata only and renders state as `—`; use a descriptor set or complete metadata file, not catalog or metadata-store input. |
| `describe` | Show the full index definition from metadata, including fields, options, versions, and record types. |
| `scan` | Emit index entries as newline-delimited JSON envelopes. `--limit` defaults to `100`; `0` means unlimited. `--reverse` reverses key order. Type-specific scan modes for `TEXT`, `BITMAP_VALUE`, `VECTOR`, and `TIME_WINDOW_LEADERBOARD` indexes are not exposed. |
| `build` | Run an online, batched, resumable build and mark the index readable when complete. An already-readable index is left alone. A unique-pending index can be published without a build after its uniqueness violations are resolved. |
| `rebuild` | Clear index entries and build progress, mark the index write-only, and build from scratch. |
| `set-state` | Change the index state. `readable` refuses an incompletely built index; use `build` to finish it. `disabled` stops maintenance; `write-only` resumes maintenance without exposing the index to queries. |

`set-state` accepts lowercase `readable`, `readable-unique-pending`,
`write-only`, or `disabled`. Lists display the canonical uppercase states
`READABLE`, `READABLE_UNIQUE_PENDING`, `WRITE_ONLY`, and `DISABLED`.

Build tuning:

| Flag | Commands | Default and meaning |
|---|---|---|
| `--limit <N>` | `build`, `rebuild` | `0`: indexer default. Records per transaction, not a total scan limit. |
| `--rps <N>` | `build`, `rebuild` | `0`: indexer default. Records-per-second throttle. |
| `--max-retries <N>` | `build`, `rebuild` | `100`: retry budget for adaptive batch-halving on transient errors. |
| `--time-limit <duration>` | `build` | `0`: no requested time limit. A duration such as `30s` bounds this pass; rerun to resume a partial build. |
| `--all-schemas` | `build` | Build across every schema in the required `--database`; omit `--schema`. |
| `--concurrency <N>` | `build --all-schemas` | `0`: fleet default of `4` schemas in parallel. |

A build commits progress in ranges and can be interrupted and resumed. If a
partial build cannot be continued with the requested settings, use the builder
and settings that began it, or use `rebuild` to start over. Building against
newer metadata can upgrade the store; ordinary record reads and writes do not
perform that migration.

For a database-wide build, the index name is optional. Omit it to build each
schema's pending indexes, or supply it to restrict the pass to one index:

```sh
fdb rl index build --database /FRL/myapp --all-schemas --yes
fdb rl index build 'Order$price' --database /FRL/myapp --all-schemas --yes
```

Database-wide passes isolate failures per schema rather than making one atomic
change across the database. An absent named index in a schema is not an error.
Progress goes to stderr; stdout carries a summary. A non-zero exit reports
failures, and rerunning resumes unfinished work.

### Stores

```text
fdb rl store create [--yes]
fdb rl store info [-o text|json]
fdb rl store dump [--subspace <label>] [--limit N]
fdb rl store lock <forbid-record-update|full-store> [--reason <text>] [--yes]
fdb rl store unlock [--yes]
fdb rl store truncate --yes
```

All store commands accept shared addressing flags. Only `create`, `lock`,
`unlock`, and `truncate` accept metadata-source flags; `info` and `dump` do not.

| Command | Behavior |
|---|---|
| `create` | Create an empty native store atomically, initializing its header and index states. Refuse an existing store or nonempty prefix. Print `Created store at <address> (metadata version <N>).` on success. |
| `info` | Read the `DataStoreInfo` header directly, without loading metadata. Show format, metadata and user versions, record-count state, lock state and reason, user fields, and the resolved keyspace prefix. JSON output is the full header proto. |
| `dump` | Read-only forensic view: one line per key, with a subspace label, tuple-decoded suffix, and value size. No metadata required. `--limit` defaults to `100`; `0` means unlimited. `--subspace` restricts the range read to one label. |
| `lock` | Set a header lock. `forbid-record-update` rejects record writes but permits store maintenance; `full-store` locks the whole store. `--reason` records text shown by `info`. |
| `unlock` | Clear the header lock, including a full-store lock. |
| `truncate` | Delete **every record**, index entry, and version stamp; retain the header. Always requires `--yes`, plus a typed address confirmation on a terminal. A full-store lock must be cleared first. |

`create` uses the normal `--yes` or interactive confirmation guard and refuses
targets overlapping the relational catalog. It accepts native `keyspace_path`
or `keyspace_tuple` addressing only: `--database`/`--schema` or a relational
context is refused. Create SQL-managed stores through [SQL DDL](/docs/cli/sql/).

The empty-prefix check and header/index-state initialization run in one
transaction. Creation never replaces records or changes an existing store's
metadata. Record reads and writes do not implicitly create a missing store.

Metadata definitions remain in the selected descriptor set, complete metadata
file, or `FDBMetaDataStore`. Creation does not implicitly persist the full
metadata definitions inside the record-store subspace; keep the selected source
available for subsequent commands.

`dump --subspace` accepts these labels:

```text
index                 index-build          index-range
index-secondary       index-state          record
record-count          record-version       sliding-window
store-info            uniq-violations
```

### Metadata inspection

```text
fdb rl meta get [-o json|yaml]
fdb rl meta types ls [-o text|json]
fdb rl meta types describe <name> [-o text|json]
```

These commands accept shared addressing and metadata-source flags. They can
inspect local descriptor sets or complete metadata files without opening a
record store. Metadata-store and catalog sources require FoundationDB.

- `get` renders the full `MetaData` message: JSON by default, YAML for a more
  compact view of large schemas.
- `types ls` lists record types, primary-key fields, and since-version values.
- `types describe` shows the primary key, record-type key, protobuf message and
  field count, and single-type, multi-type, and universal indexes.

### Metadata artifacts and evolution

```text
fdb rl meta validate --file <metadata.pb> [-o text|json]
fdb rl meta evolve-check --old <old.pb> --new <new.pb> [-o text|json] [--allow-*]
fdb rl meta diff <old.pb> <new.pb> [-o text|json]
fdb rl meta apply --file <new.pb> [--meta-store-keyspace <path>] [--force-initial] [--allow-*] [--yes]
```

All four commands read **complete serialized `MetaData.pb` artifacts**. A
`FileDescriptorSet` is not a substitute. `validate`, `evolve-check`, and `diff`
are offline and do not accept context or store-addressing flags.

| Command | Behavior |
|---|---|
| `validate` | Parse the file and run normal Record Layer structural validation. Intended for artifact checks in CI. |
| `evolve-check` | Run `MetaDataEvolutionValidator` from old to new metadata. By default, the version must advance, index rebuilds are refused, and the split-long-records setting cannot change. |
| `diff` | Report added, removed, and changed record types and indexes, plus a changed top-level version. Text uses `+`, `-`, and `~`; JSON has separate added/removed/changed lists. Differences do not themselves make the command fail; pair with `evolve-check` to validate a transition. |
| `apply` | Validate and persist new metadata to an `FDBMetaDataStore`. It is not a relational template update. |

Evolution options are explicit flags, not a literal `--allow-*` flag:

| Flag | `evolve-check` | `apply` |
|---|---|---|
| `--allow-no-version-change` | Accept equal versions, but never a lower version. | Not accepted. |
| `--allow-index-rebuilds` | Permit changes that require a full index rebuild. | Same. |
| `--allow-unsplit-to-split` | Permit changing `split_long_records` from false to true. | Same. |

`apply` accepts `--context` and `--cluster-file`. Its target is
`--meta-store-keyspace` or the context's `metadata.meta_store_keyspace`, separate
from the record store's keyspace. A file-only metadata context has no persisted
metadata store to update unless you supply that target. The first write into an
empty metadata store additionally requires `--force-initial`.

The new version must be greater than the stored version. An artifact identical
to the current metadata succeeds without a write. Evolution validation runs
again in the save transaction, and a concurrent change since confirmation is
refused. See [Metadata and store addressing](/docs/cli/metadata/) for persisted
metadata setup.

### Relational catalog

```text
fdb rl meta catalog databases [-o text|json]
fdb rl meta catalog schemas [--database <uri>] [-o text|json]
fdb rl meta catalog templates [-o text|json]
fdb rl meta catalog get <template> [--version N] [-o json|yaml]
fdb rl meta catalog repair --database <uri> --all-schemas [--concurrency N] [--yes]
```

Every catalog command accepts `--context` and `--cluster-file`. They use the
relational catalog, not a context's Record Layer metadata source. Without a
relational catalog, `databases`, `schemas`, and `templates` succeed with empty
listings; `get` fails because the requested template does not exist.

| Command | Behavior |
|---|---|
| `databases` | List database URIs, one per line in text mode. |
| `schemas` | List database, schema, template name, and pinned template version. `--database` narrows the list to one database. |
| `templates` | List every template name/version pair, including multiple stored versions. |
| `get` | Render a template's full metadata. `--version` defaults to `0` for latest; a positive value selects that version. JSON is the default; YAML is also available. This is distinct from `--meta-version` on descriptor input. |
| `repair` | Rebind every schema in one database to the latest stored version of the template that schema already uses. Both `--database` and `--all-schemas` are required. |

Saving a new template does not move existing schemas off their pinned versions.
`repair` performs that rebind; there is no `--to-version` option. A database with
several templates is rebound to each template's own latest version, not one
common version. `--concurrency` defaults to `0`, selecting the fleet default of
`4`.

Repair is resumable, not atomic across schemas: it skips schemas already at the
target version, reports failures per schema, and continues the pass. It never
targets the catalog's own schema. Progress goes to stderr and the final tally
goes to stdout, including on a partially failed pass; any failures cause a
non-zero exit.

### Planner statistics

```text
fdb rl stats collect [--database <uri>] [--schema <name>] [--batch-size N] [--max-records-per-type N] [-o text|json]
fdb rl stats collect --all-schemas [--database <uri>] [--concurrency N] [--batch-size N] [--max-records-per-type N]
fdb rl stats show [--database <uri>] [--schema <name>] [-o text|json]
fdb rl stats clear [--database <uri>] [--schema <name>] [--yes]
```

Every statistics command accepts `--context` and `--cluster-file`. Its effective
relational address comes from flags or a relational context, following the
[SQL and statistics context defaults](#sql-and-statistics-context-defaults).
Single-schema commands need a database and schema; `collect --all-schemas` needs
only a database and refuses explicit `--schema`, even an empty value. Descriptor,
metadata-file, and typed-keyspace flags do not apply.

- `collect` reads every record, counts rows per record type, and replaces stored
  statistics. It writes without a confirmation prompt. Counts are exact for a
  store at rest; primary-key changes during a multi-transaction scan can cause
  a missed or repeated count.
- `show` reports stored counts and whether the planner considers them usable.
  A negative verdict can identify absent, expired, future-version, incomplete,
  torn, unreadable, or unsupported statistics. The verdict does not depend on
  whether a connection has opted in to using them.
- `clear` removes only statistics, not records or indexes, and requires the
  normal write confirmation. The planner falls back to its default estimates.

Collection options:

| Flag | Default and meaning |
|---|---|
| `--batch-size <N>` | `0`: library default of `1000` records per transaction. Batches also stop at a `3s` time limit or `16 MiB` read limit. |
| `--max-records-per-type <N>` | `0`: no cap. Exceeding a positive cap aborts the collection and stores nothing; previous statistics remain in place and are not refreshed. The aborted command exits non-zero. |
| `--all-schemas` | Collect for every schema in one `--database`, with per-schema failure isolation. |
| `--concurrency <N>` | `0`: fleet default of `4`; applies to `--all-schemas`. |
| `-o`, `--output text\|json` | Default `text` for a single-schema report. The `--all-schemas` form emits a text tally and rejects JSON output. |

Statistics live outside record-store subspaces. They expire after roughly 24
hours, do not invalidate existing cached plans, and are used only by connections
that opt in with `planner_statistics=true`, for example:
`fdbsql:///FRL/MYAPP?schema=MAIN&planner_statistics=true`. See
[Metadata and store addressing](/docs/cli/metadata/#planner-statistics) for
planner-statistics usage.

### Status and keyspace navigation

```text
fdb rl status [-o text|json]
fdb rl keyspace resolve <path> [-o text|json]
```

`status` accepts shared addressing and metadata-source flags. It checks cluster
reachability with a global read version, the store header, metadata loading,
and the relational catalog independently. A failed cluster check exits non-zero.
Other missing or failed components are reported in the output without making
the command fail; inspect those fields rather than treating exit zero as proof
that every component is configured.

`keyspace resolve` is offline and accepts no context or metadata flags. It packs
a slash-separated path as a tuple of strings and prints the byte prefix as
lowercase hex. This does not resolve typed or relational keyspace layouts.

## SQL

```sh
fdb sql --database /FRL/myapp --schema main
fdb sql --database /FRL/myapp --schema main -c 'SELECT count(*) FROM orders'
fdb sql --database /FRL/myapp -f migrations/001.sql
```

| Flag | Meaning |
|---|---|
| `--context <name>` | Context supplying the cluster file and relational address defaults. |
| `--cluster-file <path>` | Override the context's cluster file. |
| `--database <uri>` | Database URI, such as `/FRL/myapp`; defaults to the selected relational context's database. An effective database is required. |
| `--schema <name>` | Default schema for unqualified references. Can inherit from a relational context only when `--database` is not explicit. |
| `-c`, `--command <sql>` | Run one statement and exit. |
| `-f`, `--file <path>` | Run statements from a SQL file and exit; stop on the first failure. |
| `-o`, `--output table\|csv\|json\|ndjson` | Result format; default `table`. Machine formats send row-count and duration footers to stderr. |

Address defaults follow the
[SQL and statistics context rules](#sql-and-statistics-context-defaults).
Explicit `--database` never inherits a context schema; `--schema` alone can
replace the context schema while keeping its database.

Input selection is `-c` first, then `-f`, then stdin. Without `-c` or `-f`, a
terminal starts the interactive REPL; nonterminal stdin runs a SQL script with
no banner or prompts. File and stdin scripts stop on the first failure, with
errors identifying the statement number and input, such as
`statement 2 in stdin`. They accept SQL statements, not backslash commands.
Put completed statements on separate lines; multiline statements are supported.
A final statement without a semicolon is accepted. Do not put multiple `SELECT`
statements on one line.

SQL writes and DDL have **no Record Layer `--yes` or `--dry-run` guard**. Run only
statements you intend to execute. SQL does not accept descriptor-source flags;
its catalog and SQL DDL define the schema.

### Interactive commands

Multiline SQL ends at `;`. Backslash commands run immediately when no SQL is
being accumulated. `BEGIN`, `COMMIT`, and `ROLLBACK` use the same session.

| Input | Action |
|---|---|
| `\?` | Show REPL help. |
| `\q` | Quit, as with Ctrl-D. |
| `\d [table]` | List tables, or describe a table's columns, types, nullability, and primary key. |
| `\dt` | List schema templates. |
| `\c <schema>` | Switch schema; commit or roll back an open transaction first. |
| `\i <file>` | Execute statements from a file. |
| `\e` | Open `$EDITOR` and run the edited buffer. |
| `\x` | Toggle expanded, record-per-block table display. |
| `\timing` | Toggle row-count and duration footers; initially on. |
| `\explain` | Run the previous query with `EXPLAIN`. |

Line editing supports arrow keys and Ctrl-R history search. Interactive history
is stored in `~/.fdb/sql_history`, with up to `1000` entries; changing
`FDB_CONFIG` does not move it. Ctrl-C clears a partially accumulated statement;
on an empty input buffer it exits.

### SQL output

- `table`: aligned human-readable rows; terminal output may use colors and box
  drawing, while redirected output uses plain ASCII.
- `csv`: CSV with a header row. SQL null is an empty field; bytes are hex.
- `json`: one indented array of objects keyed by column name per result set.
- `ndjson`: one object keyed by column name per row.

JSON formats use JSON null for SQL null, hex strings for bytes, and RFC3339Nano
strings for timestamps. A multi-statement file can produce several result sets;
its JSON output is not one combined array. Machine formats keep result rows on
stdout and timing or rows-affected messages on stderr. The interactive REPL's
banner and backslash-command messages are still human-oriented.

## Transaction utilities

```text
fdb tx read-version [--context <name>] [--cluster-file <path>] [-o text|json]
```

`read-version` fetches the current global read version in a read-only transaction.
It needs no record metadata or keyspace and doubles as a cluster connectivity
check. Text output is a bare integer with a newline, suitable for command
substitution; JSON output is `{read_version}`.

## Local development database

```text
fdb dev up [--name <name>] [--image <image>] [--context <name>] [--keyspace <path>] [--port N] [-o text|json]
fdb dev down [--name <name>]
fdb dev status [--name <name>]
```

`dev up` starts a throwaway, single-node FoundationDB container, initializes it
with `configure new single memory`, waits for availability, copies the cluster
file beside the effective config file, and writes and activates a context.

| `dev up` flag | Default | Meaning |
|---|---|---|
| `--name` | `fdb-dev` | Docker container name; the cluster-file basename is `<name>.cluster`. |
| `--image` | `foundationdb/foundationdb:7.3.77` | FoundationDB Docker image. |
| `--context` | `fdb-dev` | Context to write and activate. |
| `--keyspace` | `/dev` | `keyspace_path` for that context. |
| `--port` | `4500` | Server port, published on `127.0.0.1`; the same port is used inside the container. Must be in `1`–`65535`. |
| `-o`, `--output` | `text` | Bare cluster-file path, or `json` with `{cluster_file, container, context}`. |

Progress goes to stderr, leaving stdout usable as a cluster-file argument:

```sh
cluster_file="$(fdb dev up)" &&
fdb sql --cluster-file "$cluster_file" --database /FRL/myapp
```

`dev status` reports the container's minimal cluster status. `dev down` removes
the container and its volumes but leaves the fdb context and copied cluster file
in place. Both accept `--name`, defaulting to `fdb-dev`. Use distinct `--name`,
`--context`, and `--port` values for multiple instances.

The container uses Docker bridge networking, publishes only on `127.0.0.1`, and
advertises that same address and port to host clients. This avoids Docker
Desktop's opt-in host networking and container IPs that a macOS host cannot
route to. A local Docker socket and the Docker CLI are required. Remote Docker
endpoints and clients in other containers are not supported by this helper.

The database has no authentication. Loopback publication is not an isolation
boundary against local users or other containers on the Docker bridge. Use a
current Docker release; do not use this setup for production or sensitive data.

## Config commands

```text
fdb config init [--force]
fdb config path
fdb config use-context <name>
fdb config current-context [-o text|json]
fdb config get-contexts [-o text|json]
fdb config view [--context <name>] [-o yaml|json]
```

| Command | Behavior |
|---|---|
| `init` | Write a starter config at `~/.fdb/config.yaml` or `FDB_CONFIG`, with a commented example and an empty context list. Refuse to overwrite an existing file unless `--force` is set. |
| `path` | Print the effective config path. |
| `use-context` | Select an existing context and preserve comments in the config file. |
| `current-context` | Print the active name; error if no current context is set. |
| `get-contexts` | List context names in config-file order, marking the active one with `*` in text mode. |
| `view` | Print the selected context, not the entire config. `--context` selects another context. YAML is the default; JSON is also available. |

Add contexts by editing the config file; `config add-context` is not implemented.

## Version

```text
fdb version [--short] [-o text|json]
fdb --version
```

The root `--version` flag also has the short form `-v`.
The default is `fdb <version> (<go_version> <goos>/<goarch>)`. `--short` prints
only the version string and takes precedence over the selected output format.
JSON contains `{version, go_version, goos, goarch}`. A source build may report
`dev`; unavailable build fields report `unknown`.

## Structured output

`-o` is the short form of `--output`. Commands offering a text/JSON choice
default to text. Full metadata uses JSON/YAML, config view defaults to YAML, and
SQL has its own formats described above.

### JSON payloads

The table describes the current fields; optional or unset fields may be omitted.

| Command | JSON payload |
|---|---|
| `rl store info` | Full `DataStoreInfo` proto. |
| `rl index ls` | Array of `{name, type, state, record_types, last_modified_version}`. |
| `rl index describe` | `{name, type, expression_fields, column_size, subspace_key, record_types, unique, clear_when_zero, added_version, last_modified_version, has_predicate, options}`. |
| `rl meta get` | Full `MetaData` proto; also supports YAML. |
| `rl meta types ls` | Array of `{name, primary_key, since_version}`. |
| `rl meta types describe` | `{name, primary_key, since_version, record_type_key, proto_message, proto_field_count, indexes, multi_type_indexes, universal_indexes}`. Index summaries contain `{name, type, fields}`. |
| `rl meta validate` | `{file, valid}` with `valid: true` on success. |
| `rl meta evolve-check` | `{old, new, valid}` with `valid: true` on success. |
| `rl meta diff` | `{version?, record_types: {added, removed, changed}, indexes: {added, removed, changed}}`. `version` is `{old, new}`; additions are `{name, detail}`, removals are names, and changes are `{name, changes: [{field, old, new}]}`. |
| `rl meta catalog databases` | Array of `{id}`. |
| `rl meta catalog schemas` | Array of `{database, name, template, template_version}`. |
| `rl meta catalog templates` | Array of `{name, version}`. |
| `rl meta catalog get` | Full template `MetaData` proto; also supports YAML. |
| `rl stats collect` | Single-schema `{schema, records_scanned, duration_ms, collected, skipped}`. `collected` maps type names to counts; `skipped` maps names to reasons. |
| `rl stats show` | `{schema, usable, refusal?, found, per_type?, collected_at_version?, collected_at_unix_nanos?, current_version?, age_versions?, max_age_versions, synthetic_types?, ambiguous_types?, read_refusal?, read_error?, missing_types?, extra_types?}`. `found` is `true`, `false`, or `null` when existence was not established. |
| `rl status` | `{cluster, read_version, store, metadata, catalog}`. Component results are descriptive strings. |
| `config view` | Selected `Context` as protojson with snake_case keys. |
| `config get-contexts` | Array of `{name, active}`. |
| `config current-context` | `{name}`. |
| `rl keyspace resolve` | `{path, prefix_hex, prefix_len}`. |
| `rl record count` | `{count, record_type}`; `record_type` is empty for a store-wide count. |
| `tx read-version` | `{read_version}`. |
| `dev up` | `{cluster_file, container, context}`. |
| `version` | `{version, go_version, goos, goarch}`. |

### JSON envelopes

Record `get`, `scan`, and `put`, plus index `scan`, always emit newline-delimited
JSON envelopes. They do not accept `-o` because they have no competing text form.

| Command | Envelope |
|---|---|
| `rl record get`, `rl record scan`, `rl record put` | `{"primary_key": "…", "record_type": "…", "record": {…}}` |
| `rl index scan` | `{"index": "…", "index_values": "…", "primary_key": "…", "value": "…"}` |

Record payload fields use their `.proto` spelling, including snake_case, through
`UseProtoNames`. Config view does the same. Full `DataStoreInfo` and `MetaData`
JSON instead use protobuf JSON field names, such as `formatVersion`, rather than
forcing snake_case. Do not assume every proto-rendering command uses the same
field-name convention.

Primary keys, index values, and index value tuples are rendered as readable
comma-separated strings, not typed JSON arrays. Byte elements render as hex.
Use `jq -s .` to collect a newline-delimited stream into an array.

### Exit status and progress

For non-interactive commands, errors exit non-zero and report a message on
stderr. Validation failures never return `{"valid": false}` at exit zero; branch
on the exit status. A stream or report can already have been emitted before an
error, so check the exit status even when stdout is nonempty.

Two diagnostic exceptions are intentional: `rl status` can exit zero while
reporting non-cluster component failures, and `rl stats show` can successfully
report `usable: false`. The SQL REPL reports statement errors and continues;
`-c`, `-f`, and nonterminal stdin return failures to scripts.

Online index builds and database-wide maintenance send progress to stderr.
Database-wide index builds, statistics collection, and catalog repair print a
text tally on stdout, including on a partially failed pass:

```text
targets=N migrated=N skipped=N built=N collected=N no-work=N failed=N refused=N
```

## Shell completions

```text
fdb completion bash [--no-descriptions]
fdb completion zsh [--no-descriptions]
fdb completion fish [--no-descriptions]
fdb completion powershell [--no-descriptions]
```

Cobra generates scripts on demand. `--no-descriptions` omits completion
descriptions. Bash requires the `bash-completion` package. Create the per-user
directories if needed before redirecting output.

```sh
# bash, system-wide:
fdb completion bash | sudo tee /etc/bash_completion.d/fdb

# bash, per-user and lazy-loaded:
mkdir -p ~/.local/share/bash-completion/completions
fdb completion bash > ~/.local/share/bash-completion/completions/fdb

# zsh, with compinit enabled:
fdb completion zsh > "${fpath[1]}/_fdb"

# fish:
mkdir -p ~/.config/fish/completions
fdb completion fish > ~/.config/fish/completions/fdb.fish
```

For PowerShell, load the script in the current session:

```powershell
fdb completion powershell | Out-String | Invoke-Expression
```

Completion covers:

- The command tree and flag names.
- `--context`: context names from the effective config file, normally
  `~/.fdb/config.yaml`.
- `--output`: `text`/`json` for commands with those formats; `json`/`yaml` for
  metadata renders and config view; `table`/`csv`/`json`/`ndjson` for SQL.
- `--type`: record type names from locally loaded metadata.
- `--subspace` on store dump: known subspace labels.
- Positional context names on `config use-context`, index names on index
  `describe`/`scan`/`build`/`rebuild`/`set-state`, and type names on
  `rl meta types describe`.

Metadata completion does not connect to FoundationDB. Descriptor sets and local
metadata files can provide candidates; catalog and metadata-store sources
cannot. Missing or invalid config or metadata silently produces no candidates.

## Contributor note

From the repository root, `just fdb version` runs the CLI through Bazel
(`bazelisk run //cmd/fdb -- version`); `go run ./cmd/fdb version` is also available
for source development. See the [CLI overview](/docs/cli/) for building a local
binary.

Run `just test` for the standard fast validation lane. It includes CLI unit
tests and bounded end-to-end coverage for Record Layer reads and guarded writes,
SQL, catalog discovery, and local development helpers. Docker is required for
integration coverage. Consult the repository's `AGENTS.md` for the full
validation lanes.
