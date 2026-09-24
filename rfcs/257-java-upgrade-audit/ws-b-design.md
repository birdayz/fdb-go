# RFC-257 WS-B — storage correctness and metadata lifecycle

Status: detailed design submitted for review; implementation not started.
Parent: [RFC-257](../257-java-4.14.2.0-upgrade.md), WS-B.
Java specification: 4.14.2.0 `fdacd162a9c8acfadc49082b89185c823ab8ae4a`.
Go published HEAD: `71ccd8cf8b3fd0dbafe283e91171818e36af555e`.
Starting verified implementation tree: `0dedbe1b1083358fb9568a31178084c043fa31e0`.

This is one implementation milestone, with one final joint review and independent
compatibility review. The sections below are execution order, not opportunities
to declare the workstream complete independently. Research evidence and exact
source mappings are in `ws-b-research/`. Findings are source-derived until their
retained tests execute. No commit, push, merge or PR-state change is authorized.

## 1. Sliding-window insertion and delegate routing

Read the complete tagged SlidingWindowIndexMaintainer and current Go maintainer,
plus the tagged replay/window-metric regressions. Port the entry-key-first
algorithm, not the upstream commit description's claim about rereading records.

- Represent an entry by partition, window value and full primary key; the stored
  key/value and count/boundary tuple encodings stay unchanged.
- `handleInsert` reads the exact tracked-entry key serializably BEFORE any write,
  counter update or boundary movement. Missing entries follow existing insertion.
  Present entries increment `SW_REINSERT_ALREADY_TRACKED`, require a boundary,
  refresh the delegate iff inside the window, and otherwise return unchanged.
- Replace preemptive deletion entirely. Ordinary and write-only updates use the
  same locked delete-then-insert bookkeeping, with delegate callbacks selecting
  `Update` versus `UpdateWhileWriteOnly`. Eviction and overflow promotion still
  use ordinary delegate updates on the loaded records, as Java does.
- Evaluate record filtering at update entry, including conjunction predicates.
  Separate entry keys and callbacks so WS-C queue replay can reuse the mechanism
  without reevaluating predicates. No queue capability, state4 or format15 is
  enabled in WS-B; their complete protocol remains WS-C's explicit scope.
- Replace the old preemptive-delete metric and stale explanations. Retain the
  documented size-one overflow-promotion correction; do not reintroduce Java's
  stranded-overflow defect while porting the insertion fix.

Real-FDB regression matrix: ordinary replay after a user write and write-only
replay after a builder pass; both directions, partial/full windows, with/without
overflow, boundary/interior/overflow entries, partitions, ties, size one, changed
vector payload, changed ordering/partition, filters, and missing boundary.
Assert exact entries, count, boundary, delegate results and operation counters.
A replay must cause exactly one delegate insertion only for an in-window entry;
no delete/eviction/promotion or count movement. Retain the existing valid
write-only result test and broaden its explanation rather than replacing it.

## 2. Header-aware deletion and cache prerequisites

Keep the synchronous `DeleteStore(context, subspace) error` API. Read only the
store header with a serializable get. Missing/valid-noncacheable headers do not
bump the global metadata stamp; cacheable or malformed protobuf headers do.
Empty protobuf bytes are valid, noncacheable headers. Valid unknown fields and
unsupported format numbers do not prevent deletion. Read failures propagate
before scheduling any mutation. Always mark store state dirty and clear the
Java tuple-subspace range, not the broader byte-prefix range.

Add a context-aware range clear that also removes queued versionstamp mutations
and local versions in that range before commit can flush them back. Reuse it for
store deletion and index-data cleanup where those pending writes are relevant.
Do not change the FDB client or generic transaction ClearRange semantics.

Fix cache prerequisites together: admit only cacheable headers, invalidate older
cached entries when a stamp-mismatch reload is absent/noncacheable, preserve
newer-entry ordering and dirty-context bypass. Clone the proposed header before
cacheability changes so stamp invalidation observes the REAL old cacheability.
Propagate stamp-read errors. Preserve the existing header-key conflict on cached
opens. No blanket serializable global-stamp read or whole-store read conflict.

Real-FDB tests cover missing/residual/malformed/empty/unknown-field headers,
cacheable and noncacheable stores, cold and separately warmed caches, both commit
orders against header updates/create/cacheability toggles and record writes,
delete/recreate in same and separate contexts, and versioned save then delete.
Use explicit pinned read versions and sentinel writes to make conflict checking
observable; assert the first commit outcome rather than hiding it behind retry.
Use isolated fixtures where global stamp equality is asserted. Test exact range
boundaries and no resurrection of deferred version keys.

## 3. Replacement initialization, retirement and commit lifecycle

Separate metadata enumeration from build eligibility. Add `GetIndexesSince`
with the current unfiltered changed-index enumeration, use it for reconciliation,
and make `GetIndexesToBuildSince` exclude replaced originals as Java does.
Expose eligible non-readable indexes through the store's build list. Build-all
excludes originals, while explicitly requested rebuilds remain supported.

For every new/changed candidate declaring replacements select DISABLED before
calling rebuild policy. Apply this to fresh Create and fresh CreateOrOpen as
well as metadata-version reconciliation. Ordinary new-store candidates preserve
Java's empty-store shortcut; explicit DISABLED remains disabled. Adding only a
replacement option to an established original without a modified-version change
does not prematurely disable it.

Retirement requires ALL replacements to exist and be exactly READABLE, never
READABLE_UNIQUE_PENDING. Enumerate eligible originals under the state read lock,
release it, then disable through the ordinary atomic state-transition path.
Check transaction-visible state and add serializable index-state conflicts for
state-dependent maintenance, scans and retirement. Public getters retain their
API; error-capable paths use an error-returning state-read helper. Include
negative decisions to skip disabled indexes in conflict coverage.

Run retirement after changed metadata reconciliation, and schedule it after a
changed mark-readable or mark-readable-or-unique-pending operation. Add named
context commit checks, deduplicated by store subspace, not a store-object flag.
Run at precommit against current transaction state; errors abort the commit.
Plain Commit must execute the same checks, version flush, commit, postcommit
sequence as hooked commits exactly once per attempt. Preserve online build lock
state while clearing retired-index data/ranges/other build state as Java does.

Compose deletion with registered retirement checks explicitly. Source inspection
shows Java's captured retirement callback may write disabled-state keys after a
same-transaction store clear. Before implementation of this composition, retain
a live Java reproducer (mark readable, delete, commit, inspect raw store range).
The required Go invariant is no deleted-store resurrection: context-aware delete
removes the subspace's pending retirement check; recreation schedules checks for
its own metadata when needed. If the tagged Java probe confirms the defect,
record this as a deliberate boundary correction with a regression and upstream
report text, not silently claim exact parity. No external issue publication is
authorized by this RFC.

Real-FDB tests: all policy choices on fresh/changed originals, policies not called
for originals, partial replacement readiness, pending uniqueness, final inline
and online completion without another metadata version, explicit original
rebuild while replacements unready, cancellation of replacement relationships,
overlapping replacement builders and stale original writers in BOTH commit
orders, multiple store objects/subspaces, named-check deduplication, hook errors,
retry and all public context commit APIs. Deletion/recreation checks must prove
no captured callback restores deleted keys or applies old metadata to a new store.

## 4. Ignored options and identity-preserving metadata evolution

Use a private, copied, default-empty set. Add Set/GetIgnoredIndexOptions and
AsBuilder; setter, Build, copying and getter results must not share mutable
storage. Exact case-sensitive option names; duplicates collapse; nil clears;
return sorted copied slices. Preserve all flags in AsBuilder.

Compute additions/removals/changes once, subtract configured options, then pass
a fresh mutable remainder to type-specific and base option validation. Expose
`ValidateChangedIndexOptions(oldIndex, newIndex, changedOptions)` for the supplied
mutable set; never recompute it. Explicitly ignored structural option names ARE
allowed by Java (including unique); the parent RFC's structural-validation rule
means no global relaxation, not an invented unignorable-options list. Unrelated
option/type/expression/version/record-scope/primary-key checks stay strict.

Derive record correspondence from old/new UNION FIELD NUMBERS, not surviving
names or explicit type keys. Use GetUnionDescriptor, require retained message
fields, enforce bijection (no split/merge), validate recursive descriptor PAIRS,
and only then check record-type keys and rename policy. Retain complete mappings
for unchanged names for Go consumers. Replace name-based final descriptor
validation. Unionless Go schemas use a separate typed-key-first correspondence;
mixed union presence is rejected. Preserve string/bytes key distinction.

Fix union member resolution to use the actual field.Message descriptor. Separate
smallest alias tag for default type key, Java's canonical-name/otherwise-highest
serialization tag, and ALL accepted deserialization tags. Generated message
factories are reusable only for the current matching descriptor, otherwise use
dynamicpb. This is essential for persisted same-name schema revisions.

Keep the correct old-descriptor index-expression rewrite loop. Validate index
record scope before rewritten expression, and require newly attached types on an
existing index to have sinceVersion greater than oldVersion (the general
allowNoSinceVersion flag does not waive this narrower Java requirement).

Port Java rename/swap/name-reuse descriptor fixtures, including custom union
names, alias tags, explicit keys, recursive pairs, index association and field
renames, incompatible swaps, split/merge/removal, type-key changes and conflicting
multi-type rewrites. Test configuration copies and supplied-option-set behavior.
Real-FDB old-write/evolve/reopen tests pin metadata history, record identities,
fields, PK/index associations and failed-evolution atomicity. Cross-language
fixtures must prove both directions and preserve nested enum descriptors; the
separate public proto-editor API gap is not falsely claimed implemented here.

## 5. Rank-valued scan bounds

Add RankScanBounds{ScanType, RankRange, IncludeRankAsValue}, a validating
constructor, and ScanRankIndex following the existing specialized scan APIs.
Validate struct literals at dispatch too: only BY_VALUE/BY_RANK, RANK maintainer,
normal readable-state checks. Existing APIs and false flag preserve empty entry
values and avoid new per-entry lookups.

True maps entries fallibly, preserving index/key, order, continuation, terminal
reasons, cancellation/errors and close. Rank lookup uses only declared grouped
score columns: group=key[:groupingCount], score=key[groupingCount:rootColumnCount].
Validate length; never include appended PK or subtract an assumed PK width.
Keep Java's null-if-missing behavior: tuple(int64(rank)) or tuple(nil), never a
pointer or empty tuple. Rank is absolute within group, including reverse/bounded
scans and tied scores. Keep actual rank lookup serializable; do not accidentally
turn it snapshot because the underlying scan is snapshot. Synchronous mapping
uses existing cursor infrastructure; instrument the resulting cursor once.

No persisted rank bytes change. Real-FDB cases cover both bounds types, groups,
composite scores, overlapping/appended PK, ties and duplicate-counting modes,
empty/missing scores, short malformed entries, forward/reverse, page/row/byte
limits, cancellation/lookup errors, commit and cold reopen. Extend both Java and
Go rank conformance adapters with exact Value comparison and lossless tuple-byte
output: existing key-only equality is insufficient. Assert raw stored values
remain empty and all rank/index bytes unchanged by enriched reads.

## Verification and completion

Every found defect receives a retained reproducer before its fix, actual Bazel
execution and a targeted mutation kill. New test files require gazelle enrollment.
Run affected complete real-FDB targets, race targets, focused live Java probes,
full uncached suite and just test with frozen hash/population reconciliation.
Use both-direction wire interop for metadata/rank and existing HNSW persistence
coverage for sliding windows. No mocks, no new skips, no broad golden refresh,
no restricted hunts, no arbitrary corpus entries, no weakened assertions.

Final milestone review: virtual Graefe/Torvalds plus independent storage/Java
compatibility review using gpt-6-astra/xhigh/read-only; fix findings then obtain
exact-final-code-tree delta ACKs. WS-B is complete only when all sections above
are exercised and verified. WS-C–K remain separate unfinished migration work,
and local success cannot be represented as published PR CI success.
