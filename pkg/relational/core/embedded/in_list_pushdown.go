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

// IN-list pushdown: `WHERE pk_col IN (v1, v2, ..., vN)` on a single-column
// primary key decomposes into N point scans — one per list element —
// concatenated in declared list order. ORDER BY still re-sorts, so the
// emission order of the cursor is not user-visible.
//
// NOT IN bails: a NOT IN over a contiguous key range is the complement,
// which is all of the record type MINUS the listed points. That's a
// scan + post-filter, no narrower. The scan path handles it correctly
// via evalPredicate.
//
// Subquery IN bails: the subquery is materialised after the outer
// transaction opens (pre-evaluation in execSelectQuery) but decomposing
// its rows into a union of point scans is extra machinery for marginal
// benefit. MVP handles literal-list IN only.
//
// NULL in the list is ignored: `x IN (1, NULL, 2)` = `x = 1 OR x = NULL
// OR x = 2`, and `x = NULL` is UNKNOWN (never matches). So the pushdown
// can safely drop NULL elements — the remaining narrowing is correct
// and the scan's post-filter still applies the full WHERE via
// evalPredicate.
//
// Scope (single-col PK covered here; composite-PK IN below as
// tryPKCompositeInListFromWhere; secondary-index IN covered in the
// trySecondaryIndexInList* family further down):
//   - AND-chain WHERE only (flattenAndPredicates bails on OR).
//   - PK equality pushdown takes precedence: an AND with both
//     `id = 1` and `id IN (1,2)` would pick equality first (narrower).
//   - Type-mismatched literals bail to the scan so evalPredicate can
//     surface 42804, matching the other pushdown paths.
//
// Composite-PK form (tryPKCompositeInListFromWhere): `WHERE a = lit AND
// b IN (v1..vN)` on PK (a, b) — N point scans, each anchored at
// [a_lit, v_i]. Same leading-eq relaxation as the composite range
// extractor: the IN-col can sit anywhere in the PK; earlier cols
// must be equated; later cols are unconstrained (post-filtered).
//
// Secondary-index forms: single-col (trySecondaryIndexInListFromWhere)
// and composite (trySecondaryIndexCompositeInListFromWhere). Same
// decomposition, but each sub-scan uses the covering-index cursor
// when canCoverIndex holds.

// extractColInList returns (colName, values, true) when the leaf is
// exactly `col IN (lit1, lit2, ..., litN)` with every element evaluating
// to a non-NULL literal. NULL elements are silently dropped (see file
// header). The returned values preserve the declared order modulo the
// NULL drops.
//
// NOT IN / subquery IN / non-bare-column LHS / empty list all return
// false so the caller falls back to scan.
func extractColInList(
	ctx context.Context,
	c *EmbeddedConnection,
	expr antlrgen.IExpressionContext,
) (string, []any, bool) {
	pred, ok := expr.(*antlrgen.PredicatedExpressionContext)
	if !ok {
		return "", nil, false
	}
	if pred.Predicate() == nil {
		return "", nil, false
	}
	inPred, ok := pred.Predicate().(*antlrgen.InPredicateContext)
	if !ok {
		return "", nil, false
	}
	if inPred.NOT() != nil {
		return "", nil, false
	}

	colName, ok := extractColumnRef(pred.ExpressionAtom())
	if !ok {
		return "", nil, false
	}

	if inPred.InList().QueryExpressionBody() != nil {
		// IN-subquery is rejected at runtime by evalInPredicateTri /
		// evalPredicateOnMapTri; pushdown bails so the rejection
		// surfaces as the user-visible error.
		return "", nil, false
	}

	// The ANTLR rule `inList : '(' (queryExpressionBody | expressions) ')'
	//                        | preparedStatementParameter
	//                        | fullColumnName ;`
	// means Expressions() is nil for `IN ?` / `IN :param` / `IN someCol`.
	// A bare AllExpression() on nil panics — bail before touching it.
	exprsCtx := inPred.InList().Expressions()
	if exprsCtx == nil {
		return "", nil, false
	}
	exprs := exprsCtx.AllExpression()
	if len(exprs) == 0 {
		return "", nil, false
	}

	vals := make([]any, 0, len(exprs))
	for _, e := range exprs {
		pe, ok := e.(*antlrgen.PredicatedExpressionContext)
		if !ok {
			return "", nil, false
		}
		if pe.Predicate() != nil {
			// Nested predicate inside an IN element isn't a literal.
			return "", nil, false
		}
		// Evaluate the element without a row context. A non-constant
		// expression (one that references a column) returns err; NULL
		// returns (nil, nil) — both are distinguished here so NULL
		// elements drop silently while non-constants bail entirely.
		// evalConstantAtom collapses both cases, so we can't use it.
		v, err := evalExprAtom(ctx, c, nil, pe.ExpressionAtom())
		if err != nil {
			return "", nil, false // non-constant — needs a row context
		}
		if v == nil {
			continue // NULL element — drop (x = NULL is UNKNOWN)
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		// IN-list of only NULLs: predicate is always UNKNOWN →
		// zero rows match. Bail to scan; the post-filter rejects
		// every row correctly.
		return "", nil, false
	}
	// Java-aligned SQL IN semantics: the list is a SET — duplicate
	// values are equivalent to a single occurrence. Dedupe to prevent
	// the IN-list pushdown from emitting the same record multiple
	// times (point-scanning the same key twice). Preserve first-seen
	// order so tests that rely on declared ordering still pass.
	return colName, dedupeAny(vals), true
}

// dedupeAny preserves first-seen order while dropping equal repeats.
// Used by extractColInList to enforce IN's set semantics (see Java's
// PredicateSimplification which does the same). O(n²) is fine for
// the small IN lists we see (point-scans cost far more than the
// compare loop).
func dedupeAny(in []any) []any {
	if len(in) <= 1 {
		return in
	}
	out := make([]any, 0, len(in))
	for _, v := range in {
		dup := false
		for _, o := range out {
			if valuesEqual(v, o) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

// tryPKInListFromWhere reports whether the WHERE is an AND chain
// containing `pk_col IN (...)` on a single-column primary key. Returns
// the PK values to point-scan on success. PK equality pushdown is
// tried first at every call site — if we reach this function, no
// single-key equality was found.
func (c *EmbeddedConnection) tryPKInListFromWhere(
	ctx context.Context,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
) ([]any, bool) {
	if whereExpr == nil {
		return nil, false
	}

	// Single-column PK only.
	pkCols := extractPKUserFields(rt.PrimaryKey)
	if len(pkCols) != 1 {
		return nil, false
	}
	pkCol := pkCols[0]
	fd := rt.Descriptor.Fields().ByName(protoreflect.Name(pkCol))
	if fd == nil {
		return nil, false
	}

	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return nil, false
	}

	// Scan the AND leaves for a `pk_col IN (...)` predicate. First
	// hit wins — multiple IN leaves on the same column are rare and
	// the scan's evalPredicate re-applies the full WHERE so the
	// narrower list is still correct.
	for _, leaf := range leaves {
		col, vals, inOk := extractColInList(ctx, c, leaf)
		if !inOk {
			continue
		}
		if !strings.EqualFold(col, pkCol) {
			continue
		}
		// Type-check every element against the PK column kind. A
		// single mismatch bails — the scan path's evalPredicate will
		// surface 42804 correctly. Don't silently drop mismatched
		// elements (that would reduce the narrowing without the
		// post-filter knowing).
		for _, v := range vals {
			if !functions.LiteralMatchesPKKind(v, fd.Kind()) {
				return nil, false
			}
		}
		return vals, true
	}
	return nil, false
}

// tryPKInListPushdown is the SELECT-gated variant of
// tryPKInListFromWhere. Shape gates match the other PK pushdown
// helpers.
func (c *EmbeddedConnection) tryPKInListPushdown(
	ctx context.Context,
	sq *selectQuery,
	rt *recordlayer.RecordType,
) ([]any, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return nil, false
	}
	if sq.havingExpr != nil {
		return nil, false
	}
	return c.tryPKInListFromWhere(ctx, sq.whereExpr, rt)
}

// pkCompositeInList describes a composite-PK IN-list pushdown:
// equalities on the first len(prefixVals) PK cols + `IN (...)` on
// the next PK col. Any PK cols after the IN-col are unconstrained
// at the pushdown level (post-filtered by evalPredicate on each
// scanned record, same pattern as composite range).
type pkCompositeInList struct {
	prefixVals []any
	inValues   []any
}

// tryPKCompositeInListFromWhere looks for the composite-PK equivalent
// of tryPKInListFromWhere: equalities on every PK col before the
// IN-col + one IN-list on a later PK col. Same relaxation as
// tryPKCompositeRangeFromWhere (dayshift-43 dd97a817) — the IN col
// can sit anywhere, not just at the end; PK cols after it are
// unconstrained and post-filtered.
func (c *EmbeddedConnection) tryPKCompositeInListFromWhere(
	ctx context.Context,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
) (pkCompositeInList, bool) {
	if whereExpr == nil {
		return pkCompositeInList{}, false
	}
	pkCols := extractPKUserFields(rt.PrimaryKey)
	if len(pkCols) < 2 {
		// Single-col PK goes through tryPKInListFromWhere; no PK can't
		// push down at all.
		return pkCompositeInList{}, false
	}

	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return pkCompositeInList{}, false
	}

	// Walk AND leaves once, collecting equalities and at-most-one
	// IN-list per column (later leaves for the same column overwrite
	// earlier ones — last-write-wins, same as the range extractors).
	isPKCol := func(col string) bool {
		for _, pkc := range pkCols {
			if strings.EqualFold(pkc, col) {
				return true
			}
		}
		return false
	}
	equalities := make(map[string]any, len(pkCols))
	inByCol := make(map[string][]any, 1)
	for _, leaf := range leaves {
		if col, vals, inOk := extractColInList(ctx, c, leaf); inOk {
			if !isPKCol(col) {
				continue
			}
			inByCol[strings.ToUpper(col)] = vals
			continue
		}
		op, col, val, ok := extractColOpLiteral(ctx, c, leaf)
		if !ok || op != "=" {
			continue
		}
		if !isPKCol(col) {
			continue
		}
		equalities[strings.ToUpper(col)] = val
	}
	if len(inByCol) == 0 {
		return pkCompositeInList{}, false
	}

	// Find the first PK col (in declared order) carrying an IN-list.
	// Every PK col before it must have an equality.
	inK := -1
	var inVals []any
	for i, col := range pkCols {
		if vals, has := inByCol[strings.ToUpper(col)]; has {
			inK = i
			inVals = vals
			break
		}
	}
	if inK == -1 {
		return pkCompositeInList{}, false
	}
	prefixVals := make([]any, inK)
	for i := 0; i < inK; i++ {
		col := pkCols[i]
		val, has := equalities[strings.ToUpper(col)]
		if !has {
			return pkCompositeInList{}, false
		}
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
		if fd == nil || !functions.LiteralMatchesPKKind(val, fd.Kind()) {
			return pkCompositeInList{}, false
		}
		prefixVals[i] = val
	}
	// Type-check the IN col values against its field kind.
	inCol := pkCols[inK]
	fd := rt.Descriptor.Fields().ByName(protoreflect.Name(inCol))
	if fd == nil {
		return pkCompositeInList{}, false
	}
	for _, v := range inVals {
		if !functions.LiteralMatchesPKKind(v, fd.Kind()) {
			return pkCompositeInList{}, false
		}
	}
	return pkCompositeInList{
		prefixVals: prefixVals,
		inValues:   inVals,
	}, true
}

func (c *EmbeddedConnection) tryPKCompositeInListPushdown(
	ctx context.Context,
	sq *selectQuery,
	rt *recordlayer.RecordType,
) (pkCompositeInList, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return pkCompositeInList{}, false
	}
	if sq.havingExpr != nil {
		return pkCompositeInList{}, false
	}
	return c.tryPKCompositeInListFromWhere(ctx, sq.whereExpr, rt)
}

// pkCompositeInListCursor iterates N sub-scans, one per IN value,
// each narrowed to the prefix equalities || one IN value. PK cols
// after the IN-col are unconstrained at the scan level — records
// with the right prefix + IN-col value flow through and evalPredicate
// applies the residual AND leaves.
type pkCompositeInListCursor struct {
	store   *recordlayer.FDBRecordStore
	rt      *recordlayer.RecordType
	cil     pkCompositeInList
	idx     int
	current recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	closed  bool
}

func (c *pkCompositeInListCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
		}
		if c.current != nil {
			result, err := c.current.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
			}
			if result.HasNext() {
				return result, nil
			}
			_ = c.current.Close()
			c.current = nil
		}
		if c.idx >= len(c.cil.inValues) {
			return recordlayer.NewResultNoNext[*recordlayer.FDBStoredRecord[proto.Message]](
				recordlayer.SourceExhausted,
				&recordlayer.EndContinuation{},
			), nil
		}
		val := c.cil.inValues[c.idx]
		c.idx++
		// Build a pkCompositeRange with the prefix + inclusive-inclusive
		// single-value bounds on the IN-col; trailing PK cols are
		// unconstrained (lastBounds.hasLow = hasHigh = true on the same
		// point collapses the range to the exact tuple at the
		// prefix || in_val anchor, with trailing-cols unconstrained).
		cr := pkCompositeRange{
			prefixVals: c.cil.prefixVals,
			lastBounds: pkRangeBounds{
				hasLow:        true,
				low:           val,
				lowInclusive:  true,
				hasHigh:       true,
				high:          val,
				highInclusive: true,
			},
		}
		// IN-list sub-scans always run in forward (list-order emission);
		// direction is not observable because the outer chain doesn't
		// promise any natural order across sub-scans.
		c.current = pkPushdownCompositeRangeScanCursor(c.store, c.rt, cr, recordlayer.ForwardScan())
	}
}

func (c *pkCompositeInListCursor) Close() error {
	c.closed = true
	if c.current != nil {
		return c.current.Close()
	}
	return nil
}

func (c *pkCompositeInListCursor) IsClosed() bool { return c.closed }

// pkCompositeInListScanCursor wraps the composite IN-list decomposition.
func pkCompositeInListScanCursor(
	store *recordlayer.FDBRecordStore,
	rt *recordlayer.RecordType,
	cil pkCompositeInList,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	if len(cil.inValues) == 0 {
		return recordlayer.Empty[*recordlayer.FDBStoredRecord[proto.Message]]()
	}
	return &pkCompositeInListCursor{
		store: store,
		rt:    rt,
		cil:   cil,
	}
}

// secondaryIndexCompositeInList is the composite-secondary-index
// counterpart of pkCompositeInList: leading equalities on N-1 cols
// + IN-list on the remaining col of a composite VALUE index.
type secondaryIndexCompositeInList struct {
	indexName  string
	prefixVals []any
	inValues   []any
}

// trySecondaryIndexCompositeInListFromWhere finds a composite VALUE
// index where the first index col carrying an IN-list is preceded
// by equalities on every earlier index col. Same shape rules as
// tryPKCompositeInListFromWhere.
func (c *EmbeddedConnection) trySecondaryIndexCompositeInListFromWhere(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexCompositeInList, bool) {
	if whereExpr == nil {
		return secondaryIndexCompositeInList{}, false
	}
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return secondaryIndexCompositeInList{}, false
	}

	equalities := make(map[string]any, len(leaves))
	inByCol := make(map[string][]any, 1)
	for _, leaf := range leaves {
		if col, vals, inOk := extractColInList(ctx, c, leaf); inOk {
			inByCol[strings.ToUpper(col)] = vals
			continue
		}
		op, col, val, ok := extractColOpLiteral(ctx, c, leaf)
		if !ok || op != "=" {
			continue
		}
		equalities[strings.ToUpper(col)] = val
	}
	if len(inByCol) == 0 {
		return secondaryIndexCompositeInList{}, false
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
			continue
		}
		// Find first index col with IN-list.
		inK := -1
		var inVals []any
		for i, col := range idxCols {
			if vals, has := inByCol[strings.ToUpper(col)]; has {
				inK = i
				inVals = vals
				break
			}
		}
		if inK == -1 {
			continue
		}
		// Every index col before inK must have an equality on the
		// same col. Type-check too.
		prefixVals := make([]any, inK)
		ok := true
		for i := 0; i < inK; i++ {
			col := idxCols[i]
			val, has := equalities[strings.ToUpper(col)]
			if !has {
				ok = false
				break
			}
			fd := rt.Descriptor.Fields().ByName(protoreflect.Name(col))
			if fd == nil || !functions.LiteralMatchesPKKind(val, fd.Kind()) {
				ok = false
				break
			}
			prefixVals[i] = val
		}
		if !ok {
			continue
		}
		inFd := rt.Descriptor.Fields().ByName(protoreflect.Name(idxCols[inK]))
		if inFd == nil {
			continue
		}
		typeOk := true
		for _, v := range inVals {
			if !functions.LiteralMatchesPKKind(v, inFd.Kind()) {
				typeOk = false
				break
			}
		}
		if !typeOk {
			continue
		}
		return secondaryIndexCompositeInList{
			indexName:  idx.Name,
			prefixVals: prefixVals,
			inValues:   inVals,
		}, true
	}
	return secondaryIndexCompositeInList{}, false
}

func (c *EmbeddedConnection) trySecondaryIndexCompositeInListPushdown(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexCompositeInList, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return secondaryIndexCompositeInList{}, false
	}
	if sq.havingExpr != nil {
		return secondaryIndexCompositeInList{}, false
	}
	return c.trySecondaryIndexCompositeInListFromWhere(ctx, store, sq.whereExpr, rt, md)
}

// secondaryIndexCompositeInListCursor runs N sub-scans, each using
// the composite-range cursor with lastBounds collapsed to (in_val,
// in_val) inclusive-inclusive at the prefix anchor. Covering-index
// dispatch applies when canCoverIndex holds.
type secondaryIndexCompositeInListCursor struct {
	store       *recordlayer.FDBRecordStore
	cil         secondaryIndexCompositeInList
	idx         int
	coveringIdx *recordlayer.Index
	coveringRT  *recordlayer.RecordType
	current     recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	closed      bool
}

func (c *secondaryIndexCompositeInListCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
		}
		if c.current != nil {
			result, err := c.current.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
			}
			if result.HasNext() {
				return result, nil
			}
			_ = c.current.Close()
			c.current = nil
		}
		if c.idx >= len(c.cil.inValues) {
			return recordlayer.NewResultNoNext[*recordlayer.FDBStoredRecord[proto.Message]](
				recordlayer.SourceExhausted,
				&recordlayer.EndContinuation{},
			), nil
		}
		val := c.cil.inValues[c.idx]
		c.idx++
		cr := secondaryIndexCompositeRange{
			indexName:  c.cil.indexName,
			prefixVals: c.cil.prefixVals,
			lastBounds: pkRangeBounds{
				hasLow:        true,
				low:           val,
				lowInclusive:  true,
				hasHigh:       true,
				high:          val,
				highInclusive: true,
			},
		}
		if c.coveringIdx != nil {
			c.current = coveringIndexRangeScanCursor(c.store, c.coveringRT, c.coveringIdx,
				buildSecondaryIndexCompositeRangeTupleRange(cr), recordlayer.ForwardScan())
		} else {
			c.current = secondaryIndexCompositeRangeScanCursor(c.store, cr, recordlayer.ForwardScan())
		}
	}
}

func (c *secondaryIndexCompositeInListCursor) Close() error {
	c.closed = true
	if c.current != nil {
		return c.current.Close()
	}
	return nil
}

func (c *secondaryIndexCompositeInListCursor) IsClosed() bool { return c.closed }

func secondaryIndexCompositeInListScanCursor(
	store *recordlayer.FDBRecordStore,
	cil secondaryIndexCompositeInList,
	coveringRT *recordlayer.RecordType,
	coveringIdx *recordlayer.Index,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	if len(cil.inValues) == 0 {
		return recordlayer.Empty[*recordlayer.FDBStoredRecord[proto.Message]]()
	}
	return &secondaryIndexCompositeInListCursor{
		store:       store,
		cil:         cil,
		coveringIdx: coveringIdx,
		coveringRT:  coveringRT,
	}
}

// secondaryIndexInList describes an IN-list pushdown on a single-column
// VALUE index: the index name + the list of per-element equality values
// to point-scan on that index.
type secondaryIndexInList struct {
	indexName string
	values    []any
}

// trySecondaryIndexInListFromWhere looks for a single-column VALUE
// index whose column has an `IN (...)` predicate in the AND chain.
// Pure-equality pushdown on the indexed column is tried first at
// every call site (trySecondaryIndexFromWhere), so the IN-list
// extractor only needs to find its own shape — equality leaves on
// the indexed column can't reach here (they'd have matched the
// equality path and short-circuited).
//
// Only single-column VALUE indexes — composite-index IN would need
// to combine one IN leaf with other-column equalities to form the
// tuple prefix, which is a more involved variation we can add in a
// follow-up. Non-scannable indexes are skipped in the iteration,
// matching the dayshift-43 fix in trySecondaryIndexRangeFromWhere.
func (c *EmbeddedConnection) trySecondaryIndexInListFromWhere(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	whereExpr antlrgen.IWhereExprContext,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexInList, bool) {
	if whereExpr == nil {
		return secondaryIndexInList{}, false
	}
	leaves, ok := flattenAndPredicates(whereExpr.Expression())
	if !ok {
		return secondaryIndexInList{}, false
	}

	// Gather IN-list leaves by (uppercase) column.
	inByCol := make(map[string][]any, len(leaves))
	for _, leaf := range leaves {
		col, vals, inOk := extractColInList(ctx, c, leaf)
		if !inOk {
			continue
		}
		inByCol[strings.ToUpper(col)] = vals
	}
	if len(inByCol) == 0 {
		return secondaryIndexInList{}, false
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
		if len(idxCols) != 1 {
			continue
		}
		vals, found := inByCol[strings.ToUpper(idxCols[0])]
		if !found {
			continue
		}
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(idxCols[0]))
		if fd == nil {
			continue
		}
		// Every list element must be type-compatible with the index
		// column; a single mismatch bails so the scan's evalPredicate
		// surfaces 42804 per the cross-type rule.
		allOk := true
		for _, v := range vals {
			if !functions.LiteralMatchesPKKind(v, fd.Kind()) {
				allOk = false
				break
			}
		}
		if !allOk {
			continue
		}
		return secondaryIndexInList{indexName: idx.Name, values: vals}, true
	}
	return secondaryIndexInList{}, false
}

// trySecondaryIndexInListPushdown is the SELECT-gated variant.
func (c *EmbeddedConnection) trySecondaryIndexInListPushdown(
	ctx context.Context,
	store *recordlayer.FDBRecordStore,
	sq *selectQuery,
	rt *recordlayer.RecordType,
	md *recordlayer.RecordMetaData,
) (secondaryIndexInList, bool) {
	if sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 {
		return secondaryIndexInList{}, false
	}
	if sq.havingExpr != nil {
		return secondaryIndexInList{}, false
	}
	return c.trySecondaryIndexInListFromWhere(ctx, store, sq.whereExpr, rt, md)
}

// secondaryIndexInListCursor runs a sequence of single-value index
// scans, one per IN-list element, yielding records in declared list
// order. Same lazy chaining as pkInListCursor. When the SELECT's
// column set is covered by (index cols, PK cols), each sub-scan uses
// the covering cursor (one FDB round-trip per row) instead of the
// standard ScanIndexRecords path (two round-trips per row).
type secondaryIndexInListCursor struct {
	store     *recordlayer.FDBRecordStore
	indexName string
	values    []any
	idx       int
	// covering, if non-nil, is the resolved Index + RecordType used to
	// build covering sub-scans. When nil, fall back to the plain
	// secondaryIndexPushdownCursor (which fetches records).
	coveringIdx *recordlayer.Index
	coveringRT  *recordlayer.RecordType
	current     recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	closed      bool
}

func (c *secondaryIndexInListCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
		}
		if c.current != nil {
			result, err := c.current.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
			}
			if result.HasNext() {
				return result, nil
			}
			_ = c.current.Close()
			c.current = nil
		}
		if c.idx >= len(c.values) {
			return recordlayer.NewResultNoNext[*recordlayer.FDBStoredRecord[proto.Message]](
				recordlayer.SourceExhausted,
				&recordlayer.EndContinuation{},
			), nil
		}
		val := c.values[c.idx]
		c.idx++
		if c.coveringIdx != nil {
			c.current = coveringIndexRangeScanCursor(c.store, c.coveringRT, c.coveringIdx,
				buildSecondaryIndexEqualityTupleRange(val), recordlayer.ForwardScan())
		} else {
			c.current = secondaryIndexPushdownCursor(c.store, c.indexName, val, recordlayer.ForwardScan())
		}
	}
}

func (c *secondaryIndexInListCursor) Close() error {
	c.closed = true
	if c.current != nil {
		return c.current.Close()
	}
	return nil
}

func (c *secondaryIndexInListCursor) IsClosed() bool {
	return c.closed
}

// secondaryIndexInListScanCursor wraps N sequential single-value index
// scans as one cursor. When coveringRT + coveringIdx are non-nil, each
// sub-scan avoids the by-PK record fetch by synthesising the record
// from the IndexEntry directly (same covering-index mechanism as the
// equality / range paths). Caller is responsible for checking
// canCoverIndex before setting these.
func secondaryIndexInListScanCursor(
	store *recordlayer.FDBRecordStore,
	sil secondaryIndexInList,
	coveringRT *recordlayer.RecordType,
	coveringIdx *recordlayer.Index,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	if len(sil.values) == 0 {
		return recordlayer.Empty[*recordlayer.FDBStoredRecord[proto.Message]]()
	}
	return &secondaryIndexInListCursor{
		store:       store,
		indexName:   sil.indexName,
		values:      sil.values,
		coveringIdx: coveringIdx,
		coveringRT:  coveringRT,
	}
}

// pkInListCursor runs a sequence of point scans — one per IN-list
// value — lazily: the next sub-scan starts only after the previous one
// exhausts. Output order is declared-list order (ORDER BY re-sorts if
// the SELECT asked for one).
//
// Each sub-scan uses pkPushdownScanCursor which yields at most one
// record (PK equality). So the total round-trip count is N, same as
// `N separate SELECT … WHERE pk = v_i` queries but served from one
// logical cursor and one FDB transaction.
type pkInListCursor struct {
	store   *recordlayer.FDBRecordStore
	rt      *recordlayer.RecordType
	pkVals  []any
	idx     int
	current recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]]
	closed  bool
}

func (c *pkInListCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]], error) {
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
		}
		if c.current != nil {
			result, err := c.current.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]{}, err
			}
			if result.HasNext() {
				return result, nil
			}
			_ = c.current.Close()
			c.current = nil
		}
		if c.idx >= len(c.pkVals) {
			return recordlayer.NewResultNoNext[*recordlayer.FDBStoredRecord[proto.Message]](
				recordlayer.SourceExhausted,
				&recordlayer.EndContinuation{},
			), nil
		}
		c.current = pkPushdownScanCursor(c.store, c.rt, []any{c.pkVals[c.idx]}, recordlayer.ForwardScan())
		c.idx++
	}
}

func (c *pkInListCursor) Close() error {
	c.closed = true
	if c.current != nil {
		return c.current.Close()
	}
	return nil
}

func (c *pkInListCursor) IsClosed() bool {
	return c.closed
}

// pkPushdownInListScanCursor wraps the N point-scan decomposition as a
// single RecordCursor. Empty pkVals yields an empty cursor (shouldn't
// happen — tryPKInListFromWhere returns false for empty lists).
func pkPushdownInListScanCursor(
	store *recordlayer.FDBRecordStore,
	rt *recordlayer.RecordType,
	pkVals []any,
) recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]] {
	if len(pkVals) == 0 {
		return recordlayer.Empty[*recordlayer.FDBStoredRecord[proto.Message]]()
	}
	return &pkInListCursor{
		store:  store,
		rt:     rt,
		pkVals: pkVals,
	}
}
