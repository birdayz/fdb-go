package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

// CascadesRule — seed.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.CascadesRule`
// and `CascadesRuleCall`. A CascadesRule is a transform the planner
// applies to a matched subtree: given a PlannerBindings produced by
// the rule's matcher pattern, the rule's OnMatch produces one or
// more replacement expressions.
//
// Seed scope:
//
//   - CascadesRule interface: Matcher() + OnMatch(RuleCall).
//   - RuleCall: carries the bindings, a place to yield replacements,
//     and a reference to the planner configuration.
//   - YieldExpression helper: accumulates replacements produced
//     during OnMatch. The real planner consumes these to rewrite
//     the memo.
//
// Intentionally minimal. Missing from the seed:
//
//   - Cost estimation hooks (Phase 4.4).
//   - Memo / Reference integration (Phase 4.3) — rule yields today
//     produce plain values, not memo refs.
//   - PreMatch / PostMatch hooks (Java has them for gating).

// RuleCall is the context a CascadesRule receives on every match.
// OnMatch reads bindings and appends replacement expressions via
// Yield.
type RuleCall struct {
	// Bindings is the PlannerBindings the rule's matcher built up
	// during BindMatches. Rule bodies use Get[T] / Get to retrieve
	// matched values.
	Bindings *matching.PlannerBindings

	// yielded holds replacement expressions the rule produces. One
	// rule call can yield zero, one, or many (AnyOf-style rules).
	// Access via Yield / Yielded.
	yielded []any
}

// Yield records a replacement expression. The planner reads the
// accumulated list after OnMatch returns.
func (c *RuleCall) Yield(expr any) {
	c.yielded = append(c.yielded, expr)
}

// Yielded returns the replacements this RuleCall has accumulated.
// Returned slice is the RuleCall's backing array — callers must not
// mutate.
func (c *RuleCall) Yielded() []any { return c.yielded }

// CascadesRule is the transform interface. Concrete rules implement
// Matcher + OnMatch. Rule authors compose the matcher using the
// combinators in combinators.go + the Instance/AnyValue/Arithmetic
// matchers in matcher.go.
type CascadesRule interface {
	// Matcher returns the pattern this rule fires on. The planner
	// walks every expression in the memo, runs every rule's
	// matcher against it, and invokes OnMatch on successful
	// bindings.
	Matcher() matching.BindingMatcher

	// OnMatch is the rule body. It reads call.Bindings to retrieve
	// the matched expression shape and calls call.Yield for each
	// replacement it produces. Zero yields is legal — the rule
	// simply declined to fire for this match.
	OnMatch(call *RuleCall)
}

// FireRule is a simple driver for seed-time rule testing: run
// `rule.Matcher()` against `in`, and for every successful match
// invoke `rule.OnMatch`. Returns the aggregate list of yielded
// replacements across all matches. Production rule driving lives
// in the CascadesPlanner task stack (Phase 4.6); this helper
// exists so the seed has a testable entry point.
func FireRule(rule CascadesRule, in any) []any {
	matcher := rule.Matcher()
	matches := matcher.BindMatches(matching.NewBindings(), in)
	var all []any
	for _, b := range matches {
		call := &RuleCall{Bindings: b}
		rule.OnMatch(call)
		all = append(all, call.Yielded()...)
	}
	return all
}

// predicateMatcher is the generic single-type matcher: type-asserts
// `in` to T (any QueryPredicate concrete type) and binds the host on
// success. Replaces 5 hand-written near-identical matchers
// (notPredicateMatcher, comparisonPredicateMatcher, andPredicateMatcher,
// orPredicateMatcher, valuePredicateMatcher).
//
// rootType is kept as a field rather than computed via reflect so
// debug output stays cheap and the struct has non-zero size (no
// zero-size-struct aliasing — see AnyValue at matching/matcher.go).
//
// Each rule's `new...` factory returns a distinct allocation so
// pointer-identity comparisons stay distinct across rule instances.
type predicateMatcher[T predicates.QueryPredicate] struct {
	rootType string
}

func (m *predicateMatcher[T]) RootType() string { return m.rootType }
func (m *predicateMatcher[T]) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(T); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
