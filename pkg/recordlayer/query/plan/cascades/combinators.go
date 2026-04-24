package cascades

import "sync/atomic"

// Combinators: AllOf + AnyOf.
//
// Ported from Java's
// `com.apple.foundationdb.record.query.plan.cascades.matching.structure.
// {AllOfMatcher,AnyOfMatcher}`. These are the primary rule-pattern
// building blocks: real rules are AllOf(TypedMatcher, downstream,
// downstream, ...) — every downstream must match for the rule to fire.
//
// Semantics match Java's stream-based impl, adapted to our slice
// return shape:
//
//   AllOf(d1, d2, ..., dN) against `in` returns every Cartesian
//     product of (d1's matches on in) × (d2's matches) × ... (dN's).
//     An empty result from any single downstream collapses AllOf's
//     result to empty (AND semantics).
//
//   AnyOf(d1, d2, ..., dN) against `in` returns the union of every
//     downstream's matches on in. No match collapses to empty (OR).
//
// Both combinators also bind themselves so the rule body can
// retrieve the whole-input via `Get[T](b, combinator)` if the
// pattern names the combinator.

// AllOfMatcher requires every downstream to match the same input.
// Bindings produced by each downstream merge into each output
// PlannerBindings; multi-match downstreams produce a Cartesian
// product across outputs.
type AllOfMatcher struct {
	id          uint64
	rootType    string
	downstreams []BindingMatcher
}

var allOfMatcherCounter atomic.Uint64

// NewAllOf builds an AllOfMatcher whose reported RootType is rootType
// (used for debug explain output only; enforcement is in each
// downstream's own BindMatches). Passing at least one downstream is
// required — AllOf with zero downstreams degenerates to "bind self
// and return one match," which is the `InstanceMatcher` shape — use
// that directly instead.
func NewAllOf(rootType string, downstreams ...BindingMatcher) *AllOfMatcher {
	if len(downstreams) == 0 {
		panic("NewAllOf: need at least one downstream matcher")
	}
	return &AllOfMatcher{
		id:          allOfMatcherCounter.Add(1),
		rootType:    rootType,
		downstreams: downstreams,
	}
}

func (a *AllOfMatcher) RootType() string { return a.rootType }

func (a *AllOfMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	// Seed with one empty binding set so the first downstream's
	// matches land unaltered. Subsequent downstreams extend each
	// existing partial via MergedWith (Cartesian product).
	current := []*PlannerBindings{NewBindings()}
	for _, d := range a.downstreams {
		matches := d.BindMatches(outer, in)
		if len(matches) == 0 {
			// AND: any empty downstream collapses the result.
			return nil
		}
		next := make([]*PlannerBindings, 0, len(current)*len(matches))
		for _, partial := range current {
			for _, m := range matches {
				next = append(next, partial.MergedWith(m))
			}
		}
		current = next
	}
	// Bind the combinator itself so rules can retrieve the whole
	// matched input via Get[T](bindings, allOfMatcher).
	for i, b := range current {
		current[i] = b.Bind(a, in)
	}
	return current
}

// AnyOfMatcher matches when at least one downstream matches. The
// union of all downstream match sets is returned; the combinator
// binds itself into each resulting PlannerBindings.
type AnyOfMatcher struct {
	id          uint64
	rootType    string
	downstreams []BindingMatcher
}

var anyOfMatcherCounter atomic.Uint64

// NewAnyOf builds an AnyOfMatcher. Zero downstreams panics — an
// AnyOf with no downstreams always fails to match, which is rarely
// what a rule author wants to express.
func NewAnyOf(rootType string, downstreams ...BindingMatcher) *AnyOfMatcher {
	if len(downstreams) == 0 {
		panic("NewAnyOf: need at least one downstream matcher")
	}
	return &AnyOfMatcher{
		id:          anyOfMatcherCounter.Add(1),
		rootType:    rootType,
		downstreams: downstreams,
	}
}

func (a *AnyOfMatcher) RootType() string { return a.rootType }

func (a *AnyOfMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	var out []*PlannerBindings
	for _, d := range a.downstreams {
		matches := d.BindMatches(outer, in)
		for _, m := range matches {
			out = append(out, m.Bind(a, in))
		}
	}
	return out
}
