package embedded

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// Uncorrelated scalar subquery pre-evaluation and lookup.
//
// `(SELECT col FROM t WHERE ...)` embedded inside a larger SELECT's
// projection / WHERE / HAVING / ORDER BY runs once BEFORE the outer
// scan's FDB transaction opens, and the result is cached on the
// connection. This keeps inner subqueries from re-entering runInTx
// while the outer cursor is mid-scan, which breaks nested FDB tx
// semantics.
//
// preEvaluateScalarSubqueries walks the outer query's expressions
// once, finds every SubqueryExpressionAtomContext (via
// walkScalarSubqueries / walkScalarSubqueriesAtom), executes each
// uncached subquery via runScalarSubqueryOnce, and stores the
// scalar result in c.scalarSubqueryCache keyed by the inner
// QueryContext. Per-row evaluation later just does a map lookup.
//
// SQL-standard validation: exactly one column (else 42601), at most
// one row (else 21000 cardinality violation), zero rows → NULL.
// Uncorrelated only — correlated subqueries would need per-row
// re-execution with an outer-row binding (not in scope).

// evalScalarSubquery returns a `(SELECT ...)` subquery's single scalar value
// from the connection's pre-populated cache. The cache is filled by
// preEvaluateScalarSubqueries BEFORE the outer query's runInTx starts, so
// the inner query runs as its own top-level transaction — no FDB nested-tx
// weirdness from re-entering runInTx during a scan.
//
// SQL standard semantics: exactly one column (else 42601 syntax error);
// at most one row (else 21000 cardinality violation); zero rows → NULL.
// Uncorrelated only — inner query has no access to outer-row columns.
func evalScalarSubquery(ctx context.Context, conn *EmbeddedConnection, q antlrgen.IQueryContext) (any, error) {
	if conn == nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "scalar subquery not supported in this context")
	}
	if q == nil {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError, "empty subquery")
	}
	if conn.scalarSubqueryCache != nil {
		if v, ok := conn.scalarSubqueryCache[q]; ok {
			return v, nil
		}
	}
	// Fallback path: cache miss (shouldn't happen for well-formed queries
	// but a safety net for code paths that bypass preEvaluateScalarSubqueries).
	// Run the subquery now; this may fail with a nested-tx error if we're
	// inside a scan, but the error is preferable to silent wrong behaviour.
	return runScalarSubqueryOnce(ctx, conn, q)
}

// runScalarSubqueryOnce does the actual execution + arity validation.
// Called both during pre-evaluation (before any outer runInTx) and as a
// fallback from evalScalarSubquery.
func runScalarSubqueryOnce(ctx context.Context, conn *EmbeddedConnection, q antlrgen.IQueryContext) (any, error) {
	cols, _, rows, err := conn.execQueryBodyRows(ctx, q.QueryExpressionBody())
	if err != nil {
		return nil, err
	}
	if len(cols) != 1 {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError,
			"scalar subquery must return exactly one column, got %d", len(cols))
	}
	if len(rows) > 1 {
		return nil, api.NewErrorf(api.ErrCodeCardinalityViolation,
			"scalar subquery returned %d rows (expected at most 1)", len(rows))
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0][0], nil
}

// preEvaluateScalarSubqueries walks sq's projExprs / whereExpr / havingExpr /
// orderBy expressions, finds every SubqueryExpressionAtomContext, runs each
// once, and stores the result in conn.scalarSubqueryCache. Called before
// the outer query enters runInTx so inner subqueries run as top-level
// transactions (no FDB nesting). Idempotent — already-cached subqueries
// are skipped. Returns the first error hit; on error the cache may be
// partially populated but the caller will abort the outer query anyway.
//
// Uncorrelated only: since the inner query runs before the outer scan
// starts, it cannot reference outer-row columns. Correlated subquery
// support would require a different strategy (per-row re-execution with
// an outer-row binding).
func (c *EmbeddedConnection) preEvaluateScalarSubqueries(ctx context.Context, sq *selectQuery) error {
	if c.scalarSubqueryCache == nil {
		c.scalarSubqueryCache = make(map[antlrgen.IQueryContext]any)
	}
	var walkErr error
	visit := func(expr antlrgen.IExpressionContext) {
		if walkErr != nil || expr == nil {
			return
		}
		walkScalarSubqueries(expr, func(q antlrgen.IQueryContext) {
			if walkErr != nil {
				return
			}
			if _, ok := c.scalarSubqueryCache[q]; ok {
				return
			}
			v, err := runScalarSubqueryOnce(ctx, c, q)
			if err != nil {
				walkErr = err
				return
			}
			c.scalarSubqueryCache[q] = v
		})
	}
	for _, e := range sq.projExprs {
		visit(e)
	}
	if sq.whereExpr != nil {
		visit(sq.whereExpr.Expression())
	}
	visit(sq.havingExpr)
	for _, ob := range sq.orderBy {
		if ob.expr != nil {
			visit(ob.expr)
		}
	}
	return walkErr
}

// walkScalarSubqueries recurses through an expression AST, invoking
// callback for every SubqueryExpressionAtomContext. Mirrors the atom
// shapes understood by evalExprAtom so we do not miss a subquery nested
// inside arithmetic, comparison, function args, or parenthesis groups.
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
	// TODO: ArrayConstructorExpressionAtomContext is not yet handled
	// here — a subquery nested inside an array literal (e.g.
	// `ARRAY[(SELECT v FROM t)]`) will not be pre-evaluated and
	// evalScalarSubquery will hit the cache-miss fallback. Low-
	// priority because array literals with subqueries aren't in the
	// yamsql corpus today; if they show up, add a recursion branch
	// here mirroring RecordConstructorExpressionAtomContext.
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
