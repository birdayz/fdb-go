package embedded

import (
	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
)

// rejectNormalizerFaults is the part of Java's AstNormalizer that refuses a
// statement before any of it is built. Java normalizes every statement before
// planning it (PlanGenerator.java:167), visiting the whole parse tree in tree
// order (visitChildren, AstNormalizer.java:176-188; a DDL statement's children,
// :315-318), and three of its visitors fail there:
//
//   - a limit clause, OFFSET before LIMIT within one clause, 0AF00 "OFFSET
//     clause is not supported." / "LIMIT clause is not supported."
//     (visitLimitClause, :254-262; the grammar admits OFFSET only after LIMIT,
//     so an OFFSET always comes with a LIMIT);
//   - an IN predicate whose list is a nested SELECT, 0AF00 "IN predicate does
//     not support nested SELECT" (visitInPredicate, :446-463, the assertion at
//     :458-461);
//   - an IN list item that is a bare NULL, 42809 "NULL values are not allowed
//     in the IN list" (rejectNullItems, :507-511).
//
// The order is Java's PRE-order: an IN predicate's two checks run at ENTRY,
// before any of its items is visited, so a LIMIT inside the nested SELECT is
// never reached, and a LIMIT inside an item loses to a bare NULL elsewhere in
// the same list even when that item comes first. `IN ?param` and `IN column`
// write no list and are not checked.
//
// Go runs it over a CREATE SCHEMA TEMPLATE statement, at the top of the one DDL
// front end, so the first fault in tree order wins over every clause of the
// template, including a faulty earlier table and clauses Go does not build yet
// (a view or function body). LIMIT is an approved Go extension for QUERIES
// only; a template statement is not a query (an index, view or function body
// has no row limit to honour), so the extension does not reach it.
func rejectNormalizerFaults(node antlr.Tree) error {
	switch n := node.(type) {
	case *antlrgen.LimitClauseContext:
		if n.GetOffset() != nil {
			return api.NewError(api.ErrCodeUnsupportedQuery, "OFFSET clause is not supported.")
		}
		if n.GetLimit() != nil {
			return api.NewError(api.ErrCodeUnsupportedQuery, "LIMIT clause is not supported.")
		}
		return nil
	case *antlrgen.InPredicateContext:
		if err := rejectInListFaults(n.InList()); err != nil {
			return err
		}
	}
	for i := 0; i < node.GetChildCount(); i++ {
		if err := rejectNormalizerFaults(node.GetChild(i)); err != nil {
			return err
		}
	}
	return nil
}

// rejectInListFaults is visitInPredicate's entry checks: the nested SELECT,
// then the bare NULL items, of a list written out in the statement.
func rejectInListFaults(in antlrgen.IInListContext) error {
	if in == nil || in.PreparedStatementParameter() != nil || in.FullColumnName() != nil {
		return nil
	}
	if in.QueryExpressionBody() != nil {
		return api.NewError(api.ErrCodeUnsupportedQuery, "IN predicate does not support nested SELECT")
	}
	exprs := in.Expressions()
	if exprs == nil {
		return nil
	}
	for _, item := range exprs.AllExpression() {
		if expr.IsBareNullLiteral(item) {
			return api.NewError(api.ErrCodeWrongObjectType, "NULL values are not allowed in the IN list")
		}
	}
	return nil
}
