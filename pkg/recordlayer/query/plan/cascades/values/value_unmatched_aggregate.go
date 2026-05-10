package values

// UnmatchedAggregateValue is a non-evaluable marker value that stands
// for an aggregate expression not yet matched to a candidate during
// index matching. It carries a unique CorrelationIdentifier (the
// "unmatched ID") that links back to the original query aggregate
// via the GroupByMappings.unmatchedAggregatesMap.
//
// During Compensation.Intersect, when an aggregate that was previously
// unmatched becomes matched (because a second index covers it), the
// replaceUnmatchedAggregateValues function replaces these markers with
// the actual translated aggregate value.
//
// Ports Java's GroupByExpression.UnmatchedAggregateValue.
type UnmatchedAggregateValue struct {
	UnmatchedID CorrelationIdentifier
}

func NewUnmatchedAggregateValue(id CorrelationIdentifier) *UnmatchedAggregateValue {
	return &UnmatchedAggregateValue{UnmatchedID: id}
}

func (*UnmatchedAggregateValue) Children() []Value { return nil }
func (*UnmatchedAggregateValue) Name() string      { return "unmatched_aggregate" }
func (*UnmatchedAggregateValue) Type() Type        { return UnknownType }

func (*UnmatchedAggregateValue) Evaluate(_ any) any {
	panic("UnmatchedAggregateValue is a non-evaluable marker")
}

func (*UnmatchedAggregateValue) IsNonEvaluable() bool { return true }

func (v *UnmatchedAggregateValue) GetCorrelatedTo() map[CorrelationIdentifier]struct{} {
	return map[CorrelationIdentifier]struct{}{v.UnmatchedID: {}}
}

// UniqueUnmatchedID generates a fresh CorrelationIdentifier for a new
// unmatched aggregate. Mirrors Java's UnmatchedAggregateValue.uniqueId().
func UniqueUnmatchedID() CorrelationIdentifier {
	return UniqueCorrelationIdentifier()
}
