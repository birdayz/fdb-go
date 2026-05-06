# RFC 023 — Cascades Generics Decision

**Status:** Draft — resolves RFC 022 §4.-0.5 (generics-vs-interfaces spike). Supersedes RFC 021 Risk #3's deferral.
**Author:** dayshift-46 (2026-04-24).
**Scope:** Pick the Go shape for `Value` + `BindingMatcher` before Phase 4.0 lands. Does not change what the Cascades port delivers; locks in the type-system shape so subsequent rule-porting shifts don't relitigate.

## The question

Java Cascades is heavy on parameterised types with wildcard variance:

```java
interface Value extends Correlated<Value>, TreeLike<Value>, Typed, ... {}
interface BindingMatcher<T> {
    default BindingMatcher<T> where(BindingMatcher<? super T> downstream);
    default BindingMatcher<T> or(BindingMatcher<? super T> downstream);
}
```

Go generics don't have wildcard bounds. `BindingMatcher<? super T>` isn't expressible. Before committing to a shape we implemented the same tiny matcher — `ArithmeticMatcher(Add, Constant, Field)` — in two shapes and measured the friction.

The winning shape shipped to its production home at `pkg/recordlayer/query/plan/cascades/` (committed in dayshift-46). The losing shape was deleted; the seed code there is the real Phase 4.0 foundation, extended in later shifts.

## Shapes under test

### Shape (a) — interface + `any`

```go
type BindingMatcher interface {
    RootType() string
    BindMatches(outer *PlannerBindings, in any) []*PlannerBindings
}

// Matcher definition (10-line predicate):
lhs := NewConstantMatcher()
rhs := NewFieldMatcher()
matcher := &ArithmeticMatcher{Op: OpAdd, Left: lhs, Right: rhs}

// Rule body retrieval:
cv, ok := b.Get(lhs).(*ConstantValue)   // ← cast at every call site
fv, ok := b.Get(rhs).(*FieldValue)
```

- Non-generic interface. Any matcher plugs into any slot.
- Rule bodies cast `any → *ConcreteType` at every retrieval.
- Compile-time safety: "is it a Value at all"; the shape of the matched subtree is not type-checked.
- No upcast ceremony at composition.
- Per-concrete-type factories (`NewConstantMatcher`, `NewFieldMatcher`, …) one per subtype.

### Shape (b) — generic structs + constraint interfaces

```go
type BindingMatcher[T Value] interface {
    BindMatchesSafely(outer *PlannerBindings, in T) []*PlannerBindings
    BindMatches(outer *PlannerBindings, in any) []*PlannerBindings
}

// Matcher definition:
lhs := NewInstanceMatcher[*ConstantValue]()
rhs := NewInstanceMatcher[*FieldValue]()
matcher := &ArithmeticMatcher{
    Op:    OpAdd,
    Left:  UpcastToValue[*ConstantValue](lhs),  // ← upcast per composition site
    Right: UpcastToValue[*FieldValue](rhs),
}

// Rule body retrieval:
cv := Get[*ConstantValue](b, lhs)   // ← type param at call site; no .(T)
fv := Get[*FieldValue](b, rhs)
```

- Single generic `InstanceMatcher[T]` replaces per-type factories.
- Rule bodies use `Get[T]` — target type at call site, no `.(T)` boilerplate.
- Compile-time safety: the matcher's T is visible at definition; `Get[WrongType]` fails at runtime with a typed error.
- `UpcastToValue[T]` appears at every composition site where a narrow-T matcher sits under a broader-T parent. In this tiny example, two upcasts in a three-line matcher; in real Cascades rules (5+ nested matchers) that's 5+ upcasts per rule pattern.
- The spike's `ArithmeticMatcher` stores children as `BindingMatcher[Value]` because heterogeneous child types can't share a single narrower T. Shape (b)'s matcher-composition ergonomics DEGRADE to shape (a)'s semantics (runtime type assertion inside the downstream matcher's own BindMatches) — only the retrieval-side typing remains a win.

## Spike findings

### Finding 1 — Zero-size matcher structs collide as map keys

Go's spec: "two distinct zero-size variables may have the same address in memory." `&AnyValue{}` + `&AnyValue{}` collapse to the same pointer, which means PlannerBindings keyed on matcher-identity loses the ability to distinguish them. Rule authors would silently retrieve the wrong binding.

Both shapes need a nonce field on every matcher struct (`id uint64` + `NewXxx()` constructor incrementing a global). Rule author MUST use the factory, never `&Matcher{}`. This is a footgun regardless of shape choice — record it in whatever matcher-author docs land in Phase 4.2. Java doesn't hit this because `new Object()` always allocates.

### Finding 2 — Shape (b)'s composition gain is retrieval-only

The intuitive pitch for shape (b) is "generic matchers, compile-time-safe composition." The spike refutes the second half: heterogeneous downstream matchers force `ArithmeticMatcher.Left: BindingMatcher[Value]`, which is exactly shape (a)'s homogeneous `BindingMatcher` with generic sugar. The composition-time type safety is `UpcastToValue[T]` on the caller, not a compile-time narrowing in the matcher struct itself.

The real win in shape (b) is at retrieval: `Get[*ConstantValue](b, lhs)` beats `b.Get(lhs).(*ConstantValue)` for readability and failure modes. Rule bodies are read often; worth something. But it's an incremental win, not a paradigm shift.

### Finding 3 — Generic matcher combinators don't compose past 2 levels

A shape-(b) combinator generic in its input type (`AllOfMatcher[T]`) forces every downstream matcher to share T. Once you compose `AllOfMatcher[Value](m1, m2, m3)` where m1/m2/m3 want different concrete T's, you need upcasts or the combinator itself has to degrade to `BindingMatcher[Value]`. Nested combinators `AllOf[T1](AllOf[T2](...))` require writing out the full type parameter tree at the outermost call site, which becomes unreadable at 3+ levels.

This was the deciding finding. Real Cascades rule patterns are 4–7 matcher levels deep (e.g. `ImplementNestedLoopJoin` pattern in Java is 6 levels). Shape (b) at depth 3+ either (i) explodes the caller-visible type parameter list or (ii) collapses to shape (a) via upcasts. Neither is better than shape (a)'s "every matcher is the same interface type, compose freely."

## Decision

**Pick shape (a): non-generic `BindingMatcher` + `any` + per-concrete-type matcher factories.** Retrieval downcast via `.(T)` at rule body sites.

Rationale:
- Composition depth handles cleanly at 3+ levels (the realistic case).
- No upcast boilerplate at matcher definition.
- Finding 2: shape (b)'s main claimed win (compile-time composition safety) doesn't hold under heterogeneous children, which is the realistic case.
- Shape (b)'s retrieval-side ergonomics can be recovered with a shape-(a) helper: `shapea.Get[T](b *PlannerBindings, m BindingMatcher) T` using generics locally at the retrieval site, without touching the matcher interface. Best of both: generic retrieval where it's cheap, non-generic matchers where genericity costs.

## Changes to Phase 4.0 plan

1. **Matcher interface**: non-generic `BindingMatcher` with `BindMatches(outer *PlannerBindings, in any) []*PlannerBindings`. Rule bodies retrieve via a generic `Get[T](b, m) T` helper (new ergonomic layer; not on the interface).

2. **Matcher factories**: one per concrete Value subtype (`NewConstantValueMatcher`, `NewFieldValueMatcher`, `NewArithmeticValueMatcher`, …). Boilerplate-y, but the boilerplate is trivial and codegen-able if it grows beyond ~20 matchers.

3. **Matcher identity**: every matcher struct carries a nonce `id uint64`. Factory increments a global counter. `&Matcher{}` literals are banned in matcher construction — enforce via lint note / package doc comment. (A `nogo` analyser that flags bare struct literals in `pkg/relational/core/plan/.../matchers` is a candidate for a Phase 4.2 follow-up.)

4. **Value hierarchy**: `Value` is a non-generic interface. Concrete values (`ConstantValue`, `FieldValue`, `ArithmeticValue`, …) are concrete Go structs. `TreeLike<Value>` from Java becomes a `Children() []Value` method on the Value interface — no generics.

5. **`Get[T]` retrieval helper**: lives in the matcher package as a free function, uses generics internally, panics with a typed error on mismatch. Documented as "rule body retrieval contract — call with the target concrete type."

## Non-goals

- Does NOT define the `Type` hierarchy. That's a separate spike (likely Phase 4.0 proper).
- Does NOT pick the `Comparisons` shape. Also Phase 4.0.
- Does NOT lock the memo representation. Phase 4.3.

## Artifacts

- Production package: `pkg/recordlayer/query/plan/cascades/` — committed Phase 4.0 seed (dayshift-46). Port was done during the comparison; once the decision landed we deleted the losing shape and moved the winner to its Java-aligned home, dropping the "spike" framing.
- Tests: `matcher_test.go` in the same package, pinning matcher semantics + zero-size-struct finding + the `Get[T]` retrieval helper.

## Open questions

1. **Codegen for matcher factories?** 20+ concrete Value subtypes × one factory each = ~60 lines of near-duplicate code. Options: (i) hand-write them, review once, move on. (ii) `go generate` pass driven by a list of Value subtypes. No decision before Phase 4.0 — try hand-writing, see how bad it is, escalate to codegen if painful.
2. **Retrieval-panic vs retrieval-error?** Shape (a)'s `.(T)` gives rule authors `, ok`. The generic `Get[T]` panics on mismatch. Is panic acceptable for rule bodies, or should we have `GetOK[T](b, m) (T, bool)` too? Err on the side of panic-first (simpler rule bodies); add `GetOK` if a real rule needs it.
