# RFC-235: A structural predicate cannot filter a row

**Status:** PHASE 1 IMPLEMENTED (the primitive and the invariant). PHASE 2 (the
peel) is designed, measured and NOT implemented — see §11. Both gates NAK'd v1;
findings folded in §10.
**Base:** branch `nlj-existential-peel`, forked at `e24f338e7`
**Relates to:** RFC-190 (retired the NLJ N-way arm), RFC-197, RFC-200, CQ-68,
CQ-96, CQ-97; `DIVERGENCES.md` "Sibling leg aliases stay bound at runtime
(`bindMergedOuterLegs`) — Go-only namespace widening, INTERIM"
**Wire impact:** none. No key, record, index, continuation or plan-proto
encoding changes. Read-side planner only.

## 1. Decision

Port Java's `QueryPredicate.toResidualPredicate()`, wire it into the call sites
Java wires it into, and make "a filter may only carry predicates a row can
answer" an enforced invariant rather than a thing each rule must remember.

Deleting the two Go-only guards in `PartitionSelectRule` that exist only because
the primitive was missing is the SECOND phase, and it is deliberately not in this
one. §11 says why, with the measurement.

The 6,532-line `rule_implement_nested_loop_join.go` — Java's is 331 — is not a
join-lowering problem. It is the downstream cost of one absent primitive. The
causal chain is measured end to end in §3, and the primitive is at the top of
it.

This RFC does **not** delete `implementJoinWithExistential`. §7 measures exactly
what still depends on it and why that deletion is a second phase rather than a
paragraph in this one.

## 2. What Java does, and where Go stops doing it

Java's `ImplementNestedLoopJoinRule` matches
`selectExpression(exactlyInAnyOrder(outerQuantifierMatcher, innerQuantifierMatcher))`
(`ImplementNestedLoopJoinRule.java:98`) — **exactly two quantifiers**, always. An N-ary select never reaches it; it is
decomposed first. Go keeps a third arm at
`rule_implement_nested_loop_join.go:88-93` for `[ForEach, ForEach, Existential]`,
dispatching to `implementJoinWithExistential` — 630 lines, the longest function
in the tree.

The decomposition Java relies on is `PartitionSelectRule`, which enumerates
**every** subset of the quantifiers (`combinations(combinationQuantifierMatcher,
c -> 0, Collection::size)`, `PartitionSelectRule.java:65-66`). For
`[F1, F2, E]` the partitioning that survives its guards is `lower = {E}`:

- `uppersDependingOnLowersAliases` is empty (no ForEach depends on an
  existential), so `lowersCorrelatedToByUpperAliases` is empty;
- the result value does not reference `E` in the WHERE-EXISTS case, so
  `lowersCorrelatedToByUppers` is empty;
- both empty selects **Case 1** (`PartitionSelectRule.java:264-271`): the lower
  becomes `Select(result = LiteralValue(1), quantifiers = [E], predicates =
  [Exists(E)])` under a fresh ForEach quantifier, and the upper becomes three
  ForEach legs which recursively partition down to binary.

That single-existential lower is implemented by `ImplementSimpleSelectRule`,
whose class doc states the emitted chain exactly:

```
«inner plan»
| ON EMPTY NULL          (null-on-empty for-each)
| FIRST_OR_DEFAULT NULL  (Existential)
| FILTER <predicates>
| MAP <resultValue>
```

Go has that rule, and its existential arm
(`rule_implement_simple_select.go:96-124`) is a faithful port of the
FirstOrDefault wrap. So the peel's target already exists in Go.

Go blocked the peel from ever being produced, with two guards that have no Java
counterpart. Both are deleted by this change; the line numbers are their
locations at the fork point `e24f338e7`:

| site (pre-change) | guard | effect |
|---|---|---|
| `rule_partition_select.go:108` | `existentialCount == 1 && foreachCount <= 2 → return` | the exact `[F, F, E]` shape is never partitioned |
| `rule_partition_select.go:296` | `lowerHasExistential && !lowerHasForEach → continue` | `lower = {E}` — Java's Case-1 peel — is rejected |

The second guard's comment names the mechanism correctly and the culprit
incorrectly: *"the existential has nothing to attach to, and
ImplementNestedLoopJoinRule builds a residual `QOV(inner) IS NOT NULL` over a
FirstOrDefault that binds to nothing → the filter drops every row → silent empty
result."* The mechanism is real and reproduced below. The rule is not
`ImplementNestedLoopJoinRule`; a one-quantifier select never reaches it.

## 3. The measurement, and the root cause it found

Both guards disabled, nothing else changed. `//pkg/recordlayer/query/...` stayed
green (9/9 targets); `//pkg/relational/sqldriver:sqldriver_test` went to
**6,198 run / 6,177 pass / 21 fail**, and one failure was a wrong answer rather
than a wrong shape:

```
exists_in_on_probe_test.go:73: uncorrelated-EXISTS-in-ON rows = [], want [1|50 1|52 2|51]
```

`SELECT a.id, c.id FROM a JOIN c ON c.a_id = a.id AND EXISTS (SELECT 1 FROM d)`.
The peel fired and produced **Java's exact shape**:

```
Project([_current.ID#0, _current.ID#1],
  NestedLoopJoin(INNER,
    Map(PredicatesFilter(FirstOrDefault(Project([1], Scan(D))), [1 preds]), {_0: 1}),
    FlatMap(outer=Scan(A), inner=IndexScan(C_A_ID, [=]))))
```

Both legs are non-empty in isolation; the join carries no predicates; the result
was 0 rows with a nil `rows.Err()`. Rendering the filter's predicates instead of
their count identified it in one read:

| path | filter carries |
|---|---|
| working (the existing NLJ arm) | `[_current IS NOT NULL]` |
| peel (`ImplementSimpleSelectRule`) | `[EXISTS(_current)]` |

Java's `ImplementSimpleSelectRule` maps `QueryPredicate::toResidualPredicate`
over its non-tautology predicates when it builds the filter. Go's does not,
because:

```
$ grep -rn 'ToResidualPredicate' --include='*.go' pkg cmd conformance | wc -l
0
$ grep -rn 'GetCorrelatedTo'     --include='*.go' pkg cmd conformance | wc -l      # positive control
651
```

`toResidualPredicate` has **8 call sites across 7 files** in Java's main sources
(`QueryPredicate`, `LeafQueryPredicate`, `ExistentialValuePredicate`,
`PredicateWithValueAndRanges`, `ImplementFilterRule`,
`ImplementNestedLoopJoinRule`, `ImplementSimpleSelectRule`) and **zero**
counterparts in Go. It is not renamed; it was never ported.

An EXISTS predicate is a structural instruction to the semi-join rules. It names
the existential quantifier's alias, and once the subplan has been lowered to a
FirstOrDefault no physical row carries that alias. Evaluating it as a row filter
is UNKNOWN for every row, so the filter drops the whole stream **and reports
success**. That is not a slower plan; it is a wrong one that cannot be
distinguished from an empty table.

Go does perform the conversion — inline, at exactly one site, in the arm this
RFC is trying to make unnecessary:
`rule_implement_nested_loop_join.go:2581,2600` builds
`ComparisonPredicate(innerObject, Comparison{Type: ComparisonIsNotNull})`, which
is `ExistentialValuePredicate.toResidualPredicate()`
(`ExistentialValuePredicate.java:107-109`) written out by hand. The concept
exists in the codebase; it just has no name and one call site.

## 4. The port

`predicates/to_residual_predicate.go`, a package-level function over the
interface — the shape `IsTautology` and `ReplaceValues` already use for Java
default methods, since Go has no interface defaults. It returns
`(QueryPredicate, error)`, because Java's conversion is total only by throwing.

Three predicate types cannot answer for a row, and each has an unconditional
non-answer as its `Eval` — that shared tell is what makes them one class rather
than three cases:

| type | Eval | residual |
|---|---|---|
| `*ExistentialValuePredicate` | names an alias no physical row carries | `value IS NOT NULL`, comparison **minted** |
| `*PredicateWithValueAndRanges` | `TriUnknown`, unconditional | `OR` over ranges of `AND` over comparisons |
| `*Placeholder` | `TriUnknown`, unconditional | same, spelled explicitly — see below |

`*Placeholder` is in that table because Java gets it by inheritance and Go
cannot: Java's `Placeholder extends PredicateWithValueAndRanges`
(`Placeholder.java:48`), while Go's is a standalone struct
(`placeholder.go:24`), so a `case *PredicateWithValueAndRanges` cannot match it.
It would have fallen through to `default` unchanged — the same silent-drop trap
on a third type.

The existential's comparison is **minted**, never read off the predicate. Java is
unconditional (`new NullComparison(NOT_NULL)`), and Go must be: nothing enforces
the struct's field — `MustNewExistentialValuePredicate`
(`existential_value_predicate.go:53-58`) asserts only that the value is a
`QuantifiedObjectValue`, and `replace_values.go:286` builds the literal directly
with a *translated* comparison. An earlier draft reused the field and justified
it with a constructor assertion that does not exist.

`AndPredicate.and` / `OrPredicate.of` semantics are ported, not approximated:
empty conjunction → `ConstantPredicate.TRUE`, singleton → unwrapped
(`AndPredicate.java:189-205`), singleton disjunction → unwrapped
(`OrPredicate.java:438-444`). Go's `NewAnd`/`NewOr` are bare struct literals that
do none of this, and the difference is load-bearing: a singleton wrapper changes
the conjunct count the cost model reads (`predicates.go` `CountConjuncts`), and
an **empty `OrPredicate` evaluates to FALSE** — the very defect this file exists
to prevent, respelled. A rangeless sargable therefore returns
`*NonResidualPredicateError` rather than a degenerate predicate, mapping Java's
`Verify.verify(!disjuncts.isEmpty())` (`OrPredicate.java:439`).

Wired at every Java call site: `ImplementSimpleSelectRule`
(`rule_implement_simple_select.go:162`), and both `ImplementFilterRule` sites via
one up-front conversion (`rule_implement_filter.go:74`, consumed at `:124` and
`:158`) against `ImplementFilterRule.java:90`. The NLJ's existential residual
(`rule_implement_nested_loop_join.go:2599`) is deliberately NOT replaced by the
helper: it residualises against the **FirstOrDefault edge's flowed value** rather
than the predicate's own operand — a documented source-window-identity choice —
and folds the NOT into the comparison. Substituting the general helper there
would change behaviour, so it stays specialised and the invariant below is what
guarantees it cannot ship a structural predicate.

## 4a. The invariant, because three rules build filters and two forgot

Wiring call sites is not enough; it is what was already tried, once per rule, and
two of the three rules had forgotten. `predicates.FindStructuralPredicate` names
the three unevaluable types, and the single constructor every physical filter
funnels through — `NewRecordQueryPredicatesFilterPlanWithAliasFromQuantifier`,
reached by all four `NewRecordQueryPredicatesFilterPlan*` forms — rejects one.

This is the difference between fixing three bugs and closing a bug class. The
existing `validateNoIndexOnlyResidual` backstop
(`plan_executability.go:49`) is the right idea aimed at a different target: it
rejects filters carrying index-only *values* (a vector `DistanceRank`), not
predicates that cannot answer. It did not, and structurally could not, catch any
of this.

## 5. Result

`bazelisk test //pkg/recordlayer/query/...` — **9/9 targets pass**, including the
23 new unit assertions.

The earlier draft of this section reported **15,246 run / 15,229 pass / 17 fail /
0 skip** over `//pkg/recordlayer/query/... + //pkg/relational/sqldriver` and
called every failure a shape or census pin. That number is real and that scope is
stated, but the sentence around it read as *the change is row-clean*, and it is
not. The full `just test` finds more, and one is an execution failure:

```
TestYamsqlConformance/exists — test[7]:
  SELECT o.id FROM orders AS o, flags AS f WHERE EXISTS (SELECT k FROM flags) ORDER BY o.id
  query error: executor: *plans.RecordQueryNestedLoopJoinPlan emitted row type
  RECORD<Q$N LONG NOT NULL, ID LONG NULL, CUST_ID LONG NULL> outside provided
  layout carrier *values.quantifiedObjectValue
```

Two distinct signatures, same shape. The materialised NLJ *declares* an output
type derived from a single-leg `QOV` result value while its cursor *emits* the
concatenation — a pre-existing contract hole in the 6,532-line file that the peel
newly reaches. Loud, not silent, and it must be fixed before this ships.

The operator it is in is itself Go-only, which is the more useful way to hold it.
`find fdb-record-layer -name 'RecordQueryNestedLoopJoin*.java'` returns nothing;
Java's plans package contains `RecordQueryFlatMapPlan` and no materialised
nested-loop join at all, and `ImplementNestedLoopJoinRule` yields only FlatMaps.
So a Java-shaped plan reached a Go-only physical operator whose declared output
contract its cursor does not keep. That is one more instance of the pattern this
RFC is about rather than an argument against the peel, and it is fixed at the
operator.

`TestPlanShapeGolden` additionally drifts **11,223 lines**. The sampled diffs are
improvements — a materialised cross product becoming a correlated index probe —
but a blast radius that size is not a footnote, and §9's stress comparison is a
gate, not a formality.

**A measurement's scope belongs inside the sentence that quotes it.** The 17 was
a fact about two Bazel targets and got written up as a fact about the change.

## 6. The unreachable arms cannot be corpus-verified, and are pinned anyway

Two of the three types are unreachable from any SQL the engine accepts today: no
planner rule mints a `PredicateWithValueAndRanges` (its only constructions are
rebuild-path reconstructions at `replace_values.go:180,304`; positive control for
that sweep — `NewComparisonPredicate(` returns 17 non-test hits in `pkg`), and no
rule leaves a `Placeholder` in a physical filter. So a full-suite green says
nothing about either, and a first real firing would be read as a finding rather
than as an untested branch.

Both are driven by unit test, along with the recursion arms (which ARE reachable:
`NOT EXISTS` is `NotPredicate(ExistentialValuePredicate)` in an ordinary flat
list — an earlier draft of the test file claimed otherwise and was wrong), the
rangeless-sargable error, error propagation through nesting, and the invariant's
own three-type census. Mutation-verified: reverting the existential conversion
reddens 5 subtests and flipping the sargable disjunction reddens 1, neither
touching the other's.
## 7. Why the 630-line arm is not deleted in this RFC — measured, not estimated

The arm was disabled and the same suite re-run: **27 fail**, ten more than with
it enabled. Of the ten new entries, one is the arm's own unit test
(`TestRebaseOuterLegValue_DerivableLegKeepsTheLegLocalRead`) and five are
census-floor arms going quiet, both expected. The other four are one finding —
`TestFDB_ProjectedExistsOverLeftJoin` plus its three subtests:

```
projected_exists_left_join_null_extend_fdb_test.go:75:
  projected EXISTS over LEFT JOIN errored (Java answers it — a Java-parity reach gap):
  0AF00: Cascades planner could not plan query
```

`TestFDB_ProjectedExistsOverLeftJoin` — three subtests, a clean decline rather
than wrong rows. **`implementJoinWithExistential` is currently the only thing in
Go that plans a projected EXISTS over a LEFT JOIN.** Deleting it today would
convert a Java-parity gap the arm was papering over into an outright
regression.

Java reaches that shape through the same partitioning: `Quantifier.Existential`
inherits the plain `getFlowedObjectValue()` (`Quantifier.java:801-803`) and
overrides only `getFlowedObjectType()` to be nullable (`Quantifier.java:407-408`),
so Case 2 flows `QOV(E)` up and the upper's `ExistsValue` reads it. Go declines
it at `rule_partition_select.go:123-141`, where a projected existential "keeps
today's clean decline". **Phase 2 is therefore a port, not a design** — which is
the check CLAUDE.md requires before any "needs a capability that doesn't exist"
conclusion.

Phase 2, on the ACK of this one, closes that decline and then deletes:

| unit | LOC |
|---|---|
| `implementJoinWithExistential` | 630 |
| `executor.bindMergedOuterLegs` | 115 |
| `leg_local_bake_census.go` | 1,349 |
| `fold_step1_seed_census.go` | 1,240 |
| **named total** | **3,334** |

plus the support cast whose every non-comment call site is inside
`rule_implement_nested_loop_join.go` — `foldStep1Seed`,
`reconstructFoldStep1Seed`, `legOrdinalSafety`, `rebaseOuterLegValue`,
`rebasePlanBuriedRefs`, `ordinalSeedLegWindowsOf`. The exact footprint is
measured by deleting and compiling, not predicted here.

That retires the `DIVERGENCES.md` entry outright, rather than advancing its
`102` by another decrement.

## 8. Scope

In: the primitive, its four Java call sites, the two guard deletions, the
rewritten shape pins and re-aimed census floors, the sargable unit pin, the
stress comparison.

Out: Phase 2 (projected-EXISTS partitioning and the deletion above); CQ-96 (the
translator's untyped `QuantifiedObjectValue` mints — a real divergence on a
different population, which this change advances by zero reads); CQ-97
(`RecordQueryFlatMapPlan.GetResultType()` returning `UnknownType`).

## 9. What could make this wrong

- **The peel is a worse plan.** Possible; §5 says so and makes the stress
  comparison a gate rather than a formality. If it is worse, the answer is the
  cost model, not the guard — a guard that suppresses a correct alternative to
  avoid costing it is a cost-model bug wearing a rule's clothes.
- **Some shape reaches the peel that the 21-failure run did not.** The corpus is
  the evidence and it is not a proof. The sargable arm in §6 is the known
  instance and is pinned by unit test for exactly that reason.
- **The rewritten shape pins lose their teeth.** They must assert Java's chained
  FlatMap shape positively, not merely stop asserting the old one. A pin
  weakened to "some plan" is how this becomes untested.

## 10. Review round 1 — both gates NAK'd, and what changed

Both reviewers confirmed the architecture and the causal chain independently.
Graefe verified every Java citation and ran his own control —
`grep -n 'Existential\|getKind' PartitionSelectRule.java` returns **zero** hits
against 27 `Quantifier` hits in the same file — so Java genuinely has no
counterpart to either deleted guard, and `lower = {E}` is what its subset
enumeration yields. He also confirmed convergence is unaffected: Go retains both
of Java's termination guards (`rule_partition_select.go:50`, and the usefulness
check mirroring `PartitionSelectRule.java:255-260`), the peel has a non-empty
`lowerPredicates` and strictly shrinks the upper, so the added memo alternatives
are alternatives, not a fixpoint.

They NAK'd on five defects, four of which were the same mistake wearing different
clothes — **shipping the bug the RFC was written about**:

1. **`*Placeholder` uncovered.** Java gets it by inheritance; Go's standalone
   struct fell to `default` and reached filters unchanged. Both gates found it
   independently. Fixed, with a unit pin.
2. **Empty/singleton `AND`/`OR` not ported.** `NewOr()` on zero disjuncts is an
   `OrPredicate` whose `Eval` is FALSE — the silent row-drop, respelled inside the
   fix for it. Java asserts instead (`OrPredicate.java:439`). Now an error type,
   with the singleton-unwrap semantics ported too.
3. **A justification that was false.** The existential arm reused the predicate's
   own comparison, claiming "the constructor asserts it". It does not
   (`existential_value_predicate.go:53-58` asserts only the QOV). Now minted
   unconditionally, as Java does, and pinned by a test that feeds the arm a
   predicate carrying the wrong comparison.
4. **One call site of four wired.** `ImplementFilterRule`'s two sites still passed
   predicates raw. Now wired through one shared conversion.
5. **A stale comment and its dead variable.** Deleting the count-based bail
   silently narrowed the projected-existential decline from per-rule to
   per-bipartition while its comment went on describing the old behaviour. The
   condition is now count-independent, which is what the comment always claimed,
   and `existentialCount` — by then assigned and never read — is gone.

Graefe's remaining point is the one that changed the shape of the work rather
than a line of it: wiring call sites is what was *already* being done, once per
rule, and two of three rules forgot. §4a is the answer — one invariant at the
single constructor every physical filter funnels through, so the class is closed
rather than three instances of it.

Torvalds' fifth finding stands unresolved and is honest to state: the 17 pins in
§5 are still a plan, not work, and the branch is red. That, the execution failure
in §5, and the golden drift are what round 2 must close.

## 11. Why the guard deletions are a separate phase

The two halves have different risk profiles and bundling them is what made v1
unlandable.

**The primitive and the invariant change no plan.** `FindStructuralPredicate`
fires **0 times** across 20,621 relational tests: nothing on master ships a
structural predicate today. That is the negative result worth stating plainly —
this half fixes a bug that is *latent*, not one users are hitting. It is still
worth landing on its own terms, because the defect it prevents is the kind that
returns an empty result set and reports success, and because the peel cannot work
without it.

**The guard deletions change plans for a whole query class.** Measured on the
same tree: 20,621 run / 20,594 pass / **27 fail**, plus `TestPlanShapeGolden`
drifting 11,223 lines. Of the 27, nine are the layout-carrier execution failure in
§5; the rest are the shape and census pins of §5's table.

Splitting is not deferral of the second half — it is the one-logical-change rule,
and it means the invariant that makes this bug class impossible ships regardless
of how long the peel takes.

**One measurement from the split is worth keeping, because it cost a family.**
Correcting the stale comment on the projected-existential decline (§10 finding 5)
by making the condition count-independent — which is what the comment, read
alone, implies it should be — reddened 19 further tests: the whole N-way
projected-EXISTS family goes 0AF00, because a genuine N-way cluster needs
partitioning to reach a plan at all. The condition is a two-layer decline and the
count selects between the layers. The comment now says so, and names the four
tests that prove it, because the next reader will otherwise make the same tidy
and wrong repair.

Phase 2 owns: the Go-only NLJ's layout-carrier contract, the shape pins rewritten
to assert Java's chained-FlatMap shape POSITIVELY, the census floors re-aimed at
their new expected direction, the goldens re-blessed with the diff reviewed
rather than regenerated, and the stress comparison §9 makes a gate.
