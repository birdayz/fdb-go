---
title: Connections and metadata
description: "Connect fdb to a cluster and Record Layer store using contexts, annotated protobuf descriptors, persisted metadata, or the SQL catalog."
weight: 3
---

`fdb` runs SQL, manages local FoundationDB development clusters, and
inspects and manages Record Layer stores. This guide covers the `fdb rl`
commands: store headers, records, indexes, metadata, and planner statistics.
Record decoding and schema-aware operations need your app's
`RecordMetaData` — the same schema your app already builds at startup.

If your schema expresses its metadata through Record Layer protobuf
annotations, give `fdb` a compiled descriptor set. It builds the metadata
directly from those declarations; no application plugin or metadata dumper
is required.

## Connect to a cluster

A Record Layer operation needs a cluster connection, a store keyspace, and
the metadata describing its records and indexes. These are separate choices.
Metadata-only inspection can work without a cluster.

For a local development database, `fdb dev up` initializes FoundationDB and
writes an active context automatically:

```sh
fdb dev up
fdb tx read-version
```

For an existing cluster, pass its cluster-file path after the subcommand:

```sh
fdb tx read-version --cluster-file /etc/foundationdb/fdb.cluster
```

A cluster file identifies the coordinators to contact; it is not a metadata
file or a store address. For commands that use a context, `--cluster-file`
overrides its `cluster_file` for that call.

## Contexts and store addresses

Configuration lives at `~/.fdb/config.yaml`; set `FDB_CONFIG` to use another
file. To create and inspect a config:

```sh
fdb config init
fdb config path
```

`config init` writes a commented example and refuses to overwrite an existing
file unless `--force` is supplied. Edit the file at the printed path, then
select and inspect a context:

```sh
fdb config use-context prod
fdb config get-contexts
fdb config current-context
fdb config view
```

A context combines a cluster file with one store-addressing mode:

| Mode | Context fields | Per-command override |
|---|---|---|
| Native Record Layer path | `keyspace_path` plus `metadata` | Use `--keyspace-tuple` for an explicit typed address |
| Native typed tuple | `keyspace_tuple` plus `metadata` | `--keyspace-tuple` |
| SQL catalog | `database` and `schema` | `--database` and `--schema` together |

Do not set more than one addressing mode. A typed tuple keeps key types
explicit: `["myapp", 42]` is different from `["myapp", "42"]`. For a native
store, use the same tuple as the application that owns it; guessing a prefix
can point at a different store. For SQL stores, let the catalog resolve the
address instead of reconstructing it by hand.

A command-line descriptor set or `--meta-file` replaces the context metadata
source for that call. `--records-file` and `--meta-version` require an explicit
`--descriptor-set`; set the corresponding fields under `metadata.descriptor_set`
when using a context source. SQL addressing cannot be combined with descriptor
input, `--meta-file`, or `--keyspace-tuple`.

The [protobuf tutorial](/docs/cli/protobuf/) creates a new native store. The
examples below explain metadata selection for an existing application store.

## Choose a metadata source

| Your schema source | CLI input |
|---|---|
| Protobuf primary-key, index, and schema annotations | **Path A:** `--descriptor-set` or `metadata.descriptor_set` |
| Metadata already persisted in `FDBMetaDataStore` | **Path B:** `metadata.meta_store_keyspace` |
| A relational schema created through SQL DDL | **Path C:** `--database` / `--schema`, using the SQL catalog |
| An existing complete `RecordMetaDataProto.MetaData` snapshot | **Advanced:** `--meta-file` or `metadata.meta_file` |

Metadata-only commands (`fdb rl meta get`, `fdb rl meta types ls`,
`fdb rl index describe`) stay offline when reading descriptor sets or
`meta.pb` files. Reading or updating records still needs a reachable store.

---

## Path A — protobuf annotations and a descriptor set

For a complete downloadable schema and a new store, start with the
[protobuf tutorial](/docs/cli/protobuf/). The examples here show how to use
your own records schema.

Use the standard Record Layer custom options in your records schema: a
record union, primary keys, indexes, and schema settings. For example:

```proto
// myapp/records.proto
syntax = "proto2";
package myapp;

import "record_metadata_options.proto";

option (com.apple.foundationdb.record.schema).store_record_versions = true;

message Order {
  optional int64 order_id = 1 [(com.apple.foundationdb.record.field).primary_key = true];
  optional string customer = 2 [(com.apple.foundationdb.record.field).index.type = "value"];
}

message RecordTypeUnion {
  option (com.apple.foundationdb.record.record).usage = UNION;
  optional Order order = 1;
}
```

### 1. Compile the schema, including imports

From your protobuf module, with its imports configured:

```sh
buf build . --as-file-descriptor-set -o schema.binpb
```

Or use `protoc`, adding the directory containing the Record Layer proto
definitions to its import path:

```sh
protoc -I . -I /path/to/record-layer-protos \
  --include_imports --descriptor_set_out=schema.binpb myapp/records.proto
```

The output must be a `google.protobuf.FileDescriptorSet` containing the
records schema and its imported definitions, including the custom options.
The CLI reads that artifact; it does not invoke Buf or `protoc`, parse
`.proto` source text, or run application code.

### 2. Use the descriptor set directly

```sh
# Metadata inspection needs no cluster connection.
fdb rl meta get --descriptor-set schema.binpb --records-file myapp/records.proto

# Use the same schema to decode records in an existing store.
fdb rl record scan --cluster-file ./fdb.cluster \
  --keyspace-tuple '["myapp", "orders"]' \
  --descriptor-set schema.binpb --records-file myapp/records.proto --limit 10
```

`--records-file` is the schema's import path **inside the descriptor set**,
not a path to source on disk. It can be omitted only when the set contains
exactly one schema file declaring `RecordTypeUnion` or a message marked
`(record).usage = UNION`. When multiple files declare unions, select one
explicitly. The normal Record Layer builder validates the union, record
definitions, primary keys, and index options; descriptor input does not
invent missing definitions or keys.

These flags are shared by metadata-aware store commands under `fdb rl`:
record/index operations, `rl meta get` / `rl meta types`, `rl status`, and
writes against native Record Layer stores. They are not SQL flags, and
metadata-free commands such as `rl store info` and `rl store dump` do not
accept them. The artifact commands `rl meta validate`, `rl meta evolve-check`,
`rl meta diff`, and `rl meta apply` still require complete serialized
`MetaData.pb` input. Use `fdb rl meta get --descriptor-set schema.binpb` to
validate and display compiled annotations.

`--descriptor-set` cannot be combined with `--meta-file` or
`--database`/`--schema`: those select different metadata sources.

### 3. Configure a reusable context

```yaml
# ~/.fdb/config.yaml
current_context: prod
contexts:
  - name: prod
    cluster_file: /etc/foundationdb/prod.cluster
    keyspace_path: /myapp/prod/orders
    metadata:
      descriptor_set:
        path: /etc/myapp/schema.binpb
        records_file: myapp/records.proto
        # metadata_version: 7  # optional explicit override
```

```sh
fdb config use-context prod
fdb rl record scan --limit 10
```

Leave `metadata_version` (or `--meta-version`) unset to use the builder's
version derived from annotations. An explicit non-negative override, such
as `--meta-version 7`, changes only the top-level metadata version. Generated
index last-modified versions remain derived from the declarations. The override
does **not** recover the target store's metadata or application evolution
history. Use the version and declarations that match the application managing
the store.

Descriptor input supports the standard protobuf custom primary-key, index,
and schema options. It does not discover indexes or other customizations
added only in application code. If your schema is annotation-defined, no
export helper is needed. Persisted metadata and complete snapshots remain
available when you need metadata that is not contained in the annotations.

---

## Path B — use `FDBMetaDataStore`

If your app already persists metadata via `FDBMetaDataStore`, `fdb`
needs nothing beyond the keyspace path where that store lives.

### Go

If you're not already doing this, add it once during app init:

```go
// NewFDBMetaDataStore takes a single subspace.Subspace — pass whatever
// subspace your app uses for the metadata store (often derived from a
// KeySpace path via path.ToSubspace() or built with subspace.Sub()).
metaStore := recordlayer.NewFDBMetaDataStore(metaSubspace)
metaProto, err := meta.ToProto()
if err != nil {
    return err
}
_, err = rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
    return nil, metaStore.SaveRecordMetaData(rtx.Transaction(), metaProto)
})
```

### Java

```java
FDBMetaDataStore store = new FDBMetaDataStore(context, metaDataPath);
store.saveRecordMetaData(meta).join();
```

### Configure `fdb`

```yaml
contexts:
  - name: prod
    cluster_file: /etc/foundationdb/prod.cluster
    keyspace_path: /myapp/prod/orders
    metadata:
      meta_store_keyspace: /myapp/prod/_meta
```

That's the whole wiring. `fdb` reads the current `MetaData` from FDB on
every command — zero external artifacts, no staleness.

---

## Path C — SQL catalog metadata

For a relational schema created through SQL DDL, use the catalog as the
metadata source. The CLI resolves the store keyspace and the schema-pinned
template version; no descriptor set or `meta.pb` is needed:

```sh
fdb rl record scan --database /FRL/myapp --schema main --limit 10
fdb rl meta catalog get orders_tpl
```

Supply the cluster file with `--cluster-file` or a context. A relational
context can set `database` and `schema` instead of a keyspace and metadata
source; SQL, statistics, and native Record Layer commands can use those
defaults. Native Record Layer commands inherit the complete database/schema
pair or require both override flags. For SQL and statistics, an explicit
`--database` does not inherit the context's schema; pass `--schema` for table
queries. A schema-only override retains the context's database only for SQL
and statistics. Do not combine SQL catalog addressing with `--descriptor-set`
or `--meta-file`.

---

## Advanced — a complete `meta.pb` snapshot

A serialized `RecordMetaDataProto.MetaData` is an optional alternative to
annotation input, not a prerequisite. Use an existing complete snapshot or
export one if you need metadata assembled in application code.

### 1. Add a 10-line dumper binary

#### Go

```go
// cmd/dump-meta/main.go
package main

import (
	"log"
	"os"

	"fdb.dev/pkg/recordlayer"

	"myapp/internal/schema" // whatever package builds your metadata today
)

func main() {
	meta, err := schema.BuildMetaData() // your existing function
	if err != nil {
		log.Fatal(err)
	}
	if err := recordlayer.WriteRecordMetaData(meta, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
```

Run it at build time:

```sh
go run ./cmd/dump-meta > meta.pb
```

#### Java

```java
// DumpMeta.java
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataProto;

public final class DumpMeta {
    public static void main(String[] args) throws Exception {
        RecordMetaData meta = Schema.buildMetaData(); // your existing builder
        RecordMetaDataProto.MetaData proto = meta.toProto();
        proto.writeTo(System.out);
    }
}
```

Run it at build time:

```sh
java DumpMeta > meta.pb
```

### 2. Ship `meta.pb` alongside your app

Either commit a reference copy to a known location
(`/etc/myapp/meta.pb`), or ship it as part of your container image, or
publish it with your release artifacts. The file is small (typically
<50 KB). Operators need read access to it.

### 3. Configure `fdb`

```yaml
# ~/.fdb/config.yaml
current_context: prod
contexts:
  - name: prod
    cluster_file: /etc/foundationdb/prod.cluster
    keyspace_path: /myapp/prod/orders
    metadata:
      meta_file: /etc/myapp/meta.pb
```

```sh
fdb config use-context prod
fdb rl store info
fdb rl record scan --limit 10
```

Ad-hoc inspection without editing the config — `--meta-file` is a
per-subcommand flag, so it goes after the verb:

```sh
fdb rl record get --meta-file ./meta.pb 42
```

---

## Schema evolution

Every metadata source must provide the record definitions required by the
Record Layer builder. When you upgrade a schema:

- **Descriptor input (Path A)**: recompile the descriptor set, including
  imports, and ship the artifact matching the application's declarations.
  Keep version annotations or an explicit metadata-version override aligned
  with that schema. `--meta-version` does not validate a historical evolution
  or recover metadata from the target store.
- **Persisted metadata (Path B)**: call `saveRecordMetaData` with the new
  metadata; the old version is auto-archived to a history key.
  `MetaDataEvolutionValidator` runs inside `saveRecordMetaData` — it'll
  reject invalid transitions (type rename, incompatible field change,
  removed required field) — and the new version must be strictly greater
  than the stored one. Both checks run in the same transaction as the write
  (in Go and in Java), so concurrent evolvers serialize through FDB conflict
  detection.
- **SQL catalog (Path C)**: SQL DDL supports initial creation, not template
  evolution; `ALTER SCHEMA TEMPLATE` is not implemented. Build a higher-version
  template with the Go metadata builder (`metadata.NewSchemaTemplateBuilder`
  and `SetVersion`), then save it with `fleet.SaveTemplate`. Rebind existing
  schemas with `fdb rl meta catalog repair --database <db> --all-schemas --yes`
  (the catalog's `RepairSchema` API), then build pending indexes with
  `fdb rl index build --database <db> --all-schemas --yes`. Saving a template
  alone does not advance schemas pinned to an older version.
- **Complete snapshots**: export a new `meta.pb` and ship it with the app.
  Check two complete snapshots with
  `fdb rl meta evolve-check --old old.pb --new new.pb` before rolling.

Go and Java can read the same complete `MetaData` snapshot. Descriptor
input reconstructs the annotated declarations; it is not a substitute for
missing code-only customizations or schema-evolution history.

---

## Pitfalls

### `MetaData.records is empty`

`fdb` fails with this error when it loads a `MetaData` whose `records`
`FileDescriptorProto` is not set. That means the app dumped a partial
or incremental metadata update. Fix:

- **Go**: ensure you call `recordlayer.WriteRecordMetaData(meta, w)`
  with the fully-built `*RecordMetaData`, not a `*RecordMetaDataProto.MetaData`
  you hand-assembled.
- **Java**: ensure you call `meta.toProto().writeTo(out)`, not
  `MetaDataProtoEditor` output intended for incremental patches.

### Old metadata version in `FDBMetaDataStore`

If `fdb` reports index states as `WRITE_ONLY` when your app thinks
they're `READABLE`, check that the app ran `saveRecordMetaData` with
the current metadata version. Version mismatches between what the app
builds and what's persisted are the #1 source of "why is `fdb` wrong?"
reports.

### Annotations versus code-only customizations

Compiled `.proto` annotations are a supported metadata source. If the
records schema declares its Record Layer union, primary keys, indexes, and
schema options, a descriptor set is sufficient for that metadata; no
programmatic export is required.

The CLI cannot infer customizations made only by application builder calls.
A descriptor set does not discover those indexes or their evolution history.
If they matter for the store you are inspecting, use its persisted metadata
or an optional complete `meta.pb` snapshot instead. An ordinary protobuf
message definition without the necessary Record Layer declarations still
fails the normal builder validation.

---

## FAQ

**Q: My app is Java and `fdb` is written in Go. Does that matter?**
No. Descriptor input reads standard protobuf descriptors and Record Layer
custom options, independent of the application's language. A complete
`meta.pb` written by Java's `meta.toProto().writeTo(out)` also works with
commands accepting `--meta-file`.

**Q: Can I run `fdb` with no config file?**
Yes. Pass `--cluster-file` and `--database`/`--schema` on relational
commands, or use `--keyspace-tuple` and descriptor input for a standalone
Record Layer store. These flags belong to the subcommand, not the root:

```sh
fdb rl record scan --cluster-file ./fdb.cluster \
  --keyspace-tuple '["myapp", "orders"]' \
  --descriptor-set schema.binpb --records-file myapp/records.proto --limit 10
fdb sql --cluster-file ./fdb.cluster --database /FRL/myapp --schema main
```

A complete snapshot can be selected with `--meta-file` instead of
`--descriptor-set`.

A context is more convenient for repeated use. The default config is
`~/.fdb/config.yaml`; `FDB_CONFIG` overrides its path, and
`fdb config path` prints the effective path.

**Q: Do I need to rebuild `fdb` when my schema changes?**
No. `fdb` is schema-agnostic — it decodes records using whatever
metadata you point it at. Recompile a descriptor set or refresh a complete
snapshot when its schema changes. A new CLI binary is needed only for new
`fdb` features or bug fixes.

**Q: Where does `fdb` keep local state?**
Configuration lives in `~/.fdb/config.yaml` (or the path in `FDB_CONFIG`).
The interactive SQL shell persists history in `~/.fdb/sql_history`.
Record Layer commands read the cluster file and metadata source fresh;
they do not cache store contents locally.

**Q: Does `fdb` write to FDB?**
Read commands never do — they open stores with rebuild checks disabled,
so even newer descriptor metadata or a `--meta-file` cannot make them
mutate the store they inspect (`rl store info/dump`, `rl record get/scan/count`, `rl index
ls/describe/scan`, `rl meta get/validate/evolve-check/diff`, `rl meta types
ls/describe`, `rl meta catalog databases/schemas/templates/get`,
`rl keyspace resolve`, `tx read-version`).
The write surface is explicit: **`fdb sql`** executes arbitrary SQL
(INSERT/DELETE/DDL, no read-only guard); **`fdb dev up`** configures a
local Docker FDB and writes a context; and the guarded record-layer
write commands below.

## Write commands

The guarded Record Layer write commands listed below require `--yes` or
an interactive confirmation, and none can target `__SYS/CATALOG` (the relational
layer's own bookkeeping). Use the versioned template/catalog APIs and rebind
workflow described under [schema evolution](#schema-evolution) instead.

- **`rl store create`** — create a new native store using the context's
  keyspace and metadata source, or the shared addressing/metadata flags.
  Requires the normal confirmation (`--yes` for scripts). Creation initializes
  the header and index states atomically, refusing an existing store or a
  nonempty prefix. It is not a reset or an existing-store metadata update.
  Definitions remain in the chosen descriptor set, complete metadata file,
  or `FDBMetaDataStore`; they are not copied into the record-store subspace.
  SQL-managed stores must be created through SQL DDL, not this command.
  Record reads and writes do not implicitly create a missing store. See the
  [complete protobuf tutorial](/docs/cli/protobuf/) for the full flow.
- **`rl record put --type T '<json>'`** / **`rl record delete <pk>`** — both
  take `--dry-run` (runs every validation through the store's dry-run
  primitives, writes nothing) and both are confirm-gated. Deleting an
  already-absent record exits 0 ("already absent") — after a
  maybe-committed retry the first attempt may have landed. **Caution:
  `rl record put` bypasses SQL-level constraints** not encoded in
  `RecordMetaData`. Record-layer index maintenance and uniqueness hold
  transactionally, but relational-only invariants (e.g. anything the
  SQL layer enforces at statement level) do not — prefer `fdb sql -c 'INSERT …'` for relational stores unless you know why you need the
  record-layer path.
- **`rl index build <name>`** — online index build with resumable
  range-set progress; safe to interrupt, rerun resumes.
  `--max-retries` defaults to 100 for retries and adaptive batch-halving.
  `--rps` throttles independently; `--limit` tunes batch size, and `--time-limit` bounds a
  pass. `rl index rebuild` clears and starts over; `rl index set-state`
  flips READABLE / READABLE_UNIQUE_PENDING / WRITE_ONLY / DISABLED
  (READABLE refuses unless the
  index is fully built). One caveat: the indexer opens the store the
  way an app would (Java `OnlineIndexer` parity, regular check-version
  path), so building against a NEWER metadata source migrates the
  store header first — exactly as deploying that metadata would.
- **`rl meta apply --file new.pb`** — validates against the
  FDBMetaDataStore's current metadata (same gate as `evolve-check`)
  and persists on pass; the validation re-runs inside the save
  transaction, so a concurrent evolution landing between the prompt
  and the write is detected, never overwritten. Re-applying
  already-current metadata is a no-op success; the version must
  strictly increase (no `--allow-no-version-change` here — the store
  save path rejects equal versions unconditionally, like Java's
  `saveAndSetCurrent`). Requires `meta_store_keyspace` in the context
  or `--meta-store-keyspace`; `--force-initial` bootstraps an empty
  store. Descriptor sets and `meta.pb` files do not themselves persist
  metadata in FDB.
- **`rl store lock <state> [--reason …]`** / **`rl store unlock`** — set or
  clear the header lock (`forbid-record-update` / `full-store`);
  `rl store info` shows it. Both commands can manage a store that is
  already `full-store` locked (they open with the stored reason as the
  bypass); other record-store-opening commands — truncate included —
  refuse a fully locked store until you unlock it. Header inspection,
  forensic dumps, and metadata-only readers remain available. **`rl store truncate`** deletes every
  record — double-gated: `--yes` is always required, and a terminal
  additionally asks you to type the store address back.

**Q: What are `fdb sql` and `fdb rl meta catalog`?**
The relational-layer side of the CLI. `fdb sql` is a psql-style REPL
(also scriptable via `-c` / `-f` / stdin) over the `fdbsql`
`database/sql` driver; `fdb rl meta catalog databases/schemas/templates/get`
reads the relational catalog at `__SYS/CATALOG` — schema auto-discovery
with no `meta_file` wiring. Neither needs `keyspace_path` or a metadata
source in the context; they address stores by `--database`/`--schema`.
Without a relational catalog, `databases`, `schemas`, and `templates` return
successful empty listings without creating a persisted catalog; `get` fails
because the requested template does not exist. Native Record Layer stores use
the metadata sources in Path A/B instead.

## Planner statistics

The [command reference](/docs/cli/reference/) lists the statistics commands
and their flags. The following explains their effect on SQL planning.

The query planner orders joins by how big it thinks each table is. With no
statistics it thinks every table is the same size, so on a join it picks a side
by a tie-break over plan structure — right or wrong by luck, and the same way
every time. `fdb rl stats` gives it the real numbers.

Measured on a 1,000,000-row table joined against a 50-row one, the arrangement
where the tie-break guesses wrong runs **dramatically faster** once statistics
are on; the arrangement where it already guessed right does not move. See
`TestFDB_Stress_StatisticsJoinOrder`.

```sh
# One schema.
fdb rl stats collect --database /FRL/myapp --schema MAIN

# Every schema in the database — one scan each, per-schema failure isolation,
# bounded concurrency. This is the fleet form, same machinery as
# `rl index build --all-schemas`.
fdb rl stats collect --database /FRL/myapp --all-schemas

# What is stored, and whether the planner will actually use it.
fdb rl stats show --database /FRL/myapp --schema MAIN
fdb rl stats show --database /FRL/myapp --schema MAIN -o json | jq '.per_type'
```

Keys are the SQL identifiers you wrote, not the escaped storage names the record
layer uses internally: a table quoted `"MY$TABLE"` is stored as `MY__1TABLE` and
reported here as `MY$TABLE`. The names `fdb rl stats` prints follow that rule --
`per_type`, `missing_types`, `extra_types`, the skipped list, the abort banner
and the fan-out's refusal text -- so a script may key by the name it created the
table with.

`synthetic_types` is the ONE exception, and deliberately so: it names joined and
unnested record types, whose names Java stores exactly as the caller passed them
to `addJoinedRecordType`. They are not known to be escaped, and this port never
creates one, so `MY__1JOINED` could equally be a literal name or the escaping of
`MY$JOINED`. It is printed verbatim, because the only thing you can do with it
is match it against your Java-side metadata.

Besides `synthetic_types` above, three more things in `fdb` output are
deliberately NOT translated — four in total — and they are worth knowing before
reading the list that is:

- **Index names.** A separate namespace; `GetIndex` has no escape fallback, so a
  decoded index name would not resolve when passed back in.
- **Context names.** Local to this CLI, never a record type.
- **The `Proto message:` line of `fdb rl meta types describe`** (`proto_message` in
  JSON). That field reports the protobuf descriptor's full name, so the escaped
  spelling is the correct answer there -- the `Name:` field one line above it is
  the SQL identifier. One command, two namespaces, on purpose.

Everything else that prints a record-type name prints the SQL identifier:

- `fdb rl meta types` (text and JSON), and `fdb rl meta types describe`'s `Name` field
- `fdb rl meta diff` and `fdb rl index describe`
- the `record_type` field of `fdb rl record get`, `fdb rl record put` and
  `fdb rl record scan` JSON output (all three go through one writer)
- `fdb sql`'s `\d` table list and `\d <table>` column list, and its
  `not found -- available: ...` message
- shell completion, and the `not found -- available: ...` list shared by
  `fdb rl record scan` and `fdb rl record count`

Record scans already emit NDJSON: `fdb rl record scan --limit 10 | jq -r '.record_type'`
extracts type names without an `-o`/`--output` flag, which `record scan` does not accept.

A name copied out of any of them can be passed straight back in -- as `--type`,
or to `\d` -- and that holds because of two guards working together, not because
the lookup is forgiving:

- A name is shown decoded only when the decoded spelling **provably re-encodes
  to the stored one**. Where it does not (a record type legitimately named
  `__0Order`, never escaped from anything), the stored name is printed
  unchanged.
- Where a schema declares two types whose names **collide across the two
  namespaces** -- SQL `MY$TABLE` stores as `MY__1TABLE`, and a table whose SQL
  name IS `MY__1TABLE` stores as `MY__01TABLE` -- every command prints the
  STORED names instead. Uglier, and the only spellings that resolve to the type
  you meant. A schema in that state wants renaming.

```sh

fdb rl stats clear --database /FRL/myapp --schema MAIN --yes
```

**They are opt-in per connection.** Collecting changes nothing on its own; a
connection asks for them:

```
fdbsql:///FRL/MYAPP?schema=MAIN&planner_statistics=true
```

Two connections differing only in this flag do not share cached plans.

**Collection is an offline job.** It reads every record, in
continuation-driven batches so no single transaction approaches FDB's 5s limit.
Schedule it like an index build, sized to how fast your data changes SHAPE —
not how fast it changes. A table that doubles matters; a table where rows are
updated in place does not.

**`show` reports the planner's own verdict**, from the planner's own code — so
`USABLE` means the planner accepts it, not that this command found some bytes.
When it says `NOT USABLE` it names the gate that refused:

| Verdict | Meaning | Fix |
|---|---|---|
| `not collected` | nothing stored for this schema | run `collect` |
| `expired` | older than ~24h | run `collect` |
| `incomplete` | at least one table has no entry | inspect the reported missing types and collect the complete schema |
| `stamped ahead of the cluster` | the entry's version is ahead of the cluster's — a restore from backup moves versions backwards | run `collect` |
| `cluster version unavailable` | the freshness check could not read a version | a cluster problem, not a statistics problem |

An **empty table is fine** — it collects as an exact 0 and counts as complete.
Exceeding `--max-records-per-type` aborts the entire collection, exits non-zero,
and stores nothing; it does not save a partial set with that table missing.
Any previous statistics remain in place without being refreshed. Increase the
cap or remove it to collect the complete schema, or clear the old statistics
if you no longer want the planner to use them.

Completeness is **schema-wide**: one uncollected table disables statistics for
every query in that schema. That is deliberate. A missing table is treated as
the planner's default 1,000,000 rows, which standing beside a real 150-row
count would rank the missing table as the largest in the schema and drive the
join from the wrong side. Half a statistic is worse than none.

**Staleness cannot corrupt results.** A stale count can make the planner choose
a worse plan; it can never make a query return wrong rows. Row counts feed the
COST model only — never the structural bounds a rule uses to drop a `DISTINCT`
or a sort. If you would rather have no statistics than old ones, `clear` them.

**Java is unaffected.** Statistics are stored outside every record store's
subspace, so a Java client sharing the cluster neither sees nor is disturbed by
them, and `clear` cannot reach record or index data.
