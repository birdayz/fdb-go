package cascades

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// indexPredicateToQueryPredicate is the Go port of
// IndexPredicate.toPredicate(Value) (IndexPredicate.java:304-305 and each
// PoJo's override): it rebuilds the QueryPredicate a stored sparse-index
// predicate denotes, with every field path rooted at the candidate's base
// quantifier value — Java's
// index.getPredicate().toPredicate(baseQuantifier.getFlowedObjectValue())
// (ValueIndexExpansionVisitor.java:140). The result is attached to the index's
// match candidate so a query can only match the candidate when the matcher
// can account for the predicate.
//
// A RowNumberWindowPredicate converts to TRUE exactly as Java's override does
// (IndexPredicate.java:770-771): the window predicate constrains which rows
// the maintainer keeps, not which rows a matched scan may serve.
func indexPredicateToQueryPredicate(p *gen.Predicate, base values.Value) (predicates.QueryPredicate, error) {
	switch {
	case p == nil:
		return nil, fmt.Errorf("nil predicate proto")
	case p.GetAndPredicate() != nil:
		children, err := indexPredicateChildren(p.GetAndPredicate().GetChildren(), base)
		if err != nil {
			return nil, err
		}
		return &predicates.AndPredicate{SubPredicates: children}, nil
	case p.GetOrPredicate() != nil:
		children, err := indexPredicateChildren(p.GetOrPredicate().GetChildren(), base)
		if err != nil {
			return nil, err
		}
		return &predicates.OrPredicate{SubPredicates: children}, nil
	case p.GetNotPredicate() != nil:
		child, err := indexPredicateToQueryPredicate(p.GetNotPredicate().GetChild(), base)
		if err != nil {
			return nil, err
		}
		return predicates.NewNot(child), nil
	case p.GetConstantPredicate() != nil:
		switch p.GetConstantPredicate().GetValue() {
		case gen.ConstantPredicate_TRUE:
			return predicates.NewConstantPredicate(predicates.TriTrue), nil
		case gen.ConstantPredicate_FALSE:
			return predicates.NewConstantPredicate(predicates.TriFalse), nil
		case gen.ConstantPredicate_NULL:
			return predicates.NewConstantPredicate(predicates.TriUnknown), nil
		default:
			return nil, fmt.Errorf("unknown constant predicate value %v", p.GetConstantPredicate().GetValue())
		}
	case p.GetValuePredicate() != nil:
		return indexValuePredicateToQuery(p.GetValuePredicate(), base)
	case p.GetRowNumberWindowPredicate() != nil:
		return predicates.NewConstantPredicate(predicates.TriTrue), nil
	default:
		return nil, fmt.Errorf("unsupported predicate proto %v", p)
	}
}

func indexPredicateChildren(children []*gen.Predicate, base values.Value) ([]predicates.QueryPredicate, error) {
	out := make([]predicates.QueryPredicate, 0, len(children))
	for _, c := range children {
		q, err := indexPredicateToQueryPredicate(c, base)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, nil
}

// indexValuePredicateToQuery is ValuePredicate.toPredicate
// (IndexPredicate.java:598-600): FieldValue.ofFieldNames(base, fieldPath) with
// the comparison from IndexComparison.toComparison.
func indexValuePredicateToQuery(vp *gen.ValuePredicate, base values.Value) (predicates.QueryPredicate, error) {
	path := vp.GetValue()
	if len(path) == 0 {
		return nil, fmt.Errorf("value predicate without field path")
	}
	// Nested paths are field-of-field chains, exactly like the query side's
	// dotted access.
	v := base
	for _, name := range path {
		v = values.NewFieldValue(v, name, values.UnknownType)
	}
	cmp, err := indexComparisonToQuery(vp.GetComparison())
	if err != nil {
		return nil, err
	}
	return predicates.NewComparisonPredicate(v, cmp), nil
}

// indexComparisonToQuery is IndexComparison.toComparison: the SimpleComparison
// arm (IndexComparison.java:265-303) and the NullComparison arm (:331-333).
func indexComparisonToQuery(c *gen.Comparison) (predicates.Comparison, error) {
	if c == nil {
		return predicates.Comparison{}, fmt.Errorf("value predicate without comparison")
	}
	if nc := c.GetNullComparison(); nc != nil {
		if nc.GetIsNull() {
			return predicates.Comparison{Type: predicates.ComparisonIsNull}, nil
		}
		return predicates.Comparison{Type: predicates.ComparisonIsNotNull}, nil
	}
	sc := c.GetSimpleComparison()
	if sc == nil {
		return predicates.Comparison{}, fmt.Errorf("unsupported comparison proto %v", c)
	}
	var typ predicates.ComparisonType
	switch sc.GetType() {
	case gen.ComparisonType_EQUALS:
		typ = predicates.ComparisonEquals
	case gen.ComparisonType_NOT_EQUALS:
		typ = predicates.ComparisonNotEquals
	case gen.ComparisonType_LESS_THAN:
		typ = predicates.ComparisonLessThan
	case gen.ComparisonType_LESS_THAN_OR_EQUALS:
		typ = predicates.ComparisonLessThanOrEq
	case gen.ComparisonType_GREATER_THAN:
		typ = predicates.ComparisonGreaterThan
	case gen.ComparisonType_GREATER_THAN_OR_EQUALS:
		typ = predicates.ComparisonGreaterThanEq
	case gen.ComparisonType_STARTS_WITH:
		typ = predicates.ComparisonStartsWith
	case gen.ComparisonType_NOT_NULL:
		return predicates.Comparison{Type: predicates.ComparisonIsNotNull}, nil
	case gen.ComparisonType_IS_NULL:
		return predicates.Comparison{Type: predicates.ComparisonIsNull}, nil
	default:
		return predicates.Comparison{}, fmt.Errorf("unsupported comparison type %v", sc.GetType())
	}
	return predicates.Comparison{
		Type:    typ,
		Operand: values.LiteralValue(literalFromProtoValue(sc.GetOperand())),
	}, nil
}

// literalFromProtoValue mirrors LiteralKeyExpression.fromProtoValue
// (LiteralKeyExpression.java:141-171): the first set field wins, in Java's
// probe order.
func literalFromProtoValue(v *gen.Value) any {
	switch {
	case v == nil:
		return nil
	case v.LongValue != nil:
		return v.GetLongValue()
	case v.IntValue != nil:
		return v.GetIntValue()
	case v.DoubleValue != nil:
		return v.GetDoubleValue()
	case v.FloatValue != nil:
		return v.GetFloatValue()
	case v.BoolValue != nil:
		return v.GetBoolValue()
	case v.StringValue != nil:
		return v.GetStringValue()
	case v.BytesValue != nil:
		return v.GetBytesValue()
	default:
		return nil
	}
}
