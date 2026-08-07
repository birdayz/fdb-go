# RFC-216 — `LIKE 'prefix%'` reaches an index via an implied `STARTS_WITH` conjunct

Status: DESIGN ONLY — no production code lands with this RFC

Numbering note: this document was drafted as "RFC-213". That number was already
claimed by an unrelated in-flight RFC ("the result type derives from the result
value", branch `feat/cq97-rfc`), so it is renumbered to 216 — the next free slot
above the merged RFC-215. The companion dead-twin retirement referenced below as
"RFC-214" is likewise renumbered to **RFC-217**.

**Read §4.0 first.** The implementation described below was written, measured,
and REMOVED. It returned wrong rows (§4.0), and the pieces that were correct
(`values.LikeConstantPrefix` and its soundness fuzz) are not committed either,
because with the rule gone their only callers would be tests — the precise
dead-code shape this RFC's own investigation exposed elsewhere in the tree. They
return together, on top of RFC-217, when CQ-33 is actually implemented.

What DOES land now is the one fact that is load-bearing regardless of whether
the rule is ever built, expressed against the live matcher rather than against a
helper: `TestLikeMatch_NoPatternYieldsATightPrefixRange` in
`cascades/predicates/comparisons_test.go`. It pins that no LIKE pattern yields a
tight prefix range, so no future prefix-range rewrite may drop the residual
filter. Mutation-verified: making `%` able to cross a line terminator turns it
red.

The soundness result for the (uncommitted) extractor was 393,455 fuzz execs with
0 failures against the invariant
`LikeMatch(p,s,e) => HasPrefix(s, LikeConstantPrefix(p,e))`.

Original status: proposed (implementing)
Item: TODO.md CQ-33
Area: Cascades planner — PLANNING-phase exploration, predicate augmentation

## 1. The defect, measured

`WHERE s LIKE 'prefix%'` on an indexed STRING column full-scans at every table
size. Measured through `embedded.PlanQueryForTest` on a schema with
`CREATE INDEX idx_status ON t2 (status)`:

```
SELECT id FROM t2 WHERE status LIKE 'act%'   ->  Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))
SELECT id FROM t2 WHERE status =    'active' ->  Project([ID#0], IndexScan(IDX_STATUS, [=] COVERING))
SELECT id FROM t2 WHERE status LIKE '%act'   ->  Project([ID#0], PredicatesFilter(Scan(T2), [1 preds]))
```

Root cause, confirmed by reading the code rather than the comments:

- `predicates.ComparisonLike` is **not** admitted by `isSargableComparisonForMatch`
  (`match_max_match_map.go:67-81`) nor by `isScanRangeCompatible`
  (`scan_match_helpers.go:37-47`), so a LIKE conjunct can never bind to a
  candidate index placeholder.
- `Resolver.ResolveStartsWith` (`expr.go:1437`) — which builds the comparison the
  scan machinery *does* accept — has zero production callers. `walk.go`'s
  `LikePredicateContext` arm (`walk.go:1869-1906`) unconditionally routes to
  `ResolveLikeWithEscape`.
- No rule anywhere derives a range from a LIKE. Grep for `ComparisonLike` over
  non-test `pkg/` returns only its construction site, `null_rejecting_conjuncts.go:76`,
  `rule_simplify.go:467`, `values/value_like.go`, and two conformance harnesses.

Everything downstream of `ComparisonStartsWith` already exists and is unit-tested:
the sargability admission, the physical STRING-only veto
(`physical_key_types.go:156-186`), the `PREFIX_STRING` range construction
(`executor.go:1379-1411`), and the binder (`scan_range_binding.go:929-966`).
The single missing link is a producer.

### 1.1 A second finding: the scenario named for this optimization cannot detect it

`pkg/relational/conformance/yamsql/testdata/like_prefix_pushdown.yaml` is 336
lines whose header states the pushdown exists ("Previously fell through to
full-type scan + per-row likeMatch") and enumerates its bail-out cases. **No such
pushdown has ever existed.** Every one of its ~60 assertions is rows-only, and
rows are identical with or without the optimization, so the file passes
unchanged against a tree that has none of the behaviour it describes. This is
prose asserting behaviour the code does not have. This RFC makes the file's
claims true and adds the `plan_contains` assertions that make it able to fail.

## 2. The design is forced by a measurement: the range is never tight

The obvious design — "when the pattern is `<literal>%` with no other wildcard,
replace the LIKE with `STARTS_WITH(literal)`" — is **wrong in this engine**, and
the reason is not the interior-wildcard case everyone anticipates.

`LIKE`'s `%` compiles to `.*` under `Pattern.compile` with **no DOTALL flag**
(`values/like_match.go:23-42`, spec: `PatternForLikeValue.java:96-117`), so `%`
cannot consume a line terminator. Measured:

```
LikeMatch("abc%", "abcdef")   = true
LikeMatch("abc%", "abc\ndef") = false      <- but HasPrefix("abc\ndef", "abc") is TRUE
LikeMatch("abc%", "abc\rdef") = false
LikeMatch("abc%", "abc z") = false
```

So `STARTS_WITH('abc')` is a **strict superset** of `LIKE 'abc%'` for *every*
pattern shape — there is no shape for which the prefix range is tight. Dropping
the LIKE and keeping only the range returns rows the query must not return. This
is pinned by `TestLikeConstantPrefix_TrailingPercentIsNotTight`, whose failure
message names exactly what gets re-armed if `LikeMatch` ever changes.

This is CockroachDB's `tight bool` (`idxconstraint/index_constraints.go`, the
`LikeOp` case in `makeSpansForSingleColumnDatum`, and the
`RemainingFilters()` rule at `index_constraints.go:1288-1308`) arriving at the
same answer: the span is emitted, the filter is retained.

### 2.1 Consequence: augment, do not rewrite

Because the residual is mandatory, the change is expressed as **adding a
redundant conjunct that is implied by one already present**, never as a
replacement:

```
Select([... , LIKE(s,'abc%')])   ->   Select([... , LIKE(s,'abc%'), STARTS_WITH(s,'abc')])
```

The safety argument is structural, not a promise anyone has to keep:

- `STARTS_WITH` is implied by `LIKE` (fuzzed, §4), so adding it cannot remove a
  row.
- `ComparisonLike` is **not sargable by construction**, so it can never enter
  `matchedQueryPreds` (`rule_match_intermediate.go:986`), so its compensation can
  never become `NoPredicateCompensationNeeded` — it *always* lands in the
  residual loop (`rule_match_intermediate.go:1096-1115`) and is re-applied as a
  filter above the scan. There is no predicate-implication simplifier in the tree
  that could absorb it, and `FilterDedupPredicatesRule` keys on `Explain()`
  string identity, which differs between the two comparisons.

That is why this design is immune to the CQ-34 / F11 hazard class (a comparison
accepted as sargable whose bound does not fully constrain, with the residual
already removed). The alternative design — teaching the matcher that LIKE is
sargable and deriving a range from it — walks straight into that hazard: LIKE
would enter `matchedQueryPreds`, its compensation would default to
`NoPredicateCompensationNeeded`, and the LIKE filter would be **dropped**,
returning the `abc\ndef` rows above. Making that design safe would require
threading a `tight` flag through the whole compensation path. Rejected.

### 2.2 Phase: PLANNING exploration only, as an alternative

The augmented form is registered in `PlanningExplorationRules()` and **not** in
`RewritingRules()`, and it *yields an alternative member into the group* rather
than replacing the matched expression. Both choices are forced:

- REWRITING has no index knowledge and prunes to a single winner. The augmented
  form carries one more residual conjunct, so `RewritingCostModelLess` criterion
  3 ("fewer normalized residual predicate conjuncts") would discard it at the
  phase boundary every time. Registering it there is useless at best.
- Access-path selection happens in PLANNING (`MatchLeafRule` /
  `MatchIntermediateRule` are PLANNING-only), so that is where an
  access-path-enabling alternative belongs.

Keeping both members lets the cost model decide, and it already has the right
criterion — `PlanningCostModelLess` #3 is "fewer normalized residual predicates",
ahead of #4 (data-access operator count):

- **No usable index**: augmented = 2 residual conjuncts, plain = 1. Criterion #3
  discards the augmented form. No plan movement, no extra per-row `HasPrefix`,
  no golden churn on the hundreds of existing unindexed LIKE queries.
- **Usable index**: the `STARTS_WITH` is consumed into the scan range, so the
  augmented form is back to 1 residual conjunct and wins on the scan criteria.

The selection is emergent from properties the cost model already derives
(design principle 10), not an `if hasIndex` heuristic in the rule.

## 3. Prefix extraction

`values.LikeConstantPrefix(pattern string, escape rune) string`, placed beside
`LikeMatch` in `cascades/values/like_prefix.go` and dispatching through the
**same** `escapedLiteralAt` helper the matcher uses. Sharing that helper is the
point: it is the one definition of what opens an escape sequence, so the
extractor and the matcher cannot drift into the manual lockstep CQ-34 describes.

Contract (one-way, §2): `LikeMatch(p,s,e) => HasPrefix(s, LikeConstantPrefix(p,e))`.

Hazard handling falls out of sharing the helper rather than being enumerated:

| Hazard | Behaviour | Why |
|---|---|---|
| `_` or `%` inside the prefix (`'a_b%'`) | prefix `a`, residual retained | scan stops at first unescaped wildcard |
| ESCAPE, escaped `%` in prefix (`'a!%b%'` ESC `!`) | prefix `a%b` | `escapedLiteralAt` |
| escape before ordinary char (`'a!b%'` ESC `!`) | prefix `a!b` | Java's 2-entry table: not an escape, so a literal |
| dangling escape (`'abc!'` ESC `!`) | prefix `abc!` | same fallthrough; Java pins this (`like.yamsql:92`) |
| escape char == `%` (`'%%abc'` ESC `%`) | prefix `%abc`; `'%abc'` ESC `%` -> empty | same fallthrough |
| empty prefix (`'%foo'`) | **no rewrite** | guard in the rule |
| non-constant pattern | **no rewrite** | `values.EvaluateConstant` must succeed |
| non-string LHS | no *useful* rewrite | see §3.1 |
| NULL rows | unchanged | `STARTS_WITH` is null-rejecting exactly as `LIKE` is (`null_rejecting_conjuncts.go:76`); both yield UNKNOWN on a NULL input (`comparisons.go:471`) |
| case sensitivity | sound | LIKE compiles with **no** `CASE_INSENSITIVE` flag, so it is byte-exact, matching tuple byte order |
| `NOT LIKE` | **no rewrite** | the complement of a prefix range is not contiguous; guarded on the un-negated predicate only |

### 3.1 Non-string LHS — and a gap this closes

`ResolveLikeWithEscape` has a plan-time LHS type gate (`expr.go:1418-1428`,
42804 on a numeric LHS); `ResolveStartsWith` has **none**. Today that is
unreachable (zero callers). This RFC makes `ComparisonStartsWith` a live
production path, so the gate is added to `ResolveStartsWith` to match — without
it a programmatically-built `STARTS_WITH(int_col,'abc')` degrades to a residual
that evaluates UNKNOWN per row (`comparisons.go:475-479`) and silently returns
zero rows where the equivalent LIKE raises 42804.

In the rewrite path proper the LHS is already LIKE-gated, so it is String,
Unknown, Null, Enum, Date or Timestamp. Of those only `TypeCodeString` survives
the physical veto (`physical_key_types.go:182-184`); the rest are demoted to
residual and the plan is merely the unaugmented one. Sound in every case.

## 4. Proof obligations

1. **Soundness fuzz** — `FuzzLikeConstantPrefixSoundness` plus an exhaustive
   grid over an alphabet dense in `% _ ! \n`, asserting
   `LikeMatch => HasPrefix(prefix)` directly against the real matcher. A
   counterexample is a wrong-rows bug.
2. **Not-tight pin** — `TestLikeConstantPrefix_TrailingPercentIsNotTight`, the
   negative result that forbids ever dropping the residual.
3. **The optimization fires** — `plan_contains: IndexScan(...)` added to
   `like_prefix_pushdown.yaml`, which today cannot fail.
4. **Every hazard row of §3 as a distinct case**, including
   `plan_not_contains: IndexScan` for the bail-outs.
5. **Residual retention** — a scenario with a row that is inside the prefix range
   but fails the LIKE (the `\n` case), asserting both the IndexScan and the
   correct row set. This is the test that goes red if anyone later "simplifies"
   by dropping the LIKE.
6. **plandiff corpus** — Go-only extension entries with
   `Direction: DivergenceJavaErrorsGoCorrect` where Java rejects.
7. **1M stress before/after**, recorded in TODO.md.

## 4.0 STATUS: NOT REGISTERED — the rule as written returns wrong rows

The rule is present in the tree but deliberately **not** in
`PlanningExplorationRules`. Registering it returns ZERO rows for every
primary-key prefix LIKE:

```
scenario like_prefix_pushdown: test[0] "SELECT name FROM t WHERE name LIKE 'a%' ORDER BY name":
  row set mismatch (unordered=false)
    expected (2 rows):  [apple] [apricot]
    actual   (0 rows):
```

The plan does change to `PredicatesFilter(Scan(T, [<>]), [1 preds])` — the
STARTS_WITH binds — but the resulting range is EMPTY. The primary-scan range
binder is not producing the PREFIX_STRING endpoints the index path produces.
Root cause not yet diagnosed. This must be fixed and pinned before the rule is
registered.

Reachability: **latent, not live on master.** Every production producer of
`ComparisonStartsWith` was enumerated. `index_predicate_to_query.go:151` feeds
`index_expansion.go:144-153`, which adds the converted predicate to the
CANDIDATE side gated on `*ValueIndexScanMatchCandidate` — it can never reach a
primary-scan binder. `ddl/generator_predicate.go` is index-predicate DDL.
`ResolveStartsWith` still has zero production callers (only `expr_test.go:530`).
So only this (unregistered) rule can reach the defect.

The pin for that negative result is NOT a new test: it already exists as the
standing comment and assertions in
`pkg/relational/sqldriver/sargability_differential_oracle_fdb_test.go:25-26`,
which record that the tree has zero non-test callers of `ResolveStartsWith` and
no rewrite rule that produces `ComparisonStartsWith` on a primary-scan binder.
An earlier draft of this RFC credited a
`TestLikePrefixToStartsWithRule_NotRegistered`; no such test exists, and none is
added, because with the rule removed there is no rule whose non-registration
could be asserted. That claim is withdrawn.

FOUR BLOCKERS that must be confronted before the rule is ever registered.

  0. THE LOGICAL TYPE GATE MAY DISAGREE WITH THE PHYSICAL KEY TYPE.
     The rule gated on the LOGICAL LHS type being `TypeCodeString`. The live
     binder `bindScanComparisonsToRangeSet` FAILS CLOSED on a STARTS_WITH over a
     non-STRING PHYSICAL key component (`UnsupportedPhysicalStartsWithError`,
     `scan_range_binding.go:137-158`), and that check explicitly covers the
     string-backed DATE/TIMESTAMP/ENUM logical carriers. If the two can ever
     disagree, the rule rewrites a working plan into one that errors at
     execution. UNVERIFIED — it must be verified, not assumed, before shipping.

  Plus three test failures that registering the rule caused:

  - `TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest` — the two rule
    types have no direct behavioral test.
  - `TestPartitionSelect_ChainInterningBaseline`
  - `TestJavaCorpusRuns/files`

DEAD TWIN — the F11 claim in §1 is retracted. `scanComparisonsToTupleRange`
(`executor.go:1265`), including its STARTS_WITH arm at `:1379`, has **zero
production callers** and 41 test references. Every scan executes
`bindScanComparisonsToRangeSet` instead (`executor.go:268` primary, `:383`
index, `:655`, `executor_new_plans.go:105`). So `:1379` is not "unreachable from
SQL pending a producer" — it is unreachable from anything, and no planner rule
could ever have reached it. Whether the live binder implements the same
STARTS_WITH behaviour is an OPEN QUESTION, not an assumption. Retiring the dead
twin is its own change and lands as RFC-217.

Note this corrects §1.1's characterisation: `like_prefix_pushdown.yaml` cannot
detect whether the optimization FIRES, but its rows-only assertions DID catch
the broken range immediately. It is a working semantics net, not a useless
file — the gap is dimensional (no plan assertion), not total.

## 4.1 ALSO blocked on a pre-existing covering-stamp defect

Measured with the rule temporarily registered:

```
SELECT name FROM t  WHERE name   LIKE 'ap%'   ->  PredicatesFilter(Scan(T, [<>]), [1 preds])   (range EMPTY, see 4.0)
SELECT id   FROM t2 WHERE status LIKE 'act%'  ->  PredicatesFilter(Scan(T2), [1 preds])        NOT CHOSEN
```

Secondary indexes do not fire at all, and §2.2's claim that
the augmented form "wins on the scan criteria" is **measured false** for them.
Diagnosis, with the deciding counters:

- Criteria #2 and #3 and #4 all tie (`residual A=1 B=1`, `dataAccess A=1 B=1`,
  max-data-access-cardinality unknown on both). The tie is *structural*: the
  LIKE is residual on both sides by construction, so residual count can never
  discriminate for a pure-LIKE query.
- Control drops to criterion #7, `comparePrimaryVsIndexRank`
  (`planning_cost_model.go:1201`, reached from `:271`), which ranks the primary
  scan `tier=0` (`:1246`) and the index scan `tier=1` (`:1239`) — decided, index
  loses. The scalar-cost rung that would have picked the index (165000 vs
  500000) is never reached. Neutering only that rung flips the plan, so #7 is
  the sole gate.

The root cause is one level down and is **not introduced by this rule**. The
index plan is not stamped COVERING, while the `status > 'act'` control is.
`isSingularIndexScanWithFetch` (`:1389-1397`) keys on `indexScanCount==1`, so an
uncovered index scan is "contested" even with `fetchCount==0`.
`MergeProjectionAndFetchRule` (`rule_merge_projection_and_fetch.go:91`) stamps
covering only when the fetch's inner is *directly* a `RecordQueryIndexPlan`;
once `PushFilterThroughFetchRule` has pushed the residual below the fetch it is
a `RecordQueryPredicatesFilterPlan`, so the rule takes the fallback at `:117-126`
and the covering flag is lost.

Java has no such failure mode, and the reason is structural rather than a
missing branch: Java's coveringness is a distinct class,
`RecordQueryCoveringIndexPlan`, which deliberately does NOT implement
`RecordQueryPlanWithIndex` (`RecordQueryCoveringIndexPlan.java:74`). Java's
`MergeProjectionAndFetchRule.onMatch` yields `fetchPlan.getChild()` with no
shape check (`MergeProjectionAndFetchRule.java:61-77`), leaving
`Filter(CoveringIndexPlan)`; `isSingularIndexScanWithFetch`
(`PlanningCostModel.java:474-478`) then counts zero `RecordQueryPlanWithIndex`
and criterion #7 abstains (`PlanningCostModel.java:376-380`). Go collapsed the
two classes into a `covering bool` on `RecordQueryIndexPlan`, and the flag is
lost exactly when a residual sits between the fetch and the scan. Go's criterion
#7 is itself a faithful port of `PlanningCostModel.java:370-413` and must not be
touched.

The defect is pre-existing and independently observable without this rule:

```
SELECT id FROM t2 WHERE status > 'act'                        ->  IndexScan(IDX_STATUS, [<>] COVERING)
SELECT id FROM t2 WHERE status > 'act' AND status LIKE '%zz%' ->  PredicatesFilter(IndexScan(IDX_STATUS, [<>]), [1 preds])
```

The second has already lost its COVERING stamp to the residual. It still picks
the index only because criterion #3 separates the plans first (1 residual vs 2),
so #7 is never consulted. This rule's augmented form ties at #3 and #4 by
construction, which is what drops the decision onto #7 and makes the lost
covering stamp load-bearing for the first time.

**The correct fix is to restore the covering stamp through an intervening
residual plan** — the Go analogue of Java's `Filter(CoveringIndexPlan)` — not to
weaken criterion #7. That is a physical-plan change with its own plan-movement
blast radius, and it must land before this rule is useful for secondary indexes.

## 5. Rejected alternatives

- **Rewrite LIKE -> STARTS_WITH, drop the residual.** Unsound: §2's line-terminator
  measurement. Would return wrong rows for every subject containing `\n`, `\r`,
  NEL, LS or PS after the prefix.
- **Make `ComparisonLike` sargable and derive the range in the matcher.** Walks
  into the CQ-34/F11 hazard: the compensation machinery drops a sargable
  predicate by default, so the LIKE filter disappears. Needs a `tight` flag
  threaded through `PredicateMultiMap` compensation first. Strictly more
  machinery for the same plan.
- **Do it in the resolver (`walk.go`), like the BETWEEN desugar.** BETWEEN
  desugaring is a semantics-preserving syntax expansion; this is an
  access-path optimization, and optimizations belong in the optimizer. Doing it
  in the resolver makes the extra conjunct unconditional, costing every
  unindexed LIKE an extra per-row predicate and churning the explaindiff golden
  for every existing LIKE query.
- **Register in REWRITING as well.** The REWRITING cost model's residual-count
  criterion discards the augmented form at the phase boundary; it cannot
  survive to where indexes are known.
