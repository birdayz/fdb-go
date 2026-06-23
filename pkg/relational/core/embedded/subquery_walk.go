package embedded

import (
	"strings"

	"github.com/antlr4-go/antlr/v4"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// Shared parse-tree walkers used by the Cascades planner (plan_visitor.go,
// logical_predicate.go) for recursive-CTE self-reference detection and
// ORDER-BY scalar-subquery rejection. Kept after RFC-145 removed the legacy
// embedded interpreter — these are not executor code, they are tree-shape
// classifiers the planner relies on.

// containsTableRef reports whether the parse subtree references a
// table with the given uppercase name. Used by the recursive-CTE
// detection in plan_visitor.go to decide whether a CTE body actually
// self-references — the RECURSIVE keyword is a scope enabler (matches
// Postgres), so a non-self-referencing body is evaluated on the
// non-recursive path.
func containsTableRef(tree antlr.Tree, upperName string) bool {
	if tree == nil {
		return false
	}
	if tn, ok := tree.(antlrgen.ITableNameContext); ok {
		if strings.ToUpper(functions.FullIdToName(tn.FullId())) == upperName {
			return true
		}
	}
	for i := 0; i < tree.GetChildCount(); i++ {
		if containsTableRef(tree.GetChild(i), upperName) {
			return true
		}
	}
	return false
}

// walkScalarSubqueries recurses through an expression AST, invoking
// callback for every SubqueryExpressionAtomContext. Mirrors the atom
// shapes understood by the expression evaluators so a subquery nested
// inside arithmetic, comparison, function args, or parenthesis groups
// is not missed. Used by the Cascades planner to reject scalar
// subqueries in ORDER BY keys.
func walkScalarSubqueries(expr antlrgen.IExpressionContext, cb func(antlrgen.IQueryContext)) {
	if expr == nil {
		return
	}
	switch e := expr.(type) {
	case *antlrgen.PredicatedExpressionContext:
		walkScalarSubqueriesAtom(e.ExpressionAtom(), cb)
	case *antlrgen.LogicalExpressionContext:
		for i := 0; ; i++ {
			sub := e.Expression(i)
			if sub == nil {
				break
			}
			walkScalarSubqueries(sub, cb)
		}
	case *antlrgen.NotExpressionContext:
		walkScalarSubqueries(e.Expression(), cb)
	}
}

func walkScalarSubqueriesAtom(atom antlrgen.IExpressionAtomContext, cb func(antlrgen.IQueryContext)) {
	if atom == nil {
		return
	}
	switch a := atom.(type) {
	case *antlrgen.SubqueryExpressionAtomContext:
		cb(a.Query())
	case *antlrgen.MathExpressionAtomContext:
		walkScalarSubqueriesAtom(a.GetLeft(), cb)
		walkScalarSubqueriesAtom(a.GetRight(), cb)
	case *antlrgen.BitExpressionAtomContext:
		walkScalarSubqueriesAtom(a.GetLeft(), cb)
		walkScalarSubqueriesAtom(a.GetRight(), cb)
	case *antlrgen.BinaryComparisonPredicateContext:
		walkScalarSubqueriesAtom(a.GetLeft(), cb)
		walkScalarSubqueriesAtom(a.GetRight(), cb)
	case *antlrgen.RecordConstructorExpressionAtomContext:
		if rc := a.RecordConstructor(); rc != nil {
			for _, f := range rc.AllExpressionWithOptionalName() {
				walkScalarSubqueries(f.Expression(), cb)
			}
		}
	case *antlrgen.ArrayConstructorExpressionAtomContext:
		if ac := a.ArrayConstructor(); ac != nil {
			if exprs := ac.Expressions(); exprs != nil {
				for _, e := range exprs.AllExpression() {
					walkScalarSubqueries(e, cb)
				}
			}
		}
	case *antlrgen.FunctionCallExpressionAtomContext:
		// Function arguments may contain scalar subqueries (e.g.
		// UPPER((SELECT name FROM t WHERE id = 1))). Recurse into each.
		fc := a.FunctionCall()
		if fc == nil {
			return
		}
		switch f := fc.(type) {
		case *antlrgen.ScalarFunctionCallContext:
			if args := f.FunctionArgs(); args != nil {
				for _, fa := range args.AllFunctionArg() {
					walkScalarSubqueries(fa.Expression(), cb)
				}
			}
		case *antlrgen.UserDefinedScalarFunctionCallContext:
			if args := f.FunctionArgs(); args != nil {
				for _, fa := range args.AllFunctionArg() {
					walkScalarSubqueries(fa.Expression(), cb)
				}
			}
		}
	}
}
