package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// RelationalExpressionWithPredicates is the optional interface
// concrete RelationalExpressions implement when they carry a list of
// QueryPredicates as part of their node-information. Used by generic
// rules / visitors that want to manipulate predicates without caring
// about the operator class.
//
// Ports the marker interface from Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.RelationalExpressionWithPredicates`.
//
// Two operators implement it today:
//   - LogicalFilterExpression — its `queryPredicates` are the WHERE
//     conjuncts.
//   - SelectExpression — its `queryPredicates` are the SELECT-block
//     WHERE conjuncts.
//
// Other expressions do NOT implement this interface; type-assert at
// the rule callsite to opt into the generic predicate-walker.
type RelationalExpressionWithPredicates interface {
	RelationalExpression

	// GetPredicates returns the predicate list this expression
	// carries. Read-only; callers must not mutate.
	GetPredicates() []predicates.QueryPredicate
}

// Compile-time assertions — concrete predicate-bearing types
// satisfy the optional interface.
var (
	_ RelationalExpressionWithPredicates = (*LogicalFilterExpression)(nil)
	_ RelationalExpressionWithPredicates = (*SelectExpression)(nil)
)

// CountPredicates returns the number of QueryPredicates carried in
// e's node-information, or 0 if e doesn't implement
// RelationalExpressionWithPredicates. Convenience for rule-firing
// gates and analysis passes that branch on predicate density without
// caring about the operator class.
func CountPredicates(e RelationalExpression) int {
	if wp, ok := e.(RelationalExpressionWithPredicates); ok {
		return len(wp.GetPredicates())
	}
	return 0
}

// HasPredicates reports whether e is a predicate-bearing expression
// AND its predicate list is non-empty. Convenience that combines the
// type-assertion + emptiness check.
func HasPredicates(e RelationalExpression) bool {
	wp, ok := e.(RelationalExpressionWithPredicates)
	return ok && len(wp.GetPredicates()) > 0
}
