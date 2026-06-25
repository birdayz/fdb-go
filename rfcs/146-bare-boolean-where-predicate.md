# RFC-146 — Bare boolean column as a single-table top-level WHERE predicate

**Status:** Draft (v2 — Graefe NAK'd v1's bare-`ValuePredicate` lift; v2 lifts to the comparison form
`value = TRUE` so `flag` and `flag = TRUE` unify for index matching, adds the explicit NULL fold, and adds
a sargability regression test. See §2 "Why the comparison form matters".)
**Item:** TODO.md "Known gaps" — `WHERE flag` does not plan (surfaced by RFC-144 §3d, 2026-06-23).
**Reviewers:** **Graefe** (Cascades/translator alignment — REQUIRED, query-engine change) + Torvalds
(code/test quality) + codex + @claude.
**Classification:** read-side query-surface **parity** (Java 4.12 supports `WHERE flag`). No wire-format
impact — pure planning-pipeline predicate lifting.

---

## 1. Problem (verified real)

A bare boolean column used as a single-table top-level `WHERE` predicate fails to plan:

```
SELECT id FROM a WHERE flag        => 0AF00 "Cascades planner could not plan query"
SELECT id FROM a WHERE flag = TRUE => OK  Project([ID], PredicatesFilter(Scan(A), [1 pred]))
SELECT id FROM a WHERE flag IS TRUE=> OK  Project([ID], PredicatesFilter(Scan(A), [1 pred]))
SELECT id FROM a WHERE NOT flag    => OK  Project([ID], PredicatesFilter(Scan(A), [1 pred]))
```

This was reproduced against the real planner (no FDB) via `PlanQueryForTest` on schema
`A(id BIGINT NOT NULL, flag BOOLEAN, PRIMARY KEY(id))`.

The parser/resolver are **not** the problem — they correctly lift `flag` to a bare `ValuePredicate`
(`pkg/relational/core/query/expr/walk.go:1320-1334`, final line `return predicates.NewValuePredicate(v)`;
`TestWalkPredicate_BareBooleanColumn` passes). The **same** `ValuePredicate(FieldValue)` shape plans
fine in a JOIN ON clause (`SELECT a.id, b.name FROM a LEFT JOIN b ON a.flag` → green in
`TestFDB_OuterParity_BooleanOn`). So the gap is specific to the single-table WHERE path.

Java 4.12 supports `WHERE flag`: this is a real **parity gap**, not a Go-only extension.

## 2. Investigation (Java spec ↔ Go infra)

### Exact bail-out point — the translator, not the implement leg

The TODO hypothesised the gap was in the implement/data-access leg (a top-level non-comparison
`ValuePredicate` not getting materialized as `RecordQueryPredicatesFilterPlan`). **That hypothesis is
wrong.** The query never reaches the implement leg — it is short-circuited in the translator:

- `pkg/relational/core/query/cascades_translator.go:1687-1689` — `translateFilter`:
  `if f.Predicate != nil && isBareFieldPredicate(f.Predicate) { return nil }`
- helper `isBareFieldPredicate` (`cascades_translator.go:2867-2874`) — true for any
  `*predicates.ValuePredicate` whose `.Value` is a `*values.FieldValue`.

`translateFilter` returning `nil` makes the translation yield a nil `ref` →
`cascades_generator.go:350-353` → `0AF00 "Cascades planner could not plan query"`. The guard was added
in commit `85d0dd9f2` (dayshift-76) as a conservative bail. The implement leg is already capable:
`ImplementSimpleSelectRule.OnMatch` (`rule_implement_simple_select.go:58-122`) builds a
`RecordQueryPredicatesFilterPlan` from any non-tautology predicate, including a top-level bare
`ValuePredicate` — **proven** by deleting the guard in a throwaway worktree: the query then plans to
exactly `Project([ID], PredicatesFilter(Scan(A), [1 pred]))`.

### Why ON works and WHERE doesn't

Both paths build the identical predicate via `Resolver.walkPredicatedExpression`. The WHERE path
(`logical_predicate.go:111/142` → `LogicalFilter` → `translateFilter`) hits the `isBareFieldPredicate`
guard; the ON path (`logical_predicate.go:487`) threads the predicate through join translation, which
never calls `isBareFieldPredicate`, landing it as a residual (`NestedLoopJoin(LEFT OUTER, [1 pred], …)`).
`WHERE NOT flag` plans because the top node is a `NotPredicate`, not a bare `ValuePredicate`.

### Java spec — the single lift point

`fdb-relational-core/.../query/Expression.java:371-399`, `Utils.toUnderlyingPredicate`, applied during
**expression building / semantic analysis** (not a planner rule):

```java
// :377  value instanceof BooleanValue → booleanValue.toQueryPredicate(...)   (AND/OR/NOT/cmp self-convert)
// :384  value instanceof NullValue    → ConstantPredicate.NULL;
// :389  Assert.thatUnchecked(value.getResultType().getTypeCode() == Type.TypeCode.BOOLEAN,
//                            ErrorCode.DATATYPE_MISMATCH, ...);             // non-boolean rejected here
// :394  value instanceof LiteralValue → ConstantPredicate.of((Boolean)…);
// :399  return new ValuePredicate(value, new Comparisons.SimpleComparison(Comparisons.Type.EQUALS, true));
```

Three facts matter: (1) a bare boolean column lifts to `ValuePredicate(value, EQUALS TRUE)` — a
**comparison**, *identical* to what `flag = TRUE` produces (Java unifies the two; `ValuePredicate` in Java
always carries a comparison, `ValuePredicate.java:78`); (2) `NullValue` → `ConstantPredicate.NULL` and a
boolean `LiteralValue` → `ConstantPredicate`, both *before* the type assertion; (3) a **non-boolean** bare
value is rejected with `DATATYPE_MISMATCH` (`ErrorCode.java:121` → SQLSTATE `42804`).

**Why the comparison form matters (Graefe v2 NAK):** Go must lift to the SAME `ComparisonPredicate` that
`flag = TRUE` yields, NOT a bare `ValuePredicate(value)`. Go's index matcher binds only
`*ComparisonPredicate` (`rule_match_intermediate.go:412`); a bare `ValuePredicate(FieldValue)` never binds a
candidate placeholder, and nothing rewrites it into a comparison (`ValuePredicateConstantFoldRule` folds
only constant true/false/null, `rule_value_predicate_fold.go:54-57`). So a bare-`ValuePredicate` lift would
plan `WHERE flag` as a **full scan + residual filter** while `WHERE flag = TRUE` gets the boolean index —
a latent data-access divergence from Java (which plans both as index scan `[TRUE]`), invisible on an
index-less test schema. Lifting to the comparison form unifies the two, reuses the proven sargable
machinery, and renders the `isBareFieldPredicate` guard unreachable (so its removal is pure cleanup, not
the load-bearing fix).

## 3. Fix — mirror Java's `toUnderlyingPredicate` branch order, lifting to the comparison form

At `expr/walk.go:walkPredicatedExpression` (the `return predicates.NewValuePredicate(v)` leaf, line 1334),
replace the bare-value lift with Java's branch order (`Expression.java:384-399`), shared by WHERE and ON:

1. **concrete boolean literal** (`*values.BooleanValue` with non-nil `Value`) → `ConstantPredicate(TriTrue/
   TriFalse)` — **already present** (`walk.go:1328-1333`; = Java `:393-396`).
2. **`NullValue` → `ConstantPredicate(TriUnknown)`** — **add explicitly** (= Java `:384`). Today a bare
   `WHERE NULL` free-rides on `ValuePredicateConstantFoldRule(case nil)`; the comparison-form lift bypasses
   that rule (it matches only `*ValuePredicate`), so NULL must be folded here or `WHERE NULL` regresses.
3. **type gate** (= Java `:389`) — if `v.Type().Code()` is a *definitive* non-boolean (not
   BOOLEAN/NULL/UNKNOWN) → typed `DATATYPE_MISMATCH` (`api.ErrCodeDatatypeMismatch = "42804"`,
   `errcode.go:98`). Covers WHERE and ON (shared `WalkPredicate`).
4. **else → the comparison form** (= Java `:399`): `predicates.NewComparisonPredicate(v,
   predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewBooleanValue(true)})` —
   byte-for-byte what `ResolveComparison` builds for `flag = TRUE` (`expr.go:314`), so plan shape, EXPLAIN
   text, semantic hash, and **index matching** all unify with the comparison path.

Then **remove the now-unreachable guard** — delete `isBareFieldPredicate` (`cascades_translator.go:1687-
1689`) + the helper (`:2867`): no column reference produces a bare `ValuePredicate` any more, so the guard
is dead code (cleanup, not the fix). And **propagate the type error hard** — `buildWherePredicateForTable`
swallows a `WalkPredicate` error into a soft `(nil,false)` (`embedded/logical_predicate.go:112-114`) →
`0AF00`; the `DATATYPE_MISMATCH` must surface as a hard `api.Error` so the user sees `42804`.

Naively deleting the guard while keeping a bare `ValuePredicate` would (a) leave the index divergence of §2
AND (b) let `WHERE amount` plan and silently null-out instead of raising `42804` (worktree-confirmed:
`WHERE amount` / `WHERE name` plan today; the same latent hole exists on the ON side, `ON a.amount`). The
comparison-form lift + type gate close both.

## 4. Wire / behaviour impact

None on the wire (read-side planning only). Behaviour changes: `WHERE flag` now plans and returns the TRUE
rows (FALSE/NULL dropped) instead of `0AF00`; `WHERE <non-boolean>` now returns `42804` instead of `0AF00`
(closer to Java). Because `flag` lifts to the **same** `ComparisonPredicate` as `flag = TRUE`, the two
produce the **identical plan** — including, on a schema with a `VALUE INDEX` on the boolean column, an
index scan `[TRUE]` rather than a full scan + residual (the divergence §2 warns about). Metadata is
untouched. NULL/literal bare predicates stay correct: the new gate allows BOOLEAN **and** NULL/UNKNOWN
through and only rejects definitively-typed non-boolean values (concrete `BooleanValue` TRUE/FALSE fold to
`ConstantPredicate` at `walk.go:1328-1333`; `WHERE NULL` is folded explicitly per §3 step 2).

## 5. Test plan (e2e)

- **Plan-shape (non-FDB, fast)** — `plan_harness_test.go`: `SELECT id FROM a WHERE flag` over a BOOLEAN
  column asserts `PredicatesFilter(Scan(A))`. Negative cases `WHERE amount` / `WHERE name` assert
  `*api.Error` with `ErrCodeDatatypeMismatch` (42804).
- **FDB rows** — flip `TestFDB_OuterParity_BooleanWhere`
  (`pkg/relational/sqldriver/outer_join_parity_fdb_test.go:744-775`): drop the
  "documented divergence" note; assert `SELECT id FROM a WHERE flag` yields exactly `[1]` (`setupBoolDB`
  inserts `a=(1,true),(2,false),(3,null)` — identical to the green `WHERE flag = TRUE`). Add a
  `WHERE NOT flag` row assertion to pin the already-green path stays green.
- **Corpus flip** — `pkg/relational/conformance/plandiff/corpus_rfc082_divergences.go:41` entry
  `bare_bool_where_rejected` is currently `DivergenceJavaSucceedsGoRejects`; after the fix it becomes
  parity (both engines plan it) → reclassify/remove. Run the full `embedded` + `sqldriver` + `plandiff`
  suites; any test asserting `WHERE <nonbool>` expecting `0AF00` updates to `42804`.
- **Sargability (Graefe v2 — the load-bearing regression guard)** — an FDB test on a schema with a
  `VALUE INDEX` on the boolean column: `EXPLAIN SELECT … WHERE flag` must show the **same index scan** as
  `EXPLAIN SELECT … WHERE flag = TRUE` (e.g. both `plan_contains` the index-scan over `[TRUE]`, neither a
  full scan). This is the test that pins the unification — without it the bare-vs-comparison divergence
  stays unprobed (the "dimensional gap" failure mode). Also pin `WHERE flag` and `WHERE flag = TRUE`
  return the identical row set.

## 6. Gate & risk

**Graefe ACK required on RFC + impl** (translator predicate-lifting is query-engine surface) + Torvalds +
codex + @claude. Risk is low and contained: the guard only ever fired on a top-level
`ValuePredicate{FieldValue}`; comparison/AND/OR/NOT/EXISTS predicates never matched it. Two implementation
subtleties: (a) hard-error propagation (§3) — without it `WHERE <nonbool>` stays at `0AF00` instead of
`42804`; (b) the explicit NULL fold (§3 step 2) — without it `WHERE NULL` regresses once the lift moves off
the bare `ValuePredicate` that `ValuePredicateConstantFoldRule` matched.

**UNKNOWN-leniency divergence (documented):** Java strictly asserts `== BOOLEAN`; the gate here permits
BOOLEAN **and** NULL/UNKNOWN. This is a deliberate permissive-only divergence — Go's pre-plan type
resolution is less complete than Java's post-semantic-analysis, so Go may *accept* an un-typeable value it
can't prove non-boolean, but it never *rejects* anything Java accepts. Document at the call site.

## 7. Scope

In: the comparison-form lift (Java `:399`) + explicit NULL fold (Java `:384`) + type gate (Java `:389`) at
the single `walk.go` lift point, guard removal (dead-code cleanup), hard error propagation, and the tests
above (incl. the sargability guard). Out: any change to the join-ON path beyond the shared `WalkPredicate`
gate it inherits for free; rewriting existing comparison handling.

> **Doc note:** `cascades_generator.go` and `logical_predicate.go` live under
> `pkg/relational/core/embedded/` (the `0AF00` site + the soft-fallback), not `.../core/query/`.
