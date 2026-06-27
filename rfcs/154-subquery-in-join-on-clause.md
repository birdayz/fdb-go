# RFC-154 — Subqueries in JOIN ON clauses: fail-closed for unsupported shapes + EXISTS-in-ON parity

**Status:** Phase 1 DONE (committed). Phase 2a (INNER EXISTS-in-ON) DONE — Graefe DESIGN ACK on §5 obtained;
implementation green. Phase 2b (OUTER EXISTS-in-ON) DEFERRED behind a fail-closed rejection per Graefe (needs the
RFC-153 rebaser-correlation work + a typed-plan/row proof before the gate is lifted). Query-engine: SQL→logical
translation (Phase 1) + cascades translator/NLJ rule (Phase 2a).

**Origin:** Carried-forward bug-hunt item (TODO.md "Known gaps") — a pre-existing materialized-NLJ cross-product
when a JOIN ON clause carries a subquery conjunct. Root-caused this session; user chose "fix + EXISTS-in-ON parity".

## 1. The bug (verified, RED→GREEN)

```sql
SELECT a.id, c.id FROM a JOIN b ON b.a_id=a.id
  LEFT JOIN c ON c.a_id=a.id AND c.w IN (SELECT d.b_id FROM d WHERE d.id=a.id+999)
```

returned the CROSS PRODUCT `(1,50)(1,51)(2,50)(2,51)` instead of the null-extended `(1,NULL)(2,NULL)`. EXPLAIN:
`NestedLoopJoin(LEFT OUTER, FlatMap(B,A), Scan(C))` — **zero predicates**. The same shape with an INNER join, or a
scalar subquery, also dropped the conjunct. Pre-existing (base `15d2ab340` == HEAD), not an RFC-153 regression.

## 2. Root cause (NOT where TODO.md guessed)

TODO.md hypothesized an executor bug (`passesJoinPredicates`). It is actually a **translation** bug.
`upgradeJoinOnPredicates` (`embedded/logical_predicate.go`) upgrades a join's `OnText` → structured `OnPredicate`
via an `expr.Resolver`. That resolver installs **no `SubqueryPlanner`**, so `WalkPredicate` declines any subquery
atom with `UnsupportedExpressionShapeError`. The error was neither a `SourceNotFoundError` nor an `*api.Error`, so
it hit a permissive `continue` that left `OnPredicate == nil`. The cascades translator **ignores `OnText` once
`OnPredicate` is nil** → the join degrades to a CROSS PRODUCT. A classic fail-OPEN gate (the RFC-153 bug-hunt
thesis: a safety gate with a permissive default is a wrong-rows bug waiting for the right query shape).

## 3. Java reference

- **IN-subquery** (`x IN (SELECT…)`): Java rejects everywhere — `ExpressionVisitor.visitInPredicate` asserts
  `inList().queryExpressionBody() == null`, `UNSUPPORTED_QUERY` "IN predicate does not support nested SELECT". Go
  rejects it too (conformance: `correlated_subquery_probes.yaml`, CLAUDE.md #10).
- **Scalar subquery in a comparison**: not in Java's grammar at all (parse error). Go supports *uncorrelated*
  scalar subqueries (pre-evaluated) but **not correlated** ones.
- **EXISTS**: Java SUPPORTS `EXISTS` in an ON clause (`QueryVisitor.visitInnerJoin`/`visitOuterJoin` →
  `ExpressionVisitor.visitExistsExpressionAtom`). Go currently **rejects** it ("EXISTS in a JOIN ON clause is not
  yet supported") — a real Go gap vs Java.

## 4. Phase 1 — fail-closed for shapes Go doesn't support (DONE)

Go (like Java) supports neither IN-subqueries nor correlated scalar subqueries anywhere, so the correct fix is a
clean rejection, never a wrong-rows plan:

- `expr.ContainsSubqueryAtom` (typed-node walk, no GetText): detects a scalar `(SELECT…)` atom or an
  `x IN (SELECT…)` body (an `InListContext` with a `QueryExpressionBody`); `IN (a,b,c)` value lists are NOT
  matched. Mirrors the existing `ContainsExistsAtom`.
- `upgradeJoinOnPredicates`: reject a detected subquery-in-ON with `ErrCodeUnsupportedQuery`, and **fail-CLOSED**
  the `continue` — any resolver failure that can't build the ON predicate now surfaces a clean error rather than
  dropping the join condition.
- Extracted `mapPredicateWalkError`, shared by the WHERE and ON paths, so both classify undefined/ambiguous
  column, unknown source, and IN-shape failures with identical SQLSTATE (e.g. a nonexistent ON column → 42703).

Tests (`subquery_in_on_crossproduct_fdb_test.go`, `logical_predicate_test.go`, `rfc153_joined_preserved_plan_test.go`):
clean 0AF00 for IN/scalar-subquery in ON (LEFT + INNER + sole conjunct); `IN (value,list)` still works; constant /
single-eq controls null-extend; equi-join ON upgrades; nonexistent ON column → 42703. Full `just test` green.

## 5. Phase 2 — EXISTS-in-ON parity (DESIGN — needs Graefe ACK)

Goal: `… JOIN c ON c.a_id=a.id AND EXISTS (SELECT 1 FROM d WHERE d.id=a.id)` plans + executes (Java parity), and
the LEFT-OUTER form null-extends.

### 5.1 Translation
- Add `LogicalJoin.OnExistsSubqueries []ExistsSubquery` (currently no field carries ON-clause subqueries).
- `upgradeJoinOnPredicates`: when `ContainsExistsAtom(onExpr)`, install an `existsSubqueryPlanner` on the ON
  resolver (scoped to the join's source aliases for correlation), `WalkPredicate` the ON (builds an
  `ExistentialValuePredicate` and collects the `esq`), set `OnPredicate` + `OnExistsSubqueries`. Remove the
  "not yet supported" rejection.
- `translateJoin`: when `OnExistsSubqueries` is non-empty, append a `NamedExistentialQuantifier` per esq and its
  `existsInnerCorrelation` join predicate to the SelectExpression — exactly as `translateJoinWithExists` already
  does for EXISTS-in-WHERE-over-a-join. Pass `joinType` through.

### 5.2 Planning — two sub-cases
**INNER (5.2a) — DONE.** `a JOIN c ON cond AND EXISTS(s)` ≡ `a JOIN c ON cond WHERE EXISTS(s)` (no
null-extension). `upgradeJoinOnPredicates` installs an `existsSubqueryPlanner` (scoped via
`buildOuterScopeSources`) on the ON resolver, builds the ON predicate, and carries the collected
`OnExistsSubqueries` on the join; `translateJoin` appends a `NamedExistentialQuantifier` per esq (FLATTENING the ON
AND so the `ExistentialValuePredicate` is a top-level conjunct — required by `CheckBuriedExistentialPredicate` and
the `implementJoinWithExistential` routing). Graefe ACK on the approach. Pinned by FDB row tests
(`exists_in_on_fdb_test.go`): correlated EXISTS + equi-join, NOT EXISTS, sole EXISTS conjunct.

**OUTER (5.2b):** `a LEFT JOIN c ON cond AND EXISTS(s)` where `s` is correlated to the PRESERVED side. The EXISTS
gates the match under null-extension: existsFlag(a) false → no c matches → null-extend; true → LEFT JOIN c ON
cond. `implementJoinWithExistential` builds an INNER join + semi-join ON TOP, which would wrongly DROP preserved
rows whose EXISTS is false instead of null-extending — so it is **not** a valid OUTER implementation.
**Open design question for Graefe:** the correct shape is a FlatMap over the preserved side whose inner is
`if existsFlag(a) then DefaultOnEmpty(c where cond) else single-null-row`. Candidate approaches: (i) route the
preserved-correlated EXISTS into RewriteOuterJoinRule's null-supplying-inner SUBSEL as an additional ON conjunct
(RFC-153-adjacent — the EXISTS becomes part of the rewritten inner that DefaultOnEmpty null-fills); (ii) a
dedicated rule. Pending Graefe ACK; until then, OUTER EXISTS-in-ON stays **rejected fail-closed** (Phase 1
behavior) so it never returns wrong rows.

### 5.3 Execution
INNER reuses the existing FlatMap+FirstOrDefault semi-join cursor. OUTER reuses the RFC-153 null-on-empty
DefaultOnEmpty path if approach (i) is chosen.

### 5.4 Scope guard
EXISTS combined with FULL OUTER is rejected (existing `findFullOuterWithExists`). EXISTS under an OR in ON is
rejected (mirror `existsUnderDisjunction`).

## 6. Test plan (Phase 2)
yamsql + FDB row-count tests: INNER ON-EXISTS (correlated to either leg), LEFT ON-EXISTS correlated to preserved
(null-extension), NOT EXISTS in ON, EXISTS + extra ON conjunct, multi-row inner, determinism. Typed-plan
assertions for the semi-join shape (no EXPLAIN-string matching).

## 7. Wire compatibility
Read-side only. Zero wire-format impact: Java still reads/writes the identical records; this only lets Go *express*
EXISTS-in-ON (which Java also supports). IN-/scalar-subquery-in-ON remain rejected (Java parity).
