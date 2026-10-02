package cascades

import (
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// Simplify visits children before applying rules, as Java's Simplification does.
// Rules that create new child expressions must request re-exploration.
func Simplify(pred predicates.QueryPredicate, rules []CascadesRule) (predicates.QueryPredicate, error) {
	return simplifyWithReExploration(pred, rules, true, nil)
}

func simplifyWithReExploration(pred predicates.QueryPredicate, rules []CascadesRule, isRoot bool, childResults map[predicates.QueryPredicate]predicates.QueryPredicate) (predicates.QueryPredicate, error) {
	if pred == nil || len(rules) == 0 {
		return pred, nil
	}
	original := pred
	if !isRoot && childResults != nil {
		if result := childResults[pred]; result != nil {
			return result, nil
		}
	}
	for {
		var err error
		pred, err = simplifyPredicateChildren(pred, rules, childResults)
		if err != nil {
			return nil, err
		}
		for {
			next, reExplore, err := applyRulesOnce(pred, rules, isRoot)
			if err != nil {
				return nil, err
			}
			if next == pred {
				if _, registered := childResults[original]; !isRoot && registered {
					childResults[original] = pred
				}
				return pred, nil
			}
			pred = next
			if reExplore {
				break
			}
		}
	}
}

func simplifyPredicateChildren(pred predicates.QueryPredicate, rules []CascadesRule, childResults map[predicates.QueryPredicate]predicates.QueryPredicate) (predicates.QueryPredicate, error) {
	children := pred.Children()
	if len(children) == 0 {
		return pred, nil
	}
	var simpler []predicates.QueryPredicate
	for i, child := range children {
		next, err := simplifyWithReExploration(child, rules, false, childResults)
		if err != nil {
			return nil, err
		}
		if next != child && simpler == nil {
			simpler = make([]predicates.QueryPredicate, len(children))
			copy(simpler, children)
		}
		if simpler != nil {
			simpler[i] = next
		}
	}
	if simpler == nil {
		return pred, nil
	}
	// Child replacement preserves atomicity; a rule may deliberately rebuild it away.
	switch p := pred.(type) {
	case *predicates.AndPredicate:
		clone := *p
		clone.SubPredicates = simpler
		return &clone, nil
	case *predicates.OrPredicate:
		clone := *p
		clone.SubPredicates = simpler
		return &clone, nil
	case *predicates.NotPredicate:
		clone := *p
		clone.Child = simpler[0]
		return &clone, nil
	default:
		return pred, nil
	}
}

// Root-only rules expose their scope before binding to avoid allocating child calls
// that their OnMatch must decline (Java's NormalFormRule.isRoot guard).
type rootOnlySimplificationRule interface{ rootOnly() bool }

// applyRulesOnce returns the first replacement; unchanged identity ends the fixpoint.
func applyRulesOnce(pred predicates.QueryPredicate, rules []CascadesRule, isRoot bool) (predicates.QueryPredicate, bool, error) {
	rootType := reflect.TypeOf(pred)
	for _, rule := range rules {
		if scoped, ok := rule.(rootOnlySimplificationRule); !isRoot && ok && scoped.rootOnly() {
			continue
		}
		matcher := rule.Matcher()
		if typed, ok := matcher.(matching.RootOperatorMatcher); ok {
			if root := typed.RootOperator(); root != nil && root != rootType {
				continue
			}
		}
		matches := matcher.BindMatches(matching.NewBindings(), pred)
		for _, b := range matches {
			call := &RuleCall{Bindings: b, isRoot: isRoot}
			rule.OnMatch(call)
			if err := call.Err(); err != nil {
				return nil, false, err
			}
			if ys := call.Yielded(); len(ys) > 0 {
				if qp, ok := ys[0].(predicates.QueryPredicate); ok {
					return qp, call.reExplore, nil
				}
			}
		}
	}
	return pred, false, nil
}

// queryPredicateSimplificationRules mirrors DefaultQueryPredicateRuleSet.
// Absorption, not eager flattening or deduplication, determines surviving positions.
func queryPredicateSimplificationRules() []CascadesRule {
	return []CascadesRule{
		NewOrConstantSimplifyRule(),
		NewAndConstantSimplifyRule(),
		NewAndAbsorbOrRule(),
		NewOrAbsorbAndRule(),
		NewNotComparisonRewriteRule(),
		NewDeMorganRule(),
	}
}

// DefaultSimplifyRules includes constant evaluation for null-substitution proofs.
// Planner predicate rewrites use queryPredicateSimplificationRules instead.
func DefaultSimplifyRules() []CascadesRule {
	return []CascadesRule{
		NewAndFlattenRule(),
		NewOrFlattenRule(),
		NewComparisonConstantSimplifyRule(),
		NewNotConstantSimplifyRule(),
		NewAndConstantSimplifyRule(),
		NewOrConstantSimplifyRule(),
		NewAndDedupRule(),
		NewOrDedupRule(),
		NewAndAbsorbOrRule(),
		NewOrAbsorbAndRule(),
		NewNotComparisonRewriteRule(),
		NewValuePredicateConstantFoldRule(),
	}
}
