package matching

// ListMatcher pairs each element of an []any (or []Value, etc.) with
// a positional downstream matcher and only succeeds when the input
// length matches the downstream count AND every downstream binds.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.matching.structure.
// ListMatcher`. Java extends `CollectionMatcher<T>`, where the input
// is `Collection<T>`. The Go port operates on `[]any` so we don't
// need to introduce a generic CollectionMatcher hierarchy this shift
// — Cascades uses ListMatcher mostly for fixed-arity-shape pinning
// (e.g. an ArithmeticValue's [left, right] children, a
// ScalarFunctionValue's Args slice), and `[]any` covers both cases via
// the host matcher unwrapping its children.
//
// Failure modes:
//   - Length mismatch → return nil (no match).
//   - Any downstream returns no bindings → return nil.
//   - Downstreams produce multiple matches → Cartesian product across
//     positions, mirroring Java's flatMap accumulator.
//
// The matcher binds itself + each downstream when matching succeeds,
// so rule bodies can fetch any positional value via Get[T].
// The downstreams slice gives the struct non-zero size so two
// `new(ListMatcher)` calls receive distinct map-key identities (no
// nonce field needed here — see the zero-size-struct gotcha
// documented at AnyValue in matcher.go).
type ListMatcher struct {
	downstreams []BindingMatcher
}

func (*ListMatcher) isCollectionMatcher() {}

// NewListMatcher constructs a ListMatcher matching exactly len(downstreams)
// elements in order. Panics on zero downstreams — same rationale as
// NewAllOf / NewAnyOf: a zero-downstream matcher is a degenerate
// pattern, the rule author probably wants either an explicit
// length-1 matcher or a different combinator.
func NewListMatcher(downstreams ...BindingMatcher) *ListMatcher {
	if len(downstreams) == 0 {
		panic("NewListMatcher: need at least one downstream matcher")
	}
	return &ListMatcher{
		downstreams: downstreams,
	}
}

func (*ListMatcher) RootType() string { return "List" }

// BindMatches checks length, then folds matches in position order.
// Threads the running accumulator as `outer` to each downstream —
// matches AllOfMatcher's convention so per-position downstream matches
// already include the running partial; no MergedWith dance, no
// double-binding.
//
// Java's ListMatcher passes the original `outerBindings` to each
// downstream and uses MergedWith to fold; that works in Java because
// Java's TypedMatcher / EmptyCollectionMatcher don't include outer in
// their result (they return a fresh `PlannerBindings.from(matcher,
// item)`). Our matchers always return `outer.Bind(self, item)` (the
// AllOfMatcher convention), so threading is the right composition.
//
// Empty match at any position collapses the result to nil — same
// AND-style cutoff as AllOfMatcher.
func (m *ListMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	items, ok := in.([]any)
	if !ok {
		return nil
	}
	if len(items) != len(m.downstreams) {
		return nil
	}
	current := []*PlannerBindings{outer}
	for i, item := range items {
		next := make([]*PlannerBindings, 0, len(current))
		for _, partial := range current {
			matches := m.downstreams[i].BindMatches(partial, item)
			next = append(next, matches...)
		}
		if len(next) == 0 {
			return nil
		}
		current = next
	}
	for i, b := range current {
		current[i] = b.Bind(m, in)
	}
	return current
}
