# RFC-047: GROUP BY / HAVING in correlated scalar subqueries

**Status:** Implemented
**Item:** TODO.md item 60 ("GROUP BY/HAVING in correlated scalar subqueries")

## Problem

A correlated scalar subquery may carry a `GROUP BY` and/or `HAVING`:

```sql
SELECT c.name,
       (SELECT SUM(o.amount) FROM orders o WHERE o.customer_id = c.id GROUP BY o.customer_id)
FROM customers c;

SELECT c.name,
       (SELECT COUNT(*) FROM orders o WHERE o.customer_id = c.id HAVING COUNT(*) > 1)
FROM customers c;
```

Today both are **hard-rejected** at the logical-build stage with
`correlated scalar subquery: GROUP BY in inner query not yet supported` /
`... HAVING in inner query not yet supported`
(`logical_predicate.go:3940-3949`, `:3969-3973`). The rejection was added in #202
("widen correlated scalar subquery shapes") as a deliberate punt, not because of a
real architectural block.

## Framing: this is a Go-only read-side extension, not Java parity

Correlated scalar subqueries are a **Go-only extension** — Java's `fdb-relational`
rejects them at the grammar/normalizer level entirely (TODO.md line 68:
"Java rejects at grammar level. Implemented via FlatMap with JoinTypeLeftOuter").
So there is **no Java reference** for the SQL-level semantics of `GROUP BY`/`HAVING`
inside one. Per CLAUDE.md ("Wire compat is the hard line; query reach is not"), this
is an *allowed* read-side extension: it touches the read path only (pure SELECT
planning/execution), has **zero wire-format impact** (no key/record/index/continuation
change — Java still reads/writes identical records), and the bar is "deep test coverage,"
not "match Java."

The "FirstOrDefault pattern" that Graefe prescribed (TODO note) is the *record-layer-level*
mechanism (`ForEachNullOnEmpty` quantifier + LEFT-OUTER NULL-on-empty), which Java does have
and which the existing Go correlated-scalar path already uses.

### The TODO's stated blocker is stale

The TODO note says this "Requires PredicatePushDownRule to treat GroupByExpression as a
barrier (AliasMap.Compose conflict on correlation alias)." Both halves are no longer true:

1. **PredicatePushDownRule already treats `GroupByExpression` as a barrier.**
   `pushPredicateToExpression` (`rule_predicate_push_down.go:172-188`) has *no*
   `GroupByExpression` case — it falls to `default → return nil`, so a predicate is never
   pushed below a GroupBy. The barrier exists.
2. **`AliasMap.Compose` (the panicking `combine`) has no production callers.** It was
   replaced by the non-panicking `composeChildAliasPairs` (`expression.go:193`), which
   returns `(nil, false)` on conflict instead of panicking. The crash the rejection
   guarded against can no longer happen.

### The structural machinery already exists and is exercised

The existing no-`GROUP BY` aggregate scalar subquery (`SELECT COUNT(*) … WHERE …`) is
*already* built as `logical.NewAggregate(filter, nil, …)` → a Cascades `GroupByExpression`
with **empty** grouping keys, sitting above the correlation `Filter`
(`logical_predicate.go:3954-3966`, `cascades_translator.go:490-494`). It plans and executes
correctly (`correlated_scalar_subquery_count`, `aggregate_with_join` pass). Adding
**non-empty** grouping keys reuses exactly the same expression, correlation flow, and rules.

## Investigation

**Build path** (`existsSubqueryPlanner.buildCorrelatedScalar`, `logical_predicate.go:3822`):
`Scan → [Join…] → Filter(correlation pred) → Aggregate` for the aggregate case, or
`… → Sort → Limit(1)` for the non-aggregate case. A resolver with `innerScope` chained to
`outerScope` (`:3856-3891`) resolves WHERE against both inner table columns and outer
correlated columns. The result is stashed as `logical.CorrelatedScalarSubquery{Alias,
InnerPlan, InnerAlias, ScalarCol}`.

**Translation** (`translateProjectWithCorrelatedScalar`, `cascades_translator.go:360`):
peels a top `LogicalLimit` and re-attaches it as a `LogicalLimitExpression`, translates the
inner plan, and wraps outer+inner in a **LEFT OUTER** `SelectExpression` with a
`ForEachNullOnEmpty` inner quantifier. NULL-on-empty supplies the scalar "default" when the
inner yields zero rows; `LIMIT 1` supplies "first."

**Regular GROUP BY resolution** (`logical_predicate.go:1938-1986`): group-key Values come
from `resolver.WalkExpressionForProjection(groupByExpr)` → `agg.GroupKeyValues`; the HAVING
predicate from `resolver.WalkPredicate(havingExpr)` → `rewriteAggregateRefsInPredicate` →
`agg.HavingPredicate`. `translateAggregate` (`cascades_translator.go:452-513`) consumes
those: it builds a `GroupByExpression` and, when `HavingPredicate != nil`, a
`LogicalFilterExpression` above it.

## Fix

Remove the two rejections in `buildCorrelatedScalar` and build the inner plan exactly as the
regular path does, then cap with `LIMIT 1` for the scalar contract. **Three shapes:**

First, validate the grouped projection by **calling the existing top-level helper**
`validateGroupByProjection(sq, p.md)` (`logical_predicate.go:2118`) — `buildCorrelatedScalar`
already holds `p.md` and the inner `sq` in scope. It returns `api.ErrCodeGroupingError`
(SQLSTATE `42803`, "column must appear in the GROUP BY clause or be used in an aggregate
function") or `ErrCodeUndefinedColumn` for an unknown column. This is the **same** validation
the regular GROUP BY path runs — not a stringly-typed reimplementation. Wrap a non-nil result
in `CorrelatedExistsError{Cause: err}` so it propagates as the inner subquery's error.

1. **Aggregate + GROUP BY** (`SELECT COUNT(*) … GROUP BY o.status`):
   build `logical.NewAggregate(filter, sq.groupBy, [aggText], [scalarCol], "")`, resolve
   `GroupKeyValues` from `sq.groupByExprs` via the existing `resolver` (mirroring
   `:1938-1951`), then `innerOp = logical.NewLimit(innerOp, 1, 0)`.
2. **Aggregate + HAVING** (`… HAVING COUNT(*) > 1`):
   resolve the HAVING predicate via `resolver.WalkPredicate` + `rewriteAggregateRefsInPredicate`,
   set `agg.HavingPredicate`. `translateAggregate` builds the filter above the GroupBy. Then
   `LIMIT 1`. EXISTS/scalar-subquery **inside** HAVING stays rejected (consistent with the
   top-level path's `translateAggregate` guard at `cascades_translator.go:506`).
3. **Non-aggregate GROUP BY** (`SELECT status … GROUP BY o.status`): an aggregate with zero
   aggregate functions whose only projected column is a grouping key (DISTINCT-of-key).
   `validateGroupByProjection` (above) enforces that the projected column is a grouping key,
   else `42803`. Then `LIMIT 1`.

**Multi-column guard:** an aggregate scalar subquery that also projects a non-aggregate
column (`SELECT status, COUNT(*) … GROUP BY status` → 2 output columns) violates the
one-column scalar rule and is rejected with the existing "must return exactly one column"
error (checked *before* the aggregate is built, so it wins over `42803`).

### Semantics — FirstOrDefault, no runtime cardinality assertion (Graefe directive)

A `GROUP BY` may yield more than one group → more than one row. Per Graefe, we do **not**
add a runtime "exactly one row or error" assertion (it would force reading a second row
before emitting, which is incompatible with continuation-based pagination). Instead we use
the **FirstOrDefault** pattern already in place:

* `LIMIT 1` on the inner ⇒ at most the **first** group's value (read one row, streaming-safe);
* LEFT-OUTER `ForEachNullOnEmpty` ⇒ **NULL** when zero groups survive (e.g. no matching rows,
  or all filtered by HAVING).

This is identical to the existing non-aggregate path (`LIMIT 1`, first-or-default,
nondeterministic without `ORDER BY`) — so the multi-group case returns the first group's
value, not an error. The high-value case (GROUP BY a correlation-determined key, or HAVING
reducing to ≤1 group) is deterministic and exact. The empty-set behavior is *correct SQL*:
`GROUP BY COUNT(*)` over no rows yields NULL (zero groups), whereas the no-`GROUP BY`
`COUNT(*)` yields 0 — both fall out naturally because the scalar-aggregate plan emits one
row even on empty input while the grouped plan emits none.

## Performance

No new operators, no cost-model change, no extra memo work. The inner plan gains a
`GroupByExpression` with real keys (vs empty) plus a `LIMIT 1` — the same machinery the
existing scalar-aggregate and non-aggregate paths already build and plan. `LIMIT 1` *caps*
inner work per outer row. The PredicatePushDownRule barrier is unchanged (already a no-op for
GroupBy). No effect on any non-correlated-scalar query. Stress-1M expected unchanged
(no plan-shape change for existing queries; verified before/after).

## Test plan

FDB integration tests in `quality_probes_test.go` (real FDB, `t.Parallel()`), converting the
three `*_rejected` probes into correctness probes and adding new ones:

* **GROUP BY on correlation-determined key** — `(SELECT SUM(amount) … WHERE o.cid=c.id
  GROUP BY o.cid)`: deterministic, exact per-customer sum; NULL for customers with no orders
  (vs 0 for the no-GROUP-BY COUNT) — pins the empty-set NULL semantics.
* **HAVING reduces to one group** — `(SELECT COUNT(*) … GROUP BY o.cid HAVING COUNT(*) > 1)`:
  customers with >1 order get the count; others get NULL.
* **Non-aggregate GROUP BY (DISTINCT-of-key)** — `(SELECT status … GROUP BY status LIMIT 1)`.
* **EXPLAIN assertion** — the inner plan contains an aggregate/streaming-group plan + LIMIT,
  proving the GroupBy is built (not silently dropped to a fake checkbox).
* **Multi-column still rejected**; **EXISTS-inside-HAVING still rejected** (regression pins).
* **42803** for a non-aggregate non-key projection (`SELECT amount … GROUP BY status`).
* Determinism: 10× on the single-group cases (must be stable).
* Expression GROUP BY key resolves (`GROUP BY o.amount + 1`); an unresolvable
  one (`GROUP BY o.nosuchcol + 1`) **errors** rather than silently grouping
  under a null key.
```

## Known limitations

* **`ORDER BY` combined with `GROUP BY`** in a correlated scalar subquery is
  **rejected** with a clear error, not silently dropped. Ordering the *groups*
  (to make the multi-group FirstOrDefault choice deterministic) would require
  resolving the sort keys against post-aggregation output, which this builder
  does not wire — out of scope here. `ORDER BY` *without* `GROUP BY` (ordering
  rows before the `LIMIT 1`) is fully supported. Follow-up if needed.
* Expression aggregate/group-key arguments that fail to resolve **error**
  (no silent degradation to `SUM(*)` / null-key grouping).
* **Expression/constant-argument aggregate meeting a *differing* aggregate via
  HAVING** is **rejected** fail-safe (Codex catch). Aggregate slots are
  materialised under the *bare* operand name (`FN(*)` for an expression/constant
  arg), but the HAVING rewrite (`rewriteAggregateValue`) looks aggregates up by
  operand *explain* (`COUNT(1)`, `SUM(A*3)`). Sharing a slot across the two
  schemes repeatedly produced silent-wrong results (reverse `COUNT(*)`/`COUNT(1)`,
  `COUNT(DISTINCT 1)`, a HAVING repeating the visible constant aggregate), so the
  stable rule rejects: `SELECT COUNT(1) … HAVING COUNT(*)` (both directions),
  `SELECT SUM(a*2) … HAVING SUM(a*3)`, `COUNT(DISTINCT 1)`. What works (no scheme
  divergence): HAVING on `COUNT(*)`/bare-column aggregates (`SELECT SUM(amount) …
  HAVING COUNT(*) > 1`) and a projected expression aggregate with no differing
  HAVING reference (`SELECT SUM(amount*2) … GROUP BY …`). Closing it = materialise
  names from `rewriteAggregateValue`, keeping a dot-free alias for the single
  visible scalar output. Tracked in TODO.md under item 60.

---

## Addendum: full support for expression/constant-argument aggregates in HAVING (naming alignment)

**Status:** Draft → Implemented (this addendum)

### Problem

The base RFC **rejects** an expression/constant-argument aggregate (`COUNT(1)`,
`SUM(a*2)`) that meets a *differing* aggregate via `HAVING` — fail-safe, never
wrong rows, but it refuses valid SQL. The root cause is a naming mismatch:

* `addAgg` materialises each aggregate's result-map slot under the **bare** name
  `FN(bareArg)` — `COUNT(*)` for a star/constant/expression arg, `SUM(AMOUNT)`
  for a bare column.
* The HAVING-predicate rewrite (`rewriteAggregateValue`) instead references an
  aggregate by `FN(<operand-explain>)` — `COUNT(1)`, `SUM(AMOUNT*3)`.

They coincide for `COUNT(*)` and bare-column aggregates, diverge for
expression/constant args. So a HAVING reference to a divergent aggregate reads a
slot that was never materialised → NULL → drops valid groups. The base RFC
rejects these rather than risk that silent-wrong.

### Scope — single-source inner only; joins keep the base RFC's fail-safe

Torvalds' review surfaced a real hole in a "name everything canonically" approach:
the canonical name (`SUM(AMOUNT*3)`) carries an operator, so if it is used as the
`aggText`, `parseAggregateText` builds an **arithmetic** operand and
`translateAggregate` (`cascades_translator.go:481`) then **refuses to apply** the
resolver-bound `AggregateOperands` (the `!isArith` guard) — the executor evaluates
the *parser's* operand instead. For a **single-source** inner that is harmless
(the parser's bare `FieldValue{AMOUNT}` resolves correctly), but for a **join** the
parser's bare-column atom does not bind to the right quantifier → wrong value / NULL.
Fixing that cleanly means touching shared aggregate-translation code (the `!isArith`
guard) with full top-level-GROUP BY regression coverage — out of scope here.

So this addendum scopes the full support to **single-source** correlated scalar
subqueries (`len(sq.joins) == 0`), which is where expression/constant aggregates in
a `HAVING` realistically occur. A correlated scalar subquery whose inner has a
**join** keeps the base RFC's behaviour unchanged (bare `FN(*)` names; an
expression/constant aggregate meeting a differing `HAVING` aggregate stays rejected
fail-safe). That residual is tracked as a further follow-up.

### Fix (single-source) — materialise under the canonical (HAVING-rewrite) name

For a single-source inner, materialise every aggregate under the **same canonical
name the HAVING rewrite uses**, so a HAVING reference always resolves and distinct
expressions get distinct slots:

1. `canonicalAggName(fn, opVal)` — the **one** shared canonicaliser, extracted from
   `rewriteAggregateValue` and called by both, so they cannot drift:
   `FN(<uppercased ExplainValue(opVal), spaces stripped, one outer-paren pair
   stripped>)`, with `opVal == nil ⇒ "FN(*)"`. Single-source ⇒ `ExplainValue` is
   dot-free, so the canonical name is a safe `scalarCol`.
2. Use it as the aggregate's `aggText` **and** `Alias` (single-source ⇒ dot-free ⇒
   no `replaceScalarSubqueryRef` mis-parse). `finalizeGroup` keys the result map by
   both `aggResultName(spec)` and `spec.Alias` (verified), so the HAVING rewrite
   resolves via the canonical key either direction. `scalarCol` = the visible
   aggregate's canonical name (unique, dot-free).
3. **Dedup by canonical name** (identical func+operand ⇒ one slot; distinct
   expressions ⇒ distinct slots, both materialised).
4. **Drop the fail-safe rejections for the single-source path** (the
   `opaqueExpr`/`exprAggNames` collision guard and the non-visible-expression loop
   guard) — every aggregate now materialises under a HAVING-resolvable name. The
   guards remain on the **join** path.

`COUNT(DISTINCT 1)` stays rejected on both paths (DISTINCT aggregates are
unsupported here and `aggDistinct` is not threaded into the slot). Verify the
canonical aggText still carries `DISTINCT` so `parseAggregateText`'s existing
DISTINCT rejection fires.

### What this turns from "rejected" into "works" (single-source)

`SELECT COUNT(1) … HAVING COUNT(*)` (both directions), `SELECT SUM(a*2) …
HAVING SUM(a*3)`, and a HAVING repeating a visible constant aggregate. The
cases that already worked (bare-column / `COUNT(*)` HAVING, single projected
expression aggregate, `COUNT(*)` reuse) are unchanged — their canonical name
equals their bare name, so the materialised slot and scalarCol are identical to
today. **Join** inners are unchanged from the base RFC (still fail-safe).

### Performance

No new operators, no cost-model change. Same number of aggregate slots (one per
distinct aggregate the query references); naming is a build-time string change.

### Test plan

Flip the base RFC's single-source `*_rejected` probes to **correctness** probes
(deterministic single-group customers): `COUNT(1)` + HAVING `COUNT(*)` both
directions return the count; `SUM(amount*2)` + HAVING `SUM(amount*3)` returns the
sum for groups whose `SUM(amount*3)` passes. **Keep** `COUNT(DISTINCT 1)` rejected,
and add a **join** probe (`… orders o JOIN items i … HAVING SUM(i.price*2) …`) that
asserts the join path *still rejects* fail-safe (the residual). Empirically probe
each new direction against real FDB **before** asserting (this area produced
silent-wrongs three times under Codex), and re-run the full shape suite + `just
test`.
