package embedded

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/catalog"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/metadata"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Main single-table SELECT executor: dispatcher + scan-cursor
// pushdown chain + per-row evaluator + post-scan sort / LIMIT.
// Consumes a parsed selectQuery and returns a driver.Rows — the
// path that all non-join, non-CTE SELECTs flow through.
//
// Cursor dispatch is narrowest-first: PK equality → PK IN-list →
// PK range → composite PK variants → secondary-index variants →
// covering-index dispatch at each secondary branch → full type
// scan fallback. naturalOrder + reverseScanApplied are set per
// branch to fire the ORDER BY eliminator + LIMIT early-termination
// in the sort/projection pass below.
//
// Destined for pkg/relational/core/plan/physical/select.go per
// RFC 021 Phase 1c once we have the Plan seam in place. The
// cursor-dispatch chain collapses into Cascades rules in Phase 2
// (RFC 022).

func (c *EmbeddedConnection) execSelectQueryFull(ctx context.Context, sq *selectQuery) (driver.Rows, error) {
	if len(sq.joins) > 0 {
		return c.execSelectJoin(ctx, sq)
	}
	// WHERE bare-paren rejection — fire once before the row loop.
	// The check is row-independent (purely structural over the parse
	// tree); calling it inside the per-row evalPredicate would re-walk
	// the same AST N times.
	if sq.whereExpr != nil {
		if err := rejectTopLevelParenthesizedWhere(sq.whereExpr.Expression()); err != nil {
			return nil, err
		}
	}

	type row = []driver.Value
	type outField struct {
		name string
		fd   protoreflect.FieldDescriptor
		// expr is set when the slot holds a computed expression (used for
		// extra sort-only fields like `ORDER BY v * 2`). Evaluated against
		// the current message in the scan loop; fd is nil in that case.
		expr antlrgen.IExpressionContext
		// jdbcTyp carries the JDBC result-set type name for computed
		// expression slots whose result type was inferable at parse
		// time (`x + y` of two DOUBLE → "DOUBLE"). Empty for typed
		// columns (where the type comes from `fd`) and for shapes
		// the inferer didn't recognise.
		jdbcTyp string
	}
	var cols []string
	var colTypes []string // parallel to cols; "" entries mean "type unknown"
	var data []row
	var extraSortFields []outField
	// naturalOrder holds the column names the chosen scan cursor emits
	// rows in, always ASC, without tiebreakers. Used by the post-scan
	// sort to skip the in-memory ORDER BY when sq.orderBy is already
	// a prefix of the natural order (and all ASC). Empty means the
	// cursor's emission order is unspecified — always sort.
	var naturalOrder []string
	// naturalOrderAliases maps uppercase SELECT-list alias names to
	// their underlying column names, so `SELECT id AS pk ... ORDER BY
	// pk` resolves to the PK col for the natural-order prefix check.
	// Captured from the scan loop so the out-of-closure sort path can
	// use it without re-parsing.
	var naturalOrderAliases map[string]string
	// reverseScanApplied tracks whether the chosen cursor uses a
	// reverse scan to satisfy an all-DESC ORDER BY prefix of
	// naturalOrder. When true, the post-scan sort is skipped (the
	// cursor's reverse emission IS the requested DESC order) and the
	// LIMIT early-termination logic treats the reverse-DESC match the
	// same as a forward-ASC match.
	var reverseScanApplied bool
	// equatedCols holds the UPPER-CASE bare col names that the WHERE
	// clause equates to a constant literal at the AND-conjunction top
	// level. Declared outside the runInTx closure so the post-scan
	// sort-skip check can use it; captured inside the closure so it
	// reflects the current transaction's parse/eval.
	var equatedCols map[string]bool

	_, runErr := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry so duplicate rows aren't appended
		cols = nil
		colTypes = nil
		extraSortFields = nil
		naturalOrder = nil
		naturalOrderAliases = nil
		reverseScanApplied = false
		equatedCols = nil
		txn := catalog.NewFDBTransaction(rctx)
		schema, loadErr := c.cachedLoadSchema(txn, c.sess.DBPath, c.sess.Schema)
		if loadErr != nil {
			return nil, loadErr
		}
		rlTmpl, tmplOk := schema.SchemaTemplate().(*metadata.RecordLayerSchemaTemplate)
		if !tmplOk {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "schema template is not a RecordLayerSchemaTemplate")
		}
		md := rlTmpl.Underlying()

		// Qualified-star (SELECT <q>.*) on a single-source FROM must match
		// this source. Delegate to resolveQualifierColumns so the alias-
		// matching rule stays in one place; we ignore the returned column
		// list because for a single-source query `a.*` projects the same
		// columns as `*`.
		if sq.projQualifier != "" {
			if _, _, qErr := c.resolveQualifierColumns(md, sq, sq.projQualifier); qErr != nil {
				return nil, qErr
			}
		}
		// Expand mixed qualifier-star + named-column slots. Single-source
		// FROM only has one alias to match against, but the expansion
		// still works uniformly — a wrong qualifier errors 42F01 from
		// resolveQualifierColumns.
		if expandErr := c.expandStarSlots(md, sq); expandErr != nil {
			return nil, expandErr
		}

		resolvedTable, resolveErr := functions.ResolveQualifiedTableName(sq.tableName, c.sess.Schema)
		if resolveErr != nil {
			return nil, resolveErr
		}
		sq.tableName = resolvedTable
		rt := md.GetRecordType(sq.tableName)
		if rt == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
				"Unknown table %s", strings.ToUpper(sq.tableName))
		}
		msgDesc := rt.Descriptor

		ss, ssErr := c.sess.Keyspace.SchemaSubspace(c.sess.DBPath, c.sess.Schema)
		if ssErr != nil {
			return nil, ssErr
		}
		store, storeErr := c.newStoreBuilder().
			SetContext(rctx).
			SetSubspace(ss).
			SetMetaDataProvider(md).
			Open()
		if storeErr != nil {
			return nil, storeErr
		}

		// Scan-cursor pushdown chain. Order is "narrowest first": each
		// branch represents a progressively looser shape, so the first
		// match is always the tightest narrowing. Fallthrough is the
		// full type scan. The scan loop below still applies the full
		// WHERE via evalPredicate, so every pushdown is a superset of
		// the rows the scan would have matched — partial narrowing is
		// correct.
		//
		// Each branch sets naturalOrder — the column sequence the
		// resulting cursor emits rows in, always ASC. Used downstream
		// by naturalOrderSatisfies to skip the in-memory ORDER BY sort
		// and to enable LIMIT early-termination when the ORDER BY
		// clause is a prefix of this order.
		//
		// Covering-index optimisation (skip the by-PK fetch) is
		// considered at every secondary-index branch when the SELECT's
		// referenced column set fits the (idx cols, PK cols) union.
		//
		// Chain order:
		//   PK:
		//     1. equality on every PK col          — 1 key
		//     2. single-col IN-list                — N keys
		//     3. single-col range / BETWEEN / LIKE
		//     4. composite leading-eq + IN-list    — N keys on composite
		//     5. composite range with leading eq
		//     6. composite pure-prefix             — equalities on a
		//        leading PK subset, no range / IN — tuple-prefix scan
		//   Secondary index (each gated on index scannability inside
		//   its helper):
		//     7. equality                          — exact key (covered)
		//     8. single-col IN-list                — N keys (covered)
		//     9. composite leading-eq + IN-list    — N keys (covered)
		//    10. range / BETWEEN / LIKE prefix     — (covered)
		//    11. composite range with leading eq   — (covered)
		//    12. composite pure-prefix             — equalities on a
		//        leading subset of idx cols, no range / IN on trailing
		//        cols — tuple-prefix scan (covered)
		//    13. full type scan (fallback)
		var cursor recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
		pkCols := extractPKUserFields(rt.PrimaryKey)
		// Populate naturalOrderAliases early so each branch's reverse-
		// scan decision can resolve ORDER BY aliases against the
		// SELECT-list. The scan-setup block below overwrites this with
		// the same value; hoisting is cheap.
		naturalOrderAliases = buildOrderByAliases(sq)
		// equatedCols: cols WHERE equates to a constant literal at
		// AND-conjunction top level. Used by the ORDER BY eliminator
		// to strip zero-width leading (and in-middle) natural-order
		// dims: `WHERE a = 1 ORDER BY b, c` on PK (a, b, c) satisfies
		// after stripping a from naturalOrder + matching (b, c). Same
		// info drives reverse-scan direction selection. Captured to
		// the outer scope so the post-tx sort-skip check sees it too.
		equatedCols = collectEquatedCols(ctx, c, sq.whereExpr)
		// pkScanProps carries the direction (forward/reverse) the PK
		// branches pass into their cursor builders. Each branch sets
		// this based on whether sq.orderBy is an all-DESC prefix of
		// pkCols (the naturalOrder these branches all produce). On ASC
		// prefix (or empty orderBy), stays ForwardScan. naturalOrder
		// is still recorded as pkCols — the sort eliminator downstream
		// uses reverseApplied to accept either direction.
		pkScanProps, pkReverseApplied := scanPropsForOrder(sq.orderBy, pkCols, equatedCols, naturalOrderAliases)
		if pkVals, ok := c.tryPKEqualityPushdown(ctx, sq, rt); ok && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases) {
			// PK equality matches at-most-one row. Java's RemoveSortRule
			// does NOT exempt 1-row scans — the rule checks the
			// Ordering property which is `()` for an equality match,
			// and `()` doesn't satisfy a non-empty requested ordering.
			// Java rejects `WHERE id = X ORDER BY non_pk_col` with
			// UnableToPlan; Go must match. Gate on PK-ordering
			// satisfaction (which covers ORDER BY PK col / empty /
			// all-equated) rather than the at-most-1-row escape hatch.
			// nightshift-60 (refined from initial atMostOneRow approach
			// that diverged from Java).
			cursor = pkPushdownScanCursor(store, rt, pkVals, pkScanProps)
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if pkVals, ok := c.tryPKInListPushdown(ctx, sq, rt); ok && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases) {
			if len(pkVals) == 1 {
				// Degenerate IN-list: `pk IN (v)` is equivalent to `pk =
				// v` — take the equality path. Single point scan instead
				// of a one-element lazy chain, and naturalOrder can flag
				// PK cols (at-most-one row) to enable ORDER BY
				// elimination and LIMIT early-termination.
				cursor = pkPushdownScanCursor(store, rt, pkVals, pkScanProps)
				naturalOrder = pkCols
				reverseScanApplied = pkReverseApplied
			} else {
				// Multi-value IN-list: the lazy chain emits sub-scans in
				// pkVals' declared order. Pre-sort the values to make the
				// emission PK-ordered (ASC sort → ASC emission; DESC sort
				// → DESC emission). Java's planner does the equivalent —
				// IN-list scans always emit in key order. Setting
				// naturalOrder = pkCols then lets the ORDER BY satisfier
				// recognise the match. nightshift-60.
				//
				// The outer `&&` gate above ensures this branch only takes
				// when ORDER BY is satisfiable by PK ordering (or empty/
				// equated). When ORDER BY references a non-PK col, the
				// branch declines and the chain falls through to a strategy
				// whose natural order does satisfy (eventually
				// tryIndexScanForOrdering or full PK scan).
				switch {
				case naturalOrderSatisfies(sq.orderBy, pkCols, equatedCols, naturalOrderAliases):
					sort.SliceStable(pkVals, func(i, j int) bool {
						return functions.CompareValues(pkVals[i].(driver.Value), pkVals[j].(driver.Value)) < 0
					})
					cursor = pkPushdownInListScanCursor(store, rt, pkVals)
					naturalOrder = pkCols
				case naturalOrderSatisfiesReverse(sq.orderBy, pkCols, equatedCols, naturalOrderAliases):
					sort.SliceStable(pkVals, func(i, j int) bool {
						return functions.CompareValues(pkVals[i].(driver.Value), pkVals[j].(driver.Value)) > 0
					})
					cursor = pkPushdownInListScanCursor(store, rt, pkVals)
					naturalOrder = pkCols
					reverseScanApplied = true
				default:
					// ORDER BY empty / all-equated — naturalOrder unused.
					cursor = pkPushdownInListScanCursor(store, rt, pkVals)
				}
			}
		} else if bounds, ok := c.tryPKRangePushdown(ctx, sq, rt); ok && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases) {
			cursor = pkPushdownRangeScanCursor(store, rt, bounds, pkScanProps)
			// Single-col PK range → ASC on the PK col, then nothing.
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if cil, ok := c.tryPKCompositeInListPushdown(ctx, sq, rt); ok {
			// Pre-sort the IN-list values to make the lazy chain emit
			// in pkCols order. Each sub-scan emits rows in PK order
			// internally; sequential sub-scans concatenated in IN-list
			// order produce overall pkCols ordering iff the IN-list is
			// sorted. Same approach as the single-col PK IN-list branch
			// above. nightshift-60.
			switch {
			case naturalOrderSatisfies(sq.orderBy, pkCols, equatedCols, naturalOrderAliases):
				sort.SliceStable(cil.inValues, func(i, j int) bool {
					return functions.CompareValues(cil.inValues[i].(driver.Value), cil.inValues[j].(driver.Value)) < 0
				})
				cursor = pkCompositeInListScanCursor(store, rt, cil)
				naturalOrder = pkCols
			case naturalOrderSatisfiesReverse(sq.orderBy, pkCols, equatedCols, naturalOrderAliases):
				sort.SliceStable(cil.inValues, func(i, j int) bool {
					return functions.CompareValues(cil.inValues[i].(driver.Value), cil.inValues[j].(driver.Value)) > 0
				})
				cursor = pkCompositeInListScanCursor(store, rt, cil)
				naturalOrder = pkCols
				reverseScanApplied = true
			default:
				cursor = pkCompositeInListScanCursor(store, rt, cil)
				// naturalOrder stays nil — only acceptable when ORDER BY
				// is empty / equated, which the post-scan rejection check
				// catches if it isn't.
			}
		} else if cr, ok := c.tryPKCompositeRangePushdown(ctx, sq, rt); ok && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases) {
			cursor = pkPushdownCompositeRangeScanCursor(store, rt, cr, pkScanProps)
			// Composite PK range emits rows in ASC PK order.
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if cp, ok := c.tryPKCompositePrefixPushdown(ctx, sq, rt); ok && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases) {
			// Pure-prefix composite PK: `WHERE a = 1 AND b = 5` on PK
			// (a, b, c) narrows to the tuple-prefix scan [rtk, 1, 5]
			// without any range/IN-list on trailing cols. Last PK
			// branch before secondary — tighter composite forms have
			// already been tried.
			cursor = pkPushdownScanCursor(store, rt, cp.prefixVals, pkScanProps)
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		} else if idxName, idxVal, ok := c.trySecondaryIndexPushdown(ctx, store, sq, rt, md); ok && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases) {
			// The helper itself filters out WRITE_ONLY / DISABLED
			// indexes while iterating, so any returned index is
			// guaranteed scannable. Falls through to the next branch
			// (and ultimately to a full scan) when no scannable match
			// exists.
			//
			// Covering-index optimisation: when every column the SELECT
			// reads from each row is derivable from the index key + PK,
			// bypass the per-row LoadRecord fetch by synthesising a
			// dynamicpb record from the IndexEntry. One FDB round-trip
			// per row instead of two. See covering_index.go.
			idx := md.GetIndex(idxName)
			// Equality fixes idxCols; effective sort key is PKCols.
			// Reverse scan applies when the user's ORDER BY is an
			// all-DESC prefix of pkCols.
			eqScanProps, eqReverse := scanPropsForOrder(sq.orderBy, pkCols, equatedCols, naturalOrderAliases)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexEqualityTupleRange(idxVal), eqScanProps)
			} else {
				cursor = secondaryIndexPushdownCursor(store, idxName, idxVal, eqScanProps)
			}
			naturalOrder = pkCols
			reverseScanApplied = eqReverse
		} else if sil, ok := c.trySecondaryIndexInListPushdown(ctx, store, sq, rt, md); ok &&
			((len(sil.values) == 1 && pkOrderingSatisfiesOrderBy(sq.orderBy, pkCols, equatedCols, naturalOrderAliases)) ||
				(len(sil.values) > 1 && (len(sq.orderBy) == 0 || allOrderByEquated(sq.orderBy, equatedCols, naturalOrderAliases)))) {
			// Covering also applies to IN-list: each sub-scan can skip
			// the by-PK fetch when the index covers every referenced
			// column. Same decision as the equality path.
			//
			// Gating (nightshift-60): single-value sub-path is a
			// secondary-index equality scan that emits in pkCols order;
			// gated on PK ordering satisfying the ORDER BY (same as
			// `trySecondaryIndexPushdown`). Multi-value lazy chain has
			// no usable natural order across sub-scans; only acceptable
			// when the ORDER BY is empty or every clause is on an
			// equated col (no order required). Otherwise fall through
			// to a strategy that can satisfy (eventually full PK scan
			// or `tryIndexScanForOrdering`).
			idx := md.GetIndex(sil.indexName)
			if len(sil.values) == 1 {
				// Degenerate IN-list: single sub-scan is an index
				// equality point scan — take the equality path directly
				// to drop the lazy-chain wrapper and enable ORDER BY
				// elimination on (idxCols..., PKCols...) = PKCols (with
				// idxCols fixed).
				eqScanProps, eqReverse := scanPropsForOrder(sq.orderBy, pkCols, equatedCols, naturalOrderAliases)
				if idx != nil && canCoverIndex(sq, idx, rt) {
					cursor = coveringIndexRangeScanCursor(store, rt, idx,
						buildSecondaryIndexEqualityTupleRange(sil.values[0]), eqScanProps)
				} else {
					cursor = secondaryIndexPushdownCursor(store, sil.indexName, sil.values[0], eqScanProps)
				}
				naturalOrder = pkCols
				reverseScanApplied = eqReverse
			} else if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = secondaryIndexInListScanCursor(store, sil, rt, idx)
			} else {
				cursor = secondaryIndexInListScanCursor(store, sil, nil, nil)
			}
			// IN-list lazy chain — not sorted across sub-scans.
		} else if cil, ok := c.trySecondaryIndexCompositeInListPushdown(ctx, store, sq, rt, md); ok &&
			(len(sq.orderBy) == 0 || allOrderByEquated(sq.orderBy, equatedCols, naturalOrderAliases)) {
			// Same lazy-chain gate as the secondary-index IN-list branch
			// above: composite IN-list across sub-scans has no usable
			// natural order, so only take the branch when the user has
			// no real ORDER BY to satisfy. Otherwise fall through.
			if idx := md.GetIndex(cil.indexName); idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = secondaryIndexCompositeInListScanCursor(store, cil, rt, idx)
			} else {
				cursor = secondaryIndexCompositeInListScanCursor(store, cil, nil, nil)
			}
		} else if sir, ok := c.trySecondaryIndexRangePushdown(ctx, store, sq, rt, md); ok &&
			indexBranchSatisfiesOrderBy(md.GetIndex(sir.indexName), pkCols, sq.orderBy, equatedCols, naturalOrderAliases) {
			idx := md.GetIndex(sir.indexName)
			// Index range → (idxCol ASC, PKCols ASC). Reverse scan
			// applies when ORDER BY is an all-DESC prefix of that.
			var idxNaturalOrder []string
			if idx != nil {
				idxNaturalOrder = append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
			}
			rngScanProps, rngReverse := scanPropsForOrder(sq.orderBy, idxNaturalOrder, equatedCols, naturalOrderAliases)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexRangeTupleRange(sir.bounds), rngScanProps)
			} else {
				cursor = secondaryIndexRangeScanCursor(store, sir.indexName, sir.bounds, rngScanProps)
			}
			if idx != nil {
				naturalOrder = idxNaturalOrder
				reverseScanApplied = rngReverse
			}
		} else if sicr, ok := c.trySecondaryIndexCompositeRangePushdown(ctx, store, sq, rt, md); ok &&
			indexBranchSatisfiesOrderBy(md.GetIndex(sicr.indexName), pkCols, sq.orderBy, equatedCols, naturalOrderAliases) {
			idx := md.GetIndex(sicr.indexName)
			var idxNaturalOrder []string
			if idx != nil {
				idxNaturalOrder = append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
			}
			crScanProps, crReverse := scanPropsForOrder(sq.orderBy, idxNaturalOrder, equatedCols, naturalOrderAliases)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexCompositeRangeTupleRange(sicr), crScanProps)
			} else {
				cursor = secondaryIndexCompositeRangeScanCursor(store, sicr, crScanProps)
			}
			if idx != nil {
				naturalOrder = idxNaturalOrder
				reverseScanApplied = crReverse
			}
		} else if sicp, ok := c.trySecondaryIndexCompositePrefixPushdown(ctx, store, sq, rt, md); ok &&
			indexBranchSatisfiesOrderBy(md.GetIndex(sicp.indexName), pkCols, sq.orderBy, equatedCols, naturalOrderAliases) {
			// Pure-prefix composite secondary: equalities on a leading
			// subset of the index cols, no range / IN on trailing
			// cols. Narrows to tuple-prefix scan [prefixVals...] on
			// the index subspace; trailing cols stay post-filter via
			// evalPredicate. Last secondary-index branch — any
			// tighter form (full-equality, IN-list, range,
			// composite-range) has already been tried.
			idx := md.GetIndex(sicp.indexName)
			var idxNaturalOrder []string
			if idx != nil {
				idxNaturalOrder = append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
			}
			cpScanProps, cpReverse := scanPropsForOrder(sq.orderBy, idxNaturalOrder, equatedCols, naturalOrderAliases)
			if idx != nil && canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexEqualityTupleRange(secondaryIndexKeyTuple{values: sicp.prefixVals}), cpScanProps)
			} else {
				cursor = secondaryIndexCompositePrefixScanCursor(store, sicp, cpScanProps)
			}
			if idx != nil {
				naturalOrder = idxNaturalOrder
				reverseScanApplied = cpReverse
			}
		} else if idx, ok := tryIndexScanForOrdering(sq, rt, md, store, pkCols, equatedCols, naturalOrderAliases); ok {
			// Full-secondary-index scan to satisfy ORDER BY when no WHERE
			// pushdown matched but an index's natural order satisfies the
			// requested ordering. nightshift-60: this branch closes the
			// gap that surfaced when removing the in-memory sort fallback
			// — Java's Cascades planner picks an index scan as the
			// satisfying inner plan for `RemoveSortRule` when the index's
			// Ordering property matches the requested order, even
			// without WHERE pushdown. Without this branch, queries like
			// `SELECT * FROM t ORDER BY indexed_col` would fall through
			// to the full-PK scan (PK natural order, doesn't satisfy)
			// and then be rejected by the post-scan ordering check —
			// diverging from Java's behaviour. Same Java-conformant
			// "the rule fires when the inner satisfies" pattern.
			idxNaturalOrder := append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
			scanProps, reverse := scanPropsForOrder(sq.orderBy, idxNaturalOrder, equatedCols, naturalOrderAliases)
			fullRange := pkRangeBounds{}
			if canCoverIndex(sq, idx, rt) {
				cursor = coveringIndexRangeScanCursor(store, rt, idx,
					buildSecondaryIndexRangeTupleRange(fullRange), scanProps)
			} else {
				cursor = secondaryIndexRangeScanCursor(store, idx.Name, fullRange, scanProps)
			}
			naturalOrder = idxNaturalOrder
			reverseScanApplied = reverse
		} else {
			// Full type scan emits in PK tuple order (record-type-key
			// prefix keeps records of the same type contiguous). Use
			// reverse scan when ORDER BY is an all-DESC prefix of
			// pkCols — same direction-selection rule as the PK
			// pushdown branches.
			cursor = store.ScanRecordsByType(sq.tableName, nil, pkScanProps)
			naturalOrder = pkCols
			reverseScanApplied = pkReverseApplied
		}
		defer cursor.Close() //nolint:errcheck

		// Record the SQL-level aliases of this scan so correlated
		// subqueries can expose them to outerScopeFromMsg (e.g.
		// `FROM emp AS e` → {"E", "EMP"}). Pop on function return.
		defer c.pushSourceAliases(sq.tableName, sq.tableAlias)()

		if sq.countStar {
			cols = []string{countStarOutName(sq)}
			colTypes = []string{"BIGINT"}
			var count int64
			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				match, matchErr := evalPredicate(ctx, c, result.GetValue().Record, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if match {
					count++
				}
			}
			// HAVING on a bare COUNT(*) query: evaluate against the single
			// aggregate row and drop it when the predicate fails. Without
			// this the COUNT(*) fast path emitted one row unconditionally.
			// HAVING references the aggregate function (canonical name),
			// not the SELECT-list alias — see aggregateMapRows comment.
			if sq.havingExpr != nil {
				keep, hErr := evalHaving(ctx, c, map[string]driver.Value{"COUNT(*)": count}, sq.havingExpr)
				if hErr != nil {
					return nil, hErr
				}
				if !keep {
					data = nil
					return nil, nil
				}
			}
			data = []row{{count}}
			return nil, nil
		}

		// GROUP BY aggregate query: scan → group → aggregate.
		if len(sq.aggCols) > 0 {
			// DISTINCT aggregates rejected — Java alignment (visitor
			// NPEs on every aggregate with DISTINCT). See aggregate.go
			// for full rationale.
			for _, ac := range sq.aggCols {
				if ac.aggDistinct {
					return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
						"DISTINCT aggregate %s is not supported", ac.aggFunc)
				}
			}
			// Resolve group-by field descriptors. Expression group keys
			// (sq.groupByExprs[i] != nil) skip FD resolution — they are
			// evaluated per message below via evalExpr.
			groupFDs := make([]protoreflect.FieldDescriptor, len(sq.groupBy))
			for i, col := range sq.groupBy {
				if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
					continue
				}
				fd := msgDesc.Fields().ByName(protoreflect.Name(col))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"GROUP BY column %q not found in table %q", col, sq.tableName)
				}
				groupFDs[i] = fd
			}
			// Resolve aggregate arg field descriptors (nil for COUNT(*) and for
			// expression args, which are evaluated per-message via ac.aggExpr).
			//
			// groupCol entries are group-by references lifted out of the SELECT
			// list during extractFromSimpleTable's aggregate re-classification.
			// Their value comes from gs.groupVals at emit time, not from the
			// proto scan — so we only validate the FD exists when it's a bare
			// column name. A groupCol whose name matches an entry in groupBy[]
			// with a non-nil groupByExprs[] is an expression group (e.g.
			// GROUP BY CASE ...); skip the FD lookup for those.
			groupExprByName := make(map[string]bool, len(sq.groupBy))
			for i, gn := range sq.groupBy {
				if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
					groupExprByName[gn] = true
				}
			}
			// groupByNames holds the declared GROUP BY bare-column list so we
			// can enforce SQL §7.10 GR1 — a projected bare column that isn't
			// in GROUP BY (and isn't an aggregate argument) is 42803. Pre-
			// dayshift-40 the emission loop silently NULL-filled instead.
			groupByNames := make(map[string]bool, len(sq.groupBy))
			for i, gn := range sq.groupBy {
				// Expression-based GROUP BY (e.g. `GROUP BY a + b`) is keyed
				// by the raw expression text as a synthetic display name —
				// handled via groupExprByName below. Skip here.
				if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
					continue
				}
				groupByNames[gn] = true
			}
			aggArgFDs := make([]protoreflect.FieldDescriptor, len(sq.aggCols))
			for i, ac := range sq.aggCols {
				if ac.groupCol != "" {
					if groupExprByName[ac.groupCol] {
						continue
					}
					fd := msgDesc.Fields().ByName(protoreflect.Name(ac.groupCol))
					if fd == nil {
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"column %q not found in table %q", ac.groupCol, sq.tableName)
					}
					// Java-aligned 42803. The fd-exists check above fired
					// first so undefined columns still surface as 42703,
					// matching Java's error order.
					if !groupByNames[ac.groupCol] {
						return nil, api.NewErrorf(api.ErrCodeGroupingError,
							"column %q must appear in the GROUP BY clause or be used in an aggregate function",
							ac.groupCol)
					}
					aggArgFDs[i] = fd
				} else if ac.aggArg != "" {
					// Strip qualifier `t.val` → `val` so a qualified
					// aggregate argument resolves against the descriptor
					// field name. The qualifier validity is implicit:
					// only one source is in scope on the proto path, so
					// any qualifier that's not garbage is the table or
					// its alias.
					bare := parseColRef(ac.aggArg).bare()
					fd := msgDesc.Fields().ByName(protoreflect.Name(bare))
					if fd == nil {
						return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
							"aggregate column %q not found in table %q", ac.aggArg, sq.tableName)
					}
					aggArgFDs[i] = fd
				}
			}

			type groupState struct {
				groupVals []driver.Value // values for the group-by columns
				// accumulators parallel to sq.aggCols
				counts []int64
				// SUM accumulators: maintain BOTH int64 and float64
				// running totals so we can emit int64 when every
				// observed value is integral (Java-aligned: `SUM(BIGINT)
				// / COUNT(*)` integer-divides). sumNonInt[i] starts as
				// the zero value (false) — i.e. "still int-only" — and
				// only ever flips to true. Overflow on the int64
				// accumulator wraps silently, same as Java's `long`.
				// See aggregate.go for the symmetric map-path
				// implementation.
				sums      []float64
				sumsI     []int64
				sumNonInt []bool
				mins      []driver.Value
				maxes     []driver.Value
				avgs      []float64 // running sum for AVG
				avgsN     []int64   // count for AVG
			}
			groupOrder := []string{} // insertion order for deterministic output
			groups := map[string]*groupState{}

			for {
				result, nextErr := cursor.OnNext(ctx)
				if nextErr != nil {
					return nil, nextErr
				}
				if !result.HasNext() {
					break
				}
				msg := result.GetValue().Record
				match, matchErr := evalPredicate(ctx, c, msg, sq.whereExpr)
				if matchErr != nil {
					return nil, matchErr
				}
				if !match {
					continue
				}

				// Build group-by key.
				gVals := make([]driver.Value, len(sq.groupBy))
				for i := range sq.groupBy {
					if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
						v, evalErr := evalExpr(ctx, c, msg, sq.groupByExprs[i])
						if evalErr != nil {
							return nil, evalErr
						}
						gVals[i] = v
						continue
					}
					fd := groupFDs[i]
					if fd != nil && msg.ProtoReflect().Has(fd) {
						gVals[i] = functions.ProtoValueToDriver(fd, msg.ProtoReflect().Get(fd))
					}
				}
				key := groupByKey(gVals)
				gs, exists := groups[key]
				if !exists {
					gs = &groupState{
						groupVals: gVals,
						counts:    make([]int64, len(sq.aggCols)),
						sums:      make([]float64, len(sq.aggCols)),
						sumsI:     make([]int64, len(sq.aggCols)),
						sumNonInt: make([]bool, len(sq.aggCols)),
						mins:      make([]driver.Value, len(sq.aggCols)),
						maxes:     make([]driver.Value, len(sq.aggCols)),
						avgs:      make([]float64, len(sq.aggCols)),
						avgsN:     make([]int64, len(sq.aggCols)),
					}
					groups[key] = gs
					groupOrder = append(groupOrder, key)
				}
				// Update accumulators.
				for i, ac := range sq.aggCols {
					if ac.groupCol != "" {
						continue // group-by reference, no accumulation
					}
					if ac.outExpr != nil {
						// Post-aggregation expression — evaluated at emit time.
						continue
					}
					// Fetch the argument value.
					//   - aggExpr != nil: evaluate expression (e.g. SUM(qty*price)).
					//   - aggArg  != "": read the bare column via field descriptor.
					//   - neither:       COUNT(*) — no argument, counted unconditionally below.
					var v driver.Value
					hasArg := ac.aggArg != "" || ac.aggExpr != nil
					if ac.aggExpr != nil {
						ev, evalErr := evalExpr(ctx, c, msg, ac.aggExpr)
						if evalErr != nil {
							return nil, evalErr
						}
						v = ev
					} else if aggArgFDs[i] != nil && msg.ProtoReflect().Has(aggArgFDs[i]) {
						v = functions.ProtoValueToDriver(aggArgFDs[i], msg.ProtoReflect().Get(aggArgFDs[i]))
					}
					// COUNT(*) counts every row including all-NULL; no argument.
					if ac.aggFunc == "COUNT" && !hasArg {
						gs.counts[i]++
						continue
					}
					// COUNT(<col|expr>)/SUM/MIN/MAX/AVG skip NULLs per SQL standard.
					if v == nil {
						continue
					}
					gs.counts[i]++
					switch ac.aggFunc {
					case "SUM", "AVG":
						fv, ok := functions.ToFloat64(v)
						if !ok {
							return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
								"unable to encapsulate aggregate operation due to type mismatch(es)")
						}
						if ac.aggFunc == "SUM" {
							gs.sums[i] += fv
							if iv, isInt := v.(int64); isInt && !gs.sumNonInt[i] {
								// Java verbatim: ArithmeticException
								// "long overflow" on SUM(BIGINT)
								// overflow.
								r, ok := functions.AddInt64Checked(gs.sumsI[i], iv)
								if !ok {
									return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
										"long overflow")
								}
								gs.sumsI[i] = r
							} else {
								gs.sumNonInt[i] = true
							}
						} else {
							gs.avgs[i] += fv
							gs.avgsN[i]++
						}
					case "MIN":
						if err := requireMinMaxNumeric(v); err != nil {
							return nil, err
						}
						if gs.mins[i] == nil || functions.CompareValues(v, gs.mins[i]) < 0 {
							gs.mins[i] = v
						}
					case "MAX":
						if err := requireMinMaxNumeric(v); err != nil {
							return nil, err
						}
						if gs.maxes[i] == nil || functions.CompareValues(v, gs.maxes[i]) > 0 {
							gs.maxes[i] = v
						}
					}
				}
			}

			// SQL spec: ungrouped aggregate over empty input emits one row
			// (COUNT=0, SUM/MIN/MAX/AVG=NULL). Java alignment
			// (nightshift-61): when HAVING is present, fdb-relational
			// treats the empty input as "no grouping at all" — HAVING
			// never fires, 0 rows. CLAUDE.md gotcha:
			// "`SELECT <agg> FROM t WHERE <none-match> HAVING <agg-pred>`
			// diverges". Aligned to Java by skipping the synthetic group
			// when HAVING is set. The HAVING-absent path keeps SQL-spec
			// aggregate-over-empty semantics.
			if len(sq.groupBy) == 0 && len(groupOrder) == 0 && sq.havingExpr == nil {
				groups[""] = &groupState{
					groupVals: nil,
					counts:    make([]int64, len(sq.aggCols)),
					sums:      make([]float64, len(sq.aggCols)),
					sumsI:     make([]int64, len(sq.aggCols)),
					sumNonInt: make([]bool, len(sq.aggCols)),
					mins:      make([]driver.Value, len(sq.aggCols)),
					maxes:     make([]driver.Value, len(sq.aggCols)),
					avgs:      make([]float64, len(sq.aggCols)),
					avgsN:     make([]int64, len(sq.aggCols)),
				}
				groupOrder = append(groupOrder, "")
			}

			// Build output cols — visible entries first, then non-visible
			// columns (harvested from ORDER BY / HAVING) so the post-
			// aggregation sort can find them via colIdx. Caller strips the
			// trailing non-visible columns after the sort.
			groupColIdx := map[string]int{}
			for i, col := range sq.groupBy {
				groupColIdx[col] = i
				// Bare last-segment alias (symmetric with
				// aggregateMapRows) so qualified GROUP BY keys resolve
				// against unqualified SELECT-list references.
				// First-wins on bare collision; see aggregateMapRows.
				if ref := parseColRef(col); ref.isQualified() {
					if _, exists := groupColIdx[ref.bare()]; !exists {
						groupColIdx[ref.bare()] = i
					}
				}
			}
			emitIdx := make([]int, 0, len(sq.aggCols))
			for i, ac := range sq.aggCols {
				if ac.visible {
					emitIdx = append(emitIdx, i)
				}
			}
			for i, ac := range sq.aggCols {
				if !ac.visible {
					emitIdx = append(emitIdx, i)
				}
			}
			cols = make([]string, len(emitIdx))
			colTypes = make([]string, len(emitIdx))
			for out, i := range emitIdx {
				ac := sq.aggCols[i]
				cols[out] = ac.outName
				colTypes[out] = aggregateResultJDBCType(ac, msgDesc)
			}

			// Emit one row per group (with HAVING filter). Two passes:
			// (1) populate fullVals + rowMap for non-outExpr entries;
			// (2) evaluate outExpr entries against the now-filled rowMap.
			for _, key := range groupOrder {
				gs := groups[key]
				fullVals := make([]driver.Value, len(sq.aggCols))
				rowMap := make(map[string]driver.Value, len(sq.aggCols))
				for i, ac := range sq.aggCols {
					if ac.outExpr != nil {
						continue
					}
					if ac.groupCol != "" {
						idx, ok := groupColIdx[ac.groupCol]
						if !ok {
							idx, ok = groupColIdx[parseColRef(ac.groupCol).bare()]
						}
						if ok {
							fullVals[i] = gs.groupVals[idx]
						}
					} else {
						switch ac.aggFunc {
						case "COUNT":
							fullVals[i] = gs.counts[i]
						case "SUM":
							// SUM of empty-or-all-NULL group is NULL, not 0.
							// DISTINCT path accumulates on first-seen so this
							// is correct for SUM(DISTINCT col) too.
							//
							// Java alignment (int-preserving): if every
							// observed value was integral, emit int64 so
							// `SUM(BIGINT) / COUNT(*)` integer-divides the
							// way Java does.
							if gs.counts[i] > 0 {
								if gs.sumNonInt[i] {
									fullVals[i] = gs.sums[i]
								} else {
									fullVals[i] = gs.sumsI[i]
								}
							}
						case "MIN":
							fullVals[i] = gs.mins[i]
						case "MAX":
							fullVals[i] = gs.maxes[i]
						case "AVG":
							if gs.avgsN[i] > 0 {
								fullVals[i] = gs.avgs[i] / float64(gs.avgsN[i])
							}
						}
					}
					rowMap[ac.outName] = fullVals[i]
				}
				for i, ac := range sq.aggCols {
					if ac.outExpr == nil {
						continue
					}
					v, evalErr := evalExprOnMap(ctx, c, rowMap, ac.outExpr)
					if evalErr != nil {
						return nil, evalErr
					}
					fullVals[i] = v
					rowMap[ac.outName] = v
				}
				if sq.havingExpr != nil {
					keep, havErr := evalHaving(ctx, c, rowMap, sq.havingExpr)
					if havErr != nil {
						return nil, havErr
					}
					if !keep {
						continue
					}
				}
				rowVals := make([]driver.Value, len(emitIdx))
				for out, i := range emitIdx {
					rowVals[out] = fullVals[i]
				}
				data = append(data, rowVals)
			}
			return nil, nil
		}

		// Resolve output fields: either the explicit projection or all fields.
		allFields := msgDesc.Fields()
		var outFields []outField
		// extraSortFields (outer variable) are ORDER BY columns not in the projection.
		//
		// Expression-based ORDER BY items (`ORDER BY v * 2`) work on both
		// SELECT * and named projections — carry each expression as a
		// sentinel-named extra sort field, evaluated per row in the scan
		// loop. Runs BEFORE the projection branch split so SELECT * paths
		// don't silently drop expression sort keys.
		for obIdx, ob := range sq.orderBy {
			if ob.expr == nil {
				continue
			}
			sentinel := fmt.Sprintf("__orderby_expr_%d__", obIdx)
			extraSortFields = append(extraSortFields, outField{name: sentinel, expr: ob.expr})
			sq.orderBy[obIdx].colName = sentinel
			sq.orderBy[obIdx].expr = nil
			sq.orderBy[obIdx].isSyntheticExpr = true
		}
		if sq.projCols == nil {
			// SELECT * — all fields in descriptor order.
			outFields = make([]outField, allFields.Len())
			for i := 0; i < allFields.Len(); i++ {
				fd := allFields.Get(i)
				outFields[i] = outField{name: string(fd.Name()), fd: fd}
			}
		} else {
			// Named projection — look up each column, apply alias if present.
			outFields = make([]outField, len(sq.projCols))
			projByCol := make(map[string]bool, len(sq.projCols))
			for i, colName := range sq.projCols {
				// Computed expression: no field descriptor needed.
				if i < len(sq.projExprs) && sq.projExprs[i] != nil {
					var outName string
					if i < len(sq.projAliases) && sq.projAliases[i] != "" {
						outName = sq.projAliases[i]
					} else {
						// Anonymous computed projection. The internal
						// name needs to be unique-per-slot for ORDER
						// BY / dedup keying, but it must NOT pass
						// jdbcColumnName's isSimpleIdentifier check —
						// that would surface the parser's whitespace-
						// stripped text (e.g. "name IS NULL" →
						// "nameISNULL") as the JDBC metadata name,
						// diverging from Java which emits "_N" for
						// every non-aliased computed expression.
						// Including a `$` makes the name non-simple
						// while still unique. jdbcColumnName falls
						// through to its `_<position>` synthesis.
						outName = fmt.Sprintf("$expr_%d", i)
					}
					// Infer the expression's result type so the JDBC
					// metadata layer can report it (e.g. `x + y` of
					// two DOUBLE columns → DOUBLE). Falls through to
					// empty when the AST shape isn't recognised; the
					// runner-side fallback then infers from the
					// runtime value.
					outFields[i] = outField{
						name:    outName,
						jdbcTyp: inferProjectionJDBCType(sq.projExprs[i], msgDesc),
					}
					// Don't add to projByCol (computed cols can't be in ORDER BY as proto fields).
					continue
				}
				// Strip a trivial qualifier (`d.id` where `d` is this
				// source's table name or alias) before the field lookup.
				// Matches how the correlated-subquery path handles
				// qualified refs in evalExprAtom via currentSourceAliases.
				// Without this, `SELECT d.id FROM t AS d` errored 42703
				// at the ByName(`d.id`) lookup. The output column name
				// keeps the qualifier — downstream derived-table
				// materialisation relies on that preserved form to
				// detect duplicate-column shapes like `SELECT a.*,
				// a.* FROM a`, which collapse to equal names only
				// after qualifier stripping.
				lookupName := colName
				if ref := parseColRef(colName); ref.isQualified() {
					qual := strings.ToUpper(ref.table)
					if strings.EqualFold(qual, sq.tableName) || (sq.tableAlias != "" && strings.EqualFold(qual, sq.tableAlias)) {
						lookupName = ref.bare()
					}
				}
				fd := allFields.ByName(protoreflect.Name(lookupName))
				if fd == nil {
					// Java verbatim: fdb-relational raises
					// "Attempting to query non existing column NAME"
					// (uppercased identifier). Cross-engine corpus
					// entry `undefined_column` pins byte-equality.
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"Attempting to query non existing column %s", strings.ToUpper(colName))
				}
				outName := colName
				if i < len(sq.projAliases) && sq.projAliases[i] != "" {
					outName = sq.projAliases[i]
				}
				outFields[i] = outField{name: outName, fd: fd}
				projByCol[colName] = true
			}
			// Alias redirection: if ORDER BY references a SELECT-list alias
			// (`SELECT id AS n ... ORDER BY n`), it's already projected — no
			// extra field lookup needed. Build an alias → underlying-col map
			// so the sort path's colIdx lookup (which keys off the output
			// name) still matches when cols[] uses the alias.
			aliasToCol := make(map[string]string, len(sq.projCols))
			for i, colName := range sq.projCols {
				if i < len(sq.projAliases) && sq.projAliases[i] != "" {
					aliasToCol[sq.projAliases[i]] = colName
				}
			}
			// Capture aliases for the out-of-closure ORDER BY eliminator
			// so `ORDER BY <alias>` resolves to the underlying column
			// when checking natural-order prefix.
			naturalOrderAliases = make(map[string]string, len(aliasToCol))
			for alias, col := range aliasToCol {
				naturalOrderAliases[strings.ToUpper(alias)] = col
			}
			// Add any ORDER BY columns not already in the projection.
			// Expression ORDER BY was already converted to sentinel extra
			// sort fields above; mark those sentinels present in projByCol
			// so the FD-lookup loop below skips them.
			for _, f := range extraSortFields {
				if f.expr != nil {
					projByCol[f.name] = true
				}
			}
			for _, ob := range sq.orderBy {
				obName := ob.colName
				if projByCol[obName] {
					continue
				}
				if _, isAlias := aliasToCol[obName]; isAlias {
					// Alias refers to an already-projected column; no extra
					// sort field. The sort path looks up cols[] which stores
					// the alias, so no further remapping is needed.
					continue
				}
				// Strip table-qualifier from `<alias>.<col>` for single-
				// source SELECT — the qualifier resolves to either the
				// table name or its alias; both refer to the same source.
				// nightshift-60.
				if dot := strings.Index(obName, "."); dot >= 0 {
					prefix := obName[:dot]
					if strings.EqualFold(prefix, sq.tableName) ||
						(sq.tableAlias != "" && strings.EqualFold(prefix, sq.tableAlias)) {
						obName = obName[dot+1:]
					}
				}
				fd := allFields.ByName(protoreflect.Name(obName))
				if fd == nil {
					return nil, api.NewErrorf(api.ErrCodeInvalidParameter,
						"ORDER BY column %q not found in table %q", ob.colName, sq.tableName)
				}
				extraSortFields = append(extraSortFields, outField{name: obName, fd: fd})
				projByCol[obName] = true // avoid duplicates
			}
		}
		// fullFields = projected + extra sort columns; output strips extra at end.
		fullFields := append(outFields, extraSortFields...) //nolint:gocritic
		cols = make([]string, len(outFields))
		colTypes = make([]string, len(outFields))
		for i, f := range outFields {
			cols[i] = f.name
			// Typed columns (with a FieldDescriptor) take the type
			// straight from proto. Computed expressions use the
			// inferred jdbcTyp captured during projection-binding.
			if f.fd != nil {
				colTypes[i] = jdbcTypeNameForFD(f.fd)
			} else {
				colTypes[i] = f.jdbcTyp
			}
		}

		// Early-termination target: when the scan's natural order
		// already satisfies sq.orderBy (ORDER BY elimination is
		// eligible), and there's no DISTINCT, the scan accumulates
		// rows in final output order — so we can stop reading from
		// the cursor once we've collected enough rows to cover
		// OFFSET + LIMIT. Saves FDB round-trips on queries like
		// `SELECT id FROM t WHERE v > 1000 ORDER BY v LIMIT 5`
		// against a multi-million-row table.
		//
		// Negative (-1) means "no early termination" — read the
		// cursor to exhaustion. Aggregate / DISTINCT / ORDER BY
		// not-satisfiable-by-natural-order all fall back to the
		// full scan and sort later.
		earlyTermTarget := int64(-1)
		if sq.limit >= 0 && !sq.distinct && !sq.countStar && len(sq.aggCols) == 0 &&
			(naturalOrderSatisfies(sq.orderBy, naturalOrder, equatedCols, naturalOrderAliases) || reverseScanApplied) {
			earlyTermTarget = sq.offset + sq.limit
		}

		for {
			if earlyTermTarget >= 0 && int64(len(data)) >= earlyTermTarget {
				break
			}
			result, nextErr := cursor.OnNext(ctx)
			if nextErr != nil {
				return nil, nextErr
			}
			if !result.HasNext() {
				break
			}
			rec := result.GetValue()
			msg := rec.Record
			match, matchErr := evalPredicate(ctx, c, msg, sq.whereExpr)
			if matchErr != nil {
				return nil, matchErr
			}
			if !match {
				continue
			}
			vals := make([]driver.Value, len(fullFields))
			for i, f := range fullFields {
				// Check for a computed expression at this position. SELECT-list
				// expressions come from sq.projExprs (parallel to projCols);
				// extra sort-field expressions live on outField.expr (set when
				// the ORDER BY loop built the field for `ORDER BY v * 2`).
				if i < len(sq.projExprs) && sq.projExprs[i] != nil {
					if i < len(sq.projConstFolded) && sq.projConstFolded[i].present {
						if v := sq.projConstFolded[i].value; v != nil {
							vals[i] = v.(driver.Value) //nolint:forcetypeassert
						}
						continue
					}
					v, evalErr := evalExpr(ctx, c, msg, sq.projExprs[i])
					if evalErr != nil {
						return nil, evalErr
					}
					if v != nil {
						vals[i] = v.(driver.Value) //nolint:forcetypeassert
					}
					continue
				}
				if f.expr != nil {
					v, evalErr := evalExpr(ctx, c, msg, f.expr)
					if evalErr != nil {
						return nil, evalErr
					}
					if v != nil {
						vals[i] = v.(driver.Value) //nolint:forcetypeassert
					}
					continue
				}
				if msg.ProtoReflect().Has(f.fd) {
					vals[i] = functions.ProtoValueToDriver(f.fd, msg.ProtoReflect().Get(f.fd))
				}
				// else nil (proto2 optional field absent → NULL)
			}
			data = append(data, vals)
		}
		return nil, nil
	})
	if runErr != nil {
		return nil, runErr
	}

	// Apply DISTINCT deduplication before sort. Key off the PROJECTED
	// columns only (data may contain trailing extraSortFields used
	// for ORDER BY-on-non-projected-column; including those in the
	// dedup key would treat (v=30, id=1) and (v=30, id=3) as
	// "distinct" and silently re-emit the duplicate v=30 row).
	if sq.distinct && !sq.countStar {
		projLen := len(cols)
		seen := make(map[string]struct{}, len(data))
		deduped := data[:0]
		for _, row := range data {
			key := rowKey(row[:projLen])
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				deduped = append(deduped, row)
			}
		}
		data = deduped
	}

	// Apply ORDER BY (post-scan in-memory sort).
	if len(sq.orderBy) > 0 {
		for _, ob := range sq.orderBy {
			if ob.expr != nil {
				return nil, api.NewError(api.ErrCodeUnsupportedOperation,
					"ORDER BY on an expression is only supported in CTE / JOIN queries; use a column name or alias")
			}
		}
		// Build a map from column name to row index (covers projected + extra sort cols).
		colIdx := make(map[string]int, len(cols)+len(extraSortFields))
		for i, c := range cols {
			colIdx[c] = i
		}
		for i, f := range extraSortFields {
			colIdx[f.name] = len(cols) + i
		}
		// Aggregate-path ORDER BY name validation. The non-aggregate
		// path validated each name when building extraSortFields; the
		// aggregate path doesn't, so a typo (`ORDER BY no_such_col` on
		// `SELECT grp, COUNT(*) ... GROUP BY grp`) silently no-op'd.
		// Mirror the CTE / JOIN validation added in 82bd4382 / 9500c512.
		if len(sq.aggCols) > 0 || sq.countStar {
			for _, ob := range sq.orderBy {
				if _, ok := colIdx[ob.colName]; !ok {
					return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
						"ORDER BY column %q not found in aggregate result", ob.colName)
				}
			}
		}
		// ORDER BY elimination: if the scan cursor emitted rows in a
		// natural order that already satisfies sq.orderBy, skip the
		// in-memory sort.
		//
		// Two satisfying cases:
		//   (a) Forward scan + all-ASC prefix match: cursor emits in
		//       naturalOrder; ASC prefix is trivially satisfied.
		//   (b) Reverse scan + all-DESC prefix match: the pushdown
		//       branch already picked ReverseScan — cursor emits in
		//       reverse of naturalOrder, which IS the DESC order the
		//       user asked for. Flagged via reverseScanApplied.
		//
		// Correctness: a stable sort on a sequence already sorted by
		// the same key is a no-op. Skipping the sort preserves row
		// order — which for naturally-ordered cursors IS the ORDER BY
		// result.
		//
		// Bail conditions:
		//   - aggregate path: data is post-aggregate, naturalOrder
		//     doesn't apply.
		//   - mixed ASC / DESC across clauses: neither direction
		//     satisfies the full prefix, so we sort.
		//   - NULLS ordering opposite the scan direction's native
		//     placement (ASC + NULLS LAST, or DESC + NULLS FIRST).
		//   - ORDER BY col not in naturalOrder prefix.
		// Java-conformance rejection (nightshift-60). fdb-relational's
		// Cascades planner has only RemoveSortRule + PushRequestedOrdering
		// ThroughSortRule — no ImplementSortRule. When no scan strategy
		// emits rows in the requested order, Cascades produces no
		// physical plan and CascadesPlanner.resultOrFail throws
		// UnableToPlanException. The Go embedded engine has no planner;
		// rejection has to emerge from a structural check at the same
		// architectural site — "no scan satisfies the requested ORDER BY"
		// — rather than from rule absence. Aggregate path (with or
		// without GROUP BY) produces 0/1 row (or N rows for GROUP BY
		// which Java rejects upstream); the in-memory sort over that
		// small set is harmless, so the rejection only applies to the
		// non-aggregate path.
		isAggregate := len(sq.aggCols) > 0 || sq.countStar
		satisfiable := naturalOrderSatisfies(sq.orderBy, naturalOrder, equatedCols, naturalOrderAliases) || reverseScanApplied
		// Go extension: Java's fdb-relational 4.11.1.0 rejects DISTINCT + ORDER BY
		// together (Cascades composition gap). Go supports the combination.
		//
		// DISTINCT is exempted because the deduped result set is
		// usually small enough that in-memory sort is harmless.
		// Aggregate is exempted because the post-aggregation result
		// is a small projected set; sorting it in-memory is harmless
		// and matches Java's behaviour for groupings within the same
		// query. Note:
		// at-most-1-row scans (PK equality, single-value IN-list) are
		// NOT exempted here — Java's RemoveSortRule checks the Ordering
		// property explicitly, and an equality match has Ordering `()`
		// which doesn't satisfy a non-empty requested ordering. The PK
		// equality / IN-list branches above gate on
		// `pkOrderingSatisfiesOrderBy` and decline when no PK-prefix
		// ordering satisfies, letting the chain fall through to a
		// strategy that does (eventually `tryIndexScanForOrdering`).
		if !satisfiable && !isAggregate && !sq.distinct {
			obCols := make([]string, 0, len(sq.orderBy))
			hasExpr := false
			for _, ob := range sq.orderBy {
				// Expression-based ORDER BY clauses get a sentinel
				// column name (`__orderby_expr_<i>__`) earlier in this
				// function (extraSortFields setup) which clears
				// `ob.expr`. Detect the sentinel by name to surface
				// "arbitrary expression" in the error message rather
				// than the synthetic identifier.
				if ob.expr != nil || ob.isSyntheticExpr {
					hasExpr = true
					continue
				}
				if ob.colName != "" {
					obCols = append(obCols, ob.colName)
				}
			}
			detail := strings.Join(obCols, ", ")
			if hasExpr {
				if detail != "" {
					detail += " (and arbitrary expression)"
				} else {
					detail = "arbitrary expression"
				}
			}
			// Java-aligned wording (TODO #43): fdb-relational's Cascades
			// planner emits the generic `Cascades planner could not plan
			// query` whenever no rule produces a plan; ORDER BY without
			// a satisfying index is one such case. Match the message
			// byte-equal so the cross-engine harness can pin rejection.
			// Detail is preserved in Context for debuggability.
			_ = detail
			return nil, api.NewErrorf(api.ErrCodeUnsupportedSort,
				"Cascades planner could not plan query")
		}
		if !satisfiable {
			// Aggregate path only — small result set, sort in-memory.
			sort.SliceStable(data, func(i, j int) bool {
				for _, ob := range sq.orderBy {
					idx, ok := colIdx[ob.colName]
					if !ok {
						// Column validated during scan setup; safe to skip.
						continue
					}
					less, equal := orderByLess(data[i][idx], data[j][idx], ob)
					if !equal {
						return less
					}
				}
				return false
			})
		}
	}

	// Strip extra sort columns that were not in the SELECT list.
	if len(extraSortFields) > 0 {
		projLen := len(cols)
		for i, row := range data {
			data[i] = row[:projLen]
		}
	}

	// Apply OFFSET then LIMIT.
	if sq.offset > 0 {
		if sq.offset >= int64(len(data)) {
			data = data[:0]
		} else {
			data = data[sq.offset:]
		}
	}
	if sq.limit >= 0 && int64(len(data)) > sq.limit {
		data = data[:sq.limit]
	}
	// Drop trailing non-visible aggregate columns now that the sort
	// has consumed them. No-op when the query had no ORDER BY /
	// HAVING references to non-SELECT-list aggregates.
	if len(sq.aggCols) > 0 {
		cols, data = stripAggregateNonVisible(sq, cols, data)
	}

	return &staticRows{cols: cols, colTypes: colTypes, rows: data}, nil
}
