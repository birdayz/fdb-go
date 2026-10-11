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
| **Collation** | Go's `collate_jre` and `collate_icu` use `golang.org/x/text/collate`; Java uses JRE and ICU4J collation respectively. Their sort-key bytes are not an interoperability contract. Do not share collated key/index data between these implementations. See the collation section below. |
| **TEXT / Unicode** | Go's default tokenizer applies Java's per-word steps (NFKD, a Basic Multilingual Plane letter or digit, `toLowerCase(Locale.ROOT)` with final sigma, marks removed); the Java conformance suite compares their tokens for Latin, Greek, Cyrillic, Hebrew, Arabic, Hangul, Indic and supplementary-plane text. Word segmentation differs: Go uses UAX #29 (`uniseg`), Java the JDK's `BreakIterator.getWordInstance(Locale.ROOT)`. Text written without spaces (Han, kana, Thai, Lao, Khmer, Myanmar) and some connectors (`_`, `:`, `’`, `·`, `.` before a digit, `$` or `%` beside digits, U+200B, superscript digits) give different tokens, as do a few Greek words whose final-sigma context NFKD changes. Each side uses its runtime's Unicode tables: Go's follow the Go toolchain (15.0 through Go 1.26, 17.0 from Go 1.27; `uniseg` 15.0), Java's the JVM (JDK 21: 15.0). Characters assigned or changed between those versions tokenize differently, and a Go TEXT index may need rebuilding when the toolchain's Unicode version changes. Do not share a TEXT index over such text. |
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

Go collation keys are not Java/ICU-compatible. Do not use Go collation for stores
shared with Java writers, including collated indexes, primary keys, and count keys.
This revision does not reject those operations by default. Evaluating a collation
expression directly also produces Go-only bytes, not Java-compatible keys.

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
