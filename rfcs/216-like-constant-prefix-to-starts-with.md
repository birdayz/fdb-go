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

### What is actually on this branch

Stated plainly, because the body below was drafted while the prototype existed
and several passages were written in the present tense about it:

- **Landed:** `TestLikeMatch_NoPatternYieldsATightPrefixRange` in
  `cascades/predicates/comparisons_test.go`, and nothing else.
- **Not present, in any form:** the rule, its registration, `values.LikeConstantPrefix`,
  `cascades/values/like_prefix.go`, `FuzzLikeConstantPrefixSoundness`, the
  `ResolveStartsWith` type gate, and any `plan_contains` assertion in
  `like_prefix_pushdown.yaml`. `grep -rn LikeConstantPrefix pkg/` returns nothing.

The landed test is the one fact that is load-bearing regardless of whether the
rule is ever built, and it is expressed against the live matcher rather than
against a helper, which is why it survives the prototype's removal. It pins that
**no LIKE pattern yields a tight prefix range** — quantified over pattern
classes, not over one shape: wildcard-free, trailing `%`, interior `%`, `_`, an
ESCAPE-escaped wildcard inside the prefix, and a leading `%` (empty prefix).
Each class carries a witness inside the byte-prefix range that the predicate
rejects, plus a matched control so the class cannot pass vacuously. It also pins
that `_` is terminator-averse exactly as `%` is, and the final-terminator
boundary of §2.

Mutation-verified in four independent directions, each red on its own: letting
`_` cross a line terminator, making `_` consume zero-or-more, disabling escape
recognition, and removing the final-line-terminator tolerance.

The soundness result quoted below for the extractor — 393,455 fuzz execs with 0
failures against `LikeMatch(p,s,e) => HasPrefix(s, LikeConstantPrefix(p,e))` —
is a **HISTORICAL measurement of the discarded prototype**. Neither the
extractor nor `FuzzLikeConstantPrefixSoundness` is committed, so the number is
**not reproducible on this branch**. It is recorded as evidence about a design
that was explored, never as a standing property of the tree.

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

Everything downstream of `ComparisonStartsWith` already exists and is
unit-tested: the sargability admission, the physical STRING-only veto
(`physical_key_types.go:156-186`), and the LIVE binder — the STARTS_WITH tail
arm at `scan_range_binding.go:929-965` and the `PREFIX_STRING` endpoint
construction it feeds at `scan_range_binding.go:1373-1379`. The single missing
link is a producer.

An earlier draft of this section cited `executor.go:1379-1411` as the
`PREFIX_STRING` range construction. That was the DEAD TWIN
(`scanComparisonsToTupleRange`), not a live path — see the DEAD TWIN note in
§4.0 — and RFC-217 deletes it in this same PR, so the citation now points at
nothing. The live locations above are the ones any future rule reaches.

### 1.1 A second finding: the scenario named for this optimization cannot detect it

`pkg/relational/conformance/yamsql/testdata/like_prefix_pushdown.yaml` is 336
lines whose header states the pushdown exists ("Previously fell through to
full-type scan + per-row likeMatch") and enumerates its bail-out cases. **No such
pushdown has ever existed.** Every one of its ~60 assertions is rows-only, and
rows are identical with or without the optimization, so the file passes
unchanged against a tree that has none of the behaviour it describes. This is
prose asserting behaviour the code does not have.

**Still true at HEAD, and still OUTSTANDING.** The file is unchanged by this
branch: it has no `plan_contains` assertion and its header still describes a
pushdown that does not exist. A future implementation must both make the
claims true and add the assertions that let the file fail; neither has
happened. (§4.0 refines the characterisation: the file is not useless — its
rows-only assertions did catch the prototype's empty range immediately. The gap
is dimensional, not total.)

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
pattern shape that reaches a prefix range — there is no such shape for which the
prefix range is tight. Dropping the LIKE and keeping only the range returns rows
the query must not return. This is pinned by
`TestLikeMatch_NoPatternYieldsATightPrefixRange`, which carries one witness per
pattern class (wildcard-free, trailing `%`, interior `%`, `_`, escaped wildcard
inside the prefix, empty prefix) plus a matched control per class so none can
pass vacuously, and whose failure messages name exactly what gets re-armed if
`LikeMatch` ever changes.

**The counterexample needs an INTERNAL terminator, not merely a terminator.**
Java's default-mode `$` matches before ONE FINAL line terminator, and
`values.LikeMatch` models that by retrying against `trimFinalLineTerminator`
(`values/like_match.go:90-102`). Measured:

```
LikeMatch("abc%", "abc\n")    = true       <- FINAL terminator: ACCEPTED
LikeMatch("abc%", "abc\ndef") = false      <- INTERNAL terminator: rejected
```

`_` is terminator-averse on exactly the same grounds (`.` without DOTALL), which
is why the pin covers it as its own class rather than assuming `%` generalises.

### 2.0 The wildcard-free shape is NOT an equality — a carve-out that fails

An earlier draft carved out patterns with **no wildcards at all** (`LIKE 'abc'`)
on the grounds that such a pattern is an equality, its match set is the single
value, and an equality range over it *is* tight. That carve-out is **measured
false**, by the same `$` rule as above:

```
LikeMatch("abc", "abc")   = true
LikeMatch("abc", "abc\n") = true       <- $ before one final terminator
```

The match set of `LIKE 'abc'` is the literal plus the literal followed by any
ONE of the five terminators (`\r\n` counting as one) — seven values, not one. An
equality range over `'abc'` is therefore a strict SUBSET of the predicate, and
the failure direction inverts: a scan restricted to it does not return extra
rows, it **LOSES** rows. A future rule may NOT route the wildcard-free shape to
a bare equality bound and drop the residual.

The universal survives without a carve-out, in a stronger form: for every LIKE
pattern, no exact byte range — prefix or equality — equals the predicate's match
set, so the residual is mandatory in every shape. Both directions are pinned
(the `wildcard_free` class for the superset direction, PART 3 for the subset
one). This is not a new fact about the matcher — `FuzzLikeMatch`'s seed corpus
already carries `f.Add("abc", "abc\n") // true — $ before final terminator`;
what was new is noticing it refutes the carve-out.

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

- `STARTS_WITH` is implied by `LIKE` (fuzzed in the prototype only; §4
  obligation 2 is OUTSTANDING), so adding it cannot remove a
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

Nothing is registered anywhere on this branch — the prototype is gone. This
section is the design a future implementation must follow, stated in the
imperative: the augmented form belongs in `PlanningExplorationRules()` and
**not** in `RewritingRules()`, and it must *yield an alternative member into the
group* rather than replace the matched expression. Both choices are forced:

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

## 3. Prefix extraction (design; NOT in the tree)

No such function exists on this branch. The prototype's version was removed with
the rule, and the design it should be rebuilt to is:

`values.LikeConstantPrefix(pattern string, escape rune) string`, to be placed
beside `LikeMatch` in `cascades/values/like_prefix.go` and to dispatch through
the **same** `escapedLiteralAt` helper the matcher uses. Sharing that helper is
the point: it is the one definition of what opens an escape sequence, so the
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
42804 on a numeric LHS); `ResolveStartsWith` has **none**. At HEAD that is
unreachable (zero production callers), and **no gate has been added** — the
discarded prototype added one, and it went with the prototype.

**OUTSTANDING.** Any implementation that makes `ComparisonStartsWith` a live
production path must add the matching gate to `ResolveStartsWith` first, because
without it a programmatically-built `STARTS_WITH(int_col,'abc')` degrades to a
residual that evaluates UNKNOWN per row (`comparisons.go:475-479`) and silently
returns zero rows where the equivalent LIKE raises 42804.

In a rewrite path proper the LHS would already be LIKE-gated, so it is String,
Unknown, Null, Enum, Date or Timestamp. Of those only `TypeCodeString` survives
the physical veto (`physical_key_types.go:182-184`); the rest are demoted to
residual and the plan is merely the unaugmented one. Sound in every case.

## 4. Proof obligations

Exactly ONE of these is discharged. Every other line is **OUTSTANDING** and must
be built by whoever implements CQ-33; none of it exists on this branch.

1. **[DONE] Not-tight pin** — `TestLikeMatch_NoPatternYieldsATightPrefixRange`,
   the negative result that forbids ever dropping the residual, in any pattern
   shape and in both failure directions (§2, §2.0). It asserts against the live
   matcher and needs no rule, which is why it survives the prototype's removal.
   Mutation-verified in four independent directions.
2. **[OUTSTANDING] Soundness fuzz** — `FuzzLikeConstantPrefixSoundness` plus an
   exhaustive grid over an alphabet dense in `% _ ! \n`, asserting
   `LikeMatch => HasPrefix(prefix)` directly against the real matcher. A
   counterexample is a wrong-rows bug. The prototype's 393,455-exec run is
   historical only; neither the fuzz target nor the extractor it fuzzes is
   committed.
3. **[OUTSTANDING] Reachability pin** — a test that goes RED if
   `ComparisonStartsWith` acquires a production producer that reaches a
   primary-scan binder. §4.0's reachability result currently rests on grep and
   inspection, which cannot fail. This is the obligation that would have caught
   the prototype's zero-rows defect at registration time.
4. **[OUTSTANDING] The optimization fires** — `plan_contains: IndexScan(...)`
   added to `like_prefix_pushdown.yaml`, which today cannot fail.
5. **[OUTSTANDING] Every hazard row of §3 as a distinct case**, including
   `plan_not_contains: IndexScan` for the bail-outs.
6. **[OUTSTANDING] Residual retention** — a scenario with a row that is inside
   the prefix range but fails the LIKE (an INTERNAL-terminator subject; a
   final-terminator one matches and proves nothing, see §2), asserting both the
   IndexScan and the correct row set. This is the test that goes red if anyone
   later "simplifies" by dropping the LIKE.
7. **[OUTSTANDING] plandiff corpus** — Go-only extension entries with
   `Direction: DivergenceJavaErrorsGoCorrect` where Java rejects.
8. **[OUTSTANDING] 1M stress before/after**, recorded in TODO.md.

## 4.0 STATUS: REMOVED — the rule as written returned wrong rows

The rule is **not in the tree at all**. It was written, registered
experimentally, measured, and deleted; an earlier revision of this section said
it was "present but deliberately not registered", which was true of the working
copy that produced the measurements below and is not true of any committed
state. Everything in this section is a record of what the discarded prototype
did.

With it registered, it returned ZERO rows for every primary-key prefix LIKE:

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
So only the discarded rule could reach the defect.

**That result is NOT PINNED, and this RFC does not pretend otherwise.** It rests
on grep and code inspection — evidence, not a test. Two earlier claims to the
contrary are withdrawn:

- An earlier draft credited a `TestLikePrefixToStartsWithRule_NotRegistered`.
  No such test exists, and none is added: with the rule removed there is no
  rule whose non-registration could be asserted.
- A later draft credited
  `pkg/relational/sqldriver/sargability_differential_oracle_fdb_test.go:18-32`.
  Verified: that file contains **commentary only** on this point. Its runner
  (`runSargabilityCase`) increments `ctr.skipped` and RETURNS for any case whose
  baseline plan lacks an `IndexScan`, and the only end-of-run assertion is
  `if ctr.kept == 0` — a vacuity guard, not a reachability assertion. There is
  no assertion on caller count, on rule absence, or on the skip count. If a
  LIKE→STARTS_WITH producer landed, the LIKE cases would silently migrate from
  `skipped` to `kept` and the file would stay green. It cannot go red if
  reachability changes.

A real reachability pin is booked as **OUTSTANDING obligation 3** in §4. Naming
inspection evidence a "pin" is precisely the defect class this RFC exists to
document (§1.1), so it is not done here.

FOUR BLOCKERS that must be confronted before any such rule is ever registered.
All four are OUTSTANDING; none was resolved, because the prototype was removed
rather than fixed.

  0. THE LOGICAL TYPE GATE MAY DISAGREE WITH THE PHYSICAL KEY TYPE.
     The prototype gated on the LOGICAL LHS type being `TypeCodeString`. The live
     binder `bindScanComparisonsToRangeSet` FAILS CLOSED on a STARTS_WITH over a
     non-STRING PHYSICAL key component (`UnsupportedPhysicalStartsWithError`,
     `scan_range_binding.go:137-158`), and that check explicitly covers the
     string-backed DATE/TIMESTAMP/ENUM logical carriers. If the two can ever
     disagree, the rule rewrites a working plan into one that errors at
     execution. UNVERIFIED — it must be verified, not assumed, before shipping.
     RFC-217 settles what the binder DOES (it fails closed); it does not settle
     whether the rule's logical gate can admit a case the binder then rejects.

  Plus three test failures that registering the prototype caused, none of which
  was diagnosed:

  - `TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest` — the two rule
    types have no direct behavioral test.
  - `TestPartitionSelect_ChainInterningBaseline`
  - `TestJavaCorpusRuns/files`

DEAD TWIN — the F11 claim in §1 is retracted. `scanComparisonsToTupleRange`
(then at `executor.go:1265`), including its STARTS_WITH arm at `:1379`, had
**zero production callers**. Every scan executes `bindScanComparisonsToRangeSet`
instead (`executor.go:268` primary, `:383` index, `:655`,
`executor_new_plans.go:105`). So `:1379` was not "unreachable from SQL pending a
producer" — it was unreachable from anything, and no planner rule could ever
have reached it. **RFC-217 deletes it in this same PR**, so those line citations
describe a function that no longer exists; they are kept only to record what the
retraction was about.

An earlier revision left "whether the live binder implements the same
STARTS_WITH behaviour" as an OPEN QUESTION. **It is no longer open.** RFC-217
answers it, and its live tests in this PR pin the answer: the live binder has
its own STARTS_WITH tail arm (`scan_range_binding.go:929-965`) producing
`EndpointTypePrefixString` endpoints (`:1373-1379`), and it FAILS CLOSED with
`UnsupportedPhysicalStartsWithError` on a non-STRING physical key component
rather than silently degrading. It also deliberately diverges from the twin (and
from Java) on the negative-NaN bound, which RFC-217 §1a documents as sanctioned.
The remaining STARTS_WITH uncertainty for this RFC is blocker 0 below — a
logical-vs-physical type question about the RULE's gate, not about the binder.

Note this corrects §1.1's characterisation: `like_prefix_pushdown.yaml` cannot
detect whether the optimization FIRES, but its rows-only assertions DID catch
the broken range immediately. It is a working semantics net, not a useless
file — the gap is dimensional (no plan assertion), not total.

## 4.1 ALSO blocked on a pre-existing covering-stamp defect

Measured in the working copy with the (now discarded) rule temporarily
registered — these numbers cannot be reproduced on this branch, and the
covering-stamp defect they diagnose is pre-existing and independently observable
without any rule (see the two EXPLAINs at the end of this section, which DO
reproduce):

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

- **Rewrite LIKE -> STARTS_WITH, drop the residual.** Unsound: §2's
  line-terminator measurement. Precisely: it returns wrong rows for every
  subject that, after the prefix, contains a `\n`, `\r`, NEL, LS or PS the `%`
  would have to CONSUME — i.e. an INTERNAL terminator, `"abc\ndef"`. It is NOT
  wrong for a subject whose only terminator is FINAL (`"abc\n"`), which the
  predicate accepts (`$` matches before one final terminator) and the range
  also contains; an earlier draft claimed every terminator-bearing subject was
  a counterexample, and that overstated the defect. The narrower claim is the
  one the pin asserts, and it is enough: one wrong row is unsound.
  Independently, §2.0 kills the wildcard-free special case in the opposite
  direction — an equality range there LOSES rows.
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
