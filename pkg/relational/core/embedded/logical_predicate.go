package embedded

import (
	"errors"

	recordlayer "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic/rlcatalog"
)

// buildWherePredicate converts a WHERE expression context into a
// cascades.QueryPredicate using the expr walker, with the scope
// populated from the selectQuery's FROM table. Returns (nil, false)
// on any shape the walker can't handle, on a catalog lookup miss,
// or when metadata is nil. Callers use the (pred, true) branch to
// attach a rich predicate to LogicalFilter; the (nil, false) branch
// falls back to PredicateText (canonical source) so Explain still
// renders something useful.
//
// The scope has a single source — the primary table — matching the
// walker's single-source compat shim. JOIN-backed multi-source
// WHERE clauses still fall back to text since BuildScopeFromFromClause
// refuses JOIN.
func buildWherePredicate(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (cascades.QueryPredicate, bool) {
	if md == nil || sq == nil || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	if sq.tableName == "" || sq.derivedQuery != nil || len(sq.joins) > 0 {
		// Derived tables and joins aren't wired through this path yet
		// — JOIN would need a multi-source scope and the walker's
		// single-source shim removed. Derived tables need a nested
		// logical plan as the source. Both are follow-ups.
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	tbl, err := analyzer.ResolveTable(semantic.FromSegments([]string{sq.tableName}, false))
	if err != nil {
		return nil, false
	}
	alias := semantic.NewUnquoted(sq.tableAlias)
	if sq.tableAlias == "" {
		alias = semantic.NewUnquoted(sq.tableName)
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(semantic.ScopeSource{
		Table:           tbl,
		Alias:           alias,
		CorrelationName: alias.Name(),
	}); err != nil {
		return nil, false
	}
	resolver := expr.New(analyzer, scope)
	pred, err := resolver.WalkPredicate(whereExpr.Expression())
	if err != nil {
		// UnsupportedExpressionShapeError (and any other walker
		// error) degrades to the text fallback. The builder's
		// contract is best-effort: no error is fatal here.
		var unsupported *expr.UnsupportedExpressionShapeError
		if errors.As(err, &unsupported) {
			return nil, false
		}
		return nil, false
	}
	return pred, true
}

// buildLogicalPlanForSelectWithCatalog is the catalog-aware variant
// of buildLogicalPlanForSelect. It walks the WHERE predicate through
// the expr package and attaches a cascades.QueryPredicate tree to
// LogicalFilter when the walker succeeds; on any walker failure the
// filter falls back to the canonical source text (identical output
// to buildLogicalPlanForSelect for the WHERE shape alone).
//
// All non-WHERE operators (Scan / Join / Aggregate / Sort / Limit /
// Project) are identical to the text-only builder — only the
// LogicalFilter node differs when the walker succeeds. Passing md=nil
// is equivalent to calling buildLogicalPlanForSelect: every WHERE
// degrades to text.
//
// Follow-up wiring (not in this shift): plumb md into
// naive_generator's ExplainFn so Explain output shows simplified
// predicate trees when metadata is available.
func buildLogicalPlanForSelectWithCatalog(sq *selectQuery, md *recordlayer.RecordMetaData) logical.LogicalOperator {
	op := buildLogicalPlanForSelect(sq)
	if op == nil || md == nil || sq == nil || sq.whereExpr == nil {
		return op
	}
	pred, ok := buildWherePredicate(md, sq, sq.whereExpr)
	if !ok {
		return op
	}
	// Locate the LogicalFilter and upgrade it in place. The builder
	// always emits the Filter right below any Join/Aggregate/Sort/
	// Limit/Project wrappers, so we walk down the unary chain to find
	// the first (and only) Filter. Structural guarantee of the text
	// builder — if that invariant ever breaks we revisit.
	upgradeFirstFilter(op, pred)
	return op
}

// upgradeFirstFilter walks the single-child chain from op and, at
// the first LogicalFilter, sets Predicate. Stops at the first
// non-unary node or when the Filter is consumed — the text builder
// never emits more than one Filter per SELECT.
func upgradeFirstFilter(op logical.LogicalOperator, pred cascades.QueryPredicate) {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.Predicate = pred
			return
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return
		}
		cur = ch[0]
	}
}
