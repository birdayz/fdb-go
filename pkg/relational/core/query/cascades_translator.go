package query

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// TranslateToCascades converts a logical.LogicalOperator tree into a
// cascades RelationalExpression tree rooted in a Reference. This is
// the bridge between the SQL parser's logical plan and the Cascades
// optimizer.
//
// Returns the root Reference suitable for passing to Planner.Plan().
// Returns nil if the operator tree contains shapes that can't be
// translated (unsupported operators fall through to nil).
func TranslateToCascades(op logical.LogicalOperator) *expressions.Reference {
	expr := translateOp(op)
	if expr == nil {
		return nil
	}
	return expressions.InitialOf(expr)
}

func translateOp(op logical.LogicalOperator) expressions.RelationalExpression {
	if op == nil {
		return nil
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return translateScan(o)
	case *logical.LogicalFilter:
		return translateFilter(o)
	case *logical.LogicalLimit:
		return translateLimit(o)
	case *logical.LogicalUnion:
		return translateUnion(o)
	case *logical.LogicalSort:
		return translateSort(o)
	case *logical.LogicalProject:
		return translateProject(o)
	case *logical.LogicalJoin:
		return translateJoin(o)
	default:
		return nil
	}
}

func translateScan(s *logical.LogicalScan) expressions.RelationalExpression {
	return expressions.NewFullUnorderedScanExpression(
		[]string{s.Table}, values.UnknownType)
}

func translateFilter(f *logical.LogicalFilter) expressions.RelationalExpression {
	innerRef := TranslateToCascades(f.Input)
	if innerRef == nil {
		return nil
	}
	var preds []predicates.QueryPredicate
	if f.Predicate != nil {
		preds = []predicates.QueryPredicate{f.Predicate}
	}
	return expressions.NewLogicalFilterExpression(
		preds,
		expressions.ForEachQuantifier(innerRef),
	)
}

func translateLimit(l *logical.LogicalLimit) expressions.RelationalExpression {
	innerRef := TranslateToCascades(l.Input)
	if innerRef == nil {
		return nil
	}
	return expressions.NewLogicalLimitExpression(
		l.Limit, l.Offset,
		expressions.ForEachQuantifier(innerRef),
	)
}

func translateUnion(u *logical.LogicalUnion) expressions.RelationalExpression {
	quantifiers := make([]expressions.Quantifier, 0, len(u.Inputs))
	for _, branch := range u.Inputs {
		ref := TranslateToCascades(branch)
		if ref == nil {
			return nil
		}
		quantifiers = append(quantifiers, expressions.ForEachQuantifier(ref))
	}
	union := expressions.NewLogicalUnionExpression(quantifiers)
	if u.Distinct {
		unionRef := expressions.InitialOf(union)
		return expressions.NewLogicalDistinctExpression(
			expressions.ForEachQuantifier(unionRef))
	}
	return union
}

func translateSort(s *logical.LogicalSort) expressions.RelationalExpression {
	innerRef := TranslateToCascades(s.Input)
	if innerRef == nil {
		return nil
	}
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		sortKeys[i] = expressions.SortKey{
			Value:   &values.FieldValue{Field: k.Expr, Typ: values.UnknownType},
			Reverse: k.Dir == logical.SortDesc,
		}
	}
	return expressions.NewLogicalSortExpression(
		sortKeys,
		expressions.ForEachQuantifier(innerRef),
	)
}

func translateProject(p *logical.LogicalProject) expressions.RelationalExpression {
	innerRef := TranslateToCascades(p.Input)
	if innerRef == nil {
		return nil
	}
	projected := make([]values.Value, len(p.Projections))
	for i, col := range p.Projections {
		name := col
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			name = p.Aliases[i]
		}
		projected[i] = &values.FieldValue{Field: name, Typ: values.UnknownType}
	}
	return expressions.NewLogicalProjectionExpression(
		projected,
		expressions.ForEachQuantifier(innerRef),
	)
}

func translateJoin(j *logical.LogicalJoin) expressions.RelationalExpression {
	leftRef := TranslateToCascades(j.Left)
	if leftRef == nil {
		return nil
	}
	rightRef := TranslateToCascades(j.Right)
	if rightRef == nil {
		return nil
	}
	leftQ := expressions.ForEachQuantifier(leftRef)
	rightQ := expressions.ForEachQuantifier(rightRef)

	var preds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			preds = []predicates.QueryPredicate{qp}
		}
	}

	resultValue := values.NewQuantifiedObjectValue(leftQ.GetAlias())
	return expressions.NewSelectExpression(
		resultValue,
		[]expressions.Quantifier{leftQ, rightQ},
		preds,
	)
}
