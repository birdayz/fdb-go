# RFC 025 — Cascades Package Structure

**Status:** Phase 1 executed — supplements RFC 021 §Phase 2 + RFC 022. Authored swingshift-50 (2026-04-25); Phase 1 (`values/` + `predicates/` + `matching/` split + ~25 import-site updates) landed nightshift-50 (2026-04-26). Phase 2 (`rules/`) deferred until Batch A rules start landing per RFC plan.

**Scope:** Sub-package layout for `pkg/recordlayer/query/plan/cascades/`. Mirrors Java's `com.apple.foundationdb.record.query.plan.cascades` structure (4.11.1.0) — but no overly small packages, no leaky abstractions, strong per-package unit tests inspired by Java's test suite.

## Motivation

Today's `pkg/recordlayer/query/plan/cascades/` is a flat package — 11 Go source files, ~4,025 LOC — containing values, predicates, comparisons, matchers, combinators, rules, simplifiers, correlation. That's fine for the Phase 4.0 seed.

It will stop being fine soon:

- Phase 4.0 ports the full Type hierarchy (~5–10 new files).
- Phase 4.1 adds `RelationalExpression` + ~12 logical operator subclasses.
- Phase 4.2 fills out the matcher catalogue (~15 matcher shapes).
- Phase 4.5 ports ~69 CascadesRules.
- Phase 4.4 + 4.7 add ~25 properties + an unknown number of debug/event hooks.

That's >100 new types landing in a package that already has 11 files. The maintenance cost goes up linearly; the test-isolation cost goes up worse than that.

**Today's actual challenge** (per dayshift-34 audit + the swingshift-50 user direction): poor package isolation forces validation through `//conformance:conformance_test` + the yamsql harness. Those are slow, expensive, and a sign that the cheap-feedback unit-test tier is not pulling its weight. Sub-packaging mirrors Java's well-bounded layout so each package can be tested in isolation against ports of Java's existing test classes.

## Principles

**P1 — Mirror Java's structure, not invent a Go-native layout.** Java's structure is semantically driven (`values`, `predicates`, `rules`, `expressions`, `properties`, `events`, `explain`, `debug`, `typing`) and reflects ~15 years of experience with what ends up coupling tightly. Don't replay that bake-off in Go.

**P2 — No overly small packages.** A package with one struct + two helpers stays in its parent. Java sometimes splits because Java conventions favour one-class-per-file; Go convention favours grouping related types together. Don't blindly clone a 5-file Java package into a 5-file Go package if those files belong together semantically.

**P3 — No leaky abstractions.** Cross-package dependencies must go through the package's exported interface. If a downstream caller (or test) has to import package-private types via `_test.go` tricks, the boundary is wrong — fix the package's API, not the caller.

**P4 — Strong per-package unit-test coverage.** Take inspiration from Java's test suite (`fdb-record-layer-core/src/test/java/com/apple/foundationdb/record/query/plan/cascades/...`). Each Go sub-package gets deep tests for the surface it exports — parsing, matching, evaluating, simplifying — that don't require spinning up the conformance server or testcontainer FDB. Today's challenge is that without isolation, unit tests can't substitute for integration tests; sub-packaging fixes that.

**P5 — Defer splits where the contents would be tiny or where the package is forward-only.** Don't pre-create empty placeholder packages. Each split should be justified by ~300+ LOC of real content OR by a clear, near-term phase that will fill it.

## Java sub-packages (4.11.1.0) — for reference

| Sub-package | Files | Tests |
|---|---|---|
| `values` | 76 | 16 test classes |
| `rules` | 69 | 6 test classes |
| `expressions` | 28 | (in parent test dir) |
| `events` | 24 | 2 test classes |
| `predicates` | 20 | 2 test classes |
| `properties` | 19 | 1 test class |
| `explain` | 14 | (no dedicated test dir) |
| `debug` | 6 | 1 test class |
| `typing` | 5 | 0 test files (stub) |
| `matching/` | 0 | (placeholder; matchers actually live higher up in 4.11.1.0) |

## Current Go state — `pkg/recordlayer/query/plan/cascades/`

| File | LOC | Content |
|---|---|---|
| `values.go` | 1,396 | Value interface, ValueType, 10+ concrete values, ExplainValue renderer |
| `comparisons.go` | 701 | ComparisonType (13 ops), ComparisonPredicate, numeric promotion, LIKE matcher |
| `rule_simplify.go` | 637 | 11 Phase 4.5 Batch A rules (And/Or flatten + dedup + absorb, Not, ComparisonConstant) |
| `predicates.go` | 416 | QueryPredicate, TriBool, Constant/And/Or/Not/ValuePredicate, structural equality |
| `matcher.go` | 239 | BindingMatcher, PlannerBindings, AnyValue/Instance/ArithmeticMatcher |
| `simplifier.go` | 124 | Fixed-point Simplify driver, DefaultSimplifyRules |
| `simplifier_value.go` | 122 | SimplifyValue — standalone-Value constant fold |
| `combinators.go` | 123 | AllOf / AnyOf matcher combinators |
| `correlation.go` | 96 | CorrelationIdentifier, Named/Unique factories, Correlated interface |
| `rule.go` | 88 | CascadesRule interface, RuleCall, FireRule helper |
| `simplifier_predicate_values.go` | 83 | SimplifyPredicateValues — folds Value operands inside QueryPredicates |

Plus test files (`*_test.go`) and `BUILD.bazel`.

## Cross-package leaks identified

1. **`pkg/relational/core/embedded/projection_fold.go:49–116`** — constructs `expr.Resolver{Analyzer, Scope}` and calls `cascades.SimplifyValue(v)` directly. To unit-test the fold pass, callers wire up four collaborators. Suggests a missing `ExpressionResolver` interface in `expr/` AND that `SimplifyValue` should be exposed via a strategy interface, not as a free function called from outside the cascades package. **Action:** post-split, add an `ExpressionFolder` interface in `cascades/values/` or in a new `cascades/expr-bridge/` sub-package; have `expr.Resolver` implement it.

2. **`pkg/relational/conformance/plandiff/plandiff.go:362`** — calls `embedded.NewExplainOnlyGenerator()` to produce a plan tree. Couples the harness to embedded's naive implementation detail. **Action:** move `NewExplainOnlyGenerator` to a public-API package (e.g. `pkg/relational/core/query/`); the harness imports the API, not the impl.

3. **By-design boundaries (NOT leaks):**
   - `pkg/relational/core/query/expr/walk.go` returns `cascades.Value` and `cascades.QueryPredicate` from `Resolver.Walk*`. The walker IS the bridge; this coupling is intentional.
   - `pkg/relational/core/query/expr/expr.go` `Resolver.Resolve*` methods return cascades types. Same — `expr` is the parse-tree → cascades bridge by design.

## Target layout

### Phase 1 — Execute next shift (3 sub-packages)

#### `pkg/recordlayer/query/plan/cascades/values/`

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.values`
- **Files moved:** `values.go`, `simplifier_value.go`
- **Approx LOC:** 1,500
- **Justification:** Foundational type hierarchy. Java has 76 Value classes; Go is at ~10 today and will grow to ~50+ over phases 4.0–4.5. Splitting now enables per-Value tests without dragging in predicates / rules / matchers. Largest single concern by LOC.
- **Exposed API:** `Value` interface, `ValueType` enum, all 10+ concrete value types, `ExplainValue`, `IsConstantValue`, `EvaluateConstant`, `WalkValue`, `ValueSize`, `ContainsAggregate`, `SimplifyValue`, `LiteralValue`, `ParameterBinder`.
- **Test ports (priority 1):**
  - `ArithmeticValueTest.java` → arithmetic ops, NULL propagation, numeric coercion
  - `BooleanValueTest.java` → Kleene logic, NULL semantics
  - `CastValueTest.java` → type casts, promotion, narrowing

#### `pkg/recordlayer/query/plan/cascades/predicates/`

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.predicates`
- **Files moved:** `predicates.go`, `comparisons.go`, `simplifier_predicate_values.go`
- **Approx LOC:** 1,200
- **Justification:** Second-tier hierarchy. Comparisons live with predicates because `ComparisonPredicate` is the fundamental binary-op predicate; comparisons aren't useful outside that context. Java keeps these together too (Comparisons.java alongside QueryPredicate impls).
- **Exposed API:** `QueryPredicate` interface, `TriBool`, all 5 predicate types, `Comparison`, `ComparisonType`, `ComparisonPredicate`, `WalkPredicate`, `PredicateSize`, `PredicateEquals`, `AsConstant`, `SimplifyPredicateValues`.
- **Imports:** `cascades/values`.
- **Test ports (priority 2):**
  - `QueryPredicateTest.java` → And/Or/Not composition, structural equality, TriBool laws
  - `ConstantFoldingTest.java` → predicate-level fold rules (also exercises the rule infrastructure end-to-end)

#### `pkg/recordlayer/query/plan/cascades/matching/`

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.matching` (empty in 4.11.1.0; matcher infra actually lives in `matching/structure/` in newer Java; we collapse those)
- **Files moved:** `matcher.go`, `combinators.go`
- **Approx LOC:** 360
- **Justification:** Pattern-matching is a distinct concern with a distinct API surface. Will grow when `MatchCandidate` / `PartialMatch` infra lands (phase 4.6+).
- **Exposed API:** `BindingMatcher`, `PlannerBindings`, `MergedWith`, `AnyValue`, `Instance`, `ArithmeticMatcher`, `AllOf`, `AnyOf`, generic `Get[T]` retrieval helper.
- **Imports:** `cascades/values`, `cascades/predicates` (matchers match against Values + Predicates).
- **Test ports (priority 3):**
  - No dedicated Java MatcherTest in 4.11.1.0; cover indirectly via `rule_simplify_test.go` after rules/ split lands. Add a Go-native unit test for `Get[T]` retrieval semantics + zero-size-struct identity gotcha (the nonce-based factory pattern needs explicit coverage).

### Phase 2 — Execute when 4.5 Batch A rules start landing (1 sub-package)

#### `pkg/recordlayer/query/plan/cascades/rules/`

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.rules`
- **Files moved:** `rule.go`, `rule_simplify.go`, `simplifier.go`
- **Approx LOC:** 850 today; 5K+ once Batch A + Batch B rules port
- **Justification:** Java has 69 rule files. Will explode in phase 4.2+. Splitting now while only 11 rules exist is cheap; splitting later when 30+ rules live in the parent is painful.
- **Exposed API:** `CascadesRule`, `RuleCall`, `Yield`, `Yielded`, `FireRule`, `Simplify`, `DefaultSimplifyRules`, all 11 Phase 4.5 Batch A rules.
- **Imports:** `cascades/values`, `cascades/predicates`, `cascades/matching`.
- **Sub-split deferred:** Java has further sub-structure (`rules/dataAccess/`, `rules/pushDown/`, etc.) but only at >25 rules per group. Keep `rules/` flat until Batch A + Batch B port lands.
- **Test ports (priority 4):**
  - `QueryPredicateSimplificationRuleTest.java` → rule firing, yield chains, fixpoint convergence
  - `DecorrelateValuesRuleTest.java` (post-Batch-A) — complex predicate rewrite
  - `SelectMergeRuleTest.java` (post-Phase-4.1) — first expression-level rule

### Phase 3 — Execute when ported types exist (placeholder packages)

#### `pkg/recordlayer/query/plan/cascades/expressions/` (Phase 4.1+)

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.expressions`
- **Status:** No code today. Create when `RelationalExpression` + concrete operator types port.

#### `pkg/recordlayer/query/plan/cascades/typing/` (Phase 4.0+)

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.typing`
- **Status:** Today the `ValueType` enum is in `values.go`. Phase 4.0 replaces it with `Type` / `TypeRepository` / `Typed` (~150 LOC initially).
- **Decision per P2:** **Defer.** Java's typing/ has only 5 files; Go's would start at ~150 LOC. Inline into `cascades/values/` as a `type.go` file until growth justifies splitting. Revisit when Phase 4.0's Type hierarchy port reaches ~300 LOC OR when the seed `ValueType` is fully retired.

#### `pkg/recordlayer/query/plan/cascades/properties/` (Phase 4.4+)

- **Java mirror:** `com.apple.foundationdb.record.query.plan.cascades.properties`
- **Status:** No code today. Create when ExpressionProperty handlers port.

### Don't split

- **`correlation.go`** — 96 LOC. Stays in cascades/ root. `CorrelationIdentifier` is a leaf type used by Values + Expressions; if anything pull it INTO `values/` since today it's only used there. Decide post-Phase-1 based on actual call sites.
- **`events/`, `explain/`, `debug/`** — Java has them but Go has zero code there. Don't pre-create empty packages. Wire into the planner task-stack when Phase 4.6 lands.
- **Simplifier driver `simplifier.go`** — moves into `rules/` in Phase 2. Don't split into its own `simplifier/` package; it's the rule-firing fixpoint, lives with rules.

## Final layout post-Phase-1+2 execution

```
pkg/recordlayer/query/plan/cascades/
  correlation.go              # CorrelationIdentifier (root — 96 LOC)
  doc.go                      # package-level doc
  BUILD.bazel
  values/
    values.go
    simplifier_value.go
    *_test.go (per type)
    BUILD.bazel
  predicates/
    predicates.go
    comparisons.go
    simplifier_predicate_values.go
    *_test.go
    BUILD.bazel
  matching/
    matcher.go
    combinators.go
    *_test.go
    BUILD.bazel
  rules/                      # Phase 2 — execute when Batch A starts
    rule.go
    rule_simplify.go
    simplifier.go
    *_test.go
    BUILD.bazel
```

## Migration plan

1. **Phase 1, single shift:**
   - Create `cascades/values/`, `cascades/predicates/`, `cascades/matching/` directories.
   - Move source + test files; update `package` declarations.
   - Update every external caller's imports (`pkg/relational/core/embedded/`, `pkg/relational/core/query/expr/`, `pkg/relational/conformance/plandiff/`, etc.) — ~25 files touched.
   - Run `just gazelle` to refresh BUILD files.
   - Run `just test`.
   - Single commit. Subject: `cascades: split values / predicates / matching sub-packages (RFC-025 Phase 1)`.

2. **Phase 1 risk mitigation:**
   - The split is mechanical (file moves + import updates). Risk is mostly in catching every import site.
   - Pre-flight: `grep -rn '"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"' --include='*.go'` lists every caller. Confirm the count matches what was updated before committing.
   - Cycle-check: after the split, `bazelisk query 'somepath(//pkg/recordlayer/query/plan/cascades/values/..., //pkg/recordlayer/query/plan/cascades/predicates/...)'` should return empty (values must NOT depend on predicates). If it returns a path, there's a cycle to untangle.

3. **Phase 2 trigger:** Land `rules/` split when the FIRST Batch A rule (`PrimaryScanRule`, `ImplementFilterRule`, etc. — see TODO.md §HIGH 4.5) is ready to commit. Don't pre-split before then.

4. **Test-port priority:** After Phase 1 lands, port the priority-1 + priority-2 test classes from Java's test suite. Goal: each new sub-package has its own `*_test.go` covering the same surface area Java's equivalent test class covers, sized to run in <1s without conformance / testcontainer infra.

## Closing the leaks

After Phase 1 + 2 land:

1. **`projection_fold.go` SimplifyValue dependency:** introduce an `ExpressionFolder` interface in `cascades/values/`:
   ```go
   type ExpressionFolder interface { Fold(Value) Value }
   ```
   `SimplifyValue` becomes the default impl. `embedded` accepts an `ExpressionFolder` via constructor / option, defaults to `values.DefaultFolder`. Tests inject a fake folder.

2. **`projection_fold.go` Resolver dependency:** introduce an `ExpressionResolver` interface in `pkg/relational/core/query/expr/`:
   ```go
   type ExpressionResolver interface { WalkExpression(antlr.IExpressionContext) (cascades.Value, error) }
   ```
   `expr.Resolver` implements it. `embedded.foldConstantProjections` takes the interface, not the concrete type. Tests inject a fake resolver.

3. **`plandiff.go` NewExplainOnlyGenerator dependency:** move the factory to `pkg/relational/core/query/`. The harness imports `query.NewExplainOnlyGenerator(opts)`; embedded provides the impl behind the interface.

## Non-goals

- Not splitting `events/`, `explain/`, `debug/` until Phase 4.6 actually has content for them.
- Not pre-creating placeholder packages with stubs. Empty packages are noise.
- Not introducing Go-native package abstractions that don't mirror Java (e.g. a "core" sub-package that bundles values + predicates + matching). Stay aligned with Java so cross-language code review remains tractable.

## Open questions

1. **Where does `ParameterBinder` live?** Today it's in `values.go`. It's the eval-context capability called by `ParameterValue.Evaluate`. After split, lives in `cascades/values/` since it's evaluation-time, not planning-time. Confirm at split time.
2. **`Correlated` interface — values/ or root?** Used by Values to declare quantifier dependencies. Could go in `values/` (consumers) or stay in root. Decision: keep in root for now, revisit when Quantifier infra lands (Phase 4.3).
3. **Should `cascades/` itself contain anything after Phase 1+2?** Today: just `correlation.go` + `doc.go`. That's fine — the root package becomes the umbrella that defines the cross-cutting `CascadesRule` / `Yield` / etc. interfaces, with sub-packages providing concrete types.
