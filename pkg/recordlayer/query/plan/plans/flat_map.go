package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFlatMapPlan represents a correlated nested-loop join where
// for each outer row, the inner plan is re-executed with the outer row
// bound as a correlation. Mirrors Java's RecordQueryFlatMapPlan which
// uses FlatMapPipelinedCursor for execution.
//
// The key difference from RecordQueryNestedLoopJoinPlan: the inner plan
// is parameterized by the outer row via correlation bindings. This
// enables targeted index probes on the inner side (O(N×logM) vs O(N×M)).
// LEFT-OUTER note: the plan carries NO leftOuter flag. LEFT-OUTER semantics are
// emergent from the inner being wrapped in DefaultOnEmpty (whose OrElse
// continuation makes the null-extension resume-safe), exactly like Java's
// RecordQueryFlatMapPlan — see rule_implement_nested_loop_join.go's lowering. An
// earlier in-memory leftOuter/innerHadMatch flag pair re-decided the extension per
// page and was the F2 spurious-null resume bug; it was removed as dead code.
//
// The two legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQueryFlatMapPlan`'s `Quantifier.Physical outerQuantifier` /
// `innerQuantifier`). The raw `outer`/`inner` pointers they replace were a
// second storage location for the same edges. They stay two separately-named
// fields rather than a slice because the accessors, the Explain rendering and
// the executor all address them by ROLE, not by position. RFC-183 P5 step 2.
type RecordQueryFlatMapPlan struct {
	PlanExprBase
	outerQ                       expressions.Quantifier
	innerQ                       expressions.Quantifier
	outerAlias                   values.CorrelationIdentifier
	innerAlias                   values.CorrelationIdentifier
	resultValue                  values.Value
	inheritOuterRecordProperties bool
}

func NewRecordQueryFlatMapPlan(
	outer, inner RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	inheritOuterRecordProperties bool,
) *RecordQueryFlatMapPlan {
	return &RecordQueryFlatMapPlan{
		outerQ:                       QuantifierOverPlan(outer),
		innerQ:                       QuantifierOverPlan(inner),
		outerAlias:                   outerAlias,
		innerAlias:                   innerAlias,
		resultValue:                  resultValue,
		inheritOuterRecordProperties: inheritOuterRecordProperties,
	}
}

func (p *RecordQueryFlatMapPlan) GetResultType() values.Type { return values.UnknownType }

// GetChildren returns the outer leg then the inner leg, dereferenced through
// the quantifiers. The pair is always two entries wide — a nil leg stays a nil
// entry rather than shrinking the arity, which is what the executor's
// positional child handling expects.
func (p *RecordQueryFlatMapPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.GetOuter(), p.GetInner()}
}

// GetQuantifiers reports the real leg quantifiers in GetChildren order
// (outer, inner), overriding PlanExprBase's none. That order is what
// WithQuantifiers indexes into.
func (p *RecordQueryFlatMapPlan) GetQuantifiers() []expressions.Quantifier {
	if p.outerQ.GetRangesOver() == nil || p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.outerQ, p.innerQ}
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers, in
// GetQuantifiers order. The receiver is never mutated, which is what keeps a
// memoized plan safe to share.
func (p *RecordQueryFlatMapPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 2 {
		return p
	}
	cp := *p
	cp.outerQ = qs[0]
	cp.innerQ = qs[1]
	return &cp
}

func (p *RecordQueryFlatMapPlan) GetOuter() RecordQueryPlan                   { return planFromQuantifier(p.outerQ) }
func (p *RecordQueryFlatMapPlan) GetInner() RecordQueryPlan                   { return planFromQuantifier(p.innerQ) }
func (p *RecordQueryFlatMapPlan) GetOuterAlias() values.CorrelationIdentifier { return p.outerAlias }
func (p *RecordQueryFlatMapPlan) GetInnerAlias() values.CorrelationIdentifier { return p.innerAlias }
func (p *RecordQueryFlatMapPlan) GetResultValue() values.Value                { return p.resultValue }

func (p *RecordQueryFlatMapPlan) InheritOuterRecordProperties() bool {
	return p.inheritOuterRecordProperties
}

// structuralKey lists the fields that distinguish this FlatMap in the memo: the
// outer/inner correlation aliases, the inheritOuterRecordProperties flag, and
// the resultValue. Children (the two legs) are excluded. Java folds
// inheritOuterRecordProperties into computeHashCodeWithoutChildren but omits it
// from equals — Go compares it in BOTH so the equal⟹same-hash memo invariant
// holds (two plans differing only in that flag null-extend differently; they
// are not interchangeable). The resultValue uses semantic Value identity — Java
// RecordQueryFlatMapPlan.equalsWithoutChildren is semanticEqualsForResults. The
// same key drives both EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryFlatMapPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Alias(p.outerAlias).
		Alias(p.innerAlias).
		Bool(p.inheritOuterRecordProperties).
		Value(p.resultValue)
}

func (p *RecordQueryFlatMapPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFlatMapPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryFlatMapPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("flatmap|")
}

// Explain renders both legs UNGUARDED, exactly as the raw-pointer form did: a
// nil leg panics here rather than rendering "<nil>". That is deliberate — a
// FlatMap with a missing leg is not a renderable plan, and quietly printing a
// placeholder would hide it. Whether to soften this is a separate decision.
func (p *RecordQueryFlatMapPlan) Explain() string {
	return fmt.Sprintf("FlatMap(outer=%s, inner=%s)", p.GetOuter().Explain(), p.GetInner().Explain())
}

var (
	_ RecordQueryPlan                  = (*RecordQueryFlatMapPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryFlatMapPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryFlatMapPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// CanCorrelate reports that this operator anchors a correlation between its
// children (the outer leg binds the value the inner leg reads), mirroring physicalFlatMapWrapper.
func (p *RecordQueryFlatMapPlan) CanCorrelate() bool { return true }

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryFlatMapPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
