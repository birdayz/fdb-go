# RFC-226 — A projection states the row it produces

**Status:** **DRAFT rev 5 — needs a fresh Graefe + Torvalds lap.** Rev 4 was ACCEPTED and then
NAK'd on a second lap; the NAK supersedes and its three blocking findings are folded below.
Rev 5 is a bigger change than a condition-fold: **the acceptance test refuted rev 4's own §1
root-cause story**, and the same differential then found that rev 4's implemented core
*introduces a planning regression* on a shape no existing test covered (§1c). §1 is rewritten
to what was measured, with the refutation left visible rather than quietly corrected, and the
scope grows by the two consumer fixes the type change turns out to require plus the CTE
scope-registration fix the differential isolated.

Rev history: rev 1 NAK'd by both. Rev 2 ACK'd with conditions; folding them refuted rev 2's own
§4.4 (the executor wraps). Rev 3/4 ACK'd then NAK'd. Rev 5 (this) — measured refutation of §1,
regression found and fixed, four fixes, each mutation-verified separately.

**Correction to a correction, recorded rather than silently applied:** the review flagged my
`select_parser.go` / `logical_builder.go` cites as wrong on both path and line. The path was
wrong (they are under `pkg/relational/core/embedded/`, not `core/query/`) and
`logical_builder.go:713` was off by one (`:714` is the gate). But `select_parser.go:685` and
`:788` were correct as written, and the suggested `:787` is off by one — `:788` is
`// SELECT * — projCols stays nil`. Verified with `sed -n '785,790p'` and `sed -n '683,687p'`.
**Renumbered from 225** — a concurrent branch minted `rfcs/225-nested-descent-reads-by-ordinal.md`
at the same time. Master is at 224; 226 verified free via
`git ls-tree -r --name-only origin/master rfcs/`.
**Item:** road-to-prod B5 (entry point); corrects RFC-213 rev 2 §2 tier assignment
**Scope:** `expressions/logical_projection.go`, `plans/projection.go`, `values/values.go`, and
the consumer sites those turn on — which rev 5 measured to be
`cascades/rule_implement_nested_loop_join.go` (the leg-ordinal-safety walk and the orientation
gate) — plus `relational/core/embedded/logical_predicate.go`'s subquery outer-scope builder.
Measured at `aba271454`, branch `rfc/projection-result-type`.

---

## 0. Lead with what contradicts the brief

Three corrections, all measured, before any design.

### 0a. B5's headline number is wrong by ~8x, and it counts the wrong population

road-to-prod.md:164 books B5 as *"~347 UnknownType mints repo-wide"*. The repo has an
**authoritative, ratcheted, AST-based definition of a mint** —
`pkg/docscheck/unknown_type_mint_census_test.go` — pinned per file and red in both
directions. Its total:

```
$ grep -nE '^\s*"pkg/[^"]+":\s*[0-9]+,' pkg/docscheck/unknown_type_mint_census_test.go \
    | sed -E 's/.*:\s*([0-9]+),.*/\1/' | awk '{s+=$1;n++} END {print n, s}'
20 43
```

**43 production mints across 20 files**, not ~347. The census is live and non-vacuous —
run uncached, it has an anti-vacuity floor (`scanned < 100` fails):

```
$ bazelisk test //pkg/docscheck:docscheck_test --nocache_test_results \
    --test_arg="--test.run=TestUnknownTypeMint" --test_arg="--test.v"
=== RUN   TestUnknownTypeMintCensus
=== RUN   TestUnknownTypeMintDetectorPrecisionAndRecall
--- PASS: TestUnknownTypeMintDetectorPrecisionAndRecall (0.00s)
--- PASS: TestUnknownTypeMintCensus (0.16s)
Executed 1 out of 1 test: 1 test passes.
```

Where 347 came from — a raw grep of *mentions*, at the SHA road-to-prod says it measured at:

```
$ git grep -n "UnknownType" a1d281a63 -- 'pkg/**/*.go' | grep -v "_test.go:" | wc -l
352
$ git grep -n "UnknownType" aba271454 -- 'pkg/**/*.go' | grep -v "_test.go:" | wc -l
417
```

So B5's metric is a line count over mints, declines, comparisons and reads together. It has
**gone up** (352 → 417) while the real mint population went **down** (45 at `1e64d6e75` when
the census landed → 43 now). Anyone tracking B5 by its stated number reads progress as
regression.

**And the two sites this work names are not mints at all.** The census explicitly excludes
`return values.UnknownType`, and says why:

> *"a classified DECLINE ("this shape is underivable, fall back") states an answer about the
> shape, not a discarded type on a constructed value … a decline is often the FIX for a mint,
> so counting it would penalize the cure."*

`RecordQueryProjectionPlan.GetResultType` is a decline. **This RFC will not move B5's stated
number, and must not be judged by it. To be unambiguous about which of the two readings that
is: B5's line was measuring the wrong thing — it is not that this work is unimportant.**

Rev 4 continued "the two live wrong-behaviour defects of §1 are B5's actual impact showing up".
**§1 now refutes that** — those defects have their own causes and the type work does not fix
them by itself. The honest statement of B5's impact is the one §1b establishes: an unstated row
is what stops a *consumer* from being written, and the seed reconstruction that fixes the
derived-table defect is exactly such a consumer. **road-to-prod.md:164 is corrected in this change**, pointing at
the census instrument rather than restating a literal, with the refutation recorded above the
table per that page's own policy.

The reason it drifted is worth stating, because it generalizes: `grep -rn 347 pkg/docscheck/`
returns zero hits — **nothing pinned the number**. An unpinned count in an authority document is
a rumour with a citation. That is why the corrected line names the census instead of a figure.

### 0b. B5's "three named guessers" is five in-scope items

`shifts/handoff-ws-n-phase-d-typed-metadata.md:65-81` — the more specific document — heads them
*"the N-F4 name-keyed guessers"* and its own Deliverable 2 (`:98`) says **four** plus
`colref.go`. Verified in the tree today: `descriptorForColumn`
(`cascades_generator.go:3921`), `legPlanFor` (`:4087`), `qualifyAndMergeColumns` (`:5269`),
the surviving last-wins `innerByName` map (`:4212`, built `:4219-4221`, read `:4370`/`:4376`),
and `colref.go`'s `parseColRef` (`:19`). **Five.**

### 0c. RFC-213 rev 2 would entrench this exact bug

`rfcs/213-result-type-derives-from-result-value.md` is **DRAFT rev 2, awaiting Graefe +
Torvalds**. Its §2 table assigns:

> | **2** | `Limit`, `Projection`, `TempTableInsert` | `GetInner()` — pass-throughs that change no row shape |

**A projection is precisely the node that changes the row shape.** `Limit` and
`TempTableInsert` belong in tier 2; `Projection` does not. If RFC-213 ships as written,
`RecordQueryProjectionPlan.GetResultType()` returns its inner's row — which is exactly the
falsehood `logical_projection.go:94-124` already documents. (Rev 4 added "and exactly the shape
that produces the two live defects in §1"; §1 refutes that and the sentence is withdrawn — the
tier correction rests on Java's contract and on the node's role, not on a defect attribution.)
**RFC-226 moves `Projection` from tier 2 to tier 1
(derivable from `GetResultValue()`), and RFC-213 rev 3 should adopt that correction.** The two
RFCs are otherwise complementary and this one does not block on it.

---

## 1. The defects — and the refutation of this RFC's own first story

**Lead with the refutation, because it is the most important measurement in this document.**
Rev 4 said the two stranded queries fail *because* a projection states no row type: the leg is
underivable, every read through it falls back to a qualified name, and the executor gets a
source-relative ordinal against a multi-leg row. **That story is wrong, and the acceptance test
built to confirm it refuted it.** With the type work implemented and the binary verified fresh,
both queries failed IDENTICALLY to master:

```
cte_exists:     *api.Error: 42703: no FROM source aliased as C
derived_exists: *values.UnboundEvalContextError: correlated FieldValue "ID" (correlation "D")
                … (*RowEvalContext (multi-leg row cannot serve a source-relative ordinal))
```

What survived is narrower and worth keeping: `UnderivableLegs = 2, want 0` **did** go to zero
and leg D now states `RECORD<ID LONG NULL, V LONG NULL>` where it previously stated none. So
stating the row does make the leg derivable. **Leg derivability was simply not the cause of
either user-visible failure.**

The causes were then found by differential, not by reasoning, in
`pkg/relational/core/embedded`'s no-FDB planner harness — 17 arms varying one thing at a time.
There are **three** defects, not two, and none of them is the one §1 originally named.

**AND THE THREE ARE LAYERED, WHICH IS ITSELF A FINDING AND IS WHY ONE STORY FIT FOR SO LONG.**
1a gates the CTE query strictly EARLIER than 1b: the query is refused at name resolution and
never reaches the planner, so the executor defect underneath it is unobservable. Reverting 1b's
fix alone therefore does not restore the CTE arm's original 42703 — it moves that arm ONTO 1b's
executor failure, because with 1a fixed the query now gets far enough to hit it. Two independent
defects presenting through one visible symptom is exactly the configuration that makes a single
tidy root-cause narrative feel confirmed, and it is why the mutation check here had to be run
**per direction** rather than reasoned about: a whole-diff revert restores the original symptoms
and would have "confirmed" the very story §1 refutes. §6a.4 carries the run.

### 1a. The CTE arm: the subquery's outer scope registers no WITH leg (42703)

`buildOuterScopeSources` (`logical_predicate.go:7994`) builds the scope an EXISTS/scalar
subquery resolves its correlated references against. Its `addSrc` resolves each FROM leg
through `analyzer.ResolveTable` and **returns silently when that misses**. A CTE name is not a
catalog table, so a WITH leg was never registered and `c.id` inside the subquery had nothing to
bind to.

The arm that settles it is the one that looks redundant: **the same query with the EXISTS moved
into the WHERE fails identically**, and so does the unjoined form.

```
cte_proj_exists        PLAN ERROR: 42703: no FROM source aliased as C
cte_where_exists       PLAN ERROR: 42703: no FROM source aliased as C
cte_where_exists_nojoin PLAN ERROR: 42703: no FROM source aliased as C
cte_proj_exists_corr_t3 OK          ← same query, EXISTS correlated to the REAL table instead
```

So it is neither the projection nor the join nor the EXISTS: it is **which legs the outer scope
contains**. The site already knew — `logical_predicate.go:8766` says an unaliased CTE leg "is
absent from p.outerScopes (addSrc drops catalog-resolution failures)" and books the fix as
follow-on work. The derived-table leg beside it was registered for exactly this reason; the CTE
leg was the residual gap.

**Fix:** `addSrc` consults the CTE registry the enclosing query's own SELECT/WHERE/ON resolvers
already consult, taking the column schema from the registry and the alias and correlation from
*this* reference. This has nothing to do with result types and is the one finding that could
reasonably have been a separate RFC; it is folded here because it is 20 lines and because the
two failures were reported as one symptom.

> **INVARIANT (constraint on this fix, not an observation about it): every scope builder that
> resolves a FROM leg resolves CTE-FIRST, and a declared-but-underivable CTE DECLINES rather
> than falling through to the catalog.** `buildSelectScope` and `buildOuterScopeSources` are two
> implementations of one rule, and they are only correct together. Anyone adding a third scope
> builder inherits this, and anyone "simplifying" either into a catalog-first lookup with a CTE
> fallback reintroduces a shadowing bug that plans successfully and returns the wrong column set.

**The ORDER is load-bearing and was got right by reading the site it has to agree with, not by
inventing one.** The first version made the registry a FALLBACK after a catalog miss.
`buildSelectScope`'s `addSource` (`logical_predicate.go:3238`) is **CTE-FIRST**, with a comment
recording that the catalog-first order was already a review-caught bug — it analyzed the
TABLE's schema for reads that execute against the CTE — and with a TOMBSTONE rule: a declared
CTE whose schema is not derivable (nil `Table`) DECLINES rather than falling through, because a
same-named base table would otherwise bind its ordinals onto the CTE's rows. A fallback ordering
in the outer-scope builder would have reintroduced exactly that divergence one scope level down,
which is the failure mode the fix is named for. Both the order and the tombstone are mirrored,
and the pin is discriminating rather than incidental: the shadowing CTE emits a column the
shadowed table does not have, so the two orders give different answers and the mutation reads
`42703: column "T1_ID" does not exist`.

### 1b. The derived-table arm: the join's step-1 ordinal seed refuses a PROJECTION leg

This one is a genuine type-adjacent defect and it is where the RFC's supply-side work pays off.

`ImplementNestedLoopJoinRule`'s 3-quantifier join+EXISTS arm asks `foldStep1Seed` for the
leg-concat ordinal seed the materialized NLJ produces as its merged row. `legOrdinalSafety`
(`rule_implement_nested_loop_join.go:1974`) enumerates the leg shapes that can seed it — scan,
index, covering index, through transparent wrappers — and **had no arm for
`RecordQueryProjectionPlan`**, so a derived-table leg fell to the default refusal. Instrumented:

```
ZZPROBE foldStep1 RECONSTRUCT-NIL left=*plans.RecordQueryProjectionPlan right=*plans.RecordQueryScanPlan
   shape="rv=RecordConstructorValue (NOT a positional merge)"
ZZPROBE foldStep1 ACCEPT         left=*plans.RecordQueryScanPlan       right=*plans.RecordQueryScanPlan
```

With the seed refused, the NLJ keeps the SELECT's **own folded projection** as its result
value. The plan dump shows it directly — same pointers as the FlatMap above it:

```
derived:  NLJ outerAlias=T3 innerAlias=D  resultValue=[ID, T1_ID, H]     ← the projected fold
table:    NLJ outerAlias=T3 innerAlias=T1 resultValue=[ID, T1_ID, ID, V] ← the leg concat
```

`[ID, T1_ID, H]` is not a row any leg concat produces — `H` is the EXISTS the FlatMap above
computes — so the correlated `D.ID` read finds no binding for D, falls to the positional row,
and hits the loud multi-leg refusal in `values.go:1262`. The base-table twin, differing only in
that its leg is a scan, was correct throughout.

**Fix:** a projection leg is ordinal-safe and contributes its own stated row. The executor says
so outright: `executeProjection` (`executor.go:2543`) *always* emits a dense `PositionalRow`
with one slot per projection, named by `values.OutputColumnName`. The walk stops at the
projection rather than descending, because what the leg flows is the projected row, not the
scan's.

**This is the fix RFC-226's supply side enables.** `planBuriedLegConcat` builds the seed from
`GetResultType().(*values.RecordType)`; before this RFC a projection answered `UnknownType` and
the concat could not have been built at all. A comment three lines above already conceded the
shape of the problem — *"GetResultType() cannot be used for a NLJ leg — it is a stub"*. So the
type work is not the fix, it is the **prerequisite** for it, and that is the honest form of the
claim §1 originally overstated.

### 1c. The regression this RFC's own change introduces, found by the same differential

Measured by reverting the RFC diff and re-running the same 17 arms: on master
`derived_where_exists` **plans**; with the RFC's core applied it fails with *"best expression
is not a physical plan: LogicalProjectionExpression"* — no physical plan at all.

`materializedNLJOrdinalLayoutMatches` identifies which physical leg occupies which slot of a
baked seed by comparing the seed's leg windows against each leg plan's own row. The seed's
windows for a derived leg carry names with **no inferred type**; the leg's plan, now that a
projection states its row, carries LONG. `recordFieldsMatch` used `values.Field.Equals`, which
requires type equality, so:

```
ZZPROBE layout DECLINE
  run0=RECORD<ID UNKNOWN NULL, V UNKNOWN NULL>   outer=RECORD<ID LONG NULL, V LONG NULL> (Projection)
  run1=RECORD<ID LONG NULL, T1_ID LONG NULL>     inner=RECORD<ID LONG NULL, T1_ID LONG NULL> (Scan)
```

Both orientations declined, and since this gate is what admits the materialized NLJ at all, the
query was left with **no plan** rather than with one fewer alternative. The gate's own doc says
"declining is always safe" — that was true when a decline cost an alternative; it is not true
here, and the doc is corrected with the fix.

**Fix:** an unstated field type is not a difference. `UnknownType` means "not inferred", so
requiring equality against it turns an ABSENCE into a MISMATCH. Names and ordinals still have to
agree, and two legs that both state a type still have to agree on it — both directions pinned.

The relaxation is scoped to one decision, and that is checkable rather than asserted:

```
$ grep -rn "recordFieldsMatch(" --include='*.go' pkg/ | grep -v "_test.go"
…:2435:  ok := recordFieldsMatch(runs[0].Typ, outerType) && recordFieldsMatch(runs[1].Typ, innerType)
…:2473:  func recordFieldsMatch(a, b *values.RecordType) bool {
```

One production caller — the orientation gate — plus the definition. And the corpus agrees it is
scoped: the plan-shape golden over 2522 queries moved only in the one scenario this change
edits (§6a).

**This is the finding that most changes how this RFC should be read.** The supply-side change is
correct and Java-faithful, and it is *not safe on its own*: turning `UnknownType` into a real
type switches on consumers that were fail-open while it was unstated, and at least one of them
was comparing against a value that was never going to match. Rev 4's blast-radius section
(§5) enumerated readers of the two changed methods; it did not catch this, because the
consumer that broke reads `GetResultType()` **through a helper on a different node's plan**
(`planRowRecordType`), which no sweep for the changed methods' call sites would surface. A
type-supply change needs a *behavioural* differential over shapes, not a call-site census.

### 1d. What the probe pins now

`pkg/relational/sqldriver/projection_result_type_probe_fdb_test.go` (real FDB) asserts ROWS on
all seven arms, EXISTS column included by value; the fast sentinel
`pkg/relational/core/embedded/derived_source_exists_plan_test.go` pins the TEN plan shapes (an earlier draft said nine; the subtests count ten); and
`pkg/recordlayer/query/plan/cascades/projection_leg_ordinal_seed_test.go` pins the three
decisions themselves, which a functional test cannot distinguish from any other route to a
working plan. Each of the three fixes was mutated separately and each went RED alone (§6).

---
## 2. Java is the spec

| Java | file:line | what it says |
|---|---|---|
| `RelationalExpression.getResultType()` | `expressions/RelationalExpression.java:194-197` | `default Type.Relation getResultType() { return new Type.Relation(getResultValue().getResultType()); }` |
| override count | — | **No relational expression and no physical plan in Java overrides it.** The row type is *always* the result value's type. |
| `LogicalProjectionExpression.getResultValue()` | `LogicalProjectionExpression.java:102-106` | `return inner.getFlowedObjectValue();` |
| its javadoc | `LogicalProjectionExpression.java:47` | *"this expression is only used when we plan `RecordQuery`s"* — **the Go comment's claim is verified** |
| its only two construction sites | `RelationalExpression.java:187`, `LogicalProjectionExpression.java:98` | the legacy `fromRecordQuery` bridge, and its own `translateCorrelations`. **Zero hits under any `fdb-relational-*` module.** |
| and it never survives | `rules/RemoveProjectionRule.java:55-60` | `// just remove the projection` — `call.yieldPlan(innerPlan)` |
| the REAL SQL projection | `GraphExpansion.java:396` | `new SelectExpression(RecordConstructorValue.ofColumns(resultColumns), quantifiers, getPredicates())` |
| driven from SQL | `relational/…/query/LogicalOperator.java:475-481` | `expandedOutput.underlyingAsColumns().forEach(selectBuilder::addResultColumn)` |
| its physical twin | `RecordQueryMapPlan.java:78-82, 143-147` | stores `resultValue`; defines **no** `getResultType` — inherits the default |
| unnamed column naming | `Type.java:2646-2651`, `:2920-2924`, `:371-374` | `"_" + i`, 0-based ordinal; an already-auto-generated name is regenerated |
| memo unification | `Reference.java:504-513` | reduces `getResultType()` over all members with `Verify.verify(left.equals(right))` |

**The load-bearing reading.** Java's `LogicalProjectionExpression` gets away with returning its
inner's row because it is legacy-only and is *always erased before planning finishes*. Go uses
that class as the SQL projection — the job Java gives `SelectExpression`/`RecordQueryMapPlan` —
and Go's projection **does** survive into the plan. The inherited contract is therefore not
merely unhelpful in Go, it is false, and `Reference.getResultType()`'s `Verify.verify` shows
Java would not tolerate the Go arrangement for a moment.

Java's actual contract for the node holding this role is one sentence: **the projection's
result value is a record constructor over its columns, and its result type is that value's
type.**

---

## 3. Capability check — nothing is missing

Every ingredient already exists in Go; this is not blocked on building anything.

| need | Go | file:line |
|---|---|---|
| record constructor value | `RecordConstructorValue`, `NewRecordConstructorValue(fields ...RecordConstructorField)` | `values/values.go:4481`, `:4545` |
| named-field record type | `RecordType{Fields []Field{Name, FieldType, Ordinal}}`, `NewRecordType` | `values/type.go:347`, `:310`, `:713` |
| Java's `"_" + i` | `OrdinalFieldName(ordinal int) string` (0-based), inverse `IsOrdinalFieldName` | `values/type.go:854`, `:865` |
| memo unification (Java's `Reference.getResultType`) | `Quantifier.GetFlowedObjectType` — ported deliberately, returns `*MemberResultTypeDisagreementError` instead of crashing | `expressions/quantifier.go:302`, error at `:271` |

Two preconditions the code itself states:

1. **Use `NewRecordConstructorValue`, never `NewRawRecordConstructorValue`.** `values.go:4572-4576`:
   *"NEVER use this for a projection RC: NewRecordConstructorValue (above) appends `_2`/`_3`
   suffixes, which is correct there (SQL projection column naming) — a raw duplicate under
   name-keyed plan-time lookup (FieldIndex first-match) silently resolves to the first match,
   the exact conflation ordinal identity exists to avoid."*
2. **`RecordConstructorValue.Type()` (`values.go:4596`) copies `f.Name` verbatim** and does not
   apply `OrdinalFieldName`. Go normalizes only at proto-lowering (`proto_type.go:304-308`).
   So an unnamed projection slot would yield `Field{Name: ""}` and feed the name-keyed readers
   an empty string.

---

## 4. Decision

**Both projection nodes state the row they produce, as a record constructor over their own
columns, and derive their result type from it. Nothing recovers it by name.**

Concretely:

1. **`LogicalProjectionExpression.GetResultValue()`** returns
   `values.NewRecordConstructorValue(fields...)` built from `GetProjectedValues()` paired with
   `GetAliases()`. The `inner.GetAlias()` QOV passthrough is deleted, and the standing
   "do not clean this up" warning at `:94-121` is replaced by the reason the new answer is right.
2. **`RecordQueryProjectionPlan`** gains its own `GetResultValue()` built the same way from
   `GetProjections()`/`GetAliases()` — it currently inherits `PlanExprBase.GetResultValue`
   (`plans/plan_expression.go:97`), which mints a *fresh unique correlation alias*, so the type
   cannot be derived from the inherited value and must come from the columns directly.
   **`GetResultType()` then returns `GetResultValue().Type()`** and the `UnknownType` decline
   at `:144` is deleted.
3. **Every slot is named.** A slot with no user `AS` and no derivable display name takes
   `values.OrdinalFieldName(i)` — Java's `"_" + i`, 0-based, the constant Go already has.
   A projection never emits `Field{Name: ""}`, and that is asserted, not assumed (§6.4).
4. **An IDENTITY projection states its inner's row, not a one-slot wrap of it.** This is the
   design's load-bearing case and rev 1 got it wrong by omission.

   Two rules yield the inner expression **into the projection's own reference**, gated on
   identity — verified in the tree:

   ```go
   // rule_remove_projection.go:41-51   (gate at :41, yield at :50)
   if !projW.IsIdentity() { return }
   innerExpr := findPhysicalExpr(innerRef)
   if innerExpr == nil { return }
   call.Yield(innerExpr)

   // rule_projection_elim.go:65        (one-slot QOV-over-inner gate at :47-63)
   call.Yield(p.GetInner().GetRangesOver().Get())
   ```

   So the projection and its inner become **co-members of one equivalence class**. Under items
   1-2 taken naively, the projection member would state `RecordConstructor[_0: <inner row>]` —
   *one* field wrapping the row — while the co-member inner states the inner row itself, *N*
   fields. The arities differ, so `refineRowTypes` cannot reconcile them and
   `Quantifier.GetFlowedObjectType` (`quantifier.go:302`) returns
   `MemberResultTypeDisagreementError` for **every** quantifier over that reference. On
   `SELECT *` — the most common shape in the corpus. Today the projection contributes nothing
   (`quantifier.go:324` `continue`) and the mismatch is invisible; the change would turn it
   into a hard planner failure.

   > **MEASURED, and it overturns the paragraph below.** Rev 2 justified the identity arm purely
   > from memo co-membership and did not check the executor. It was asked to, and the answer is
   > that **the executor WRAPS**. `executeProjection` (`executor/executor.go:2545-2614`) does
   > `slots := make([]any, len(projections))`, evaluates one projected value per slot, and
   > always emits `QueryResult{Positional: &PositionalRow{Type: projType, Slots: slots}}` with
   > `projType := positionalTypeFromNames(posNames)` over exactly `len(projections)` names. For a
   > one-slot identity projection that is a **1-slot row wrapping the inner row**, not the inner's
   > N-field row.
   >
   > So stating `inner.GetFlowedObjectValue()` would make the stated type differ from the row
   > actually produced — a wrong-slot read, which is worse than the disagreement it was meant to
   > avoid. **Option (a) is refuted by measurement and this sub-decision is withdrawn.**
   >
   > The measurement also reframes the problem. The projection member and the yielded inner
   > member genuinely produce *different rows* — 1 wrapped slot versus N fields — so they are not
   > semantically equivalent and should not be co-members of one equivalence class at all. The
   > disagreement is not a reporting artifact to be smoothed over; it is
   > `Quantifier.GetFlowedObjectType` correctly detecting that two members of one class emit
   > different shapes. Today the projection's silence hides it.
   >
   > **The sweep is done and it settles the question — (c), and more cheaply than expected.**
   >
   > Command and result: of 31 references to the two projection constructors,
   > **15 are real call sites** (the other 16 are the constructor definitions), split
   > **6 ORIGIN / 9 REBUILD**. Every REBUILD is a 1:1 map over an inbound list and cannot
   > introduce a shape. **Every one of the 6 ORIGIN sites emits `*values.FieldValue` slots and
   > none can emit a bare QOV**, proven by a zero-hit sweep with a well-formedness control:
   >
   > ```
   > $ grep -rnE "(projected|projVals|projs|newProjs|composed)\[[a-zA-Z]+\] *= *values\.NewQuantifiedObjectValue" --include='*.go' pkg/ | grep -v '_test.go'
   > (no output)
   > $ grep -rnE "(projected|projVals|projs|newProjs|composed)\[[a-zA-Z]+\] *= *values\.NewFieldValue" --include='*.go' pkg/ | grep -v '_test.go'
   > pkg/relational/core/query/cascades_translator.go:4813
   > pkg/relational/core/query/cascades_translator.go:7065
   > ```
   >
   > And the decisive structural fact: **`SELECT *` builds no projection node at all.** The star
   > is dropped at parse time (`pkg/relational/core/embedded/select_parser.go:685` — *"nil = SELECT \* …"*, `:788` —
   > *"SELECT \* — projCols stays nil"*) and the builder skips the operator
   > (`pkg/relational/core/embedded/logical_builder.go:714` — `if len(sq.projCols) > 0 {`). Everywhere Go *does* expand a star
   > it expands **per field**, Java's `GraphExpansion.java:396` behaviour reached by a different
   > route (`cascades_translator.go:9130`/`:9139`, `:2536`, `:7717-7731`,
   > `core/query/ddl/generator.go:418-473`, whose comment says so outright: *"Java has expanded
   > the star into every column by this point, so Go expands it here"*).
   >
   > **So the ill-shaped member is already unreachable from SQL.** `IsIdentity()` is called on
   > real traffic from two sites (`rule_remove_projection.go:41`, `fk_chain_cardinality.go:290`)
   > and **never returns true** there; its only producers are ~8 test files and the two planner
   > fuzzers (`planner_fuzz_test.go:220-223`, `plan_extraction_fuzz_test.go:88-91`). Both
   > elimination rules are registered (`default_rules.go:72`, `:322`) and both are dead on SQL
   > traffic.
   >
   > **Decision: (c).** Make the one-slot whole-row projection **unbuildable**, so the
   > disagreement is unreachable by construction rather than caught by a consumer. (b) loses on
   > the repo's own rule that emergent structure beats a downstream check: it leaves the
   > ill-shaped member constructible and merely forbids one consumer from noticing, which is the
   > bolted-on `if X { throw }` that diverges the moment the structure moves. (a) is refuted by
   > the executor measurement above. Nothing in the sweep needs a one-slot whole-row projection —
   > the finding that would have killed (c) did not appear, and it was looked for.
   >
   > Because no SQL path builds one, **(c) costs nothing in reach**: it forbids a shape only tests
   > and fuzzers construct. The general contract of items 1-3 and 6 therefore applies to *every*
   > projection on real traffic with no identity special case at all — which is why this
   > sub-decision shrinks rather than grows the design.
   >
   > **`rule_remove_projection.go:50` and `rule_projection_elim.go:65` under (c):** their guards
   > become unsatisfiable by construction, so both rules become dead code. They are **deleted**,
   > not left registered-and-unreachable — a registered rule that cannot fire is precisely the
   > dead-twin shape `#683` and RFC-217/RFC-221 removed from this tree.
   >
   > **And deleting them is not a divergence from Java, because Go's rule was never a faithful
   > port.** This is the load-bearing argument and it is stronger than "Go's star builds no
   > projection", which is true but incidental. Java's `RemoveProjectionRule` is **ungated** —
   > `RemoveProjectionRule.java:55-60` is literally `// just remove the projection` followed by
   > `call.yieldPlan(innerPlan)` — and it can be, because Java's `LogicalProjectionExpression` is
   > legacy-only and returns its inner's flowed value, so removal is *always* type-preserving.
   > Go bolted an `IsIdentity()` guard onto it precisely because Go uses that node as the SQL
   > projection, where removal is **not** generally type-preserving. Go had therefore already
   > diverged: it kept the registration while the Java rule's referent — the `fromRecordQuery`
   > bridge at `RelationalExpression.java:187`, with zero hits under any `fdb-relational-*`
   > module (§2) — does not exist in Go at all. Deleting a guard-neutered vestige of a rule whose
   > subject Go never ported is not removing a Java rule; it is finishing a conversion that
   > stopped halfway.
   >
   > **Caveat, recorded so the deletion is reversible on purpose rather than by accident: if Go
   > ever ports the legacy `RecordQuery` bridge (`RelationalExpression.fromRecordQuery`), the
   > rule comes back with it** — ungated, as Java has it, because at that point the node really
   > would be legacy-only and removal really would be type-preserving.
   >
   > **`IsIdentity()` itself is deleted** (`plans/projection.go:130-142`), not left standing.
   > Under (c) it is always-false, and a live-looking predicate that can never fire is how
   > someone reintroduces the shape by writing a producer for it. It goes together with its three
   > callers: `rule_remove_projection.go:41`, `rule_projection_elim.go:47-63`, and
   > `fk_chain_cardinality.go:290`'s always-false fast path, whose general branch below computes
   > the same result anyway.
   >
   > **STATUS: (c) IS NOT IMPLEMENTED — and rev 5's code claimed it was.** The design below
   > stands as the plan for the follow-on change (see §6b), but what SHIPPED has no constructor
   > guard at all: all seven `LogicalProjectionExpression` constructors are plain struct fills,
   > the one-slot whole-row shape is fully CONSTRUCTIBLE, and `values.ProjectionResultValue` —
   > which is a DERIVATION, not a constructor — merely declines to synthesise a row for it, so
   > `GetResultValue` falls back to an untyped QOV. Rev 5's source comments asserted the shape
   > was "unbuildable, enforced by the constructors below" and the fallback "pinned as unreachable
   > by test"; a passing test built the shape and reached the fallback. Those claims are deleted
   > and the arm is now pinned as REACHABLE, in both directions
   > (`TestLogicalProjectionFallsBackToUntypedQOV` for the declining shape,
   > `TestLogicalProjectionStatesProjectedRow` for the ordinary one — the pair is what
   > distinguishes "projections decline" from "this ONE shape declines", which a single test
   > could not).
   >
   > Note also that the declining population is **strictly larger than `IsIdentity()`**: the guard
   > fires on any single bare QOV, including one over a FOREIGN correlation, which is not an
   > identity projection. The §4.4 sweep measured identity projections and is therefore a LOWER
   > bound on how often the fallback is taken.
   >
   > **Acceptance for (c) is unbuildability, not detection**, and it needs TWO arms because one
   > of them cannot see the whole risk:
   >
   > **(i) The guard lives AT THE CONSTRUCTOR**, not in a review sweep. The ORIGIN/REBUILD sweep
   > above enumerates *call sites*, and a call-site enumeration structurally **cannot see a shape
   > synthesized at runtime** — `rule_projection_merge.go` composes P1∘P2 and
   > `rule_merge_projection_and_fetch.go` rebuilds across a fetch, so either could compose a
   > one-slot whole-row list from inputs that are individually fine. That is exactly why this is
   > a constructor guard and not a grep: the grep is evidence about today's sources, the guard is
   > a property of every future composition.
   >
   > **(ii) A corpus census that `GetFlowedObjectType` never returns
   > `MemberResultTypeDisagreementError`, with a non-vacuity floor on references reduced.** (c)
   > makes *one* ill-shaped member unreachable; it does not prove no *other* rule yields a
   > differing-arity co-member into a reference. Without the floor, a census over zero references
   > reports the same clean zero as a census over a healthy corpus — the empty-set false positive.
   >
   > The fuzz corpora that currently build identity projections
   > (`planner_fuzz_test.go:220-223`, `plan_extraction_fuzz_test.go:88-91`) are updated in the
   > same change, and that update is itself evidence the shape is gone rather than hidden.

   ~~**Decision: when `IsIdentity()` holds, the stated value is `inner.GetFlowedObjectValue()` —
   the row itself.**~~ Chosen over the alternative (stop both rules yielding into the
   projection's reference) because it is the Java-faithful one, and the asymmetry explains why:
   Java's row-preserving projections are *exactly* the ones `RemoveProjectionRule` erases, and
   `LogicalProjectionExpression.getResultValue()` returning `inner.getFlowedObjectValue()`
   (`:102-106`) is what lets Java survive its own `Verify.verify` at `Reference.java:509`. Java
   never builds a one-slot whole-row projection at all — `GraphExpansion.java:396` expands
   `SELECT *` into per-field columns — so Go's identity projection has no Java counterpart and
   the general contract cannot be lifted onto it unexamined.

   This is not a special case bolted on: it is the same rule stated once. *A projection states
   the row it produces.* An identity projection produces its inner's row; a shape-changing one
   produces a record over its columns. The existing `IsIdentity()` predicate
   (`plans/projection.go:130-142`) already decides exactly that, and it is algebraic — one slot
   holding a QOV over this projection's own inner quantifier — not a structural proxy.

   ---

   #### 4.4′ Three corrections folded from the NAK, each of which stands

   **(1) The Java claim above is factually false, and the conclusion survives for a better
   reason.** This RFC said Java "never builds a one-slot whole-row projection at all" because
   `GraphExpansion.java:396` expands `SELECT *` per field. It does not. `LogicalOperator.java`'s
   `canAvoidProjectingIndividualFields` (declared `:503`, called `:476`) is a fast path that
   fires on exactly a single-quantifier `SELECT * FROM T` whose expansion matches the underlying
   record field-for-field, and it then does
   `selectBuilder.build().buildSelectWithResultValue(passedThroughResultValue)` with the
   quantifier's own whole-row value. **Java does build a whole-row-stating projection.**

   The real divergence is a shape difference, and naming it correctly is what makes the argument
   robust: **Java's `RecordQueryMapPlan` holds ONE `Value` — the result value IS the row — while
   Go's `RecordQueryProjectionPlan` holds a COLUMN LIST.** A length-1 list whose single element
   is a whole-row QOV is therefore not "the row"; it is necessarily a **1-slot wrap** of the row,
   which is exactly what `executeProjection` emits. Java has no way to express Go's ill-shaped
   member because its projection has no slot list to put a whole row into. So the conclusion —
   Go's one-slot whole-row projection has no Java counterpart — is right, and it follows from
   the storage shape rather than from a false claim about star expansion.

   **(2) The two elimination rules are UNSOUND, not merely dead, and the deletion is re-founded
   on that.** `rule_remove_projection.go:50` and `rule_projection_elim.go:65` yield the inner
   expression into the *projection's own* reference. That rewrites a 1-slot row into an N-field
   row — a wrong row, not a lost alternative — for any projection whose guard passes. It has
   been invisible only because `GetResultType()` returned `UnknownType` and `quantifier.go:324`
   `continue`d past the member, so nothing ever compared the two shapes.

   Founding the deletion on unsoundness rather than on unreachability matters because it stops
   depending on a reachability sweep that could be wrong tomorrow: a rule that produces a wrong
   row when it fires must go whether or not it currently fires. The same applies to
   `fk_chain_cardinality.go:290` — `return childThread` for a one-slot plan is *unsound*, not
   merely unreachable, and its general branch below already computes the right answer.

   **(3) "Unbuildability" is not what (c) delivers, and the acceptance is restated.** All seven
   projection constructors are plain struct fills with `slices.Clone` and no validation, so the
   ill-shaped value still compiles after both deletions. The property (c) actually delivers is a
   **yield invariant**:

   > *No rule yields a child expression into its parent's reference unless the parent is
   > row-preserving.*

   That is checkable and it is the thing that was violated. The population is already swept —
   `rule_projection_elim.go:65`, `rule_sort_constant_keys_elim.go:60`,
   `rule_unsorted_sort_elim.go:44`, `rule_set_op_singleton.go:33` and `:64`, plus
   `rule_remove_projection.go:50` — and once the two projection rules are gone, **the four
   survivors all forward the row literally**, so the invariant holds by inspection over the
   whole population rather than by a constructor guard that cannot be written.

   **(4) `memo.Integrate`'s group-merging path is the route no lap has read.** Differently-shaped
   co-members can arrive in one reference not only by a rule yielding into a parent's reference
   but by two references being MERGED. Rev 5 does not claim that path is clean; it names it as
   the open edge of the yield invariant, because the invariant as stated quantifies over rule
   yields and group merging is not a rule yield. The census in §6 is what covers it empirically
   — a `MemberResultTypeDisagreementError` arising from a merge is indistinguishable at the
   consumer from one arising from a yield, which is precisely why the census is denominated in
   references reduced rather than in rules fired.

   ---

5. **The two nodes share one builder** so logical and physical cannot drift, including the
   identity arm. Their identity keys already share `values.ProjectionOutputIdentityKey` for
   exactly this reason; the row they state gets the same treatment. This is a guard against
   drift between two implementations, not a property check — when RFC-213 puts
   `GetResultValue()` on `RecordQueryPlan` and derives the type from it, the physical node's
   value *becomes* the logical one and the guard is deleted rather than maintained.

6. **`RecordConstructorValue.Type()` (`values.go:4596`) becomes the single normalization
   point**, applying `OrdinalFieldName(i)` to any empty field name — matching Java, where
   normalization lives in `Type.Record.fromFields` (`Type.java:2532` → `normalizeFields`
   `:2617-2682`) and not in each caller. Rev 1 put the naming at the projection construction
   sites and called the central question "open"; that was two mechanisms for one concept
   (construction-site naming plus `proto_type.go:304-308` normalizing again at proto lowering),
   and a per-site discipline is broken by the next constructor site that forgets. "No field is
   unnamed" is an invariant of the record type. The measured surface is stated in §7.
7. **RFC-213 alignment:** `Projection` moves from tier 2 to tier 1. §4 of that RFC (put
   `GetResultValue()` on `RecordQueryPlan`, derive `GetResultType()` from it) is the arrangement
   this change adopts locally for one plan; when RFC-213 lands, item 2 above collapses into its
   general rule and the explicit `GetResultType()` override is deleted.

### Why the alternatives lose

- **Keep the status quo.** Two live loud defects, one of them unpinnable, and Java's
  `Reference.getResultType()` `Verify.verify` shows the arrangement is not representable in the
  spec. Not a candidate.
- **`GetInner()` passthrough (RFC-213 rev 2 tier 2).** This is the *current* logical-side
  behaviour re-spelled, and it is what produces `multi-leg row cannot serve a source-relative
  ordinal`. A projection over a two-leg join would claim to flow both legs when it flows the
  projected columns. It is wrong for the same reason the existing site is wrong, and it would
  make the physical twin agree with the logical one **on the false answer** — strictly worse
  than today, because today the physical side at least declines honestly.
- **Restructure Go onto Java's `SelectExpression` + `RecordQueryMapPlan` naming.** The most
  literally-Java option, and it was considered against the "long-term correct, not smallest
  diff" criterion. It loses on the merits, not on size: Go's `RecordQueryProjectionPlan`
  already *is* Java's Map plan semantically — it holds columns and an inner quantifier and maps
  one row to another. Adopting Java's **contract** (record-constructor result value; type
  derived from it) captures 100% of the semantic content; adopting its **class names** captures
  zero further semantics while rewriting every rule and executor arm that names the Go node.
  The rename is a real question and it is a separate, purely-nominal one; conflating it with the
  semantic fix would make an already-wide behavioural change unreviewable.
- **Fix only the physical side (A).** Insufficient and measurably so: the leg-underivability
  runs through `Quantifier.GetFlowedObjectType`, which reads **`GetResultValue()` on memo
  members** — the logical side. A alone leaves `UnderivableLegs = 2`.
- **Teach the name-keyed readers to guess better.** This is the mechanism B5 exists to delete.

### What this change does NOT close, stated plainly

B5's headline impact is *"wrong client VALUES on cross-leg same-name-different-type"*. That
travels through `deriveColumnsFromPlan` (`cascades_generator.go:3693`), which **reads no result
type at all** — `grep -n 'GetResultType' pkg/relational/core/embedded/cascades_generator.go`
returns zero hits — and dispatches on concrete plan type into the five guessers of §0b.
**A + B alone produce no client-metadata improvement.** They make the type *available* where it
is currently absent, which is the precondition the guessers' deletion needs.

**And that creates a hazard this RFC must handle rather than note.** Today the cross-leg
same-name-different-type defect is partly marked by the loud failures of §1. After this change
those failures are gone and the wrong-VALUES defect goes **silently** live — strictly worse for
anyone reading a green suite. Deleting the five guessers of §0b is genuinely a separate change
(it rewrites `deriveColumnsFromPlan`'s whole dispatch), but shipping this one without a sentinel
is not acceptable.

**So this change lands a committed reproducer pinning the CURRENT wrong cross-leg VALUES** — a
test that asserts today's wrong behaviour with a failure message naming exactly what it means
when it flips ("the guessers were deleted or fixed; replace this with the correct VALUES"). It
is a characterization pin, red-when-fixed by design, and it is the sentinel that makes the
guesser deletion's completion visible instead of silent. Without it, §4's scope statement would
be a deferral wearing a lab coat.

---

## 5. Blast radius — MEASURED, and the method that MISSED one

**Read §1c before this section.** The census below is a call-site enumeration of the two changed
methods, and it did not surface the consumer that broke: `materializedNLJOrdinalLayoutMatches`
reads `GetResultType()` **on a different node's plan, through a helper** (`planRowRecordType`),
so it appears in neither list as a caller of anything this RFC touches. A call-site census
answers "who calls the method I edited"; it cannot answer "who was relying on the value that
method used to return". For a change that turns `UnknownType` into a real type, the second
question is the whole risk — every fail-open consumer keyed on "unstated" switches on at once —
and only a behavioural differential over query shapes reaches it. The counts below stand; the
method that produced them is no longer presented as sufficient.

**State this as a rule for any future "X now answers instead of declining" change**, because the
shape recurs and the census method will look adequate every time: enumerate the CONSUMERS OF THE
VALUE, not the callers of the method. The two populations differ exactly where a helper sits
between them, and a helper is the normal case rather than the exotic one — `planRowRecordType`
is three lines. The tractable substitute for an impossible enumeration is a differential over
query shapes that exercises the newly-stating node in each ROLE it can occupy (here: as a join
leg, as a subquery source, and as the query root), which is what §6a.1's 17-arm matrix is.

The counts, each with the command that produced it:

```
$ grep -rn '\.GetResultType()' --include='*.go' pkg/ | grep -v '_test.go' | wc -l
56          # across 40 files; interface declared once, plans/plan.go:70
$ grep -rn '\.GetResultValue()' --include='*.go' pkg/ | grep -v '_test.go' | wc -l
159         # across 68 files; interface declared once, expressions/expression.go:72
```

Rev 1 wrote "22 of the 56" and "21 of the 159" citing "working notes". The first is right, the
second was wrong and conflated call sites with definitions. Both now carry their command:

```
$ grep -rn '\.GetResultType()' --include='*.go' pkg/recordlayer/query/plan/plans/ | grep -v '_test.go' | wc -l
22          # pure delegation: `return inner.GetResultType()`
$ grep -rn "func .*GetResultValue() values.Value" --include='*.go' pkg/ | grep -v '_test.go' | wc -l
54          # GetResultValue DEFINITIONS (the 159 above are CALL sites — a different population)
$ grep -rn "func .*GetResultValue() values.Value" --include='*.go' -A3 pkg/ | grep -v '_test.go' | grep -c "GetFlowedObjectValue()"
19          # of those 54, the ones forwarding an inner flowed value
```

So **22 of 56** result-type sites and **19 of 54** result-value definitions are transitive
amplifiers: the moment a projection states a type, every wrapper stacked above one starts
stating one too. That is the intended effect and it is the bulk of the radius.

The sites that **decide** rather than forward, in risk order:

| site | today | after | risk |
|---|---|---|---|
| `rule_remove_projection.go:50` and `rule_projection_elim.go:65` | yield the inner **into the projection's own reference**, making a 1-slot-wrapping plan and an N-field plan co-members of one class; the projection contributes no type so the arity mismatch is invisible | **DELETED under Decision §4.4(c).** Their guards are unsatisfiable once the one-slot whole-row projection is unbuildable | **Was rated HIGHEST in rev 2 on the belief that this collides on `SELECT *`. That belief was WRONG and this RFC's own sweep refuted it:** `SELECT *` builds no projection node at all (`pkg/relational/core/embedded/select_parser.go:685`, `pkg/relational/core/embedded/logical_builder.go:714`), and no ORIGIN site emits a bare QOV, so the collision is unreachable from SQL and was only ever reachable from tests and the two planner fuzzers. The residual risk is not a wrong row — it is a *dead rule left registered*, which reads as coverage. Hence deletion rather than a guard |
| `Quantifier.GetFlowedObjectType` `quantifier.go:324` | projection member hits `rt == nil` → `continue` | contributes a row type, and can now **disagree** with a sibling → `MemberResultTypeDisagreementError` | **highest** — a silent skip becomes a hard error. `AllMembers()` includes *exploratory* members, so the comparison is against more than the winner |
| `distinctKeyColumns` `rule_implement_distinct_final.go:904` | concrete-type branch short-circuits; generic branch returns nil | wrappers above a projection take the generic branch and **mint resolved ordinals** | **high** — starts baking ordinals. Its comment's stated invariant ("its GetResultType is always UnknownType") becomes false and must be rewritten |
| `bakedIntersectionKeys` `intersector_primary_key.go:1220` | always declines on a projection leg | **REFUTED — no change.** The whole-corpus census reads `bakedIntersectionKeys resolved 412 UNRESOLVED 0`, so no stub-typed leg reaches it | **none** — the "high" rating was contradicted by this RFC's own citation, unread |
| `isSimplePassthroughOf` `rule_implement_simple_select.go:210` | `flowedType == nil` → `return true`, Map skipped | runs `qov.Typ.Equals(flowedType)` | medium — an identity Map kept/dropped flips. Its own doc warns this "inverts the moment a result value does" |
| `planRowRecordType` `rule_implement_nested_loop_join.go:2429` | falls through to nil | verifies leg layouts. **Accepts `*RecordType` or `RelationType`** where two other sites accept only bare `*RecordType` | medium — fixes the return shape: bare `*RecordType` |
| `rule_push_set_operation_through_fetch.go:403` | `resultType == values.UnknownType` pointer-identity fallback fires | may stop firing | medium — the one explicit `== UnknownType` branch |
| `fk_chain_cardinality.go:671` | declines the cardinality cap | cap fires on new shapes | medium — cost/plan-shape churn |
| `scan_range_execution_identity.go:455,:488` | feeds `typeField("result-type", …)` into an execution-identity hash | hash changes for every plan containing a projection | **expect golden churn here first** |

**RETRACTED — the two-walker paragraph was wrong in both halves, and a review round then
repeated one of them.** Rev 4 said `executor.go` and `rule_implement_unordered_union.go` both
descend PAST a projection via `GetInner()` and both need a "don't descend" arm added. Neither
does and neither needs one: `planColumnNamesWithMD` (`executor.go:2806`) and
`physicalPlanColumnNames` (`rule_implement_unordered_union.go:147`) each match
`*plans.RecordQueryProjectionPlan` as the FIRST arm of their loop and return its names, so the
`GetInner()` descent below is unreachable for a projection. The review that caught the first
half asserted the union walker has no arm; it has one — `grep -c RecordQueryProjectionPlan` over
that file returns 1, and it is that arm. There is no code to add here.

That makes both walkers **unaffected for a stronger reason than rev 4 gave**: not "they descend
past it" but "they never read `GetResultType()` for a projection at all". Pinned in both
packages (`TestPhysicalPlanColumnNames_StopsAtProjection`,
`TestPlanColumnNames_StopsAtProjection`) because two readings in a row got it backwards.

And the executor arm is INDEPENDENT CORROBORATION of the naming choice: it resolves a projected
column's name with `values.OutputColumnName`, which is the same function
`values.ProjectionResultValue` uses to name the fields of the row a projection now states. The
executor-visible name and the stated row's field name come from ONE authority by construction.

**The per-consumer census was quoted and never read for its numbers.** `unresolved_result_type_-
census_gate_test.go:66-71` records the whole-FDB-corpus reading per consumer, and it answers two
of the rows above outright:

```
bakedIntersectionKeys    resolved 412    UNRESOLVED 0
distinctKeyColumns       resolved 4      UNRESOLVED 31
planRowRecordType        resolved 620    UNRESOLVED 104
```

`UNRESOLVED 0` for `bakedIntersectionKeys` means no stub-typed leg reaches it, so the "high risk"
rating above was refuted by a citation two sections away in the same document.

**`distinctKeyColumns`' 31 unresolved reads are NOT wrappers over projections — MEASURED.** The
natural reading (and a review round's) is that they must be, since the site short-circuits on a
direct projection inner. Instrumenting the site and running the whole sqldriver FDB corpus says
otherwise:

```
31 ZZDKC unresolved inner=*plans.RecordQueryFlatMapPlan
 2 ZZDKC resolved   inner=*plans.RecordQueryScanPlan
 2 ZZDKC resolved   inner=*plans.RecordQueryPredicatesFilterPlan
 1 ZZDKC resolved   inner=*plans.RecordQueryIndexPlan
```

Every one is a `RecordQueryFlatMapPlan`, which is still a `GetResultType` stub and which this RFC
does not touch — so those 31 cannot have flipped, and the count is 31 before and after.

The wrapper-over-projection arm is real but **unreachable from SQL**: every `SELECT DISTINCT` the
SQL layer builds puts the projection immediately under the `Distinct`
(`Distinct(Project(Project(Limit(Scan))))`, `Distinct(Project(PredicatesFilter(Project(Scan))))`,
…), so no corpus can cover it and no plan pin can be written for it. It therefore gets a UNIT pin
that drives it directly — `cascades/distinct_key_columns_wrapper_test.go` — which goes red with
the physical projection change reverted (`fixture: the wrapper states *values.PrimitiveType, not
a RecordType`). An arm no corpus reaches is precisely the arm whose first firing would be read as
a finding rather than as untested code.

That pin also surfaced **CQ-101**: it could not use a `LIMIT` as the wrapper, because
`RecordQueryLimitPlan.GetResultType()` is still a flat `UnknownType` stub and swallows the row
its inner now states. Booked with the measurement, deliberately not fixed here — flipping a stub
needs its own role-differential, which is §5's own rule.

**Two censuses must be reconciled, not relaxed** (both files say so themselves):

- `unresolved_result_type_census.go` — its UNRESOLVED bucket shrinks by design.
  `MinSites`/`MinReads` are **collapse** guards, and its doc warns that when a consumer stops
  needing a result type, *"zero becomes that site's steady state and MinSites becomes
  unsatisfiable. Reconcile it with the new expected value then; do not relax it to whatever the
  run produced."* Per CLAUDE.md's shelf-life rule, the guard direction may **invert** — the
  alarm becomes growth — and the failure message must say which direction is now the alarm.
- `orientationGateFloors` (`sqldriver/embedded_fdb_test.go`) — the census watching the very gate
  §1c relaxes, and rev 5 did not list it. Its two DECIDING arms had **no bound in either
  direction**: `Matched` and `Declined` were unfloored and uncapped, so a gate that proved nothing
  satisfied every other check (the partition still adds up; `UnverifiableCeiling` cannot notice a
  drop). Both are now bounded — `MatchedFloor` (collapse = the comparison never runs) and
  `DeclinedCeiling` (growth = queries LOSE their plans, since this gate is what admits the
  materialized NLJ at all). Every arm is driven by
  `TestOrientationGateCensus_AssertionArmsGoRed`, not just by the corpus.

  **What §1c actually moves, isolated by mutation rather than inferred from drift:** current
  corpus `calls 506, unverifiable 104, matched 232, declined 68`; with the unstated-field arm of
  `recordFieldsMatch` removed and nothing else changed, `calls 504, unverifiable 104, matched 230,
  declined 68`. So the relaxation moves **two** firings, both into `Matched`, and **`Declined`
  does not move at all**. The `84 → 104` / `61 → 68` drift from the previously documented reading
  is ordinary corpus growth — worth saying plainly, because that drift is in exactly the
  direction that would otherwise look like this change's fingerprint.
- `legLocalBakeFloors` (`sqldriver/embedded_fdb_test.go:759`) — `UnderivableLegs` should reach 0
  *including* the new derived-table arm. That is the acceptance criterion, not a floor to move.

**`refineRowTypes` — decided, not flagged.** `refineRowTypes` (`quantifier.go:353`) treats an
empty `RecordName` as "not yet bound to a named struct" and refines *toward* a sibling's named
type, so a projection-built (anonymous) record adopts a sibling's struct name. Rev 1 called this
"probably correct", which is not a design. **Decision: correct and intended.** A projection's
output record is genuinely anonymous — SQL gives `SELECT a, b FROM t` no type name — so adopting
a co-member's name is not renaming a named row, it is binding one that never had a name; the
refinement direction (unstated loses to stated) is exactly right for it. Pinned by a test
asserting the projection co-member *adopts* rather than erases a sibling's `RecordName`.

---

## 6. Acceptance — what was MEASURED, and what rev 5 still owes

Rev 4 stated this section as predictions. Rev 5 restates it as results, and separates the two
populations rather than merging them, because a merged list is how an unbuilt item reads as a
green one.

### 6a. MEASURED — done, with the command and the outcome

**Headline, so the rest can be read as detail:**

```
$ bazelisk test //pkg/relational/... //pkg/docscheck:docscheck_test \
    //pkg/recordlayer/query/... --nocache_test_results
Executed 37 out of 37 tests: 37 tests pass.

$ bazelisk test //pkg/recordlayer/... --nocache_test_results
Executed 15 out of 15 tests: 15 tests pass.
```


1. **Both stranded queries return correct rows on real FDB**, with the EXISTS column asserted by
   value on every arm:

   ```
   $ bazelisk test //pkg/relational/sqldriver:sqldriver_test --nocache_test_results \
       --test_arg="--test.run=^TestFDB_ProjectionResultTypeProbe$" --test_arg="--test.v"
   === RUN   TestFDB_ProjectionResultTypeProbe
   --- PASS: TestFDB_ProjectionResultTypeProbe (0.11s)
   Executed 1 out of 1 test: 1 test passes.
   ```

   Seven arms: `cte_control`, `cte_exists`, `derived_control`, `derived_exists`,
   `table_exists_control`, `cte_where_exists`, `derived_where_exists`. The WHERE arms are the
   ones §1c's regression would have taken the plan away from, and the base-table arm is the twin
   that localized §1b.

2. **Nine plan shapes pinned in the fast harness**
   (`core/embedded/derived_source_exists_plan_test.go`), each asserting a `FlatMap` — so an arm
   cannot go green on a plan that answered the query without lowering the correlated EXISTS —
   and the joined arms additionally asserting the `NestedLoopJoin` whose merged row the
   correlated read addresses. Nine `--- PASS` subtests counted, not inferred from the parent.

3. **The three decisions pinned directly**
   (`cascades/projection_leg_ordinal_seed_test.go`), because a functional test cannot
   distinguish the intended route to a working plan from any other: a projection leg is
   ordinal-safe; it seeds a 4-slot concat with the projection's own columns first; a projection
   that cannot state its row is refused rather than guessed at; and the orientation gate ignores
   an unstated field type while still discriminating on names and on two stated types that
   disagree.

4. **Mutation check, three independent directions, each reverted alone.** This fix can be wrong
   in four ways and each one was made to fail on its own:

   | direction reverted | test | failure |
   |---|---|---|
   | projection arm in `legOrdinalSafety` | unit + FDB probe | `a projection leg must be ordinal-safe: refused at *plans.RecordQueryProjectionPlan` / `cte_exists: unexpected query error: *values.UnboundEvalContextError: correlated FieldValue "ID" (correlation "C") … (multi-leg row cannot serve a source-relative ordinal)` |
   | CTE fallback in `buildOuterScopeSources` | plan pins | `plan failed: *api.Error: 42703: no FROM source aliased as C` on all four CTE arms |
   | unstated-type arm in `recordFieldsMatch` | unit + plan pins | `a window whose field types were never inferred must still match a leg that states them` / `plan failed: best expression is not a physical plan: *expressions.LogicalProjectionExpression` on all three `where_exists` arms |
   | CTE-first ordering → catalog-first | shadowing plan pin | `plan failed: *api.Error: 42703: column "T1_ID" does not exist` |

   Each was restored and re-run green. The FDB-probe mutation is the one worth reading twice:
   reverting §1b's fix moved the *CTE* arm onto the same executor failure the derived arm had,
   which is the direct evidence that the two defects are layered — 1a gated the CTE query before
   it could reach 1b.

5. **The corpus plan-shape golden moved by 55 lines, ALL of them in the one yaml scenario this
   change edits.** That is the measurement that bounds the blast radius of §1c's relaxation:
   over 351 files and 2522 queries, loosening the orientation gate's type comparison changed no
   other plan. Re-blessed with the documented command and the diff is in the change, as that
   test's own failure message requires.

   Two pins in that suite were CHARACTERIZATION pins on the defects fixed here and both flipped,
   which is the outcome each had asked for in writing:
   `yamsql/testdata/projected_exists_over_a_derived_source.yaml` (`error_code: "42703"` → rows,
   plus its two WHERE forms, plus the derived arm its header said this runner could not host)
   and `sqldriver/quality_probes_test.go`'s `cte_with_exists` (`expectError` → `want [Alice
   Bob]`, the exact assertion its comment named). The second is not a duplicate of the first: its
   CTE is ALIASED (`FROM active a`), so the column schema comes from the WITH registry while the
   alias and correlation come from the reference.

   `FEATURE_MATRIX.md` and `SQL_COVERAGE.md` are regenerated with it — that scenario goes from
   3 cases (2 supported, 1 error-path) to 6 supported, which is the corpus recording the same
   flip in the one place that counts capability rather than plans.

6. **The fold-step1 seed census caught the new corpus and was updated by CONTROL, which is what
   that gate's own comment demands.** It is a hard EQUALITY, and it went
   `DECLINE rv-no-exist-ref == 216, want EXACTLY 212`. Two unfiltered uncached runs of
   `//pkg/relational/sqldriver:sqldriver_test` differing ONLY in whether the probe file is in the
   package: **file out → the target PASSES at the committed 604/182/212; file in → 614/188/216.**
   So the whole movement is that one file's, measured rather than reasoned.

   The SPLIT is the part worth reading, because it states the fix from the census's side:
   ACCEPT +6 is the three projected-EXISTS arms under both orientations, and no-exist-ref +4 is
   the two WHERE-EXISTS arms. Meanwhile `reconstruct-nil` holds at 102, **all bare-QOV, with the
   `rv=RecordConstructorValue (NOT a positional merge)` bucket at 0** — that bucket is exactly
   where a projection leg used to land, so the six firings that would have declined there are
   the six that now ACCEPT.

7. **An environmental finding, reported rather than worked around: `/home` hit 100% mid-run**
   (`No space left on device` from Bazel's BEP writer), which per this repo's own stress-test
   guidance degrades timing measurements and can fail actions for reasons unrelated to the
   change. 20 Bazel output bases pointed at workspaces that no longer exist (75G); reclaiming
   only those took the disk to 89% and the suite was re-run from there. No live output base was
   touched — the check is `DO_NOT_BUILD_HERE`'s recorded workspace path still existing.

8. **`//pkg/recordlayer/...` unfiltered, uncached: 15/15 pass** — the suite that owns the two
   changed cascades sites.

   `//pkg/docscheck` caught the supply-side change too, and correctly: its
   `GetResultType() == UnknownType` **stub inventory** is red in BOTH directions, and a stub
   disappearing fails precisely so a deliberate shrink cannot be confused with a renamed plan.
   `RecordQueryProjectionPlan` is removed from the list with the reason named, and the header's
   count goes twelve → eleven. Note where it was filed: **tier 2, "forward the inner"** — the
   same wrong tier RFC-213 assigned, recorded independently in a second instrument.

9. **`//pkg/recordlayer/query/... //pkg/relational/core/...` unfiltered, uncached: 24/24 pass.**

10. **1M stress, before/after, both worktrees on the SAME filesystem** (`/home`, checked at
    89% before starting — a baseline taken at the 100% of §6a.7 would have measured the disk).
    Baseline is a detached worktree at `aba271454`; both legs uncached, `--test.v`, 24 `=== RUN`
    lines each.

    **No regression. Totals 177.36s baseline → 177.52s current (+0.09%),** and all **23**
    subtest arms compared pairwise — every delta inside run-to-run noise, largest `+0.15s` on
    `full_scan_count` (3.02 → 3.17) against a ~3s scan. Point lookups unchanged at 0.01–0.06s,
    `order_by_pk_full` 3.42 → 3.51, `scan_all_wide` 3.37 → 3.38. Bulk load 6.27s/6.28s for the
    100k customers and the 1M orders load within a second of each other.

    That the arms are 23 on both sides matters as much as the times: the stress arms assert row
    counts, so a plan change that returned different rows would fail rather than merely slow
    down. Zero moved.
   The first run was RED, and correctly so: the embedded corpus's qualifier-recovery census
   declares a `existsSortSplit` split floor of 0 — *watched, not proven* — and the new plan pins
   drive that arm 6 times, all AGREED. That is the declaration going stale, not a defect. The
   floor is raised to 3 (below the measurement, per that block's own policy) and the prose that
   said "this package plans no sorted EXISTS fold over a join" is corrected, since it was a fact
   about the corpus and the corpus changed.

### 6b. NOT IMPLEMENTED in rev 5 — carried, not claimed

Rev 4's §4.4 decisions (c) are **not in this change**, and rev 5 does not pretend otherwise.
What is implemented is the supply side — both nodes state their row, `GetResultType` derives
from `GetResultValue`, `RecordConstructorValue.Type()` normalizes names — plus the three
consumer/scope fixes of §1. The following are owed and should be re-reviewed on their own
merits now that §1's story has changed:

10. **Delete `rule_remove_projection.go` and `rule_projection_elim.go`, `IsIdentity()`, and
   `fk_chain_cardinality.go:290`'s fast path**, re-founded on unsoundness (§4.4′(2)) rather than
   on reachability. **The mutation direction for this becomes: restore either deleted rule and
   observe a WRONG ROW** — not a `MemberResultTypeDisagreementError`, which is the shape rev 4
   assumed. The rule rewrites a 1-slot row into an N-field row, so the observable is the row,
   and a pin that asserts the row is the only one that cannot be satisfied by a coincidence.

11. **The yield invariant as the stated property** (§4.4′(3)) rather than "unbuildability", which
   the seven plain-struct-fill constructors do not deliver. Its acceptance is an inspection over
   the six-site population plus the corpus census, and its open edge is `memo.Integrate`'s
   group-merging path (§4.4′(4)), which no lap has read and which the invariant does not
   quantify over.

12. **The CQ-63 gate arm for the derived-table query in `sqldriver`.** `UnderivableLegs = 2 → 0`
   was measured on the rev-4 branch and leg D now states `RECORD<ID LONG NULL, V LONG NULL>`;
   what is *not* yet done is landing that arm in the suite with the `LegDerivations` /
   `SiteExists` / `MergeSlots` population floors holding on the same unfiltered run. A filtered
   run drops those floors by design and its zero proves nothing.

13. **`3b` restated, since rev 4's wording is unsatisfiable as an assertion.** "Returns a row
   type" is not assertable — `(nil, nil)` is documented-reachable at `quantifier.go:302` — so
   the pin asserts **arity and field names** of the stated row. That is the property that
   distinguishes a projection stating its columns from one wrapping its inner, and it is the
   property §1b's seed depends on.

14. *(was: the 1M stress comparison — now DONE and moved to §6a.10.)*

15. **§6.9 of rev 4 is retired as stale.** It demanded the probe become an asserting test
    rather than `t.Logf`-only. It already asserts — every arm goes through `requireRows` /
    `requireRows3`, which `t.Fatalf` on an unexpected error and compare rows element-wise. There
    is nothing left to do and the item was describing an earlier revision of the file.

---

## 7. The normalization point — answered, not left open

Rev 1 left "should Go normalize centrally?" as an open question. That was a punt, and it was
also the thing forcing Decision §4.3 to be a per-site discipline. **Decision §4.6 answers it:
`RecordConstructorValue.Type()` (`values.go:4596`) is the single normalization point**, matching
Java's `Type.Record.fromFields` → `normalizeFields` (`Type.java:2532`, `:2617-2682`). Naming is
an invariant of the record type, not a duty of each of its constructors.

Two findings from the sweep that motivated it.

**A third copy of this derivation already exists in the tree**, open-coded:

```go
// rule_push_requested_ordering_through_projection.go:62-78
projValues := proj.GetProjectedValues()
aliases := proj.GetAliases()
...
    if name == "" {
        name = values.ExplainValue(v)        // <-- a NAME minted from a RENDERING
    }
    fields[i] = values.RecordConstructorField{Name: strings.ToUpper(name), Value: v}
resultValue := values.NewRecordConstructorValue(fields...)
```

That is exactly the derivation Decision §4 puts on the node, written a third time, and its
unnamed-slot fallback mints a column name from `ExplainValue` — a *rendering* — where Java uses
`"_" + i`. Deriving identity-bearing names from renderings is the anti-pattern RFC-215 and
RFC-218 exist to remove. **The shared builder replaces this copy; it is not left beside it**, or
this change ships the dual mechanism Decision §4.5 exists to prevent.

Five files both read a projection's columns and build record constructors:

```
$ grep -rln "GetProjectedValues()\|GetProjections()" --include='*.go' pkg/ | grep -v '_test.go' \
    | while read f; do grep -q "RecordConstructorField\|NewRecordConstructorValue" "$f" && echo "$f"; done
pkg/recordlayer/query/plan/cascades/rule_push_requested_ordering_through_projection.go
pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go
pkg/recordlayer/query/plan/cascades/rule_implement_unordered_union.go
pkg/relational/core/embedded/cascades_generator.go
pkg/relational/core/query/cascades_translator.go
```

Each is inspected during implementation and either routed through the shared builder or shown to
be building something else. **The empty-name sweep itself is answered by construction rather than
by grep**: a line-scoped grep cannot see multi-line composite literals (rev 1 attempted one and
it was inconclusive — 14 "hits" that were all `Name:` on the following line), so the honest
instrument is a runtime assertion in `RecordConstructorValue.Type()`'s normalization plus a unit
pin that every projection-stated `RecordType` has non-empty names in every slot. That is
acceptance item 4.
