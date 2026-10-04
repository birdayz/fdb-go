package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/matching"

// conditionalImplementationRule is Java's ConditionalImplementationCascadesRule:
// its rules share one root and are tried in order on a (group, expression)
// pair, each only when every rule before it made no progress. It never matches
// on its own; TransformImplTask runs the inner rules.
type conditionalImplementationRule struct {
	rules []ImplementationRule
}

func newConditionalImplementationRule(rules ...ImplementationRule) *conditionalImplementationRule {
	return &conditionalImplementationRule{rules: rules}
}

func (r *conditionalImplementationRule) Matcher() matching.BindingMatcher {
	return r.rules[0].Matcher()
}

func (r *conditionalImplementationRule) OnMatch(call *ImplementationRuleCall) {
	call.Fail(errConditionalRuleMatched)
}

// OnlyOnPrunedInputs: Java requires the inner rules to agree.
func (r *conditionalImplementationRule) OnlyOnPrunedInputs() bool {
	return isPrunedInputsRule(r.rules[0])
}

// conditionalExpressionRule is Java's ConditionalExplorationCascadesRule, run
// by TransformExprTask the same way.
type conditionalExpressionRule struct {
	rules []ExpressionRule
}

func newConditionalExpressionRule(rules ...ExpressionRule) *conditionalExpressionRule {
	return &conditionalExpressionRule{rules: rules}
}

func (r *conditionalExpressionRule) Matcher() matching.BindingMatcher { return r.rules[0].Matcher() }

func (r *conditionalExpressionRule) OnMatch(call *ExpressionRuleCall) {
	call.Fail(errConditionalRuleMatched)
}

type conditionalRuleMatchedError struct{}

func (conditionalRuleMatchedError) Error() string {
	return "a conditional rule only groups rules for the planner to schedule; it never matches"
}

var errConditionalRuleMatched error = conditionalRuleMatchedError{}

// enabledImplementationRules drops the disabled rules by Java simple name,
// filtering a conditional rule's inner rules and dropping it once empty.
func enabledImplementationRules(rules []ImplementationRule, disabled map[string]struct{}) []ImplementationRule {
	out := rules[:0:0]
	for _, r := range rules {
		if cond, ok := r.(*conditionalImplementationRule); ok {
			var inner []ImplementationRule
			for _, ir := range cond.rules {
				if _, off := disabled[shortTypeName(ir)]; !off {
					inner = append(inner, ir)
				}
			}
			if len(inner) == len(cond.rules) {
				out = append(out, r)
			} else if len(inner) > 0 {
				out = append(out, newConditionalImplementationRule(inner...))
			}
			continue
		}
		if _, off := disabled[shortTypeName(r)]; !off {
			out = append(out, r)
		}
	}
	return out
}

// enabledExpressionRules is enabledImplementationRules for exploration rules.
func enabledExpressionRules(rules []ExpressionRule, disabled map[string]struct{}) []ExpressionRule {
	out := rules[:0:0]
	for _, r := range rules {
		if cond, ok := r.(*conditionalExpressionRule); ok {
			var inner []ExpressionRule
			for _, er := range cond.rules {
				if _, off := disabled[shortTypeName(er)]; !off {
					inner = append(inner, er)
				}
			}
			if len(inner) == len(cond.rules) {
				out = append(out, r)
			} else if len(inner) > 0 {
				out = append(out, newConditionalExpressionRule(inner...))
			}
			continue
		}
		if _, off := disabled[shortTypeName(r)]; !off {
			out = append(out, r)
		}
	}
	return out
}
