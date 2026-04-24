package shapea

// PlannerBindings is an append-only multimap from matcher-identity
// (pointer) to matched values. Mirrors Java's PlannerBindings,
// compressed for the spike.
//
// Identity keying: two separate `Instance[ConstantValue]` matchers
// that happen to look identical still bind separately, since rule
// authors expect to distinguish between "left operand" and "right
// operand" matches. A pointer key (or the matcher's own identity)
// preserves that.
type PlannerBindings struct {
	// For the spike, a slice-per-matcher is enough. A real impl
	// would use a Multimap with stable iteration.
	entries map[BindingMatcher][]any
}

// NewBindings returns an empty PlannerBindings.
func NewBindings() *PlannerBindings {
	return &PlannerBindings{entries: map[BindingMatcher][]any{}}
}

// Bind appends in under matcher's identity. Returns a new Bindings
// (immutable-style) so matchers don't mutate caller state
// across speculative matches.
func (b *PlannerBindings) Bind(matcher BindingMatcher, in any) *PlannerBindings {
	out := &PlannerBindings{entries: make(map[BindingMatcher][]any, len(b.entries)+1)}
	for k, v := range b.entries {
		out.entries[k] = v
	}
	out.entries[matcher] = append(append([]any{}, out.entries[matcher]...), in)
	return out
}

// Get returns the single value bound to matcher, panicking if 0 or
// >1 are bound. Rule bodies call Get after a successful match.
// Panics are OK for malformed bindings — rule authors get immediate
// feedback. Note: the return type is `any` — rule bodies must
// downcast. That's the whole point of shape (a).
func (b *PlannerBindings) Get(matcher BindingMatcher) any {
	vs := b.entries[matcher]
	if len(vs) != 1 {
		panic("expected exactly one binding for matcher")
	}
	return vs[0]
}

// GetAll returns all values bound to matcher (possibly empty).
func (b *PlannerBindings) GetAll(matcher BindingMatcher) []any {
	return b.entries[matcher]
}

// BindingMatcher is the non-generic interface every shape-(a)
// matcher implements. The `in` parameter is `any` so callers can
// compose heterogeneous matchers in a homogeneous slice (the
// AllOf / AnyOf combinators depend on this).
//
// bindMatches contract (mirrors Java): if `in` is not an instance
// of this matcher's root type, return an empty result set — no
// error, no panic. Rule authors get the runtime-typecheck-on-
// downstream-bind behaviour for free.
//
// Return shape: []*PlannerBindings rather than iterator/stream.
// The spike doesn't care about laziness; a real port would use
// iter.Seq or a callback.
type BindingMatcher interface {
	// RootType identifies what concrete type this matcher can
	// match. Used by the matcher dispatch to skip obviously-
	// unmatchable inputs fast. `any` here — the concrete type is
	// conveyed via the matcher impl's runtime behavior.
	RootType() string
	// BindMatches returns the extended bindings produced by this
	// matcher against `in`, one per successful match. Empty means
	// "no match"; nil is equivalent.
	BindMatches(outer *PlannerBindings, in any) []*PlannerBindings
}

// --- AnyValue -------------------------------------------------------

// AnyValue matches any Value. Bound value is the Value itself.
//
// **Spike finding:** zero-size matcher structs COLLIDE as map keys.
// `&AnyValue{}` + `&AnyValue{}` share an address under Go's
// zero-size-type optimisation, so two distinct `AnyValue` matchers
// would bind to the same identity in PlannerBindings and the rule
// body would retrieve the wrong value. A nonce field (or any field)
// forces distinct allocation — use the global counter so every call
// to `NewAnyValue` produces a distinct-identity matcher.
type AnyValue struct {
	id uint64
}

var anyValueCounter uint64

// NewAnyValue constructs a fresh AnyValue matcher with a unique
// identity so bindings can't collide. Rule authors MUST use this
// rather than `&AnyValue{}`.
func NewAnyValue() *AnyValue {
	anyValueCounter++
	return &AnyValue{id: anyValueCounter}
}

func (*AnyValue) RootType() string { return "Value" }

func (m *AnyValue) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	if _, ok := in.(Value); !ok {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}

// --- Instance: matches on concrete Go type -------------------------

// Instance matches when `in` is an instance of the exact concrete
// Go type carried by this matcher. The type is conveyed via the
// constructor helper, which locks in the runtime-check via a
// closure so the interface stays non-generic.
type Instance struct {
	rootType string
	matches  func(any) bool
}

// NewConstantMatcher produces a matcher that only matches *ConstantValue.
// There's one constructor per concrete type — no generics, so every
// type that wants a matcher carries a corresponding hand-written
// factory. For the spike I write three.
func NewConstantMatcher() *Instance {
	return &Instance{rootType: "ConstantValue", matches: func(in any) bool {
		_, ok := in.(*ConstantValue)
		return ok
	}}
}

// NewFieldMatcher produces a matcher that only matches *FieldValue.
func NewFieldMatcher() *Instance {
	return &Instance{rootType: "FieldValue", matches: func(in any) bool {
		_, ok := in.(*FieldValue)
		return ok
	}}
}

func (i *Instance) RootType() string { return i.rootType }
func (i *Instance) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	if !i.matches(in) {
		return nil
	}
	return []*PlannerBindings{outer.Bind(i, in)}
}

// --- ArithmeticMatcher: typed host + two downstreams ---------------

// ArithmeticMatcher matches *ArithmeticValue and recurses two
// downstream matchers on left + right. Downstream types are
// BindingMatcher — homogeneous — so the matcher can accept any
// child-matcher shape (including AnyValue).
type ArithmeticMatcher struct {
	Op    ArithmeticOp
	Left  BindingMatcher
	Right BindingMatcher
}

func (*ArithmeticMatcher) RootType() string { return "ArithmeticValue" }
func (a *ArithmeticMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	av, ok := in.(*ArithmeticValue)
	if !ok || av.Op != a.Op {
		return nil
	}
	// Recurse left, then right. Cartesian product of match sets is
	// how Java's combinator composes multiple match streams.
	leftMatches := a.Left.BindMatches(outer, av.Left)
	if len(leftMatches) == 0 {
		return nil
	}
	var out []*PlannerBindings
	for _, lb := range leftMatches {
		rightMatches := a.Right.BindMatches(lb, av.Right)
		if len(rightMatches) == 0 {
			continue
		}
		out = append(out, rightMatches...)
	}
	if len(out) == 0 {
		return nil
	}
	// Bind the arithmetic node itself so the rule body can fetch it.
	for i, b := range out {
		out[i] = b.Bind(a, av)
	}
	return out
}
