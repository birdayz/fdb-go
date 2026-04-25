package cascades

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
	matcher BindingMatcher
}

// NewValuePredicateConstantFoldRule constructs the rule.
func NewValuePredicateConstantFoldRule() *ValuePredicateConstantFoldRule {
	return &ValuePredicateConstantFoldRule{matcher: newValuePredicateMatcher()}
}

func (r *ValuePredicateConstantFoldRule) Matcher() BindingMatcher { return r.matcher }

func (r *ValuePredicateConstantFoldRule) OnMatch(call *RuleCall) {
	vp := call.Bindings.Get(r.matcher).(*ValuePredicate)
	if vp.Value == nil {
		// Malformed predicate — leave for ValuePredicate.Explain's
		// nil-value rendering. Folding a nil Value to UNKNOWN would
		// silently swallow the structural error.
		return
	}
	lit, ok := EvaluateConstant(vp.Value)
	if !ok {
		// Value isn't constant — leave the predicate alone.
		return
	}
	switch v := lit.(type) {
	case bool:
		if v {
			call.Yield(NewConstantPredicate(TriTrue))
		} else {
			call.Yield(NewConstantPredicate(TriFalse))
		}
	case nil:
		call.Yield(NewConstantPredicate(TriUnknown))
	default:
		// Constant-but-non-bool literal: ValuePredicate.Eval would
		// degrade to UNKNOWN at runtime, so collapse to UNKNOWN at
		// plan time. The embedded executor reports the same shape.
		_ = v
		call.Yield(NewConstantPredicate(TriUnknown))
	}
}

// The `_ bool` field forces non-zero struct size so two
// `new(valuePredicateMatcher)` allocations land at distinct heap
// addresses (see AnyValue at matcher.go:130-136 for the zero-size-
// struct gotcha). No nonce is needed for actual identity tracking.
type valuePredicateMatcher struct{ _ bool }

func newValuePredicateMatcher() *valuePredicateMatcher {
	return &valuePredicateMatcher{}
}

func (*valuePredicateMatcher) RootType() string { return "ValuePredicate" }
func (m *valuePredicateMatcher) BindMatches(outer *PlannerBindings, in any) []*PlannerBindings {
	if _, ok := in.(*ValuePredicate); !ok {
		return nil
	}
	return []*PlannerBindings{outer.Bind(m, in)}
}
