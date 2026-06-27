package properties

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// DynamicTypeProvider is the optional interface an expression
// implements to expose its dynamic types. Matches Java's
// RelationalExpression.getDynamicTypes().
type DynamicTypeProvider interface {
	GetDynamicTypes() []values.Type
}

// EvaluateUsedTypes walks the expression tree bottom-up and collects
// all dynamic (complex) Type objects used anywhere in the tree.
// Matches Java's UsedTypesProperty.evaluate.
//
// Expressions that implement DynamicTypeProvider contribute their
// types directly; all children's types are unioned.
func EvaluateUsedTypes(expr expressions.RelationalExpression) []values.Type {
	if expr == nil {
		return nil
	}
	seen := make(map[values.Type]struct{})
	evaluateUsedTypesRec(expr, seen)
	result := make([]values.Type, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	return result
}

func evaluateUsedTypesRec(expr expressions.RelationalExpression, seen map[values.Type]struct{}) {
	if expr == nil {
		return
	}
	// Collect dynamic types from this expression.
	if dtp, ok := expr.(DynamicTypeProvider); ok {
		for _, t := range dtp.GetDynamicTypes() {
			seen[t] = struct{}{}
		}
	}
	// Recurse into children.
	for _, q := range expr.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			evaluateUsedTypesRec(m, seen)
		}
	}
}
