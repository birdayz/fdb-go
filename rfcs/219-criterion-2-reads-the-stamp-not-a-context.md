# RFC-219 — Criterion 2 reads the stamp, not a context

Status: **DRAFT, revision 4** — awaiting Graefe + Torvalds.
Closes: TODO CQ-30 (the criterion-2 half RFC-195 explicitly left open).
Deliberately does NOT close: Go's gap against Java's index criterion 1 (§3).

The §4 decision has survived all three revisions unchanged. Every NAK has hit the
*case*, never the direction — which is itself the finding worth stating: the
design was reached early and the evidence for it took three passes to become
true.

Rev 2 corrected rev 1's fictional ranking bug (§2) and its self-contradicting
§7.2. Rev 3 corrects the one change rev 2 introduced:

**Rev 2 claimed porting Java's PK-bound criterion 1 was "cheap and needs no new
plumbing." Refuted.** Go's candidate never folds the PK into the sargable surface
— a documented invariant (`match_candidate_index.go:678-681`) — so
`len(comps) == len(index key columns)` by construction and criterion 1 has **no
constructible input in Go**. Rev 2 cited `GetPKColumnNames()` as the enabling
data; that supplies PK *names and arity*, while criterion 1 needs PK *bindings*.
The mutation test rev 2 proposed could only have been satisfied by a shape
production cannot build.

That is this series' recurring failure — a cited mechanism that, exercised
against the shape in question, excludes exactly that shape — and it recurred in
the one paragraph rev 2 added rather than verified. Rev 3 scopes criterion 1 out
with the invariant as the stated reason and pins it as a negative result.

Rev 3 also replaced the review-reported census with a **structural** argument for
both arms (§2), stated the concrete walk's conversion explicitly (§4), and closed
the stress gate outright (§7.9).

**Rev 4** fixes three defects in rev 3's supporting case, none of which touched
the direction:

- **The scope-out is now measured, not argued.** Rev 3's pin covered one of the
  four production index-plan construction sites, and the four enforce the width
  invariant by three different mechanisms. The blocking question was whether
  `physicalGroupingPrefixCount` can exceed its candidate's column count — which
  would give criterion 1 a constructible input and make the whole scope-out
  wrong. **Measured: it cannot** (clamped at construction,
  `aggregate_index_candidate.go:137-138`, against `GetColumnNames()` returning
  the same `groupCols`, `:221`). §7.6's pin now drives all four sites and states
  the relation actually meant, `len(comps) <= len(GetColumnNames())` — equality
  holds only at `match_candidate_index`.
- **The inheritance is no longer described as free.** It is decisive: the
  abstaining side loses the rung outright (§3).
- **The divergence register is corrected** (§7.11); it previously said
  "functionally equivalent" about precisely this criterion.

---

## 0. What this RFC is NOT

It is not "port `CardinalitiesProperty`". **Measured: the property is already
ported, 1:1** — `properties/cardinality.go:1-16` says so, and RFC-195 already
routes the property map and all three cost walks through one derivation behind
`properties.CardinalityProver` (`cardinality_bounds.go:99`).

The companion note claimed five cost estimates still violate a proven bound.
**Measured: closed** — `TODO.md:10247` (`- [x] CQ-29 … DONE via RFC-195`), and
it was **seven**, not five. §5 carries the instructive one forward as a hazard.

The remaining defect is narrower: criterion 2 derives data-access maxima outside
the unified derivation, asymmetrically, and one of its four helpers is dead.

---

## 1. The lead defect: a gate certifying agreement with a dead function

This finding is unchanged from rev 1 and is the strongest one in the document.

`TestRFC195_Criterion2AgreesWithTheProvenBound` (`cardinality_clamp_rfc195_test.go:1176`)
is the instrument RFC-195 booked to keep the criterion-2 fork visible. Its scan
arm calls `scanProvableMaxCard` (`:1191`), which has **zero non-test callers**.

Grep is suggestive; a compile is proof. Renaming it and building the
**production library only**:

```
$ sed -i 's/^func scanProvableMaxCard(/func scanProvableMaxCardRENAMED_DEAD_PROBE(/' planning_cost_model.go
$ bazelisk build //pkg/recordlayer/query/plan/cascades:cascades
INFO: Build completed successfully, 395 total actions
```

**Positive control** — the same harness must break on a function that *is* live,
or the negative result is uninterpretable (a harness defect and a genuine
absence look identical):

```
$ sed -i 's/^func indexProvableMaxCard(/func indexProvableMaxCardRENAMED_CONTROL(/' planning_cost_model.go
$ bazelisk build //pkg/recordlayer/query/plan/cascades:cascades
pkg/recordlayer/query/plan/cascades/planning_cost_model.go:633:23: undefined: indexProvableMaxCard
ERROR: Build did NOT complete successfully
```

Worse than dead, its doc comment asserts a role it lost:

> `planning_cost_model.go:476-477` — "Operates on the bare
> `*plans.RecordQueryScanPlan`, the physical scan expression the memo-descent
> cost walk sees (RFC-184 W2)."

The consequence is exact: **the agreement nobody was checking was never being
checked.** The scan half of criterion 2 could have diverged arbitrarily from the
property and this gate would have stayed green, because the thing it compares
against the property is not the thing that ranks plans.

## 2. The asymmetry is real, and it is LATENT — no production reaching

`countLogicalPlanNode` answers one question — "does this data access prove a row
bound?" — with two policies:

| Arm | Line | Prover | Consults `PlanContext`? |
|---|---|---|---|
| scan | `planning_cost_model.go:618` | `scanPlanProvableMaxCard(scan, ctx)` | **yes** |
| index | `planning_cost_model.go:633` | `indexProvableMaxCard(index)` | **no** |

The index arm reads `p.IsUnique()` / `len(p.GetColumnNames())` off the plan and
abstains when unstamped, while the concrete walk resolves both from context via
`indexMetadata(pl, ctx)` (`:2741`).

**But production never constructs an unstamped index plan.** Every construction
site stamps:

```
$ grep -rn --include='*.go' "NewRecordQueryIndexPlan(" pkg/ | grep -v '_test.go'
aggregate_index_candidate.go:276        -> stampIndexMetadata(c, ...)
match_candidate_index.go:961            -> stampIndexMetadata(c, indexPlan)   (:968)
rule_implement_nested_loop_join.go:4373 -> stampIndexMetadata(cand, ...)
windowed_index_match_candidate.go:330   -> stampIndexMetadata(c, indexPlan)   (:337)
```

`stampIndexMetadata` (`abstract_data_access_rule.go:536-540`) calls
`WithIndexMetadata(cand.GetColumnNames(), candidatePKColumns(cand), candidateUnique(cand))`.

Reachability census, instrumented on both arms (**reported by review, not
independently reproduced here** — see §7.5, which makes it a deliverable):

```
plandiff_test (SQL corpus):  INDEX_SEEN=0  INDEX_DIVERGED=0  SCAN_SEEN=403  SCAN_STAMPED=403
cascades_test (unit):        INDEX_SEEN=2  INDEX_DIVERGED=1  SCAN_SEEN=230  SCAN_STAMPED=0
```

`SCAN_SEEN=403` is the control that makes `INDEX_SEEN=0` interpretable: the walk
is entered 403 times, the index arm is not reached at all.

**Therefore rev 1's "user-visible consequence" paragraph is withdrawn.** There is
no SQL reproducer, and this RFC does not claim one. The asymmetry is a **latent
defect on an unreachable arm**. The fix is dead-arm removal plus latent-defect
closure — worth doing because an unreachable arm is one refactor away from
reachable, and because the arm that is reachable (scan) is proven by the same
census to run *only* through the stamp on the corpus.

### The scan arm, structurally — the argument that actually licenses §4

The census is review-reported and this RFC should not rest on it. The structural
argument is stronger, and rev 3 takes it for **both** arms. Rev 2 verified only
the index arm — the one that is *unreachable* and therefore low-stakes — and left
unverified the scan arm, which is reachable and whose path §4 deletes. That is
the wrong way round, and it is the lesson this revision records.

Every production scan construction site, with its stamping guard:

```
$ grep -rn --include='*.go' "NewRecordQueryScanPlan(" pkg/ | grep -v '_test.go'
rule_primary_scan.go:47          -> :56  WithPrimaryKey(pkVals)  iff ctx != nil && len(recordTypes)==1 && len(pkCols)>0
rule_ordered_primary_scan.go:107 -> :112 WithPrimaryKey(pkVals)  unconditional
primary_scan_match_candidate.go:397 -> :401 WithPrimaryKey(pkVals) iff candidate has PK values
```

The first site's guard is **identical to the ctx fallback's own preconditions**:
`pkFullyEqualityBound` (`planning_cost_model.go:2793-2801`) also requires a
non-nil ctx, exactly one record type, and a non-empty ctx PK list. So a scan that
`rule_primary_scan` leaves unstamped is precisely a scan the ctx path *also*
declines. The second site always stamps; the third stamps whenever the candidate
has PK values, which is the same condition under which the ctx lookup could
succeed.

**Therefore deleting the ctx parameter cannot lose a proof**: there is no
production scan that is unstamped *and* provable via context.

One honesty note on that claim, because rev 3 advertised it as "checkable by
reading three call sites" while it quietly rested on a fourth fact. The two
predicates are evaluated at **different times** — `rule_primary_scan` reads
`call.Context` at rule-application time, `pkFullyEqualityBound` reads `ctx` at
cost time. The identity therefore also requires that these are the same context
for a given plan. They are: both derive from the single `PlanContext` threaded
through one planning run, and there is no path that swaps it mid-run. That is the
fourth fact, now stated rather than assumed; if context threading ever becomes
per-phase, this argument needs re-checking and §7.6's pin is not what would catch
it.

The census remains as corroboration — and `SCAN_STAMPED=403/403` on the SQL
corpus against `0/230` in the unit suite is a clean statement that the
`PlanContext` fallback is a **test-only path**, exercised by unit fixtures that
decline to stamp and by nothing else.

`rfc219_criterion2_asymmetry_probe_test.go` (three tests, all passing, verified
uncached with three `=== RUN` lines under `--test.run=TestRFC219_LogicalWalk`;
an uncached `--test.run=RFC219` over the whole package yields **9** across both
RFC-219 test files) characterises the arm's behaviour. It is
relabelled in rev 2 as a **latent-asymmetry characterisation**, not a bug
reproducer. Its control 2 — the same index plan `.WithIndexMetadata(...)` proves
`max=1` through the logical walk — remains direct empirical support for §4.

## 3. Java: two criteria, not one — and Go's candidate model cannot reach the first

Rev 1 quoted `:315` and cut at `...` exactly where Java's **first** criterion
lives. In full (`CardinalitiesProperty.java:313-355`):

```java
// try to see if the primary key is bound by equalities
if (matchCandidate instanceof WithPrimaryKeyMatchCandidate) {
    final var primaryKeyValuesOptional = ((WithPrimaryKeyMatchCandidate)matchCandidate).getPrimaryKeyValuesMaybe();
    if (primaryKeyValuesOptional.isPresent()) {
        final var primaryKeyValues = primaryKeyValuesOptional.get();
        if (equalityBoundValues.containsAll(primaryKeyValues)) {
            return Cardinalities.atMostOne();
        }
    }
}

if (matchCandidate.isUnique() && matchCandidate instanceof ValueIndexScanMatchCandidate) {
    ... .limit(matchCandidate.getColumnSize()) ...
    if (equalityBoundValues.containsAll(keyValues)) {
        return Cardinalities.atMostOne();
    }
}
return Cardinalities.unknownMaxCardinality();
```

Criterion 1 is **PK-bound**, criterion 2 is **uniqueness**, and criterion 1 falls
through to criterion 2 on a miss. Go's `indexProvableMaxCard` implements only
criterion 2 (`!p.IsUnique() → abstain`).

**Consequence: an index whose scan comparisons bind the primary key but which is
NOT unique proves at-most-one in Java and abstains in Go.** This is a real
divergence, and it is *not* the divergence this RFC set out to fix — routing
criterion 2 through the property as currently written would leave Go short of
Java on the very arm being ported.

Criterion 1 is genuinely live in Java, not dead code: `ValueIndexScanMatchCandidate`
implements `ScanWithFetchMatchCandidate` (`:52`), which extends
`WithPrimaryKeyMatchCandidate` (`ScanWithFetchMatchCandidate.java:44`), so the
`instanceof` matches for index plans. The apparent rebase asymmetry — criterion 2
rebases `indexKeyValues`, criterion 1 does not rebase `primaryKeyValues` — is
**correct, and resolves from source**: `primaryKeyValuesOptionalSupplier` calls
`MatchCandidate.computePrimaryKeyValuesMaybe` (`ValueIndexScanMatchCandidate.java:132`)
→ `ScalarTranslationVisitor.translateKeyExpression`, whose values are rooted at
`Quantifier.current()` (`ScalarTranslationVisitor.java:230`), whereas
`indexKeyValues` are built in the `baseAlias` domain and therefore *must* be
rebased before comparison — criterion 1's block (`:330-333`) contains no
`AliasMap` at all, while criterion 2 rebases at `:342`/`:347`.

The same-domain claim is not merely plausible, it is **provable**:
`equalityBoundValues` are themselves built through
`new ScalarTranslationVisitor(...).toResultValue(Quantifier.current(), getBaseType())`
(`ValueIndexLikeMatchCandidate.java:139-141`) — the identical construction as
`primaryKeyValues`. Both sides of criterion 1's `containsAll` are current-domain
values, so it compares like with like and genuinely fires. Criterion 1's only
`return` is on its hit path (`:334`), so a miss falls through to criterion 2 at
`:339` as described. So the divergence is real.

### Decision: scope criterion 1 OUT, because Go's candidate model forbids its input

Rev 2 claimed porting criterion 1 was "cheap and needs no new plumbing." **That is
refuted.** Criterion 1 asks `equalityBoundValues.containsAll(primaryKeyValues)` —
it needs the scan's comparisons to **bind PK positions**. Java can supply that
because `indexKeyValues` spans index-key *plus* the PK suffix, which is precisely
why criterion 2 must call `.limit(getColumnSize())` to trim the PK back off.

Go structurally cannot, by a documented invariant:

```
match_candidate_index.go:678-681
//  … The PK is NOT part of the sargable surface (see unmatchedFieldsForIndex
// in planning_cost_model.go): unlike Java, Go's candidate never folds the
// PK into sargableAliases, and that invariant must hold.

match_candidate_index.go:689-690
// GetSargableAliases returns the ordered parameter list (one per
// index key column).

match_candidate_index.go:953
comps := make([]*predicates.ComparisonRange, len(c.sargableAliases))
```

`len(comps) == len(sargableAliases) == index key column count`, **by
construction**. No production index plan can carry an equality on a PK position.

Rev 2's proposed evidence compounded the error: `GetPKColumnNames()`
(`index_scan.go:287`) supplies PK *names and arity*, while criterion 1 needs PK
*bindings*. Different data, and the missing half is the half the invariant
forbids. A mutation test asserting "PK-bound but non-unique proves at-most-one"
could only be satisfied by a hand-built plan with more comparisons than index key
columns — a shape production cannot construct. That is rev 1's NAK'd mistake
relocated to a new arm: a both-directions mutation over a fiction.

**So the root cause of this divergence is the sargable-surface invariant, not a
missing criterion**, and it is therefore out of scope for CQ-30. Criterion 1 is
scoped out with that invariant as the stated reason, and pinned as a negative
result (§7.6) so the door stays visibly shut.

The alternative — **(b) fold the PK into the sargable surface** — is the only way
to make criterion 1 reachable, and it is rejected here as a change that belongs to
its own RFC with its own measurement. Its cost is already documented in this
codebase, in one contiguous comment at `planning_cost_model.go:2833-2835`
(rev 3 cited `:2839-2868`, the function body rather than the comment — the quote
below is verbatim and unspliced):

> "Adding the PK suffix here would over-count and penalize a fully-bound index
> probe vs a full scan, mis-ranking criterion #12 toward a full-scan join driver
> (the RFC-069 multiway regression)."

Folding the PK in re-opens a regression this
repo already paid for, to reach a criterion whose shape — an index scan with
every PK column equality-bound — is one a primary-key point lookup already serves.
Doing that inside a cost-model-unification RFC would be exactly the kind of
"smaller change that leaves the architecture incoherent" the ruling forbids, in
reverse: a larger change smuggled in on an unrelated ticket.

### Honesty ledger: Java's plan carries its candidate only during planning

Rev 1's premise sentence was over-broad. The field is assigned only in the
constructor (`RecordQueryIndexPlan.java:246`) via `MatchCandidate.toEquivalentPlan`,
with no setter, and is absent for the legacy planner, for proto-deserialized
plans, deliberately after `minimize()`, and unconditionally for
`RecordQueryTextIndexPlan` — which returns `Optional.empty()`
(`RecordQueryTextIndexPlan.java:176-178`) **while still being in the
`RecordQueryPlanWithIndex` slice `PlanningCostModel:334` iterates.**

The conclusion survives — Cascades planning is exactly when the cost model runs,
so the candidate is present where it matters.

**But abstention is not free, and rev 3 described it as if it were.** The outer
guards at `PlanningCostModel.java:121` and `:125` are disjunctive — "at least one
side is known" — so they tolerate only the case where *both* sides abstain, which
is exactly the case where nothing is at stake. One-sided abstention is
**decisive**:

```java
// PlanningCostModel.java:127-132
if (maxOfMaxCardinalityOfAllDataAccessesA.isUnknown()) {
    return 1;      // A abstains -> A LOSES outright
}
if (maxOfMaxCardinalityOfAllDataAccessesB.isUnknown()) {
    return -1;     // B abstains -> B LOSES outright
}
```

So where Java proves `atMostOne` on a PK-bound non-unique index and Go abstains,
Go does not "stay silent" — **Go loses a tiebreak Java wins.** The plan Java ranks
first on criterion 2, Go ranks last on the same rung, and the comparison never
reaches the structural tie-breakers below it. Rev 3's sentence claiming Go
"inherits that tolerance rather than needing to engineer around it" was false in
precisely the asymmetric case that matters, and is withdrawn.

That is the real cost of the inheritance decided in §3, and §7.11 records it in
the divergence register rather than leaving it in this document alone.

## 4. Decision

**Stamp-only, plan-local derivation: delete the private provers and route
criterion 2 through `properties.CardinalityProver`. `PlanContext` stops being
consulted for cardinality.** Java's PK-bound criterion is scoped out with a
stated structural reason and a negative-result pin (§3, §7.6).

This converts **both** context consumers, and the RFC says so explicitly rather
than leaving the second implicit:

- the **logical** arm's `scanPlanProvableMaxCard(scan, ctx)` (`:618`, `:2337`) —
  its ctx parameter goes;
- the **concrete** arm's `indexMetadata(pl, ctx)` (`:2741-2752`), which is
  **ctx-only, never reads the stamp, and matches candidates by index *name***.
  §2's four-site enumeration is what makes converting it to stamp-only safe:
  every production index plan already carries the same column names and
  uniqueness the name lookup would find. Rev 2 left this arm unstated, resting on
  a census that measures only the logical walk.

Two corrections to rev 1's framing, both of which strengthen the case:

- It is **two** context-taking provers, not four. `indexPlanProvableMaxCard` is
  itself plan-local (signature `(pl, cols, unique)`); context enters only at its
  call site through `indexMetadata`. `scanProvableMaxCard` is dead. So the fork
  is `scanPlanProvableMaxCard`'s ctx parameter plus `indexMetadata`'s lookup.
- The property and the live prover are **the same derivation**, differing only in
  *where primary-key arity comes from*. Both bottom out in
  `properties.ProvenFullEqualityMultiplicity` (`physical_equality_shape.go:250`),
  which already carries the `EqualityBoundsCoverKey` guard (`:256`) and the
  `UnsupportedKnownNaN` guard (`:260`). This is the cleanest statement of the
  fork and a better argument than rev 1's "two independently-coded switches":
  there is one algorithm with two arity sources, and §2's census shows the
  second source is test-only.

This is porting into an established pattern, not new machinery. Go deliberately
does not hold a candidate object on plans; it stamps the *distillations*
properties need, already with Java's empty-candidate semantics —
`WithDistinctRecordsSignal` (`index_scan.go:100-106`, whose comment cites
"Java's empty-candidate default"), `WithCommonPrimaryKey` (`:107-119`, `nil` =
abstain), and `WithIndexMetadata` (`:323`).

### Rejected alternatives

**A. Extend `CardinalityProver` to take a `PlanContext`.** Smallest diff, loses
no proof. Rejected: it bolts a parameter onto a property Java defines without
one and makes a plan's proven bound depend on ambient state — the same "two
disagreeing answers" defect CQ-30 exists to remove, relocated. §2's census kills
it independently: the parameter would preserve a path measured to be test-only.

**B. Collapse onto the plan-local property as it stands today.** This *is* the
chosen option. Rev 2 rejected it pending a criterion-1 port; §3 shows that port
has no constructible input in Go, so the collapse is correct as-is. The residual
gap against Java is real, is caused by the sargable-surface invariant rather than
by this RFC, and is pinned rather than silently inherited.

**C. Keep both derivations, document the divergence.** Rejected: "Go's
substitute for X" is an admission that X is the answer.

**D. Delete the dead `scanProvableMaxCard` and stop.** Rejected: it makes the
gate honest without making the model correct, and a green gate over a corrected
pointer would certify agreement while §2's asymmetry stays live.

## 5. Hazard class: fix the arm the model *prefers*

CQ-29's seventh shape is a standing hazard, not a closed bug. The zero-collapse
fix landed on `RecordQueryRecursiveLevelUnionPlan` and not on
`RecordQueryRecursiveDfsJoinPlan`, which "proved nothing" — and the DFS join is
**the arm the cost model prefers**, since the level union carries a strictly
larger buffer term by construction. Measured on identical children,
`FlatMap(scan, dfsJoin)` costed 0 rows against `FlatMap(scan, levelUnion)` at 1e6.
A fix applied to the losing arm is invisible; the winning arm keeps the bug.

This RFC is exposed twice: a twin pair (scan/index) and a walk pair
(logical/concrete), each with one side unrouted.

**Verification items 3 and 4 only NAME this hazard; §7.5's census CLOSES it.**
Unit assertions that both arms share one derivation cannot detect that an arm is
never *reached* — which is precisely the hole rev 1 fell into.

## 6. A hypothesis this RFC does not rest on

Before designing, I predicted the ctx-fallback arm was untested. **Refuted by
mutation** — setting `pkLen = len(ctx.GetPrimaryKeyColumns(...))` to `pkLen = 0`
reddened four tests:

```
--- FAIL: TestLogicalCounts_PrimaryKeyContextFallback
--- FAIL: TestConcreteJoinCost_CompositePKPrefixNotPointProbe
--- FAIL: TestPKFullyEqualityBound/context_fallback_rejects_composite_PK_partial_prefix
--- FAIL: TestPKFullyEqualityBound/context_fallback_accepts_composite_PK_fully_bound
```

Rev 1 then drew the wrong inference: that these four become acceptance criteria
to preserve. **They do not.** Coverage of a path is not an argument for keeping
the path — and §2's census shows this path is test-only, so the tests are
covering a road that leads nowhere production goes. §7.2 states what actually
becomes of each.

## 7. Verification

1. **Re-point the agreement gate at what production calls**, with a non-nil
   context, failing if any criterion-2 prover is reachable that the property does
   not also answer. Its own mutation control: perturb one prover's answer, the
   gate must redden.

2. **Per-test disposition of the four §6 tests** (rev 1 wrongly said "pass
   unchanged"; they test the deleted path):

   | Test | Disposition |
   |---|---|
   | `TestPKFullyEqualityBound/"context fallback rejects composite PK partial prefix"` | **Deleted.** Its stamp-based twin already exists and passes: `"stamped composite PK partial prefix bind is not full"` (`rfc186_pk_gate_test.go:77`). No coverage lost. |
   | `TestPKFullyEqualityBound/"context fallback accepts composite PK fully bound"` | **Deleted.** Twin already exists: `"stamped composite PK fully bound with nil ctx"` (`:87`). No coverage lost. |
   | `TestPKFullyEqualityBound/"stamped arity wins over conflicting context"` (`:119`) | **Converted** to "stamped arity is the only source" — the conflict it arbitrates cannot arise once no context is consulted. |
   | `TestPKFullyEqualityBound/"unstamped multi-type scan does not use first type context"` (`:144`) | **Promoted** to the pin that the ctx door stays shut: strengthened to assert *every* unstamped scan abstains, with a failure message naming what gets re-armed if a context source returns. |
   | `TestLogicalCounts_PrimaryKeyContextFallback` (`rfc190_scan_hint_cost_test.go:116`) | **Converted** to a stamp-based equivalent asserting the same two outcomes (partial prefix → unbounded; full PK → max 1) from the stamp. |
   | `TestConcreteJoinCost_CompositePKPrefixNotPointProbe` | **Converted** to the stamp-based equivalent; the composite-prefix-is-not-a-point-probe assertion is preserved, its arity source changes. |

   That two of the six already have passing stamped twins is itself evidence for
   §4: the stamp path is the tested one.

3. **Twin-pair completeness** — scan and index arms of one walk answer through
   the same derivation, driven for both (§5 hazard, naming half).

4. **Walk-pair completeness** — logical and concrete walks agree on every
   data-access shape, both directions.

5. **The reachability census, committed as an instrument** (§5 hazard, closing
   half). It drives **both arms of both walks from explicit state** rather than
   from whatever the corpus happens to reach, and it must be reproduced by me
   before merge — §2 currently cites it as reported, which is a gap this item
   closes. **Guard direction has already inverted:** an `INDEX_SEEN` floor is
   *unsatisfiable today*, so the alarm is now **growth, not collapse** — a
   non-zero `INDEX_SEEN` on the SQL corpus means an unstamped index plan became
   constructible and the latent arm is live again. The `SCAN_SEEN` floor stays a
   collapse guard: a zero there means the instrument died. Both directions go in
   the failure messages.

6. **Java criterion 1 is scoped out, pinned as a negative result** (§3). Rev 2's
   both-directions mutation is **deleted**: its input shape — an index plan with
   more comparisons than index key columns — cannot be constructed by production
   code, so the mutation would have run over a fiction.

   What replaces it is `TestRFC219_IndexPlanWidthInvariant`, a pin on the
   invariant that makes criterion 1 unreachable. Rev 3's version cited **one**
   site and claimed to cover "every production index-plan construction path".
   There are four, enforcing width by **three different mechanisms**, and only
   the first gives equality:

   | Site | Mechanism | Relation | Measured |
   |---|---|---|---|
   | `match_candidate_index.go:953` | `make(..., len(c.sargableAliases))`, "one per index key column" (`:689-690`) | `==`, structural | 3 / 3 |
   | `windowed_index_match_candidate.go:322` | `make(..., len(c.GetSargableAliases()))` = groupingAliases + rank; `columnNames` is an **independent** constructor parameter (`:80-81`) with no clamp | `<=`, **caller contract** | 2 / 2 |
   | `aggregate_index_candidate.go:268-276` | appends at most `physicalGroupingPrefixCount`, **clamped at construction** to `len(groupCols)` (`:137-138`); `GetColumnNames()` returns `groupCols` (`:221`) | `<=`, structural | 2 / 2 |
   | `rule_implement_nested_loop_join.go:4373` | hardcoded single-element slice; `candCols` comes from `plainFieldColumnsForShortcut`, which returns `c.columnNames` verbatim (`match_candidate_index.go:1135`), and the loop's `len(candCols) == 0 { continue }` guard (`:4337`) forces `len(GetColumnNames()) >= 1` | `<=`, structural | 1 / 2 |

   So the relation actually meant is **`len(comps) <= len(GetColumnNames())`**,
   not the equality rev 3 wrote. The test drives all four real construction paths
   with a prefix map binding every sargable alias (maximally wide comps), plus a
   **positive control** that hand-builds an over-wide plan (`comps=2`,
   `columnNames=1`) and confirms the predicate fires — without which four green
   subtests are uninterpretable. The aggregate arm additionally proves its clamp
   rather than assuming it, by passing `physicalGroupingPrefixCount = 5` into a
   2-group-column candidate and observing it truncated to 2.

   **Residual, stated rather than buried: three of the four are structural; the
   windowed site is the one genuine caller contract.** Its `columnNames` and
   `groupingAliases`/`rankAlias` are independent constructor parameters with no
   clamp relating them, so a caller passing a short `columnNames` against many
   grouping aliases would violate the invariant and nothing in the constructor
   would stop it.

   That arm is also **unreachable today**, which strengthens the scope-out rather
   than weakening it: `NewWindowedIndexScanMatchCandidate` has **no non-test
   caller** —

   ```
   $ grep -rn --include='*.go' "NewWindowedIndexScanMatchCandidate(" . | grep -v ^./fdb-record-layer/
   windowed_index_covered_ordinal_test.go:27
   windowed_index_covered_ordinal_test.go:143
   rfc219_index_width_invariant_test.go:152
   windowed_index_match_candidate.go:77          (the definition)
   ```

   so its `ToScanPlan` is production *code* on a currently unreachable path and
   its unclamped parameter space is not exploitable. The pin's windowed arm
   therefore measures the canonical rank-index shape, **not** an observed
   production one, and earlier revisions calling it "the honest production shape"
   were wrong. If a production caller ever appears, this is the first arm to
   re-check.

   The failure message names both consequences of a breach: folding the PK into
   the sargable surface makes Java's criterion 1 reachable and this RFC's
   scope-out stale, and it re-opens the `unmatchedFieldsForIndex` / RFC-069
   mis-ranking. This is a negative result and it is load-bearing — it is the sole
   justification for deliberately inheriting a real Java divergence, so a
   quarter-surface pin was a pin in name only.

7. **The dead function is gone**, pinned by a test that no criterion-2 prover
   exists outside the property.

8. **Corpus plan-diff — expected result is ZERO movement** (see §8).

9. **1M stress comparison — GATE CLOSED. This run is NOT part of this RFC's
   verification at current headroom.** `df -h /home` on 2026-08-08 reports
   **45G avail / 96% used** — past the ~95% line where ext4 point-lookup latency
   degrades and reports as a planner regression, and worse than both the 61G/94%
   and 49G/95% measured earlier in review. The trend is downward, so this is not
   a "re-check before running" item. It is reopened only when free space recovers
   to real headroom; until then a baseline would measure the disk, not the
   change. Baseline must then live in a sibling worktree on the same filesystem
   (never `/tmp`, never another disk).

10. `just test` green.

11. **The divergence register is corrected.** `DIVERGENCES.md` classified
    criterion 2 as "Functionally equivalent" — which, once this RFC deliberately
    inherits Java's criterion-1 gap, actively misleads a future reader into
    mistaking this ACK for parity. The row is reclassified to **Deliberate
    divergence** with the sargable-surface invariant as the cause and §3's
    ranking consequence (`PlanningCostModel.java:127-132`, the abstaining side
    loses outright) stated, plus a `#### Criterion 2` detail section matching the
    existing criterion 6 and 7 entries. This is the single change that most
    protects a future reader, because it is the file they will consult *instead
    of* this RFC. **Done in this branch** — it is evidence for the lap, not a
    post-merge chore, since the whole scope-out rests on it being recorded.

## 8. Prediction (inverted from rev 1), and risk

Rev 1 predicted plan movement — "criterion 2 will now prove bounds for unique
index probes it previously abstained on." **With `INDEX_SEEN=0` that prediction
is wrong. The correct prediction is ZERO golden movement**, and this RFC commits
to it as a falsifiable pass/fail rather than a per-golden justification exercise:

- **Zero changed goldens → pass.** Consistent with the census: the deleted paths
  are unreachable from SQL, and the scan arm already runs stamp-only (403/403).
- **Any changed golden → the RFC is wrong somewhere**, and the change is not
  re-blessed. It is explained first. Rev 2 listed a criterion-1 explanation here;
  that is **void** under §3, since criterion 1 is scoped out and has no
  constructible input. So exactly one candidate explanation remains: a
  construction site the §2 index enumeration or the §2 scan enumeration missed —
  i.e. a plan reaching a walk unstamped. That single-candidate property is what
  makes this prediction useful: any movement points at one specific class of
  mistake, in code, not at a judgement call about plan quality.

That second bullet is the real value of inverting the prediction: rev 1's framing
would have absorbed either outcome as "expected movement, justified per golden."
This framing cannot, and a plan that got worse can no longer hide inside a
re-bless.
