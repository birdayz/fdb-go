package shapeb

import "sync/atomic"

// PlannerBindings in shape (b): a typed lookup per matcher identity.
//
// The tension: bindings are heterogeneous — a rule pattern binds a
// ConstantValue and an ArithmeticValue in the same match — but Go
// generics can't express "map from BindingMatcher[?] to the matched
// value's concrete type". Two realistic options:
//
//  1. Store values as `any` keyed by the matcher (erase the T).
//     The generic `Get[T]` fetcher casts on retrieval. This is
//     basically shape (a) inside, wrapped in generic sugar — the
//     `any` still exists, but the rule-body downcast is hidden
//     behind a single `Get[ConstantValue](m)` call.
//  2. Per-type binding maps (`map[*ConstantMatcher]*ConstantValue`,
//     `map[*FieldMatcher]*FieldValue`, …) — explodes with the
//     number of Value subtypes; not viable.
//
// We take option (1). The win over shape (a) is the retrieval site
// is generic: `Get[*ConstantValue](bindings, m)` infers the target
// type from the explicit T argument, and the assertion failure
// surfaces as a typed error rather than a `.(T)` downcast
// everywhere.
type PlannerBindings struct {
	entries map[any][]any
}

func NewBindings() *PlannerBindings {
	return &PlannerBindings{entries: map[any][]any{}}
}

// bind is an untyped helper used by matcher internals.
func (b *PlannerBindings) bind(matcher any, in any) *PlannerBindings {
	out := &PlannerBindings{entries: make(map[any][]any, len(b.entries)+1)}
	for k, v := range b.entries {
		out.entries[k] = v
	}
	out.entries[matcher] = append(append([]any{}, out.entries[matcher]...), in)
	return out
}

// Get retrieves the single value bound to matcher. The target type T
// is explicit at the call site — if the binding is of a different
// type the call panics with a clear message. Same compile-time
// safety envelope as shape (a) but less ceremony at the rule body.
func Get[T Value](b *PlannerBindings, matcher any) T {
	vs := b.entries[matcher]
	if len(vs) != 1 {
		panic("expected exactly one binding for matcher")
	}
	v, ok := vs[0].(T)
	if !ok {
		panic("bound value is not of the requested type")
	}
	return v
}

// BindingMatcher[T] is the generic matcher interface. T is the
// concrete Value subtype this matcher binds to. A homogeneous slice
// of matchers with DIFFERENT T parameters is NOT expressible in Go
// generics — the AllOf / AnyOf combinators either (i) use a
// non-generic base wrapper (shape (b) degrades to shape (a) for
// composition) or (ii) are themselves generic in T, which means all
// matchers in one combinator must share a single root T.
//
// For the spike we pick the combinator shape that matches how
// Cascades rule patterns actually read: root matcher is typed
// concretely; downstream matchers receive the parent's child type
// through `T Value` with explicit parameterisation.
type BindingMatcher[T Value] interface {
	// BindMatchesSafely is called when the caller has already
	// ensured in : T (e.g. the parent matcher narrowed to the
	// concrete subtype via its own type assertion).
	BindMatchesSafely(outer *PlannerBindings, in T) []*PlannerBindings
	// BindMatches is the type-erased entry point. Implementations
	// test the runtime type of `in` and cast down to T on match —
	// mirrors Java's default bindMatches(Object in).
	BindMatches(outer *PlannerBindings, in any) []*PlannerBindings
}

// --- Instance matcher for a specific Value subtype -----------------

// InstanceMatcher is a generic matcher that binds iff `in` is
// assignable to T. Pure generic — rule authors don't write per-type
// wrappers.
//
// **Spike finding:** zero-size matcher structs collide as map keys
// (same as shape (a)'s AnyValue). NewInstanceMatcher bakes in a
// nonce so distinct matchers have distinct identities.
type InstanceMatcher[T Value] struct {
	id uint64
}

// atomic so parallel test runs / pattern builds don't race.
var instanceMatcherCounter atomic.Uint64

// NewInstanceMatcher constructs a fresh InstanceMatcher[T] with a
// unique identity so bindings don't collide.
func NewInstanceMatcher[T Value]() *InstanceMatcher[T] {
	return &InstanceMatcher[T]{id: instanceMatcherCounter.Add(1)}
}

func (m *InstanceMatcher[T]) BindMatchesSafely(outer *PlannerBindings, in T) []*PlannerBindings {
	return []*PlannerBindings{outer.bind(m, in)}
}

func (m *InstanceMatcher[T]) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	casted, ok := in.(T)
	if !ok {
		return nil
	}
	return m.BindMatchesSafely(outer, casted)
}

// AnyMatcher binds any Value. In shape (b) its T is Value (the
// upper bound) — same shape as shape (a)'s AnyValue but via the
// generic interface.
//
// Carries a nonce for the same reason InstanceMatcher does.
type AnyMatcher struct {
	id uint64
}

// atomic so parallel test runs / pattern builds don't race.
var anyMatcherCounter atomic.Uint64

// NewAnyMatcher constructs a fresh AnyMatcher with a unique identity.
func NewAnyMatcher() *AnyMatcher {
	return &AnyMatcher{id: anyMatcherCounter.Add(1)}
}

func (m *AnyMatcher) BindMatchesSafely(outer *PlannerBindings, in Value) []*PlannerBindings {
	return []*PlannerBindings{outer.bind(m, in)}
}

func (m *AnyMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	v, ok := in.(Value)
	if !ok {
		return nil
	}
	return m.BindMatchesSafely(outer, v)
}

// --- Upcast wrapper: BindingMatcher[T] → BindingMatcher[Value] ------

// UpcastToValue lifts a concrete BindingMatcher[T] into a
// BindingMatcher[Value] so it can sit in a homogeneous child slot
// (ArithmeticMatcher.Left, AllOfMatcher's downstream list, …). Every
// time a rule author composes a narrow matcher under a broader
// combinator, this wrapper has to appear. That boilerplate is the
// cost of shape (b)'s generics — compile-time safety at definition,
// explicit upcasts at composition.
//
// In shape (a) this is implicit (all matchers are the same
// non-generic type); in Java it's implicit (`? super T` wildcards
// handle the variance for the caller). In Go generics you pay
// explicitly.
type upcast[T Value] struct {
	inner BindingMatcher[T]
}

func (u *upcast[T]) BindMatchesSafely(outer *PlannerBindings, in Value) []*PlannerBindings {
	// At runtime, in is either T or not. Cast and forward.
	casted, ok := in.(T)
	if !ok {
		return nil
	}
	// Bind under the INNER matcher's identity so rule bodies can
	// retrieve via the original BindingMatcher[T] reference.
	return u.inner.BindMatchesSafely(outer, casted)
}

func (u *upcast[T]) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	return u.inner.BindMatches(outer, in)
}

// UpcastToValue wraps a BindingMatcher[T] so it satisfies
// BindingMatcher[Value]. Required at every composition site where a
// narrower-T matcher is used under a broader-T parent.
func UpcastToValue[T Value](m BindingMatcher[T]) BindingMatcher[Value] {
	return &upcast[T]{inner: m}
}

// --- ArithmeticMatcher: two downstream matchers over Value ---------

// Tension point: Left and Right matchers want to carry different T
// parameters (left is a Constant, right is a Field). Go generics
// can't express `BindingMatcher[? super Value]` — you'd need a
// single T that's an upper bound of both concrete left/right types.
// The only T that works for heterogeneous children is `Value`
// itself.
//
// Two ways out:
//
//	(i) This matcher stores Left/Right as BindingMatcher[Value], and
//	    forces downstream matchers to be on Value even when the rule
//	    author wants them narrower. Downstream matchers type-assert
//	    in their own BindMatches body (same cost as shape (a)).
//	(ii) This matcher is itself generic on two type params
//	    `ArithmeticMatcher[L Value, R Value]`. Works, but the type
//	    parameter list explodes as combinator depth grows — a
//	    three-level pattern becomes `M[A, M[B, M[C, D]]]` and
//	    eventually you can't write the type.
//
// Shape (b) picks (i): store `BindingMatcher[Value]` and let
// downstream matchers do the narrowing. Which means shape (b)'s
// gain over shape (a) is purely the retrieval-side type safety
// (Get[*ConstantValue] vs `.(*ConstantValue)`); matcher composition
// is indistinguishable.
type ArithmeticMatcher struct {
	Op    ArithmeticOp
	Left  BindingMatcher[Value]
	Right BindingMatcher[Value]
}

func (a *ArithmeticMatcher) BindMatchesSafely(outer *PlannerBindings, in *ArithmeticValue) []*PlannerBindings {
	if in.Op != a.Op {
		return nil
	}
	leftMatches := a.Left.BindMatches(outer, in.Left)
	if len(leftMatches) == 0 {
		return nil
	}
	var out []*PlannerBindings
	for _, lb := range leftMatches {
		rightMatches := a.Right.BindMatches(lb, in.Right)
		if len(rightMatches) == 0 {
			continue
		}
		out = append(out, rightMatches...)
	}
	for i, b := range out {
		out[i] = b.bind(a, in)
	}
	return out
}

func (a *ArithmeticMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	av, ok := in.(*ArithmeticValue)
	if !ok {
		return nil
	}
	return a.BindMatchesSafely(outer, av)
}
