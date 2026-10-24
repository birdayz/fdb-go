# Compatibility boundaries

This page describes the v0.2.0 candidate and development tree, not a published tag. It is not a promise that all Java
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
| **Vector indexes** | Java-compatible `vector` engines and the Go-only `vector_spfresh` type are distinct. SPFresh's layout is not readable by Java's vector maintainer. Link `pkg/recordlayer/vectorindex` for either type; an unlinked known type produces `IndexMaintainerNotLinkedError`. Persist explicit engine/options for shared deployments and use Java's canonical option names/values; a VECTOR index's structure and options are validated at build as in Java, the dimension count mandatory. The vector is read as Java reads it: value column 0 as serialized vector bytes, any other spelling refused at write. A binary that does not link `vectorindex` still builds a plain index with invalid options, and refuses it at use. Do not infer matching configuration from the word “vector” alone. |
| **Collation** | Go's `collate_jre` and `collate_icu` use a frozen x/text snapshot (UCA 6.2 / CLDR 23 weights, Unicode-15 normalization and classifiers), including locale parsing/matching; their keys do not follow the Go compiler's Unicode tables. Java uses JRE and ICU4J collation respectively. Store operations that use those incompatible keys fail by default with `GoOnlyCollationError`. `StoreBuilder.SetGoOnlyCollation(true)` is only for stores never shared with Java. See the collation section below. |
| **TEXT / Unicode** | Go's default tokenizer version 0 pins NFKD, letter/digit admission, simple casing, final-sigma properties and mark stripping to **Unicode 15.0.0**, independently of the Go compiler. Its Unicode edition therefore stays aligned with JDK 21; changing compilers does not change its tokens or positions. This preserves existing Go v0 data, not full Java tokenizer parity. Word segmentation still differs: Go uses UAX #29 (`uniseg` 15.0), Java `BreakIterator.getWordInstance(Locale.ROOT)`. Unspaced Han, kana, Thai, Lao, Khmer and Myanmar, plus some connectors (`_`, `:`, `’`, `·`, `.` before a digit, `$` or `%` beside digits, U+200B, superscript digits), give different tokens. Go's final-sigma algorithm also differs from JDK 21's internal word-boundary scan and narrower Cased set: `ĿΣ` gives Go `l·ς` versus Java `l·σ`; U+10780 followed by Σ gives Go final ς versus Java σ. Go-written v0 postings for these inputs already differ from Java's. Do not share a TEXT index over such text. Full parity requires an explicit index migration/versioning policy; see [RFC-265](../rfcs/265-unicode15-compatibility-pin.md). |
| **Continuations** | Leaf and structural cursor framing has Java conformance coverage, including narrowed-range common-prefix encoding. Record scans and index scans (ordinary VALUE/VERSION, COUNT/SUM, MIN/MAX_EVER_TUPLE, MAX_EVER_VERSION and BITMAP_VALUE) return Java's last-key token and stop reason at an exact returned-row-limit boundary, followed by an empty page. SQL statement continuations are engine-private; Go rejects caller-supplied `api.OptContinuation`. Go streaming-aggregate accumulator payloads and in-memory-sort state are not Java-compatible, even where a protobuf message name is shared. Do not exchange these tokens between engines or assume stability across plan changes. |
| **Store format** | An unpinned Go open can upgrade an older store to format 14 when its transaction commits. Java's default 7 is not its maximum: Java 4.14.2.0 can open 14 and 15. Older readers and downgrade paths must be checked separately. |
| **Earlier Go SQL storage** | The v0.2.0 candidate changes the relational catalog/database/schema keyspace from v0.1.0's. There is no automatic migration from the old Go layout. A common FDB cluster does not make these layouts interchangeable. |

### Mapped-range backend capability

The pure-Go transaction exposes the optional `fdb.MappedRangeReader` capability
for exact primary ranges, with forward or reverse primary scans and forward
secondary ranges. Snapshot transactions do not expose it. The `libfdbc` backend
does not expose mapped reads: Apple's Go binding has no mapped-range API, and
this repository's raw C mapped-range decoder is a test-only, non-ABI oracle.
The Record Layer's `ScanIndexRecordsWithFetchMethod` supports
`IndexFetchMethod.USE_REMOTE_FETCH` and `USE_REMOTE_FETCH_WITH_FALLBACK` on the
pure-Go backend. On the `libfdbc` backend remote fetch reports Java's
`UnsupportedRemoteFetchIndexException`, so `USE_REMOTE_FETCH` fails and
`USE_REMOTE_FETCH_WITH_FALLBACK` scans the index and loads each record. A
`ScanIndexRecordsWithFetchMethod` remote-fetch scan with a positive
`ExecuteProperties.Skip` checks remote fetch's
preconditions and then runs as SCAN_AND_FETCH: a skip would be a selector offset
on the mapped read, a request shape the server does not support for every range.
Java sends that offset. The SQL option `INDEX_FETCH_METHOD`, when set, is the
method every BY_VALUE index scan of a plan fetches its records with, as in Java;
unset, Go uses `SCAN_AND_FETCH`, where Java defaults to
`USE_REMOTE_FETCH_WITH_FALLBACK`.

The optional raw `MappedRangeSkipReader` takes a signed 32-bit initial Skip and
ignores nonpositive values. Positive Skip is resolved client-side with ordinary
native key reads before sending a mapped request with plain selectors (offset 1
at both ends). FDB 7.3.77 does not support raw mapped selectors that resolve to an
inverted range. Large skips are resolved in bounded steps without integer
wrapping; `MaxInt32` means skipping that many rows, unlike Java's overflowing
forward selector addition. Skipped secondary records are not mapped, while
original primary conflict coverage is retained. Later batches resume without
reapplying Skip. Nonpositive Skip adds no resolution request. This raw capability
does not change the Record Layer's positive-Skip route described above.

### Single-record reads and conflicts

Modern record loads read the complete strict record subspace, `[packedPK + 00,
packedPK + ff)`, including inline versions and absent split suffixes. Saves,
deletes, dry runs, batch saves and index-record fetches use that same load.
The legacy layout without an unsplit suffix retains its bare-key point read;
old-format versions are read separately. Standalone `LoadRecordVersion` remains
a point read. `RecordExists` instead reads the first physical key under the
primary-key prefix, including the bare key, matching Java's limit-one scan.

Go has no Record Layer context-property storage or `LOAD_RECORDS_VIA_GETS`
setting. It implements Java's default range path; Java's optional GETS path
also protects the entire record range, but differs on some malformed layouts.
Neither read strategy changes the persisted record or version bytes.

Record scans of stores whose versions use the old separate format read each
version in its own request. Under a time limit or a failing scan limit, Go
returns every record completed before the stop. Java's record pipeline may drop
or pre-empt records whose version read is still pending
(MapPipelinedCursor.java:157-175), so Java can return fewer rows there.

### Index scan Skip

Index cursors (VALUE, VERSION, RANK, leaderboard, PERMUTED_MIN/MAX, COUNT/SUM,
MIN/MAX_EVER_LONG, MIN/MAX_EVER_TUPLE, MAX_EVER_VERSION and BITMAP_VALUE
entries) apply a positive `ExecuteProperties.Skip` as Java's `KeyValueCursorBase`
does: after the continuation replaces the resumed bound, the begin selector
(forward) or end selector (reverse) moves by the skip. The returned-row limit is
not widened by Skip, and the cursor neither returns skipped entries nor charges them to
scanned-record or byte limits. A caller that keeps Skip on a resumed page skips
again, as in Java.

Java adds the skip with `int` arithmetic, so a forward skip of
`Integer.MAX_VALUE` passes offset `Integer.MIN_VALUE` to `getRange`. Go
saturates the skip at `MaxInt32-1` in both directions instead; this differs from
an exact skip only beyond 2^31-2 entries, which in practice one transaction
cannot resolve.

The pure-Go client resolves the skipped selector with a `GetKey` that is not
bounded by the scan range. When Skip overshoots the remaining entries, Go reads
keys beyond the range until the selector resolves and, in a serializable
transaction, adds them to the read-conflict range; `libfdb_c` bounds both by the
range. Ordinary, covering and fetch query index plans pass Skip to the index
cursor when binding a single physical range without a residual key filter, so
skipped entries are neither scan-limit charges nor record loads. Multi-range
`ScanRangeSet` and key-filtered plans, other index types, and the mapped reads
of remote-fetch plans keep one outer Skip, plus the outer returned-row limit.
See DIVERGENCES.md.

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
standards certification or proof for every combination of features.
[SQL_CONFORMANCE.md](../SQL_CONFORMANCE.md) is a feature matrix against the pinned Java version,
measured by `DocClaimsJavaProbe`; the cross-engine tests cover combinations it does not.
