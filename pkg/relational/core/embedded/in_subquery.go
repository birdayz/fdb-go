package embedded

import (
	"context"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// Uncorrelated `col IN (SELECT ...)` subquery pre-evaluation.
//
// Sibling of scalar_subquery.go's scalar pre-evaluator. Same
// motivation: running the subquery BEFORE the outer runInTx opens
// avoids nested FDB transactions AND materialises the value list
// so the IN-list pushdown chain can decompose into point scans.
//
// Uncorrelated only. Correlated IN-subqueries (`... WHERE id IN
// (SELECT x FROM other WHERE other.fk = outer.id)`) bind to the
// outer scan's current row; pre-evaluating them without an outer
// row would either resolve to the wrong values or error. We walk
// ONLY the AND-chain leaves of the top-level WHERE — any subquery
// nested under OR / NOT / LIKE / BETWEEN escapes the walk and stays
// on the runtime evalPredicate path, where correlated semantics are
// preserved.
//
// Best-effort: if the subquery errors during pre-evaluation (parse-
// tree mismatch, column-not-found for a correlated ref, unsupported
// shape), we silently DROP the cache entry — the runtime path then
// handles it as before. A pre-evaluation error is NOT propagated
// because it would reject legitimate correlated queries.

// preEvaluateInSubqueries walks the outer SELECT's WHERE as an AND
// chain, finds every `col IN (SELECT ...)` leaf, executes each
// subquery once in its own transaction, and stores the resulting
// value list in conn.inSubqueryCache keyed by the inner
// QueryExpressionBody. Idempotent — already-cached subqueries skip.
//
// Bails silently on any failure (treats as "not pushable"). The
// runtime IN-subquery evaluator in eval_predicate handles the
// fallback.
//
// Only single-column subquery results are cached; multi-column
// results are skipped (SQL errors on multi-col IN). NULL rows
// are dropped (match scalar IN-list semantics).
func (c *EmbeddedConnection) preEvaluateInSubqueries(ctx context.Context, sq *selectQuery) {
	if sq.whereExpr == nil {
		return
	}
	leaves, ok := flattenAndPredicates(sq.whereExpr.Expression())
	if !ok {
		return
	}
	for _, leaf := range leaves {
		body := extractInSubqueryBody(leaf)
		if body == nil {
			continue
		}
		if c.inSubqueryCache != nil {
			if _, cached := c.inSubqueryCache[body]; cached {
				continue
			}
		}
		vals, perErr := runInSubqueryOnce(ctx, c, body)
		if perErr != nil {
			// Best-effort: leave uncached so the runtime path can
			// handle it. A correlated subquery will naturally error
			// here because the outer scope isn't on the stack.
			continue
		}
		if c.inSubqueryCache == nil {
			c.inSubqueryCache = make(map[antlrgen.IQueryExpressionBodyContext][]any)
		}
		c.inSubqueryCache[body] = vals
	}
}

// extractInSubqueryBody returns the QueryExpressionBody of a leaf
// predicate that looks like `col IN (SELECT ...)` (positive form,
// bare column LHS, non-nil subquery body). Returns nil on any
// mismatch — the caller treats that as "not pushable, stay on the
// runtime path."
func extractInSubqueryBody(expr antlrgen.IExpressionContext) antlrgen.IQueryExpressionBodyContext {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return nil
	}
	if pred.Predicate() == nil {
		return nil
	}
	inPred, ok := pred.Predicate().(*antlrgen.InPredicateContext)
	if !ok {
		return nil
	}
	if inPred.NOT() != nil {
		return nil
	}
	if _, isCol := extractColumnRef(pred.ExpressionAtom()); !isCol {
		return nil
	}
	inList := inPred.InList()
	if inList == nil {
		return nil
	}
	body := inList.QueryExpressionBody()
	if body == nil {
		return nil
	}
	return body
}

// runInSubqueryOnce executes the subquery and validates it's a
// single-column result. Drops NULL rows (match literal IN-list
// semantics: `x IN (1, NULL, 2)` evaluates as if NULL weren't
// there for unequal x; here too the NULL contributes nothing the
// equality IN-list extractor would use).
func runInSubqueryOnce(ctx context.Context, conn *EmbeddedConnection, body antlrgen.IQueryExpressionBodyContext) ([]any, error) {
	cols, _, rows, err := conn.execQueryBodyRows(ctx, body)
	if err != nil {
		return nil, err
	}
	if len(cols) != 1 {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError,
			"IN subquery must return exactly one column, got %d", len(cols))
	}
	vals := make([]any, 0, len(rows))
	for _, r := range rows {
		if len(r) == 0 || r[0] == nil {
			continue
		}
		vals = append(vals, r[0])
	}
	// Java-aligned SQL IN set semantics: the subquery result is a SET
	// for IN purposes — duplicate values are equivalent to a single
	// occurrence. Dedupe here so the IN-list pushdown never point-
	// scans the same PK twice (which would emit the matched record
	// twice, as the extractColInList dedup does for literal IN-lists).
	return dedupeAny(vals), nil
}
