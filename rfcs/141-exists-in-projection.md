# RFC-141 — EXISTS in the projection list (Java 4.12, RFC-135 §4 R4)

**Status:** Implemented (v3 — Phase 1 + Phase 2 shipped in `f16a0802a`, PR #336; Phase 2 re-architected to
Java's FirstOrDefault-inner + pure-map FlatMap + separate filter after Graefe's design NAK on a Go-only
`existsProjectMode`; codex hardening rounds
1–11, see §8a–§8h)
**Item:** RFC-135 §4 **R4** — port Java 4.12's "EXISTS subqueries in the projection list" (commit
`c9274172c` #4168). Today Go accepts `EXISTS(subquery)` only in `WHERE`; 4.12 lets it appear as a
SELECT-list value (`SELECT id, EXISTS(SELECT 1 FROM child WHERE …) AS has_child FROM parent`).
**Reviewers:** **Graefe** + Torvalds (Cascades values/predicates + planning).

---

## 1. Problem (verified real)

Java 4.12 `c9274172c` refactored `ExistsValue` from a non-evaluable quantifier wrapper into a proper
**evaluable** `Value`, so EXISTS can be a projected column, not just a filter. The 4.11→4.12 diff on
`ExistsValue.java`:

```
- implements BooleanValue, QuantifiedValue, Value.NonEvaluableValue   // 4.11: holds an alias, can't eval
+ implements BooleanValue, ValueWithChild                              // 4.12: holds a child Value, evals
+ public Object eval(store, context) { return getChild().eval(store, context) != null; }
+ public Value getChild() { return value; }   // value = QuantifiedObjectValue.of(existential quantifier)
+ public ValueWithChild withNewChild(rebasedChild) { … }
+ proto: setValue(getChild().toValueProto(...))   // PExistsValue.value (field 3), child = QuantifiedObjectValue
```

Go's `ExistsValue` (`pkg/recordlayer/query/plan/cascades/values/value_exists.go`) still stores only
`Alias CorrelationIdentifier` and panics on `Evaluate()`; the SQL walk
(`pkg/relational/core/query/expr/walk.go:90`) turns `EXISTS` into an `ExistsPredicate` wrapped as a
predicate value — fine for `WHERE`, but there is no evaluable Value to place in a projection. So
`SELECT EXISTS(…)` is unsupported. The 4.12 proto (`PExistsValue.value`, `PExistentialValuePredicate`)
is already synced (the bump; pinned by `plan_proto_schema_test.go`). **Java-parity gap, not a Go
extension; not wire-format** (projections are planner-computed, not persisted).

## 2. Investigation (Java mechanism)

- **`ExpressionVisitor.visitExistsExpressionAtom`** (one visitor for both WHERE and SELECT): builds an
  *existential* quantifier over the subquery's select operator and returns `Expression.ofUnnamed(new
  ExistsValue(QuantifiedObjectValue.of(existentialQuantifier)))` — i.e. always an `ExistsValue`, whose
  child projects the existential quantifier's object.
- **WHERE** now wraps that value in an **`ExistentialValuePredicate`** (replacing the standalone
  `ExistsPredicate`); **SELECT** uses the `ExistsValue` directly as the column value.
- **Execution:** the existential quantifier lowers to a `FlatMap`/`exists()` over the correlated subplan
  — for each outer row, the subplan runs and `eval` yields `true`/`false` (child object non-null ⇒ at
  least one row). Correlated and non-correlated both fall out of the existential-quantifier mechanism.

## 3. Fix — ONE mechanism, phased along the representation seam (Graefe + Torvalds)

Java has **one** representation of EXISTS: an `ExistsValue` whose child is a `QuantifiedObjectValue` over
an existential quantifier. WHERE is *not* a separate concept — it is that value funnelled through
`ExistsValue.toQueryPredicate() → ExistentialValuePredicate(child, NullComparison(NOT_NULL))`. The
standalone `ExistsPredicate` was **deleted** in 4.12. Go must collapse to the same single mechanism;
keeping Go's leaf-alias `ExistsPredicate` alongside the value form is the dual-mechanism /
special-case anti-pattern (principle #10) and is **not** optional.

### Phase 1 — the representation refactor (one commit; WHERE-EXISTS only; no new user surface)

1. **`ExistsValue` → `ValueWithChild`** (`value_exists.go`): replace `Alias CorrelationIdentifier` with a
   child `Value` (the `QuantifiedObjectValue` of the existential quantifier). `Children() = [child]`,
   `GetChild()/WithNewChild()`, and `Evaluate(store, ctx) = child.Evaluate(store, ctx) != nil` (boolean,
   matching Java's `eval`). Keep `BooleanValue`. This makes `ExistsValue` a **transparent composite**;
   the 11 sites that special-cased the leaf alias now **delegate to the child**:

   | Site | Today (leaf/alias) | After (delegate to child) |
   |---|---|---|
   | `value_exists.go` struct/`Children()`/`GetCorrelatedTo`:54 | `Alias`; `Children()=[]`; reads `v.Alias` | child `Value`; `Children()=[child]`; `GetCorrelatedToOfValue(child)` |
   | `rebase.go:44-48` | new `{Alias: newAlias}` | rebase the child: `{Value: Rebase(child)}` |
   | `value_correlation.go:34` | adds `q.Alias` | drop the case — child's own walk covers it |
   | `semantic_equals.go:38-40` | compares `Alias` via map | drop the case — structural children recursion |
   | `semantic_hash.go:48` | "exists" tag, no child | drop the case — default structural hash folds child |
   | `map_field_values.go:322` | `EqualsWithoutChildren` on `Alias` | compare on `Value` (or empty — children compared separately) |
   | `functional_dependency.go:32` | reads `n.Alias` | read the child `QuantifiedObjectValue`'s alias |
   | `rule_decorrelate_values.go:415` | translate `n.Alias` | translate the child: `{Value: translate(child)}` |

2. **Port `ExistentialValuePredicate`** (`predicates/`): a `ValuePredicate` subtype wrapping
   `(value=QuantifiedObjectValue, comparison=NullComparison(NOT_NULL))` — "the existential quantifier's
   object is non-null ⇒ ≥1 row". Constructor verifies the value is a `QuantifiedObjectValue`. Proto
   `PExistentialValuePredicate` (a `PValuePredicate super`) already synced.
3. **`ExistsValue.toQueryPredicate()`** → `ExistentialValuePredicate(child, NOT_NULL)` (Java's bridge).
4. **Rewrite the WHERE join-rule call sites** `rule_implement_nested_loop_join.go:280` and `:436`: detect
   the existential semi-join via the value-predicate shape (a `QuantifiedObjectValue`-over-existential-
   alias with a `NOT_NULL` comparison) instead of `type-switch on *ExistsPredicate`; negation comes from
   the surrounding `NotPredicate`/compensation, exactly as Java. `walk.go:1716 walkExistsPredicate` now
   builds the `ExistentialValuePredicate` (via the `ExistsValue` + `toQueryPredicate`).
5. **Delete `ExistsPredicate`** (`exists_predicate.go`) — fully subsumed. **Regression bar:** the entire
   correlated/non-correlated WHERE-EXISTS + NOT-EXISTS suite stays green *after* the swap (not by leaving
   the old path), plus the 10× determinism loop (alias-namespace bugs surface as nondeterminism).

### Phase 2 — projection wiring (the new user surface)

6. **Projection** (`walk.go` + select-element lowering): the **same** `ExistsValue` is produced
   unconditionally; the *consumer* decides — a predicate position calls `toQueryPredicate()` (Phase 1), a
   SELECT-element position uses the `ExistsValue` directly as the column value. No `isProjection` flag on
   the value or walk — the split is structural (where the walk is invoked), mirroring Java's
   single-visitor / two-consumer design (Graefe Q5).
7. **Logical-builder registration (the hidden blocker).** Today only WHERE-EXISTS subqueries are
   collected into the existential-subquery list that the cascades translator reads to attach the
   `NamedExistentialQuantifier` to the `SelectExpression` (`logical_predicate.go` `BuildExists` /
   `cascades_translator.go:736-771`). A *projected* `ExistsValue` carries an existential alias but is
   never registered, so its quantifier never attaches. Phase 2 must collect projected-EXISTS subqueries
   into the same list (walk the projected `RecordConstructorValue` for `ExistsValue` children after
   `upgradeLogicalSelect`, register each alias), so the existing translator attaches the quantifier
   identically to the WHERE case.
8. **Execution — re-architect to Java's emergent structure, NO new cursor mode** (Graefe design NAK on
   the earlier `existsProjectMode`). Java's `RecordQueryFlatMapPlan.executePlan` is a **pure `.map()`** —
   no exists/notExists/leftOuter branch. The semi-join behaviour is **emergent from what wraps the
   inner**: `ImplementNestedLoopJoinRule` wraps an **existential** inner in
   `RecordQueryFirstOrDefaultPlan(inner, NullValue)`, so the inner yields **exactly one row** (first real
   inner row, or NULL on empty). The map then emits exactly one outer row; `ExistsValue.eval = child != null`
   reads it (bound ⇒ true, NULL ⇒ false). **Filtering (WHERE) is a *separate* `PredicatesFilterPlan`** that
   carries the `ExistentialValuePredicate` *on top* and drops `false` rows. Therefore:
   - **WHERE-EXISTS** = FirstOrDefault-inner map **+** a predicates-filter on top.
   - **SELECT-EXISTS** = the **same** FirstOrDefault-inner map, **no** filter — the boolean is projected by
     the map's `resultValue`. Zero new execution code.
   - **EXISTS in both** = the same map + the filter; the boolean is projected by the map regardless (the
     filter only drops survivors), so projection is never tied to a mode (Graefe Q3 dissolves).

   Go currently **diverges**: it folds the semi-join *filter* into the cursors — `flatMapCursor`
   `existsMode`/`notExistsMode` (`flat_map_cursor.go:99/109`) and `nljCursor` `JoinExists/JoinNotExists`
   return the bare `qualifyOuterRow` and structurally **cannot** project the boolean. Phase 2 repairs this
   to match Java:
   - wrap the **existential inner** in `RecordQueryFirstOrDefaultPlan(inner, NullValue)` and **use it as
     the join inner** (`physical_default_on_empty_wrapper.go` / `NewRecordQueryFirstOrDefaultPlan` exist).
     Today the FOD wrapper is *built* at `rule_implement_nested_loop_join.go:318` but **bypassed** — the
     live join (`:347`) reads the **raw** `innerPlan` while the FOD `fodQuant` rides as a dead right
     quantifier (`:355`); and the `:345` comment block (asserting FOD must *not* wrap the inner) is the
     exact inverse of this design and must be deleted;
   - make the FlatMap/NLJ cursors **pure maps** (`computeResult` always; remove the `existsMode`/
     `notExistsMode`/`JoinExists`/`JoinNotExists` short-circuits);
   - move WHERE-EXISTS filtering to a `PredicatesFilterPlan` carrying the `ExistentialValuePredicate`.

   After this, projected-EXISTS needs **no** new execution code — it is the pure map with no filter on
   top. This is a larger re-architecture than "wire a projection", but it is the faithful collapse Phase 1
   promised; the WHERE-EXISTS + NOT-EXISTS suite is the behaviour-preserving regression bar (+ 10×
   determinism), and the EXISTS plans' `EXPLAIN` shape changes from a fused exists-join to
   `Filter(Exists) over FlatMap(FirstOrDefault(inner))` — matching Java.

## 4. Performance

No hot-path change for non-EXISTS queries. A projected EXISTS costs the same correlated-subquery
execution as the equivalent WHERE-EXISTS (one existential probe per outer row); the planner may
short-circuit the existential probe (first row wins), as WHERE-EXISTS does.

## 5. Wire / behaviour impact

None on persisted bytes. New read-side capability (`SELECT EXISTS(…)`) that matches Java 4.12 exactly;
where both engines run the query they agree (conformance). `NOT EXISTS` in a projection follows as the
negation of the boolean value.

## 6. Test plan

**Phase 1 (refactor):** the existing correlated/non-correlated WHERE-EXISTS **and** NOT-EXISTS suite must
stay green *after* `ExistsValue` becomes `ValueWithChild`, `ExistentialValuePredicate` replaces
`ExistsPredicate`, and the join-rule sites are rewritten — proving the representation swap is
behaviour-preserving (the regression bar; Graefe). Plus a 10× determinism loop on a representative
correlated-EXISTS plan (alias-namespace bugs surface as nondeterminism — query-engine skill). No new
SQL surface in this commit, so a bisect lands on the refactor alone.

**Phase 2 (projection):** port Java's `exists-in-select.yamsql` scenarios as yamsql/FDB tests —
non-correlated `SELECT EXISTS(SELECT 1 FROM t2)`; correlated `SELECT p.id, EXISTS(SELECT 1 FROM c WHERE
c.pid = p.id) FROM p`; `NOT EXISTS` in projection; EXISTS in projection *and* WHERE together; multi-join
subquery. Each asserts both rows **and** an `EXPLAIN` plan-shape (the existential `JoinExists`/FlatMap
fires, not a fallback), plus the determinism loop on the new plans.

## 7. Decisions (resolved with Graefe + Torvalds)

1. **One mechanism, `ExistentialValuePredicate` mandatory, `ExistsPredicate` deleted** (Graefe Q1).
   Keeping the leaf-alias predicate alongside the value form is the dual-mechanism anti-pattern; Java
   collapsed to the single `ExistsValue`-over-`QuantifiedObjectValue` form and Go must too. The two
   `rule_implement_nested_loop_join` call sites are ported to the value-predicate shape.
2. **Phase along the representation seam, not core-vs-advanced** (Graefe Q2): Phase 1 = the atomic
   value/predicate refactor (WHERE-EXISTS only, full suite green, isolated for bisect); Phase 2 =
   projection wiring + the yamsql scenarios. Landing the refactor atomically with new SELECT syntax is
   how a latent WHERE regression hides behind green projection tests.
3. **Context split is structural, not a flag** (Graefe Q5): one `ExistsValue` produced unconditionally;
   the consumer (WHERE → `toQueryPredicate`; SELECT → direct value) decides. No `isProjection` bool.

## 8. Phase 2 fold over ORDER BY / LIMIT and alongside a scalar subquery

The projected-EXISTS fold pushes the SELECT-list `RecordConstructor` (which carries the `ExistsValue`)
INTO the existential `SelectExpression`'s result value, so the boolean is computed by the FlatMap with
the inner existential binding live. Two cases must thread through that fold faithfully:

1. **Intervening ORDER BY / LIMIT.** The logical builder emits `Project(Sort(Filter))` for an ORDER BY
   (LIMIT is hoisted above the project). The fold therefore must NOT require the existential filter to be
   the project's *direct* input — `findExistsFilterUnderUnaryChain` descends through the intervening
   `Sort`/`Limit` (only those two are transparent; a `Project`/`Join`/`Aggregate` between changes the row
   shape and is not folded through), folds the projection into the `SelectExpression`, and re-applies the
   sort/limit ON TOP — `generateSort(generateSimpleSelect(output...), orderBys)` in Java's
   `LogicalOperator.generateSelect`. The sort keys rebase onto the projected output record (Java's
   `OrderByExpression.pullUp` onto the select's result value).

   When an ORDER BY key is **not** in the SELECT output (`SELECT id, EXISTS(...) FROM t1 ORDER BY col1`),
   this ports Java's `remainingOrderByExpressions` branch: append the missing sort column(s) to the folded
   projection, sort on the extended record, then wrap a final projection that selects only the original
   output (drops the sort columns — Java's pull-up). Without this the sort key would not resolve against
   the projected record and the sort would silently no-op. Narrowing: a sort key that is a *computed*
   expression (not a bare/qualified column name) is not appendable by name and is left unfolded — the same
   tolerance Java applies to order-by expressions it cannot pull up.

2. **A scalar subquery alongside the EXISTS.** `SELECT id, EXISTS(...), (SELECT MAX(id) FROM t2) FROM t1`
   mixes a projected EXISTS with an uncorrelated scalar subquery. The scalar subquery is pre-evaluated and
   bound by alias by the executor; its collection (`t.scalarSubqueries`) MUST run for every projection,
   including the fold path. The fold's collection therefore runs *before* the early return — skipping it
   (the original bug) left the scalar column unbound → NULL. A scalar subquery in the SELECT list is a Go
   read-side extension (Java's `fdb-relational` grammar cannot parse one there), so there is no Java plan
   shape to match; the contract is correctness with zero wire impact (the scalar is pre-evaluated, never
   stored).

## 8a. Round-3: the safety guard + three more fold shapes

The fold structurally pattern-matches plan shapes. Round-2 codex review found that any shape the matcher
does **not** recognize silently falls through to a plan where the projected `ExistsValue` is evaluated
**above** the FlatMap — its existential binding gone, so `ExistsValue.Evaluate` returns a constant
`false` (silent wrong result). The fix is a two-layer **safety guard** that bounds the entire long tail,
plus three specific common shapes.

### The safety guard (mechanism)

A projected `ExistsValue` is correct **only** when it lives in the `resultValue` of the
`SelectExpression` that owns its existential quantifier — i.e. it is evaluated by the FlatMap with the
inner binding live. Every path is now forced to EITHER fold the projection there OR reject cleanly with
`ErrCodeUnsupportedQuery` (message `"projected EXISTS in this query shape is not yet supported"`). No path
silently ships a wrong result. The guard has two layers because an unfoldable shape can lose the
`ExistsValue` at *different* stages:

- **Post-translation structural guard** (`query.CheckProjectedExistsFolded`, run after
  `TranslateToCascadesWithSubqueries`, before planning). Two passes over the Reference tree: (A) map each
  existential-quantifier alias → the `SelectExpression` that declares it; (B) require every `ExistsValue`
  found in any expression's *own-scope* values (`SelectExpression.resultValue`,
  `LogicalProjectionExpression.projectedValues`, `GroupByExpression` grouping keys + aggregate operands,
  `LogicalSortExpression` sort keys) to be emitted by exactly that owner. An `ExistsValue` whose owner is
  a *different* expression — or whose alias no `SelectExpression` owns — would evaluate with a dead
  binding ⇒ reject. This catches any future fold gap where the `ExistsValue` survives translation but
  lands in the wrong place. `ExistsValue.Evaluate` returns `false` (never errors/panics) on an unbound
  binding, which is exactly why a runtime check is impossible and a structural pre-planning guard is the
  correct mechanism.
- **Logical-level guard** (`findUnfoldableProjectedExists` + `validateGroupByProjection`'s structural
  EXISTS check, run before translation). Some shapes drop the `ExistsValue` *during* logical building, so
  the post-translation guard can't see it: a `GROUP BY` over an EXISTS expression resolves the existential
  in the aggregate path (which has no `SubqueryPlanner`) to a nil Value, and an `Aggregate`/`Distinct`/
  `Union`/second `Project` between the projection and the existential filter is not fold-reachable. These
  are rejected at the logical level, mirroring `findExistsFilterUnderUnaryChain`'s transparency set
  (`Sort`/`Limit` only). EXISTS detection is structural — `expr.ContainsExistsAtom` walks typed ANTLR
  nodes, no `GetText`/text matching.

### The three round-3 fixes (common shapes that now fold)

1. **Projected EXISTS + JOIN in FROM, no WHERE** (`SELECT t1.id, t2.id, EXISTS(...) FROM t1 JOIN t2 ON …`).
   Root cause: `attachOrSynthesizeExistsFilter` wrapped the synthesized existential filter *above* the
   whole `Project(Join)`, so the projection (with the `ExistsValue`) ran before the FlatMap → constant
   `false` + leaked inner columns. Fix: the synthesized filter is now placed *under* the projection (above
   the join); `buildExistentialSelect` routes a join input to `buildExistentialJoinSelect`, which flattens
   the join's two `ForEach` quantifiers + the existential into **one** `SelectExpression` with the
   projection as result value (the same 2+1 flatten `translateJoinWithExists` does for WHERE-EXISTS, but
   emitting the folded projection). `ImplementNestedLoopJoinRule.implementJoinWithExistential` then, when
   the result value references the existential quantifier (a projected EXISTS), uses the **rebased
   projection** as the FlatMap's result value — leg columns rebase onto the merged-outer-row qualified
   keys, the existential `QuantifiedObjectValue` onto the inner FOD binding — instead of the bare
   merged-row identity. Outer-join FROM with a projected EXISTS is not folded; the guard rejects it.

2. **ORDER BY referencing the EXISTS SELECT-list alias** (`SELECT id, EXISTS(...) AS has_t2 FROM t1 ORDER
   BY has_t2 DESC`). Root cause: `upgradeSortKeyValues` copies the projected expression into the sort
   key's `Value`, so the key was the raw `ExistsValue`, re-applied above the FlatMap → `false` for every
   row → no real ordering. Fix: `pullUpSortKeyValue` (in `applySortOverRef`) pulls a sort key whose Value
   matches a folded output field up to a `FieldValue` over that output column — Java's
   `OrderByExpression.pullUp` onto the lower select's `getResultValue()` — so the sort orders by the
   already-materialized boolean column.

3. **Parenthesized `NOT (EXISTS(...))` in a projection.** Root cause: the NOT child is a
   `PredicatedExpression` over a paren-wrap `RecordConstructor`, not a direct `ExistsExpressionAtom`, so
   `existsAtomOf` returned nil → fell to the predicate path → NULL column. Fix: `existsAtomOf` /
   `existsAtomInExpressionAtom` unwrap the single-element unnamed paren-wrap `RecordConstructor` (and
   recurse for nested parens) to find the `ExistsExpressionAtom` under NOT; a bare projected
   `(EXISTS(...))` folds via the same unwrap in the `PredicatedExpression` projection branch.

### Supported vs cleanly-rejected projected-EXISTS shapes

**Supported (fold correctly):**
- `SELECT id, EXISTS(corr/non-corr) [AS x] FROM t` — projected EXISTS / NOT EXISTS, correlated or not,
  empty-subquery (FALSE), join-subquery (EXISTS over a join *inside* the subquery).
- `… ORDER BY id` / `… ORDER BY <exists-alias>` / `… ORDER BY <col-not-in-select>` /
  `… ORDER BY <qualified col>` / `… LIMIT n` — Sort and Limit are transparent; the EXISTS-alias key pulls up
  to the output column. A qualified key's table qualifier is **source-aware** (round-5 fix #2; see §8c): for a
  **single-table** source it is stripped to the bare output column (the merged row carries bare keys); for a
  **JOIN** source it is **kept qualified** (`t2.sk`→`T2.SK`) and resolves the authoritative qualified
  merged-row key — never the last-leg-wins bare key (the wrong leg). Selected and non-selected qualified keys,
  both legs, both directions, work.
- `SELECT id, EXISTS(...), (SELECT MAX(id) FROM t2) FROM t` — projected EXISTS alongside an **uncorrelated**
  scalar subquery (a CORRELATED scalar subquery alongside a projected EXISTS is cleanly rejected — §8b).
- `SELECT t1.id, t2.id, EXISTS(...) FROM t1 JOIN t2 ON … [ORDER BY <qualified leg col>]` (INNER join in FROM,
  no/with WHERE; qualified ORDER BY on either leg, selected or not — round-5 §8c).
- `SELECT id, NOT (EXISTS(...)) [AS x] FROM t` and `NOT ((EXISTS(...)))` — parenthesized/nested NOT.
- PK / secondary-index fast-path correlated probes.

**Cleanly rejected (`ErrCodeUnsupportedQuery`, never wrong rows):**
- **Multiple** projected EXISTS in one SELECT, or EXISTS in WHERE *and* SELECT — the multi-existential
  boundary (>1 existential quantifier; needs nested FlatMaps with intermediate record-bundling, never
  supported in the Go port).
- Projected EXISTS with a **GROUP BY / aggregate**, **DISTINCT**, or **UNION** between the projection and
  the existential filter — not fold-reachable; the aggregate path has no `SubqueryPlanner`.
- **GROUP BY on an EXISTS expression** (the EXISTS column as a grouping key).
- **Outer-join** (LEFT/RIGHT/FULL) in FROM with a projected EXISTS — the semi-join flatten cannot carry
  the NULL-padded drain.
- **Projected EXISTS + a CORRELATED scalar subquery** in the same SELECT (added round-4; see §8b).

## 8b. Round-4: the fold-bypass audit (no silent fall-through)

The fold's early return in `translateProject` (`if sel := translateProjectOverExistsFilter(...); sel != nil
{ return sel }`) skips every projection-processing branch BELOW it. Round-4 codex review found that two of
those skipped branches were silently-wrong on SUPPORTED shapes. The systematic fix audited **every** step
that runs after the early-return point and ensured each is either (a) run in the folded path, or (b) the
query is cleanly rejected — never silent fall-through.

### Bypass audit (every step after the fold's early-return)

| step / `LogicalProject` field | runs in folded path? | handling |
|---|---|---|
| `ScalarSubqueries` (uncorrelated) collection | **yes** — runs *before* the early return (`translateProject` top); pre-evaluated + bound by alias | supported (§8 case 2) |
| `Projections` / `Aliases` | yes — iterated into the folded `RecordConstructor` fields | supported |
| `ProjectedValues` | yes — used directly as the field Values | supported |
| `IsComputed` | yes — fold bails to the text-fallback on an unresolved computed slot, same as the ordinary path | supported / clean fallback |
| `AggregateSlots` | n/a in either path — only read by the upstream INSERT…SELECT promotion guard; projected EXISTS + aggregate is rejected by `findUnfoldableProjectedExists` before translation | rejected upstream |
| ORDER BY / LIMIT re-application | yes — the fold re-applies the unary chain on top, with EXISTS-aware **and qualified-aware** sort-key pull-up (round-4 fix #2) | supported |
| **`CorrelatedScalarSubqueries`** dispatch (`translateProjectWithCorrelatedScalar`) | **no** — the early return bypassed it → the correlated `ScalarSubqueryValue` stayed unbound → NULL column | **round-4 fix #1: cleanly rejected** |

### The two round-4 fixes

1. **Projected EXISTS + a CORRELATED scalar subquery → clean reject.** `SELECT id, EXISTS(...), (SELECT v
   FROM t2 WHERE t2.fk = t1.id) FROM t1` populates both `proj.ExistsSubqueries` *and*
   `proj.CorrelatedScalarSubqueries` (both methods on the same subquery planner, both invoked during the
   projection-value walk). The fold's early return fired before the correlated-scalar dispatch, so the
   correlated `ScalarSubqueryValue` was never rewritten to a join-leg `FieldValue` and read `NULL` every
   row. Threading the correlated scalar through the fold is genuinely incompatible: the projected-EXISTS
   fold builds an existential `SelectExpression` whose result value is the projection `RecordConstructor`
   evaluated by the FlatMap, while the correlated-scalar path builds a *different* structure — a LEFT-OUTER
   join `SelectExpression` anchored on the outer row with `NewScalarSubqueryAnchoredRecord` and its own
   per-row LIMIT-peel. Composing both is a 3-way quantifier nest the NLJ rule does not implement (the
   multi-quantifier boundary the port already rejects). Per "correctness over coverage", the shape is
   **cleanly rejected** — at the logical guard (`findUnfoldableProjectedExists`: a projected-EXISTS
   `LogicalProject` carrying `CorrelatedScalarSubqueries`) and defense-in-depth in `translateProject`
   (`len(p.CorrelatedScalarSubqueries) > 0` before the fold → nil). UNCORRELATED scalar + projected EXISTS
   still works (it is pre-evaluated and collected *before* the early return), so the rejection is narrow.

2. **Qualified ORDER BY key.** `SELECT id, EXISTS(...) AS has_t2 FROM t1 ORDER BY t1.col1 DESC` — the
   appended (remainingOrderBy) / pulled-up sort key was a flat `FieldValue "T1.COL1"`, but the folded output
   record exposes the column under its **bare** name (the projection names columns bare/by-alias, and the
   outer scan row flows columns under bare keys). The qualified key resolved to `NULL` every row → the sort
   silently no-oped → `DESC` fell to scan order. Fix: `sortKeyColumnName` + a new `stripSortQualifier` strip
   the single table qualifier so the appended remainingOrderBy column is bare (and its value resolves against
   the outer scan row), and `pullUpSortKeyValue` rebases a qualified `FieldValue` key onto the bare output
   column — **but only when a bare output field matches**, so a JOIN-in-FROM projected EXISTS (whose output
   columns ARE qualified, `T1.ID`/`T2.ID`) keeps its qualified key and resolves against the qualified output
   field. This is the qualified analog of the round-3 non-qualified non-selected-column fix; `ORDER BY
   t1.col1` and `ORDER BY col1` now fold identically.

Regression pins: `projected_exists_round4_fdb_test.go` — qualified ORDER BY (non-selected `t1.col1` DESC,
selected `t1.id` DESC, ASC control) asserting real ordering (a no-op sort visibly fails on DESC); the
correlated-scalar clean-reject guard sentinel (asserts the exact unsupported message, fails if any rows with
a NULL scalar return); and an uncorrelated-scalar still-works control (proves the rejection is narrow).

## 8c. Round-5: WHERE-EXISTS column leak + the join ORDER BY wrong-leg regression

Round-5 codex review found two silent-wrong bugs: one in WHERE-EXISTS column metadata, one introduced by the
round-4 qualified-ORDER-BY fix when the FROM source is a JOIN.

### Fix #1 — `SELECT * … WHERE EXISTS(…)` reported the inner subquery's columns

The RFC-141 re-architecture plans a plain `WHERE EXISTS` / `WHERE NOT EXISTS` as an **IDENTITY** FlatMap: its
result value is the **outer row's `QuantifiedObjectValue`** (the existential level only filters; the row that
flows out is the outer row unchanged), with the semi-join boolean dropped by a `PredicatesFilter` on top. The
cursor emits ONLY the outer row, so the reported columns are EXACTLY the outer plan's columns.

`deriveColumnsFromFlatMap` (`embedded/cascades_generator.go`) special-cased the **projected-EXISTS**
RecordConstructor result value, then fell through to merging the outer+inner table columns — so a plain
WHERE-EXISTS (whose result value is the bare outer QOV, not a RecordConstructor) advertised t1's columns AND
the inner t2's, even though only t1's row is returned (a metadata leak; a `SELECT *` scan into the wrong arity
mis-binds). The deleted semi-join column path used to return outer columns only. Fix: detect the
identity-over-outer shape — the result value is a `QuantifiedObjectValue` whose correlation equals the
FlatMap's `GetOuterAlias()` — and return ONLY the outer plan's columns.

### Fix #2 — qualified ORDER BY over a JOIN sorted by the wrong leg

The round-4 fix stripped `t2.id`→bare `ID` for non-selected qualified sort keys. For a **JOIN** source the
FlatMap merged outer row carries columns under BOTH a BARE key (last-leg-wins — the wrong leg) AND the
authoritative QUALIFIED `LEG.COL` key (`mergeRows` writes both: Pass A bare, Pass B per-leg qualified). So a
strip-to-bare key resolved the wrong leg's value: `ORDER BY t2.sk DESC` over `t1 JOIN t2` sorted by t1's `sk`.
This was the same last-leg-wins trap as the P1a alias-binding fix.

Fix: classify the fold's FROM source (`classifySortSource` — a binary INNER `LogicalJoin` is a join source,
carrying its leg FROM-aliases; anything else is single-table). The qualifier handling is then **source-aware**:

- **single-table**: strip to bare (`resolveKeyName`) — the merged outer row is the scan row with bare keys; a
  qualified key resolves against the bare column. Unchanged from round-4.
- **JOIN**: KEEP the qualified key (`T2.SK`) when its qualifier names a leg. The appended remainingOrderBy
  field is named with the qualified key (`T2.SK`) and carries a **qualified leg reference value**
  (`FieldValue{Field:COL, Child:QOV(LEG)}`) — which the NLJ rule's `rebaseOuterLegValue` rewrites to the merged
  row's qualified `LEG.COL` key (resolving the correct leg, exactly as a selected leg column is rebased).
  `pullUpSortKeyValue` keeps the qualified key so the `InMemorySort` resolves `compareByField("T2.SK")` against
  that qualified output column. The sort-key analog of `rebaseOuterLegValue`, consistent with the P1a fix.

A selected qualified key (`t2.id` where `T2.ID` is an output column) was ALSO broken (it was stripped to bare
`ID` → wrong leg); the source-aware handling fixes both the selected and non-selected join cases. An
**unqualified** ORDER BY of a column that collides across legs (`ORDER BY sk` over two `sk` columns) is
rejected by the semantic analyzer (`42702: column reference is ambiguous`) BEFORE the fold — a clean error,
never a silent wrong leg.

Regression pins: `projected_exists_round5_fdb_test.go` — the P1 `SELECT *`/`SELECT * NOT EXISTS` column-metadata
tests (exactly the outer table's columns, arity-checked Scan); and the full ORDER BY matrix {single-table,
2-table INNER JOIN} × {sort key selected, NOT selected} × {qualified, unqualified} × DESC/ASC. The join rows use
colliding `sk`/`id` columns whose leg orderings are deliberate INVERSES, so a wrong-leg or no-op sort yields a
different t1.id sequence and fails loudly.

## 8d. Round-6: the derivation-consistency root-cause (labels + ORDER-BY alias)

Round-6 codex review found two more silent-wrong bugs, **both** rooted in the same anti-pattern: the
projected-EXISTS fold RECONSTRUCTS column-metadata and sort-key derivation piecemeal instead of REUSING the
normal (non-EXISTS) projection path's logic. Each round added a point-patch to the fold; the next review found
the next reconstruction gap. The round-6 fix is structural — make the folded path's column-metadata and
sort-key derivation CONSISTENT-BY-CONSTRUCTION with the normal path, by extracting and sharing the normal
path's helpers, so future variations are correct by construction.

### The two derivation divergences found and unified

| dimension | normal projection path | folded-EXISTS path (BEFORE) | unified (AFTER) |
|---|---|---|---|
| **ResultSet Name + Label** | `deriveColumnsFromProjection`: datum Name = alias / qualified col ref; display Label = alias, else BARE leaf of a qualified FieldValue (`SELECT t1.id` → label `ID`, never `T1.ID`); type + nullability resolved against the defining leg | `deriveColumnsFromFlatMap` folded branch: `Name = upper(f.Name)` (qualified `T1.ID`), `Label` empty → ResultSet exposed `T1.ID`; type defaulted via an ad-hoc outer-name map | extracted the normal per-column derivation into shared **`deriveProjectionColumnDef(value, alias, idx, descs)`** (Name+Label+type+nullable), reused by BOTH paths; **`foldedFieldAlias`** recovers the SELECT-list alias from the fold's RecordConstructor field (compares BARE LEAVES so an unaliased qualified column is recognized) |
| **ORDER BY sort key** | the sort sits BELOW the projection; `upgradeSortKeyValues` resolves an alias to its projected Value and the scan row is sorted before the rename | the fold re-applies the sort ON TOP of the folded projection; `pullUpSortKeyValue`'s FieldValue case returned BEFORE the output-field-value match the non-FieldValue case had → an alias key resolved by NAME and read a same-named output column (`ORDER BY x` where `x = id AS x, id = col1 AS id` read field `ID` = col1) | `pullUpSortKeyValue` runs the output-field-value match (**`pullUpToOutputField`**, the shared helper) FIRST for EVERY key shape — the same key↔output-field correspondence the normal alias path uses — so an alias key pulls up to the output field it IS; name-based resolution is the fallback (appended remainingOrderBy columns) |

**P2a** (`SELECT col1 AS id, id AS x, EXISTS(...) FROM t1 ORDER BY x`): the alias `x` was resolved by
`upgradeSortKeyValues` to the projected Value `FieldValue{ID}` (= `ProjectedValues[X]`, pointer-identical to
the output field `X`'s Value). `pullUpToOutputField` matches that pointer and pulls up to `FieldValue{X}`, so
the sort orders by the materialized `X` column (= t1.id), not by the field named `ID` (= col1) that the
name-based path was wrongly reading. Running the value-match FIRST for the FieldValue case is the whole fix —
it is exactly the resolution Java's `OrderByExpression.pullUp` performs against the lower select's result value.

**P2b** (`SELECT t1.id, EXISTS(...) FROM t1 …`): the folded field for `t1.id` carries `Name = "T1.ID"` but its
Value is the bare rebased outer-row key `FieldValue{ID}`. `foldedFieldAlias` compares the bare leaves
(`bare("T1.ID") == bare("ID")` → both `ID`) and recognizes the field as UNALIASED, so
`deriveProjectionColumnDef` derives the display label as the bare leaf `ID` — identical to `SELECT t1.id`
without an EXISTS. For a JOIN the field value is the composite `FieldValue{Field:ID, Child:QOV(T1)}` whose
`ExplainValue` is the qualified `T1.ID`, so the datum Name stays qualified (resolving the authoritative
merged-row key) while the label is still the bare leaf — matching the JOIN control.

Regression pins: `projected_exists_round6_fdb_test.go` — P2a ORDER BY by {column alias, expression alias,
qualified col, bare col} over distinct values (a wrong-field sort yields a different visible order and fails);
P2b label **+ type + nullability** parity with a non-EXISTS control query for {bare, aliased, qualified,
qualified-over-JOIN} columns asserted via the driver's `Columns()`/`ColumnTypes()`, plus a qualified-datum
value-scan proving the changed datum-lookup Name still resolves.

### Final supported vs cleanly-rejected projected-EXISTS shapes (post round-6)

**Supported (fold correctly, real ordering, labels identical to a non-EXISTS control):**
- `SELECT … EXISTS/NOT EXISTS [AS x] FROM t` — projected, correlated or not, empty (FALSE), join-inside-subquery.
- `SELECT *  … WHERE EXISTS/NOT EXISTS(…)` — reports EXACTLY the outer table's columns (round-5 fix #1).
- `SELECT id / id AS a / t1.id / t1.id (over JOIN), EXISTS(…) …` — every projected column's Name/Label/type/
  nullability is IDENTICAL to the same query WITHOUT the EXISTS (round-6 fix P2b): a bare/aliased column keeps
  its name; a qualified column reports the BARE leaf as the user-visible label.
- `… ORDER BY id` / `<exists-alias>` / `<select-list alias of a column>` / `<expression alias>` /
  `<col-not-in-select>` / `<qualified col>` / multi-key / `LIMIT n`. An ORDER-BY of a SELECT-list alias resolves
  to the OUTPUT field it names, never a same-named underlying column (round-6 fix P2a). Single-table strips the
  qualifier; a JOIN keeps it and resolves the named leg — selected and non-selected, both legs, both directions.
- `SELECT t1.id, t2.id, EXISTS(…) FROM t1 JOIN t2 ON … [ORDER BY <qualified leg col>]` — INNER join in FROM.
- `SELECT id, EXISTS(…), (SELECT MAX(id) FROM t2) FROM t` — alongside an UNCORRELATED scalar subquery.
- `SELECT id, NOT (EXISTS(…)) [AS x] FROM t` / `NOT ((EXISTS(…)))` — parenthesized/nested NOT.
- PK / secondary-index fast-path correlated probes.

**Cleanly rejected (`ErrCodeUnsupportedQuery` / a clean semantic error, NEVER wrong rows):**
- **Multiple** projected EXISTS in one SELECT, or EXISTS in WHERE *and* SELECT — the multi-existential boundary.
- Projected EXISTS with a **GROUP BY / aggregate**, **DISTINCT**, or **UNION** between projection and filter.
- **GROUP BY on an EXISTS expression**.
- **Outer-join** (LEFT/RIGHT/FULL) in FROM with a projected EXISTS.
- **Projected EXISTS + a CORRELATED scalar subquery** in the same SELECT (round-4).
- **Unqualified ORDER BY of a column that collides across join legs** — `42702 ambiguous` (semantic analyzer,
  pre-fold; round-5).
- **ORDER BY a COMPUTED expression that is not a SELECT output** — e.g. `… ORDER BY col1 + 1` where `col1 + 1`
  is not projected (round-7). `collectExtraSortColumns` can only append *named* columns, so the sort
  re-applied above the folded FlatMap would read a record lacking the expression's inputs and silently
  mis-order; the fold bails (→ guard rejects) instead. A SELECTED computed expression (ordered by its
  alias, or matching a projected field) still folds correctly — the rejection is narrow.

## 8e. Round-8: the alias-provenance root-cause (datum-Name + label, over JOIN and under hidden ORDER BY)

Round-8 codex review found two more metadata divergences, **both** because the fold RE-DERIVED a projected
column's alias/Name/Label from the FOLDED record (its value shape, via a bare-name heuristic) instead of
carrying the ORIGINAL `LogicalProject`'s per-column alias provenance (`Aliases[i]`, where `""` means no alias).
Every round added another piecemeal re-derivation; the next review found the next gap. The round-8 fix removes
the inference entirely and makes the folded path's column Name+Label **consistent-by-construction** with both
the executed record key and Java's rule.

### The execution contract that dictates the fix

`RecordConstructorValue.Evaluate` keys the folded output row by `f.Name` — ONE map key per field — and a
positional/named Scan looks a column up by `ColumnDef.Name`. Therefore the datum `Name` of a folded column MUST
equal `f.Name`; any value-derived Name diverges from the key and the Scan reads NULL. The user-visible label is,
per Java (`LogicalOperator.generateSimpleSelect`/`generateSort` run the top-level output through
`clearQualifier()`, and `RelationalStructMetaData.getColumnLabel == getColumnName == the StructType field name`),
the SELECT-list Identifier's BARE LEAF — `SELECT t1.id` → `ID`, `t1.id AS id` → `ID`, `id AS the_id` → `THE_ID`,
`t2.id` over a JOIN → `ID`. Java never re-derives the label from the column's Value.

### Fix P1 — datum Name + label from `f.Name`, not the value (`deriveColumnsFromFlatMap` / new `foldedColumnDef`)

The old folded branch called `foldedFieldAlias` (a bare-name heuristic) then `deriveProjectionColumnDef`, which
set the datum `Name` from the value's `ExplainValue`. Two divergences:

- **explicit alias == bare leaf** (`SELECT t1.id AS id … JOIN`): `foldedFieldAlias` saw `bare("ID") == bare(value
  "T1.ID")` and reported UNALIASED, so the datum Name became the qualified value name `T1.ID` while the record is
  keyed by the alias `ID` → the column read **NULL**.
- **unaliased qualified over a JOIN** (`SELECT t2.id … JOIN`): the NLJ rule rebases the value to the composite
  `FieldValue{Field:ID, Child:QOV(merged)}`, so `foldedFieldAlias`'s `Child==nil` bare-compare was skipped → it
  returned the qualified `f.Name` as a fake alias → label leaked **`T2.ID`** instead of bare `ID`.

`foldedFieldAlias` is **deleted**. The new `foldedColumnDef(f, descs)` sets `Name = f.Name` (the record key,
cannot diverge), `Label = bare leaf of f.Name` (Java's rule), and resolves the column TYPE from the value
(ExistsValue → BOOLEAN; a leg column against its defining descriptor via the value's qualified reference). No
alias inference, no value-derived Name — both divergences are impossible by construction.

### Fix P2 — the hidden-ORDER-BY cleanup re-projection must reuse the ORIGINAL aliases

When a non-selected sort column is appended (`SELECT t1.id, EXISTS(…) FROM t1 ORDER BY t1.sk`), the fold wraps a
final cleanup `LogicalProjectionExpression` that drops the hidden column. It force-aliased EVERY visible field to
its datum Name (`projAliases[i] = name`), which turned an unaliased `t1.id` into an explicit alias `T1.ID`
(qualified label leaked into the ResultSet) and dropped the EXISTS column's BOOLEAN type. Fix: the cleanup reuses
the ORIGINAL `p.Aliases[i]` (`""` → leave the column unaliased so its label derives the bare leaf, exactly as the
non-hidden-sort path) and preserves each projected value's type. The cleanup's `FieldValue.Field` stays equal to
the fold's `f.Name` (the exact record key, no qualified→bare fallback in `FieldValue.Evaluate`), so a Scan never
reads NULL. Additionally, `deriveColumnsFromProjection` now inherits a renamed pass-through column's type from its
inner plan's same-named derived column — a column renamed to its alias is not a proto field, so the
descriptor-based type lookup couldn't resolve it; the projection never RE-TYPES a column it merely renames/drops,
so it inherits the inner folded column's type. Adding a hidden sort column no longer changes any visible column's
public Name/Label/type.

### What round-8 threaded through (the root fix)

- `LogicalProject.Aliases[i]` (the explicit-alias flag, `""`==none) → the cleanup re-projection's per-column
  alias, replacing the force-alias-everything behaviour.
- The folded `RecordConstructorField.Name` (which the fold already set from `Aliases[i]` when explicit, else the
  column reference) → used DIRECTLY as the generator's datum `Name` and (bare-leaf) `Label`, replacing the
  value-shape `foldedFieldAlias` inference.
- Each folded value's TYPE → preserved through the cleanup (`FieldValue.Typ`) and, for genuinely-Unknown plain
  columns, inherited from the inner folded column in `deriveColumnsFromProjection`.

Regression pins: `projected_exists_round8_fdb_test.go` — P1 explicit-alias-over-JOIN + unaliased-qualified value
scan (reads NULL without the fix) and named-scan; a comprehensive `{bare, aliased, qualified, t1.id AS id over
JOIN, t1.id unaliased over JOIN}` Name+Label+type+nullability parity matrix vs non-EXISTS controls plus a non-NULL
value scan each; P2 hidden-ORDER-BY label/type parity for {qualified, aliased, bare} columns vs TRUE non-EXISTS
controls with the same hidden-sort shape (a force-alias revert labels the EXISTS column `T1.ID`/loses the type
while the control keeps `ID`/`BIGINT`). All round-1..7 tests still green.

## 8f. Round-9: the existential inner correlation must have its OWN identity (alias-shadow regression + computed-column naming)

Round-9 codex review found a **correctness REGRESSION of plain WHERE-EXISTS** (P1, silent-wrong) plus a metadata
divergence (P2). Both are fixed at the root.

### Fix P1 (REGRESSION, silent-wrong) — the existential inner correlation collided with the outer source alias

A WHERE-EXISTS (or NOT-EXISTS, or projected) whose subquery reuses the OUTER source TABLE — an alias-shadowing
self-subquery such as `SELECT id FROM t WHERE id > 1 AND EXISTS (SELECT 1 FROM t WHERE id = 1)` — gives the outer
source alias and the existential inner correlation the **same** name (`T`). The post-FlatMap re-architecture
(`ImplementNestedLoopJoinRule.implementExistentialSelect`) derived the inner correlation from
`sel.GetSourceAliases()[1]` = the subquery's SOURCE TABLE name (`sourceAlias(esq.Plan)`). When that equals the
outer source alias, the new pure-map FlatMap path:

- bound BOTH the outer row and the FirstOrDefault inner under the SAME correlation in `flatMapCursor.computeResult`
  (`WithBinding(outerAlias,…).WithBinding(innerAlias,…)`) — the inner overwrote the outer ⇒ the pass-through outer
  row was the FOD inner (NULL on an empty subquery) ⇒ a `SELECT id` read `converting NULL to int64`; and
- misclassified an outer-only predicate (`id > 1`, correlated to the shared name `T`) as an INNER (join)
  predicate (`GetCorrelatedToOfPredicate(p)[innerCorr]` matched), pushing it BELOW the FOD where it filtered the
  inner instead of the outer.

The OLD semi-join cursor returned the current outer row directly, so this worked before the re-architecture — hence
a regression. The root cause is the divergence from Java: **an existential quantifier in Java has its OWN unique
correlation identity** (`Quantifier.uniqueID()`), never the source table's name; Go's `buildCorrelatedExists`
qualified the inner correlation under the source table name, which can collide. (Go already mints a unique
`esq.Alias` via `UniqueCorrelationIdentifier()` for the existential QUANTIFIER, but the rule ignored it and used the
source name for the inner CORRELATION.)

Fix (`existsInnerCorrelation` in `cascades_translator.go`, applied at all three existential-SelectExpression build
sites — `buildExistentialSelect`, `buildExistentialJoinSelect`, `translateJoinWithExists`): register the existential
inner under the UNIQUE existential quantifier alias `esq.Alias` (so `GetSourceAliases()[N]` carries it and the rule
derives `innerCorr = esq.Alias`), and rebase the inner-leg references of the join predicate from the source alias to
`esq.Alias` IN LOCKSTEP (`predicates.RebasePredicate(joinPred, {srcAlias: esq.Alias})`) so the predicate's QOV
correlation still matches the FlatMap inner binding. Outer and inner correlations are then distinct by construction,
the FlatMap never clobbers the outer binding, and outer-vs-inner predicate classification stays correct.

The rebase is GUARDED to a clean single-table-scan inner (`existsInnerSafeToRename`). Two inner shapes carry
references to their own source alias that the rename cannot reach, so they keep the source-alias (leg) routing:

- a **JOIN inner** emits a MERGED row resolved by qualified leg keys (`T2.ID`, `T3.T2_ID`, …) with NO single-alias
  binding (`executePredicatesFilter`: `producesMergedRows ⇒ bindAlias=false`); renaming would point the predicate
  at a `<uniqueAlias>.*` namespace `mergeRows` never writes ⇒ NULL ⇒ EXISTS always false; and
- a **nested-EXISTS inner** (a `LogicalFilter` carrying its own `ExistsSubqueries`) has a nested existential
  correlation that references the MIDDLE scan's source alias from INSIDE `esq.Plan` — not in `esq.JoinPredicate` —
  so the rename leaves it orphaned.

The alias-shadow collision the rename fixes only arises for the single-alias-bound scan (one bare namespace bound
under one alias); the merged-row / nested-EXISTS inners route by distinct qualified keys and cannot clobber the
outer binding, so leaving them on source-alias routing is correct (and necessary).

### Fix P2 (metadata) — unaliased COMPUTED select item named by the expression text

`SELECT id + 1, EXISTS(...) AS e FROM t` — the fold named the folded computed field with the expression TEXT
(`name := strings.ToUpper(col)` = `ID + 1`). `RecordConstructorValue.Evaluate` keys the executed row by `f.Name`,
and `foldedColumnDef` derives Name+Label from it, so `Rows.Columns()` reported `ID + 1`. The normal projection path
exposes an unaliased non-field (computed) expression under a GENERATED positional name (`_0` —
`deriveProjectionColumnDef`'s `_idx` rule; `executeProjection` also stores the value under the `_i` key). Adding the
EXISTS therefore changed the public column name from `_0` to `ID + 1` and broke downstream positional references.

Fix (`translateProjectOverExistsFilter`): when a folded column has NO explicit alias and its resolved value is not a
`*values.FieldValue` (a computed expression — Java's anonymous-expression projection), name it with the SAME
positional `_i`. The folded column's record key + datum Name + display Label are then identical to the non-EXISTS
control on every axis (consistent-by-construction with the normal path; no `foldedColumnDef` change needed — its
existing `bare("_0") == "_0"` derivation is already correct, and the hidden-ORDER-BY cleanup re-projection inherits
the name via `fields[i].Name`).

Regression pins: `exists_alias_shadow_fdb_test.go` — P1 alias-shadow self-subqueries for WHERE-EXISTS, NOT-EXISTS,
correlated, and projected EXISTS (each returned NULL/wrong/filtered before the fix), asserting the correct rows;
`exists_computed_column_fdb_test.go` — P2 column-name parity with a `SELECT id + 1` control read DYNAMICALLY (so the
test pins parity, not a hardcoded scheme) plus correct values. Full sqldriver bazel suite + `pkg/recordlayer/query/
...` + `pkg/relational/core/...` green; EXISTS suite 10× deterministic; all round-1..8 + WHERE/NOT-EXISTS tests
still green.

## 8g. Round-10: the predicate-routing root-cause (multi-table inner) + qualified-ORDER-BY/alias collision

Round-10 codex review found two more silent-wrong bugs, **both** a single-correlation assumption breaking once the
shape carries MORE than one relevant correlation. Each is fixed at the root; **no EXISTS shape is rejected** — the
audit (below) shows every multi-table-inner shape folds correctly.

### Fix P2a (silent-wrong) — a multi-table EXISTS inner correlating to a NON-rightmost leg dropped every row

`… EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)` — the existential inner is a multi-table (comma/JOIN)
source. The whole correlation predicate `t2.t1_id = t1.id` lives in `esq.JoinPredicate` (the inner FROM has no own
WHERE beyond it); `existsInnerCorrelation` reports the inner correlation as `sourceAlias(esq.Plan)` = the RIGHTMOST
leg (t3), because `existsInnerSafeToRename` (correctly) declines to rename a JOIN inner. The NLJ rule
(`implementExistentialSelect`, and the JOIN-in-FROM `implementJoinWithExistential`) then split the non-EXISTS
predicates by a SINGLE inner-correlation equality test (`GetCorrelatedToOfPredicate(p)[innerCorr]`). The predicate
references t2 (the non-rightmost leg), not t3, so it matched **neither** "inner" nor stayed correctly "outer-only" —
it was bucketed as outer-only and evaluated ABOVE/around the FlatMap with **no t2 binding**, so the correlation never
held and **every outer row was dropped** (WHERE → 0 rows; projected → constant `false`).

Root fix (`rule_implement_nested_loop_join.go`, `predicateTouchesInner`, variadic over the outer correlations): a
non-EXISTS predicate is routed BELOW the FirstOrDefault (against the inner row) iff it references ANY correlation
OTHER than the FlatMap's OUTER leg(s). The FlatMap binds exactly the outer row(s) under the outer correlation(s)
(one for `implementExistentialSelect`, the two join legs for `implementJoinWithExistential`); every other
correlation a predicate names is an inner leg — the existential source table, or one of its multi-table FROM legs.
The correlation predicate then sits below the FOD where the merged inner row resolves the inner-leg columns by their
qualified `LEG.COL` keys (`producesMergedRows ⇒ resolve-by-key`) and the live outer binding resolves the correlated
outer column. A single-table inner is unaffected (its one leg references the rule's inner correlation, which is not
an outer correlation, so it still routes below).

**Audit (every multi-table-inner shape — all CORRECT, none rejected):** 2-leg and 3-leg inners; correlation to the
leftmost / rightmost leg; an inner-only join conjunct between legs (`AND t3.t2_id = t2.id`); an explicit `JOIN … ON`
inner; NOT-EXISTS; a non-correlated multi-table inner; an outer-only predicate alongside the inner (`col1 > 97 AND
EXISTS(…)`); projected and projected-NOT-EXISTS; the inner under a JOIN in the OUTER FROM (the
`implementJoinWithExistential` path, WHERE and projected). All return the correct rows/booleans.

### Fix P2b (silent-wrong ORDER) — a qualified ORDER BY key whose bare name collides with a SELECT alias

`SELECT col1 AS id, EXISTS(...) FROM t1 ORDER BY t1.id` — the fold stripped the qualified source key `t1.id` to its
bare leaf `ID` (single-table source) and tested OUTPUT MEMBERSHIP by that bare NAME. The SELECT-list alias `id`
(which projects `col1`) is also named `ID`, so the membership matched the unrelated alias, the key was treated as
"already in output", and the sort ordered by the output column `ID` (= col1) instead of `t1.id` — a silent wrong
order. (The control `SELECT t1.id … ORDER BY t1.id` worked only because the output field keeps the qualified NAME
`T1.ID`, so the bare `ID` did not collide.)

Root fix (`cascades_translator.go`): output membership for a sort key is now **VALUE-based**, not bare-name-based.
`sortKeySourceValue(k)` builds the source-column Value the key references (a BARE `FieldValue` over the outer scan
row for single-table; a QUALIFIED leg reference for a JOIN leg), and `sortKeyInOutput` reports "in output" only when
some output field's VALUE semantically equals it — i.e. an output field GENUINELY PROJECTS that source column, never
merely shares a bare name with an unrelated alias. A non-projected qualified source key is appended as a hidden
`remainingOrderBy` field NAMED BY ITS QUALIFIED PROVENANCE (`T1.ID`) — collision-free with the output alias `ID` —
carrying the source-column value; the final cleanup re-projection drops it. `pullUpSortKeyValue` resolves each key by
VALUE match: first the key's RAW value (SELECT-list aliases incl. the computed EXISTS boolean, set by
`upgradeSortKeyValues`), then the source-column value (against the EXTENDED fields incl. the hidden column). The
bare-alias ORDER BY path is UNCHANGED: `ORDER BY id` (the alias) still resolves to the output column, because
`upgradeSortKeyValues` set the key's Value to the projected value and Pass 1 pulls it up; only a QUALIFIED source
reference (`t1.id`) is distinguished from a same-named alias. The JOIN matrix (round-5) is unaffected — its source
value is the qualified leg reference, value-matched the same way.

Regression pins: `projected_exists_round10_fdb_test.go` — P2a {2-leg non-rightmost / rightmost, 3-leg,
inner-join-pred, explicit `JOIN…ON`, NOT-EXISTS, outer-pred, projected, projected-JOIN-from, WHERE-JOIN-from} all
asserting correct rows + a single-table control; P2b qualified-`t1.id` ASC/DESC ordering (asserting the col1 row
sequence — a wrong-column sort visibly differs), the bare-alias-is-output-column control (proves the alias path is
unchanged), and the selected-qualified pull-up control. Both fixes verified revert-proof (each revert fails exactly
its dimension; the controls still pass). Full sqldriver bazel suite + `pkg/recordlayer/query/...` +
`pkg/relational/core/...` green; EXISTS suite 10× deterministic; all round-1..9 + WHERE/NOT-EXISTS tests still green.

### Final supported vs cleanly-rejected projected/WHERE-EXISTS shapes (post round-10)

**Supported (correct rows, real ordering):** everything in the post-round-6 list, PLUS:
- **Multi-table EXISTS inner** (`EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)`) — comma-join or explicit
  `JOIN…ON`, 2+ legs, correlation to ANY leg (leftmost/rightmost), inner-only join conjuncts, NOT-EXISTS,
  non-correlated, alongside an outer-only predicate, in WHERE or projected, and under a JOIN in the OUTER FROM.
- **`SELECT col1 AS id, EXISTS(…) FROM t ORDER BY t1.id`** — a qualified source ORDER-BY key whose bare leaf
  collides with a SELECT alias orders by the SOURCE column, never the same-named output alias.

**Cleanly rejected (`ErrCodeUnsupportedQuery` / a clean semantic error, NEVER wrong rows):** unchanged from
post-round-6 (multiple existentials / WHERE+SELECT EXISTS; GROUP BY/DISTINCT/UNION between projection and filter;
GROUP BY on an EXISTS; outer-join FROM with projected EXISTS; projected EXISTS + a correlated scalar subquery;
ambiguous unqualified ORDER BY across join legs; ORDER BY a non-projected COMPUTED expression).

## 8h. Round-11: the predicate-routing root-cause (route by the KNOWN inner-leg set, not "any non-outer")

Round-11 codex review found that the round-10 predicate-routing fix REGRESSED a NEW shape. Round-10 routed a
non-EXISTS predicate BELOW the FirstOrDefault iff it referenced ANY correlation OTHER than the FlatMap's outer
leg(s) — an ABSENCE test ("not the outer ⇒ inner"). That is wrong for a correlation that is neither outer NOR an
inner leg: an UNCORRELATED SCALAR SUBQUERY in a predicate (`price > (SELECT MAX(x) FROM t2)`) carries its OWN
alias (`ScalarSubqueryValue.GetCorrelatedTo` adds it) that is non-outer yet NOT an inner table leg — it is a
pre-evaluated EXTERNAL binding. The absence test pushed the scalar predicate BELOW the FOD; alongside an empty
NOT-EXISTS the FOD yields NULL, its IS-NULL residual admits every outer row, and the below-FOD scalar comparison
never ran → the scalar predicate was silently dropped (`price > MAX(x) AND NOT EXISTS(empty)` returned every
NOT-EXISTS-true row, including rows that fail `price > MAX(x)`).

### Fix P1 (silent-wrong) — route by inner-leg-set MEMBERSHIP, not "any non-outer"

The existential subquery's inner-leg correlations are KNOWN to the rule: they are the existential subquery's
actual FROM-source aliases (ALL legs of a multi-table inner). The discriminator is now POSITIVE membership in
that set:

- `collectInnerLegAliases(innerRef, innerCorr)` (`rule_implement_nested_loop_join.go`) computes the full
  inner-leg set by walking the existential subplan Reference and gathering every source alias it DECLARES (each
  `SelectExpression.GetSourceAliases()` entry + every ForEach/Physical quantifier alias — never a value-tree
  binding, so a scalar-subquery / parameter alias can NEVER enter). It then distinguishes two cases by whether
  the rule's inner correlation `innerCorr` is itself a declared leg:
  - **Multi-table inner** (`EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)`): `existsInnerCorrelation`
    declines the unique-alias rename, so `innerCorr` is the rightmost leg = a declared leg. Predicates reference
    the RAW leg aliases (t2, t3), resolved through the merged inner row's qualified `LEG.COL` keys → return ALL
    declared legs (`innerCorr ∪ {t2, t3, …}`). This preserves round-10's multi-table fix.
  - **Single-table inner** (`EXISTS (SELECT 1 FROM t WHERE …)`): `existsInnerCorrelation` RENAMED the inner
    correlation to a unique alias and rebased the join predicate onto it, so the predicate references `innerCorr`
    alone — never the subplan's raw scan alias → return `{innerCorr}`. Crucially the raw scan alias is NOT
    included: in the alias-shadow self-subquery (`FROM t … EXISTS (SELECT 1 FROM t …)`) the outer and inner share
    the name `T`, and an outer-only predicate (`id > 1`, correlated to `T`) would be mis-routed below the FOD if
    `T` leaked in (the round-9 regression — re-avoided by construction).
- `predicateReferencesInnerLeg(p, innerLegs)` replaces `predicateTouchesInner`: a predicate routes below the FOD
  iff its correlation set INTERSECTS innerLegs. Everything else — outer legs, AND scalar-subquery aliases, AND
  parameter/other external bindings — stays OUTER, where the pre-evaluated value is read and the comparison
  actually filters the outer row. Applied at BOTH existential-join rule methods (`implementExistentialSelect`
  with the single outer leg; `implementJoinWithExistential` with the two JOIN legs).

### Fix P2 (silent-wrong) — the projected-EXISTS fold dropped a WHERE-clause scalar subquery

`SELECT id, EXISTS(...) FROM t1 WHERE price > (SELECT MAX(x) FROM t2)` populated the FILTER's
`f.ScalarSubqueries`, but the fold's early return in `translateProject` bypasses `translateFilter` — the only
place those are registered into `t.scalarSubqueries` for the executor to pre-evaluate. So the WHERE-clause scalar
subquery was never pre-evaluated, its value stayed unbound (NULL), and `price > NULL` dropped every row (the
query returned 0 rows). This is the same fold-bypass class as round-4 (every step after the early-return must run
or the query be cleanly rejected). Fix: `translateProjectOverExistsFilter` now collects `f.ScalarSubqueries`
(exactly as `translateFilter` does) before building the existential select, so the executor pre-evaluates and
binds them. (A projected EXISTS + a CORRELATED scalar subquery in the SELECT list is still cleanly rejected —
§8b — unchanged; only the UNCORRELATED WHERE-clause scalar is pre-evaluated.)

### Predicate-routing audit (every EXISTS predicate shape — each correct or cleanly-rejected, no silent-wrong)

| shape | routing | result |
|---|---|---|
| outer-only column predicate (`col1 > 97 AND EXISTS`) | not in innerLegs → OUTER | filters the outer ✓ |
| inner-leg single-table (`EXISTS (SELECT 1 FROM t2 WHERE t2.fk = t1.id)`) | predicate refs renamed innerCorr ∈ innerLegs → below FOD | correlated semi-join ✓ |
| scalar-subquery-in-pred (`price > (SELECT MAX(x) FROM t2) AND [NOT] EXISTS`) | scalar alias ∉ innerLegs → OUTER | scalar filters the outer ✓ (round-11 P1) |
| multi-leg inner (`EXISTS (SELECT 1 FROM t2, t3 WHERE t2.t1_id = t1.id)`) | leg alias ∈ innerLegs (all legs) → below FOD | correct rows ✓ (round-10) |
| NOT-EXISTS (any of the above) | same routing, IS-NULL residual | correct ✓ |
| projected EXISTS (any of the above) | same routing, boolean projected by the map | correct ✓ |
| parameter marker in pred (`price > ? AND [NOT] EXISTS`) | parameter carries NO correlation → OUTER | parameter filters the outer ✓ |
| projected EXISTS + WHERE-clause scalar subquery | scalar pre-evaluated in the fold path | correct ✓ (round-11 P2) |
| projected EXISTS + a CORRELATED scalar subquery | logical guard | cleanly rejected (`ErrCodeUnsupportedQuery`) — §8b |

Regression pins: `projected_exists_round11_fdb_test.go` — the scalar-subquery-in-predicate shape with NOT-EXISTS
(empty inner for some rows), EXISTS, multi-table NOT-EXISTS, projected EXISTS + WHERE scalar, a parameter-marker
control, plus audit controls (plain NOT-EXISTS / EXISTS, outer-col predicate, scalar without EXISTS). The dataset
is built so the scalar EXCLUDES a NOT-EXISTS-true row (id 0, price ≤ MAX), so a dropped scalar visibly INCLUDES
it and fails loudly. Both fixes verified revert-proof: reverting the routing to "any non-outer" makes the
scalar-with-NOT-EXISTS return `[0 1 2 3 4]` (want `[2 4]`) and the EXISTS variant return `[]` (want `[3]`);
reverting the fold scalar-collection makes the projected-EXISTS-with-WHERE-scalar return `[]`. Full sqldriver
bazel suite + `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite 10× deterministic; all
round-1..10 + WHERE/NOT-EXISTS tests still green.

### Final supported vs cleanly-rejected (post round-11)

**Supported (correct rows, real ordering):** everything in the post-round-10 list, PLUS:
- **A scalar subquery (or parameter marker) in a predicate ALONGSIDE EXISTS / NOT-EXISTS** —
  `price > (SELECT MAX(x) FROM t2) AND [NOT] EXISTS (…)`, single-table or multi-table inner, WHERE or projected:
  the scalar/parameter predicate stays OUTER and actually filters; the EXISTS routing is unchanged.
- **`SELECT id, EXISTS(…) FROM t WHERE <pred with an uncorrelated scalar subquery>`** — the WHERE-clause scalar
  subquery is pre-evaluated in the fold path (no longer silently NULL).

**Cleanly rejected:** unchanged from post-round-10.

## 8i. Round-12: the convergence backstop — any EXISTS not in a directly-handled position is REJECTED, never mis-evaluated

Rounds 1–11 each point-handled one more EXISTS shape, and the next review found the next gap. codex round-12
found two more silent-wrong shapes — and the underlying problem is structural: an EXISTS atom can appear
ANYWHERE in an expression tree (WHERE predicate or SELECT item), so point-handling each shape never converges.
Round-12 replaces the whack-a-mole with a **comprehensive structural backstop**: the small set of
DIRECTLY-HANDLED positions stays working, and EVERY other position that contains an EXISTS is detected
structurally (typed predicate / parse tree, never `GetText`) and rejected cleanly with `ErrCodeUnsupportedQuery`
— so after round-12 there is NO silent-wrong EXISTS case anywhere.

### Directly-handled positions (kept working)

- **WHERE**: a predicate that is a direct existential / NOT-existential — `IsExistentialPredicate` /
  `IsNotExistentialPredicate`, i.e. a top-level `ExistentialValuePredicate` or a single-`NOT`-wrapped one
  (including each conjunct of a top-level AND). The NLJ rule lowers these to a FirstOrDefault inner + residual
  `QOV IS [NOT] NULL` filter.
- **SELECT**: a top-level projected `EXISTS(...)` / `NOT EXISTS(...)`, or its single paren/NOT wrapper of a bare
  EXISTS (`(EXISTS(...))`, `NOT (EXISTS(...))`, `NOT ((EXISTS(...)))`). These fold into the existential
  SelectExpression's result value (the ExistsValue eval, with the inner binding live).

### The two backstops (mechanism)

The two silent-wrong classes lose the existential at different layers, so each has its own structural detector;
both are typed-tree walks, never text matching:

- **P1a — wrapped WHERE EXISTS** (`query.CheckBuriedExistentialPredicate`, run post-translation alongside
  `CheckProjectedExistsFolded`, before planning, on BOTH the SELECT and the DML planning paths — the DML planner
  reuses the existential NLJ rule, so `DELETE/UPDATE … WHERE NOT (NOT EXISTS(...))` is just as silently-wrong, it
  matched every targeted row; primitive `predicates.ContainsExistentialPredicate`). With the
  FirstOrDefault re-architecture, only predicates matched by `IsExistentialPredicate` /
  `IsNotExistentialPredicate` add the residual `QOV IS [NOT] NULL` filter; an existential buried under any OTHER
  wrapper — `WHERE NOT (NOT EXISTS(...))`, `EXISTS(...) OR p`, `id > 1 AND NOT (NOT EXISTS(...))`, `NOT (EXISTS(...)
  AND p)` — falls into the regular-predicate bucket, where the empty FirstOrDefault's NULL default is never
  removed and every outer row silently passes. The guard walks every predicate-bearing expression
  (SelectExpression / LogicalFilterExpression): for each top-level predicate NOT in a directly-handled position,
  if its subtree CONTAINS an `ExistentialValuePredicate` at any depth → reject. (Since RFC-141 collapsed to the
  single `ExistentialValuePredicate` representation, a buried existential that reaches a plannable tree always
  carries one — so this is exhaustive.)
- **P1b — nested projected EXISTS** (`expr.NestedExistsProjectionError`, raised in `walkExpressionInner` in
  projection position; structural check `expr.ContainsExistsAtom` + `isDirectlyFoldableProjectedExists`). The
  projection walker only exposes an evaluable `ExistsValue` when EXISTS is the directly-foldable top-level SELECT
  shape; a nested EXISTS — `CASE WHEN EXISTS(...) THEN 1 ELSE 0 END`, `EXISTS(...) AND x`, `(EXISTS(...) OR x)`,
  `NOT (EXISTS(...) AND x)` — takes the predicate path → a `predicateValue` whose ExistsValue is evaluated ABOVE
  the FlatMap with the binding dead → constant false / NULL. In projection position, if the SELECT item CONTAINS
  an EXISTS atom but is NOT one of the three directly-foldable shapes, the walker returns
  `NestedExistsProjectionError`. Crucially this is a DISTINCT error from `UnsupportedExpressionShapeError`
  (which the projection callers SWALLOW to fall back to the text path — itself the silent-wrong route); the two
  projection callers (`logical_predicate.go`, `plan_visitor.go`) convert it to `ErrCodeUnsupportedQuery`.

A bare projected `(EXISTS(...))` / `NOT (EXISTS(...))` and a direct nested EXISTS inside a SUBQUERY's own WHERE
(a top-level existential within its OWN SelectExpression) are NOT rejected — they are directly-handled. A
fake-checkbox test (`TestFDB_SubqueryInCase`) that asserted `CASE WHEN EXISTS(...)` "works" while only checking
`err == nil` and never validating the (all-ELSE, silent-wrong) rows is now corrected to pin the clean rejection.

### Final supported vs cleanly-rejected projected/WHERE-EXISTS shapes (post round-12)

**Supported (correct rows, real ordering):** unchanged from post-round-11 — exactly the directly-handled WHERE
and SELECT positions above (top-level / single-NOT / paren-wrapped EXISTS & NOT-EXISTS, correlated or not,
multi-table inner, JOIN-in-FROM, alongside ORDER BY / LIMIT / an uncorrelated scalar subquery, a scalar/parameter
predicate alongside EXISTS, a direct EXISTS in a subquery's own WHERE).

**Cleanly rejected (`ErrCodeUnsupportedQuery` / a clean semantic error, NEVER wrong rows):** everything from
post-round-11, PLUS — the convergence backstop — **ANY EXISTS not in a directly-handled position**:
- **WHERE**: an existential buried under a wrapper that is not the direct / single-NOT shape — `NOT (NOT
  EXISTS(...))`, `EXISTS(...) OR p`, `id > 1 AND NOT (NOT EXISTS(...))`, `NOT (EXISTS(...) AND p)`, any deeper
  AND/OR/NOT nesting.
- **SELECT**: a nested projected EXISTS — `CASE WHEN EXISTS(...) THEN ...`, `EXISTS(...) AND/OR/= x`,
  `NOT (EXISTS(...) AND x)`, `(EXISTS(...) OR x)`, or any expression that merely CONTAINS an EXISTS without being
  the directly-foldable top-level shape.
- **OTHER POSITIONS (adversarial audit, round-12):** three more silent-wrong positions, all where the EXISTS is
  NOT a top-level boolean term and so is silently dropped / folded to a constant false:
  - **JOIN ON** clause (`t1 JOIN t2 ON EXISTS(...)`): the ON resolver carries no SubqueryPlanner → the EXISTS
    fails to resolve and the whole ON condition is dropped → every joined row passes. Detected by
    `expr.ContainsExistsAtom` in `upgradeJoinOnPredicates`, rejected cleanly.
  - **ORDER BY** key (`ORDER BY EXISTS(...)`, `ORDER BY CASE WHEN EXISTS(...) …`): the sort-key resolver carries
    no SubqueryPlanner → the key keeps its raw text and never evaluates → wrong/no ordering. Detected by
    `expr.ContainsExistsAtom` in the ORDER-BY validation (`plan_visitor.go` + `logical_predicate.go`), rejected.
  - **WHERE clause, but BURIED in a scalar expression** (`WHERE CASE WHEN EXISTS(...) THEN 1 ELSE 0 END = 1`,
    `WHERE (EXISTS(...)) = true`): the EXISTS is lowered into a CASE / comparison operand (a scalar Value) with no
    existential quantifier driving it, so it evaluates to a constant false → every row dropped. Detected by a
    structural parse-tree walk `expr.WhereExistsInScalarPosition` (the WHERE companion to
    `isDirectlyFoldableProjectedExists`): an EXISTS is directly-handled in WHERE iff the path from the WHERE root
    reaches it through ONLY boolean-combinator nodes (AND/OR, NOT, transparent paren-wrap); the moment it is
    reachable only via a scalar node (comparison, CASE, function, arithmetic) it is buried → reject. The check runs
    on the SELECT WHERE (plan_visitor.go), the DML WHERE (`DELETE/UPDATE … WHERE <buried EXISTS>`, at the DML
    dispatch in cascades_generator.go — the DML WHERE-build path differs and folds the EXISTS to a constant too),
    and across an `INSERT … SELECT` subtree (`expr.AnyWhereExistsInScalarPosition`, since the INSERT-SELECT body is
    rebuilt through a path that bypasses the per-statement guard). A SELECT's WHERE inside a CTE body / derived table
    is also caught (those build through plan_visitor.go).
  - EXISTS in a **HAVING** clause already surfaces a clean "could not plan query" error.
  (An `ORDER BY <select-list-EXISTS-alias>` is NOT rejected — the key is the alias identifier, not a raw EXISTS
  atom; the round-3 supported shape is preserved. A top-level WHERE EXISTS / NOT-EXISTS / AND-conjunct / paren is
  the directly-handled shape and is NOT rejected.)

Regression pins: `projected_exists_round12_fdb_test.go` — P1a {double-NOT, OR, buried-in-AND, NOT-of-AND}, P1b
{CASE-WHEN, AND, NOT-of-AND, OR} guard sentinels (each asserts the clean error and FAILS if any rows return), plus
controls proving every directly-handled shape still WORKS (top-level projected EXISTS/NOT-EXISTS, simple WHERE
EXISTS/NOT-EXISTS, paren/nested-NOT, WHERE EXISTS AND pred, and a direct nested EXISTS in a subquery WHERE), and a
DML sentinel+controls (`TestFDB_ProjectedExistsRound12_DML`: `DELETE … WHERE NOT (NOT EXISTS)` rejects with no row
change; direct `DELETE WHERE [NOT] EXISTS` still mutates the correct rows). Both backstops verified revert-proof:
disabling them makes the sentinels return the silent-wrong rows (the DML revert silently DELETEs all rows).
`predicates.ContainsExistentialPredicate` unit-tested across all wrapper depths. Full sqldriver bazel suite +
`pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS suite 10× deterministic; all round-1..11 +
WHERE/NOT-EXISTS tests still green.

## 8j. Round-13: the convergence fix — the round-12 backstops must STOP at subquery boundaries (nested-subquery EXISTS is classified in its OWN context)

The round-12 backstops closed every *silent-wrong* surface. codex round-13 found the FINAL convergence issue, and
it is an **over-rejection**, not a silent-wrong result — so the silent-wrong surface stayed closed. The round-12
structural EXISTS detectors (`ContainsExistsAtom`, `WhereExistsInScalarPosition`, `AnyWhereExistsInScalarPosition`,
and the projection backstop via `isDirectlyFoldableProjectedExists`) recursed into NESTED subqueries, so an EXISTS
belonging to a nested scalar / IN / derived-table subquery's OWN clause was mis-attributed to the OUTER expression
and falsely rejected:

```
SELECT id, (SELECT MAX(id) FROM t2 WHERE EXISTS (SELECT 1 FROM t3)) FROM t1
```

failed with "projected EXISTS in this query shape is not yet supported" — the outer projection's structural
detector descended into the scalar subquery (a `SubqueryExpressionAtomContext`) and saw the nested EXISTS, even
though that EXISTS belongs to the scalar subquery and was planned correctly before round-12.

**The principle — each subquery is classified in its own translation context.** A scalar / IN / derived-table
subquery nested inside an expression opens a NEW query scope; its WHERE / projection / ORDER BY EXISTS atoms are
that subquery's concern and are guarded when *it* plans, not the outer expression's. The round-12 detectors must
match an `ExistsExpressionAtomContext` at the CURRENT query level (kept) but must NOT descend into a nested
subquery node.

**Fix (boundary stop).** A single shared helper `introducesNestedQueryScope` (walk.go) identifies the
expression-level nodes that open a new query scope: `SubqueryExpressionAtomContext` (`(subquery)` as a value) and
`InListContext` (`x IN (subquery)`). The four parse-tree detectors consult it consistently:
- `ContainsExistsAtom` — matches an EXISTS atom at the current level (before any boundary check; its own subquery
  is not descended because matching returns immediately), then STOPS at a `introducesNestedQueryScope` node.
  `WhereExistsInScalarPosition`'s "buried" leaf (`ContainsExistsAtom(ctx)`) inherits the stop.
- `AnyWhereExistsInScalarPosition` — its tree walk skips a `introducesNestedQueryScope` child, so it reaches only
  the INSERT's own SELECT-body WHERE, not a nested subquery's WHERE.
- `isDirectlyFoldableProjectedExists` / `existsAtomOf` / `existsAtomInExpressionAtom` were already boundary-safe
  (they only descend through NOT / paren-wrap shapes, never into a `SubqueryExpressionAtomContext`).
- The logical-tree / value-tree detectors (`projectValuesReferenceExists`, `projectionHasExistsValue`,
  `CheckProjectedExistsFolded`) were already boundary-safe by construction: a scalar subquery is a
  `ScalarSubqueryValue` whose `Children()` is `nil` — `WalkValue` cannot see its inner plan; the EXISTS lives as an
  `ExistentialValuePredicate` inside the subquery's own plan, owned by the subquery's own `SelectExpression`.

**Root fix found while pinning the variants (a real silent-wrong-result bug the boundary stop EXPOSED).** A
scalar / EXISTS / derived-table subquery's inner plan is built through `buildLogicalPlanForQueryWith*` →
`buildLogicalPlanForSelectWithCTECatalog_postBuild`, a SECOND WHERE-build path distinct from the PlanVisitor's
`visitSelectQuery`. That path did NOT carry the `WhereExistsInScalarPosition` guard, so a buried-scalar EXISTS in a
nested subquery's OWN WHERE (`(SELECT MAX(id) FROM t2 WHERE CASE WHEN EXISTS(...) THEN 1 ELSE 0 END = 1)`) silently
folded to constant-false — wrong scalar result, *inconsistent with running the same subquery standalone* (which
rejects). The guard is now added to `postBuild` too, so a nested subquery behaves identically standalone or
embedded. Additionally, the outer WHERE-walk error handlers (plan_visitor.go and postBuild) swallowed a structured
`*api.Error` raised by a nested `BuildExists`/`BuildScalar` into the text-fallback path — reporting the generic
"Cascades planner could not plan query" instead of the precise reason. They now propagate an `*api.Error` from the
walk verbatim (after the specific remappings, so the correlated-EXISTS→`ErrCodeUndefinedColumn` fallback still
takes precedence) — a deliberate, already-classified rejection surfaces as-is.

Regression pins: `projected_exists_round13_fdb_test.go` — the round-13 query and variants PLAN and return correct
rows (scalar-subquery WHERE-EXISTS in projection; scalar-subquery-EXISTS alongside a top-level projected EXISTS in
the same SELECT; derived-table WHERE-EXISTS), the buried-CASE-EXISTS-in-a-scalar-subquery variant rejects in its
OWN context with the SAME message as the standalone run (asserted side-by-side; also pinned for the derived-table
and EXISTS-subquery forms via the unified `postBuild` guard + the `*api.Error` propagation), and CONTROLS prove the
genuine round-12 OUTER-level rejections still fire (outer `CASE WHEN EXISTS`, `WHERE NOT (NOT EXISTS)`, outer
buried-scalar WHERE-EXISTS). Detector-level unit pins in `where_exists_position_test.go`
(`TestContainsExistsAtom_SubqueryBoundary` + nested-subquery cases in `TestWhereExistsInScalarPosition`). Full
sqldriver bazel suite + yamsql + plandiff + `pkg/recordlayer/query/...` + `pkg/relational/core/...` green; EXISTS
suite 10× deterministic; all round-1..12 + WHERE/NOT-EXISTS tests still green.
