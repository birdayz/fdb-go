# RFC-183 — Rules yield fully-linked plans: delete the nil-inner shell architecture

**Status:** Draft — needs Graefe + Torvalds ACK (query-engine architecture).
**Tracks:** RFC-167's deeper layer (the nil-inner-shell finding), promoted to its own design at the owner's request after RFC-182 P2 patched a fourth repair site.
**Relates to:** RFC-070 (deferred child linkage — the origin), RFC-076 (`findBestPhysicalPlan`), RFC-182 (the harness that keeps surfacing this class).

## 1. Problem

Go's Cascades has a state Java cannot represent: a physical plan node whose
child is **nil**, to be filled in later by a `WithChildren` relink at
extraction. These are "shells", and they are the root cause of a recurring
family of defects. RFC-182's grammar extension alone surfaced four:

| Symptom | Repair that shipped |
|---|---|
| `PredicatesFilter(<nil>)`; before compensation existed, the same queries silently **dropped a residual** (wrong rows) | set ops added to `isLeafReplaceable` |
| `InUnion(PredicatesFilter(<nil>))` | a relink added to a wrapper that had none |
| nil-inner shells selected as valid plans | shell guard generalized from Fetch-only |
| `InJoin(<nil>)` on nested `IN`s | `shouldRelinkInner` + recursive `completeShellPlan` |

Each fix is local and correct; none removes the category. The pattern is
diagnostic: **we are repairing plans after the fact instead of never
building broken ones.**

## 2. Java: the property we lack, and why it holds

Java has no shell window. Verified against 4.12.11.0 (citations from the
port research):

1. **A plan IS an expression, and its child is a Quantifier.**
   `RecordQueryFetchFromPartialRecordPlan.java:84` holds
   `private final Quantifier.Physical inner`; `:142` `getChild()` returns
   `inner.getRangesOverPlan()`. `Quantifier.java:467` declares
   `@Nonnull private final Reference rangesOver`, assigned in a private
   constructor; the only factories (`:611-622`) all require a `Reference`.
   There is no `setRangesOver` anywhere in `plan/`. A child is a
   *dereference through a non-null final reference*, not a stored pointer
   that can be nil.
2. **Rules memoize the child BEFORE constructing the parent.**
   `PushFilterThroughFetchRule.java:197-225`:
   `Quantifier.physical(call.memoizePlan(innerPlan))` is evaluated as a
   constructor *argument*. No window exists in which the parent lacks its
   child.
3. **Every yield asserts it.** `CascadesRuleCall.java:221-241`
   `verifyChildrenMemoized` runs an unconditional `Verify` (not a debug
   check) that each quantifier's reference is already in the traversal.
4. **Copy-on-write everywhere else.** `withChild(Reference)` returns a new
   node (`RecordQueryPredicatesFilterPlan.java:161-164`); the only mutable
   state is the member set *inside* a group, never the parent→child edge.

So Java's guarantee is structural, then asserted, then preserved. Go has
none of the three.

## 3. What the shells cost us today

Constructs that exist ONLY because a child can be nil:

| Construct | Location |
|---|---|
| `isNilInnerShell` / `isNilInnerFetch` | `physical_fetch_from_partial_record_wrapper.go:175,192` |
| `completeShellPlan` / `resolveInnerPlan` / `planWithInner` / `maxShellCompletionDepth` | `physical_wrapper.go:326-407` |
| `findPhysicalPlan` / `findBestPhysicalPlan` / `extractChildPlanFromQuantifier` | `physical_wrapper.go:302-435` |
| `shouldRelinkInner` / `planIsShell` / `isOrderDestroying` / `isLeafReplaceable` | `physical_wrapper.go:467-571` |
| `pinOrderedSpine` / `planHasDirectChild` + its extraction twin | `winner_lookup.go:90-165`, `properties/extract.go:360-412` |
| template-aware costing & hashing (`planNodeIsStub`, `exprConcreteHash`, …) | `planning_cost_model.go` (~250 lines) |
| the winner-clearing exemption for shells | `unified_tasks.go:503-521` |

**16 relink sites** across 11 wrapper files plus 5 inline. Roughly 600
lines whose only purpose is coping with a state Java cannot express — and
every one is a place the next bug can hide.

## 4. The surprise: only 6 rules create shells

`rule_push_filter_through_fetch.go:91,101` ·
`rule_push_map_through_fetch.go:94` ·
`rule_push_distinct_through_fetch.go:66,76` ·
`rule_push_distinct_below_filter.go:86` ·
`rule_push_in_join_through_fetch.go:80,100` ·
`rule_push_set_operation_through_fetch.go:388`

All are push-through rules; all have a Java counterpart that memoizes the
child first. And critically, **the Go rules already hold the concrete child
expression at yield time** (e.g. `rule_push_filter_through_fetch.go:85`
`fetchInnerExpr := findPhysicalExpr(fetchInnerRef)`).

The only thing stopping them passing it is a comment
(`physical_fetch_from_partial_record_wrapper.go:160-170`) asserting that
setting the inner at rule time "would introduce stale plan references (the
inner's own children haven't been extracted yet)". That rationale is
Java-invalid — Java dereferences through the reference, so staleness cannot
arise — and it appears never to have been retested. `winner_lookup.go:88-89`
already concedes the point in passing: *"Java bakes concrete children at
rule time via memoizePlan, so it has no unpinned window at all."*

Note also that the data-access path is **not** a shell source:
`abstract_data_access_rule.go:476-486` already builds a fully-linked fetch.
The cost model's claim that "the data-access path builds chains of these
shells" (`planning_cost_model.go:1227`) is stale and should be corrected
regardless of this RFC's outcome.

## 5. Plan

**P0 — falsify the premise (cheap, decisive).** In the 6 push rules, pass
the concrete child plan already in hand instead of `nil`; leave quantifiers
untouched. Add a temporary assertion that `isNilInnerShell` never fires for
those rules. Run the full suite, the yamsql corpus, the rowdiff harness and
the 1M stress.
*Outcome A:* clean → the stale-reference fear is folklore, and P1-P4 become
a deletion exercise.
*Outcome B:* a shape regresses → we have a concrete, reproducible reason,
and P5 becomes mandatory rather than optional.
**No further phase starts before P0 reports.**

**P1 — install Java's invariant.** Make `MemoizeFinalExpression` actually
register into the memo/traversal (today it returns a bare `InitialOf` —
`implementation_rule.go:132`), then port `verifyChildrenMemoized` into
`Yield`, plus a debug-mode rejection of any yielded shell. Add
`Verify(len(finalMembers)==1)` to `AdvancePlannerStage` and a
`GetOnlyElementAsPlan`, matching `Reference.java:208-212,236-239`.

**P2 — delete the relinks.** With no shells produced, the 16 relink sites
become pure structural rebuilds; `shouldRelinkInner`, `isLeafReplaceable`,
`isOrderDestroying`, `planIsShell`, `findPhysicalPlan`,
`findBestPhysicalPlan`, `completeShellPlan`, `resolveInnerPlan`,
`planWithInner` all go. `WithChildren` takes Java's `withChild(Reference)`
shape.

**P3 — delete template-aware costing/hashing** and the winner-clearing
exemption. They exist only to cost and hash a tree with holes.

**P4 — delete the pins.** `pinOrderedSpine` / `planHasDirectChild` /
`planEmbedsDirectChild` exist only because a wrapper's quantifier and its
embedded plan can disagree. Once they cannot, ordering delegation is
ordinary rule-time memoization.

**P5 (optional, deep) — unify the hierarchies.** Make
`plans.RecordQueryPlan` a `RelationalExpression` holding quantifiers,
deleting all 23 `physical_*_wrapper.go`. This is Java's actual shape and
makes the bug class *unrepresentable* rather than merely absent. Large;
touches the executor. Only justified if P0 lands in Outcome B, or if the
wrapper layer keeps generating defects after P4.

## 6. Riskiest unknowns

1. **Is "stale plan references" ever true?** P0 exists to answer this
   before anything is deleted.
2. **Cost/hash coupling.** Shells collapse to identical hashes today
   (`planning_cost_model.go:1971`); removing them changes hashes and costs
   and can flip winners. Requires the 1M stress before/after plus an
   EXPLAIN-diff sweep over the yamsql corpus.
3. **Winner-selection timing.** Baking at rule time freezes the child
   choice earlier. Java tolerates this because
   `ImplementIntersectionRule.java:91-98` memoizes a plan *partition* — a
   set — so the group keeps alternatives. Go's `findBestPhysicalPlan`
   exists precisely because "first physical member" was wrong (RFC-076).
   The port likely needs one yielded alternative per candidate child: a
   real search-space expansion that must be measured, not assumed.
4. **Multi-child wrappers.** `completeShellPlan` handles unary only;
   `rule_push_set_operation_through_fetch.go` builds N-leg shells. Whether
   every leg has a resolvable concrete plan at rule time is unverified.
5. **Always-on vs debug-only invariants.** Java splits enforcement between
   unconditional `Verify` and `Debugger.sanityCheck`. Go has no `Debugger`
   equivalent, so which invariants are always-on is an open design question
   — and the wrong answer is a production panic.

## 7. Success criteria

- `grep -c "nil-inner\|shell"` in `cascades/` trends to zero, and
  `isNilInnerShell` has no production callers.
- The RFC-182 harness runs clean at ≥50k seeds across the full grammar,
  including the shapes that produced findings 1-4.
- 1M stress within noise of the pre-change baseline.
- DIVERGENCES.md's "repair-at-EXTRACTION" section is **deleted**, not
  amended — the debt is gone rather than re-described.
