package cascades

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// CascadesRule — the lightweight rule shape for QueryPredicate / Value
// rewrites.
//
// Models Java's `CascadesRule` matcher + OnMatch pattern for the
// predicate-simplification surface: given a PlannerBindings produced
// by the rule's matcher pattern, the rule's OnMatch yields one or more
// replacement predicates/values. These rules are driven by the
// Simplify fixpoint driver (simplifier.go) and by FireRule in tests;
// they yield plain values, not memo References, and carry no cost
// hooks — deliberately, since predicate simplification is a
// strict-reduction rewrite, not a cost-based choice.
//
// The planner's RelationalExpression rules are the separate
// ExpressionRule / ImplementationRule interfaces
// (expression_matcher.go, implementation_rule.go), driven by the task
// stack (unified_tasks.go) with Memo/Reference integration and
// per-phase cost models.

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

// FireRule drives a predicate/value rule once against `in`: run
// `rule.Matcher()`, and for every successful match invoke
// `rule.OnMatch`. Returns the aggregate list of yielded replacements
// across all matches. Used by rule unit tests; production predicate
// simplification runs through Simplify's fixpoint loop
// (simplifier.go), and planner rules through the task stack
// (unified_tasks.go).
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

// RootOperator returns the one concrete type BindMatches admits — computed
// from the type parameter (matching.RootOperatorMatcher). An interface
// type parameter returns nil (always bucket) — same guard as
// ExpressionMatcher; indexing under an interface type would key the rule
// where no concrete lookup ever hits.
func (m *predicateMatcher[T]) RootOperator() reflect.Type {
	t := reflect.TypeFor[T]()
	if t.Kind() == reflect.Interface {
		return nil
	}
	return t
}

func (m *predicateMatcher[T]) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(T); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}
