// Package cascadesspike is a RFC-022 §4.-0.5 exploratory spike —
// two sketches of Cascades' Value + BindingMatcher interfaces in
// idiomatic Go, compared head-to-head so the Phase 4.0 port can
// commit to one shape with eyes open.
//
// This package is NOT production code. It compiles under `just
// build`, has tests pinning the matcher semantics for each shape,
// and will be deleted (or ported to production shape) after the
// decision in docs/rfcs/023-...md (or an update to RFC-022).
//
// # What we're measuring
//
// Cascades is an optimiser that rewrites expression trees via
// pattern-matching rules. The matcher DSL needs to:
//
//  1. Bind matcher objects to matched subtrees so rule bodies
//     can retrieve them after the match completes.
//  2. Accept downstream matchers that are narrower than the host
//     matcher's type (Java: `BindingMatcher<? super T>`).
//  3. Compose into rules whose bindings are heterogeneous —
//     a rule might bind a `ConstantValue` and an `ArithmeticValue`
//     in the same pattern, retrieve them back as their concrete
//     types in the rule body.
//
// In Java this is expressed with `BindingMatcher<T>` +
// `? super T` / `? extends T` wildcards. Go generics do not have
// wildcard bounds (`BindingMatcher[? super T]` isn't expressible).
// So the spike is: which Go shape gives us (1) enough compile-time
// safety that rule authors don't silently mis-bind and (2) not so
// much type-system friction that matcher definitions become
// unreadable?
//
// # Two shapes
//
// [shapea] — Interfaces + `any`. All matchers use the same
// non-generic `BindingMatcher` interface. Matched values flow
// through `any`. Rule bodies downcast via type assertion.
// Resembles how our current index-expression evaluator handles
// heterogeneous KeyExpression trees. Compile-time safety is
// limited to "is it a Value at all"; matcher-shape mismatch is a
// runtime panic/skip.
//
// [shapeb] — Generic structs. `BindingMatcher[T Value]` takes a
// type parameter. Downstream matchers compose via explicit T
// declarations. Heterogeneous rule patterns bundle matcher calls
// inline rather than in a homogeneous slice (Go generics can't
// express "slice of matchers over ? extends Value").
//
// # Decision criteria
//
//   - API friction at matcher definition (lines of code for the
//     same 10-line predicate matcher)
//   - Rule author ergonomics (can they retrieve a bound value as
//     its concrete type without boilerplate?)
//   - Compile-time safety (does a mis-typed downstream matcher
//     error at build time, or at rule fire time?)
//   - Downstream impact on the AllOf / AnyOf / TypedWithDownstream
//     combinators that rule authors call heavily
package cascadesspike
