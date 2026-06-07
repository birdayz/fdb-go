# RFC-091 — Value/Predicate evaluation returns errors instead of panicking

Part of TODO-production.md **P0.3-A1**. Query-engine change → **Graefe ACK** (Cascades
alignment) + **Torvalds** (code quality) required. Builds on the bradfitz "don't leak
panics" policy and the P0.2 db/sql boundary recover (already landed).

## Problem

The SQL value-evaluation engine signals *user* errors by **panicking**, then relies on a
handful of narrow, gappy `recover()`s in the executor to turn them back into errors:

- `Value.Evaluate(evalCtx any) any` (`values/values.go:125`) has no error channel, so
  `1/0` (`values.go:1717`), arithmetic overflow (`:1700/1706/1712/1720`), CAST failures
  (`:1935`–`2076`), and scalar/IN/comparison type mismatches (`:1295`, `comparisons.go:404/
  462`) `panic` typed errors.
- `QueryPredicate.Eval(ctx) TriBool` (`predicates.go:197`) has the same gap.
- The executor catches these in 4 places (`executor.go:734,918,2505`,
  `executor_new_plans.go:337`) plus 2 fold-time recovers (`values.go:416 EvaluateConstant`,
  `simplifier_value.go:218 tryCastConstant`). The coverage is incomplete — `streaming_cursors.go:218,254`
  and `flat_map_cursor.go:213` call `Evaluate` with **no** recover above them, so a div0 in
  GROUP BY / aggregate / join context crashes the host today (only the P0.2 boundary recover
  now stops a full crash; the result is still a generic 500, not the right SQL error).
- `executor.go:739` / `executor_new_plans.go:337` are worse than incomplete: on an
  *unexpected* panic they set `keep=false`, **silently dropping the row** (the projection
  path at `:929` errors instead — inconsistent, and a silent-wrong-results bug).

## Constraint

- Java is the reference: `Value.eval()` throws `RecordCoreException`, propagated through the
  `RecordCursor` pipeline to the executor/JDBC seam. Go has no exceptions, so the faithful
  port is `(T, error)` — not panic/recover. Precedent in-repo: `KeyExpression.Evaluate`
  already returns `([][]any, error)`.
- The typed errors (`ArithmeticOverflowError`, `ArithmeticDivisionByZeroError`,
  `ScalarTypeMismatchError`, `InvalidCastError`, `TypeMismatchError`) and the SQLSTATE
  mapping (`translateExecError`, `cascades_generator.go:1135`) **already exist** — this is
  plumbing existing errors through returns, not a new taxonomy.
- Genuine "can't happen" invariant asserts stay panics (bradfitz policy): do NOT thread
  errors through the ~134 internal invariant sites. Per the audit, only ~22 eval panics are
  user-reachable.

All cited paths: `pkg/recordlayer/query/plan/cascades/{values,predicates}/` and the
executor at `pkg/recordlayer/query/executor/` (NOT `pkg/relational/sqldriver`;
`comparisons.go` lives in `predicates/`, not `values/`).

## Fix

Change the two evaluation interfaces to return errors, then delete the now-redundant
control-flow recovers:
- `Value.Evaluate(evalCtx any) any` → `(any, error)` — ~63 impls, ~125 non-test call sites
  (~500 incl. tests). Convert the user-reachable panics to `return …, &TypedErr{}`; keep
  genuine constructor/invariant asserts (`NewPromoteValue`, `AggregateValue.Evaluate`
  "must be evaluated over rows", `DistanceRank` "reached row evaluation") as panics.
- `QueryPredicate.Eval(ctx) TriBool` → `(TriBool, error)` — ~12 impls, ~8 call sites.

### Migration mechanism — transitional method (Graefe #2 / Torvalds #1)

A direct interface-signature flip is **atomic** across all impls + every caller, so
"per-package compilable commits" is impossible. Instead:
1. Add `EvaluateErr(evalCtx any) (any, error)` to the `Value` interface (and `EvalErr` to
   `QueryPredicate`), implemented on all impls with the real error-returning logic; the old
   `Evaluate(ctx) any` becomes a thin wrapper `v, err := x.EvaluateErr(ctx); if err != nil {
   panic(err) }; return v` — old behavior preserved, builds green.
2. Migrate call sites **per package** (`values/` → `predicates/` → `executor/`) from
   `Evaluate`→`EvaluateErr`, threading the error. Each commit compiles + is bisectable.
3. Collapse: delete the old `Evaluate`/`Eval` wrappers, rename `EvaluateErr`→`Evaluate` (end
   state is the canonical name returning `(any, error)`).

### The constant-fold paths are a real bug, not plumbing (Graefe #1 / Torvalds #3)

`EvaluateConstant` (values.go:416) currently maps typed eval errors → `(nil, ok=true)`, so a
constant `1/0`/overflow **folds to NULL at plan time** (→ `LiteralValue(nil)` /
`TriUnknown`) — the error is *swallowed*, diverging from the runtime path (→22012). Its
sibling `tryCastConstant` (simplifier_value.go:217) does the opposite (error → decline to
fold → surfaces at runtime). **Unify on decline-to-fold:** both call `EvaluateErr`; on ANY
error, do not fold. The recovers are then removed cleanly — a genuine *invariant* panic
during plan-time folding is backstopped by the P0.2 db/sql boundary recover (folding runs
under `QueryContext`→`gen.Plan`), so no defense-in-depth net is lost. Pin `SELECT 1/0` AND
`WHERE 1/0` (+ constant overflow / invalid-cast) end-to-end → correct SQLSTATE.

### Staging

- **A1 — signature (transitional method) + plumbing, per-package, behavior-preserving.**
  **Preserve Kleene short-circuit error semantics**: And/Or/Not/ValuePredicate
  (`predicates.go:290,347,412,464`) check `err` per child *before* the TriBool switch, keeping
  short-circuit returns — `FALSE AND 1/0`→FALSE; `1/0 AND FALSE`→error; `UNKNOWN AND 1/0`
  →error. Pin all three (Graefe: matches Java `AndOrPredicate`).
  **Highest-risk sites are blocking gates INSIDE A1** (Graefe #3): the currently-unrecovered
  `streaming_cursors.go:218,254` (sort/group key + aggregate operand) and
  `flat_map_cursor.go:213` (join) — assert div0/overflow/cast in GROUP BY / aggregate / join
  return the *error* (not an absent row). A half-converted sweep there is silent-wrong-results,
  strictly worse than today's crash.
  **Guard against silent error drops** (Torvalds #2): a CI grep-audit (and a nogo check if
  feasible) banning `_ =` / single-value discard of `EvaluateErr`/`EvalErr` results; audit
  every migrated call site.
- **Enumerate all ~22 user-reachable sites** from `docs/panic-audit.md` and pin each (Torvalds
  #4): values.go `1295,1685,1692,1700,1706,1712,1717,1720,1725,1935,1945,1949,1955,1970,1974,
  1980,1999,2041,2056,2076`; comparisons.go `404,462`. A missed site still panics → 500.
- **Pin the `keep=false` row-drop bug FIRST** (before the GATE): unexpected predicate-eval
  error must surface, never silently drop a row. `executor.go:739` + `executor_new_plans.go:337`.
- **GATE** (between A1 and A2): conformance + 1M stress + **`-race`**, **per-query seeded
  set-equality** over N runs. Quarantine predicate: exclude queries whose RESULT SET (not just
  order) is nondeterministic — i.e. `LIMIT`/`FETCH FIRST` over a partial order, and the known
  join-enumeration row-count-nondeterministic shapes (TODO.md:54). Aggregate row-count diff is
  rejected outright.
- **A2 — delete the 6 recovers** (separate commit): `executor.go:734,918,2505`,
  `executor_new_plans.go:337`, `values.go:416`, `simplifier_value.go:218`. Bisect separates
  "plumbing broke it" from "removing the net exposed a masked bug."

## Verification

- Per-package `just test` green after each A1 commit; determinism 10× on affected planner
  tests.
- New pins: div0 → 22012, overflow → 22003, CAST → 22F3H, type-mismatch → 22000/42804 as
  *returned* errors (incl. the previously-uncovered GROUP BY / aggregate / join eval paths
  via `streaming_cursors.go` / `flat_map_cursor.go`); Kleene short-circuit both orderings;
  the `keep=false` row-drop regression.
- GATE green; stress-1M within noise; full suite + `-race` green after A2.

## Out of scope

- The ~134 internal invariant asserts (stay panics; bradfitz policy).
- `merge_cursor.go:24` / `tuple.Pack` reachability (separate item P0.2-G).
- Wire/format: none (read-path only).

## Reviewers
Graefe (the Value-level seam vs operator-boundary; Kleene semantics; GATE methodology),
Torvalds (incomplete-conversion / dead-recover / the keep=false bug).
