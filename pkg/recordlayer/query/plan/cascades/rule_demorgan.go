package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// DeMorganRule applies De Morgan's law to push a NOT past an AND or
// OR boundary:
//
//	NOT(AND(a, b, c)) ≡ OR (NOT a, NOT b, NOT c)
//	NOT(OR (a, b, c)) ≡ AND(NOT a, NOT b, NOT c)
//
// Mirrors Java's `DeMorgansTheoremRule`. Kleene-safe: each child term
// is independently negated and the major is swapped, preserving 3VL
// semantics. Reducing-after-fold: subsequent NotConstantSimplifyRule +
// AndConstantSimplifyRule passes can collapse the negated tree
// further (`NOT TRUE` → FALSE, `OR(FALSE, FALSE)` → FALSE, etc.).
//
// Java's default query-predicate rules include this rewrite. The separate
// constant-evaluation set adds it through NormalizationRules.
type DeMorganRule struct {
	matcher matching.BindingMatcher
}

// NewDeMorganRule constructs the rule.
func NewDeMorganRule() *DeMorganRule {
	return &DeMorganRule{matcher: newNotPredicateMatcher()}
}

func (r *DeMorganRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *DeMorganRule) OnMatch(call *RuleCall) {
	not := call.Bindings.Get(r.matcher).(*predicates.NotPredicate)
	switch child := not.Child.(type) {
	case *predicates.AndPredicate:
		// NOT(AND(...)) → OR(NOT ..., NOT ..., ...).
		negated := make([]predicates.QueryPredicate, len(child.SubPredicates))
		for i, sp := range child.SubPredicates {
			negated[i] = &predicates.NotPredicate{Child: sp}
		}
		call.YieldAndReExplore(&predicates.OrPredicate{SubPredicates: negated})
	case *predicates.OrPredicate:
		// NOT(OR(...)) → AND(NOT ..., NOT ..., ...).
		negated := make([]predicates.QueryPredicate, len(child.SubPredicates))
		for i, sp := range child.SubPredicates {
			negated[i] = &predicates.NotPredicate{Child: sp}
		}
		call.YieldAndReExplore(&predicates.AndPredicate{SubPredicates: negated})
	default:
		// NOT over a non-And/Or child — out of scope; let
		// NotConstantSimplifyRule / NotComparisonRewriteRule handle
		// leaves and double-negation.
	}
}

// NormalizationRules combines NOT distribution with constant evaluation.
func NormalizationRules() []CascadesRule {
	out := []CascadesRule{NewDeMorganRule()}
	return append(out, DefaultSimplifyRules()...)
}
