package embedded

import (
	recordlayer "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	cascades "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic/rlcatalog"
)

// buildWherePredicateForTable converts a WHERE expression context
// into a cascades.QueryPredicate using the expr walker, with a
// single-source scope over the named table. Returns (nil, false) on
// any shape the walker can't handle, on a catalog lookup miss, or
// when metadata is nil.
//
// The (pred, true) branch is what callers attach to a LogicalFilter;
// the (nil, false) branch is the signal to fall back to the
// canonical source text. Error discrimination is intentionally
// coarse — unsupported shape, catalog miss, nil metadata all land
// in the same (nil, false) bucket — because every error at this
// boundary has the same handling: use text.
//
// tableAlias may be empty; the table's own name fills in.
func buildWherePredicateForTable(
	md *recordlayer.RecordMetaData,
	tableName, tableAlias string,
	whereExpr antlrgen.IWhereExprContext,
) (cascades.QueryPredicate, bool) {
	if md == nil || tableName == "" || whereExpr == nil || whereExpr.Expression() == nil {
		return nil, false
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	tbl, err := analyzer.ResolveTable(semantic.FromSegments([]string{tableName}, false))
	if err != nil {
		return nil, false
	}
	alias := semantic.NewUnquoted(tableAlias)
	if tableAlias == "" {
		alias = semantic.NewUnquoted(tableName)
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
		return nil, false
	}
	return pred, true
}

// buildWherePredicate is the selectQuery-shaped adapter over
// buildWherePredicateForTable. Declines on derived tables and
// joins since they need a multi-source scope (follow-up).
func buildWherePredicate(
	md *recordlayer.RecordMetaData,
	sq *selectQuery,
	whereExpr antlrgen.IWhereExprContext,
) (cascades.QueryPredicate, bool) {
	if sq == nil || sq.derivedQuery != nil || len(sq.joins) > 0 {
		return nil, false
	}
	return buildWherePredicateForTable(md, sq.tableName, sq.tableAlias, whereExpr)
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
	_ = upgradeFirstFilter(op, pred) // invariant: text builder always emits a Filter for a WHERE clause
	return op
}

// upgradeFirstFilter walks the single-child chain from op and, at
// the first LogicalFilter, sets Predicate. Stops at the first
// non-unary node. Returns true when a Filter was found and
// upgraded; false when the walk terminated without seeing one.
// Text builder's invariant is that a WHERE-carrying SELECT always
// emits exactly one Filter on the unary spine, so a false return
// signals the invariant broke — tests assert on it so a future
// builder change that drops the Filter doesn't silently throw
// away the predicate.
func upgradeFirstFilter(op logical.LogicalOperator, pred cascades.QueryPredicate) bool {
	for cur := op; cur != nil; {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			f.Predicate = pred
			return true
		}
		ch := cur.Children()
		if len(ch) != 1 {
			return false
		}
		cur = ch[0]
	}
	return false
}

// buildLogicalPlanForDeleteWithCatalog is the catalog-aware variant
// of buildLogicalPlanForDelete. If the WHERE walks cleanly through
// the expr resolver, the emitted LogicalFilter carries a
// QueryPredicate tree; otherwise the plan is identical to the
// text-only builder.
func buildLogicalPlanForDeleteWithCatalog(
	del antlrgen.IDeleteStatementContext,
	md *recordlayer.RecordMetaData,
) logical.LogicalOperator {
	op := buildLogicalPlanForDelete(del)
	if op == nil || md == nil || del == nil {
		return op
	}
	tableName := ""
	if tn := del.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	w := del.WhereExpr()
	if w == nil || tableName == "" {
		return op
	}
	pred, ok := buildWherePredicateForTable(md, tableName, "", w)
	if !ok {
		return op
	}
	_ = upgradeFirstFilter(op, pred) // invariant: text builder always emits a Filter for a WHERE clause
	return op
}

// buildLogicalPlanForUpdateWithCatalog is the catalog-aware variant
// of buildLogicalPlanForUpdate. Same shape as the Delete variant —
// walker failure falls back to text form on LogicalFilter.
func buildLogicalPlanForUpdateWithCatalog(
	upd antlrgen.IUpdateStatementContext,
	md *recordlayer.RecordMetaData,
) logical.LogicalOperator {
	op := buildLogicalPlanForUpdate(upd)
	if op == nil || md == nil || upd == nil {
		return op
	}
	tableName := ""
	if tn := upd.TableName(); tn != nil && tn.FullId() != nil {
		tableName = functions.FullIdToName(tn.FullId())
	}
	w := upd.WhereExpr()
	if w == nil || tableName == "" {
		return op
	}
	pred, ok := buildWherePredicateForTable(md, tableName, "", w)
	if !ok {
		return op
	}
	_ = upgradeFirstFilter(op, pred) // invariant: text builder always emits a Filter for a WHERE clause
	return op
}
