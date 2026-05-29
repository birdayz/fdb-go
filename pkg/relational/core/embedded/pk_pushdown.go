package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Primary-key pushdown extractors and cursor builders.
//
// Four shapes (narrowest first):
//   1. Equality — every PK col pinned to a literal. Single-key lookup.
//   2. Range / BETWEEN — single-col PK with `>`/`>=`/`<`/`<=`/BETWEEN
//      bounds. Tuple-range scan.
//   3. Composite range — composite PK with equalities on the leading
//      cols + a range on the first un-equated col. Same shape as #2
//      but anchored by a prefix.
//   4. (See pk_prefix_pushdown.go) Composite pure-prefix — equalities
//      on a leading PK subset, no range / IN-list on any col.
//
// SELECT's execSelectQueryFull picks the tightest narrowing inline so it
// can record naturalOrder for ORDER BY elimination.
//
// File companion: see in_list_pushdown.go for the IN-list shapes,
// like_prefix_pushdown.go for LIKE, covering_index.go for index-
// covered variants, secondary_index_pushdown.go for secondary-
// index equivalents, pk_prefix_pushdown.go for pure-prefix.

// tryPKEqualityPushdown reports whether `sq`'s WHERE clause is a
// simple primary-key equality match (`pk_col = literal`) against a
// record type whose primary key is a single user field (possibly
// prefixed by RecordTypeKey). When it returns true, the caller can
// narrow the scan from ScanRecordsByType to ScanRecordsInRange on the
// exact PK — reducing a full type scan to a single KV lookup.
//
// Conservative on purpose: bails on anything complicated
// (aggregates/GROUP BY/count*, composite PKs, AND chains, OR, qualified
// column refs the parser doesn't cleanly match to the bare PK field
// name, non-literal RHS). The fallback is the existing scan, which is
// correct but slower.
//
// Returns the single TupleElement for the equality literal when
// viable. The caller constructs the full FDB range key from it
// together with `rt.GetRecordTypeKey()`.
func (c *EmbeddedConnection) tryPKEqualityPushdown(
	ctx context.Context,
	sq *selectQuery,
	rt *recordlayer.RecordType,
) ([]any, bool) {
	// Gate on SELECT-specific shape. Aggregate / GROUP BY / COUNT(*)
	// paths all run through their own code below the cursor; leave
	// them to the full scan. HAVING similarly evaluates post-aggregate.
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return nil, false
	}
	if sq.havingExpr != nil {
		return nil, false
	}
	return c.tryPKEqualityFromWhere(ctx, sq.whereExpr, rt)
}

// tryPKEqualityFromWhere is the shared core of PK equality pushdown:
// given a WHERE expression and a record type, it reports whether the
// WHERE resolves to an AND-chain of equalities covering every PK
// column with literals of the right type. Used by SELECT, UPDATE,
// and DELETE call sites; each caller layers its own shape gates on
// top (e.g. SELECT bails on aggregates).
func (c *EmbeddedConnection) tryPKEqualityFromWhere(
	ctx context.Context,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
) ([]any, bool) {
	if whereExpr == nil {
		return nil, false
	}

	// Primary key shape: an ordered list of user fields. Accepts a
	// bare FieldKeyExpression (single col, intermingled table) or a
	// CompositeKeyExpression whose children are
	// [RecordTypeKey?, Field(col1), Field(col2), ...] (the two shapes
	// CREATE TABLE emits).
	pkCols := extractPKUserFields(rt.PrimaryKey)
	if len(pkCols) == 0 {
		return nil, false
	}

	// Walk the WHERE into a flat list of AND-joined leaf predicates.
	// Any non-AND operator (OR, XOR, NOT) bails — conservative for MVP.
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return nil, false
	}

	// Collect `col = literal` equalities from the leaves. Non-equality
	// leaves (e.g. `other_col > 10`, `SIN(x) > 0`, `IS NULL`) are kept
	// in the AND chain: they'll be re-evaluated by the scan loop's
	// evalPredicate after the narrowed cursor yields at most one row.
	// This is safe because the range scan is a SUPERSET of the
	// matching rows and the existing WHERE filter still runs on the
	// loaded record.
	equalities := make(map[string]any, len(leaves))
	for _, leaf := range leaves {
		col, val, ok := extractColEqualsLiteral(ctx, c, leaf)
		if !ok {
			continue
		}
		equalities[strings.ToUpper(col)] = val
	}

	// Build the PK tuple in declared order. Every PK column must
	// appear in the WHERE; partial matches can't be narrowed to a
	// single key.
	pkVals := make([]any, len(pkCols))
	for i, col := range pkCols {
		val, ok := equalities[strings.ToUpper(col)]
		if !ok {
			return nil, false
		}
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
		if fd == nil {
			return nil, false
		}
		// Type compatibility: the literal must match the PK column's
		// proto kind. See literalMatchesPKKind for rationale — a
		// type-mismatched literal must fall through to the scan so
		// evalPredicate surfaces 22000.
		if !functions.LiteralMatchesPKKind(val, fd.Kind()) {
			return nil, false
		}
		pkVals[i] = val
	}
	return pkVals, true
}

// pkPushdownScanCursor builds a tuple-prefix range scan for a
// fully-or-partially-determined primary key. When pkVals covers every
// PK col (the equality / 1-element IN-list paths) the scan yields at
// most one record; when pkVals covers only a leading subset (the
// composite pure-prefix path) the scan yields every record whose PK
// starts with that prefix. extractPKUserFields gates this to the
// RecordTypeKey-prefixed PK shape only, so the tuple is always
// `{rtk, pkVal1, pkVal2, ...}` — the prefix anchors the scan to the
// right record type, matching ScanRecordsByType's fast-path semantics
// (see primaryKeyHasRecordTypePrefix in pkg/recordlayer/store.go).
func pkPushdownScanCursor(
	store *recordlayer.FDBRecordStore,
	rt *recordlayer.RecordType,
	pkVals []any,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	low := make(tuple.Tuple, 0, 1+len(pkVals))
	low = append(low, rt.GetRecordTypeKey())
	for _, v := range pkVals {
		low = append(low, v)
	}
	return store.ScanRecordsInRange(
		low, low,
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, scanProps,
	)
}

// pkRangeBounds describes an open or half-open range constraint on a
// single-column primary key derived from WHERE. Either bound may be
// absent (represented by the `has…` flag), in which case the scan
// runs to the corresponding end of the record-type range.
type pkRangeBounds struct {
	hasLow, hasHigh             bool
	low, high                   any
	lowInclusive, highInclusive bool
}

// pkPushdownRangeScanCursor builds a range scan bounded by `bounds`.
// When only one side is set, the other falls back to the end of the
// record-type range (`{rtk}` with RangeInclusive, matching
// ScanRecordsByType's prefix semantics).
func pkPushdownRangeScanCursor(
	store *recordlayer.FDBRecordStore,
	rt *recordlayer.RecordType,
	bounds pkRangeBounds,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	rtk := rt.GetRecordTypeKey()
	var low, high tuple.Tuple
	lowEp := recordlayer.EndpointTypeRangeInclusive
	highEp := recordlayer.EndpointTypeRangeInclusive
	if bounds.hasLow {
		low = tuple.Tuple{rtk, bounds.low}
		if bounds.lowInclusive {
			lowEp = recordlayer.EndpointTypeRangeInclusive
		} else {
			lowEp = recordlayer.EndpointTypeRangeExclusive
		}
	} else {
		low = tuple.Tuple{rtk}
	}
	if bounds.hasHigh {
		high = tuple.Tuple{rtk, bounds.high}
		if bounds.highInclusive {
			highEp = recordlayer.EndpointTypeRangeInclusive
		} else {
			highEp = recordlayer.EndpointTypeRangeExclusive
		}
	} else {
		high = tuple.Tuple{rtk}
	}
	return store.ScanRecordsInRange(low, high, lowEp, highEp, nil, scanProps)
}

// tryPKRangePushdown is the SELECT-gated variant of
// tryPKRangeFromWhere. Same shape gates as tryPKEqualityPushdown.
func (c *EmbeddedConnection) tryPKRangePushdown(
	ctx context.Context,
	sq *selectQuery,
	rt *recordlayer.RecordType,
) (pkRangeBounds, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return pkRangeBounds{}, false
	}
	if sq.havingExpr != nil {
		return pkRangeBounds{}, false
	}
	return c.tryPKRangeFromWhere(ctx, sq.whereExpr, rt)
}

// pkCompositeRange describes a composite-PK range scan. `prefixVals`
// holds the equality-equated values for every PK column BEFORE the
// first range column; `lastBounds` holds the range bounds for that
// first range column. PK columns AFTER the range column are
// unconstrained here and re-applied as post-filter by the scan loop.
// Name historical — the "last" in `lastBounds` predates the dd97a817
// relaxation that allows the range col to sit anywhere in the PK.
type pkCompositeRange struct {
	prefixVals []any // equalities for PK cols 0..rangeK-1
	lastBounds pkRangeBounds
}

// tryPKCompositeRangePushdown is the SELECT-gated variant of
// tryPKCompositeRangeFromWhere.
func (c *EmbeddedConnection) tryPKCompositeRangePushdown(
	ctx context.Context,
	sq *selectQuery,
	rt *recordlayer.RecordType,
) (pkCompositeRange, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return pkCompositeRange{}, false
	}
	if sq.havingExpr != nil {
		return pkCompositeRange{}, false
	}
	return c.tryPKCompositeRangeFromWhere(ctx, sq.whereExpr, rt)
}

// tryPKCompositeRangeFromWhere recognises the case of a composite
// PK where the first PK column with a range predicate is preceded by
// equalities on all earlier PK columns. The range column contributes
// the scan bounds; any equality/range leaves on PK columns AFTER the
// range column are ignored here and re-applied by the scan loop's
// evalPredicate post-filter.
//
// Why relaxed past the last column: composite key ranges in FDB are
// contiguous as long as the prefix is fixed and the first varying
// component carries a bound. Pinning unrelated trailing columns in
// the range tuple would over-constrain the key space. For PK cols
// after the range col we therefore leave the scan bounds open and
// post-filter.
//
// Pure-equality composite is handled by tryPKEqualityFromWhere at
// every call site and runs first; if we see no range on any PK col
// we bail. A PK whose leading equalities are partial (e.g. `a=1`
// without a range on b for PK (a,b,c)) also bails here — a pure
// prefix narrowing is a separate feature.
func (c *EmbeddedConnection) tryPKCompositeRangeFromWhere(
	ctx context.Context,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
) (pkCompositeRange, bool) {
	if whereExpr == nil {
		return pkCompositeRange{}, false
	}
	pkCols := extractPKUserFields(rt.PrimaryKey)
	if len(pkCols) < 2 {
		// Single-col PKs go through tryPKRangeFromWhere; 0-col PKs
		// can't be pushed down at all.
		return pkCompositeRange{}, false
	}
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return pkCompositeRange{}, false
	}
	// Walk AND leaves once, splitting by op into equalities and range
	// bounds. Non-PK leaves stay as post-filter (ignored here).
	equalities := make(map[string]any, len(pkCols))
	rangeByCol := make(map[string]pkRangeBounds, len(pkCols))
	isPKCol := func(col string) bool {
		for _, pkc := range pkCols {
			if strings.EqualFold(pkc, col) {
				return true
			}
		}
		return false
	}
	for _, leaf := range leaves {
		if col, lo, hi, ok := extractColBetweenLiteral(ctx, c, leaf); ok {
			if !isPKCol(col) {
				continue
			}
			colUpper := strings.ToUpper(col)
			b := rangeByCol[colUpper]
			b.hasLow = true
			b.low = lo
			b.lowInclusive = true
			b.hasHigh = true
			b.high = hi
			b.highInclusive = true
			rangeByCol[colUpper] = b
			continue
		}
		if col, prefix, ok := extractColLikePrefixLiteral(ctx, c, leaf); ok {
			if !isPKCol(col) {
				continue
			}
			lb := likePrefixToPKRangeBounds(prefix)
			colUpper := strings.ToUpper(col)
			b := rangeByCol[colUpper]
			b.hasLow = true
			b.low = lb.low
			b.lowInclusive = lb.lowInclusive
			if lb.hasHigh {
				b.hasHigh = true
				b.high = lb.high
				b.highInclusive = lb.highInclusive
			}
			rangeByCol[colUpper] = b
			continue
		}
		op, col, val, ok := extractColOpLiteral(ctx, c, leaf)
		if !ok {
			continue
		}
		if !isPKCol(col) {
			continue
		}
		colUpper := strings.ToUpper(col)
		switch op {
		case "=":
			equalities[colUpper] = val
		case ">":
			b := rangeByCol[colUpper]
			b.hasLow = true
			b.low = val
			b.lowInclusive = false
			rangeByCol[colUpper] = b
		case ">=":
			b := rangeByCol[colUpper]
			b.hasLow = true
			b.low = val
			b.lowInclusive = true
			rangeByCol[colUpper] = b
		case "<":
			b := rangeByCol[colUpper]
			b.hasHigh = true
			b.high = val
			b.highInclusive = false
			rangeByCol[colUpper] = b
		case "<=":
			b := rangeByCol[colUpper]
			b.hasHigh = true
			b.high = val
			b.highInclusive = true
			rangeByCol[colUpper] = b
		}
	}
	// Find the first PK col (in declared order) carrying a range
	// bound. Every PK col before it must have an equality.
	rangeK := -1
	for i, col := range pkCols {
		if _, has := rangeByCol[strings.ToUpper(col)]; has {
			rangeK = i
			break
		}
	}
	if rangeK == -1 {
		return pkCompositeRange{}, false
	}
	prefixVals := make([]any, rangeK)
	for i := 0; i < rangeK; i++ {
		col := pkCols[i]
		val, ok := equalities[strings.ToUpper(col)]
		if !ok {
			return pkCompositeRange{}, false
		}
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
		if fd == nil || !functions.LiteralMatchesPKKind(val, fd.Kind()) {
			return pkCompositeRange{}, false
		}
		prefixVals[i] = val
	}
	rangeCol := pkCols[rangeK]
	bounds := rangeByCol[strings.ToUpper(rangeCol)]
	if !bounds.hasLow && !bounds.hasHigh {
		// Shouldn't happen — rangeK was only set when at least one
		// bound existed — but keep the guard for clarity.
		return pkCompositeRange{}, false
	}
	rangeFD := rt.Descriptor.Fields().ByName(protoreflect.Name(rangeCol))
	if rangeFD == nil {
		return pkCompositeRange{}, false
	}
	if bounds.hasLow && !functions.LiteralMatchesPKKind(bounds.low, rangeFD.Kind()) {
		return pkCompositeRange{}, false
	}
	if bounds.hasHigh && !functions.LiteralMatchesPKKind(bounds.high, rangeFD.Kind()) {
		return pkCompositeRange{}, false
	}
	return pkCompositeRange{prefixVals: prefixVals, lastBounds: bounds}, true
}

// pkPushdownCompositeRangeScanCursor builds a range scan whose low
// and high tuples share the same leading prefix (the equated PK cols
// before the range col) and differ only in the range col. Since the
// dd97a817 relaxation the range col may sit at any position in the
// PK, not just the last — `cr.prefixVals` is the equated prefix
// before it, `cr.lastBounds` are its bounds, and PK cols after the
// range col are unconstrained here and re-applied as post-filter by
// the scan loop. When the range is open on one side, the corresponding
// tuple falls back to the prefix (inclusive) — covering the full
// range of that prefix's suffix values in either direction.
func pkPushdownCompositeRangeScanCursor(
	store *recordlayer.FDBRecordStore,
	rt *recordlayer.RecordType,
	cr pkCompositeRange,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	rtk := rt.GetRecordTypeKey()
	prefix := make(tuple.Tuple, 0, 1+len(cr.prefixVals))
	prefix = append(prefix, rtk)
	for _, v := range cr.prefixVals {
		prefix = append(prefix, v)
	}
	low := append(tuple.Tuple{}, prefix...)
	high := append(tuple.Tuple{}, prefix...)
	lowEp := recordlayer.EndpointTypeRangeInclusive
	highEp := recordlayer.EndpointTypeRangeInclusive
	if cr.lastBounds.hasLow {
		low = append(low, cr.lastBounds.low)
		if !cr.lastBounds.lowInclusive {
			lowEp = recordlayer.EndpointTypeRangeExclusive
		}
	}
	if cr.lastBounds.hasHigh {
		high = append(high, cr.lastBounds.high)
		if !cr.lastBounds.highInclusive {
			highEp = recordlayer.EndpointTypeRangeExclusive
		}
	}
	return store.ScanRecordsInRange(low, high, lowEp, highEp, nil, scanProps)
}

// tryPKRangeFromWhere recognises single-column PK range predicates
// (`>`, `>=`, `<`, `<=`, and `BETWEEN lo AND hi`). Returns the
// low/high bounds when viable, or (_, false) otherwise. Single-column
// PKs only; composite PKs with a range on any column go through
// tryPKCompositeRangeFromWhere.
//
// Multiple bounds on the same side are collected with last-write-wins;
// the scan loop's existing WHERE evaluator re-applies the full
// predicate to each loaded row, so correctness holds even when the
// bounds we chose are looser than necessary.
func (c *EmbeddedConnection) tryPKRangeFromWhere(
	ctx context.Context,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
) (pkRangeBounds, bool) {
	if whereExpr == nil {
		return pkRangeBounds{}, false
	}
	pkCols := extractPKUserFields(rt.PrimaryKey)
	if len(pkCols) != 1 {
		return pkRangeBounds{}, false
	}
	pkCol := pkCols[0]

	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return pkRangeBounds{}, false
	}

	fd := rt.Descriptor.Fields().ByName(protoreflect.Name(pkCol))
	if fd == nil {
		return pkRangeBounds{}, false
	}

	var bounds pkRangeBounds
	pkColUpper := strings.ToUpper(pkCol)
	for _, leaf := range leaves {
		// `col BETWEEN lo AND hi` — inclusive on both sides.
		if col, lo, hi, ok := extractColBetweenLiteral(ctx, c, leaf); ok {
			if strings.ToUpper(col) != pkColUpper {
				continue
			}
			if !functions.LiteralMatchesPKKind(lo, fd.Kind()) || !functions.LiteralMatchesPKKind(hi, fd.Kind()) {
				return pkRangeBounds{}, false
			}
			bounds.hasLow = true
			bounds.low = lo
			bounds.lowInclusive = true
			bounds.hasHigh = true
			bounds.high = hi
			bounds.highInclusive = true
			continue
		}
		// `col LIKE 'prefix%'` — narrows to [prefix, strinc(prefix))
		// on STRING-kind PK columns. Other kinds can't carry a LIKE
		// literal anyway (SQL surfaces a type error at eval time).
		if col, prefix, ok := extractColLikePrefixLiteral(ctx, c, leaf); ok {
			if strings.ToUpper(col) != pkColUpper {
				continue
			}
			if fd.Kind() != protoreflect.StringKind {
				return pkRangeBounds{}, false
			}
			lb := likePrefixToPKRangeBounds(prefix)
			bounds.hasLow = true
			bounds.low = lb.low
			bounds.lowInclusive = lb.lowInclusive
			if lb.hasHigh {
				bounds.hasHigh = true
				bounds.high = lb.high
				bounds.highInclusive = lb.highInclusive
			}
			continue
		}
		op, col, val, ok := extractColOpLiteral(ctx, c, leaf)
		if !ok {
			continue
		}
		if strings.ToUpper(col) != pkColUpper {
			continue
		}
		if !functions.LiteralMatchesPKKind(val, fd.Kind()) {
			return pkRangeBounds{}, false
		}
		// `=` intentionally not handled here — the caller tries
		// tryPKEqualityFromWhere first, which succeeds for any valid
		// equality on a single-col PK. Reaching this function with
		// an equality leaf means equality pushdown already rejected
		// the query for some other reason; we skip equalities here
		// and let any range predicates determine the bounds.
		switch op {
		case ">":
			bounds.hasLow = true
			bounds.low = val
			bounds.lowInclusive = false
		case ">=":
			bounds.hasLow = true
			bounds.low = val
			bounds.lowInclusive = true
		case "<":
			bounds.hasHigh = true
			bounds.high = val
			bounds.highInclusive = false
		case "<=":
			bounds.hasHigh = true
			bounds.high = val
			bounds.highInclusive = true
		}
	}
	if !bounds.hasLow && !bounds.hasHigh {
		return pkRangeBounds{}, false
	}
	return bounds, true
}
