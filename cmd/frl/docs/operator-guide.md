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

Both paths produce the same `RecordMetaData` for `frl`. Store-reading
commands (`record get/scan/count`, `index ls/scan`) accept both paths.
**Path B support is not yet universal**: `meta get`, `meta types
ls/describe`, and `index describe` currently read `meta_file` sources
only and reject `meta_store_keyspace` with a clear error (they open no
FDB connection today; RFC-174 Slice 5 completes them). With Path B,
point those commands at an exported file via `--meta-file` in the
meantime.

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
  (type rename, incompatible field change, removed required field).

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
Two commands can. **`frl sql` executes arbitrary SQL** — including
`INSERT`, `DELETE`, `CREATE DATABASE`, and DDL — against the relational
layer; there is no read-only guard. **`frl fdb up`** configures a local
Docker FoundationDB (`configure new single memory`) and writes a context
into your frl config. Everything else is read-only: `store info/dump`,
`record get/scan/count`, `index ls/describe/scan`, `meta
get/validate/evolve-check/diff`, `meta types ls/describe`, `meta
catalog …`, `keyspace resolve`, `tx read-version` — and read-only
commands open stores with rebuild checks disabled, so even a newer
`--meta-file` cannot make them mutate the store they inspect. The
record-layer write wave (`record put/delete`, `index
build/rebuild/set-state`, `meta apply`, `store lock/truncate`) lands in
RFC-174 Slice 4 with confirmation flags and dry-run support.

**Q: What are `frl sql` and `frl meta catalog`?**
The relational-layer side of the CLI. `frl sql` is a psql-style REPL
(also scriptable via `-c` / `-f` / stdin) over the `fdbsql`
`database/sql` driver; `frl meta catalog databases/schemas/templates/get`
reads the relational catalog at `__SYS/CATALOG` — schema auto-discovery
with no `meta_file` wiring. Neither needs `keyspace_path` or a metadata
source in the context; they address stores by `--database`/`--schema`.
Plain-core clusters (no relational layer) get a clear "no relational
catalog on this cluster" error pointing back at Path A/B.
