# RFC-189 — Cascades quality review residuals (2026-07-22 findings)

**Status:** ACKED (rev 2), IMPLEMENTATION IN PROGRESS. Graefe ACK (two A4 conditions folded below +
hardening notes adopted); Torvalds conditional ACK (conditions 2/3/4 folded; condition 1 — physical PR
split — reconciled below within one PR per owner directive). Final impl HEAD gets one joint milestone
review lap.

**Implementation progress (branch `feat/rfc189-cascades-review-residuals`):**
- **DONE (landed, each RED→GREEN + full 56-target FDB suite green per commit):** WS-A correctness
  cluster — A1 (finding 8, hang), A2 (finding 5, signed-zero), A3 (finding 9, projection), A4 (finding 7,
  correlation); WS-B safe plan-quality — B1 (M3 point-scan cardinality, + a `WithPrimaryKey` builder
  field-drop footgun fixed en route), B2 (M4-fu-3 VERSION fan-out); WS-C value-layer — C1 (12a int
  precision + panic guard), C2 (12b CAST trim set), C3 (12c Union `IsNeeded` assert), C4 (12d EXISTS
  dedup); WS-D dead-function deletes (Demote, findMatchingReachableCandidate wrapper, isExploratoryMember
  dup) — the intersection-rule deletion was RECLASSIFIED to KEEP (see WS-D below); WS-F — F2 (real
  RemoveRangeOneRule port), F3 (IndexScanPreference wired into the config mirror); **B3** (M5 structural
  common-PK — the INDEX arm re-ported structurally via a new `KeyExpression→[]Value` translator, threaded
  IndexDef→candidate→plan; validated by rowdiff+plandiff+1M-stress, no flip / no dropped rows). GetPlans
  deprecate-lite: no change needed (already documented + the aligned `GetPhysicalExpressions` twin).
- **E2 (dense producer) — AUDITED then REVERTED (kept booked):** Java-faithful in isolation but it
  regresses designated survivors (the dense tree-depth tiebreak fires the predicate-count rung before a
  simplicity rung → prefers a deeper Sort(Scan) over a plain Scan). plandiff/rowdiff/1M-stress were green
  (final plans unaffected on the corpus) but the designation-invariant unit tests caught it — the per-flip
  audit protocol working as intended. Root cause is Go's designated-comparator RUNG ORDER, not the producer
  density; the proper fix needs Java rung-order verification. Low value; booked (as RFC-188 deliberately did).
- **REMAINING (large / review-gated — not to be rushed):** **B3 item-d** (scan-arm structural conversion —
  a B1 cardinality-count ripple, booked); **WS-D MatchIntermediate** permutation-enumeration port (medium,
  edge-case optimization); **F1** (finding 11 OR-expansion PLANNING phase relocation + mapping-kind
  taxonomy — Graefe-gated infra); **E1** (M4 Fetch-storedRecord, ~48 flips — the DANGEROUS dropped-dedup
  direction, needs the full per-flip duplicate-bearing audit + reviewer sign-off; E2's outcome shows these
  arms can regress). Then the milestone review lap (Graefe+Torvalds+codex+@claude) + PR.

**Rev-2 review folds:** A4 — carry the `dep != q.GetAlias()` self-filter onto the now-live
`rule_partition_select.go:674` and drop the redundant manual walk (Graefe); correct the
`in_like_select:141` text — it is a structural no-op regardless of the fix (Graefe). A1 — add a
bounded `visited` guard to the two cycle-guard-free consumers (`GetCorrelatedTo`,
`childRefsMatchInMemo`) as defense-in-depth, since Go's cross-group merge breaks Java's
acyclic-by-interning guarantee (Graefe). B3 — return an empty Values list, not nil, for a null PK
(Java `Optional.of(emptyList())`) so `ValuesStructurallyEqual` stays consistent (Graefe). C2 — the
test must use a code point in (U+0020, Unicode-WS], e.g. NBSP U+00A0 (Torvalds — ASCII space trims
in both engines). E1 — each elision-class flip needs a **duplicate-bearing** row-level cross-engine
assertion, not count-parity on random data (Torvalds). F1 — the gate keys on *that* OrPredicate
being index-matched, not merely any match (Graefe). F3 — wire `IndexScanPreference` into the
existing `PlannerConfiguration` mirror (consulted by the cost model), not a bespoke dead knob
(Torvalds); if that surface can't reach it, drop it.

**Packaging (Torvalds condition 1, reconciled).** Owner directive is one PR (chosen after the split
was explicitly offered); Graefe rules bundling E1/E2 Cascades-acceptable ("risk is review fatigue,
not correctness"). Torvalds' correctness concern — a bad plan flip must not block the A1 hang / A2
memo fixes — is met *within* one PR by ordering: workstreams A–D/C/F land as the first commits;
**E1/E2 land last as self-contained, independently-revertable commits.** A failed per-flip audit
reverts only those commits (`git revert`), leaving the correctness fixes on the branch to merge. This
gives the "independently releasable" property Torvalds requires without a physical split. Re-presented
to Torvalds at the milestone lap; escalate to the owner if the packaging NAK holds.
**Tracks:** TODO.md "FINDINGS 2026-07-22" — the residual findings after systemic problems A
(RFC-187, leaf-name column matching) and B (RFC-188, cost-model soundness) shipped. Covers the
latent correctness/hang cluster (findings 8, 5, 9, 7), the safe-direction cost/property follow-ups
(M3, M4-fu-3, M5), the value-layer/compensation gaps (finding 12), the dead-code/missed-match
cleanup (finding 13), and — per owner directive, in ONE PR with full per-flip validation — the two
plan-flip optimization surfaces (M4 Fetch-storedRecord, finding-6 dense producer) and the
rule/config parity items (finding 11, RemoveRangeOne, IndexScanPreference).
**Baseline:** master at `bf5582c39` (RFC-188 merged). Java source at `fdb-record-layer/` tag 4.12.11.0.
**Work branch:** `feat/rfc189-cascades-review-residuals` (one mega-PR, owner directive 2026-07-22).

**Why this matters.** Two of these are the "green-CI-hides-it" class the CLAUDE.md names as the real
danger: **finding 8 is a latent planner HANG** (a hang is worse than a wrong row) and **finding 5 is
a memo-interning correctness defect** (equal values that hash apart). Finding 9 is a latent
wrong-projection on the covering-index path. None of the read-side plan-choice items change
key/record/index/continuation *format*; plan choice is wire-visible only for cross-engine
continuation resumption. The CORE (epoch termination, memo merge/cycle guards, 3-tier interning,
3VL null logic, directional cost rungs) was traced sound in the review — every defect here is in the
periphery/derivations, and each fix is either a Java-faithful port or the deletion of a Go-only
divergence.

**Review unit.** Umbrella RFC: workstreams are the review granularity. RFC gets one Graefe+Torvalds
ACK before implementation; the whole implementation gets ONE joint milestone review lap at the end
(Graefe + Torvalds + codex + @claude), findings folded, final HEAD delta-reconfirmed. Green tests per
commit in between; no per-commit reviewer laps.

---

## Workstream A — latent correctness / hang cluster (findings 8, 5, 9, 7)

### A1. Finding 8 (latent planner HANG) — merge cycle guard walks Members(), not AllMembers()

**Go:** `memo_merge.go` — `reachable` (the cross-group-merge cycle guard used by `mergeable`) recurses
over **exploratory members only**: the inner walk `for _, mem := range r.Members()` (`:116`) and the
outer seed `for _, mem := range from.Members()` (`:125`). `Members()` (`expressions/reference.go:263`)
returns `r.members` — exploratory only. But the two consumers that traverse the merged DAG walk
**finals** and have **no** cycle guard: `Reference.GetCorrelatedTo` (`reference.go:772`) iterates
`AllMembers()` (exploratory + final) and recurses into child refs unbounded with no `visited` set;
`childRefsMatchInMemo` (`expressions/memo_equal.go:142-153`) recurses `a.Members()` **and**
`a.FinalMembers()`, and its own comment (`memo_equal.go:131-134`) explicitly relies on "the cross-group
merge guard in memo_merge.go forbids creating a cycle."

**Java:** has **no** cross-group merge — the merge/`reachable`/`mergeable` machinery is a Go-only
extension (RFC-037, `memo_merge.go:7-12`); Java stays acyclic by interning at insert time
(`Reference.java:314` `insertFinalExpression`, `:466` `containsInMemo`). Java's DAG *consumers*
enumerate **all** members: `getCorrelatedTo` walks `getAllMemberExpressions()` (`Reference.java:474-480`),
`containsAllInMemo` checks exploratory **and** final (`:439-440`). So the traversal surface Java uses is
AllMembers — matching Go's *consumers*, not Go's *guard*.

**Root cause:** `reachable` is sound today only by an **undocumented REWRITING invariant**: the sole
REWRITING path that inserts a final is `FinalizeExpressionsRule` (the only `rewritingImplRules` entry,
`planner.go:168`), which promotes the **same exploratory pointer** into finals
(`rule_finalize_expressions.go:40-45`; `physical_wrapper.go:293` "promotes the SAME pointer into both
sets"). Every *distinct* `InsertFinal` fires during PLANNING, where `mergeable` short-circuits on
`m.planningActive` (`memo_merge.go:91`). So during REWRITING every final child is already an exploratory
child → `Members()` reachability == `AllMembers()` reachability. **One distinct-final `InsertFinal`
during REWRITING breaks it:** `reachable` misses the final edge, `mergeable` approves a cycle, and the
next `GetCorrelatedTo`/`childRefsMatchInMemo` spins forever.

**Fix:** in `reachable`, replace both `Members()` with `AllMembers()` (`memo_merge.go:116` and `:125`).
This aligns the guard's enumeration with the exact surface its consumers traverse — the
emergent-property fix (design principle 10), not a downstream `if cycle {}` check. Near-zero cost:
`AllMembers()` returns `r.members` directly when finals are empty (`reference.go:280-282`), which is the
REWRITING hot path; it allocates a fresh slice only when finals are non-empty (`:283`).
**Defense-in-depth (Graefe hardening note, adopted):** because Go's cross-group merge is a Go-only
extension that breaks Java's acyclic-by-interning guarantee, the guard is the *only* thing keeping the
two cycle-guard-free consumers safe — `GetCorrelatedTo` (`reference.go:772,796`) sets its cache only at
the end of the recursion, so a cycle recurses forever, and `childRefsMatchInMemo`
(`memo_equal.go:142-153`) has no guard at all. Add a bounded `visited` set to both so a
guard-under-approximation (or any future merge bug) degrades to a wrong/incomplete result, never a
hang. A hang is worse than a wrong row; the guard fix removes the trigger, the consumer guards remove
the failure *mode*.

**Reachability:** purely latent today (no REWRITING rule inserts a distinct final). The existing
`TestMemoMerge_SkipsCyclicMerge` (`memo_merge_test.go:128`) routes its ancestor edge through an
*exploratory* member, so it passes under both the buggy and fixed guard — it cannot catch this.

**Test:** `TestMemoMerge_SkipsCyclicMergeThroughFinal` — mirror the existing test but route the ancestor
edge through a **final**: build `inner` over `scanRef`; `outer` whose *exploratory* child is `scanRef`
(so `reachable(outer,inner)` over `Members()` cannot reach `inner`); `outer.InsertFinal(Distinct(→inner))`
(the distinct-final edge); then `Integrate(outer, Distinct(→scanRef))` so `findEquivalentRef` matches
`inner`. Assert `MergeCount()==0` and `outer.Canonical()!=inner.Canonical()`. Current code merges
(`MergeCount()==1`) → RED; fixed code walks the final via `AllMembers()`, skips → GREEN. The buggy run
fails the assertion cleanly (the merge itself doesn't hang), so the suite reports a failure rather than
wedging.

### A2. Finding 5 (memo dedup correctness) — scalar signed-zero: equal-but-different-hash

**Go:** `values/map_field_values.go:254` `constantValuesEqual` scalar fallthrough `return a == b`
(reached via `EqualsWithoutChildren` → `*ConstantValue` arm `:344`). For boxed `float64`/`float32`,
Go's `==` says `-0.0 == +0.0` → **true**. The semantic hash (`values/semantic_hash.go:156`,
`const:%T=%v`) renders `-0` ≠ `0`. So two ConstantValues **compare equal but hash apart** → the memo
interning invariant (equal ⟹ same hash) is violated → a duplicate member is inserted (or an intern
miss). The `[]float64`/`[]float32` arms (`:230-253`) already fixed exactly this with
`math.Float64bits`/`Float32bits`; the scalar path was missed.

**Java:** `LiteralValue.equalsWithoutChildren` (`:116`) → `Comparisons.evalComparison(EQUALS,…)` →
`toClassWithRealEquals(v).equals(…)` = `Double.equals` (doubleToLongBits): `-0.0 ≠ +0.0`. Its hash is
bit-based too (`objectPlanHash` → `Double.hashCode`), so Java is self-consistent and treats signed
zeros as **unequal**. The bitwise fix is therefore **Java-aligned**, not Go-only plumbing — Go's `==`
is the anomaly.

**Fix:** before `map_field_values.go:254`, mirror the array arms — `float64`/`float32` type-switch
comparing `math.Float64bits`/`math.Float32bits`, else `return a == b`. Signed zeros become unequal
(matching Java and the `%v` hash); identical-bit NaN equal and hash-coherent; differing-bit NaN unequal
sharing a bucket (an allowed collision, same as the array-arm comment). No wire impact — memo-internal
semantic identity only, not continuation/plan hash.

**Test:** `TestConstantValue_SignedZeroEqualsIsHashConsistent` (values pkg unit): `a =
&ConstantValue{math.Copysign(0,-1)}`, `b = &ConstantValue{0.0}`; pin the invariant
`EqualsWithoutChildren(a,b) ⟹ semanticHash(a)==semanticHash(b)` (pre-fix: equal=true, hashes differ →
RED). Controls: two `+0.0` and two identical-bit NaN → equal AND same hash. Plus a memo/Reference insert
asserting no false-duplicate.

### A3. Finding 9 (latent wrong projection) — TranslateQueryValueMaybe self-pull-up

**Go:** `max_match_map.go:771` (inside `TranslateQueryValueMaybe`) calls
`values.PullUpValue(entry.candidateValue, entry.candidateValue, candidateAlias)` — the pull-up ROOT (2nd
arg) is the candidate PART itself, so `PullUpValue`'s case-1 self-equality (`pullup.go:24`) **always
fires** and every part collapses to `QOV(candidateAlias)`. The RecordConstructor field-selecting branch
is never reached.

**Java:** `MaxMatchMap.java:238-243` pulls each candidate part up relative to the **ROOT** candidate
value: `candidateValue.pullUp(ImmutableSet.copyOf(mapping.values()), …, candidateAlias)`, then (`:254-261`)
each entry's part is looked up in that map → pulled up against the root, becoming a distinct field
reference (`FV("col0")`, `FV("col1")`).

**Root cause:** self-referential pull-up. Masked whenever the matched part **equals the whole root**
(single-column, QOV passthrough, identity) — where `QOV(alias)` is coincidentally correct — which
covers every current test. Detonates when `m.candidateValue` is a **multi-field
`RecordConstructorValue`** (a covering-index result value) with mapping entries whose parts are distinct
proper sub-fields: correct `FV(a)→FV(col0)`, `FV(b)→FV(col1)`; buggy both → `QOV(alias)`, so every
projected column returns the whole record / a wrong column (column-swap, duplicate-column mis-projection).

**Fix:** pull each part up against the root — `values.PullUpValue(entry.candidateValue, m.candidateValue,
candidateAlias)`; 1:1 with Java, precompute `values.PullUpValues([]Value{parts…}, m.candidateValue,
candidateAlias)` once before the loop (`PullUpValues` already exists, `pullup.go:216`, used in
`rich_ordering.go:686` — unused here, which is the substance of the finding). **Also:** the nil-fallback
branch (`max_match_map.go:772-783`, structural-equal → `QOV`) is currently **dead** (case-1 never returns
nil); after the fix it goes live, and Java has **no** such fallback — it returns `Optional.empty()` if any
part fails to pull up (`MaxMatchMap.java:258-259`). Delete the Go fallback to match Java's fail-closed
behavior (a translate that can't pull up a part must fail, not silently emit `QOV`).

**Reachability:** SQL-reachable via a covering index projecting ≥2 columns — `PullUpValueMaybeWithEquivalence`
(`pullup.go:69-83`) calls `TranslateQueryValueMaybe` with `pullThroughValue = expr.GetResultValue()`,
which for a covering index is a multi-field RecordConstructor; `AdjustMaybe` (`rule_adjust_match.go:274,350`)
is the other caller. Latent today because current covering-index tests exercise single-column /
whole-record projections.

**Test:** unit — `queryValue = RC(a=FV("x"), b=FV("y"))`, `candidateValue = RC(col0=FV("x"),
col1=FV("y"))` (candidate output names differ so projection is observable); `TranslateQueryValueMaybe(alias)`
must equal `RC(a=FV("col0"), b=FV("col1"))`. Buggy yields `RC(a=QOV, b=QOV)` → RED. Add a swap variant
(`candidate = RC(col0=FV("y"), col1=FV("x"))`) to prove ordinal correctness. A yamsql/FDB covering-index
test projecting ≥2 columns pins it end-to-end (wrong/duplicated output columns), but the unit test is the
reliable regression.

### A4. Finding 7 (correlation under-approximation) — Quantifier.GetCorrelatedTo() returns empty

**Go:** `expressions/quantifier.go:250` returns an empty set. The transitive walk exists on the
Reference (`reference.go:766`, recursing via `q.GetRangesOver()`), but the quantifier accessor
short-circuits to `{}`.

**Java:** `Quantifier.java:711` `getCorrelatedTo()` → `correlatedToSupplier` (`:102`) =
`getRangesOver().getCorrelatedTo()` — transitive, non-empty. Under-approximation is the **dangerous
direction**: a correlated leg reported as free-standing (0-row class) lets any consumer treat it as
uncorrelated.

**Fix:** delegate — `if ref := q.GetRangesOver(); ref != nil { return ref.GetCorrelatedTo() }` else empty
(`Reference.GetCorrelatedTo` already filters own-member aliases; `q.alias` is bound at the parent, not
inside the ranged-over ref, so no self-filter needed).

**Reachability:** both current quantifier-direct call sites are already compensated, so returning empty
is load-bearing-safe *today* — but each needs care once the accessor returns real correlations (Graefe
A4 conditions):
- `rule_partition_select.go:674` becomes **live**. Its sibling walk (`:702-708`) deliberately applies a
  `dep != q.GetAlias()` self-filter, which the `:674` consumer does not — and given Go's documented
  quantifier alias-reuse (`reference.go:781-785`), "redundant-but-harmless" is **unproven**. **Fix:**
  carry the `dep != q.GetAlias()` self-filter onto the `:674` use of the now-real correlation set and
  **remove** the now-redundant manual walk at `:702-708` (one derivation, not two). This is the substance
  of the A4 fix, not an afterthought.
- `rule_push_requested_ordering_through_in_like_select.go:141` is a structural **no-op** (`_ = alias`,
  result unused) **regardless** of this fix — the real ordering check runs downstream in
  ImplementInJoin/InUnion. The fix does **not** turn it into a real guard (the earlier draft's claim was
  wrong); it stays a no-op. Leave the line as-is (or delete the dead `_ = alias`); do not wire a reject
  here (that would be a separate change with over-reject risk).

It IS a Cascades change (the quantifier's correlation contract), so it trips the Graefe gate; "contained
today, latent trap" is accurate — any new consumer calling `q.GetCorrelatedTo()` (as Java code freely
does) silently gets `{}`.

**Test:** `TestQuantifier_GetCorrelatedTo_Transitive` — a quantifier ranging over a Reference whose member
correlates to external alias `X` (Select with predicate `QOV(X).f`); assert `q.GetCorrelatedTo()` contains
`X` (pre-fix empty → RED). Model on `expressions/correlation_test.go`.

---

## Workstream B — safe-direction cost/property parity (M3, M4-fu-3, M5)

### B1. M3-followup — bound equality-bound primary scans in the whole-plan-cardinality guard

**Go:** `cardinality.go:404-405` — the `RecordQueryScanPlan` arm always returns `UnknownMaxCardinality`.
Yet `scanProvableMaxCard` (`planning_cost_model.go:459-481`) already bounds a full-coverage all-equality
scan at 1. So `wholePlanMaxCardinalityKnown` (`:416-426`, which drives off `computeCardinalities`)
under-reports a full-PK-equality primary scan (a point lookup) as unknown, and the M3 outer guard
over-abstains for a bounded-primary-scan-vs-unbounded comparison where Java applies criterion #2.

**Java:** `CardinalitiesProperty.visitRecordQueryScanPlan` (~`:512-536`) returns `Cardinalities(0,1)` when
`ordering.getEqualityBoundValues().containsAll(primaryKeyValues)`.

**Fix (~5 lines):** in the scan arm, reuse `scanProvableMaxCard(p)` exactly as the index arm does
(`cardinality.go:407-423`): if it proves a bound, return `Cardinalities{Min:0, Max:1}`. The TODO's
"thread ctx for PK count" is unnecessary — `scanProvableMaxCard` already proves full coverage via
`numBound == len(comps) && allEquality`; cross-check `len(comps) == len(p.GetPrimaryKeyValues())`
(`scan.go:94`) for full-PK coverage.

**Reachability / flip risk:** none — safe direction (only tightens an abstention); RFC-188 confirmed zero
corpus impact.

**Test:** `TestComputeCardinalities_PrimaryPointLookupBounded` (all-PK-equality scan → max=1;
partial/range → unknown), plus extend `TestWholePlanMaxCardinalityKnown` to prove the guard now
recognizes the point lookup.

### B2. M4-followup-3 — narrow the non-VALUE fail-closed; audit value-candidacy vs Java

**Go:** `index.go:76-95` `CreatesDuplicates`; `:91-93` blanket `if i.Type != IndexTypeValue { return
true }` — fails closed to duplicate-producing for ANY non-VALUE index type reaching a value-scan
candidate.

**Java:** VALUE, **VERSION**, **RANK**, **PERMUTED_MAX/MIN** all build a `ValueIndexScanMatchCandidate`
via `expandValueIndexMatchCandidate`, and for each `createsDuplicates =
index.getRootExpression().createsDuplicates()` (the real fan-out over the root key expression) — so a
scalar VERSION index is **distinct**. TEXT/multidimensional genuinely fan out; ATOMIC_MUTATION and VECTOR
are not value candidates.

**Disposition:** the finding's Question B ("should Go EXCLUDE these non-VALUE types from value
candidates?") is the **wrong direction** — Java *includes* VERSION/RANK/PERMUTED, so excluding them would
lose query reach, not close a divergence. The real divergence is the blanket fail-closed over-reporting
`true` for the Java-admitted value-candidate types. **Fix (Question A, safe/optimization-only):** compute
the real `createsDuplicates(RootExpression)` for VALUE/VERSION/RANK/PERMUTED, and keep the fail-closed
`true` only for types whose scans genuinely emit multiple entries per record (TEXT tokenization,
multidimensional) and for unrecognized root expressions (preserving finding-10-M4-followup-2's fail-closed
default). First verify Go actually constructs value candidates for VERSION/RANK (gating lives upstream of
`plan_context_builder.go:102`); if Go does not admit them today, this narrows to VALUE + a booked
note that admitting them is separate reach work.

**Reachability / flip risk:** safe (identical rows cross-engine; at most a redundant DISTINCT is removed).
No corpus flip unless a VERSION/RANK/PERMUTED value candidate is reachable.

**Test:** `TestIndex_CreatesDuplicates_VersionScalarIsDistinct` (scalar VERSION → false; TEXT/fan-out →
true; unrecognized root → true).

### B3. M5-followup — structural common-PK for index scans (re-port the reverted M5)

**Go:** `plan_properties.go:216-259` `computePrimaryKey`; the index arm returns `nil` (`:223-234`) —
RFC-188 reverted M5 to nil because the by-column-NAME PK wrongly equated record types whose PK
*expressions* differ but share field names (`Field("ID")` vs `Concat(RecordTypeKey(), Field("ID"))` both
flatten to `["ID"]`), which would let `ImplementDistinctUnionRule` dedup two legs that must both survive
(dropped rows). The plan (`index_scan.go:59`) carries only `pkColumnNames []string` (bare names), and
that field is empty for fan-out indexes because it doubles as the ordering suffix.

**Java:** `PrimaryKeyProperty.visitIndexPlan` (`:293-295`) =
`ScalarTranslationVisitor.translateKeyExpression(indexPlan.getCommonPrimaryKey(),
resultType.getInnerType())` — `translateKeyExpression` (`:221-232`) normalizes the key for positions and
expands each component to a `Value` that **encodes structure** (record-type-key prefixes, nesting), not
bare names. `commonPrimaryKeyMaybe` (`:138-150`) compares with `.equals()`.

**Fix (medium — plan-metadata plumbing):** plumb the index's common-PK **KeyExpression** onto
`RecordQueryIndexPlan` as a **new field separate from `pkColumnNames`** (e.g. `commonPrimaryKeyValues
[]values.Value`), translated once at build time via the existing `ScalarTranslation` /
`NormalizeKeyForPositions` infra (`key_expression.go`, `store_delete_where.go`, `aggregate_function.go`),
threaded from metadata through `NewValueIndexScanMatchCandidateWithFunctions` and the build sites. Two
independent fields resolves the conflation: the common-PK field is populated **regardless of fan-out**
(index entries always carry the PK), while `pkColumnNames`/ordering-suffix stays empty for fan-out.
`computePrimaryKey`'s index arm returns the translated Values; `commonPKFromChildren` (`:268-293`, already
comparing `[]values.Value` via `ValuesStructurallyEqual`) then compares real PK identity and cannot equate
`Field("ID")` with `Concat(RecordTypeKey(), Field("ID"))`. **Null-PK case (Graefe note):** Java returns
`Optional.of(emptyList())` for a null common PK, not empty/absent — so the Go index arm must return an
**empty (non-nil) Values list** for that case, keeping `ValuesStructurallyEqual` consistent (an empty list
is a known-empty PK, distinct from `nil` = "unknown, don't dedup").

**Reachability / flip risk:** MODERATE — re-enables `ImplementDistinctUnionRule` dedup on index-scan legs.
A correct **structural** PK is precisely what makes that dedup safe (the M5 revert's whole rationale). Any
resulting EXPLAIN change is validated as a genuine safe dedup, and the dropped-rows guard below is the
correctness net. 1M stress before/after.

**Test:** `TestPrimaryKey_IndexScanCarriesStructuralPK` — an index scan surfaces PK Values encoding the
record-type-key prefix. **Dropped-rows guard (FDB integration):** two record types sharing PK column name
`ID` but different PK expressions (`Field("ID")` vs `Concat(RecordTypeKey(), Field("ID"))`); a UNION of
index scans over both must **not** dedup — assert full row count preserved AND EXPLAIN shows a
non-deduped union. Red on the by-name (reverted) approach; green on structural.

---

## Workstream C — value-layer / compensation gaps (finding 12)

### C1. Finding 12a — value-layer IN exact-int comparison + non-comparable `==` guard

**Go:** `values/value_in.go:52` `equalsAny` calls `promoteNumeric(a,b)` on **any** numeric pair —
including `int64↔int64` — to `float64`, then `af==bf`, so two distinct int64 > 2^53 compare **equal**
(disagreeing with the exact predicate-layer IN). `:52`/`:55` (guarded only against `[]byte`) and
`value_array_distinct.go:101` use bare `==` which **panics** on non-comparable `[]any`/map elements.

**Java:** `InOpValue.eval` (`:103`) → `Comparisons.evalComparison(EQUALS,…)` →
`toClassWithRealEquals(v).equals(…)` = exact `Long.equals` (never float-coerced); cross-type is promoted
to a common `maximumType` at plan-build (`values.go` ~`:235`), so same-typed longs compare bit-exact.

**Fix:** replace `promoteNumeric` with the predicate-layer discipline the Go IN predicate already uses
(`comparisons.go:430 cmpAny` → `values.CompareExactInts` `:589` for same-type integers, float promotion
only when a float is genuinely present); guard the final `==` (type-switch out slices/maps, or a
reflect-comparable check) so non-comparable elements return `false` instead of panicking. Same for
`value_array_distinct.go:101`.

**Reachability:** real — constant-fold of `IN`/`ARRAY_DISTINCT` over int64 literals > 2^53; the panic is
reachable when a list/array element is a nested `[]any`/map.

**Test:** value pkg unit — `9007199254740993 IN (9007199254740992)` → FALSE; `ARRAY_DISTINCT` over nested
arrays does not panic.

### C2. Finding 12b — CAST(string AS INT/LONG/DOUBLE) trim set (premise corrected)

**Premise correction:** the finding claimed "Java rejects leading/trailing space" — **wrong**, Java also
trims (`CastValue.java:187,195,203,211` all `…trim()`). The actual (much narrower) divergence: Go
`strings.TrimSpace` strips the full Unicode-whitespace set; Java `String.trim()` strips only code points
≤ U+0020. So `CAST(' 5' AS INT)` = 5 in Go but throws in Java.

**Fix:** at `values.go:3365,3394,3519`, replace `strings.TrimSpace` with a Java-`trim()`-equivalent set
(`strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })`) — **not** "drop the trim." Low priority,
exotic-whitespace only, but a real cross-engine CAST divergence.

**Test:** `CAST(' 5' AS INT)` (NBSP-prefixed, U+00A0) must error (Java does not strip NBSP). **Torvalds condition 3:** the probe MUST
use a code point in (U+0020, Unicode-WS] — an ASCII space (U+0020) trims in *both* engines and proves
nothing; keep an ASCII-space control that still trims in both.

### C3. Finding 12c — ForMatchCompensation.Union asserts both result-fns needed

**Go:** `compensation.go:1035-1042` — `Union` keeps `c`'s result function (`rcf`) whenever it reaches the
else branch, even when only the other side's is needed → wrong output shape.

**Java:** `Compensation.java:617-624` — `if (!rcf.isNeeded() && !other.isNeeded()) → none; else {
Verify.verify(rcf.isNeeded()); Verify.verify(other.isNeeded()); pick rcf; }` — Java **asserts both are
needed** and throws if only one is.

**Fix:** add the two `IsNeeded()` assertions (return an `ImpossibleCompensation` / typed error on
violation, matching Java's `Verify.verify` fail-loud). Reachability: latent — the else is reached only
when both compensations are needed (the whole-comp gate at `:990-998`) yet their result-fns disagree,
which needs a genuine 2-child ForMatch union (OR → union over one candidate); effectively single-child
gated today.

**Note (booked, NOT in this workstream):** `ComputeResultCompensation` (`compensation.go:154`) hardcodes
`EmptyGroupByMappings()` where Java's non-null pull-up branch calls `pullUpAggregateCandidateMappings`
(`Compensation.java:485`). That is a larger port (needs `pullUpAggregateCandidateMappings` + downstream
aggregate/GROUP-BY mapping plumbing that is incomplete in Go) and is latent until an aggregate-index match
consumes `groupByMappings`. Booked in TODO.md as its own item; the Union assertion (the cheap, safe half)
lands here.

**Test:** a union of two ForMatch compensations where only one result-fn is needed hits the assertion
(loud, not silent-wrong).

### C4. Finding 12d — PredicateEquals handles ExistentialValuePredicate

**Go:** `predicates/predicates.go:113-187` — the switch covers Constant/And/Or/Not/Value/Comparison but
has no `*ExistentialValuePredicate` case, so two EXISTS fall through to `return false` (`:186`). The type
is handled everywhere else (rebase, correlation, semantic_hash) — only `PredicateEquals` misses it.

**Disposition:** pure **optimization** — memo interning / `SemanticHashCode` already dedups for
correctness; `PredicateEquals` only drives AndDedup/OrDedup, so `EXISTS AND EXISTS` merely misses a
redundant-conjunct collapse (no wrong answers). Fixing removes the inconsistency.

**Fix:** add `case *ExistentialValuePredicate` comparing the wrapped Value (`valueNamesEqual`) + the
Comparison.

**Test:** `WHERE EXISTS(q) AND EXISTS(q)` dedups to one conjunct.

---

## Workstream D — dead code, missed matches, maintainability (finding 13)

**Intersection rules — RECLASSIFIED to KEEP (implementation-discovered, supersedes the RFC's DELETE
proposal; flagged for milestone re-confirmation).** The RFC proposed deleting `rule_implement_intersection.go`,
`rule_intersection_merge.go`, and the `IntersectionSingletonElimRule` in `rule_set_op_singleton.go` as an
SQL-unreachable "closed dead loop" — SQL intersections are built directly as physical plans by
`WithPrimaryKeyIntersector` (`intersector_primary_key.go:17-19`), never via a `LogicalIntersectionExpression`.
That reachability claim is correct, BUT implementation revealed the rules are a **complete, working
logical→physical intersection path that is a live planner-completeness SAFETY NET**: `FuzzPlanner_WithBatchA_NoPanic`
(`planner_batch_a_fuzz_test.go`) builds logical-intersection shapes (opcode 7) and asserts the planner
produces a complete physical plan (the BestMember invariant), and `TestPlanner_IntersectionOverTwoScansProducesPhysicalIntersection`
white-box-tests the same. Deleting the rules breaks those with an invariant-break (no BestMember) — not a
latent bug — i.e. it removes real fuzz/white-box coverage of the logical-intersection planning path for
marginal benefit. They are not dead in the harmful (regression-masking / latent-unsound) sense: they are
exercised and pass. **Disposition: keep the rules; the `IntersectionSingletonElimRule` in
`rule_set_op_singleton.go` also shares its file with the LIVE `UnionSingletonElimRule` (unions ARE seeded),
so the file stays regardless.** The underlying divergence (SQL bypasses the logical-intersection flow) remains
the separately-booked architectural item. Milestone review re-confirms this reclassification with Graefe.

**Deletes (genuinely dead; done):**
- `matched_ordering_part.go:193` `Demote()` — DEAD (only test callers; panics on non-equality). **DELETE**
  with its tests.
- `max_match_map.go:647` `findMatchingReachableCandidate` — DEAD thin `nil`-equivalence wrapper;
  production uses `…WithEquivalence` (`:410,:564`); wrapper is test-only (`:782,:800`). **DELETE** with its
  tests.
- `unified_tasks.go:674 isExploratoryMember` ≡ `:695 isAlreadyExploratoryMember` — byte-identical live
  duplicates. **Consolidate** to one; keep the O(n²)-per-round membership-scan note as a booked perf item
  (swap to a `map[*expr]struct{}` set if groups grow — not a correctness issue).

**Keep (harmless faithful port):** `rule_merge_fetch_into_covering_index.go` — dead/unreachable
(`wrapScanPlanWithCoverage`, `abstract_data_access_rule.go:514`, strips the Fetch at construction) but a
faithful Java port kept as a registered no-op safety net. Do **not** port anything; leave as-is (or delete
if trimming dead rules wholesale — Graefe's call).

**Real missed-match gap — PORT:** `rule_match_intermediate.go:249-275` pairs quantifiers **positionally**
(`candidateQs[i]` vs `queryQs[i]`); Java's `MatchIntermediateRule` → `RelationalExpression.subsumedBy`
enumerates **all bijections** via `EnumeratingIterable<PartialMatchWithQuantifier>` (import `:26`), so Go
silently misses index matches whenever the candidate quantifier order ≠ query order for multi-quantifier
expressions. Port the permutation enumeration (`EnumeratingIterable`/AliasMap bijection + per-permutation
child-match + yield-all). This is the one substantive item in this workstream; test with a two-quantifier
candidate whose quantifier order is swapped relative to the query, asserting the index match is found.

**Deprecate-lite:** `expression_partition.go:69` `GetPlans()` is not index-aligned with `GetExpressions()`
(skips non-physical members; the code comment already documents the RFC-183 §12 mispairing and points
callers to `GetPhysicalExpressions`). Make `GetPlans` panic on misuse or rename/deprecate it so the
footgun can't bite silently. `NewPlanPartition:30` map-iteration nondeterminism is a documented dead path
— leave it.

**Out of scope (characterized only):** the NLJ rule (`rule_implement_nested_loop_join.go`, 3417 LOC vs
Java's 331, heavy reverted-attempt comment narrative violating comment-hygiene) is a large refactor for
its own RFC — booked, not touched here.

---

## Workstream E — plan-flip optimization surfaces (owner directive: in-PR with full per-flip audit)

Both items below open/reorder an optimization surface. Per the CLAUDE.md, a plan flip must never be
waved through — each flip is EXPLAIN-diffed and verified as a genuine improvement (sort elimination,
safe dedup), NEVER a dropped-dedup or dropped-row. These land AFTER workstreams A–D so the correctness
fixes are banked first, **as self-contained independently-revertable commits** (packaging reconciliation
in the header): a failed audit reverts only these commits, never the correctness fixes. **Per-flip audit
protocol (mandatory, both items):** capture full corpus EXPLAIN before/after; for EVERY changed plan
line, (1) confirm row-level equivalence (same rows, cross-engine), (2) classify the flip as a genuine
optimization vs a regression, (3) Java-verify the new shape is what the Java planner produces; 1M stress
before/after; codex + Graefe + Torvalds sign-off on the flip set. **E1 (DISTINCT-elision) additionally
(Torvalds condition 2):** 1M *random* stress + EXPLAIN pins do NOT catch a dropped dedup unless duplicate
keys are present — so every elision-class flip needs a **duplicate-bearing** row-level cross-engine
assertion (a fixture with repeated keys on the eliminated-DISTINCT column, asserting the elided plan
still returns the correct DISTINCT row set), not count-parity on random data. Any flip that isn't a
provable improvement blocks the arm (revert + book), never ships.

### E1. M4-followup — Fetch stored-record transparency (~48 plan flips)

**Go:** `computeStoredRecord` (`plan_properties.go:163-198`) has **no**
`RecordQueryFetchFromPartialRecordPlan` arm → hits `default: return false` (`:195-196`). (The distinct
`:93`, PK `:248`, and cardinality `:381` switches already have the transparent Fetch arm — only
storedRecord lacks it.) Java `StoredRecordProperty.visitFetchFromPartialRecordPlan` (`:306-308`) returns
`true`.

**Consequence of the fix:** adding the arm is 1:1 Java, but it is the switch that turns ON the
non-covering DISTINCT elision — `ImplementDistinctFinalRule` stops filtering out the `Fetch(IndexScan)`
partition, so the M4 non-unique-scalar-index DISTINCT elision fires on the common non-covering path:
~48 corpus lines flip, `InMemorySort([ID], Fetch(InJoin(IndexScan)))` → `InUnion(IndexScan)` on IN-list
queries (apparent sort-elimination improvements). **Fix:** add the transparent arm; then run the per-flip
audit above on all ~48 flips. Ships only if every flip is a proven safe sort-elimination.

**Test:** `TestStoredRecord_FetchIsTransparent` (unit, the arm) + the IN-list EXPLAIN pins for the flipped
shapes + a **duplicate-bearing** row-level FDB correctness test (Torvalds condition 2): a fixture with
repeated values on the DISTINCT column, asserting the elided `InUnion(IndexScan)` plan returns the exact
same DISTINCT row set as the pre-flip `InMemorySort(Fetch(InJoin(IndexScan)))` — count-parity on random
data would silently pass a dropped dedup.

### E2. Finding 6-followup — dense predicate-count producer (~11 plan flips)

**Go:** `designated_final.go:259-284` produces `predCountByLevel` **sparse** (`:280-282` adds only for
`RelationalExpressionWithPredicates`), so the highest-level tiebreak
(`comparePredicateCountByLevel`, `planning_cost_model.go:151`, `intCompare(maxLevelA, maxLevelB)`) uses
the highest **predicate** level, not Java's tree-depth `getHighestLevel`. Java
(`PredicateCountByLevelProperty.java:208-218`) **always** `put(currentLevel, count)` (0 included) →
**dense**; `getHighestLevel` (`:156-158`) = `lastKey()` = tree depth; `compare` (`:182-193`) tiebreaks on
it.

**Consequence of the fix:** making the producer dense (`counts[currentLevel] += predCount`
unconditionally, 0 included) matches Java exactly, but flips ~11 REWRITING survivors (nested redundant
Project, Limit/Project reorder). Pre-existing (master's producer is sparse too). **Fix:** dense the
producer + drop the `maxLevelA/maxLevelB` union-scan special-casing in the comparator (the ascending
first-map iteration RFC-188 restored stays); then run the per-flip audit on all ~11 flips, Java-verifying
each new survivor against the Java planner.

**Test:** `TestPredicateCountByLevel_DenseProducer` (a node with a non-predicate level records a 0 entry;
the highest-level tiebreak now reflects tree depth) + the ~11 EXPLAIN pins.

---

## Workstream F — rule / config parity (finding 11, RemoveRangeOne, IndexScanPreference)

### F1. Finding 11 — PredicateToLogicalUnionRule: only expand index-exploitable ORs

**Go:** `rule_predicate_to_logical_union.go:99-135` collects **every** top-level `OrPredicate` and expands
to `Distinct(Union(...))` unconditionally; registered in `DefaultExpressionRules()`
(`default_rules.go:127`) = **REWRITING** phase.

**Java:** `PredicateToLogicalUnionRule` is a **PLANNING-phase match-partition rule**
(`extends CascadesRule<MatchPartition>`, `ofExpressionAndMatches` root `:137-138`) — it fires **after**
index matching and binds existing `PartialMatch`es. Gate (`:197-211`): build `partiallyMatchedOrs` from
each match's predicate-map entries with `MappingKind.OR_TERM_IMPLIES_CANDIDATE`; if any to-be-expanded
`OrPredicate` is **not** in `partiallyMatchedOrs`, `return` (no union). So Java expands only ORs a partial
match already mapped to an index candidate; `a=1 OR b=2` with no index → no partial match → no expansion.
**Gate precision (Graefe):** the gate keys on *that specific OrPredicate* appearing in `partiallyMatchedOrs`
(an `OR_TERM_IMPLIES_CANDIDATE` mapping for that exact predicate) — NOT merely on the expression having
*any* partial match. A rule that expands whenever some unrelated predicate matched an index would
re-introduce the bloat.

**Fix (infra — Graefe-sensitive):** this is not a gate-add — Go's rule is a REWRITING expression rule with
no access to match partitions or a predicate-mapping-kind taxonomy. Replicating Java needs (a) Go's
data-access matching to carry an `OR_TERM_IMPLIES_CANDIDATE`-equivalent mapping kind on partial matches,
and (b) the rule relocated to PLANNING exploration as a match-partition rule. **Step 1 — probe:** confirm
`a=1 OR b=2` with no index actually produces `Distinct(Union(Scan,Scan))` in Go today and measure the
plan/memo cost, to size the fix and confirm it's worth the phase relocation. **Step 2 — port:** add the
mapping-kind signal + relocate + gate. Graefe reviews the phase move explicitly.

**Test:** yamsql, no-index table — `SELECT * FROM t WHERE a=1 OR b=2` plans a scan with a residual OR
filter (`plan_contains` NOT `Distinct`/`Union`); positive control — indexes on `a`,`b` → the union.

### F2. Finding 2-followup-a — port Java's real RemoveRangeOneRule

**Java:** `RemoveRangeOneRule.java:45-102` (`ExplorationCascadesRule<SelectExpression>`) drops a ForEach
quantifier over an exploratory `TableFunctionExpression` whose value is exactly `RANGE(0,1)` when (1) the
Select has **> 1** quantifier, (2) the value is exactly RANGE(0,1), (3) the alias is **unreferenced** by
the result value, predicates, or sibling quantifiers — a single-row cross-join identity that is dead
weight once other real quantifiers exist.

**Go:** already mints the shape (`rule_decorrelate_values.go:156-165`
`NewTableFunctionExpression(NewRangeValue(0,1,1))`; `isRangeOneQuantifier` `:535+` detects it) — but only
when *all* quantifiers are values-boxes, so the resulting Select has exactly **one** quantifier, which
Java's `>1` guard excludes. So the exact trigger is **not SQL-reachable today** — latent.

**Fix (~60-line `ExplorationCascadesRule`, all primitives exist):** port it into PLANNING exploration for
parity (this also reclaims the `RemoveRangeOne` name cleanly after RFC-188 deleted the Go-invented
LIMIT-removal impostor).

**Test:** memo-level unit — a 2-quantifier Select with one unreferenced RANGE(0,1) quantifier; assert the
quantifier is dropped and the other survives. (Honest: no SQL shape triggers it today, so the pin is
memo-level.)

### F3. Finding 3-followup — model IndexScanPreference

**Go:** `planning_cost_model.go:977` hardcodes the `PREFER_SCAN` default (`return -1`); `PlanContext` has
no `IndexScanPreference` knob.

**Java:** proto `PREFER_SCAN=0`/`PREFER_INDEX=1`/`PREFER_PRIMARY_KEY_INDEX=2`; the Cascades default is
`PREFER_SCAN`. The multi-type flip (`recordTypes>1 && !primaryKeyHasRecordTypePrefix ? PREFER_INDEX :
PREFER_SCAN`, `RecordQueryPlanner.java:193-195`) is the **legacy** constructor's default only.

**Fix (Torvalds condition 4 — wire it, don't add a dead knob):** add an `IndexScanPreference` field to
the **existing `PlannerConfiguration` mirror** (`plan_context.go:41`, which already models the consulted
subset of Java's `RecordQueryPlannerConfiguration` — `allowDuplicateProjections`, `shouldJoinRightDeep`,
`deferCrossProducts` — all modeled, defaulted, and consulted by rules), defaulting `PREFER_SCAN` in
`DefaultPlannerConfiguration()`. Consult it in `primaryVsIndexVerdict` (the `PREFER_INDEX`/
`PREFER_PRIMARY_KEY_INDEX` sign + an "index-scan-is-on-PK" check for `PREFER_PRIMARY_KEY_INDEX`). This is
wired into the real config surface exactly like its sibling fields — not a bespoke `PlanContext` knob.
**Honest scope note:** the non-default branch has **zero behavioral effect on any reachable SQL today**
(no surface sets a non-default `PlannerConfiguration`, same as its siblings; and the only divergent case
— multi-type store whose PK lacks a record-type prefix — cannot fire because SQL-surface PKs always
carry a RecordTypeKey prefix). The value is *parity of the mechanism*, consistent with how Go already
models the rest of `RecordQueryPlannerConfiguration`. If, on inspection, the `PlannerConfiguration` mirror
turns out not to be consultable from `primaryVsIndexVerdict`'s call path, **drop** the field and book it
(no dead config).

**Test:** `TestPrimaryVsIndexVerdict_HonorsIndexScanPreference` — a `PlannerConfiguration{IndexScanPreference:
PREFER_INDEX}` (a legitimate config value, the same shape Java's config object takes) flips the verdict
toward the index for a shape where the `PREFER_SCAN` default prefers the scan; same pattern as any
sibling-config test.

---

## What this is / is NOT

- **Correctness fixes** (A1 hang, A2 memo dedup, A3 wrong projection, C1 int precision/panic) vs
  **plan-choice fixes** (B, E, F1) vs **cleanups** (C2/C3/C4, D). None change key/record/index/
  continuation **format**; plan choice is wire-visible only for cross-engine continuation resumption.
- Every fix is a **Java-faithful port** or the **deletion of a Go-only divergence** (D intersection rules,
  A3 nil-fallback). No new Cascades rule/phase/physical operator except F1's phase *relocation* (which
  matches Java's phase) and F2's port of a rule Java *has*. Graefe: match-then-implement untouched.
- The Go-only extension rungs RFC-188 cleared (statistics scalar-cost, `compareJoinOrdering`,
  redundant-sort) are NOT touched.

## Risks & mitigations

- **R1 — plan churn (E1/E2/B3/F1).** The plan-flip arms (E1 ~48, E2 ~11) get the mandatory per-flip audit
  (row-level + EXPLAIN + Java-verify + 1M stress + codex/Graefe/Torvalds sign-off); B3 and F1 get 1M
  stress + EXPLAIN review. Any flip that isn't a provable improvement blocks its arm (revert + book).
- **R2 — B3 dropped-rows.** The structural-PK re-port is the mechanism that makes the DistinctUnion dedup
  safe; the dropped-rows FDB guard (two same-column-name/different-PK-expression legs must not dedup) is
  the correctness net, red on by-name and green on structural.
- **R3 — A3 fail-closed change.** Deleting the dead nil-fallback makes translate fail-closed (Java parity);
  the covering-index unit + swap variant prove the projection, and a translate that can't pull up a part
  now fails loudly instead of emitting `QOV`.
- **R4 — A4/F1 Cascades contract changes.** A4 (quantifier correlation) and F1 (phase relocation) are
  Cascades-semantic changes → explicit Graefe review; both are already-compensated / Java-matching.

## Implementation order (DFS, one finding to completion, green per commit, e2e each)

1. **A1** finding 8 (hang) — `AllMembers()` + the through-final cycle regression.
2. **A2** finding 5 (memo dedup) — bitwise scalar float + hash-consistency pin.
3. **A3** finding 9 (projection) — root pull-up + delete dead nil-fallback + covering-index unit.
4. **A4** finding 7 (correlation) — transitive delegate + correlated-quantifier pin.
5. **B1** M3 (cardinality) — bound the point-scan; **B2** M4-fu-3 (fail-closed narrow); **B3** M5
   (structural PK) — plumb + dropped-rows guard + stress.
6. **C1** 12a (int precision/panic); **C2** 12b (trim set); **C3** 12c (Union assert); **C4** 12d (EXISTS
   dedup).
7. **D** dead-code deletes (intersection rules, Demote, findMatchingReachableCandidate, exploratory dup) +
   GetPlans deprecate-lite + **MatchIntermediate permutation port**.
8. **F2** RemoveRangeOne port; **F3** IndexScanPreference (tested via direct PlanContext).
9. **F1** finding 11 — probe, then phase-relocate + gate (Graefe reviews the move).
10. **E2** finding-6 dense producer (~11 flips + per-flip audit); **E1** M4 Fetch-storedRecord (~48 flips +
    per-flip audit) — last, riskiest, correctness fixes already banked.
11. Full 56-target suite + determinism ×10 on affected planner tests + 1M stress before/after; milestone
    review lap (Graefe + Torvalds + codex + @claude), fold, delta re-confirm.

No finding "done" until a test (unit + FDB where row/plan-visible) pins the corrected behavior. No
`t.Skip`, no "for now". Tidy: mark finding-10-M4-followup-2 (fail-closed unknown index root) DONE in
TODO.md — it landed in RFC-188 round 7.
