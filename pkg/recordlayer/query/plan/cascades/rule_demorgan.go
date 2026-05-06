package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
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
// **Not part of DefaultSimplifyRules.** Java applies De Morgan as a
// separate normalisation pass (`BooleanNormalizer`); the seed
// Simplify driver runs only constant-fold + identity-drop +
// absorbing-element + leaf-NOT-rewrite. Use NormalizationRules() (or
// build a custom rule list) when De Morgan is desired.
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
		call.Yield(&predicates.OrPredicate{SubPredicates: negated})
	case *predicates.OrPredicate:
		// NOT(OR(...)) → AND(NOT ..., NOT ..., ...).
		negated := make([]predicates.QueryPredicate, len(child.SubPredicates))
		for i, sp := range child.SubPredicates {
			negated[i] = &predicates.NotPredicate{Child: sp}
		}
		call.Yield(&predicates.AndPredicate{SubPredicates: negated})
	default:
		// NOT over a non-And/Or child — out of scope; let
		// NotConstantSimplifyRule / NotComparisonRewriteRule handle
		// leaves and double-negation.
	}
}

// NormalizationRules is the rule set to use when De Morgan
// distribution + nested-NOT push-down are desired in addition to the
// default simplification pass. Java's BooleanNormalizer applies
// these as a separate pre-CNF normalisation; callers wanting the
// same effect compose `Simplify(pred, NormalizationRules())` with the
// existing Simplify driver.
//
// Order matters:
//   - DeMorganRule comes BEFORE the default rules so the AND/OR
//     boundaries surface for the constant-fold / identity-drop
//     follow-on passes.
//   - DefaultSimplifyRules then collapses the resulting AND/OR/NOT
//     leaves further (NotComparisonRewriteRule turns NOT(=) into
//     <>, NotConstantSimplifyRule double-negates, etc.).
func NormalizationRules() []CascadesRule {
	out := []CascadesRule{NewDeMorganRule()}
	return append(out, DefaultSimplifyRules()...)
}
