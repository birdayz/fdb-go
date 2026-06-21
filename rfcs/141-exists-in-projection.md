# RFC-141 — EXISTS in the projection list (Java 4.12, RFC-135 §4 R4)

**Status:** Draft (v2 — decided one-mechanism + representation-seam phasing after Graefe + Torvalds NAK)
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
7. **Planning/execution (no new rule):** the projected EXISTS carries its existential quantifier into the
   owning `SelectExpression`; `rule_implement_nested_loop_join` already lowers it to the `JoinExists`/
   FlatMap semi-join. The only difference from WHERE is that the boolean (`child.Evaluate() != nil`) is
   **returned** as a column rather than **filtered on** — same machinery, different consumer (Graefe Q3).

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
