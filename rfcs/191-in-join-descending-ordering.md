# RFC-191: Derive a real ordering for sorted IN-joins — Java parity for the no-sort shape (CQ-10f)

**Status:** Draft — revision 3. The full-table-scan **regression is CLOSED** (shipped as CQ-20, `8e12a1b59` +
`da2c6d57b`, outside this RFC). What remains is a **parity enhancement**: defects (a) and (b) below, needed to
reach Java's no-sort IN-join shape. Blocked on a **Graefe ruling** — not on further investigation — over a
measured Java rule-registration asymmetry that this revision traces to file:line. See "Design decision requiring
a Graefe ruling."
**Area:** Cascades query engine — `RecordQueryInJoinPlan.HintOrdering`, `ImplementInJoinRule`/`ImplementInUnionRule`
requested-ordering enumeration, plan partitioning (`ToPlanPartitions`/`orderingsEqual`), `PlanningCostModel`
**Reviewers:** Graefe (Cascades alignment + the InJoin-vs-InUnion cost decision, and the ruling this revision
requests), Torvalds (code quality), codex, @claude

## Revision note

### Revision 3 — status change: regression closed by CQ-20; this RFC is now a parity enhancement blocked on a
### Graefe ruling

Since revision 2, **CQ-20 shipped** (`8e12a1b59` + `da2c6d57b`) and closed the full-table-scan regression this RFC
was originally written to fix. It did so by re-diagnosing and fixing revision 2's defect (c) **at its source
property** (`PKScanOrdering`, `pkg/recordlayer/query/plan/plans/ordering.go`) rather than by either option this
RFC's revision-2 design-decision section proposed (extending `expression_partition.go`'s shared partition-key
machinery, or a narrower per-rule patch). See "Root cause: (c), CLOSED" below for what actually shipped and how it
compares to what was proposed.

With (c) closed, `SELECT * FROM tbl WHERE id IN (1,2,3) ORDER BY id DESC` now plans
`InMemorySort([ID DESC], InJoin(Scan(TBL,[=]), binding ASC))` — three bounded seeks under a sort — instead of a
full reverse table scan. **This is not yet Java parity**: Java plans the same query with no sort at all. Closing
that remaining gap is defects (a) and (b), unchanged in substance from revision 2, but their status changes here:

- A **live Java planner was instrumented** (`MemoTraceProbe`, described in full below — reusable, should not have
  to be rediscovered) to observe candidate-level memoization, not just final winners. It **refutes two prior
  hypotheses** about why Java reaches no-sort and Go doesn't, and locates the actual mechanism: an asymmetry in
  which `RecordQueryInJoinPlan` subclass `PushInJoinThroughFetchRule` is registered for, which looks like a Java
  oversight rather than a deliberate design choice.
- Because the mechanism is (probably) a Java bug, **this RFC can no longer just say "port Java's algorithm" and be
  done** — Go must choose between reproducing Java's asymmetry faithfully (bug included) or deliberately diverging
  from it. That choice needs a ruling, not an implementation. This revision presents both options with their
  consequences and does not pick one.
- **Corrected numbers**: revision 2's "196/196 (100%) bail at defect (b)" is superseded — post-CQ-20 the call
  count into the requested-ordering enumeration arm rose to 238 (more partitions now surface distinct Fixed-bound
  members, each producing its own call), and a prototype of the accessor-path bridge fix shows the bail rate drops
  to 40/238 (17%), not to zero. The 40 residual bails are uncharacterised — flagged under Undetermined, not
  guessed at.
- **23 of 25 corpus flips previously prototyped for (a)+(b) are now Java-confirmed correct** against the live
  instrumentation. The 2 exceptions are FETCH-required secondary-index shapes where Go's current behavior already
  matches Java's measured shape — an unguarded (a)+(b) would regress them. See "23/25 corpus flips" below.

None of this reopens revision 2's `orderingsEqual`/binding-blind-partition-key diagnosis, the concurrency
counter-argument, or the false-comment findings — those are carried forward unchanged and are noted as such where
they recur below.

### Revision 2 — responding to a Graefe/Torvalds NAK on revision 1

Revision 1's crux argument was refuted by measurement: the `inJoinCount` cost rung is reached 62 times
corpus-wide and InJoin does win real head-to-head comparisons against InUnion, so "the rung is simply unreached"
was false. Revision 1 also mis-described defect (c)'s firing mechanism (measured 0/196) and treated (c) and (d)
as two independent defects needing two independent fixes, when they are one architectural divergence with one
fix. This revision:

- **Retracts** the "blocker removal" crux and replaces it with a measured preference-change argument.
- **Merges** (c) and (d) into a single re-diagnosed root cause — Go's plan-partition key is binding-blind where
  Java's is not — traced to its actual source (`HintOrdering`/`properties.Ordering`, not `orderingsEqual`'s
  comparator body) and newly measured with a live rung/partition trace, including the partial-PK-prefix boundary
  the NAK asked to be investigated.
- **Drops** defect (d)'s proposed `expressions.FinalOf`-single-member copy of InUnion's pinning mechanism — it
  turns out to be unnecessary once the partition key is fixed, and copying it would have entrenched a Go-only
  workaround Java has no counterpart for, at a second site.
- **Reduces the design from four fixes to three**: (a) port `HintOrdering`, (b) fix the accessor-path Value
  bridge, (c) make the partition key binding-aware. All three still ship atomically, for the reasons revision 1
  gave.

Everything below is rewritten to reflect this. Where a claim from revision 1 survived the review unchanged, it is
carried forward and marked as such rather than silently re-asserted.

## Methodology note — what is measured vs read vs projected

This RFC leans on three different strengths of evidence and labels each claim:

- **[JAVA]** — read directly from `fdb-record-layer/` in this checkout (tag 4.12.11.0), quoted with file:line.
- **[GO]** — read directly from the current Go source at HEAD (`473d0d0af`), quoted with file:line.
- **[MEASURED — reviewer]** — executed by the Graefe/Torvalds review pass that NAK'd revision 1 (32 instrumented
  returns in `planningCostModelCompareWith`, swept over the full 2475-query corpus via
  `embedded.PlanPhysicalForTest`). Carried forward verbatim; not independently re-run in this revision.
- **[MEASURED — this revision]** — executed directly in this revision, in a throwaway `git worktree` (removed
  after), using the same `embedded.PlanPhysicalForTest` no-FDB harness plus targeted debug instrumentation of
  `ToPlanPartitions`, `enumerateSourceOrderingsForRequestedOrdering`, and `adjustBindingsForInUnion`.
- **[MEASURED — live Java, revision 3]** — executed against a **live Java planner** (not the no-FDB Go harness
  above), instrumented with a `MemoTraceProbe` registered on the public `PlannerEventListeners` bus, listening for
  `InsertIntoMemoPlannerEvent` and logging every `RecordQueryInJoinPlan`/`RecordQueryInUnionPlan` the planner
  memoizes — win or lose, not just the final winner. Driven through the conformance server's `planSql` step with
  FDB started via `WithDirectIP()` (the default cluster file trips an assertion in Java's FDB client that Go's
  tolerates). See "The measured Java mechanism" below for the full method and findings — it is reusable and
  documented here so it does not have to be rediscovered.
- Anything not measured is flagged **[PROJECTED]** and is a to-do for implementation, not a fact this RFC
  asserts.

This branch has repeatedly shipped a "Java does X" or "the mechanism is Y" claim that turned out to be reasoning,
not measurement — the `ORDER BY <pk-prefix> DESC` full-reverse-scan claim (CQ-10d), a mixed-direction-merge claim,
and now revision 1's own crux and its description of defect (c)'s firing mechanism. The discipline this revision
adds is not "measure once" — revision 1 measured plenty and still got the crux wrong, because it measured the
*blast radius* without measuring whether the rung it leaned on was *actually the load-bearing one*. The fix is to
re-verify a claim's own mechanism directly, every time, not just its aggregate outcome.

## Problem [existing TODO.md entry, re-verified] — the full-scan half is CLOSED, the sort-elimination half remains

```
schema: tbl(id BIGINT NOT NULL, k BIGINT NOT NULL, a, b, PRIMARY KEY (id,k)), INDEX ia ON tbl(a)
SELECT * FROM tbl WHERE id IN (1, 2, 3) ORDER BY id DESC

Go (before CQ-20): PredicatesFilter(Scan(TBL) REVERSE, [1 preds])                 -- FULL TABLE SCAN
Go (current, after CQ-20): InMemorySort([ID DESC], InJoin(Scan(TBL,[=]), binding ASC))
                                                                                   -- 3 bounded seeks, under a sort
Java:               INJOIN q0 -> { SCAN([IS TBL, EQUALS q0]) }, SORTED DESC       -- 3 bounded seeks, no sort
```

The Java side of this line is **[MEASURED]** against the live fdb-relational planner through the conformance
`SqlPlanSteps` harness (existing prior work, cited in TODO.md's CQ-10f entry) — not re-verified again here.

Introduced by commit `17a407aa3`. At its parent, the plan was
`InMemorySort([ID DESC], InJoin(Scan(TBL,[=]), binding))` — worse than Java (an avoidable sort), but not a full
scan. Default statistics, so the choice is size-independent: the same full scan wins on a billion-row table.

**CQ-20 closed the FULL-SCAN half of this gap** (defect (c) below), landing Go back at its pre-`17a407aa3` shape —
three bounded seeks, still under an avoidable sort. Corpus: 26 of 2633 statements moved, **12 improvements / 14
neutral / 0 regressions**, 1M stress byte-identical. Itemized, and summing to 26: 5 full scan → bounded InJoin,
4 redundant `InMemorySort` eliminated, 2 `InJoin`→`InUnion` with the sort eliminated, and the reproducer itself
(12 improvements); 12 tag-only `binding`→`binding ASC` on plans already under a sort, 1 REVERSE flag dropped
beneath an outer re-sort, and 1 `NestedLoopJoin` operand flip on an INNER join (14 neutral). An earlier revision
of this RFC quoted "21 improvements / 5 neutral" — that figure was a summary that did not reconcile with its own
category table, and 13 of the 26 are shape flips (the improvements plus the `NestedLoopJoin` reorder), which is a
different count again and is what the "13 shape flips" figure elsewhere refers to.
**What remains open is the sort itself** — Java needs none, because its `RecordQueryInJoinPlan` advertises a real
ordering and its `ImplementInJoinRule` can find it through the requested-ordering arm. That is defects (a) and
(b), the subject of the rest of this RFC.

A fifth, related defect — an IN-join's `sorted`/`reverse` flags claiming an ordering the values were never
actually put in — was **already fixed** in `b2318ddb5` (CQ-10g), along with a plan-invariant that rejects any
plan claiming an order its values don't back. That fix is a prerequisite for this one.

## Root cause, revised: three defects, one of which is a merge of the old (c) and (d)

### (a) `RecordQueryInJoinPlan.HintOrdering` returns the empty ordering unconditionally

**Unchanged from revision 1** — not implicated in the NAK.

**[GO]** `pkg/recordlayer/query/plan/plans/ordering.go:471-473`:

```go
func (p *RecordQueryInJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}
```

Absent from `HintRichOrdering()` too: only `RecordQueryScanPlan`, `RecordQueryIndexPlan`,
`RecordQueryVectorIndexPlan`, `RecordQueryFetchFromPartialRecordPlan`, and one `abstract_data_access_rule.go`
wrapper implement it. `RecordQueryInJoinPlan` does not.

**[JAVA]** `OrderingProperty.visitInJoinPlan` (`OrderingProperty.java:392-459`) — quoted in full in revision 1,
unchanged here. The algorithm: find the inner ordering's binding for the IN-alias; if it's Fixed
(equality-bound) **and** the in-source is sorted, promote **that one binding** to directional and concatenate it
in front of the (filtered) inner ordering. This is the exact shape defect (a) needs to port.

This defect is real and independent of everything else in this RFC: Go's InJoin already produces
`sorted=true, reverse=false` candidates on the CQ-10g-era hardcoded-ASC path (62 corpus-wide, per the reviewer's
`inJoinCount` sweep below), and every one of those advertises `Ordering{IsKnown:false}` to any downstream
consumer of `HintOrdering` — a latent missed-sort-elimination bug that exists whether or not this RFC's other two
fixes land. Kept in scope because (b)/(c) below make more InJoin candidates reachable, and every one of them
needs (a) to advertise its order correctly once built.

### (b) The requested-ordering arm's binding lookup never bridges Value identity — sharpened, now the sole
### confirmed blocker for InJoin

**[GO]** `pkg/recordlayer/query/plan/cascades/rule_implement_in_join.go`,
`enumerateSourceOrderingsForRequestedOrdering`, line 277:

```go
bindings := richOrdering.GetBindingMap()[part.Value]
if len(bindings) == 0 {
	return nil
}
```

`GetBindingMap()` returns `map[values.Value][]OrderingBinding`, keyed by Go map identity over the
`values.Value` interface — not by any equality method. `part.Value` comes from the translator's baked requested
ordering (a `FieldValue` with a resolved ordinal, e.g. `ID#0`); the map's keys come from the inner plan's rich
ordering (a lazy `FieldValue`, `ID`). Different Go values, so the lookup misses on every call.

**[MEASURED — reviewer]**, instrumenting this exact line over the 2475-query corpus:

```
requestedOrderingWalk entered        : 196
  -> nil @emptyBindingLookup         : 196  (100%)
  -> nil @sortOrderIsDirectional     : 0
  -> parts matching an explode alias : 0
```

**[MEASURED — this revision]**, independently reproduced on a 2-query micro-repro (`ORDER BY id DESC` and
`ORDER BY id DESC, k DESC` against the reproducer's schema, one partition per call, 4 calls total): all 4 hit
`EMPTY BINDING LOOKUP`, 0 reached the directional check below it. Same signature the reviewer found, on a
narrower, hand-picked pair of queries chosen specifically to probe the boundary the NAK asked about (see the
partial-PK-prefix section below) — the fact that the failure mode is identical on both the partial-PK and
full-PK variant is itself evidence that defect (b) is genuinely request-shape-independent, not an artifact of
which corpus queries happened to get sampled.

**The bridge already exists**, in `(*RichOrdering).Satisfies` (`properties/rich_ordering.go:254-299`), which
resolves through the *private* `orderingKeyFor` (`:319`): exact `ExplainValue` match first, then the
ordinal-free `ColumnNameValue` bridge, then the full-accessor-path bridge via `values.CanBridgeOrderingValueRoots`.
Neither `orderingKeyFor` nor its caller `bindingMapForExplain` (`:305`) is exported; `rule_implement_in_join.go`
is in package `cascades` and cannot call them.

**Sharpened per the NAK**: the two Values here are **not semantically equal** — `SemanticEqualsUnderAliasMap`
returns false for the baked-vs-lazy pair; only their rendered `ColumnNameValue` (`ID`) agrees. **The fix must go
through the name/accessor-path bridge `orderingKeyFor` already implements, not through a semantic-equality
lookup** — a semantic-equality-based fix would silently fail to close this gap, because the two `FieldValue`s
genuinely are not semantically equal (one carries a resolved ordinal, the other doesn't; `SemanticEqualsUnderAliasMap`
is not the relation that unifies them). The fix is: add a small public accessor on `*RichOrdering` — e.g.
`BindingsForValue(v values.Value) []OrderingBinding` — that runs the same `orderingKeyFor` + `bindingMapForExplain`
path `Satisfies` uses internally, and have `enumerateSourceOrderingsForRequestedOrdering` call it instead of the
raw map index. Grep the `properties` test files first for an existing equivalent public helper before adding one.

**This is the sole confirmed, unconditional blocker for InJoin's requested-ordering enumeration today** — 100%
of calls die here, 0% ever reach the code the old defect (c) description blamed. Everything below that used to
be called "defect (c)" for InJoin is therefore **inert** until (b) is fixed — but it is *not* inert for InUnion,
which has its own, independent path into the same underlying problem (see the merged defect below).

**`ImplementInUnionRule` does not share defect (b)**, and this is by construction, not luck. **[GO]**
`rule_implement_in_union.go`'s `bakeMergeComparisonKeys` (comment at lines 14-24) resolves its merge-comparison
keys through "the REQUESTED ordering's own part value... matched by `ColumnNameValue`, the same bridge
`orderingKeyFor` uses" — InUnion's requested-ordering path already routes through the name-based bridge defect (b)
is about adding to InJoin. That is why the partial-PK-prefix trace above (and the corpus flips already landing
`Fetch(InUnion(...))` today) can reach the co-partitioning bug (the old "defect (c)"/"defect (d)") directly,
without first needing defect (b)'s fix at all — InUnion's Value-identity problem was already solved before this
RFC existed. InJoin's `rule_implement_in_join.go` never adopted the same bridge; that gap, not a shared root
cause, is what defect (b) closes.

### (c) [MERGED — was (c)+(d)] The plan-partition key is binding-blind; Go's partitioning co-groups members Java
### would never co-partition — **CLOSED, shipped as CQ-20**

**Status update (revision 3): this defect is fixed and shipped**, `8e12a1b59` + `da2c6d57b`, outside this RFC (it
did not need the milestone-level Cascades gate — Graefe classified it as a narrow, isolable fix during the CQ-20
review, separate from the (a)+(b) workstream this RFC still gates). The diagnosis below is **unchanged and
confirmed correct** — CQ-20's root cause is exactly this section's `PKScanOrdering` finding — but the **fix that
shipped is narrower than either option this RFC's revision-2 design-decision section proposed**: neither (i)
extending `expression_partition.go`'s shared `orderingsEqual`/`orderingPartitionHash` to compare binding kind, nor
(ii) a per-rule patch at the two IN-rule call sites. Instead, `PKScanOrdering` itself
(`pkg/recordlayer/query/plan/plans/ordering.go`) was fixed to **trim the equality-bound PK prefix out of its
`Keys`**, mirroring the firstNonEq logic `RecordQueryIndexPlan.HintOrdering` already used and matching Java's
`ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons` (`ValueIndexLikeMatchCandidate.java:126-196`).
Once the source property stops reporting a Fixed-bound column as a sorted key, the existing, **untouched**
`orderingsEqual` correctly tells the two members apart — the partitioner was never wrong, it was being handed
lossy input. This is the narrowest of the three options considered anywhere in this RFC's history, and it means
the blast-radius risk flagged below (eight other `ToPlanPartitions`/`RollUpPlanPartitions` consumers) never
materialized: `expression_partition.go` was not touched at all. Confirmed by the corpus diff (26/2633 changed, 0
regressions) and by the new regression test
`TestToPlanPartitions_SeparatesFixedBoundScanFromSortedUnboundScan`
(`pkg/recordlayer/query/plan/cascades/pk_scan_ordering_partition_test.go`).

The rest of this section is kept verbatim as the historical diagnosis — it correctly identifies the root cause
CQ-20 fixed, and the reasoning is reused below for why (a)/(b) still need the merged-partition analysis.

Revision 1 described this as two separate defects: (c) "the wrong partition member gets picked first," fixed by
scanning for a promotable member; (d) "the whole partition gets memoized unpinned," fixed by copying InUnion's
`expressions.FinalOf`-single-member pin to InJoin. The NAK's finding that Java's `ImplementInJoinRule` **also**
memoizes the whole, unpinned partition — exactly like Go — refutes (d) as originally framed: if Java does the
same thing and is correct, the bug can't be "Go fails to pin," it has to be something upstream that makes the
*partition itself* wrong in a way Java's never is. That thing is here.

**[GO]** `pkg/recordlayer/query/plan/cascades/expression_partition.go:392-410`:

```go
func orderingsEqual(a, b properties.Ordering) bool {
	if a.IsKnown != b.IsKnown || len(a.Keys) != len(b.Keys) {
		return false
	}
	for i := range a.Keys {
		if !values.ValuesStructurallyEqual(a.Keys[i], b.Keys[i]) {
			return false
		}
		if a.DescendingAt(i) != b.DescendingAt(i) || a.NullsFirstAt(i) != b.NullsFirstAt(i) {
			return false
		}
	}
	return true
}
```

The NAK's framing was "`orderingsEqual` does not compare binding maps." That is true but understates the bug:
**it is not fixable by editing this function's body**, because the `properties.Ordering` type it operates on —
`{IsKnown, Keys, Descending, NullsFirst}` — has no field to hold a binding map in the first place. Binding
information (Fixed/equality-bound vs. genuinely Sorted vs. Choose) lives only in the separate `RichOrdering`
type, which plan partitioning never consults. The type that drives partitioning is structurally incapable of
representing the distinction this bug is about.

Where this comes from: `computeWrapperOrdering` (`plan_properties.go:323-331`) is what populates the partition
key (`PropOrdering`), and for `RecordQueryScanPlan` it dispatches to `PKScanOrdering`
(`plans/ordering.go:234-249`):

```go
func PKScanOrdering(plan *RecordQueryScanPlan) properties.Ordering {
	pk := plan.GetPrimaryKeyValues()
	...
	desc := make([]bool, len(pk))
	if plan.IsReverse() {
		for i := range desc { desc[i] = true }
	}
	return properties.Ordering{IsKnown: true, Keys: pk, Descending: desc}
}
```

This includes **every** PK column in `Keys`, all with the same `Descending` flag from `IsReverse()`, **without
ever looking at the scan's comparisons**. A per-binding equality scan `Scan(TBL,[id=q0])` and a fully unbound
`Scan(TBL)` — one with `id` Fixed, one with `id` genuinely Sorted-Ascending — report the **identical**
`Ordering{Keys:[id,k], Descending:[false,false]}`. There is no way for `orderingsEqual` to tell them apart; the
information was already discarded one layer up, at `HintOrdering()`.

Compare `RecordQueryScanPlan.HintRichOrdering()` (`plans/ordering.go:546-570`), which *does* carry this
distinction — equality-bound PK positions become `FixedBinding`, the rest `SortedBinding` — but is only
consulted by `computeWrapperRichOrdering`, which `ToPlanPartitions`/`toPartitionsFromMap` never calls.

**[JAVA]** `Ordering.equals` (`Ordering.java:250-261`):

```java
@Override
public boolean equals(final Object o) {
    ...
    final var ordering = (Ordering)o;
    return getBindingMap().equals(ordering.getBindingMap()) &&
           getOrderingSet().equals(ordering.getOrderingSet()) &&
           isDistinct() == ordering.isDistinct();
}
```

Java has **one** `Ordering` class, and it always carries a binding map; `equals` always compares it. Java's
`PlanPartition` grouping (which uses this `equals`) therefore **cannot** co-partition a Fixed-bound scan with a
Sorted-unbound one in the first place — their binding maps differ, so they are unequal Orderings by construction,
full stop. Go split one Java concept into two Go types (`Ordering` for partitioning, `RichOrdering` for
richer downstream reasoning) and the type that actually drives partitioning is the one that lost the
distinction. This is the real root cause: **not a missing comparison inside `orderingsEqual`, but a partition key
computed from a type that cannot express what Java's equivalent key always expresses.**

**[MEASURED — this revision]**, direct trace confirming both members really do co-partition and that this drives
both InJoin's (b)-adjacent pick and InUnion's failure:

```
INJOIN/INUNION partition members=2 ordering=known=true keys=[ID ASC,K ASC]
  [0] *plans.RecordQueryPredicatesFilterPlan PredicatesFilter(Scan(TBL), [1 preds])
      rich=keys=[ID(fixed=false sortOrder=Ascending), K(fixed=false sortOrder=Ascending)]
  [1] *plans.RecordQueryScanPlan Scan(TBL, [=])
      rich=keys=[ID(fixed=true sortOrder=Fixed), K(fixed=false sortOrder=Ascending)]
```

Exactly the shape revision 1 described from static reading, now measured: `Scan(TBL,[id=q0])` (`id` Fixed) and
`PredicatesFilter(Scan(TBL))` (`id` Sorted-Ascending, from the PK's natural order) land in the same partition,
member `[0]` (the wrong, unbound one) sorts first.

**For InJoin**: this is the mechanism the old "defect (c)" described — `richOrdering` picked from partition
member `[0]` — but it is **provably inert today**, because (b) already returns `nil` before this ever matters
(0/196 measured). It becomes *live* the moment (b) is fixed, so it still needs fixing, just not on its own
timeline — it was never separately observable until now.

**For InUnion**: this is **not** inert — InUnion has its own, independent path to the same first-member pick
that is **not** gated by defect (b) at all. `ImplementInUnionRule.OnMatch` (`rule_implement_in_union.go:249-255`)
does the identical first-physical-member scan, then `adjustBindingsForInUnion`
(`rule_implement_in_union.go:368-436`) uses it directly — see the reproducer diagnosis below for the measured
mechanism.

**Why the merge, and why (d) as originally proposed must be dropped**: once the partition key correctly
separates Fixed-bound members from Sorted/unbound ones — mirroring Java's `Ordering.equals` — a partition that
contains the promotable member either (i) contains *only* structurally-equal (by the fixed binding-map key)
members, in which case "first member" is unambiguous and correct, no picking logic needed at all; or (ii)
legitimately contains multiple genuinely-equivalent alternatives (e.g. two different Fixed-bound scans satisfying
the same equality), in which case memoizing the *whole* partition unpinned and letting the memo's own
cost-driven `Winner()` resolution choose among them is **exactly what Java does** (per the NAK's own finding) and
is correct — the cheaper candidate (fewer residual predicates, bounded data-access count) wins on ordinary
costing grounds, with no special pinning required. The originally-proposed defect-(d) fix — copying InUnion's
`memberSatisfiesOrdering` scan + `pinOrderedSpine` + `expressions.FinalOf`-single-member pin
(`rule_implement_in_union.go:305-342`) to InJoin — solves a problem that a correct partition key makes not exist,
and does so by entrenching a Go-only mechanism at a second call site (its own doc comment,
`implementation_rule.go:130-136`, warns `FinalOf` *"changes what `ExploreGroupTask` does with the reference"* —
a warning revision 1 quoted but never resolved). **Dropped from this design.**

## The partial-PK-prefix boundary — investigated per the NAK, does not reveal a fourth defect

The NAK flagged an unremarked boundary: `ORDER BY id DESC, k DESC` (full PK) plans correctly via
`InUnion(Scan(TBL,[=]) REVERSE, bindings=1, DESC)`, while `ORDER BY id DESC` alone (partial-PK prefix, the
reproducer's own shape) degenerates to the full reverse scan. Investigated directly.

**[MEASURED — this revision]**, both queries run against the reproducer's schema with partition/rich-ordering
tracing on:

**`ORDER BY id DESC`** — one partition only, two co-partitioned members (`[0]` unbound-Sorted-ASC,
`[1]` Fixed-bound), as shown above. InJoin dies at defect (b) (100% of calls). InUnion's
`adjustBindingsForInUnion` picks member `[0]`'s binding for `id` — `Sorted(Ascending)`, `IsDirectional()==true` —
which takes the **first** branch of the function (`rule_implement_in_union.go:381-385`) and keeps it
`Sorted(Ascending)` **without ever consulting the request**:

```go
sortOrder := properties.SortOrderOf(bindings)
if sortOrder.IsDirectional() {
	adjustedBM[val] = []properties.OrderingBinding{properties.SortedBinding(sortOrder)}
	continue
}
```

The DESC-vs-ASC mismatch then fails `IsCompatibleWithRequestedSortOrder` inside
`EnumerateSatisfyingComparisonKeyValues`, `satisfyingKeys=0`, InUnion yields nothing for this partition. Final
plan: `PredicatesFilter(Scan(TBL) REVERSE, [1 preds])`.

**`ORDER BY id DESC, k DESC`** — **two** partitions. Partition A is the identical co-partitioned pair above,
with the identical failure (member `[0]` picked, `satisfyingKeys=0`). But a **second, singleton** partition B
also exists:

```
INJOIN partition members=1 ordering=known=true keys=[ID DESC,K DESC]
  [0] *plans.RecordQueryScanPlan Scan(TBL, [=]) REVERSE
      rich=keys=[ID(fixed=true sortOrder=Fixed), K(fixed=false sortOrder=Descending)]
```

`Scan(TBL,[id=q0]) REVERSE` — genuinely Fixed on `id`, genuinely Sorted-Descending on `k` — generated by the
"enumerate descending ordered variants" data-access machinery (`17a407aa3`) specifically because there is a `k`
column left over to push an ordering request into once `id` is consumed by the per-binding equality. Because this
partition has exactly **one** member, "pick the first physical member" cannot pick wrong — there is nothing else
to pick. `adjustBindingsForInUnion` sees `id` already Fixed (not directional), falls into the promotion branch,
and — because the requested-Value lookup here (`reqMap[val]`) is *also* subject to the same lazy-vs-baked Value
identity mismatch as defect (b), confirmed by the adjusted binding coming out `Choose` rather than an
explicitly-promoted `Sorted(Descending)` — lands on `ChooseBinding()`, which is unconditionally compatible with
any requested direction. `k` stays `Sorted(Descending)`, which happens to already match the DESC request.
`satisfyingKeys=1`. Final plan: `InUnion(Scan(TBL, [=]) REVERSE, bindings=1, DESC)` — via partition B, not
because partition A's bug was avoided.

**Conclusion, directly measured, not projected: the boundary is incidental, not a distinct root cause.** The
2-column case does not "work" because defects (b)/(c) don't apply there — they apply *identically* (defect (b)
still kills every InJoin call; the co-partitioning bug still fires on partition A exactly as before). It works
because an *unrelated* mechanism (reverse-ordered-variant enumeration, which only fires when there is a
non-PK-prefix column left to push an ordering request into) happens to manufacture a second, accidentally
uncorrupted partition that sidesteps the co-partitioning bug by having only one member. The 1-column case has no
such escape hatch, because there is nothing left to push down once `id` alone consumes the entire request.

This **does not collapse the defect list below three** — it is not a fourth, independent mechanism. It **confirms**
the re-diagnosis above with a second, structurally different measured scenario: defect (b) is
request-shape-independent (fires identically in both queries), and the co-partitioning bug is the actual and
sufficient explanation for every InUnion/InJoin failure on this shape family, with the boundary's location fully
explained by whether an unrelated enumeration feature happens to produce a lucky singleton partition. It does
strengthen the case for fixing the partition key itself (this RFC's revised defect (c)) rather than only patching
call sites: a call-site patch confined to InJoin/InUnion would still leave this exact co-partitioning failure mode
live for any other partition consumer that hits the same shape without a second data-access feature bailing it
out by luck.

## The measured Java mechanism — live instrumentation, revision 3

Revision 2 reasoned about Java's algorithm entirely from static source reading (`OrderingProperty.visitInJoinPlan`,
`PlanningCostModel.java`'s cost rungs). This revision adds **candidate-level runtime evidence** from a live Java
planner, because two plausible-sounding hypotheses about *why* Java reaches no-sort turned out to be wrong, and
static reading alone would not have caught it.

### Method — `MemoTraceProbe`, reusable

Java's Cascades planner exposes `PlannerEventListeners`, a public event bus. This revision registers a listener
for `InsertIntoMemoPlannerEvent` — fired every time the planner memoizes a candidate expression into a memo group,
whether or not that candidate ever wins — and logs every `RecordQueryInJoinPlan`/`RecordQueryInUnionPlan` insert:
its concrete subclass, `sorted`/`reverse` flags, and the inner plan shape. This is driven through the conformance
server's `planSql` step, with FDB started via `WithDirectIP()` — the default cluster-file path trips an assertion
in Java's FDB client that Go's client tolerates, so `WithDirectIP()` is required to get a live Java planner running
at all in this environment. The probe and the `WithDirectIP()` requirement are both reusable for any future
Cascades RFC that needs to see what Java actually memoizes, not just what it finally picks — record it here so the
next investigation doesn't have to rediscover either.

### Two prior hypotheses, refuted

**[MEASURED — live Java, revision 3]**

1. *"Java never enumerates an ordered InJoin candidate for the FETCH-required shape"* — **false**. It does:
   ```
   NEW RecordQueryInComparandJoinPlan sorted=true reverse=false ::
     [IN arrayDistinct(...) SORTED] | INJOIN q -> { ISCAN(IA [EQUALS q]) }
   ```
   Java builds this candidate and memoizes it. It then **loses a genuine cost comparison** against the InUnion
   candidate for the same logical query — there is no elimination step that removes it before costing.

2. *"The three fetch rungs tie and control falls through to `numSourcesInJoin`"* — **false for ASC**. The decision
   happens **one rung earlier**, at the fetch-position rung, not at `numSourcesInJoin`.

### The measured mechanism — ASC

- `RecordQueryIndexPlan` (a bare, non-covering index scan) implements `RecordQueryPlanWithIndex` and is counted as
  an **implicit** fetch by `PlanningCostModel.java:213`; `RecordQueryCoveringIndexPlan` is invisible to that count.
- `PlanningCostModel.java:231-236` breaks the "ISCAN vs Covering+explicit Fetch" tie by counting only **explicit**
  `RecordQueryFetchFromPartialRecordPlan` nodes — 0 on the bare-ISCAN side, 1 on the Covering+Fetch side — so
  InJoin's inner settles on the bare `ISCAN(IA[EQUALS q])` shape, never a covering scan wrapped in an explicit
  fetch.
- **`PushInJoinThroughFetchRule` is registered only for `RecordQueryInValuesJoinPlan` and
  `RecordQueryInParameterJoinPlan`** (`PlanningRuleSet.java:152-153`) — **never** for `RecordQueryInComparandJoinPlan`,
  which is what `toInJoinPlan()` produces for every *sorted* IN-source. So `Fetch(InJoin(Covering(...)))` is
  structurally never built for the sorted case. Confirmed empirically: that shape appears nowhere in either the
  ASC or DESC trace.
- `PushSetOperationThroughFetchRule` **is** registered for `RecordQueryInUnionOnValuesPlan`
  (`PlanningRuleSet.java:160`) and fires, producing `Fetch(InUnion(Covering(...)))` with the fetch at depth 0 (the
  root of the plan).
- At the top-level comparison, `numFetches` ties 1–1 (InJoin's inner scan is non-covering and counted implicit;
  InUnion's fetch is explicit but also counts 1). Then **`fetchDepth`/`fetchPositionCompare`
  (`PlanningCostModel.java:220-226`) decides**: InUnion's fetch sits at depth 0 (it *is* the plan root), InJoin's
  fetch (implicit, inside the InJoin's inner) is buried at depth 1. InUnion wins the fetch-depth rung.
  **`numSourcesInJoin` is never reached for this pair** — hypothesis 2 above is refuted precisely because this
  earlier rung already decides it.

### DESC — same mechanism, different outcome

**[MEASURED — live Java, revision 3]**: every `Fetch(InUnion(...))` candidate memoized for the DESC case wraps the
**unbounded** leg — `COVERING(IA<,>REVERSE)|FILTER a EQUALS q`, a full reverse scan with the equality demoted to a
residual filter, not used as a search argument — **never** the bounded `COVERING(IA[EQUALS q])` leg that the ASC
trace uses. With no depth-0 InUnion candidate available for DESC, both InJoin and InUnion tie at fetch-depth 1,
control reaches `numSourcesInJoin` (`PlanningCostModel.java:254-262`), which **unconditionally prefers InJoin** —
matching the observed DESC winner in the original reproducer.

**Not traced, and explicitly not claimed**: *why* the bounded-covering InUnion leg is unreachable for DESC
specifically. This is recorded as unresolved, not as the same phenomenon as the descending/`ToOrderedBytesValue`
fragility documented elsewhere in this codebase (Go lacking a descending-bytes evaluator) — that connection is
*plausible* given both are DESC-specific index-scan-direction issues, but it is **unproven**; no measurement in
this revision traces the DESC InUnion candidate generation path itself. See Undetermined.

### The 3-column shapes throw, they don't silently degrade

**[READ — Java]**: `ORDER BY a, id, k` and its DESC form both throw `UnableToPlanException` at
`CascadesPlanner.resultOrFail` (`CascadesPlanner.java:403-407`, called from `planGraph` at `:389`): after
`planPartial` reaches fixpoint, the root reference's `getFinalExpressions()` is empty — exploration stalls at the
logical `SelectExpression` with zero physical candidates ever reaching the root. This is a Java planning failure
for this shape family, not a silently-worse plan; it means the 3-column shapes are not usable evidence for either
side of the design-decision question below (Java has no observable winner there to match or diverge from).

### 23 of 25 corpus flips are Java-confirmed correct

**[MEASURED — this revision]**: of the corpus flips a prototype of (a)+(b) produces (revision 2's "prototyped and
reverted" set, re-run against current HEAD), **23 of 25 are confirmed correct** against the live-Java trace above
— Go's flipped plan shape matches what the instrumented Java planner actually memoizes and wins for the
corresponding query.

**The 2 exceptions are exactly the FETCH-required secondary-index shapes**
(`in_over_primary_scan_sarg.yaml#14,#15`). **Go today already plans both as `Fetch(InUnion(...))`**, which matches
Java's measured ASC winner for that shape (per the mechanism above). Current Go behavior is *already right* on
these two — an unguarded (a)+(b) landing would flip them to `Fetch(InJoin(...))`-or-similar and **regress** them,
because Go has no `RecordQueryInComparandJoinPlan`/`RecordQueryInValuesJoinPlan` split and so cannot naturally
reproduce Java's rule-registration asymmetry that keeps InJoin from winning this specific FETCH-required pair. See
the design-decision section immediately below — this is the concrete case that makes the asymmetry question load-
bearing rather than academic.

A per-item file:line table for the 23 confirmed flips lives with the review pass that produced this measurement
(the same live-Java sweep as the mechanism trace above); not reproduced entry-by-entry here to avoid re-typing
data this RFC did not independently re-derive. The 2 exceptions and their file:line are the load-bearing detail
for the design decision and are recorded in full above.

### A guard aimed at `numSourcesInJoin` would be wrong

A fix that special-cased the `numSourcesInJoin` tie-break (e.g. "don't let InJoin win `numSourcesInJoin` for
FETCH-required shapes") would happen to fix the DESC case, because DESC really does reach that rung — but it would
**completely miss ASC's actual mechanism**, which decides one rung earlier at fetch-depth and never touches
`numSourcesInJoin` at all. This is exactly the same failure mode revision 1 fell into with its own crux (measuring
the *outcome* without verifying the *actual load-bearing rung*) — recorded here so a future implementation attempt
doesn't repeat it a third time.

## Design decision requiring a Graefe ruling

Java's ASC outcome for the FETCH-required shape depends on a fact this revision cannot resolve by reading more
code: **`PushInJoinThroughFetchRule` is registered for `RecordQueryInValuesJoinPlan` and
`RecordQueryInParameterJoinPlan` but not for `RecordQueryInComparandJoinPlan`** (`PlanningRuleSet.java:152-153`),
even though `toInJoinPlan()` produces the comparand variant for every *sorted* IN-source — the exact case this RFC
is about. There is no comment, test, or design note in the Java source explaining why the comparand variant is
excluded. **This looks like an oversight, not a deliberate design choice** — but this RFC cannot prove Java's
authors intended it, and it is not this RFC's place to guess at Java's intent and encode the guess as Go's
behavior without a ruling.

Go therefore has two options, and this RFC presents both without picking one:

**(i) Reproduce Java's asymmetry faithfully, oversight included.** Go implements the same fetch-accounting and
fetch-depth mechanism Java uses, including the same gap — an equivalent to `PushInJoinThroughFetchRule` that
covers Go's value/parameter-sourced InJoins but not its comparand/sorted-sourced ones. Consequence: Go matches
Java bit-for-bit on this shape family, including a shape Java's own authors may not have intended, and inherits
whatever future surprise Java's own asymmetry produces (e.g. if Java fixes it upstream, Go silently diverges
again). Consistent with this repo's "Java is the reference, always" and "doesn't work in Java → doesn't work in
Go, in the same architectural way" principles taken literally — those principles are about not silently diverging
where both engines run the same query, and this is exactly that case.

**(ii) Deliberately diverge — extend the fetch-push rule to cover the comparand-equivalent Go shape too.**
Go treats this as a bug Java happens to have and Go doesn't need to inherit, on the reasoning that a `Fetch(InJoin(
Covering(...)))` shape is strictly better (fewer bytes read from FDB, same row count) whenever it's reachable, and
there is no principled reason to withhold it from sorted IN-sources specifically. Consequence: Go's read-side
query surface exceeds Java's for this shape (allowed under "the read-side query surface MAY go beyond Java" — this
is a genuine net-new capability, not a shared-surface silent divergence, *provided* the ruling agrees this framing
is correct), but it means Go and Java can disagree on which physical plan wins for the identical logical query and
schema — a fact any cross-engine golden/corpus comparison must account for explicitly rather than assume away.

**Why this needs a ruling and not an implementation decision**: the two options are not "which is more correct" —
option (i) is more Java-faithful, option (ii) is more logically defensible on cost grounds alone. This is exactly
the kind of judgment call the query-engine gate exists to route to Graefe rather than have an implementer pick
silently. Flagging it here is what makes this a legitimate deferral rather than a dropped finding, per this
repo's own "no excuses" principle — the finding is fully measured and file:line-cited above; only the *choice*
between two measured-correct options is outstanding.

### What Go would need for option (i)

If Graefe rules for (i), the implementation has three concrete pieces, all measured/read above:

1. **Count a non-covering index scan as an implicit fetch**, the way Java's `RecordQueryPlanWithIndex` bucket
   does (`PlanningCostModel.java:213`) — Go's current fetch-count rung (see `planning_cost_model.go`, criterion
   referenced in DIVERGENCES.md's cost-model table) needs the equivalent bucket, not just an explicit
   `RecordQueryFetchFromPartialRecordPlan` count.
2. **Compare fetch depth from the plan root, not only fetch count** — Go's `inJoinCount`/fetch rungs currently
   have no depth-from-root comparison at all; this is a new rung, not an extension of an existing one.
3. **Reproduce the rule-registration asymmetry** — push a fetch through InUnion but not through the
   sorted-IN-source InJoin shape.

**Note directly relevant to the ruling**: Go does not split `RecordQueryInJoinPlan` into Java's
value/parameter/comparand sibling classes — Go has one struct with a `sourceKind` discriminant field (see CQ-10g's
writeup in TODO.md, "the live plan representation is the flat `sourceKind` + `[]any` fields... not a class
hierarchy"). Piece 3 above therefore has **no natural expression in Go's type system** — Go would need to key the
fetch-push rule off `sourceKind` explicitly, which is a special-case `if sourceKind == X` check of exactly the
kind this repo's design principle #10 ("emergent behaviour over special-case checks") warns against. That Go's
architecture makes option (i) *structurally* more awkward than it is in Java (where it falls out of which rule
class matches which plan class) is itself an argument relevant to the ruling, not a reason to avoid stating the
asymmetry plainly.

## The crux, retracted and replaced

### What revision 1 claimed, and why it's false

Revision 1 argued the InUnion→InJoin ascending flips were safe because Go already has Java's
`numSourcesInJoinCompare`/`inJoinCount` cost rung, and InUnion currently wins "by being the only surviving
candidate, not by cost comparison" — implying the rung is essentially unreached today and this RFC "merely"
unblocks it into firing, inheriting Java's own already-accepted preference.

**[MEASURED — reviewer]**, instrumenting all 32 returns in `planningCostModelCompareWith` and sweeping all 2475
corpus queries:

- The `inJoinCount` rung **is** reached: **62** decisions corpus-wide, many literal `A=InUnion B=InJoin` pairs.
- On `SELECT id, name FROM t WHERE id IN (1, 3) ORDER BY id`, the rung fires **three times** and **InJoin wins**.
- Ordering-tagged InJoin candidates **are** already yielded today, e.g.
  `InJoin(TypeFilter([T], Scan(T, [=])), binding ASC)`.
- Of the 25 corpus queries whose winning plan contains `InUnion`, the rung fired during planning for **15
  (60%)**.
- InJoin loses **higher up** for the other 10: at projection level once the enforcer sort attaches
  (`Project(InUnion(...))` beats `Project(InMemorySort([ID ASC], InJoin(...)))` at the primary-vs-index-scan
  rung), and elsewhere at the data-access-cardinality and residual-predicate rungs.

So InUnion is demonstrably not "the only surviving candidate" today, and the rung is demonstrably not unreached.
**The crux as stated in revision 1 is false. Retracted.**

### Re-argued: this is a preference change, not a blocker removal — measured before/after

What this RFC actually does to the ascending-ORDER-BY corpus queries is change which of two *already-competing*
candidates wins, by making InJoin's candidate exist and be correctly ordering-tagged in more cases than it is
today (defects (a)-(c) above are about *reachability of a correctly-tagged InJoin candidate*, not about whether
the cost rung that picks between reachable candidates is itself broken — that rung is fine and stays untouched).

**[JAVA]** `ImplementInJoinRule` and `ImplementInUnionRule` match the identical logical pattern and both
`call.yieldPlan(s)` into the same memo group; neither defers to the other. The choice is made once, downstream,
by `PlanningCostModel.compare`.

**[JAVA]** `PlanningCostModel.java:251-262` — the explicit, direction-independent "more InJoin nodes wins" rung,
quoted in full in the prior revision, unchanged here.

**[GO]** `planning_cost_model.go:354-355` ports this rung faithfully:

```go
if opsA.inJoinCount != opsB.inJoinCount {
	return intCompare(opsB.inJoinCount, opsA.inJoinCount)
}
```

This rung's own logic (`inJoinCount`, more-is-better) is a correct, faithful port of Java's — that specific claim
from revision 1 survives. What does **not** survive is the implication that this rung's mere presence, faithfully
ported, made the InUnion→InJoin flips inevitable or "already decided by Java." The flips happen because making
more InJoin candidates *reachable* changes what competes at this rung (and at the rungs above it — see the
corrected chain-position discussion below) in specific corpus queries; that is a genuine, measured preference
shift this RFC is choosing to make, on the grounds that it makes Go pick the same *winner* Java picks for these
shapes (13 measured corpus flips, see Blast Radius) — not on the grounds that "the rung was already going to fire
this way regardless."

### The concurrency counter-argument does not hold, verified against Go's actual executor — unchanged, not
### implicated in the NAK

**[GO]** `executeInJoin` (`executor_new_plans.go:1060`): `FlatMapPipelinedWithCheck(..., pipelineSize=1)` —
sequential.

**[GO]** `executeInUnion`'s merge path (`mergeSortCursor.OnNext`, same file, ~1425-1429) pulls every leg with a
blocking `s.cursor.OnNext(ctx)` in a plain `for` loop — also sequential, despite the comment above it citing
Java's async `whenAll` semantics. Zero concurrent FDB range reads across IN-union legs in Go today. This claim
was **not** implicated in the NAK and is carried forward unchanged: whatever concurrency advantage Java's async
client gives real InUnion execution, Go's synchronous merge cursor doesn't have it, so there is nothing to trade
away by preferring InJoin here.

### Two existing corpus comments assert the opposite by reasoning, not measurement, and the reasoning is wrong —
### unchanged, origin now fully traced

`in_list_pushdown.yaml:74-78` and `in_list_index_plan.yaml:56-60` both assert, without a Java citation, that
"InJoin gives no cross-value order" / "InJoin provides no cross-value order." **[JAVA]**
`SortedInValuesSource.java:58-61` sorts its values at construction, and `OrderingProperty.visitInJoinPlan` derives
a real ordering from exactly that. The premise is false; both pins move. **[GO]** `git log -S` traces the wording
to two commits, not one: `ec72b8e3f` originated it in `in_list_index_plan.yaml` (RFC-181 P0.3, the leg-pinning
rewrite of the union rules); `cebcbd94b` (RFC-190 190.2, the cost-comparator transitivity fix) then **copied** the
same false premise verbatim into `in_list_pushdown.yaml`, its own comment explicitly citing
"Mirrors the already-documented rule at in_list_index_plan.yaml:56-60" — i.e. the second commit propagated the
first's unverified claim rather than independently re-deriving it. Neither commit cites Java. Both pins are
un-Java-verified and both are expected to move once (a)/(b) land — not implicated in the NAK, carried forward.

## Blast radius — corrected numbers

**Revision 3 update — the (c) blast-radius risk below never materialized.** It was written against revision 2's
proposed fix (extend `expression_partition.go`'s shared partitioning machinery), which is not what shipped. CQ-20's
actual fix touches only `PKScanOrdering` in `ordering.go` — none of the eight consumer files listed below were
touched, and the corpus diff (26/2633 changed, 0 regressions) confirms no blast radius beyond the two IN-rules.
Kept below as the historical record of the risk that was flagged and correctly not taken.

**Revision 3 correction — the defect-(b) bail count changed because of CQ-20, and the previously-cited "0" ceiling
after a fix was never actually measured.** Revision 2's `196/196 (100%)` bail figure (below) was measured
*pre-CQ-20*. Post-CQ-20, the partition-key fix surfaces more Fixed-bound scans as distinct partition members,
each producing its own call into `enumerateSourceOrderingsForRequestedOrdering`'s requested-ordering arm — the
call count into that arm rose to **238**, still **238/238 (100%) bail** at the identity-lookup line before any
fix. **[MEASURED — this revision]**, prototyping (b)'s accessor-path bridge fix to measure the ceiling: the bail
rate drops to **40/238 (17%)**, not to zero as an earlier, informal citation of this number claimed. The 40
residual bails are **uncharacterised** — not yet root-caused, listed under Undetermined rather than guessed at.

**[MEASURED — reviewer]**, against current HEAD (not the stale base revision 1 used):

- **62** total `inJoinCount`-rung decisions corpus-wide.
- **25** corpus queries carry `InUnion` in the winning plan on current HEAD (revision 1's "~30" projection and
  its own "13, stale base" measurement are both superseded by this figure — neither is the current-HEAD count).
- Of those 25, the `inJoinCount` rung fires for **15 (60%)**; the remaining 10 are decided at rungs above it
  (see chain-position correction below).
- Row correctness, memo invariants, and plan-shape diffing were re-run against current HEAD by the reviewer as
  part of the NAK sweep; no regression reported in that pass.

**[PROJECTED — required before this RFC leaves Draft]**: a fresh, current-HEAD, non-stale-base plan-shape diff
specifically for the *revised* (three-fix) design once implemented — revision 1's own "18 shape flips / 13
InUnion→InJoin / 4 full-scan→InJoin / 0 InMemorySort→InJoin / 1 NestedLoopJoin-operand-flip" breakdown was against
a stale base and is not re-asserted here as current.

**[NEW — required before implementation]**: the revised design's defect (c) fix touches shared partition
infrastructure (`expression_partition.go`'s `orderingsEqual`/`orderingPartitionHash`/`toPartitionsFromMap`, or
whatever narrower mechanism implementation settles on — see Design decisions), which is consumed by **eight
other files**, not just the two IN-join/IN-union rules:

```
rule_implement_simple_select.go
rule_implement_sort.go
rule_implement_distinct_union.go
rule_implement_distinct_final.go
rule_implement_unordered_union.go
unified_tasks.go
```

Making the partition key binding-aware can only ever *split* existing partitions further (never merge), which is
directionally safe (it can't newly co-partition anything that wasn't already co-partitioned), but it can change
which partitions these other six call sites iterate over and in what count/order. **This must be corpus-diffed
specifically for these six files' behavior, not just for InJoin/InUnion's own output**, before this RFC leaves
Draft. This risk did not exist in revision 1's four-call-site-local design and is new to this revision's
narrower, more Java-aligned three-fix design — flagged rather than hidden.

## NestedLoopJoin operand-order flip in `cte_error_codes.yaml` — chain-position citation corrected

Revision 1's "Claim 3" argued Go's `inJoinCount` rung sits "in the same position in the chain" as Java's,
citing its placement between criteria #12 and #14 in `DIVERGENCES.md`'s table. **That is true for criterion #13
in isolation** (`DIVERGENCES.md:215`: `13. InJoin count (more=better) | ... | Aligned`), but the surrounding
argument over-read it as meaning Go's *execution order* tracks Java's numbering generally. It doesn't, and the
RFC's own NestedLoopJoin discussion undercuts it:

**[GO]** `planning_cost_model.go:261`:

```go
if cmp := compareJoinOrdering(a, b, stats, ctx); cmp != 0 {
	return cmp
}
```

This is criterion #15 (`FlatMap join ordering` / join-order costing) by DIVERGENCES.md's own numbering, but it
executes at line 261 — immediately after recursive-CTE decomposition (#5) and well **before** `inJoinCount` (#13)
at line 354, and before the IN-plan SARG penalty (#6, line 265) and primary-vs-index (#7, line 269) too.
**[GO]** `DIVERGENCES.md:217` documents this explicitly: `15. FlatMap join ordering | ... | compareJoinOrdering,
hoisted after recursive CTE... | **Documented Go broadening/order divergence**`. Revision 1 read "Aligned" in the
#13 row and, without checking the adjacent #15 row, generalized it into a claim that row directly contradicts.

**[MEASURED — reviewer]**: corpus decisions for the InUnion-vs-InJoin comparisons that don't reach `inJoinCount`
land at rungs *above* #13 in Go's actual execution order (residual-predicate-count, primary-vs-index), consistent
with #15's hoist and with the 60%-not-100% `inJoinCount`-reached figure above.

The shadowed-CTE cross-join flip itself (`cte_error_codes.yaml`, 0-indexed entry 5) — **[MEASURED]**, rung-traced
by the reviewer — decides at the deterministic structural plan-hash tiebreak (criterion #17) once InJoin no
longer needs a wrapping sort on either operand order, both earlier rungs tying. Two things make this a non-issue:
(1) `DIVERGENCES.md`'s own criterion-17 entry already documents Go's structural hash as explicitly not required
to byte-match Java's — an already-accepted divergence category, not a new one; (2) the query is a plain `INNER`
cross join with an `unordered: true` full-cross-product row assertion, so operand order has zero effect on the
result set.

## Design decisions

### Revision 3: (c) already shipped; (a)+(b) ship together once the Graefe ruling picks (i) or (ii)

Revision 2 proposed three fixes shipped atomically. **(c) is done** — shipped as CQ-20, via the narrower
source-property fix described above, not via either option this section originally weighed. What remains is (a)
and (b), which still need to ship together (see Rollout), but **cannot ship at all until the design-decision
ruling above lands**, because landing them unguarded would flip the 2 known FETCH-required exceptions
(`in_over_primary_scan_sarg.yaml#14,#15`) the wrong way. The two remaining fixes:

1. **(a)** Port `RecordQueryInJoinPlan.HintOrdering` to Java's `OrderingProperty.visitInJoinPlan` algorithm.
2. **(b)** Add a public `*RichOrdering` accessor (e.g. `BindingsForValue`) that runs the same
   `orderingKeyFor`/`bindingMapForExplain` bridge `Satisfies` uses internally, and switch
   `enumerateSourceOrderingsForRequestedOrdering`'s raw map lookup to use it. Verify at implementation time
   whether `adjustBindingsForInUnion`'s `reqMap[val]` lookup (`rule_implement_in_union.go:424`) needs the
   identical bridge — the partial-PK-prefix trace above shows it landing on `ChooseBinding()` rather than an
   explicit promotion, consistent with (but not conclusively proven to be caused by) the same Value-identity gap.

**A third piece is now required alongside (a)+(b), and its shape depends entirely on the ruling**: either the
fetch-accounting/fetch-depth/rule-asymmetry machinery for option (i), or the "just extend the fetch-push rule"
change for option (ii) (see "What Go would need for option (i)" above — option (ii) is comparatively small, since
it only adds a fetch-push rule for the comparand-equivalent Go shape rather than also reproducing an asymmetry).
Neither is specified further here; that is implementation-time work gated on the ruling, not on more research.

**(c)'s original design-decision text (options (i)/(ii) for the partition key, and the dropped defect-(d) fix) is
retained below as the historical record of what was considered before the narrower `PKScanOrdering` fix shipped.**

**[Historical, retained]** (c) [merged, was (c)+(d)]: make the plan-partition key binding-aware, so a Fixed-bound
member and a Sorted/unbound member sharing the same key-and-direction shape land in different partitions —
mirroring Java's `Ordering.equals`, which always compares the binding map because Java has one `Ordering` type,
not two. Two options were weighed: (i) derive the partition key from `computeWrapperRichOrdering` instead of
`computeWrapperOrdering`/`HintOrdering`, extending `orderingsEqual`/`orderingPartitionHash` to compare/hash each
key's binding kind (Fixed vs. Sorted-direction vs. Choose), or (ii) a narrower variant that only changes the two
IN-rules' own partition consumption without touching the shared `expression_partition.go` machinery. Neither
shipped — the fix instead trimmed the lossy input at its source (`PKScanOrdering`), which needed neither option's
machinery. Recorded so a future reader doesn't wonder why this section discussed options the eventual fix doesn't
use.

**[Historical, retained] Dropped**: the original defect-(d) fix (copy InUnion's `pinOrderedSpine` +
`expressions.FinalOf`-single-member pin to InJoin). Per the merged-defect discussion above, once (c) makes the
partition key correct, InJoin's existing `MemoizeFinalExpressionsFromOther`-whole-partition approach is
Java-aligned and doesn't need it. This reasoning held regardless of which of the three (c) implementations
shipped, and remains correct post-CQ-20.

### InUnion's own first-member pick and `pinOrderedSpine`/`FinalOf` mechanism: not touched by this RFC, flagged
### for a future look

InUnion's second-stage `memberSatisfiesOrdering` scan + `pinOrderedSpine` + `FinalOf` (lines 305-342) is InUnion's
*own* pre-existing mechanism, not something this RFC is adding. With (c) fixed, it may become partially redundant
(the first-member pick it works around becomes safe), but auditing whether it can be simplified is a separate,
larger question this RFC does not need to answer to fix the reproducer — flagged as a candidate follow-up, not
in scope here.

## Rollout: (a)+(b) ship together, gated on the design-decision ruling — (c) already shipped separately

(a) alone is inert — nothing calls `HintOrdering` on a `RecordQueryInJoinPlan` usefully without (b) also letting
the requested-ordering enumeration reach a real candidate. (c), which used to gate both, is done (CQ-20). **What
changed this revision: (a)+(b) can no longer ship the moment they're implemented and tested** — they must ship
*with* whichever fetch-accounting/rule-asymmetry mechanism the Graefe ruling selects (option (i) or (ii) above),
because an unguarded landing regresses the 2 known FETCH-required exceptions. The rollout order is therefore:

1. Graefe ruling on (i) vs (ii) (blocking — nothing below can start implementation without it, though the
   research/measurement in this RFC is complete either way).
2. Implement (a)+(b) plus the ruled-on third piece, together, in one PR.
3. Milestone-level Graefe+Torvalds review of that PR (the query-engine gate), plus codex and @claude, per this
   repo's standard process — unchanged from revision 2's framing, just re-scoped to two fixes instead of three
   since (c) already had its own review pass as part of CQ-20.

## Risks

- **This RFC cannot proceed to implementation without the Graefe ruling above.** Unlike revision 2's risks, this
  is not "unmeasured, must be measured before Draft exit" — the measurement is done (the live-Java mechanism, the
  23/25 confirmation, the 2 exceptions). What's missing is a decision on measured facts, which only the ruling can
  supply.
- **Landing (a)+(b) without the ruled-on third piece regresses `in_over_primary_scan_sarg.yaml#14,#15`** — the 2
  Java-confirmed exceptions where Go's current `Fetch(InUnion(...))` already matches Java. This is the concrete,
  measured consequence of skipping the design decision, not a hypothetical.
- **The blast-radius risk from (c)'s original shared-infrastructure design never materialized** — CQ-20 shipped
  the narrower source-property fix instead, touching only `ordering.go`. Retained above as historical record, not
  as an open risk.
- **The InUnion→InJoin flips (25 corpus queries carry InUnion in the winning plan on current HEAD; 60% already
  reach the `inJoinCount` rung) remain the biggest single behavior-visible surface area.** Mitigated as in
  revision 1: Go already has Java's rung and its logic is a faithful port; the crux is a measured preference
  change, not an inevitability — every flipped corpus pin still needs individual re-blessing with its
  Java-verification status recorded. 23/25 of the prototyped flips now carry that status (Java-confirmed correct);
  the 2 that don't are exactly the shapes the ruling gates.
- **The NestedLoopJoin operand flip's mechanism (structural hash tiebreak, criterion #17) is confirmed, not
  guessed** — carried forward from revision 1, unaffected by the NAK or by this revision.
- **Defect (b)'s fix is now the most load-bearing single change among (a)/(b)** — it is the 100%-of-calls (238/238
  pre-fix) blocker for InJoin; getting the accessor-path bridge wrong (e.g. reaching for semantic equality instead)
  silently reintroduces this exact bug. The prototyped bridge still leaves 40/238 (17%) bailing for an
  uncharacterised reason — implementation must root-cause those 40, not just declare victory on the 198 it fixes.
- **Existing pinned corpus scenarios move** — `in_list_pushdown.yaml:74-81`, `in_list_index_plan.yaml:56-62` (the
  false "InJoin gives no cross-value order" comments, now traced to two commits, `ec72b8e3f` and `cebcbd94b`),
  `in_over_primary_scan_sarg.yaml` (2 of these must NOT move per the ruling gate above — the other entries in this
  file may), `in_plan_winner_stability.yaml:101-102` (un-Java-verified InUnion pin, best candidate for the live
  conformance-harness check). Carried forward from revision 1, all still expected to move except the 2 gated
  exceptions.

## Undetermined — listed honestly, not guessed at

- **The 40 residual defect-(b) bails** (out of 238, post-prototype-bridge) are uncharacterised. Root-causing them
  is implementation-time work; this RFC does not know whether they share one mechanism or several.
- **Why the bounded-covering InUnion leg is unreachable for DESC specifically** is not traced. A connection to
  the descending/`ToOrderedBytesValue` fragility documented elsewhere in this codebase is plausible (both are
  DESC-specific index-scan-direction issues) but unproven — no measurement in this revision follows the DESC
  InUnion candidate-generation path itself.
- **Whether Java would elect InJoin or InUnion for a single-column DESC FETCH-required shape** is not measured —
  the DESC trace above covers the reproducer's own (non-FETCH-required, primary-key) shape; the FETCH-required
  exceptions (`in_over_primary_scan_sarg.yaml#14,#15`) were only measured for ASC.

## Test plan

- **Red→green unit tests** for each fix in isolation where practical: `HintOrdering` on a sorted vs unsorted
  `RecordQueryInJoinPlan`; the requested-ordering enumeration finding a Fixed binding through the bridge for a
  baked vs lazy `Value`; the partition-key fix separating a Fixed-bound scan from a Sorted-unbound scan that
  previously co-partitioned.
- **[NEW — required by the NAK] A binding-heterogeneous-partition test**: construct (or find/adapt a corpus
  query producing) a partition holding both a Fixed-bound member and a Sorted/unbound member over the same
  key-and-direction shape, and assert that whichever downstream consumer picks a representative member (InJoin's
  and InUnion's `richOrdering` pick, and any of the six other `ToPlanPartitions` consumers this RFC's blast-radius
  diff touches) yields the **bound** member when a promotable ordering is being derived — not whichever happens
  to iterate first. This is the direct regression test for the merged (c) defect; it must exist independent of
  the reproducer, because the reproducer alone doesn't distinguish "partition key fixed" from "call site got
  lucky."
- **The reproducer itself**, both `ORDER BY id DESC` (partial-PK-prefix, the shape that currently degenerates)
  and `ORDER BY id DESC, k DESC` (full-PK, the shape that currently "works" for the wrong reason) — as `EXPLAIN`
  assertions (the sorted InJoin/InUnion chain, no `InMemorySort`, no full reverse scan) and as an FDB integration
  test asserting row order end-to-end with IN-list values in non-monotonic literal order (`IN (2, 1, 3)`). Both
  variants matter: the second currently passes today for reasons this RFC's own investigation shows are
  accidental (a lucky escape-hatch partition), and a regression that removed the reverse-variant enumeration
  feature without this RFC's partition-key fix landing first would silently make it fail the same way the
  first variant does today — the test must pin the *mechanism* (which partition/candidate wins), not just the
  row order, or `plan_contains` assertions on both shapes.
- **yamsql corpus**: re-bless every moved `plan_contains`/`plan_not_contains` pin with `rows:` still asserting
  correct data; correct the false "InJoin gives no cross-value order"/"provides no cross-value order" comments in
  both `in_list_pushdown.yaml` and `in_list_index_plan.yaml`.
- **`cte_error_codes.yaml` scenario 5**: comment explaining the hash-tiebreak mechanism, keeping the existing
  `rows:` assertion (unordered, full cross-product) as the correctness net.
- **`in_over_primary_scan_sarg.yaml#14,#15` must NOT move** — an explicit negative test (these two scenarios keep
  their current `Fetch(InUnion(...))` pin) is the regression guard for the design-decision ruling actually being
  honored in the implementation, not just decided on paper.
- **[SUPERSEDED — moot]** The six-other-`ToPlanPartitions`-consumer blast-radius diff this section originally
  required for (c) is no longer applicable to (a)+(b)'s scope — (c) shipped without touching
  `expression_partition.go` at all, and the CQ-20 corpus diff (26/2633, 0 regressions) already stands as that
  diff's answer. (a)+(b) do not touch partition-key computation, so this item does not recur for them.
- **Whichever option the ruling picks** needs its own test: for (i), a test that Go's fetch-depth/implicit-fetch
  rung reproduces the measured Java ASC tie-break (InUnion's depth-0 fetch beats InJoin's depth-1 implicit one)
  and that the FETCH-required exceptions keep their current winner; for (ii), a test that the extended fetch-push
  rule fires for the comparand-equivalent Go shape and that the resulting `Fetch(InJoin(Covering(...)))` plan is
  row-correct end-to-end against real FDB.
- **Stress test 1M** before/after per this repo's standard workflow.
- **Determinism check**: run the reproducer and 2-3 sample flips 10x each, since this touches partition
  membership and iteration order directly.

## Addendum: verification summary — revision 2

- **Retracted**: the crux ("the rung is unreached, this RFC merely unblocks it") — measured false: 62 rung
  decisions corpus-wide, InJoin already wins real head-to-head comparisons today. Replaced with a measured
  preference-change argument.
- **Retracted**: defect (c)'s described firing mechanism for InJoin (`sortOrder.IsDirectional()` returning true)
  — measured 0/196; defect (b)'s raw map lookup fires 196/196 and (c) never gets a chance to matter for InJoin as
  it stands today.
- **Merged**: defects (c) and (d) into one re-diagnosed root cause — a binding-blind plan-partition key
  (`properties.Ordering`/`HintOrdering`, not `orderingsEqual`'s comparator body) — traced to its source, confirmed
  live for InUnion (not merely inert-and-latent like it is for InJoin today), and confirmed by Java source
  (`Ordering.equals` always compares the binding map because Java has one `Ordering` type).
- **Dropped**: the original defect-(d) fix (`expressions.FinalOf`-single-member copy). The NAK's own finding —
  Java's `ImplementInJoinRule` also memoizes the whole unpinned partition — refutes it once the partition key
  itself is fixed; copying InUnion's pin mechanism to a second site is no longer needed and would have entrenched
  a Go-only workaround.
- **Investigated per the NAK**: the partial-PK-prefix boundary. Directly measured, not projected: it is
  incidental (an unrelated reverse-ordered-variant-enumeration feature manufactures a lucky singleton partition
  for the full-PK case), not a fourth defect. Does not collapse the design below three fixes; strengthens the
  case for fixing the partition key rather than patching call sites.
- **Corrected**: "Claim 3" — Go's `inJoinCount` rung is Aligned in isolation, but the surrounding chain-position
  claim ("matching Java's placement") was over-read; `compareJoinOrdering` (#15) is hoisted to run at line 261,
  well before `inJoinCount` (#13), a divergence `DIVERGENCES.md:217` already documents.
- **Corrected**: blast-radius numbers — 25 corpus queries carry InUnion in the winning plan on current HEAD
  (superseding both revision 1's "~30" projection and its own stale-base "13" measurement); 60% of those already
  reach the `inJoinCount` rung.
- **New finding, not in revision 1**: fixing (c) at the shared-infrastructure level (the preferred option) has a
  blast radius beyond InJoin/InUnion — six other partition consumers — that must be corpus-diffed before this RFC
  leaves Draft. This is a genuinely new risk introduced by the more Java-aligned design, not hidden or
  downplayed.
- **Confirmed, unaffected by the NAK**: the concurrency counter-argument (Go's InUnion merge is fully sequential,
  same as InJoin); the two false "InJoin gives no cross-value order" corpus comments (`git log -S` traces them to
  a Go-side refactor, never Java-verified); the NestedLoopJoin flip's mechanism (structural hash tiebreak,
  criterion #17, an already-documented divergence category).
- **Still not measured, flagged rather than guessed**: the live Java conformance-harness (`SqlPlanSteps`) check
  for the ascending case; a precise current-HEAD blast-radius diff for the *revised* three-fix design (only the
  reviewer's pre-revision numbers exist so far); the six-consumer partition-key blast-radius diff; whether
  `adjustBindingsForInUnion`'s `reqMap[val]` lookup needs the same accessor-path bridge as (b) (observed landing
  on `ChooseBinding()` in the full-PK trace, consistent with but not conclusively proven to share defect (b)'s
  cause).

  *(Revision 3 note: the "live Java conformance-harness check for the ascending case" and the "six-consumer
  partition-key blast-radius diff" items above are both now resolved — see below — and retained here only as the
  historical record of what revision 2 flagged as outstanding.)*

## Addendum: verification summary — revision 3

- **Status change**: the full-table-scan regression this RFC was originally written against is **CLOSED**,
  shipped as CQ-20 (`8e12a1b59` + `da2c6d57b`), outside this RFC's own gate (Graefe classified it as a narrow,
  isolable fix during the CQ-20 review). This RFC is now scoped to defects (a)+(b) only — a **parity
  enhancement** (Java plans no sort; Go still plans one), not a regression fix.
- **(c) resolved**: what shipped is narrower than either option revision 2 weighed — a source-property fix
  (`PKScanOrdering` trims its equality-bound PK prefix) rather than a shared-partition-infrastructure change.
  Revision 2's diagnosis of the root cause was correct; its two proposed fix shapes were both superseded by a
  third, simpler one. The blast-radius risk revision 2 flagged for (c) never materialized, since the shared
  `expression_partition.go` machinery was never touched.
- **New evidence class**: a live Java planner instrumented with a `MemoTraceProbe` (candidate-level, not just
  final-winner visibility) — reusable, documented in full above so it doesn't have to be rebuilt.
- **Two hypotheses refuted by the live trace**: Java does enumerate an ordered comparand InJoin candidate for the
  FETCH-required shape (it loses a genuine cost comparison, isn't eliminated); the ASC decision happens at the
  fetch-depth rung, one step before `numSourcesInJoin`, not at `numSourcesInJoin` itself.
- **Mechanism located, file:line**: `PlanningCostModel.java:213` (implicit-fetch counting for
  `RecordQueryPlanWithIndex`), `:220-226` (fetch-depth tiebreak), `:231-236` (explicit-fetch-only counting),
  `:254-262` (`numSourcesInJoin`); `PlanningRuleSet.java:152-153` vs `:160` (the `PushInJoinThroughFetchRule`
  registration asymmetry that is this revision's central finding); `CascadesPlanner.java:389,403-407` (the
  3-column `UnableToPlanException` shapes, confirmed to throw rather than silently degrade, and therefore not
  usable evidence for the design decision).
- **DESC traced to the same mechanism with a different outcome**, and one thing explicitly left unresolved:
  Java's DESC InUnion candidates never wrap the bounded-covering leg, only the unbounded-reverse one — this
  revision does not know why, and does not guess.
- **New finding requiring a decision this RFC cannot make**: `PushInJoinThroughFetchRule`'s exclusion of
  `RecordQueryInComparandJoinPlan` looks like a Java oversight, not a deliberate design choice. Go must choose
  between reproducing it faithfully (option (i)) or deliberately diverging (option (ii)); both are presented with
  consequences, neither is picked — flagged for a Graefe ruling. This is what makes (a)+(b) blocked-pending-ruling
  rather than dropped: the research is complete, only the choice is outstanding.
- **23 of 25 previously-prototyped corpus flips are Java-confirmed correct** against the live trace. The 2
  exceptions (`in_over_primary_scan_sarg.yaml#14,#15`) are the FETCH-required shapes where Go's current behavior
  already matches Java — the concrete case that makes the design decision load-bearing, not academic.
- **Corrected numbers**: defect (b)'s bail rate is **238/238 (100%) before a fix, 40/238 (17%) after** a
  prototyped accessor-bridge fix — not the "0" an earlier informal citation claimed, and not revision 2's
  pre-CQ-20 196-call baseline (the call count itself changed because CQ-20 surfaces more distinct partition
  members). The 40 residual bails are uncharacterised, not zero and not explained.
- **False corpus comments, origin now fully traced to two commits**: `ec72b8e3f` originated the false "InJoin
  gives no cross-value order" claim in `in_list_index_plan.yaml`; `cebcbd94b` copied the same unverified claim
  into `in_list_pushdown.yaml`, citing the first as its source rather than independently re-deriving it. Neither
  commit cites Java.
- **`ImplementInUnionRule` does not share defect (b)** — its `bakeMergeComparisonKeys` already routes through the
  `ColumnNameValue`/`orderingKeyFor` bridge defect (b) is about adding to InJoin. This is why InUnion's flips are
  reachable today without waiting on (b), and it sharpens defect (b)'s scope to InJoin specifically.
- **Undetermined, listed rather than guessed**: the 40 residual defect-(b) bails; why DESC's bounded-covering
  InUnion leg is unreachable; whether Java would elect InJoin or InUnion for a single-column DESC FETCH-required
  shape (not measured — the DESC trace covers only the reproducer's non-FETCH-required shape).
