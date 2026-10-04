package embedded

import (
	"strconv"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"google.golang.org/protobuf/proto"
)

// slidingWindowQualify is IndexPredicate.tryFromRowNumberPredicate over a
// view's QUALIFY: `ROW_NUMBER() OVER ([PARTITION BY f, ...] ORDER BY g) <= K`
// becomes the index's RowNumberWindowPredicate. The direction is always ASC,
// as Java stores it. It also returns the view's text without the clause,
// which is what compiles as a query.
func slidingWindowQualify(q antlrgen.IQueryContext, definition string) (*gen.Predicate, string, bool) {
	if q == nil || q.Ctes() != nil {
		return nil, "", false
	}
	term, ok := q.QueryExpressionBody().(*antlrgen.QueryTermDefaultContext)
	if !ok {
		return nil, "", false
	}
	st, ok := term.QueryTerm().(*antlrgen.SimpleTableContext)
	if !ok || st.QualifyClause() == nil {
		return nil, "", false
	}
	qc := st.QualifyClause()
	pe, ok := qc.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok || pe.Predicate() != nil {
		return nil, "", false
	}
	cmp, ok := pe.ExpressionAtom().(*antlrgen.BinaryComparisonPredicateContext)
	if !ok {
		return nil, "", false
	}
	if op, err := expr.ComparisonOpFromCtx(cmp.ComparisonOperator()); err != nil || op != predicates.ComparisonLessThanOrEq {
		return nil, "", false
	}
	size, ok := integerConstant(cmp.GetRight())
	if !ok {
		return nil, "", false
	}
	fc, ok := cmp.GetLeft().(*antlrgen.FunctionCallExpressionAtomContext)
	if !ok {
		return nil, "", false
	}
	naf, ok := fc.FunctionCall().(*antlrgen.NonAggregateFunctionCallContext)
	if !ok {
		return nil, "", false
	}
	win, ok := naf.NonAggregateWindowedFunction().(*antlrgen.NonAggregateWindowedFunctionContext)
	if !ok || win.ROW_NUMBER() == nil || win.OverClause() == nil || win.OverClause().WindowSpec() == nil {
		return nil, "", false
	}
	spec := win.OverClause().WindowSpec()
	rw := &gen.RowNumberWindowPredicate{Size: proto.Int32(int32(size)), Direction: gen.RowNumberWindowPredicate_ASC.Enum()}
	if pc := spec.PartitionClause(); pc != nil {
		for _, f := range pc.AllFullId() {
			if len(f.AllUid()) != 1 {
				return nil, "", false
			}
			rw.PartitionFields = append(rw.PartitionFields, &gen.FieldPath{Field: []string{functions.FullIdToName(f)}})
		}
	}
	ob, ok := spec.OrderByClause().(*antlrgen.OrderByClauseContext)
	if !ok || ob == nil || len(ob.AllOrderByExpression()) != 1 {
		return nil, "", false
	}
	obe := ob.AllOrderByExpression()[0]
	col, ok := obe.Expression().(*antlrgen.PredicatedExpressionContext)
	if !ok || col.Predicate() != nil {
		return nil, "", false
	}
	ref, ok := col.ExpressionAtom().(*antlrgen.FullColumnNameExpressionAtomContext)
	if !ok || len(ref.FullColumnName().FullId().AllUid()) != 1 {
		return nil, "", false
	}
	rw.OrderingField = []string{functions.FullIdToName(ref.FullColumnName().FullId())}
	runes := []rune(definition) // token offsets count code points
	text := string(runes[:qc.GetStart().GetStart()]) + string(runes[qc.GetStop().GetStop()+1:])
	return &gen.Predicate{RowNumberWindowPredicate: rw}, text, true
}

func integerConstant(a antlrgen.IExpressionAtomContext) (int64, bool) {
	c, ok := a.(*antlrgen.ConstantExpressionAtomContext)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(c.GetText(), 10, 32)
	return n, err == nil
}
