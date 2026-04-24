package cascades

import "sync/atomic"

// Predicate-simplification rules — seed.
//
// Examples of the rule pattern for Phase 4.5 Batch A. Each rule
// defines a matcher and OnMatch body; FireRule drives them from
// tests. These mirror Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.
// simplification.*` predicate simplifications:
//
//   - AndConstantSimplifyRule → AndPredicate with a constant child
//     simplifies. TRUE child drops; FALSE child collapses whole
//     AndPredicate to FALSE.
//   - OrConstantSimplifyRule → OrPredicate mirror: FALSE child
//     drops; TRUE child collapses whole OrPredicate to TRUE.
//
// Seed uses a QueryPredicate-shaped matcher (the existing matcher
// interface is over `any`, so it works directly on QueryPredicate
// trees without any new matcher types).

// AndConstantSimplifyRule matches an AndPredicate and folds constant
// children per Kleene AND identities.
type AndConstantSimplifyRule struct {
	matcher BindingMatcher
}

// NewAndConstantSimplifyRule constructs the rule.
func NewAndConstantSimplifyRule() *AndConstantSimplifyRule {
	m := &AndConstantSimplifyRule{}
	// Match any *AndPredicate via an Instance-like matcher. Seed
	// doesn't have a generic predicate-type matcher, so inline one.
	m.matcher = newAndPredicateMatcher()
	return m
}

func (r *AndConstantSimplifyRule) Matcher() BindingMatcher { return r.matcher }

func (r *AndConstantSimplifyRule) OnMatch(call *RuleCall) {
	and := call.Bindings.Get(r.matcher).(*AndPredicate)
	// Collect non-TRUE children; short-circuit on FALSE.
	kept := make([]QueryPredicate, 0, len(and.SubPredicates))
	for _, sp := range and.SubPredicates {
		if cp, ok := sp.(*ConstantPredicate); ok {
			if cp.Value == TriFalse {
				// Whole AND collapses to FALSE regardless of siblings.
				call.Yield(NewConstantPredicate(TriFalse))
				return
			}
			if cp.Value == TriTrue {
				// TRUE is AND-identity; drop.
				continue
			}
			// UNKNOWN: keep as-is — the AND rule fires again on a
			// rewrite that canonicalises UNKNOWN before the AND.
		}
		kept = append(kept, sp)
	}
	// Only yield when we actually changed something.
	if len(kept) == len(and.SubPredicates) {
		return
	}
	switch len(kept) {
	case 0:
		call.Yield(NewConstantPredicate(TriTrue))
	case 1:
		call.Yield(kept[0])
	default:
		call.Yield(&AndPredicate{SubPredicates: kept})
	}
}

// OrConstantSimplifyRule matches an OrPredicate and folds constant
// children per Kleene OR identities.
type OrConstantSimplifyRule struct {
	matcher BindingMatcher
}

// NewOrConstantSimplifyRule constructs the rule.
func NewOrConstantSimplifyRule() *OrConstantSimplifyRule {
	m := &OrConstantSimplifyRule{}
	m.matcher = newOrPredicateMatcher()
	return m
}

func (r *OrConstantSimplifyRule) Matcher() BindingMatcher { return r.matcher }

func (r *OrConstantSimplifyRule) OnMatch(call *RuleCall) {
	or := call.Bindings.Get(r.matcher).(*OrPredicate)
	kept := make([]QueryPredicate, 0, len(or.SubPredicates))
	for _, sp := range or.SubPredicates {
		if cp, ok := sp.(*ConstantPredicate); ok {
			if cp.Value == TriTrue {
				call.Yield(NewConstantPredicate(TriTrue))
				return
			}
			if cp.Value == TriFalse {
				// FALSE is OR-identity; drop.
				continue
			}
		}
		kept = append(kept, sp)
	}
	if len(kept) == len(or.SubPredicates) {
		return
	}
	switch len(kept) {
	case 0:
		call.Yield(NewConstantPredicate(TriFalse))
	case 1:
		call.Yield(kept[0])
	default:
		call.Yield(&OrPredicate{SubPredicates: kept})
	}
}

// --- Predicate matchers -------------------------------------------

// andPredicateMatcher / orPredicateMatcher are minimal Instance-like
// matchers over *AndPredicate / *OrPredicate. No zero-size gotcha
// (both structs are addressable; the matcher is used directly from
// the rule's Matcher() field, not allocated repeatedly).

// Nonce counters so distinct matcher instances have distinct
// identities (avoids Go's zero-size-struct address collision that
// would otherwise break PlannerBindings' matcher-key lookups when
// multiple rule instances are live at once).
var (
	andPredicateMatcherCounter atomic.Uint64
	orPredicateMatcherCounter  atomic.Uint64
)

type andPredicateMatcher struct{ id uint64 }

func newAndPredicateMatcher() *andPredicateMatcher {
	return &andPredicateMatcher{id: andPredicateMatcherCounter.Add(1)}
}
func (*andPredicateMatcher) RootType() string { return "AndPredicate" }
func (m *andPredicateMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	if _, ok := in.(*AndPredicate); !ok {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}

type orPredicateMatcher struct{ id uint64 }

func newOrPredicateMatcher() *orPredicateMatcher {
	return &orPredicateMatcher{id: orPredicateMatcherCounter.Add(1)}
}
func (*orPredicateMatcher) RootType() string { return "OrPredicate" }
func (m *orPredicateMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	if _, ok := in.(*OrPredicate); !ok {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}
