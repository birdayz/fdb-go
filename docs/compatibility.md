# Compatibility boundaries

This page describes the v0.2.0 release scope plus explicitly marked **Unreleased
(post-v0.2.0)** development boundaries. The v0.2.0 scope remains frozen in the
changelog. **Publication is pending**; no published tag or completed gate is asserted. It is not a promise that all Java
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

## Full-text queries

The Go query executor accepts typed `plans.TextScan` predicates for ALL, ANY,
PHRASE, ALL_WITHIN (inclusive positional distance), PREFIX, ALL_PREFIXES and
ANY_PREFIX, and runs them through `recordlayer.TextScan`, the port of Java's
`TextScan`, once per bound physical group. String comparands use the index
tokenizer's QUERY mode and declared version; pre-tokenized `[]string`
comparands bypass normalization. Stop-word slots are retained for phrases.
ALL/ANY do not require repeated occurrences of repeated query terms; phrases
do. Scans combine postings by the complete suffix after the token, as Java's
`TextScan` does, then fetch ordinary stored records. SQL-equal signed-zero
groups are scanned as separate physical groups and keep their identity.
Original text cannot be reconstructed from tokens: text scans are not covering
scans of the original string. Existing filters, sorting, limits, distinct and
transaction retry infrastructure apply above this access path.

Positional matching follows Java 4.14.2.0's permissive omitted-position behavior:
a known-present term with no retained positions occupies a wildcard phrase slot
and is ignored for distance checks. If no positions remain, the positional test
passes. This is **not proof of exact phrase/proximity occurrence**. Like Java's
text planner, Go declines positional and prefix-set text access paths when index
metadata omits positions; ALL, ANY and singleton PREFIX remain eligible.
Grouping columns must all be bound, by an equality or an IN list, and carry
authoritative physical key types in the typed scan; an IN list runs the text
scan once per element under an IN join. Suffix bounds (which the SQL planner
does not build) are applied to the group's posting keys rather than seeked, so
a bound cannot skip matches inside a bunch whose first key precedes it. Prefix
scans cannot also have suffix bounds. A grouping column without a binding
leaves the query to ordinary record filtering, not an incomplete text scan.
Prefix overlaps are deduplicated by primary key by the planner; direct raw
text-index plans may intentionally return duplicate entries. ALL_PREFIXES is
Java's probable intersection with `Text.containsAllPrefixes`' default sizing;
SQL's predicate is strict, so the plan re-checks each record with the
predicate, as Java's planner does. Each group's continuation is `TextScan`'s,
wrapped in the scan-range set's envelope; whole query continuations are not
exchanged with Java. Existing low-level token-scan formats and persisted text
index maintenance are unchanged.

### SQL full-text syntax (Go extension)

Java Relational 4.14.2.0 reserves the lexer token `FULLTEXT`, but its parser and
built-in SQL function catalog do not expose full-text queries or full-text index
DDL. The following is explicitly a **Go SQL extension**, not Java SQL parity:

```sql
CREATE TABLE docs (id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id))
CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)

SELECT id FROM docs
WHERE tenant = ? AND TEXT_CONTAINS_ALL(body, ?)
ORDER BY id DESC LIMIT 20
```

`PARTITION BY` is optional. DDL requires exactly one scalar STRING text column;
partition columns must be scalar. The tokenized column must not also be part of
the primary key: normal primary-key trimming would replace the recoverable key
component with a token, so these indexes cannot safely fetch records. Go declines
such existing metadata as a text access path (ordinary record filtering remains
available), and raw executable text plans return a typed error. Existing stored
entries are not rewritten. The live Java 4.14.2.0 conformance probe
`probeTextPrimaryKeyOverlap` pins this boundary: PK positions `[0,-1]` turn
stored `("red blue",1)` into extracted `("red",1)`, and both index fetch and a
Java `TISCAN` query fail with `RecordCoreStorageException` despite the original
record existing. Java does not reconstruct this layout; this is not a deferred
Java-supported retrieval feature. See `conformance/text_index_conformance.java`
and its matching Go conformance assertion. DDL creates an ordinary wire-format
TEXT index
with default tokenizer/version options, not a separate SQL storage format.
`TEXT_CONTAINS_ALL`, `TEXT_CONTAINS_ANY`, `TEXT_CONTAINS_PHRASE`,
`TEXT_CONTAINS_PREFIX`, `TEXT_CONTAINS_ALL_PREFIXES` and
`TEXT_CONTAINS_ANY_PREFIX` take `(field, query [, literal_tokenizer_name])`.
`TEXT_CONTAINS_ALL_WITHIN` takes
`(field, query, nonnegative_literal_distance [, literal_tokenizer_name])`.
The field and query must be STRING (query parameters are allowed). SQL resolves
an omitted tokenizer name explicitly to `default`; an explicit name uses that
registered tokenizer's maximum supported version on both indexed and residual
paths. An index with a different tokenizer or an older version is not eligible
for that SQL predicate. The lower-level Record Layer comparison retains Java's
context-dependent contract: an eligible text scan uses its index tokenizer and
configured version; ordinary record evaluation uses the primary tokenizer (or
fallback tokenizer, or default) at its maximum version. Without a primary
tokenizer name, any eligible index tokenizer can be selected, as in Java.
Applications requiring index-independent semantics should name the tokenizer
and use a current-version index, or use the SQL extension's resolved contract.
SQL string comparands, including prefixes, are tokenized; the Record Layer's
pre-tokenized list API is the way to request raw case-sensitive token spelling. NULL fields
or query values yield UNKNOWN; empty/all-stop-word queries select no records,
except that a prefix query that is not exactly one token (`''`) fails when
evaluated against a record, as Java's `TextComparison.eval` does, while a text
index scan returns no records for it.
Bound distance/tokenizer parameters are specialized at planning and pinned in
plan-cache constraints; changing them cannot reuse a differently specialized
predicate. A distance or tokenizer name that is not compile-time constant is
rejected explicitly rather than evaluated under a different predicate.

SQL sorting is a normal operator **above** the text access path; this is not a
claim that Java's classic Record Layer planner, which rejects text-index planning
when a sort is requested, has the same eligibility rule. The existing Unicode
and Java word-boundary compatibility boundary below still applies unchanged.

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
| **Continuations** | Leaf and structural cursor framing has Java conformance coverage, including narrowed-range common-prefix encoding. Record scans and index scans (ordinary VALUE/VERSION, COUNT/SUM, MIN/MAX_EVER_TUPLE, MAX_EVER_VERSION and BITMAP_VALUE) return Java's last-key token and stop reason at an exact returned-row-limit boundary, followed by an empty page. SQL statement continuations are engine-private; Go rejects caller-supplied `api.OptContinuation`. Go streaming-aggregate accumulator payloads and in-memory-sort state are not Java-compatible, even where a protobuf message name is shared. Do not exchange these tokens between engines or assume stability across plan changes. **Unreleased:** bounded TEXT suffix scans share continuation framing, not result/page parity: Go bounds decoded logical entries; Java bounds physical bunch signposts and can omit lower rows or leak upper rows. Keep these continuations within one engine. |
| **Store format** | An unpinned Go open can upgrade an older store to format 14 when its transaction commits. Java's default 7 is not its maximum: Java 4.14.2.0 can open 14 and 15. Older readers and downgrade paths must be checked separately. |
| **Version-dependent record keys** | Native `VersionKey()` primary-key columns are refused at metadata construction/load, as in Java. Maintained-count expressions and primary-key functions such as ordering wrappers see the chosen record version; incomplete values reaching ordinary key encoders are refused with `IncompleteVersionstampError`. VERSION indexes rooted at a function over a version are refused during metadata/DDL validation because functions declare zero version columns; equivalent VALUE indexes and KeyWithValue version-in-VALUE declarations remain legal metadata, with incomplete values refused at write time. Counts are checked only when maintained; dry runs prepare primary keys but do not maintain counts. Unsupported metadata and earlier null-version counters are not migrated automatically: migrate or recreate affected pre-release stores before mixed-engine use. See [RFC-275](../rfcs/275-recordlayer-review-lows.md). |
| **Brotli record compression (Unreleased)** | `TransformedRecordSerializerBuilder.SetGoOnlyBrotliCompression(level)` is opt-in for Go-only stores. It writes compression version 2, which Java 4.14.2.0 and default Go readers reject. The default serializer and compression version 1 remain Java-compatible zlib; Brotli-enabled readers also accept version 1 and bare records. Record keys, native index entries and custom index values are not changed by this setting. See [the compression boundary](../DIVERGENCES.md#brotli-record-compression-go-only-extension). |
| **Transaction size thresholds** | Go-only `ContextConfig` thresholds are opt-in: `TransactionSizeWarnBytes` logs at WARN and counts once per transaction without failing the operation; `TransactionSizeErrorBytes` returns `TransactionSizeExceededError`. Java has no matching user-set thresholds. Stored bytes are unchanged. See [RFC-275 threshold boundaries](../DIVERGENCES.md#transaction-size-thresholds-go-only-extension-rfc-275). |
| **Key-space paths** | **Unreleased flat-prefix cutover:** `Path.ToSubspace()` now emits Java-flat tuple keys, not the nested tuple prefix emitted by earlier Go builds. `LegacyNestedSubspace()` explicitly accesses old data; Record Layer opens on guarded flat path subspaces reject populated legacy prefixes unless `SetAllowLegacyKeySpaceCutover(true)` explicitly acknowledges cutover. No automatic migration exists; raw byte conversion discards this guard. See [upgrade instructions](upgrade.md#earlier-go-keyspace-path-prefixes). Paths retain stored values, not resolved logical names; `PathFromTuple` does not reverse-resolve names. Byte/integer sibling constants and resolver restrictions remain in [the KeySpace boundaries](../DIVERGENCES.md#key-space-paths-hold-the-stored-value-rfc-275). |
| **Legacy pending versions (Unreleased)** | With metadata versions disabled, Go deletes pending/local versions with the body; Java can retain a pending orphan. Previously committed ignored bytes remain preserved in this disabled case. With versions enabled at format 5, an incomplete save followed by point deletion removes Java's pending mutation but preserves exact previously committed V0 bytes; Go also clears that committed version key. See [the exact boundary](../DIVERGENCES.md#legacy-pending-record-version-deletion). |
| **Split-disabled replacement cleanup (Unreleased)** | With an unsplit suffix retained, no incomplete primary-key versionstamp, splitting disabled and a known previously split image without an inline version, Go clears the old chunk keys before writing suffix 0. Java 4.14.2.0 skips that clear and leaves suffixes 1..n alongside the new body. This intentional cleanup changes the persisted key set and can change subsequent reads; it does not change the split-record format or perform historical cleanup. See [the exact boundary](../DIVERGENCES.md#split-disabled-replacement-clears-known-old-split-tails). |
| **Opt-in batch conflicts (Unreleased)** | `CoalesceIndexWriteConflicts` adds whole maintained-index conflict ranges, so readers of untouched entries may conflict/retry where ordinary Java saves do not. Default conflicts and persisted bytes are unchanged. See [the boundary and regression](../DIVERGENCES.md#opt-in-batch-index-conflict-widening). |
| **Unordered TEXT identity (Unreleased)** | Go's probable-intersection seen set uses packed tuple identity. Equal byte-array keys can match where Java uses array identity; distinct Float/Double NaN payload keys remain distinct where Java's boxed-list equality suppresses a later key, including nested lists. These unpaged result-set exceptions are separate from scheduling and resumed Bloom false positives. See [unordered TEXT merges](../DIVERGENCES.md#unordered-text-merges-are-serial). |
| **Earlier Go SQL storage** | v0.2.0 changes the relational catalog/database/schema keyspace from v0.1.0's. There is no automatic migration from the old Go layout. A common FDB cluster does not make these layouts interchangeable. |

### TLS verification

The pure-Go client verifies a `:tls` cluster's servers as libfdb_c does: the CA in
`FDB_TLS_CA_FILE` is the only trust root, `FDB_TLS_VERIFY_PEERS` decides, and no host
name is checked. A `WithTLSConfig` config keeps Go's own verification, including a
host-name check against each dialed address. Pure Go reads no `TLS_*` network option,
refuses some encrypted key formats libfdb_c loads, and fails the open on a bad setting.
DIVERGENCES.md lists each difference.

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

Both clients resolve the skipped selector inside the range read (RFC-262 D10).
An overshooting Skip is therefore bounded by the range as in `libfdb_c`, and the
read-conflict range is the same. Pinned against libfdb_c by
`//pkg/fdbgo/client:selector_range_conformance_test` and
`TestDifferential_GetRangeSelectorRYW`. Ordinary, covering and fetch query index
plans pass Skip to the index cursor when binding a single physical range without a residual key filter, so
skipped entries are neither scan-limit charges nor record loads. Multi-range
`ScanRangeSet` and key-filtered plans, other index types, and the mapped reads
of remote-fetch plans keep one outer Skip, plus the outer returned-row limit.
See DIVERGENCES.md.

### Mutual indexing retries

Mutual indexing fragments end at primary keys, with split suffixes removed from
shard-map boundaries read at the transaction's read version. Boundary lookups are
snapshot reads. Claimed ranges and built-range entries retain Java's encoding.

Go does not implement Java 4.14.2.0's `anyJumper`: after a claimed chunk exhausts
runner retries, Go retains that exact claim and applies lessen-work backoff and
a smaller limit. Java instead records the error and revisits the fragment; an
unchanged range rethrows it, so a transaction-too-large (2101) can end Java's
mutual build without a smaller retry. Go also omits Java's repeated-range guard
that fails after more than 1000 repeats; it finishes each claim chunk by chunk or
returns the error when lessen-work retries are exhausted. These are build-control
differences, not changes to stored records or index/range-set bytes. See
[RFC-279](../rfcs/279-mutual-indexing-boundaries.md) and
[DIVERGENCES.md](../DIVERGENCES.md#mutual-indexing-has-no-anyjumper-rfc-279).

### Text queries

**Unreleased (post-v0.2.0).** This API/behavior is outside the frozen release cut.

`recordlayer.TextScan` executes Java's `TextScan.scan` for every TEXT comparison
type, and `PlanTextQuery` builds the plan `RecordQueryPlanner.planText` builds.
Their entries, records and continuations use Java's encodings (including the
Guava 33 Bloom filters in a `TEXT_CONTAINS_ALL_PREFIXES` continuation). On the
shared unbounded surface with equivalent comparison-key ordering, each engine
resumes the other's continuations, subject to the unordered identity exceptions
above. Go's ordered merges use packed tuple order; Java `KeyComparisons` uses
object comparison: signed UUID halves, UTF-16 strings and canonicalized NaNs,
and can throw for mixed Long/BigInteger elements. Their rows and order can differ
before pagination (UUID ALL/ANY contrasts are pinned without normalizing Java's
answers). Keep continuations over these typed keys within one engine; matching
framing alone does not promise matching remaining rows. Suffix-bounded TEXT scans are a Go
read-side extension: Go includes the lower predecessor bunch and filters decoded
logical entries at both bounds; Java scans physical bunch signposts, omitting
some lower rows and returning some upper rows outside the logical range. Their
bounded continuations are not a cross-engine remaining-row/page contract; use
same-engine resumes. The intersections of
`TEXT_CONTAINS_ALL`, `TEXT_CONTAINS_PHRASE` and `TEXT_CONTAINS_ALL_WITHIN` seek
instead of streaming each token: on the shared unbounded surface with equivalent
comparison ordering and without an out-of-band limit their pages are Java's; under a scanned-records, byte or time
limit Go reads less per row, so a
page can hold more rows than Java's, and every continuation still resumes at the
same next row. `TEXT_CONTAINS_ANY_PREFIX` and multi-token
`TEXT_CONTAINS_ALL_PREFIXES` return rows in the order Java's merge returns them
when every token read is already complete; Java's order over FDB is the order
reads complete. See DIVERGENCES.md, "TEXT queries".

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
- [CHANGELOG.md](../CHANGELOG.md), especially the v0.2.0 storage changes.

For SQL, [FEATURE_MATRIX.md](../FEATURE_MATRIX.md) inventories scenarios, not a
standards certification or proof for every combination of features.
[SQL_CONFORMANCE.md](../SQL_CONFORMANCE.md) is a feature matrix against the pinned Java version,
measured by `DocClaimsJavaProbe`; the cross-engine tests cover combinations it does not.
