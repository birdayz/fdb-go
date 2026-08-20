# Operator guide — wiring your app for `frl`

`frl` is an introspection CLI for stores built on the FoundationDB Record
Layer. It reads `DataStoreInfo` headers, decodes records, lists indexes,
and dumps metadata. To do any of that it needs your app's
`RecordMetaData` — the same schema your app already builds at startup.

This guide shows the two supported ways to expose that metadata, in Go
and in Java.

## Quick decision tree

```
  Does your app already use FDBMetaDataStore?
         │
       no│yes
         ▼
   Path A          Path B
   (dump meta.pb)  (FDBMetaDataStore)
```

Most apps — **including Apple's documented default** — build metadata
programmatically and don't persist it. Those apps want Path A. If you
already adopted `FDBMetaDataStore` for schema-evolution reasons, Path B
has zero extra steps.

Both paths produce the same `RecordMetaData` for `frl`, and every
command accepts both — the metadata-only commands (`meta get`, `meta
types ls/describe`, `index describe`) dial FDB only when the source
actually lives there, so `meta_file` setups stay fully offline. A third
source exists for relational clusters: `--database`/`--schema` resolves
metadata from the catalog's schema-pinned template.

---

## Path A — dump `meta.pb` (recommended for programmatic metadata)

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

### 3. Configure `frl`

```yaml
# ~/.frl/config.yaml
current_context: prod
contexts:
  - name: prod
    cluster_file: /etc/foundationdb/prod.cluster
    keyspace_path: /myapp/prod/orders
    metadata:
      meta_file: /etc/myapp/meta.pb
```

```sh
frl config use-context prod
frl store info
frl record scan --limit 10
```

Ad-hoc inspection without editing the config — `--meta-file` is a
per-subcommand flag, so it goes after the verb:

```sh
frl record get --meta-file ./meta.pb 42
```

---

## Path B — use `FDBMetaDataStore`

If your app already persists metadata via `FDBMetaDataStore`, `frl`
needs nothing beyond the keyspace path where that store lives.

### Go

If you're not already doing this, add it once during app init:

```go
// NewFDBMetaDataStore takes a single subspace.Subspace — pass whatever
// subspace your app uses for the metadata store (often derived from a
// KeySpace path via path.ToSubspace() or built with subspace.Sub()).
metaStore := recordlayer.NewFDBMetaDataStore(metaSubspace)
_, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
    return nil, metaStore.SaveRecordMetaData(rtx.Transaction(), meta.ToProto())
})
```

### Java

```java
FDBMetaDataStore store = new FDBMetaDataStore(context, metaDataPath);
store.saveRecordMetaData(meta).join();
```

### Configure `frl`

```yaml
contexts:
  - name: prod
    cluster_file: /etc/foundationdb/prod.cluster
    keyspace_path: /myapp/prod/orders
    metadata:
      meta_store_keyspace: /myapp/prod/_meta
```

That's the whole wiring. `frl` reads the current `MetaData` from FDB on
every command — zero external artifacts, no staleness.

---

## Schema evolution

Both paths require that the `MetaData.records` `FileDescriptorProto` is
populated. When you upgrade schema:

- **Path A**: rebuild `meta.pb` as part of your deploy pipeline, ship
  the new artifact alongside the new binary, and update the
  `meta_file:` path (or overwrite in place). Validate the evolution
  with `frl meta evolve-check --old old.pb --new new.pb` in CI before
  rolling.
- **Path B**: call `saveRecordMetaData` with the new metadata; the old
  version is auto-archived to a history key. `MetaDataEvolutionValidator`
  runs inside `saveRecordMetaData` — it'll reject invalid transitions
  (type rename, incompatible field change, removed required field) —
  and the new version must be strictly greater than the stored one.
  Both checks run in the same transaction as the write (in Go and in
  Java), so concurrent evolvers serialize through FDB conflict
  detection.

Both paths produce binary-compatible `MetaData` protos — the same bytes
work in both.

---

## Pitfalls

### `MetaData.records is empty`

`frl` fails with this error when it loads a `MetaData` whose `records`
`FileDescriptorProto` is not set. That means the app dumped a partial
or incremental metadata update. Fix:

- **Go**: ensure you call `recordlayer.WriteRecordMetaData(meta, w)`
  with the fully-built `*RecordMetaData`, not a `*RecordMetaDataProto.MetaData`
  you hand-assembled.
- **Java**: ensure you call `meta.toProto().writeTo(out)`, not
  `MetaDataProtoEditor` output intended for incremental patches.

### Old metadata version in `FDBMetaDataStore`

If `frl` reports index states as `WRITE_ONLY` when your app thinks
they're `READABLE`, check that the app ran `saveRecordMetaData` with
the current metadata version. Version mismatches between what the app
builds and what's persisted are the #1 source of "why is `frl` wrong?"
reports.

### Programmatic-only metadata with no dumper

If your app has neither `FDBMetaDataStore` nor a `meta.pb` dumper,
`frl` cannot introspect your store. There is no workaround short of
adding one of the two paths. Parsing `.proto` files alone is not
sufficient — many index types (RANK, TEXT, VECTOR, etc.) can't be
expressed via proto options.

---

## FAQ

**Q: My app is Java and `frl` is written in Go. Does that matter?**
No. `MetaData` wire format is identical; a `meta.pb` written by Java's
`meta.toProto().writeTo(out)` deserializes cleanly with any `frl`
command that accepts `--meta-file`.

**Q: Can I run `frl` with no config file?**
Not fully in v1. `--meta-file` is available on every read command that
touches metadata (`record get/scan/count`, `index ls/describe/scan`,
`meta get/types ls/types describe`), but there are no root-level
`--cluster-file` / `--keyspace-path` flags yet — those live only in
the context. Until the root-level overrides land, the minimum
ergonomic setup is a one-context `~/.frl/config.yaml`.

**Q: Do I need to rebuild `frl` when my schema changes?**
No. `frl` is schema-agnostic — it decodes records using whatever
metadata you point it at. A new binary only ships for new `frl`
features or bug fixes.

**Q: Where does `frl` cache anything?**
Nowhere. Every command reads the cluster file + metadata source fresh.

**Q: Does `frl` write to FDB?**
Read commands never do — they open stores with rebuild checks disabled,
so even a newer `--meta-file` cannot make them mutate the store they
inspect (`store info/dump`, `record get/scan/count`, `index
ls/describe/scan`, `meta get/validate/evolve-check/diff`, `meta types
ls/describe`, `meta catalog …`, `keyspace resolve`, `tx read-version`).
The write surface is explicit: **`frl sql`** executes arbitrary SQL
(INSERT/DELETE/DDL, no read-only guard); **`frl fdb up`** configures a
local Docker FDB and writes a context; and the guarded record-layer
write commands below.

## Write commands

Every mutating command requires `--yes` or an interactive confirmation,
and none can ever target `__SYS/CATALOG` (the relational layer's own
bookkeeping — evolve schemas through SQL DDL instead).

- **`record put --type T '<json>'`** / **`record delete <pk>`** — both
  take `--dry-run` (runs every validation through the store's dry-run
  primitives, writes nothing) and both are confirm-gated. Deleting an
  already-absent record exits 0 ("already absent") — after a
  maybe-committed retry the first attempt may have landed. **Caution:
  `record put` bypasses SQL-level constraints** not encoded in
  `RecordMetaData`. Record-layer index maintenance and uniqueness hold
  transactionally, but relational-only invariants (e.g. anything the
  SQL layer enforces at statement level) do not — prefer `frl sql
  INSERT` for relational stores unless you know why you need the
  record-layer path.
- **`index build <name>`** — online index build with resumable
  range-set progress; safe to interrupt, rerun resumes.
  `--max-retries` defaults to 100 (enables throttling + adaptive
  batch-halving), `--rps`/`--limit` tune load, `--time-limit` bounds a
  pass. `index rebuild` clears and starts over; `index set-state`
  flips READABLE / WRITE_ONLY / DISABLED (READABLE refuses unless the
  index is fully built). One caveat: the indexer opens the store the
  way an app would (Java `OnlineIndexer` parity, regular check-version
  path), so building against a NEWER metadata source migrates the
  store header first — exactly as deploying that metadata would.
- **`meta apply --file new.pb`** — validates against the
  FDBMetaDataStore's current metadata (same gate as `evolve-check`)
  and persists on pass; the validation re-runs inside the save
  transaction, so a concurrent evolution landing between the prompt
  and the write is detected, never overwritten. Re-applying
  already-current metadata is a no-op success; the version must
  strictly increase (no `--allow-no-version-change` here — the store
  save path rejects equal versions unconditionally, like Java's
  `saveAndSetCurrent`). Requires `meta_store_keyspace` in the context
  or `--meta-store-keyspace`; `--force-initial` bootstraps an empty
  store. Path A setups have nothing in FDB to apply to.
- **`store lock <state> [--reason …]`** / **`store unlock`** — set or
  clear the header lock (`forbid-record-update` / `full-store`);
  `store info` shows it. Both commands can manage a store that is
  already `full-store` locked (they open with the stored reason as the
  bypass); every other command — truncate included — refuses a fully
  locked store until you unlock it. **`store truncate`** deletes every
  record — double-gated: `--yes` is always required, and a terminal
  additionally asks you to type the store address back.

**Q: What are `frl sql` and `frl meta catalog`?**
The relational-layer side of the CLI. `frl sql` is a psql-style REPL
(also scriptable via `-c` / `-f` / stdin) over the `fdbsql`
`database/sql` driver; `frl meta catalog databases/schemas/templates/get`
reads the relational catalog at `__SYS/CATALOG` — schema auto-discovery
with no `meta_file` wiring. Neither needs `keyspace_path` or a metadata
source in the context; they address stores by `--database`/`--schema`.
Plain-core clusters (no relational layer) get a clear "no relational
catalog on this cluster" error pointing back at Path A/B.

## Planner statistics

The query planner orders joins by how big it thinks each table is. With no
statistics it thinks every table is the same size, so on a join it picks a side
by a tie-break over plan structure — right or wrong by luck, and the same way
every time. `frl stats` gives it the real numbers.

Measured on a 1,000,000-row table joined against a 50-row one, the arrangement
where the tie-break guesses wrong runs **dramatically faster** once statistics
are on; the arrangement where it already guessed right does not move. See
`TestFDB_Stress_StatisticsJoinOrder`.

```sh
# One schema.
frl stats collect --database /myapp --schema MAIN

# Every schema in the database — one scan each, per-schema failure isolation,
# bounded concurrency. This is the fleet form, same machinery as
# `index build --all-schemas`.
frl stats collect --database /myapp --all-schemas

# What is stored, and whether the planner will actually use it.
frl stats show --database /myapp --schema MAIN
frl stats show --database /myapp --schema MAIN -o json | jq '.per_type'

frl stats clear --database /myapp --schema MAIN --yes
```

**They are opt-in per connection.** Collecting changes nothing on its own; a
connection asks for them:

```
fdbsql:///myapp?schema=MAIN&planner_statistics=true
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
| `incomplete` | at least one table has no entry | run `collect`; if it recurs, a table is exceeding `--max-records-per-type` |
| `stamped ahead of the cluster` | the entry's version is ahead of the cluster's — a restore from backup moves versions backwards | run `collect` |
| `cluster version unavailable` | the freshness check could not read a version | a cluster problem, not a statistics problem |

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
