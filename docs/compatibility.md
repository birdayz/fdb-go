# Compatibility boundaries

This page describes the development tree. It is not a promise that all Java
features work, that all historical Go releases share one storage format, or that
an arbitrary Java query plan can resume in Go. See [status](../STATUS.md) and
[upgrade guidance](upgrade.md).

## Reference versions

| Component | Reference / boundary |
|---|---|
| Java Record Layer and Relational | **4.14.2.0**, pinned artifacts in `MODULE.bazel` |
| FoundationDB | **7.3.77**, the client source and test-cluster pin |
| Pure-Go transport | FDB **7.3 protocol** (`ProtocolVersion73` in `pkg/fdbgo/transport/handshake.go`); not a multi-version client |
| Store format | Go default **14**, Java default **7**; both support through **15** in the pinned reference |
| Go/tooling | Exact versions in `go.mod`, `MODULE.bazel` and `.bazelversion` |

FDB 8.0 compatibility is **not established by this repository's 7.3 target**.
This is a support boundary, not a statement about whether upstream has released
8.0. Updating the API version number alone cannot add another wire protocol.

## What sharing data means

The Record Layer targets Java's tuple keys, record serialization, split records,
versions, store headers and supported index layouts. The conformance suite in
`conformance/` performs bidirectional reads/writes for covered cases. The
`libfdbc` build tag changes the Record Layer / SQL transport backend, not the
higher-level storage layout; it does not remove the exceptions below.

### Exceptions to check before mixed Go/Java use

| Area | Boundary |
|---|---|
| **Lucene and spatial extensions** | This tree does not ship Java's Lucene index implementation or its geospatial extension maintainers. Unknown index types fail with `UnknownIndexTypeError` when a maintainer is requested; they are not silently maintained as VALUE indexes. Go's implemented `MULTIDIMENSIONAL` R-tree is a different feature, not a claim of Lucene or geospatial-extension parity. |
| **Synthetic record types** | Joined and unnested record type declarations survive metadata load/save, but are not executable Go record types. Do not assume Go can query or maintain their synthetic indexes. Preserving a declaration is not implementing it. |
| **Metadata preservation** | `RecordMetaData` preserves joined/unnested declarations, UDFs, views, stored queries and unknown proto fields on round-trip. This is not a blanket claim that UDFs or views are unsupported: SQL has its own implemented surface. Check the operation and the tested SQL corpus rather than inferring support from proto presence. |
| **Vector indexes** | Java-compatible `vector` engines and the Go-only `vector_spfresh` type are distinct. SPFresh's layout is not readable by Java's vector maintainer. Link `pkg/recordlayer/vectorindex` for either type; an unlinked known type produces `IndexMaintainerNotLinkedError`. Persist explicit engine/options for shared deployments and use Java's canonical option names/values; Go also accepts some metric spellings Java does not. Do not infer matching configuration from the word “vector” alone. |
| **Collation** | Go's `collate_jre` and `collate_icu` use `golang.org/x/text/collate`; Java uses JRE and ICU4J collation respectively. Store operations that use those incompatible keys fail by default with `GoOnlyCollationError`. `StoreBuilder.SetGoOnlyCollation(true)` is only for stores never shared with Java. See the collation section below. |
| **TEXT / Unicode** | The index layout alone does not guarantee equivalent token keys. Go's default tokenizer uses `uniseg` word segmentation and Go Unicode normalization/casing; Java uses `BreakIterator.getWordInstance(Locale.ROOT)` and Java's Unicode implementation. Cross-language equivalence for arbitrary scripts and Unicode versions is not established. Validate tokenization for your language corpus before sharing a TEXT index. |
| **Continuations** | Leaf and structural cursor framing has Java conformance coverage. SQL statement continuations are engine-private; Go rejects caller-supplied `api.OptContinuation`. Go streaming-aggregate accumulator payloads and in-memory-sort state are not Java-compatible, even where a protobuf message name is shared. Do not exchange these tokens between engines or assume stability across plan changes. |
| **Store format** | An unpinned Go open can upgrade an older store to format 14 when its transaction commits. Java's default 7 is not its maximum: Java 4.14.2.0 can open 14 and 15. Older readers and downgrade paths must be checked separately. |
| **Earlier Go SQL storage** | The development tree changed the relational catalog/database/schema keyspace after v0.1.0. There is no automatic migration from the old Go layout. A common FDB cluster does not make these layouts interchangeable. |

### Vector defaults are layer-specific

`DefaultHNSWConfig` is a low-level HNSW configuration, not a general promise about
SQL DDL or SPFresh. For the Java-compatible `vector` type, an absent
`vectorEngine` selects HNSW in both implementations. `DefaultHNSWConfig` uses
Euclidean distance, M=16, MMax=16, MMax0=32, efConstruction=200 and efRepair=64;
inlining is false. These defaults are not a blanket Go/Java divergence.
SPFresh has its own defaults in `DefaultSPFreshConfig` and its own persisted layout.
Use explicit, matching metadata rather than relying on another engine's defaults.
The Java-compatible HNSW/GuardiANN option readers and the SQL DDL option mapping
are separate code paths; tests cover them separately.

### Collation safety

Go collation keys are not Java/ICU-compatible. By default, stores return
`GoOnlyCollationError` for collated index access, validation and rebuilding through
the store/indexer APIs; single-record saves/deletes and batch saves that must
maintain a collated index or count key; and collated primary-key evaluation.
Disabled indexes do not require maintenance. Metadata round-trips, raw-key record
reads and range clears remain available. Opening an evolved store can refuse when
reconciling collated indexes or count keys, including enabling queued maintenance.

Only stores that no Java process opens may opt in with
`StoreBuilder.SetGoOnlyCollation(true)`. The option is not persisted, and it does
not migrate existing keys. Online indexers inherit it through
`OnlineIndexerBuilder.SetRecordStoreBuilder`. Direct expression evaluation and
explicitly constructed low-level maintainers are outside the store guard and can
produce Go-only bytes. The Go-only SPFresh bulk builder also uses those low-level
maintainers; its index layout must never be shared with Java.

The Java oracle pins sampled mismatches for both providers and reports the JVM
runtime/vendor, locale-provider configuration, registry and ICU version. Matching
locale and strength is not sufficient: JRE provider/rule versions and ICU's
algorithm/data versions are part of the sort-key format.

Treat any existing Go-collated store as Go-only. Switching a transport backend to
libfdb_c does not switch the collation implementation. Java can read raw bytes,
but computes different lookup/order keys; that is why sharing collated indexes is
unsafe. A Java-compatible collation migration requires regenerating keys/indexes
with the intended implementation. Changing a flag or relabeling metadata cannot
convert persisted sort keys. Do not use a shared Java/Go writer rollout to perform
that migration.

## Sources and verification

The concrete boundaries above can be checked in:

- `pkg/recordlayer/store.go`, `store_builder.go` and `format_version_split_test.go`
  (format target versus supported ceiling and index dispatch);
- `pkg/recordlayer/metadata.go` and `metadata_proto.go` (metadata preservation);
- `pkg/recordlayer/vectorindex/` and `pkg/relational/core/embedded/ddl.go`
  (vector options, engines and Go extensions);
- `pkg/recordlayer/text_tokenizer.go` and `collate_function_key_expression.go`;
- `pkg/relational/core/embedded/cascades_generator.go` and the “SQL statement
  continuations are engine-private” section of [DIVERGENCES.md](../DIVERGENCES.md);
- [CHANGELOG.md](../CHANGELOG.md), especially the unreleased storage changes.

For SQL, [FEATURE_MATRIX.md](../FEATURE_MATRIX.md) inventories scenarios, not a
standards certification or proof for every combination of features. The historical
[SQL_CONFORMANCE.md](../SQL_CONFORMANCE.md) matrix records an earlier reference;
use the pinned Java version and current cross-engine tests when evaluating a query.
