# RFC-240: One normal form, not two

**Status:** DESIGN v2. v1 was NAK'd by both gates on the same finding (§6);
findings folded in §11.
**Base:** branch `worktree-bughunt`, forked at `19a043a0b`
**Relates to:** the measurement committed at `9241f2d2d`
(`TestNormalForm_NotOverConnective_TwoImplementationsDisagree`) and the
`TODO.md` entry of the same name, which this RFC closes.
**Wire impact:** none on the READ path. One WRITE path is touched and frozen —
§7.

## 1. Decision

Replace Go's two normal-form implementations with one, parameterized over
Java's `Mode` (major/minor), so `NormalizePredicatesRule` normalizes through a
`NOT` over a connective exactly as `BooleanPredicateNormalizer` does — and feed
the COST MODEL the same negate-aware metric Java feeds it, rather than the
negate-blind one it reads today.

Four things land, in this order, each its own commit with its own single-cause
plan diff (§10):

| | change | why separable |
|---|---|---|
| A | absorption tie-break → Java's | moves only goldens with a duplicate clause |
| B | cost sites → negate-aware metric | moves only goldens with a NOT under a cost comparison |
| C | strict normal form + mode parameterization; delete the lax pair | moves only goldens with a NOT over a connective |
| D | delete `NormalizePredicatesRule.normalized`; termination becomes algebraic | C is what makes it sound |

Commit D is EXPECTED to move no golden. That is a PREDICTION §8.6 tests, not a
property — §6 was corrected from asserted to measured and the same standard
applies here.

## 2. What Java does

`BooleanPredicateNormalizer` has ONE of each thing and a `Mode` that swaps the
roles of AND and OR (`:72-112`):

| concept | DNF | CNF |
|---|---|---|
| major (outer) | `Or` | `And` |
| minor (inner) | `And` | `Or` |

- `isInNormalForm` (`:255-293`) — a `NotPredicate` over anything that is not a
  variable falls to `:287-289` and is **not** in normal form.
- `toNormalized(predicate, negate)` (`:370-384`) — carries a negate flag; under
  negation the major and minor arms SWAP. De Morgan as a role swap, not a
  rewrite.
- `getMetrics(predicate, negate)` (`:319-334`) — the size, with the same swap.
- `normalize` vs `normalizeAndSimplify` (`:209-229`) differ ONLY in whether
  `applyAbsorptionLaw` runs.

`NormalizePredicatesRule.onMatch` calls `normalizeAndSimplify` in CNF mode.

## 3. What Go does, and the defect

| file | normal-form test | conversion | size | `NOT` over a connective |
|---|---|---|---|---|
| `normalize_dnf_exact.go` | `isInDNFStrict` | `toDNFNegated` | `dnfSizeNegated` | pushed — matches Java |
| `rule_normalize_predicates.go` | `isInCNF` / `isInDNF` | `toCNFNormalized` / `toDNFNormalized` | `cnfSize` / `dnfSize` | treated as a LEAF |

`isLeafPredicate` (`rule_normalize_predicates.go:168-175`) returns true for a
`NotPredicate`. `AND(NOT(AND(a, b)), c)` therefore reads as already-CNF,
`normalizeCNF` returns `(pred, false)`, and the rule declines. Java produces
`(NOT a OR NOT b) AND c`.

**Reachable, not latent.** `expr.ResolveNot`
(`pkg/relational/core/query/expr/expr.go:1874-1879`) builds a plain
`NotPredicate` with no De Morgan, and `WHERE NOT (cat = 'A' OR cat = 'B')` is in
the corpus (`yamsql/testdata/complex_where_java.yaml:62`).

**Consequence: plan quality, not wrong rows.** The CNF shape is the precursor
`PredicateToLogicalUnionRule` consumes to split a disjunction across index
accesses.

## 4. The port

Java's `Mode` is an ENUM with a switch, and so is this — not a struct of
closures. Closures would allocate on a path the cost model calls per plan
comparison, and they would be a Go-only shape where Java has an enum.

```go
type normalFormMode int
const (
    normalFormDNF normalFormMode = iota // major Or,  minor And
    normalFormCNF                       // major And, minor Or
)
```

`isInNormalForm(p, mode)`, `normalFormSize(p, negate, mode)`,
`toNormalized(p, negate, mode)` — each a switch on the mode where Java calls
`mode.instanceOfMajorClass` / `mode.majorWithChildren`. The DNF bodies already
in `normalize_dnf_exact.go` are the correct algorithm; the work is threading the
mode through them.

### 4.1 The variable test, stated exactly

Java's `isNormalFormVariable` (`:490-492`) is
`isAtomic() || instanceof LeafQueryPredicate`. Go's equivalent is
`normalize_dnf_exact.go`'s `dnfVariable` (`:48-57`) — "not an
`AndPredicate`/`OrPredicate`/`NotPredicate`". It is **not**
`rule_normalize_predicates.go`'s `isLeafPredicate` (`:168`), which omits
`NotPredicate` from its exclusion list and is the thing being deleted. Naming
which one survives matters: picking the wrong one silently changes the write
path §7 freezes.

Two things about that equivalence are asserted here and PINNED by a test rather
than argued:

- **It holds today.** Among non-test `QueryPredicate` implementations in
  `pkg/.../cascades/predicates`, only And/Or/Not return a non-empty
  `Children()`. A test enumerates the concrete types and fails when a new
  connective-shaped predicate appears without an arm.
- **Go fails OPEN where Java fails LOUD.** Java's `isInNormalForm` throws
  `"unknown boolean expression"` (`:292`) for a predicate that is neither
  variable, connective, nor NOT. Go's `dnfVariable` default arm answers
  "variable". Today the sets coincide so nothing differs; the divergence is in
  the FAILURE direction, and it is recorded at the site.

### 4.2 Atomicity is a real Go gap, not a hypothetical

Java's `withAtomicity(true)` is set and read by
`PredicateToLogicalUnionRule.java:220,232,303` — the exact downstream consumer
of this rule's output. Go has no atomicity concept at all, so:

- `isAtomic()` never suppresses descent, and
- Java's two metrics — `normalFormSize` (guard) and `normalFormFullSize` (cost
  model) — which differ ONLY at atomic nodes (`:323,327,330`), coincide in Go.

Go therefore needs ONE size function where Java has two fields. That is stated
at the definition so the day atomicity lands, the site that must split is
marked.

## 5. Absorption tie-break (commit A)

Java removes `ci` when
`ci.size() > cj.size() || (ci.size() == cj.size() && i < j)` (`:461`). Go uses
`i > j` (`rule_normalize_predicates.go:273`).

Equal size plus `containsAll` means the clauses are the SAME SET, so the
tie-break decides only WHICH duplicate survives — never the content. But that
is the emitted child order: on `[A, X, A]` Java yields `[X, A]`, Go `[A, X]`.

It lands FIRST and ALONE because its golden population (goldens containing a
duplicate clause) is disjoint from C's (goldens containing a NOT over a
connective). Bundled, no golden move could be attributed to a cause.

Java also mutates its list in place while iterating (`absorbed.remove(i)`,
`size--`, `continue nexti`); Go's two-pass form compares against the full
deduped list. That cannot change the RESULT — absorption computes the minimal
antichain under inclusion, which is unique — and the two-pass form is readable.
Stated, not silently diverged from.

## 6. The cost model reads the normalizer's metric (commit B)

**v1 had this backwards.** It proposed freezing today's `cnfSize` under the name
`residualPredicateComplexity` and claiming "no cost-model delta by
construction". Both gates rejected it, and they were right:

```
NormalizedResidualPredicateProperty.java:81-90
    countNormalizedConjuncts(expression)
      -> BooleanPredicateNormalizer.getDefaultInstanceForCnf()
             .getMetrics(magicPredicate).getNormalFormFullSize()

consumed at PlanningCostModel.java:142-143 and RewritingCostModel.java:92-93
```

Go's own comment at `designated_final.go:232-238` says it ports that property.
So both Go call sites — `designated_final.go:245` and
`planning_cost_model.go:768` — ARE Java's normalizer metric, mis-ported to the
negate-blind `cnfSize`. `cnfSize(NOT(a OR b))` is 1
(`rule_normalize_predicates.go:195-196`, the NOT arm recurses without swapping
roles); Java's `getMetrics` is 2.

Both sites move to `normalFormSize(p, false, normalFormCNF)`. `cnfSize`,
`dnfSize` and `dnfSizeNegated` are deleted; there is no renamed survivor.

**The plan delta is measured, not asserted.** A cost input changing is the
POINT of commit B, and its golden diff is read on that basis: every golden that
moves must contain a NOT beneath a cost comparison.

## 7. The write path must not move

`NormalizeDNFWithoutSimplification` is called by
`pkg/relational/core/query/ddl/generator_predicate.go:55` and its output becomes
stored `RecordMetaDataProto.Predicate` bytes. Java reaches it via
`MaterializedViewIndexGenerator.java:675`, which calls `normalize`, not
`normalizeAndSimplify` — which is why the no-absorption variant exists.

It is ALREADY the exact port. Re-expressing its bodies through the mode
parameter must return the same thing for every input. Commit A cannot reach it
(absorption does not run there); commits B and D do not touch it; commit C
re-expresses it.

## 8. How this is verified

**8.1 Java's own test suite is the oracle.**
`BooleanPredicateNormalizerTest.java:69-165` asserts exact expected outputs for
both modes: `atomic` (`:69-74` — pins that a bare `not(P1)` stays put, the
boundary of the change), `flattenDnf`/`flattenCnf`,
`distributeDnf`/`distributeCnf`, `deMorgan` (`:131-140` — the four cases this
RFC exists to make pass), `complexDnf`, `complexCnf`, `complexRoundTrip`. These
are ported directly. This is the primary verification and it checks the port
against Java's ASSERTED behaviour rather than against itself.

`assertExpectedNormalization` (`:262-267`) also asserts **stability** —
normalizing a normal form returns it unchanged. Ported, and it is what §9 rests
on.

**8.2 The write path is byte-stable.** A golden corpus of
`NormalizeDNFWithoutSimplification` outputs is captured from the tree at
`19a043a0b` — BEFORE commit C — and committed. v1's "differential against
today's implementation" was unrunnable, because today's implementation is
deleted in the same change; a golden captured beforehand is the runnable form.
The corpus is bounded by the size estimate so the file stays readable (the
unbounded first attempt produced 2.1 MB, which is a golden nobody reviews).

**8.3 The negate-aware metric is asserted positively**, with named examples —
`normalFormSize(NOT(a OR b), false, CNF) == 2`, not 1 — rather than by a
differential against the function being replaced.

**8.4 The behaviour change is pinned positively.**
`TestNormalForm_NotOverConnective_TwoImplementationsDisagree` is REPLACED by its
positive form: `normalizeCNF(AND(NOT(AND(a,b)), c))` must produce
`(NOT a OR NOT b) AND c`.

**8.5 Semantics.** `FuzzNormalForm_PreservesSemantics` (`3ebbfd2a6`) drives all
three entry points per-row with UNKNOWN as its own outcome. It now reaches the
NOT-over-connective inputs that previously declined.

**8.6 Plan-shape diff, with a NON-EMPTY floor.** Full suite, then the
explaindiff corpus, per commit. Commit C MUST move at least one golden — zero
moved goldens means the change did not take, and would otherwise read as
success. Each moved golden is attributed to that commit's single cause.

**8.7 Determinism.** The planner tests touching normalization run 10x with
`--nocache_test_results` — without it Bazel serves a cached result and the loop
measures nothing. "Affected" is defined as the targets whose goldens moved in
8.6, plus `//pkg/recordlayer/query/plan/cascades:cascades_test`.

## 9. `NormalizePredicatesRule.normalized` is deleted (commit D)

The rule carries `normalized map[*expressions.SelectExpression]struct{}`
(`:35,54,87-88`) — a Go-only identity-keyed set, never cleared, marking
expressions it has already fired on. Java has no counterpart; its termination
comes from `isInNormalForm` returning true for the rule's own output, so a
re-fire yields `Optional.empty()`.

Commit C is what makes that true in Go: `toNormalized` emits
major-of-minor-of-variables-or-NOTs, which is exactly what the STRICT
`isInNormalForm` accepts. Under the lax test that was also true, so the map was
never load-bearing for termination — it is an optimization that happens to be
unbounded per-planner state, and this RFC widens what fires and therefore what
it accumulates.

Declining is only half of why deleting it is safe; the other half is that a
re-fire must COST nothing rather than accumulate a duplicate member.
`Reference.Insert` dedups on `EqualsWithoutChildren` plus `sameChildReferences`
(`expressions/reference.go:654`) with a `SemanticEquals` fallback below it for
the fresh-Reference case. That is the Go analogue of Java's memo dedup, and it
is what the map was standing in for.

It is deleted, and the property it stood in for is asserted directly: the rule
declines on its own output (Java's stability assertion, §8.1).

`default_rules.go:457` claims "rules are fresh per-call (rules are stateless)".
That is false for this rule today and true after commit D. The comment is
corrected either way.

## 10. Dead code, enumerated

Characterising this as "the lax pair" is what let v1 leave the tree red.
Deleted by commit C:

`rule_normalize_predicates.go`: `isInCNF`, `isLeafPredicate`, `toCNFNormalized`,
`orToCNF`, `isInDNF`, `dnfSize`, `toDNFNormalized`, `andToDNF`, `cnfSize`.

`normalize_dnf_exact.go`: `isInDNFStrict`, `dnfSizeNegated`, `dnfSumNegated`,
`dnfProductNegated`, `toDNFNegated`, `dnfMajorNormalized`, `dnfMinorNormalized`
— each subsumed by its mode-parameterized replacement.

TWO SURVIVE, and they survive for different reasons:

- `dnfVariable` — renamed, as §4.1's variable test.
- `dnfVariableOrNot` — renamed and kept **mode-independent**. Java's
  `isNormalFormVariableOrNotPredicate` (`:494-504`) is `static`: it asks
  "variable, or NOT over a variable", and neither half consults the mode.
  Threading a mode through it would be a Go-only parameter on a Java-static
  function, which is the shape this RFC exists to remove.

Test consumers that must be repointed in the same commit, because a
half-converted tree does not compile and that is how v1 went red:

| site | symbol |
|---|---|
| `rule_normalize_predicates_test.go:133` | `isInCNF` |
| `normalize_size_overflow_test.go:51` | `dnfSizeNegated` |
| `normalize_size_overflow_test.go:56` | `dnfSize` |
| `normalize_size_overflow_test.go:67` | `cnfSize` |
| `normal_form_semantics_fuzz_test.go:67` | `cnfSize` |
| `normal_form_semantics_fuzz_test.go:74` | `dnfSize` |
| `normal_form_semantics_fuzz_test.go:81` | `dnfSizeNegated` |
| `normal_form_not_handling_test.go` | replaced wholesale per §8.4 |

The overflow pin (`normalize_size_overflow_test.go`) is repointed at the LIVE
`normalFormSize`, not at any frozen proxy — it exists to prove the saturating
guard rejects a 4^32 cross product, and pointing it at a dead function would
retire that proof silently.

## 11. Stale claims this change must correct

Grepped, not recalled. Each names a symbol or behaviour this change removes:

| site | stale text | commit |
|---|---|---|
| `rule_predicate_to_logical_union.go:279` | "the dual of `orToCNF`" — a deleted symbol, in PRODUCTION source | C |
| `planning_cost_model_test.go:213` | `"OR(A,B) [cnfSize=1] ..."` in a failure string | B |
| `planning_cost_model_test.go:232` | `"AND(A,B,C) [cnfSize=3] ..."` in a failure string | B |
| `normal_form_semantics_fuzz_test.go:33,44` | the 19683 measurement, attributed to `cnfSize` | B |
| `default_rules.go:457` | "rules are stateless" — false for this rule (§9) | D |
| `chained_unnest_predicate_pushdown_fdb_test.go:213-214` | "normalizeCNF treats a NotPredicate as an OPAQUE leaf" | C |

The 19683 figure is not just a renamed attribution: it is a measurement under a
metric commit B CHANGES, and the negate-aware metric returns a different number
for a NOT-bearing input. It is RE-DERIVED under `normalFormSize` and the bound
it justifies is recomputed, or the comment states a ratio nobody can reproduce.

### 11.1 The chained-unnest comment

`pkg/relational/sqldriver/chained_unnest_predicate_pushdown_fdb_test.go:213-214`
states: "NormalizePredicatesRule does NOT De Morgan it (DeMorganRule is absent
from the default rules; normalizeCNF treats a NotPredicate as an OPAQUE leaf)".
Commit C makes that false. The row expectations there are unchanged —
normalization is semantics-preserving, and `NOT(A AND B)` becomes a single
CNF clause `(NOT A OR NOT B)`, so no pure-outer conjunct becomes extractable —
but the comment is load-bearing reasoning for why that row set is right, and a
false explanation of a correct expectation is worse than no explanation.

## 12. What this RFC does not do

It does not touch the size LIMIT. The measurement in
`normal_form_semantics_fuzz_test.go` stands: a 43-node predicate with an
estimate of 19683 — 1.9% of the 1,000,000 limit — allocates 349 MiB, because
the estimate counts CLAUSES and the cost is in ATOMS. Java is identical on both
counts and computes `normalFormMaximumNumMinors`, the clause WIDTH, without
gating on it. Changing the limit is a deliberate divergence from Java with its
own blast radius.

Commit C makes MORE predicates reach distribution, so it moves traffic toward
that cost. §8.6's lap is where a pathological case would surface.
