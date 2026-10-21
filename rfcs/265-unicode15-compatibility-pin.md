# RFC-265: Compiler-independent Unicode 15 compatibility data

**Status:** Implemented; pin-only scope agreed with the owner.
**Base:** `2804215b86cbe3d06b8f865810d628c27a737d23`.
**Reference:** Java Record Layer / Relational 4.14.2.0, JDK 21 (Unicode 15.0).

## Contract and decision

Freeze today's Go behavior at Unicode **15.0.0**, independently of the compiler.
Go 1.26.10 before/after the pin and the pinned Go 1.27.3 build must produce
byte-identical TEXT tokens, positions and persisted keys. Do not redefine tokenizer
version 0, change its segmentation/sigma algorithm, or make existing Go-only
collation keys into Java keys. Java's `TextTokenizer.java:47-55` requires frozen
logic for previously indexed data; updates/deletes retokenize old records.

Use a project-owned, versioned `internal/unicode15` package with generated,
checked-in property and simple-case tables and a checked-in generator. Expose
only the required classifiers, simple rune/string lower/upper and simple-fold
comparison. Keep simple mapping, full string casing and exact identifier equality
as distinct policies: replacing these with `cases.Fold` is not equivalent.

Retain the current x/text normalization engine in a scoped internal snapshot,
regenerating only its Unicode-15 tables from pinned inputs and removing their
compiler-release selector. Retain the current collator and its runtime dependency
closure in the same versioned internal tree, including locale parsing/matching
that affects collation; redirect normalization, Ideographic and numeric Nd lookups
to frozen data. Preserve existing UCA 6.2 / CLDR 23 weights
and locale behavior, not newly generated Unicode-15 collation weights. Retain
upstream provenance and LICENSE files, plus an explicit inventory of local
snapshot changes. This avoids inventing a new normalizer
or collator while keeping regeneration explicit. A whole-module x/text fork would
also freeze unrelated dependencies; an ordinary module pin does **not** freeze
Unicode (`norm` and `cases` select 15 under `!go1.27`, 17 under `go1.27`).

The base uses x/text **v0.42.0**; the upgrade proposes v0.43.0. Their selected
`norm/tables15.0.0.go` and `collate/tables.go` are byte-identical, respectively
SHA-256 `9e29fbffef687d6d2191c999c466729dff2f394d8dcbec377a2fc14336f33cdb`
and `470786e0371903f7449b12e261dba458ed3e0c785c95fd3becd7c40864878469`.
Snapshot the base algorithms; unrelated dependency upgrades remain separate.

## Exact data and reproducibility

Inputs are the exact bytes at `https://www.unicode.org/Public/15.0.0/ucd/<file>`.
The hashes below were computed from downloaded files and independently matched
the corresponding members of the official
[`UCD.zip`](https://www.unicode.org/Public/zipped/15.0.0/UCD.zip), whose SHA-256 is
`5fbde400f3e687d25cc9b0a8d30d7619e76cb2f4c3e85ba9df8ec1312cb6718c`.

| File | SHA-256 |
|---|---|
| UnicodeData.txt | `806e9aed65037197f1ec85e12be6e8cd870fc5608b4de0fffd990f689f376a73` |
| PropList.txt | `e05c0a2811d113dae4abd832884199a3ea8d187ee1b872d8240a788a96540bfd` |
| DerivedCoreProperties.txt | `d367290bc0867e6b484c68370530bdd1a08b6b32404601b8c7accaf83e05628d` |
| CaseFolding.txt | `cdd49e55eae3bbf1f0a3f6580c974a0263cb86a6a08daa10fbf705b4808a56f7` |
| SpecialCasing.txt | `78b29c64b5840d25c11a9f31b665ee551b8a499eca6c70d770fcad7dd710f494` |
| CompositionExclusions.txt | `3b019c0a33c3140cbc920c078f4f9af2680ba4f71869c8d4de5190667c70b6a3` |
| DerivedNormalizationProps.txt | `d5687a48c95c7d6e1ec59cb29c0f2e8b052018eb069a4371b7368d0561e12a29` |
| NormalizationTest.txt | `fb9ac8cc154a80cad6caac9897af55a4e75176af6f4e2bb6edc2bf8b1d57f326` |

Runtime tables need UnicodeData general categories (L, Nd, Mn/Mc/Me, Lu/Ll/Lt),
simple lower/upper mappings, canonical combining classes and recursive canonical
and compatibility decompositions, including algorithmic Hangul. PropList supplies
Other_Lowercase, Other_Uppercase and Ideographic; include White_Space where existing
helpers are routed. CaseFolding C/S entries generate the existing Go simple-fold
orbits, **not** F/T full/Turkic folding. DerivedCoreProperties cross-checks the
Cased union. Composition exclusions and normalization quick-check/composition
properties support the frozen normalizer's composing forms used by collation;
NormalizationTest is the independent normalization corpus. SpecialCasing documents
full/conditional casing for differential fixtures and future Java adapters; it
must not silently replace today's simple-case policies.

The generator verifies the manifest before parsing, uses no host Unicode/casing
database, sorts output deterministically and emits a literal `15.0.0` version.
Inputs/licenses are checked-in Bazel data, so regeneration is offline and
compiler-independent. A regeneration check must leave a clean diff. Runtime
compatibility code must not reach unpinned Unicode-dependent imports or select
tables using Go build tags. Enforce this with a nogo/dependency regression guard
over the pinned dependency closure: reject stdlib `unicode`, `strings.ToLower`,
`strings.ToUpper`, `strings.EqualFold`, x/text `cases`/`norm`/`collate`, and
compiler-release table selectors (allow fixed encoding mechanics). Test each
forbidden arm. Future editions get new package/data versions;
Unicode-15 behavior remains callable for existing data.

## Scope, in priority order

1. **Persisted TEXT/query tokens:** `text_tokenizer.go:196,224,237,255,264-274`
   and every built-in tokenizer that delegates to it. Preserve the exact order:
   uniseg v0.4.7 segmentation → frozen NFKD → BMP letter/Nd admission → current
   contextual lowercase → all-plane M stripping. Normalize supplementary inputs
   before admission; filtering by age or BMP before NFKD is incorrect. Preserve
   the normalizer's current combining-run/stream-safe and malformed-UTF-8 behavior.
   `text_index_maintainer.go:268-277,349,372-393` must use the same fixed policy for
   insertion and old-record removal, with unchanged tokenizer version metadata.
2. **Other keyed paths:** Go-only collated primary/index/count keys in
   `collate_function_key_expression.go`, including locale numeric collation
   (`kn`), implicit Ideographic weights and normalization. Keep `GoOnlyCollationError`
   and the opt-in guard. Freeze `keystore_key_manager.go:foldAlias`'s existing
   simple lowercase policy for load/lookup; do not change alias equivalence.
3. **Identifier/name comparisons:** route appropriate simple lower/upper/fold
   uses in relational semantic identifiers/qualified names, embedded bindings and
   aliases, planner/executor/result-label lookup, fleet/catalog lookup and lexer
   lookahead. Preserve quoted/exact comparisons. The identifier grammar is ASCII
   **after** simple uppercase lookahead: U+0131 `ı` and U+017F `ſ` are admitted
   because they map to `I`/`S`; semantic normalization also maps them to ASCII.
   Pin `ıd`, `ſelect` and a quoted identifier (kept verbatim). Protobuf names
   reject non-ASCII; Unicode 15→17 has no new mappings into/out of ASCII, so this
   is not a claim that this bump already moves ordinary catalog tuple keys.
4. **SQL functions:** `query/plan/cascades/values/values.go:2891,2900` UPPER/LOWER
   keep their existing Go-only simple mapping (catalog says Go-only), including
   derived comparisons and DML values. Do not add full-string case expansions.
   Route existing BMP numeric/UUID classifiers to fixed Nd for future stability;
   this upgrade adds no BMP Nd or White_Space changes. Raw tuple strings, byte
   comparisons and UTF-8/UTF-16 mechanics are unchanged.

## Java fidelity and deliberately retained divergences

NFKD data, BMP admission, simple mappings and M categories stay at JDK 21's
Unicode edition. That does **not** establish complete JDK tokenizer parity.
JDK 21 `ConditionalSpecialCasing.isFinalCased` creates a fresh ROOT BreakIterator
on the **normalized** token and scans for prior/no later Cased points within its
boundaries. Its Cased predicate is Ll/Lu/Lt plus only `02B0..02B8`, `02C0..02C1`,
`02E0..02E4`, `0345`, `037A`, `1D2C..1D61`, `2160..217F`, `24B6..24E9`, not the
full UCD Cased union. Current Go instead uses first/last Cased over the whole
normalized outer segment. Neither algorithm changes in this pin.

Existing measured fixtures in `conformance/text_index_conformance_test.go` pin:

| Existing divergence | Input | Go v0 tokens | Java/JDK 21 tokens |
|---|---|---|---|
| uniseg versus BreakIterator segmentation | `x:y` | `["x:y"]` | `["x", "y"]` |
| NFKD changes sigma's internal word boundary | `ĿΣ` | `["l·ς"]` | `["l·σ"]` |
| NFKD creates an internal space before a later Cased point | `ΑΣ\u037A` | `["ασ "]` | `["ας "]` |
| JDK's narrower Cased predicate | `\U00010780Σ` | `["\U00010780ς"]` | `["\U00010780σ"]` |

Go-written v0 postings for these affected inputs **already differ from Java's**.
The pin preserves them, not fixes them. Full JDK 21 tokenizer parity requires an
owner migration/versioning decision, tracked in [TODO.md](../TODO.md#rfc-266-full-jdk-21-parity-and-text-index-migration).
Future JDK-faithful adapters must be separate from the frozen Go-v0 policy.

Two additional, Unicode-version-independent divergences stay pinned: keystore
aliases use Go simple lowercase (`İ` → `i`), whereas JDK PKCS12 uses full
`toLowerCase(Locale.ENGLISH)` (`İ` → `i\u0307`); column-label lookup uses Go
simple `EqualFold` (`İ` versus `i` is false), whereas Java `equalsIgnoreCase`
(upper- then lower-cased characters) considers that pair equal
(`RowStruct.java:330`, `RelationalStruct.java:159`). Add explicit independent
Go/Java expected fixtures rather than silently changing either lookup policy.

## Verification gates

- **Independent no-diff baseline:** before replacing callers, retain a test-only
  exporter and immutable fixed expected fixtures from base `2804215b8` on Go
  **1.26.10**, captured **before replacing any caller**.
  Cover all code points for classification/simple mappings/fold orbits, all scalar
  normalizations, the UCD normalization corpus, checked-in tokenizer/SQL/alias
  inputs and a deterministic adversarial multi-codepoint corpus. Compare actual
  bytes, not two helpers sharing the new tables. Record corpus identities and
  nonempty counts; do not claim exhaustive equality over arbitrary strings from
  a finite corpus. Keep stream-safe combining runs, invalid UTF-8, Hangul,
  supplementary inputs, dotted I, sigma, ligatures and stable ASCII controls.
- **Version-delta fixtures:** U+1C89/A7CB (16), A7CE/A7F1 (17), outlined
  U+1CCD6→A (16), U+1ACF/0897 marks, existing U+0264 uppercase and `AΣʕ`
  (U+0295 loses Cased in 17). Add old-only fold pairs FB05/FB06, 0390/1FD3,
  03B0/1FE3; ideographs 18CFF/323B0/2EBF0; numeric-locale supplementary Nd.
  Assert helper behavior separately from segmentation-sensitive whole terms.
- **Java fixtures:** existing `tokenizeDefault` conformance step already emits
  token lists without FDB. Capture and check in JDK-21/4.14.2.0-produced expected
  tokens for the version-delta corpus too (including 1C89, A7CB, 1CCD6, 1ACF,
  FB05 and 18CFF); derive and assert position maps. Shared cases must agree;
  the explicit
  existing-divergence fixtures must keep their distinct Java and Go expectations.
  Record Java runtime/build and artifact identity. No source prediction is a
  claimed executed Java result.
- **Persisted bytes:** real-FDB tests compare raw sorted TEXT subspace key/value
  bytes and positions after write, update, delete and rebuild, with grouped and
  omitted-position variants. Separate compiler-built writer/updater/deleter
  processes use the same store in both 1.26→1.27 and 1.27→1.26 directions;
  also update/delete a pre-pin 1.26-written fixture. Assert obsolete terms absent,
  final emptiness and Java equality on shared inputs. Compare exact opted-in
  collation PK/index/count bytes, range lookup/order at all strengths and numeric
  locales against the fixed Go baseline, **not** Java collation keys.
- **Matrix:** after implementation, serialize `just test` and `just test-full`
  through the shared-machine lane wrapper under **Go 1.26.10 and Go 1.27.3** in
  isolated worktrees. Verify the Bazel-selected SDK, fixture inputs and unchanged
  tree; record both compiler identities and executed/cached counts in receipts.
  No `go test`. During implementation the owner permits focused Bazel targets
  with nice 19, the reserved CPU mask and `--batch`; official lanes remain with
  the owner, and hook commits use the shared fast.lock. Generator drift and both-compiler fixture equality are
  release gates, not a promise inferred from either compiler passing alone.

`docs/compatibility.md` states the fixed Unicode-15 contract, retaining the
pre-existing segmentation/sigma and Go-only collation boundaries above.

## Sources

- [JDK 21 Character: Unicode 15.0](https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/lang/Character.html).
- [JDK 21+35 ConditionalSpecialCasing](https://github.com/openjdk/jdk/blob/jdk-21%2B35/src/java.base/share/classes/java/lang/ConditionalSpecialCasing.java).
- [Java 4.14.2.0 DefaultTextTokenizer](https://github.com/FoundationDB/fdb-record-layer/blob/4.14.2.0/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/common/text/DefaultTextTokenizer.java), especially 111-135,175-177.
- Local validation evidence (not published): source audit `${FDB_PERF_HOME}/go127-upgrade/unicode-audit-go.md`,
  `unicode-audit-data/source-deltas.md`, and `research/java-unicode15-source-audit.md`.
