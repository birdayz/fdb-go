package embedded

// SimplifyValue wire-in for the embedded projection path.
//
// Each non-nil entry in selectQuery.projExprs is a parse-tree-shaped
// SELECT-list expression evaluated per row by evalExpr. When the
// expression is row-context-independent (`SELECT 1+2 FROM t`,
// `SELECT UPPER('hi'), price FROM t`), evaluating it on every row is
// pure waste. foldConstantProjections walks each slot through an
// expr → values.Value walker, runs values.SimplifyValue, and when
// the simplified node is constant per IsConstantValue, evaluates it
// once and stores the Go value in projConstFolded[i]. Per-row consumers
// (select_query_full.go proto path, cte_scan.go map path) check the
// slot before calling evalExpr.
//
// Architecture (post-RFC-025 leak closure): the routing logic in
// foldConstantProjectionsWith takes the walker + folder via injected
// interfaces (`expr.ExpressionResolver` + `values.ExpressionFolder`)
// rather than constructing them inline. The thin entry-point
// `foldConstantProjections` builds the production deps via
// buildProjectionResolver + values.DefaultFolder and forwards. Tests
// call the With form with fakes — they assert routing behaviour
// without standing up a real catalog or metadata.

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/expr"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/semantic/rlcatalog"
)

// projectionFold is a per-projExpr cache slot. present=true means the
// expression was determined to be row-context-independent at plan time
// and `value` is the Go-native driver.Value to reuse on every row.
type projectionFold struct {
	value   any
	present bool
}

// foldConstantProjections is the production entry point. Builds the
// real Resolver from `md` + the seed semantic stack, builds the
// production folder from values.DefaultFolder, and delegates to
// foldConstantProjectionsWith for the actual routing work.
//
// Best-effort: nil sq, empty projExprs, or nil md short-circuits.
// Likewise a catalog-lookup miss inside buildProjectionResolver
// returns nil and the pass becomes a no-op.
func foldConstantProjections(sq *selectQuery, md *recordlayer.RecordMetaData) {
	if sq == nil || len(sq.projExprs) == 0 || md == nil {
		return
	}
	resolver := buildProjectionResolver(sq, md)
	if resolver == nil {
		return
	}
	foldConstantProjectionsWith(sq, resolver, values.DefaultFolder())
}

// foldConstantProjectionsWith is the testable core. Routes each
// projExpr through the supplied resolver + folder. Callers get to
// inject fakes that return canned answers — the routing logic
// (already-folded slots are preserved, the slice grows when needed,
// resolver errors and folder declines silently leave the slot unset)
// can then be exercised against a fake without setting up the real
// catalog stack.
//
// Idempotent: re-calling preserves already-folded slots so the
// second-pass dispatchers in execSelectQuery don't clobber prior
// work.
func foldConstantProjectionsWith(sq *selectQuery, resolver expr.ExpressionResolver, folder values.ExpressionFolder) {
	if sq == nil || len(sq.projExprs) == 0 || resolver == nil || folder == nil {
		return
	}
	if sq.projConstFolded == nil {
		sq.projConstFolded = make([]projectionFold, len(sq.projExprs))
	} else if len(sq.projConstFolded) < len(sq.projExprs) {
		grown := make([]projectionFold, len(sq.projExprs))
		copy(grown, sq.projConstFolded)
		sq.projConstFolded = grown
	}
	for i, e := range sq.projExprs {
		if e == nil || sq.projConstFolded[i].present {
			continue
		}
		v, err := resolver.WalkExpression(e)
		if err != nil {
			continue
		}
		val, ok := folder.Fold(v)
		if !ok {
			continue
		}
		sq.projConstFolded[i] = projectionFold{value: val, present: true}
	}
}

// buildProjectionResolver constructs a Resolver with a scope covering
// the primary table plus any JOIN sources. Returns nil when any
// catalog lookup fails (the Resolver would just decline column refs
// later). Mirrors buildWherePredicateForJoins's scope construction;
// derived-table sources still decline since the seed Resolver can't
// build an inner-plan source.
func buildProjectionResolver(sq *selectQuery, md *recordlayer.RecordMetaData) *expr.Resolver {
	if sq.tableName == "" || sq.derivedQuery != nil {
		// SELECT-without-FROM already evaluates each projExpr exactly once
		// in execSelectQuery, so there's nothing to cache. Derived tables
		// need an inner-plan source the seed Resolver can't construct.
		return nil
	}
	cat := rlcatalog.Wrap(md)
	analyzer := semantic.NewAnalyzer(cat, false)
	scope := semantic.NewScope(nil)
	addSource := func(tableName, alias string) bool {
		tbl, err := analyzer.ResolveTable(semantic.FromSegments(strings.Split(tableName, "."), false))
		if err != nil {
			return false
		}
		aliasID := semantic.NewUnquoted(alias)
		if alias == "" {
			aliasID = semantic.NewUnquoted(tableName)
		}
		return scope.AddSource(semantic.ScopeSource{
			Table:           tbl,
			Alias:           aliasID,
			CorrelationName: aliasID.Name(),
		}) == nil
	}
	if !addSource(sq.tableName, sq.tableAlias) {
		return nil
	}
	for _, j := range sq.joins {
		if !addSource(j.tableName, j.alias) {
			return nil
		}
	}
	return expr.New(analyzer, scope)
}
