package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Secondary-index pushdown extractors and cursor builders.
//
// Three shapes:
//   1. Equality (single-col or composite): every index col equated in
//      WHERE → point scan on the index key.
//   2. Range (single-col): `>`, `>=`, `<`, `<=`, `BETWEEN` on the
//      single-col index → range scan.
//   3. Composite range: equalities on the leading index cols + a
//      range on the first un-equated col → range scan with fixed
//      prefix.
//
// Each extractor returns the index name + a payload (key value(s) or
// bounds) that the paired cursor builder feeds into
// store.ScanIndexRecords. Non-scannable indexes are filtered in-place
// (WRITE_ONLY / DISABLED) so callers never see a mid-build failure.
//
// Lives in its own file — companion to in_list_pushdown.go,
// covering_index.go, like_prefix_pushdown.go, pk_prefix_pushdown.go
// — so Phase 1c / 1e of RFC 021 can lift each pushdown shape into
// its own plan/physical operator without churning connection.go.

// trySecondaryIndexPushdown looks for a VALUE index on a single bare
// column that matches a `col = literal` leaf in WHERE, and returns a
// cursor over the index-scoped records. This lets `SELECT ... WHERE
// status = 'active'` scan the `status` index range instead of the
// full type range. Conservative MVP:
//   - single-column VALUE index only (RootExpression = Field(col))
//   - any AND-chain leaf that equals the index column with a literal
//     triggers the match; other leaves stay as post-filter via the
//     scan loop's existing evalPredicate
//   - callers still gate on query shape (aggregates / GROUP BY / …)
//     via tryPKRangePushdown's pattern
func (c *EmbeddedConnection) trySecondaryIndexPushdown(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (indexName string, keyVal any, ok bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return "", nil, false
	}
	if sq.havingExpr != nil {
		return "", nil, false
	}
	return c.trySecondaryIndexFromWhere(ctx, store, sq.whereExpr, rt, md)
}

// trySecondaryIndexFromWhere is the shared core of secondary-index
// pushdown: given a WHERE and the record type + metadata, find a
// single-column VALUE index that matches a col=literal equality in
// the AND chain. UPDATE / DELETE call this directly (no aggregate
// gates apply to mutations); SELECT wraps it in
// trySecondaryIndexPushdown above.
//
// Non-scannable indexes (WRITE_ONLY / DISABLED) are skipped inside
// the iteration so a later scannable match can still be picked up —
// without this the caller would have to fall through to a full scan
// even when a different index would work.
func (c *EmbeddedConnection) trySecondaryIndexFromWhere(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (indexName string, keyVal any, ok bool) {
	if whereExpr == nil {
		return "", nil, false
	}
	leaves, lok := flattenAndPredicates(whereExpr.Expression())
	if !lok {
		return "", nil, false
	}
	// Collect bare col=literal equalities into a map.
	equalities := make(map[string]any, len(leaves))
	for _, leaf := range leaves {
		col, val, leafOk := extractColEqualsLiteral(ctx, c, leaf)
		if !leafOk {
			continue
		}
		equalities[strings.ToUpper(col)] = val
	}
	if len(equalities) == 0 {
		return "", nil, false
	}
	// Scan indexes on this record type. Pick the first scannable
	// VALUE index whose single-column key matches an equality leaf.
	indexes := md.GetIndexesForRecordType(rt.Name)
	for _, idx := range indexes {
		// VALUE index only. NewIndex always stamps Type="value"; the
		// empty-string arm is defensive for hand-constructed Indexes.
		if idx.Type != "" && idx.Type != "value" {
			continue
		}
		if !store.IsIndexScannable(idx.Name) {
			continue
		}
		idxCols := secondaryIndexColumns(idx)
		if len(idxCols) == 0 {
			continue
		}
		// Every index column must have an equality in the AND chain
		// with a type-compatible literal. Tuple is built in declared
		// index-column order.
		vals := make([]any, len(idxCols))
		matched := true
		for i, col := range idxCols {
			v, found := equalities[strings.ToUpper(col)]
			if !found {
				matched = false
				break
			}
			fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
			if fd == nil {
				matched = false
				break
			}
			if !functions.LiteralMatchesPKKind(v, fd.Kind()) {
				matched = false
				break
			}
			vals[i] = v
		}
		if !matched {
			continue
		}
		if len(vals) == 1 {
			return idx.Name, vals[0], true
		}
		// Composite index: pack all values into a single tuple that
		// secondaryIndexPushdownCursor can pass straight to
		// ScanIndexRecords as a point range on the full index key.
		// The tuple is wrapped in a sentinel so the cursor builder
		// can distinguish it from a single-value match.
		return idx.Name, secondaryIndexKeyTuple{values: vals}, true
	}
	return "", nil, false
}

// secondaryIndexKeyTuple wraps the composite-index key values so
// secondaryIndexPushdownCursor can tell composite from single-col
// without reflecting on the concrete type.
type secondaryIndexKeyTuple struct {
	values []any
}

// secondaryIndexColumns returns the ordered list of field names that
// make up a VALUE index's key, or nil if the shape isn't one we push
// down on. Accepts FieldKeyExpression (single column) or
// CompositeKeyExpression whose children are all FieldKeyExpressions
// (the two shapes SQL DDL's buildIndexKeyExpression emits).
func secondaryIndexColumns(idx *recordlayer.Index) []string {
	switch e := idx.RootExpression.(type) {
	case *recordlayer.FieldKeyExpression:
		return e.FieldNames()
	case *recordlayer.CompositeKeyExpression:
		return e.FieldNames()
	}
	return nil
}

// tryIndexScanForOrdering finds a scannable VALUE secondary index
// whose natural emission order (idxCols ++ pkCols) satisfies the
// query's ORDER BY clause. Returns the matching `*Index` and true on
// the first match; called from the scan-strategy chain as the last
// branch before the full-PK fallback. Mirrors Java's Cascades planner
// picking an index scan as the satisfying inner plan for
// `RemoveSortRule` when the index's Ordering property matches the
// requested order, even without WHERE pushdown — without this branch,
// removing the in-memory sort fallback would reject queries Java
// accepts (`SELECT col FROM t ORDER BY indexed_col`). nightshift-60.
//
// Returning the `*Index` (not just its name) lets the caller skip a
// metadata re-lookup whose result it already trusts; the
// metadata-lookup path is the same we just walked.
func tryIndexScanForOrdering(
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
	store *recordlayer.FDBRecordStore,
	pkCols []string,
	equatedCols map[string]bool,
	naturalOrderAliases map[string]string,
) (*recordlayer.Index, bool) {
	if len(sq.orderBy) == 0 {
		return nil, false
	}
	indexes := md.GetIndexesForRecordType(rt.Name)
	for _, idx := range indexes {
		if idx.Type != "" && idx.Type != "value" {
			continue
		}
		if !store.IsIndexScannable(idx.Name) {
			continue
		}
		idxCols := secondaryIndexColumns(idx)
		if len(idxCols) == 0 {
			continue
		}
		idxNaturalOrder := append(append([]string{}, idxCols...), pkCols...)
		if scanSatisfiesOrderBy(sq.orderBy, idxNaturalOrder, equatedCols, naturalOrderAliases) {
			return idx, true
		}
	}
	return nil, false
}

// secondaryIndexPushdownCursor wraps `store.ScanIndexRecords` and
// adapts its `*FDBIndexedRecord` stream to the `*FDBStoredRecord` the
// SQL scan loop expects. The `keyVal` argument is either a single
// value (single-col index) or a `secondaryIndexKeyTuple` carrying
// the composite-index key values in declared order. The returned
// cursor yields only the records matching the exact index key.
func secondaryIndexPushdownCursor(
	store *recordlayer.FDBRecordStore,
	indexName string,
	keyVal any,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	inner := store.ScanIndexRecords(indexName, buildSecondaryIndexEqualityTupleRange(keyVal), nil, scanProps)
	return recordlayer.MapCursor(inner, func(ir *recordlayer.FDBIndexedRecord) *recordlayer.FDBStoredRecord[proto.Message] {
		return ir.Record
	})
}

// secondaryIndexRange describes a range scan on a single-column VALUE
// index: the index name plus the low/high bounds on the indexed
// column. Mirror of pkRangeBounds + index name.
type secondaryIndexRange struct {
	indexName string
	bounds    pkRangeBounds
}

// trySecondaryIndexRangePushdown is the SELECT-gated variant of
// trySecondaryIndexRangeFromWhere.
func (c *EmbeddedConnection) trySecondaryIndexRangePushdown(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexRange, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return secondaryIndexRange{}, false
	}
	if sq.havingExpr != nil {
		return secondaryIndexRange{}, false
	}
	return c.trySecondaryIndexRangeFromWhere(ctx, store, sq.whereExpr, rt, md)
}

// trySecondaryIndexRangeFromWhere looks for a single-column VALUE
// index whose column carries at least one range predicate (`>`,
// `>=`, `<`, `<=`) in the AND chain. Both sides of a bounded range
// are collected (`WHERE col > 5 AND col < 10`). Extra non-indexed
// leaves remain post-filtered by the scan loop's evalPredicate.
//
// Equalities on the indexed column are intentionally skipped here —
// trySecondaryIndexFromWhere is tried first at every call site and
// handles the pure-equality case; if we reach this function with an
// equality leaf, equality pushdown already rejected it (e.g. type
// mismatch) and we must not resurrect it with a range bound.
//
// Multiple range leaves on the same column use last-write-wins; the
// scan loop's evalPredicate re-applies the full WHERE to each loaded
// row, so correctness holds even when the chosen bound is looser.
func (c *EmbeddedConnection) trySecondaryIndexRangeFromWhere(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexRange, bool) {
	if whereExpr == nil {
		return secondaryIndexRange{}, false
	}
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return secondaryIndexRange{}, false
	}
	// Bucket range bounds by (uppercase) column. Equalities skipped.
	// BETWEEN lo AND hi contributes both inclusive bounds to the
	// column's entry. LIKE 'prefix%' contributes [prefix,
	// strinc(prefix)) (half-open, string cols only).
	rangeByCol := make(map[string]pkRangeBounds, len(leaves))
	for _, leaf := range leaves {
		if col, lo, hi, ok := extractColBetweenLiteral(ctx, c, leaf); ok {
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
			colUpper := strings.ToUpper(col)
			lb := likePrefixToPKRangeBounds(prefix)
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
		if !ok || op == "=" {
			continue
		}
		b := rangeByCol[strings.ToUpper(col)]
		switch op {
		case ">":
			b.hasLow = true
			b.low = val
			b.lowInclusive = false
		case ">=":
			b.hasLow = true
			b.low = val
			b.lowInclusive = true
		case "<":
			b.hasHigh = true
			b.high = val
			b.highInclusive = false
		case "<=":
			b.hasHigh = true
			b.high = val
			b.highInclusive = true
		}
		rangeByCol[strings.ToUpper(col)] = b
	}
	if len(rangeByCol) == 0 {
		return secondaryIndexRange{}, false
	}
	// Pick the first scannable single-column VALUE index whose
	// column carries at least one range bound with a type-compatible
	// literal. Non-scannable (WRITE_ONLY / DISABLED) indexes are
	// skipped so a later scannable match can still be picked up.
	indexes := md.GetIndexesForRecordType(rt.Name)
	for _, idx := range indexes {
		if idx.Type != "" && idx.Type != "value" {
			continue
		}
		if !store.IsIndexScannable(idx.Name) {
			continue
		}
		idxCols := secondaryIndexColumns(idx)
		if len(idxCols) != 1 {
			continue
		}
		bounds, found := rangeByCol[strings.ToUpper(idxCols[0])]
		if !found || (!bounds.hasLow && !bounds.hasHigh) {
			continue
		}
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(idxCols[0]))
		if fd == nil {
			continue
		}
		if bounds.hasLow && !functions.LiteralMatchesPKKind(bounds.low, fd.Kind()) {
			continue
		}
		if bounds.hasHigh && !functions.LiteralMatchesPKKind(bounds.high, fd.Kind()) {
			continue
		}
		return secondaryIndexRange{indexName: idx.Name, bounds: bounds}, true
	}
	return secondaryIndexRange{}, false
}

// secondaryIndexRangeScanCursor wraps `store.ScanIndexRecords` with a
// single-column range on the index key, and adapts the resulting
// `*FDBIndexedRecord` stream to `*FDBStoredRecord`. When one side of
// the range is open, the corresponding endpoint falls back to
// TreeStart / TreeEnd so the scan runs to the end of the index range
// in that direction.
func secondaryIndexRangeScanCursor(
	store *recordlayer.FDBRecordStore,
	indexName string,
	bounds pkRangeBounds,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	inner := store.ScanIndexRecords(indexName, buildSecondaryIndexRangeTupleRange(bounds), nil, scanProps)
	return recordlayer.MapCursor(inner, func(ir *recordlayer.FDBIndexedRecord) *recordlayer.FDBStoredRecord[proto.Message] {
		return ir.Record
	})
}

// secondaryIndexCompositeRange describes a range scan on a composite
// VALUE index. `prefixVals` holds the equality-equated values for
// every index column BEFORE the first range column (the "prefix");
// `lastBounds` holds the range bounds for that first range column.
// Index columns AFTER the range column are unconstrained here and
// re-applied as post-filter by the scan loop. Mirror of
// pkCompositeRange but carrying the index name. Name historical —
// the "last" in `lastBounds` predates the 9dcb0bfb relaxation that
// allows the range col to sit anywhere, not only at the end.
type secondaryIndexCompositeRange struct {
	indexName  string
	prefixVals []any // equalities for index cols 0..rangeK-1
	lastBounds pkRangeBounds
}

// trySecondaryIndexCompositeRangePushdown is the SELECT-gated variant
// of trySecondaryIndexCompositeRangeFromWhere.
func (c *EmbeddedConnection) trySecondaryIndexCompositeRangePushdown(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexCompositeRange, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return secondaryIndexCompositeRange{}, false
	}
	if sq.havingExpr != nil {
		return secondaryIndexCompositeRange{}, false
	}
	return c.trySecondaryIndexCompositeRangeFromWhere(ctx, store, sq.whereExpr, rt, md)
}

// trySecondaryIndexCompositeRangeFromWhere recognises composite VALUE
// indexes where the first index column carrying a range predicate is
// preceded by equalities on all earlier index columns. The range
// column contributes the scan bounds; any leaves on index columns
// AFTER the range column are ignored here and re-applied as
// post-filter by the scan loop's evalPredicate.
//
// Pure-equality composite pushdown is handled by
// trySecondaryIndexFromWhere and runs first at every call site, so a
// fully equated composite key never reaches this extractor. Matches
// the relaxation applied to PK composite range pushdown.
func (c *EmbeddedConnection) trySecondaryIndexCompositeRangeFromWhere(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexCompositeRange, bool) {
	if whereExpr == nil {
		return secondaryIndexCompositeRange{}, false
	}
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return secondaryIndexCompositeRange{}, false
	}
	// Walk AND leaves once, splitting into equalities and range bounds.
	// BETWEEN lo AND hi contributes both inclusive bounds.
	//
	// We don't filter to index cols here: without picking an index
	// first we don't know which cols are "index cols", and multiple
	// indexes may share different columns. Non-index leaves land in
	// the maps but are never looked up — the index-selection loop
	// below only probes rangeByCol / equalities on actual idxCols, so
	// asymmetry vs the PK path (which knows its cols upfront and can
	// filter inline) is harmless.
	//
	// Multiple range leaves on the same column use last-write-wins;
	// correctness holds because the scan loop's evalPredicate
	// re-applies the full WHERE to each loaded row, even when the
	// chosen bound is looser.
	equalities := make(map[string]any, len(leaves))
	rangeByCol := make(map[string]pkRangeBounds, len(leaves))
	for _, leaf := range leaves {
		if col, lo, hi, ok := extractColBetweenLiteral(ctx, c, leaf); ok {
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
	if len(rangeByCol) == 0 {
		return secondaryIndexCompositeRange{}, false
	}
	// Pick the first scannable composite VALUE index where the
	// first-in-declared-order index column carrying a range bound is
	// preceded by equalities on every earlier index column.
	// Non-scannable (WRITE_ONLY / DISABLED) indexes are skipped so
	// a later scannable match can still be picked up.
	indexes := md.GetIndexesForRecordType(rt.Name)
	for _, idx := range indexes {
		if idx.Type != "" && idx.Type != "value" {
			continue
		}
		if !store.IsIndexScannable(idx.Name) {
			continue
		}
		idxCols := secondaryIndexColumns(idx)
		if len(idxCols) < 2 {
			continue
		}
		rangeK := -1
		for i, col := range idxCols {
			if _, has := rangeByCol[strings.ToUpper(col)]; has {
				rangeK = i
				break
			}
		}
		if rangeK == -1 {
			continue
		}
		prefixVals := make([]any, rangeK)
		matched := true
		for i := 0; i < rangeK; i++ {
			col := idxCols[i]
			v, found := equalities[strings.ToUpper(col)]
			if !found {
				matched = false
				break
			}
			fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
			if fd == nil || !functions.LiteralMatchesPKKind(v, fd.Kind()) {
				matched = false
				break
			}
			prefixVals[i] = v
		}
		if !matched {
			continue
		}
		rangeCol := idxCols[rangeK]
		bounds := rangeByCol[strings.ToUpper(rangeCol)]
		rangeFD := rt.Descriptor.Fields().ByName(protoreflect.Name(rangeCol))
		if rangeFD == nil {
			continue
		}
		if bounds.hasLow && !functions.LiteralMatchesPKKind(bounds.low, rangeFD.Kind()) {
			continue
		}
		if bounds.hasHigh && !functions.LiteralMatchesPKKind(bounds.high, rangeFD.Kind()) {
			continue
		}
		return secondaryIndexCompositeRange{
			indexName:  idx.Name,
			prefixVals: prefixVals,
			lastBounds: bounds,
		}, true
	}
	return secondaryIndexCompositeRange{}, false
}

// secondaryIndexCompositeRangeScanCursor builds a range scan whose
// low and high tuples share the same leading prefix (the equated
// leading index cols) and differ only in the range component.
//
// Open ends fall back to the prefix tuple with EndpointTypeRangeInclusive,
// not TreeStart/TreeEnd. Inclusive on the high side appends 0xFF to the
// packed prefix (see index_scan.go ToFDBRange), so pack(prefix)+0xFF
// covers every suffix under the prefix — exactly what we want for an
// open-ended composite scan bounded by the equated cols. The single-col
// range cursor uses TreeStart/TreeEnd because there IS no prefix there.
func secondaryIndexCompositeRangeScanCursor(
	store *recordlayer.FDBRecordStore,
	cr secondaryIndexCompositeRange,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	inner := store.ScanIndexRecords(cr.indexName, buildSecondaryIndexCompositeRangeTupleRange(cr), nil, scanProps)
	return recordlayer.MapCursor(inner, func(ir *recordlayer.FDBIndexedRecord) *recordlayer.FDBStoredRecord[proto.Message] {
		return ir.Record
	})
}

// Composite pure-prefix secondary-index pushdown.
//
// `WHERE region = 'us' AND plan = 'pro'` on idx_region_plan(region,
// plan) is a FULL equality — trySecondaryIndexFromWhere handles it.
// `WHERE region = 'us'` alone narrows to the PREFIX `[region='us']`
// of idx_region_plan; this branch covers the one-or-more-leading-
// equalities-with-room-to-spare case that falls through the
// full-equality check. Without it, a partial-prefix equality on a
// composite index falls back to a full type scan — a real loss when
// the leading equated col is selective.
//
// Mirrors tryPKCompositePrefixPushdown for PK cols. Tried LAST of
// the secondary-index branches, after the tighter full-equality /
// IN-list / range / composite-range forms have had a shot.
//
// Bail cases:
//   - Single-col indexes (no composite structure).
//   - No equality on the first index col (prefix would be empty).
//   - Equality on every index col (full-equality path caught it).

type secondaryIndexCompositePrefix struct {
	indexName  string
	prefixVals []any
}

// trySecondaryIndexCompositePrefixPushdown is the SELECT-gated variant
// of trySecondaryIndexCompositePrefixFromWhere.
func (c *EmbeddedConnection) trySecondaryIndexCompositePrefixPushdown(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexCompositePrefix, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return secondaryIndexCompositePrefix{}, false
	}
	if sq.havingExpr != nil {
		return secondaryIndexCompositePrefix{}, false
	}
	return c.trySecondaryIndexCompositePrefixFromWhere(ctx, store, sq.whereExpr, rt, md)
}

// trySecondaryIndexCompositePrefixFromWhere picks the first scannable
// VALUE index whose leading index cols all have equalities in the
// AND-chain WHERE, returning the longest leading prefix of equated
// cols. Trailing non-equated (or non-indexed) leaves remain post-
// filtered by the scan loop's evalPredicate.
func (c *EmbeddedConnection) trySecondaryIndexCompositePrefixFromWhere(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexCompositePrefix, bool) {
	if whereExpr == nil {
		return secondaryIndexCompositePrefix{}, false
	}
	leaves, lok := flattenAndPredicates(whereExpr.Expression())
	if !lok {
		return secondaryIndexCompositePrefix{}, false
	}
	// Collect bare col=literal equalities into a map.
	equalities := make(map[string]any, len(leaves))
	for _, leaf := range leaves {
		col, val, leafOk := extractColEqualsLiteral(ctx, c, leaf)
		if !leafOk {
			continue
		}
		equalities[strings.ToUpper(col)] = val
	}
	if len(equalities) == 0 {
		return secondaryIndexCompositePrefix{}, false
	}
	indexes := md.GetIndexesForRecordType(rt.Name)
	for _, idx := range indexes {
		if idx.Type != "" && idx.Type != "value" {
			continue
		}
		if !store.IsIndexScannable(idx.Name) {
			continue
		}
		idxCols := secondaryIndexColumns(idx)
		if len(idxCols) < 2 {
			// Single-col indexes are handled by the equality / range /
			// IN-list branches; skip.
			continue
		}
		prefixVals := make([]any, 0, len(idxCols))
		for _, col := range idxCols {
			v, found := equalities[strings.ToUpper(col)]
			if !found {
				break
			}
			fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
			if fd == nil || !functions.LiteralMatchesPKKind(v, fd.Kind()) {
				prefixVals = nil
				break
			}
			prefixVals = append(prefixVals, v)
		}
		if len(prefixVals) == 0 {
			continue
		}
		if len(prefixVals) == len(idxCols) {
			// Full equality — trySecondaryIndexFromWhere caught it.
			// Keep this branch non-overlapping.
			continue
		}
		return secondaryIndexCompositePrefix{indexName: idx.Name, prefixVals: prefixVals}, true
	}
	return secondaryIndexCompositePrefix{}, false
}

// secondaryIndexCompositePrefixScanCursor builds a tuple-prefix scan
// on a composite secondary index. Low = High = prefix tuple, inclusive
// both — FDB expands this to every key starting with the prefix (pack
// + 0xFF on the high side). The MapCursor adapts the indexed-record
// stream to the stored-record stream the scan loop expects.
func secondaryIndexCompositePrefixScanCursor(
	store *recordlayer.FDBRecordStore,
	cp secondaryIndexCompositePrefix,
	scanProps recordlayer.ScanProperties,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	// Reuse the equality range builder with a composite key tuple —
	// it produces exactly the inclusive-inclusive prefix range we need.
	keyTuple := secondaryIndexKeyTuple{values: cp.prefixVals}
	inner := store.ScanIndexRecords(cp.indexName, buildSecondaryIndexEqualityTupleRange(keyTuple), nil, scanProps)
	return recordlayer.MapCursor(inner, func(ir *recordlayer.FDBIndexedRecord) *recordlayer.FDBStoredRecord[proto.Message] {
		return ir.Record
	})
}
