# RFC-087 — Go-only scalar string/math functions (Cascades values path)

**Status:** Implemented — design ACK'd (v7) by **Graefe + Torvalds + bradfitz**;
impl + audit corrections below; full non-stress suite green (49/49). Awaiting impl
re-ACK (Graefe/Torvalds) + @claude on the PR. Binding completion gate (Graefe v7): beyond
green build + green plandiff/sqldriver/cascades/yamsql + ~40 corpus re-points, also
**(1) audit EVERY residual panic reachable from a per-row `Evaluate`/`Eval` path and
prove each is truly programmer-invariant (unreachable from user data)** — the compiler
does NOT flag a data-dependent panic wrongly left unconverted, so it could slip past
green suites (the v4/v5/v6 crash class relocated); **(2) pin the SWALLOW axis** — a
decline-to-fold test for `EvaluateConstant`, `tryCastConstant`, AND `rule_simplify`
(`WHERE 5='abc'`) proving each leaves the node / doesn't crash, not just the propagate
edges. — Draft v7 history: direction ACK'd by all (bradfitz, Torvalds, Graefe agree:
convert both eval interfaces to `(…, error)`, whole runtime typed-error family,
collapse recovers). The v4/v5/v6 NAKs were each the SAME class: an unenumerated
*consumer* of the interface being converted (filter recover, then predicate path,
then `passesJoinPredicates`/`nljCursor`, then `rule_simplify`). **That enumeration is
the compiler's job, not the RFC's** (bradfitz: the signature change makes every
consumer a build error until handled — mechanical and verified). v7 stops
pre-listing every mechanical call site and instead pins (a) the SEMANTIC decisions a
human must make and (b) the green-build/green-suites completion gate. Known consumers
of `QueryPredicate.Eval`/`Comparison.Eval` (the compiler will surface any more):
filter `pred` closures (executor.go:704/731/842, executor_new_plans.go:335) +
`filterResultCursor.pred` field; **`passesJoinPredicates` (executor.go:1642/:1653)
→ thread `(bool,error)` out through `nljCursor.OnNext` (streaming_cursors.go:846/878,
which has NO recover)**; **`rule_simplify.go:381` `ComparisonConstantSimplifyRule` —
plan-time, void: on type-mismatch error SWALLOW → don't-fold (not propagate); pin
with a both-constant test (`WHERE 5='abc'`)**. Counts: **11** `QueryPredicate` impls
(+`Comparison.Eval`); 39 `Value` impls. Reviewers: Graefe (Cascades), Torvalds
(code), bradfitz (Go idioms), @claude.
**Driver:** un-skip `TestYamsqlConformance` (line-54 follow-up); ~34 of 85 failing
scenarios use scalar functions Go rejects. **User-directed** Go-only read-side
extension; **user chose the full error-channel path.**
**Framing:** Reverses the scalar-function half of swingshift-64 (`802a33d3`,
"Java-aligned rejection"). These are read-side scalar string/math functions —
**zero wire impact**; Java's `BuiltInFunctionCatalog` has no entry → net-new in Go,
allowed per CLAUDE.md ("read-side surface MAY go beyond Java… provided wire compat
holds + deep tests").

## v1 was wrong (Graefe): the embedded layer is dead for this

`SELECT UPPER(col)` is rejected at **plan time**, not runtime:
`planSelectCascades` → `query.FindUnsupportedFunction(logicalOp)`
(`cascades_generator.go:279`) walks the `LogicalProject`, finds
`ScalarFunctionValue{UPPER}`, calls `IsCascadesSafeScalarFunction("UPPER")`
(`values.go:841`) → false → `42883 Unsupported operator UPPER`. The embedded ANTLR
walker (`scalar_functions.go:216`) is reached only for INFORMATION_SCHEMA / explain.
**Restoring the deleted `scalar_functions.go` arms (v1's plan) has zero effect.**
The fix lives entirely in the Cascades `values` path — the single pipeline.

## Fix (Cascades values path)

1. **Open the plan-time gate.** Add the family to `IsCascadesSafeScalarFunction`
   (`values.go:841`): STRING — UPPER, LOWER, LENGTH/LEN/CHAR_LENGTH/CHARACTER_LENGTH,
   OCTET_LENGTH, SUBSTRING/SUBSTR, TRIM/LTRIM/RTRIM, CONCAT/CONCAT_WS, REPLACE,
   LEFT, RIGHT, POSITION, REVERSE; MATH — ABS, MOD, FLOOR, CEIL/CEILING, ROUND,
   SQRT, POWER/POW, SIGN, PI, EXP, LN, LOG. Full coherent family (Graefe Q2 ACK).
2. **Complete the runtime seed** `evalScalarFunction` (`values.go:893`) for every
   gated name — happy-path semantics already exist for most; fill gaps (CONCAT,
   TRIM, LEFT/RIGHT, POSITION, REVERSE, EXP/LN/LOG) porting the verbatim semantics
   from `802a33d3^:scalar_functions.go` (helpers `functions.ToFloat64`/`ToIntegerArg`/
   `CompareValues` all still exist).
3. **Error return — the proper Go design (CLAUDE.md #4: never panic in library code).**
   Today `evalScalarFunction` "declines to nil" on edge cases — a latent wrong-NULL
   bug — and the only way to raise typed runtime errors is **panicking** them (a #4
   violation), recovered at boundaries. Fix it properly, and — per Graefe — move the
   **entire runtime (data-dependent) typed-error family together**; a half-conversion
   leaves a panic that escapes a now-dropped `recover` → uncaught per-row crash.
   - **Change the interface:** `Value.Evaluate(evalCtx any) any` →
     `Evaluate(evalCtx any) (any, error)` across all **39** implementations (38 in the
     `values` pkg + `predicateValue.Evaluate`, `core/query/expr/walk.go:580`). Interface
     at `values.go:125`. Most are mechanical (`return existing, nil`); compile errors
     enumerate every one (no silent `_ = …Evaluate` discards exist — grep-verified).
   - **Convert the whole runtime typed-error family to RETURNS** (not just arithmetic):
     `ArithmeticOverflowError` (→22003), `ArithmeticDivisionByZeroError` (→22012),
     `InvalidCastError` (CastValue, →22018), `ScalarTypeMismatchError` (Arithmetic
     1685/1692, ScalarFunction 1295), and `predicates.TypeMismatchError`. ~22 panic
     sites. `ScalarFunctionValue.Evaluate` adds a new `&InvalidArgumentError{}`
     (SQRT<0 → 22023). **Programmer-invariant panics STAY** (not data-dependent):
     nil child (`NewPromoteValue:2226`), `COUNT(*)` misuse (`:2425/2428`),
     `AggregateValue` "must go through aggregator", `WithNullability` ANY.
   - **Convert the predicate interface too (Graefe v5 — the 3 filter recovers depend
     on it).** `predicates.TypeMismatchError` is panicked from `Comparison.Eval`
     (`comparisons.go:404/:462`), reached via `QueryPredicate.Eval(any) TriBool` — a
     SEPARATE interface (`predicates.go:197`, 9 impls). Convert
     `QueryPredicate.Eval → (TriBool, error)` (+ `Comparison.Eval`/`EvalAgainst`,
     `ComparisonPredicate.Eval`), and thread the error through the filter `pred`
     closures (`executor.go:704/731/842`, `executor_new_plans.go:335`) +
     the `filterResultCursor.pred` field (`func(QueryResult) bool` →
     `func(QueryResult) (bool, error)`). Without this, `WHERE numcol = 'abc'` panics
     and the dropped filter recover crashes the goroutine — the v4 failure mode
     relocated to the predicate path.
   - **Collapse ALL 6 recover sites to `if err != nil`** (Torvalds — v4 listed only 3):
     `executor.go:733`, `:917`, **`:2481`** (`filterResultCursor.OnNext`),
     `executor_new_plans.go:337`, `values.go:416` (`EvaluateConstant` — kills the
     constant-fold swallow Graefe flagged), **`simplifier_value.go:218`**
     (`tryCastConstant`, recovers cast/arith from `cast.Evaluate(nil)` at :228).
     All 6 collapse only once BOTH `Value.Evaluate` AND `QueryPredicate.Eval` return
     errors (the projection/constant-fold sites need the former, the 3 filter sites
     the latter).
   - The `errors.As`→`api.Error` conversion (`cascades_generator.go:1144`) is unchanged
     (the structs already satisfy it).
   Net (bradfitz): the panic/recover dance dies, the hot path loses its per-row
   `defer recover()` (faster), and `(nil,nil)`=SQL-NULL vs `(nil,err)`=runtime-error is
   finally unambiguous — the real correctness win.

## Residual-panic audit (Graefe v7 binding gate — done up front)

The compiler enforces *consumer* completeness, NOT the stays-a-panic split. Audited
all **46** panics in `values/` + `predicates/` (per-row `Evaluate`/`Eval`-reachable):
- **CONVERT → error (22, data-dependent):** the typed runtime-error family
  `ArithmeticOverflowError` / `ArithmeticDivisionByZeroError` / `InvalidCastError` /
  `ScalarTypeMismatchError` / `predicates.TypeMismatchError` (+ new `InvalidArgumentError`
  for SQRT<0). These are exactly the family this RFC moves to returns.
- **STAY (23, programmer-invariant — proven unreachable from user query data):** all
  string-literal construction/type-system invariants (`NewAggregateValue COUNT(*)`,
  `NewPromoteValue` nil/Unknown, `New{Record,Enum,Primitive}Type` dup/structured,
  `WithNullability` RelationType/NONE/ANY, `QuantifiedObjectValue` zero-corr,
  `ComparisonRange` equality/inequality API-misuse) + the non-evaluable planner
  placeholders that should never reach per-row eval (`DerivedValue` / `ExistsValue` /
  `IndexedValue` / `ThrowsValue` / `UnmatchedAggregateValue` / `AggregateValue.Evaluate`
  must-go-through-aggregator) + the two `unhandled Value type %T` rewriter guards
  (`replace.go:356`, `map_field_values.go:467`) + the `tryCastConstant` default
  re-panic. None is reachable from user data — each is a code/planner bug.
- **RESOLVED — STAYS (1):** `comparisons.go:425` — `DistanceRank … reached row
  evaluation`. K-NN *is* SQL-expressible (`QUALIFY ROW_NUMBER() OVER (… ORDER BY
  <distance>) ≤ K`), BUT `logical_qualify.go` (three-state return, lines 19-26) fails
  **loud at build time** if the K-NN can't be lowered to a vector index — the query
  never reaches execution un-lowered. So a `DistanceRank` in an *executable* plan is
  always index-lowered; reaching the per-row panic means the lowering escaped =
  planner bug, not user data → programmer-invariant, STAYS. Proof rests on
  `logical_qualify.go`'s build-time rejection being complete; **pin it** with a
  regression test that an un-lowerable K-NN fails at PLAN time, not row eval.

Audit conclusion: **22 convert, 24 stay, 0 unresolved** — the stays-a-panic split is
fully classified; the implementation can `grep panic\(` the two packages post-change
and confirm exactly the 24 remain.

### Implementation audit corrections (empirical, found by running all suites)

The static audit above mis-classified one site, caught by the A3 corpus run:
- **`AggregateValue.Evaluate` — CONVERT, not stay.** Listed as "must go through
  aggregator → programmer-invariant," but `WHERE COUNT(*) > 0` (A3 corpus
  `agg_in_where_rejected`) reaches it on the per-row scalar path: it IS reachable
  from user data (Go's planner doesn't yet reject aggregate-in-scalar-context like
  Java does at plan time). Converted to a new `AggregateEvalError` → SQLSTATE 42803.
  Follow-up (separate from RFC-087): reject aggregate-in-WHERE at PLAN time to match
  Java exactly (TODO).
- **Executor merge/sort-key panics (5 sites: `intersectionCompKeyFunc`,
  `multiIntersectionCompKeyFunc`, `mergeSortCursor.isBetter`/`extractKey`,
  executor.go:1391) — STAY, pre-existing.** These `ComparisonKeyFunc`s have no
  error channel and NEVER had a recover; before the refactor the inner `Evaluate`
  would have panicked the same typed error here, so the explicit `panic(err)`
  preserves prior behavior exactly (no regression). Their keys are index /
  pre-projected field references (computed sort/merge expressions are projected to
  a column upstream, where the error propagates). The one reachable computed-key
  path — the MAIN sort cursor (`executor.go:2893` `sortFn`) — correctly threads the
  error via `sortErr` (pinned: `ORDER BY <overflow>` → 22003, not a crash).
  Threading `ComparisonKeyFunc` itself (ripples into wire-adjacent `merge_cursor.go`)
  is a separate, pre-existing-scope follow-up; flagged for Graefe.
- **`EXP(1000)` Inf-guard** was dropped for EXP only in the Phase D port (returned
  +Inf instead of NULL); restored to mirror POWER/SQRT. Pinned.

**Swallow-axis tests (Graefe v7 gate):** pin decline-to-fold for `EvaluateConstant`,
`tryCastConstant`, AND `rule_simplify` — each must leave the node / not crash on a
type-mismatch (mirror `WHERE 5='abc'` for all three), not only the propagate edges.

## Generics? No — `any` is correct here

A `Value` tree is heterogeneous and runtime-typed: `ABS(longCol) + 1.5` stores a
LONG-returning child and a DOUBLE literal under one `+` node in one `[]Value`; a
result type comes from the column/CAST/data, not compile time. A generic `Value[T]`
fixes `T` at compile time and breaks `[]Value` homogeneity — you'd erase back to `any`
at every tree edge. This mirrors Java (`Value.eval()→Object`, type via
`Typed.getResultType()`); Go's faithful analog is `Evaluate()→any` + `Type()`. The
only idiomatic cleanup is making `evalCtx` a small capability interface (the
`CorrelationBinder`/`ParameterBinder` seam already exists) — **deferred, orthogonal**.
bradfitz confirmed: `Value[T]` is a dead end; a sealed `Datum` interface adds
exhaustiveness but **no perf win** (still an interface box, same heap alloc as `any`);
the only alloc-free win is a concrete tagged-union value type — a large separate
refactor to revisit *only if a profile shows per-row boxing cost*. Ship `(any,error)`.

## Blast radius — derived from the tree (Torvalds: v2 counts were both wrong)

Restoring makes Go return rows where Java rejects → every rejection-parity pin flips.
ALL must be re-pointed BEFORE merge (else CI red across plandiff + sqldriver +
cascades, which the yamsql run never touches). Counts derived by grep, not estimated;
the *authoritative* set is "whatever flips when the gate opens" — driven by running
each suite post-impl and re-pointing exactly the failures (no hand-waved count):

- **plandiff `corpus.go`** — `grep -c '_rejected'` = **83** total; **~40 flip** (the
  rest are non-scalar `cast_*`/`insert_*`/`date_literal_rejected`, or already in the
  divmap). The flippers are the plain (no-`Divergence`) entries whose Query calls a
  gated fn: ABS×8, LOWER×7, UPPER×6, SUBSTR×4, CONCAT×4, MOD×3, LENGTH×3,
  SUBSTRING/POWER/FLOOR×1 (the T_CSL_/T_UCL_/T_AMM_ block is *within* this set, not
  additive). Each → **JavaErrorsGoCorrect** (`corpus_rfc082_divergences.go`, shape
  `{Direction, Reason, GoExpectedRows}`) with `GoExpectedRows` (Graefe Q3 bucket).
- **Go unit tests** — `grep -c expectRejectionOrCascadesError embedded_fdb_test.go`
  = **33** assertions across **~13 test funcs**, including table-driven `opName` loops
  (≈3168/3211/3518/5465/6070/6111/6166: CONCAT/SUBSTRING/REPLACE/LTRIM/RTRIM/REVERSE/
  SQRT/POWER) the v2 RFC missed; `:6514` is a comment, not an assertion. swingshift-64's
  own log says it converted "17+6+4" sqldriver tests — that is the real scale. Flip
  each gated-fn assertion to expected rows.
- **yaml** — **7** rejection-pin files (`string_functions`, `trim_concat`,
  `scalar_subquery_types`, `error_code_regression` entries, …) reverted to their
  `802a33d3^` row-expecting form.

The merge gate is **green plandiff + sqldriver + cascades + yamsql**, not a count —
the count is documented for review scope, but correctness is "run all four suites,
re-point every flip, zero red."

## Performance / wire

Per-row scalar eval; no planning, no wire change. Plan-time gate is a map lookup.

## Test plan

- New `*_fdb_test.go` (sqldriver, CI): each restored function over a column arg →
  correct rows; NULL-arg → NULL; **error edges** ABS `MinInt64`→22003, MOD/0→22012,
  SQRT<0→22023 (proves the error channel, not decline-to-nil).
- Un-skipped `TestYamsqlConformance` before/after: the ~34 scalar scenarios flip
  green; **no previously-passing scenario regresses** (the 137+7+7 re-points cover
  the flips).
- `just test` green across plandiff + sqldriver + cascades + yamsql + A3.

## Out of scope

- The other ~51 yamsql failures (row mismatches, 0A000 features, error codes,
  comma-join `D.DNAME`) — separate buckets.
- The `t.Skip` removal — lands only when all 85 are green.
