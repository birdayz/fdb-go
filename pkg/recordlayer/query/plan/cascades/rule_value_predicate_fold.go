package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ValuePredicateConstantFoldRule unwraps a ValuePredicate whose Value
// folds to a constant bool / null at plan time:
//
//	ValuePredicate{Value: ConstantValue(true)}    → ConstantPredicate(TriTrue)
//	ValuePredicate{Value: ConstantValue(false)}   → ConstantPredicate(TriFalse)
//	ValuePredicate{Value: NullValue}              → ConstantPredicate(TriUnknown)
//	ValuePredicate{Value: BooleanValue(true)}     → ConstantPredicate(TriTrue)
//	ValuePredicate{Value: BooleanValue(nil)}      → ConstantPredicate(TriUnknown)
//
// Mirrors Java's `ConstantFoldingValuePredicateRule`. Without this
// rule, SimplifyPredicateValues collapses the Value tree but leaves
// the ValuePredicate wrapper — `(true AND something)` stays
// `AND(VP(true), something)` instead of folding to just `something`
// via the AndConstantSimplifyRule pass.
//
// The rule fires AFTER SimplifyPredicateValues has run (or after
// other rules have folded the Value). It only operates on Values that
// IsConstantValue + EvaluateConstant accept; non-constant Values
// (FieldValue, ArithmeticValue with non-constant children) leave the
// wrapper alone.
//
// Type-degraded inputs (Value evaluates to a non-bool literal — e.g.
// `ConstantValue(int64(1))` wrapped in ValuePredicate) fold to
// TriUnknown, matching ValuePredicate.Eval's runtime degradation
// behaviour. This prevents the wrapper surviving past simplification
// when the embedded executor would also report UNKNOWN at runtime.
type ValuePredicateConstantFoldRule struct {
	matcher matching.BindingMatcher
}

// NewValuePredicateConstantFoldRule constructs the rule.
func NewValuePredicateConstantFoldRule() *ValuePredicateConstantFoldRule {
	return &ValuePredicateConstantFoldRule{matcher: newValuePredicateMatcher()}
}

func (r *ValuePredicateConstantFoldRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ValuePredicateConstantFoldRule) OnMatch(call *RuleCall) {
	vp := call.Bindings.Get(r.matcher).(*predicates.ValuePredicate)
	if vp.Value == nil {
		// Malformed predicate — leave for ValuePredicate.Explain's
		// nil-value rendering. Folding a nil Value to UNKNOWN would
		// silently swallow the structural error.
		return
	}
	lit, ok := values.EvaluateConstant(vp.Value)
	if !ok {
		// Value isn't constant — leave the predicate alone.
		return
	}
	switch v := lit.(type) {
	case bool:
		if v {
			call.Yield(predicates.NewConstantPredicate(predicates.TriTrue))
		} else {
			call.Yield(predicates.NewConstantPredicate(predicates.TriFalse))
		}
	case nil:
		call.Yield(predicates.NewConstantPredicate(predicates.TriUnknown))
	default:
		// Constant-but-non-bool literal: ValuePredicate.Eval would
		// degrade to UNKNOWN at runtime, so collapse to UNKNOWN at
		// plan time. The embedded executor reports the same shape.
		_ = v
		call.Yield(predicates.NewConstantPredicate(predicates.TriUnknown))
	}
}

func newValuePredicateMatcher() *predicateMatcher[*predicates.ValuePredicate] {
	return &predicateMatcher[*predicates.ValuePredicate]{rootType: "ValuePredicate"}
}
