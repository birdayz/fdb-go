# RFC-220 — Coveringness is a plan type built at the access path, not a flag stamped downstream

Status: proposed (implementation gated on Graefe + Torvalds ACK)
Java reference: fdb-record-layer 4.12.11.0 (pinned `MODULE.bazel:117`)

## 1. The defect

MEASURED at `4465dcc6d` (`PlanQueryForTest`, schema `t2(id BIGINT, status STRING, PK(id))` +
`INDEX idx_status ON t2(status)`):

```
SELECT id FROM t2 WHERE status > 'act'                        => Project([ID#0], IndexScan(IDX_STATUS, [<>] COVERING))
SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%' => Project([ID#0], PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds]))
```

Same index, same projected columns, same covering entry. The second lost `COVERING`
because `PushFilterThroughFetchRule` put a `RecordQueryPredicatesFilterPlan` between the
fetch and the scan, and the rules that stamp coveringness do not descend through it.

Pinned on master by `pkg/relational/core/embedded/like_prefix_not_sargable_test.go`
(`TestLikePrefix_IsNotSargable_AndTheCoveringStampIsLost`, PART 2), landed by RFC-217/216 in
#658 as a NEGATIVE result — it asserts the defect, and its failure message says the
expectation is what to update when the stamp is restored. This RFC is what updates it.

### 1.1 The cost consequence, now MEASURED

#658's test comment said the cost effect "was NOT asserted here and was never measured".
Measured now, by a direct probe on the cost model's predicate:

```
PROBE stamp-lost  isSingularIndexScanWithFetch=true
PROBE stamp-kept  isSingularIndexScanWithFetch=false
```

with `expressionCounts{indexScanCount:1, coveringIndexCount:0, fetchCount:0}` and
`{indexScanCount:0, coveringIndexCount:1, fetchCount:0}`. `planning_cost_model.go:1393`
returns `true` on `indexScanCount == 1` **before** consulting `fetchCount`, so an unstamped
covering scan is classified "singular index scan **with fetch**" at `fetchCount == 0` — a
self-contradictory classification — and is routed to the contested tier at `:1239` instead of
the unencumbered default at `:1247`.

NOT measured, and not claimed: that this flips any specific plan against a primary scan. The
misclassification is proven; a resulting plan flip is not. §8 is where any flip shows up, and
each moved golden is justified individually rather than cited as evidence for this paragraph.

## 2. Root cause: Go decides coveringness in the wrong place

### 2.1 What Java does

`ValueIndexScanMatchCandidate.toEquivalentPlan` (`ValueIndexScanMatchCandidate.java:229-248`)
delegates to `tryFetchCoveringIndexScan` (`:250-282`), which — whenever the index entry can
be turned into a partial record — **unconditionally** builds

```
RecordQueryFetchFromPartialRecordPlan( RecordQueryCoveringIndexPlan( RecordQueryIndexPlan ) )
```

The covering plan is a distinct class **holding** the index plan as a field
(`RecordQueryCoveringIndexPlan.java:74-78`). Critically it
`implements RecordQueryPlanWithNoChildren`, and `tryFetchCoveringIndexScan` memoizes only the
covering plan (`:280`) — the inner index plan is a plain field, never a quantifier, never its
own memo group.

`MergeProjectionAndFetchRule.onMatch` (`MergeProjectionAndFetchRule.java:62-78`) then does
exactly one thing: if every projected value pushes through the fetch,
`call.yieldPlan(fetchPlan.getChild())`. **No shape check, no type assertion, no stamping.**
It removes the fetch; it never decides coveringness.

So when `PushFilterThroughFetchRule` inserts a filter, the child is
`PredicatesFilter(CoveringIndexPlan)` and `getChild()` returns it intact. Coveringness cannot
be lost because nothing downstream ever had to recognise it.

### 2.2 What Go does

Go inverted this. `wrapScanPlanWithCoverage` (`abstract_data_access_rule.go:639-663`) emits a
bare `Fetch(IndexScan)` and deliberately declines to set coveringness (`_ = coveringColumns`
at `:644`), documenting:

> "covering is decided downstream by MergeProjectionAndFetchRule, which compares the actual
> projection's columns against the index's covered columns … That is strictly more precise
> than the coarse isCovering signal here (`!comp.IsFinalNeeded()`), which does not see the
> projection"

That rationale compares against the wrong Java operation. It conflates two decisions Java
keeps separate:

- **constructing** the covering plan — Java: access path, unconditional, needs no knowledge
  of the projection;
- **deciding the projection is covered**, i.e. removing the fetch — Java: downstream, in the
  merge rule.

Go does both downstream, so coveringness must be *recognised* by a rule inspecting a subtree,
and any operator pushed between the fetch and the scan defeats the recognition. The set of
such operators is not enumerable, which is why patching recognisers cannot close this.

### 2.3 The census — six sites, not two

`grep -rn "WithCovering" pkg/ | grep -v _test`, repo-wide, excluding the definition at
`index_scan.go:383`:

| # | Site | Role |
|---|------|------|
| 1 | `abstract_data_access_rule.go:665` | access path, **bare-index branch only** — never the `Fetch(IndexScan)` branch |
| 2 | `rule_push_map_through_fetch.go:84` | downstream stamper |
| 3 | `rule_merge_projection_and_fetch.go:94` | downstream stamper |
| 4 | `rule_streaming_agg_from_index.go:134` | downstream stamper |
| 5 | `rule_implement_streaming_agg.go:113` | downstream stamper, `WithCovering(nil)` — the empty-column case |
| 6 | `rule_implement_projection.go:76` | downstream stamper |

#658's PART 3 disabling experiment measured **two** stampers, correctly, **for one query
shape**. That was then promoted to a repo-wide claim of "two rules". The repo-wide number is
six. Five of the six are downstream recognisers that exist only because construction was
deferred.

## 3. Why the boolean cannot be repaired — two independent proofs

Go collapsed Java's two classes into a `covering bool` on `RecordQueryIndexPlan`
(`index_scan.go:42-43, 375-389`). The shape `Fetch(Covering(Index))` — routine in Java,
emitted by every value-index access path — is **unrepresentable** in that encoding:

**(a) The flag changes what the operator emits.** `executor.go:421` branches on
`IsCovering()` to reconstruct a partial record from the index entry instead of fetching full
records by primary key. It is an execution mode, not a rendering hint.

**(b) Go's fetch is a pass-through.** `executeFetchFromPartialRecord`
(`executor.go:1861-1874`) delegates straight to the inner: "In Go, the index scan executor
already returns full records, so the fetch is a pass-through". So
`Fetch(Index[covering=true])` would emit *partial* records to the fetch's consumer — silent
wrong rows, not a different plan.

Together the boolean forces "covering" and "no fetch above me" to be the same bit. Java keeps
them separate, which is exactly what makes an intervening operator harmless.

## 4. Decision

**Reintroduce `plans.RecordQueryCoveringIndexPlan` as a distinct plan type holding a
`*RecordQueryIndexPlan` as a FIELD, construct it at the access path as Java does, delete the
`covering` bool, and reduce the downstream stampers to yielding the fetch's child.**

1. **New type** — holds the inner `*RecordQueryIndexPlan` plus the entry→partial-record
   mapping today carried by `coveringColumns` / `AllCoveredEntryColumns` (`index_scan.go:272-284`,
   entry-layout order `columnNames ++ valueColumnNames`). Must support the empty-column case
   (§2.3 site 5). Mirrors `RecordQueryCoveringIndexPlan.java`, delegating `IsReverse`, index
   name, match candidate and max-cardinality to the inner (`:141-190`).

   **Criterion C1 — the inner index plan is a plain field:** never memoized, never a child
   quantifier, invisible to child traversal and to `expressionCounts`. Java's type
   `implements RecordQueryPlanWithNoChildren` and memoizes only the covering plan. If Go
   exposes the inner as a child, two things break: other rules match the bare index plan and
   yield a group member whose result value differs from `Covering(Index)` (unsound group),
   and the census counts it so `indexScanCount` returns to 1 and the §1.1 misclassification
   is re-armed. A test drives this directly.

   **Criterion C2 — because C1 makes the inner a field, nothing in child traversal folds it,
   so every identity surface on the new type must fold the inner's FULL identity itself:**
   index name, scan comparisons / ranges, and reverse — not merely the covering columns.
   This binds both `structuralKey` (§6) and the fingerprint salt (§7). Getting C1 right and
   C2 wrong trades one memo collision for a strictly worse one: two covering scans over the
   same index with *different scan ranges* would collapse into a single group and one of
   them would execute the other's ranges. C1 is what creates this obligation, which is why
   it is stated rather than left implicit in a field list.

2. **Access path** — `wrapScanPlanWithCoverage`'s `Fetch(IndexScan)` branch builds
   `Fetch(Covering(IndexScan))` unconditionally whenever the entry→partial-record mapping
   exists, matching `tryFetchCoveringIndexScan`. The `isCovering` / `!comp.IsFinalNeeded()`
   signal (`abstract_data_access_rule.go:453`) is **deleted**, not refined — Java never
   consults such a signal.

3. **Fetch executor** — stops being a pass-through and performs the fetch. Java maps
   `QueryResult::getIndexEntry` (`RecordQueryFetchFromPartialRecordPlan.java:128-132`); Go's
   `QueryResult` already carries `PrimaryKey` (`query_result.go:37`), and
   `coveringIndexCursor` already populates it (`executor.go:1389`). A new cursor is required:
   the existing `indexFetchCursor` consumes `RecordCursor[*recordlayer.IndexEntry]`, not
   `RecordCursor[QueryResult]`. It must keep `indexFetchCursor`'s loud orphan behaviour
   (`RecordCoreStorageError`, `executor.go:1311-1327`, Java `IndexOrphanBehavior.ERROR`).

   Note `coveringLogicalOrdinals` (`executor.go:1399-1412`) is all-or-nothing and today
   silently degrades a covering scan to the fetch path when a covered column has no top-level
   logical slot (nested column, multi-type scan). Under this design that degradation becomes
   the fetch doing its job, which is the correct structure rather than a fallback.

4. **Downstream stampers** — sites 2-6 lose their `WithCovering` calls and yield the fetch's
   child. `WithCovering` and the `covering` field are deleted.

5. **Cost model** — `coveringIndexCount` keys on the type at **both** census copies
   (`planning_cost_model.go:628` and `:2348`). `isSingularIndexScanWithFetch` (`:1389-1397`)
   is left alone: its `indexScanCount==1` early return is a faithful port of Java's identical
   short-circuit and is not a bug in Java, where a bare `RecordQueryPlanWithIndex` always
   denotes a fetching scan. It is self-contradictory only because Go's flag lets a fetchless
   covering scan wear that class. The type removes that possibility; the ordering is not
   "repaired".

### 4.1 The projection is retained — a deliberate, permanent divergence

Go's `MergeProjectionAndFetchRule` retains the projection where Java drops it
(`rule_merge_projection_and_fetch.go:103-115`). An earlier draft of this RFC called that a
fallback pending a port of "Java's result-value rewrite". **That was wrong about Java and is
corrected here.**

Java's `RecordQueryCoveringIndexPlan.getResultValue()` returns
`IndexedValue(indexPlan.getResultType().getInnerType())` — the base record type — carrying a
standing TODO admitting it is not the projected/flattened shape. `pushValue` only translates
the projection's Values for the *feasibility test*; Java then drops both and tolerates a
wider output type. **There is no result-value rewrite in Java to port.**

So Go retaining the projection over the covering scan is *more* correct than Java, and it is
recorded as an intentional permanent divergence citing that Java TODO — not as a deferral,
not effort-gated. §4.4 is worded accordingly: the stampers yield the fetch's child, with the
projection retained above it. That is not Java's literal `yieldPlan(fetchPlan.getChild())`,
and the difference is deliberate. This will be noted in `DIVERGENCES.md`.

## 5. Alternatives rejected

**(A) Teach the stampers to descend through an intervening residual, keep the boolean.**
Patches recognisers against *one* operator type; the next operator pushed below a fetch
re-opens the identical hole, and that set is not enumerable. Preserves the six-way redundancy
that made the root cause hard to attribute (disabling any one stamper leaves the plan
byte-identical). Leaves `Fetch(Covering(Index))` unrepresentable (§3). It is smaller, which
per CLAUDE.md is not a selection criterion.

**(B) Reintroduce the type but keep construction in the merge rule.** The framing the task
brief offered. Fixes the reported symptom but keeps coveringness a downstream *recognition*
problem — the merge rule must still locate the index plan under an arbitrary subtree. Also
diverges from Java for no reason, since Java's construction site needs strictly less
information (it never looks at the projection). Rejected because it leaves an architecture a
later reader cannot derive from the Java source.

## 6. Identity, hashing, and the memo

Three identity surfaces key on `covering` today. All three are addressed explicitly.

**`structuralKey`** (`index_scan.go:397-426`) folds `Bool(p.covering)` at `:422` — but not
`coveringColumns`, so two plans differing only in *which* columns are covered currently
collapse into one memo Reference. That is an **unsound group collapse, not a cosmetic gap**:
two covering scans over different covered-column sets emit *different partial records*, so
merging them lets one execute as the other. Including the covering columns in the new type's
`structuralKey` is therefore **required**, and is a correctness fix in its own right rather
than a nicety adopted in passing.

Per criterion C2 (§4.1) the new key must additionally fold the **inner index plan's full
identity** — index name, scan comparisons/ranges, reverse — because C1 makes the inner a
field that child traversal does not reach. Pinned by two tests: covering plans differing only
in covered columns do not dedup, and covering plans over the same index differing only in
scan range do not dedup.

**`PlanHash`** (`plan_hash.go:19`) folds `HashCodeWithoutChildren` → `structuralKey`, so it
moves. `plan_hash.go:14-18` states it is in-memory only — the plan cache is keyed on
normalized SQL text and no continuation embeds it — so this is compat-free. The RFC records
that reasoning rather than relying on it silently.

**`stablePlanNodeHash`** (`planning_cost_model.go:3060-3064`), the criterion-#17 tiebreak,
does **not** currently include `covering`. A new node type changes node heads there and can
flip a cost tie with no golden moving — i.e. invisibly. The new type therefore gets an
explicit node-head entry, and a test pins the head string so a silent tiebreak drift becomes
a failing assertion rather than a mystery plan change.

## 7. Continuation compatibility — the wire line

`indexScanRangeFingerprintSalt` (`scan_range_execution_identity.go:363-386`) folds
`boolField("covering", plan.IsCovering())` at `:375` and
`stringsField("covering-columns", plan.GetCoveringColumns())` at `:376` into a SHA-256 digest.
That digest reaches the user-visible continuation: `executor.go:388` →
`boundScanRangeSet.fingerprint()` (`scan_range_binding.go:1426-1429`) →
`gen.ScanRangeSetContinuation.Fingerprint` (`scan_range_set_cursor.go:421-441`), and on
resume `scan_range_set_cursor.go:145` rejects a mismatch.

This RFC did not mention it in its first draft. It is the CLAUDE.md hard line and it is
resolved as follows, in two cases:

- **Plan unchanged by this RFC** → the fingerprint **must be byte-identical**. The new type's
  salt reproduces the existing field order and builder prefix exactly (`index`, then
  `index-name`, `scan-type`, `reverse`, `covering`, `covering-columns`,
  `primary-key-columns`, `record-types`, `physical-key-types`, `flowed-type`). A golden test
  over the digest bytes pins this; it is the one assertion that must not be re-blessed.
  Criterion C2 applies here too: the salt must fold the inner plan's index name, scan
  comparisons and reverse flag directly, since the covering wrapper's own fields no longer
  reach them through a child. A salt that folded only the covering columns would give two
  covering scans over different ranges the SAME fingerprint, and a continuation from one
  would be silently ACCEPTED by the other — the one failure mode in this design that is not
  loud.
- **Plan changed by this RFC** (a scan that gains coveringness) → the fingerprint necessarily
  changes and an in-flight token is **rejected loudly** at `scan_range_set_cursor.go:145`.
  That is the designed behaviour for a plan change across a binary upgrade, and it is a loud
  failure rather than silent corruption. It is called out here so the behaviour is chosen,
  not discovered.

No plan proto serialization exists in Go — `pkg/recordlayer/query/plan/plans/` has no
`ToProto`/`FromProto`, and `PRecordQueryCoveringIndexPlan` (`proto/apple/record_query_plan.proto:1775-1777`,
`gen/record_query_plan.pb.go:11297`) is generated but unreferenced outside `gen/`. So the
record/index/continuation formats are otherwise untouched. Verified by grep, reported as
such.

## 8. Blast radius

This is a physical-plan change and plans will move; that is the point. §4.2 makes the access
path emit `Fetch(Covering(Index))` on **every value-index match**, so the radius is *not* the
defect shape. Re-derived from the proposed change:

**Re-derived at `1d78ddb08` (post-#679), not carried over.** #679 landed 497 plan assertions
across 37 yamsql files, which grew the yamsql `COVERING` surface from 8 references in 4 files
to **22 in 8** — so the goldens are no longer the only thing that moves, and the earlier
table understated it.

| Count | Golden / fixture |
|---|---|
| 68 | `pkg/relational/conformance/explaindiff/testdata/plan_shape.golden` |
| 8 | `yamsql/testdata/order_by_elimination.yaml` (new in #679) |
| 4 | `yamsql/testdata/rfc202_generated_index_plans.yaml` |
| 4 | `yamsql/testdata/index_scan_direction.yaml` (new in #679) |
| 2 | `yamsql/testdata/covering_index_pushdown.yaml` |
| 2 | `pkg/simfdb/hunt/golden/testdata/orders.golden` |
| 1 each | `cascades_plan_shapes.yaml`, `index_scan_order.yaml` (new), `index_range_predicates_java.yaml` (new), `like_patterns_java.yaml` |

`pkg/docscheck/yamsql_plan_claim_gate_test.go` (also new in #679) fails when a yamsql header
claims a plan shape the file does not assert. Any header this change invalidates will be
reported by that gate rather than found by inspection — it is treated as a required signal,
not noise to re-bless.

Plus ~15 Go test files carrying hard `" COVERING"` Explain strings, `rowdiff/run.go:495-499`'s
`"Covering"` vs `"IndexScan"` family classification, and **69 non-test references to
`*plans.RecordQueryIndexPlan` across 28 files** — each a type switch or assertion the new
sibling type must be added to. An earlier draft cited "44 hits, 7 files" from a grep scoped
to `PredicatesFilter(IndexScan(`; that was the defect shape, and it undercounted the change
actually proposed.

Every moved golden is justified individually. A bulk re-bless is not acceptable and none is
planned. Where a golden moves only by gaining `COVERING` on a scan that was already covering
in substance, that is stated per line rather than in aggregate.

Java renders a different explain shape for its covering plan
(`COVERING(IDX [EQUALS …] -> …)`, pinned against Go at
`row_version_index_plan_fdb_test.go:113-148`). Converging Go's rendering on Java's is
**explicitly out of scope** here — it would churn all 68 golden lines for a reason unrelated
to this defect, and it is separable. Go keeps the `IndexScan(… COVERING)` rendering.

## 9. Coverage

Each pin is a concrete test, mutation-checked, one mutation per independent failure
direction:

1. **`TestLikePrefix_…`'s `residual_below_the_fetch_drops_the_covering_stamp`** expectation
   flips to carry `COVERING`. Its `why` string already instructs exactly this. Direction 1.
2. **`Fetch(Covering(Index))` executes correctly** — an FDB test on a query whose projection
   is NOT covered, proving the fetch resolves full records rather than leaking partial rows.
   Direction 2, and the one that catches a regression to the pass-through fetch. A second
   assertion covers the orphan-entry path, which must still error loudly.
3. **The inner index plan is invisible to the census** (criterion C1) —
   `TestCoveringPlanInnerIsNotCounted`: build `Covering(Index)`, run `expressionCounts` over
   it, assert `indexScanCount == 0 && coveringIndexCount == 1`, and assert child traversal
   yields no quantifier for the inner. Direction 3. This replaces #658's PART 3 disabling
   experiment, which becomes meaningless once there is one construction site.
4. **Identity pins** (§6) — `structuralKey` distinguishes differing covering-column sets;
   `stablePlanNodeHash` has a pinned node head for the new type.
5. **Continuation fingerprint golden** (§7) — digest bytes for an unchanged plan, asserted
   equal to the pre-change value. The one assertion that must never be re-blessed.
6. **CQ-83's orphaned claim.** #658 retired "single-range projection errors loudly on forks"
   with no replacement, because the helper it pinned no longer exists (the range SET is now
   the only shape a consumer receives) — currently prose in TODO.md with nothing able to
   report a regression. The covering executor is a consumer of `newScanRangeSetCursor`, so a
   covering scan over a **non-terminal** signed-zero fork must return both rows (non-terminal
   is load-bearing: with a terminal fork the two zeros are adjacent and a single-range
   narrowing still passes). That pins "the consumer receives the whole range set" on the new
   path in the shape that actually breaks, instead of re-asserting a deleted helper.

   Feasibility VERIFIED rather than assumed: `executor.go:383` calls
   `bindScanComparisonsToRangeSet` — which delegates to the terminal-widening binder at
   `scan_range_binding.go:263`, the one that emits non-terminal forks as range-set
   alternatives — **before** the `IsCovering()` branch at `:421`. So the covering path
   provably consumes the same forked range set. The pin has a home; it is not contingent.

Every new test file is confirmed present in its Bazel target's `srcs` via `just gazelle` plus
a grep of the target's output for a line only that test emits — a file outside `srcs`
produces no signal at all while `go test` runs it happily.

## 10. Stress

1M stress baseline MEASURED at the branch point `4465dcc6d` (this tree, RFC-only diff), same
filesystem, `df -T .` = `xfs`, 89% used, 111G available. The box is XFS, so CLAUDE.md's ~95%
figure (an ext4 claim) does not transfer; the run is judged on its own numbers.

`TestFDB_Stress_1M` PASS in 336.10s. Key rows: PK lookups 0.02-0.09s; index equality 0.05s;
`full_scan_count` 4.72s; `order_by_pk_full` 1M rows 6.13s; `scan_all_narrow` 1M 6.26s;
`scan_all_wide` 1M 6.24s; `full_scan_sparse_filter` 97 rows 4.77s; `in_list` 26.4ms;
`needle_in_haystack_pk` 7.7ms. Recorded in TODO.md's "Stress test 1M baseline" table. The
after-run is compared row by row at implementation completion.

## 11. Gate

RFC → Graefe + Torvalds ACK **before** implementation → implement → one joint review lap +
a single codex run (`--timeout 2h`, never split) → PR → @claude LGTM → delta re-confirmation
on the final head → merge. Never merge on a NAK.

## 12. Unblocks

TODO.md **CQ-33** (`LIKE 'prefix%'` has no index access path). CQ-33's constraint stands and
this RFC does not weaken it: a byte-prefix range is a strict superset of `LIKE 'prefix%'`, so
the residual LIKE may never be dropped. RFC-216 pins that no LIKE pattern yields a tight
prefix range.
