package query

import (
	"strings"

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
	t := &cascadesTranslator{
		cteScope: make(map[string]logical.LogicalOperator),
	}
	return t.translateRef(op)
}

type cascadesTranslator struct {
	cteScope map[string]logical.LogicalOperator
}

func (t *cascadesTranslator) translateRef(op logical.LogicalOperator) *expressions.Reference {
	expr := t.translateOp(op)
	if expr == nil {
		return nil
	}
	return expressions.InitialOf(expr)
}

func (t *cascadesTranslator) translateOp(op logical.LogicalOperator) expressions.RelationalExpression {
	if op == nil {
		return nil
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return t.translateScan(o)
	case *logical.LogicalFilter:
		return t.translateFilter(o)
	case *logical.LogicalLimit:
		return t.translateLimit(o)
	case *logical.LogicalUnion:
		return t.translateUnion(o)
	case *logical.LogicalSort:
		return t.translateSort(o)
	case *logical.LogicalProject:
		return t.translateProject(o)
	case *logical.LogicalJoin:
		return t.translateJoin(o)
	case *logical.LogicalAggregate:
		return t.translateAggregate(o)
	case *logical.LogicalDistinct:
		return t.translateDistinct(o)
	case *logical.LogicalCTE:
		return t.translateCTE(o)
	default:
		return nil
	}
}

func (t *cascadesTranslator) translateScan(s *logical.LogicalScan) expressions.RelationalExpression {
	key := strings.ToUpper(s.Table)
	if body, ok := t.cteScope[key]; ok {
		// Remove this CTE from scope while translating its body so that
		// scans inside the body resolve to real tables, not back to the
		// CTE itself (which would cause infinite recursion when the CTE
		// name shadows the underlying table name).
		delete(t.cteScope, key)
		result := t.translateOp(body)
		t.cteScope[key] = body
		return result
	}
	return expressions.NewFullUnorderedScanExpression(
		[]string{s.Table}, values.UnknownType)
}

func (t *cascadesTranslator) translateFilter(f *logical.LogicalFilter) expressions.RelationalExpression {
	innerRef := t.translateRef(f.Input)
	if innerRef == nil {
		return nil
	}
	if f.Predicate == nil && f.PredicateText != "" && len(t.cteScope) > 0 {
		// Text-only predicate on a query that involves CTE references.
		// The catalog-aware builder couldn't resolve column types for
		// the CTE table name. Bail so the planner falls back to naive
		// rather than silently dropping the filter.
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

func (t *cascadesTranslator) translateLimit(l *logical.LogicalLimit) expressions.RelationalExpression {
	innerRef := t.translateRef(l.Input)
	if innerRef == nil {
		return nil
	}
	return expressions.NewLogicalLimitExpression(
		l.Limit, l.Offset,
		expressions.ForEachQuantifier(innerRef),
	)
}

func (t *cascadesTranslator) translateUnion(u *logical.LogicalUnion) expressions.RelationalExpression {
	quantifiers := make([]expressions.Quantifier, 0, len(u.Inputs))
	for _, branch := range u.Inputs {
		ref := t.translateRef(branch)
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

func (t *cascadesTranslator) translateSort(s *logical.LogicalSort) expressions.RelationalExpression {
	innerRef := t.translateRef(s.Input)
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

func (t *cascadesTranslator) translateProject(p *logical.LogicalProject) expressions.RelationalExpression {
	innerRef := t.translateRef(p.Input)
	if innerRef == nil {
		return nil
	}
	projected := make([]values.Value, len(p.Projections))
	for i, col := range p.Projections {
		name := col
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			name = p.Aliases[i]
		}
		if isComputedExpression(col) {
			return nil
		}
		projected[i] = &values.FieldValue{Field: name, Typ: values.UnknownType}
	}
	return expressions.NewLogicalProjectionExpression(
		projected,
		expressions.ForEachQuantifier(innerRef),
	)
}

func (t *cascadesTranslator) translateDistinct(d *logical.LogicalDistinct) expressions.RelationalExpression {
	innerRef := t.translateRef(d.Input)
	if innerRef == nil {
		return nil
	}
	return expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(innerRef))
}

func (t *cascadesTranslator) translateAggregate(a *logical.LogicalAggregate) expressions.RelationalExpression {
	if a.Having != "" {
		return nil
	}
	innerRef := t.translateRef(a.Input)
	if innerRef == nil {
		return nil
	}
	groupKeys := make([]values.Value, len(a.GroupKeys))
	for i, key := range a.GroupKeys {
		groupKeys[i] = &values.FieldValue{Field: key, Typ: values.UnknownType}
	}
	aggSpecs := make([]expressions.AggregateSpec, 0, len(a.Aggregates))
	for _, aggText := range a.Aggregates {
		spec, ok := parseAggregateText(aggText)
		if !ok {
			return nil
		}
		aggSpecs = append(aggSpecs, spec)
	}
	return expressions.NewGroupByExpression(
		groupKeys,
		aggSpecs,
		expressions.ForEachQuantifier(innerRef),
	)
}

func parseAggregateText(text string) (expressions.AggregateSpec, bool) {
	upper := strings.ToUpper(strings.TrimSpace(text))
	lparen := strings.Index(upper, "(")
	if lparen < 0 {
		return expressions.AggregateSpec{}, false
	}
	rparen := strings.LastIndex(upper, ")")
	if rparen < lparen {
		return expressions.AggregateSpec{}, false
	}
	funcName := strings.TrimSpace(upper[:lparen])
	operandText := strings.TrimSpace(upper[lparen+1 : rparen])

	var fn expressions.AggregateFunction
	switch funcName {
	case "COUNT":
		fn = expressions.AggCount
	case "SUM":
		fn = expressions.AggSum
	case "MIN":
		fn = expressions.AggMin
	case "MAX":
		fn = expressions.AggMax
	case "AVG":
		fn = expressions.AggAvg
	default:
		return expressions.AggregateSpec{}, false
	}

	var operand values.Value
	if operandText == "*" {
		operand = &values.ConstantValue{Value: nil, Typ: values.UnknownType}
	} else {
		operand = &values.FieldValue{Field: operandText, Typ: values.UnknownType}
	}

	return expressions.AggregateSpec{Function: fn, Operand: operand}, true
}

func (t *cascadesTranslator) translateJoin(j *logical.LogicalJoin) expressions.RelationalExpression {
	if j.Kind != logical.JoinInner {
		return nil
	}
	leftRef := t.translateRef(j.Left)
	if leftRef == nil {
		return nil
	}
	rightRef := t.translateRef(j.Right)
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

func (t *cascadesTranslator) translateCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	if c.Recursive {
		return nil
	}
	t.cteScope[strings.ToUpper(c.Name)] = c.Body
	result := t.translateOp(c.Main)
	delete(t.cteScope, strings.ToUpper(c.Name))
	return result
}

func isComputedExpression(col string) bool {
	for _, c := range col {
		switch c {
		case '(', '+', '-', '*', '/', '%':
			return true
		}
	}
	return false
}
