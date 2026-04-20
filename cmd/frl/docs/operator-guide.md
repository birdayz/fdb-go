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

Both paths produce the same `RecordMetaData` for `frl`. Commands work
identically.

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

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

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
Nowhere yet. Every command reads the cluster file + metadata source
fresh. If/when BSR-style remote schema sources return, they'll cache
under `~/.frl/cache/`.

**Q: Does `frl` write to FDB?**
No — every v1 command is read-only (`store info/dump`, `record
get/scan/count`, `index ls/describe/scan`, `meta get/validate/
evolve-check/diff`, `meta types ls/describe`, `keyspace resolve`,
`tx read-version`). Future `store truncate` / `store destroy` /
`index build` / `meta apply` commands will write, and those are
designed separately with explicit confirmation flags and dry-run
defaults.
