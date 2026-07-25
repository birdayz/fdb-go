package recordlayer

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// multidimensionalIndexMaintainer maintains a MULTIDIMENSIONAL index using a Hilbert R-tree.
// Each unique prefix gets its own R-tree. Items are stored with dimensional coordinates
// as the Point and remaining key components as the key suffix.
// Matches Java's MultidimensionalIndexMaintainer.
type multidimensionalIndexMaintainer struct {
	standardIndexMaintainer
	rTreeConfig RTreeConfig
}

func newMultidimensionalIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.WritableTransaction,
	store indexStoreContext,
	numDimensions int,
) *multidimensionalIndexMaintainer {
	config := parseRTreeConfig(index, numDimensions)
	return &multidimensionalIndexMaintainer{
		standardIndexMaintainer: *newStandardIndexMaintainer(index, indexSubspace, tx, store),
		rTreeConfig:             config,
	}
}

// R-tree index option keys for configuring the Hilbert R-tree.
const (
	IndexOptionRTreeMaxM   = "rtreeMaximumM"
	IndexOptionRTreeMinM   = "rtreeMinimumM"
	IndexOptionRTreeSplitS = "rtreeSplitS"

	// IndexOptionRTreeStorage controls the node storage strategy.
	// Matches Java's IndexOptions.RTREE_STORAGE.
	IndexOptionRTreeStorage = "rtreeStorage"

	// IndexOptionRTreeStoreHilbertValues controls whether Hilbert values are stored.
	// Matches Java's IndexOptions.RTREE_STORE_HILBERT_VALUES.
	IndexOptionRTreeStoreHilbertValues = "rtreeStoreHilbertValues"

	// IndexOptionRTreeUseNodeSlotIndex controls whether a node slot index is maintained.
	// Matches Java's IndexOptions.RTREE_USE_NODE_SLOT_INDEX.
	IndexOptionRTreeUseNodeSlotIndex = "rtreeUseNodeSlotIndex"
)

// parseRTreeConfig reads R-tree configuration from index options.
// Supports IndexOptionRTreeMaxM, IndexOptionRTreeMinM, IndexOptionRTreeSplitS.
func parseRTreeConfig(index *Index, numDimensions int) RTreeConfig {
	config := DefaultRTreeConfig(numDimensions)
	if v, ok := index.Options[IndexOptionRTreeMaxM]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.MaxM = n
		}
	}
	if v, ok := index.Options[IndexOptionRTreeMinM]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.MinM = n
		}
	}
	if v, ok := index.Options[IndexOptionRTreeSplitS]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			config.SplitS = n
		}
	}
	if v, ok := index.Options["rtreeStoreHilbertValues"]; ok {
		if v == "false" {
			config.StoreHilbertValues = false
		}
	}
	// "rtreeStorage" = "BY_SLOT" is not supported in Go; BY_NODE is the default and recommended.
	return config
}

// getDimensionsExpression extracts the DimensionsKeyExpression from the index.
func (m *multidimensionalIndexMaintainer) getDimensionsExpression() *DimensionsKeyExpression {
	return extractDimensionsExpression(m.index.RootExpression)
}

// extractDimensionsExpression finds the DimensionsKeyExpression in an expression tree.
// Handles KeyWithValueExpression wrapping and CompositeKeyExpression (ThenKeyExpression) chains.
func extractDimensionsExpression(expr KeyExpression) *DimensionsKeyExpression {
	switch e := expr.(type) {
	case *DimensionsKeyExpression:
		return e
	case *KeyWithValueExpression:
		return extractDimensionsExpression(e.innerKey)
	case *CompositeKeyExpression:
		if len(e.expressions) > 0 {
			return extractDimensionsExpression(e.expressions[0])
		}
	}
	return nil
}

// Update handles insert/delete/update for the MULTIDIMENSIONAL index.
// Acquires write lock to serialize R-tree mutations.
// Matches Java's MultidimensionalIndexMaintainer.updateIndex() which acquires
// a write lock via context.doWithWriteLock(LockIdentifier).
func (m *multidimensionalIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	lockKey := string(m.indexSubspace.Bytes())
	m.store.AcquireWriteLock(lockKey)
	defer m.store.ReleaseWriteLock(lockKey)
	dimExpr := m.getDimensionsExpression()
	if dimExpr == nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: root expression must be DimensionsKeyExpression", m.index.Name)
	}

	var oldEntries, newEntries []indexEntry

	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}
	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	// Skip entries that are identical between old and new — avoids unnecessary
	// R-tree delete+insert when coordinates/value haven't changed.
	if len(oldEntries) > 0 && len(newEntries) > 0 {
		var err error
		oldEntries, newEntries, err = removeCommonEntries(m.index, oldEntries, newEntries)
		if err != nil {
			return err
		}
	}

	// Process deletes.
	for _, entry := range oldEntries {
		if err := m.deleteEntry(dimExpr, entry); err != nil {
			return err
		}
	}

	// Process inserts.
	for _, entry := range newEntries {
		if err := m.insertEntry(dimExpr, entry); err != nil {
			return err
		}
	}

	return nil
}

// insertEntry inserts a single index entry into the appropriate R-tree.
func (m *multidimensionalIndexMaintainer) insertEntry(dimExpr *DimensionsKeyExpression, entry indexEntry) error {
	prefix, dims, suffix := dimExpr.SplitIndexEntry(entry.key)

	// Build the R-tree subspace (per-prefix).
	rtSubspace := m.indexSubspace
	if len(prefix) > 0 {
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}

	// Validate dimensional coordinates are int64.
	for i, d := range dims {
		if _, ok := asInt64(d); !ok {
			return fmt.Errorf("MULTIDIMENSIONAL index %q: dimension %d must be int64, got %T", m.index.Name, i, d)
		}
	}
	point := Point{Coordinates: dims}

	// Build key suffix: remaining index columns + trimmed PK.
	trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
	if err != nil {
		return err
	}
	keySuffix := make(tuple.Tuple, 0, len(suffix)+len(trimmedPK))
	keySuffix = append(keySuffix, suffix...)
	keySuffix = append(keySuffix, trimmedPK...)

	// Value from the index entry.
	value := entry.value
	if value == nil {
		value = tuple.Tuple{}
	}

	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)
	}
	return rtree.InsertOrUpdate(m.tx, point, keySuffix, value)
}

// deleteEntry removes a single index entry from the appropriate R-tree.
func (m *multidimensionalIndexMaintainer) deleteEntry(dimExpr *DimensionsKeyExpression, entry indexEntry) error {
	prefix, dims, suffix := dimExpr.SplitIndexEntry(entry.key)

	rtSubspace := m.indexSubspace
	if len(prefix) > 0 {
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}

	point := Point{Coordinates: dims}

	trimmedPK, err := m.index.TrimPrimaryKey(entry.primaryKey)
	if err != nil {
		return err
	}
	keySuffix := make(tuple.Tuple, 0, len(suffix)+len(trimmedPK))
	keySuffix = append(keySuffix, suffix...)
	keySuffix = append(keySuffix, trimmedPK...)

	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)
	}
	return rtree.Delete(m.tx, point, keySuffix)
}

// UpdateWhileWriteOnly handles updates during WRITE_ONLY state.
// MULTIDIMENSIONAL is idempotent (insertOrUpdate is upsert-safe).
func (m *multidimensionalIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan scans the R-tree for items matching an MBR predicate.
// The scanRange is used for prefix filtering (first PrefixSize elements scope the R-tree subspace)
// and for extracting spatial bounds for MBR-based subtree pruning.
// When PrefixSize > 0 but no specific prefix is provided in scanRange, enumerates all
// distinct prefixes (prefix skip-scan).
// Supports proto-wrapped continuation tokens (MultidimensionalIndexScanContinuation) and row limits.
// For basic scans without spatial predicates, returns all items in Hilbert order.
func (m *multidimensionalIndexMaintainer) Scan(
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	dimExpr := m.getDimensionsExpression()
	if dimExpr == nil {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("MULTIDIMENSIONAL index %q: root expression must be DimensionsKeyExpression", m.index.Name),
		}
	}

	// 1. Check if prefix skip-scan is needed: PrefixSize > 0 but no prefix provided in scanRange.
	if dimExpr.PrefixSize > 0 && (scanRange.Low == nil || len(scanRange.Low) < dimExpr.PrefixSize) {
		// The skip-scan's own continuation is a FlatMapContinuation pairing the
		// prefix position (outer) with the per-prefix R-tree position (inner) —
		// see prefixSkipScanCursor's doc comment. Parse it once, up front.
		var resumePrefixBytes, resumeInnerBytes []byte
		if len(continuation) > 0 {
			var flatMapCont gen.FlatMapContinuation
			if err := flatMapCont.UnmarshalVT(continuation); err != nil {
				return &errorCursor[*IndexEntry]{
					err: fmt.Errorf("MULTIDIMENSIONAL index %q: invalid prefix skip-scan continuation: %w", m.index.Name, err),
				}
			}
			resumePrefixBytes = flatMapCont.OuterContinuation
			resumeInnerBytes = flatMapCont.InnerContinuation
		}
		cursor := &prefixSkipScanCursor{
			m:                 m,
			dimExpr:           dimExpr,
			scanRange:         scanRange,
			scanProperties:    scanProperties,
			resumePrefixBytes: resumePrefixBytes,
			resumeInnerBytes:  resumeInnerBytes,
		}
		// Row limiting happens exactly once, OUTSIDE the cross-prefix cursor —
		// matching Java's innerScanProperties = scanProperties.with(clearSkipAndLimit)
		// (:124) plus the .skipThenLimit() applied to the whole flattened result
		// (:179). LimitRowsCursor stops pulling once N rows are delivered and
		// reuses THIS cursor's own (already prefix-aware) continuation for the
		// Nth row as the ReturnLimitReached continuation — the row-limit
		// boundary needs no encoding of its own, so it can never regress to an
		// unresumable placeholder.
		return LimitRowsCursor[*IndexEntry](cursor, scanProperties.ExecuteProperties.ReturnedRowLimit)
	}

	// 2. Extract prefix from scanRange to scope the R-tree subspace.
	var prefix tuple.Tuple
	rtSubspace := m.indexSubspace
	if dimExpr.PrefixSize > 0 && scanRange.Low != nil && len(scanRange.Low) >= dimExpr.PrefixSize {
		prefix = scanRange.Low[:dimExpr.PrefixSize]
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}

	// 3. Extract spatial bounds from scanRange for MBR-based subtree pruning.
	mbrPredicate := m.buildMBRPredicate(dimExpr, scanRange)

	// 4. Parse continuation token.
	// Java wraps all MULTIDIMENSIONAL continuations in FlatMapContinuation (from flatMapPipelined).
	// Try FlatMapContinuation first; fall back to raw MultidimensionalIndexScanContinuation
	// for backward compatibility with old Go-produced tokens.
	var lastHV *big.Int
	var lastKey tuple.Tuple
	var outerContinuation []byte
	if len(continuation) > 0 {
		var parsed bool
		var flatMapCont gen.FlatMapContinuation
		if err := flatMapCont.UnmarshalVT(continuation); err == nil && flatMapCont.InnerContinuation != nil {
			// Java-compatible FlatMapContinuation wrapper.
			var cont gen.MultidimensionalIndexScanContinuation
			if err := cont.UnmarshalVT(flatMapCont.InnerContinuation); err == nil {
				if cont.LastHilbertValue != nil {
					lastHV = new(big.Int).SetBytes(cont.LastHilbertValue)
				}
				if cont.LastKey != nil {
					var tupErr error
					lastKey, tupErr = fastUnpack(cont.LastKey)
					if tupErr != nil {
						return &errorCursor[*IndexEntry]{
							err: fmt.Errorf("MULTIDIMENSIONAL index %q: invalid continuation lastKey: %w", m.index.Name, tupErr),
						}
					}
				}
				outerContinuation = flatMapCont.OuterContinuation
				parsed = true
			}
		}
		if !parsed {
			// Fallback: try raw MultidimensionalIndexScanContinuation (old Go format).
			var cont gen.MultidimensionalIndexScanContinuation
			if err := cont.UnmarshalVT(continuation); err != nil {
				return &errorCursor[*IndexEntry]{
					err: fmt.Errorf("MULTIDIMENSIONAL index %q: invalid continuation: %w", m.index.Name, err),
				}
			}
			if cont.LastHilbertValue != nil {
				lastHV = new(big.Int).SetBytes(cont.LastHilbertValue)
			}
			if cont.LastKey != nil {
				var err error
				lastKey, err = fastUnpack(cont.LastKey)
				if err != nil {
					return &errorCursor[*IndexEntry]{
						err: fmt.Errorf("MULTIDIMENSIONAL index %q: invalid continuation lastKey: %w", m.index.Name, err),
					}
				}
			}
		}
	}

	// 5. Create R-tree iterator (lazy — fetches leaf nodes on demand).
	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)}
	}
	iter := rtree.ScanIterator(m.tx, lastHV, lastKey, mbrPredicate)

	// 6. Build exact point filter from dimensional bounds (matches Java's containsPosition).
	pointFilter := m.buildPointFilter(dimExpr, scanRange)

	// 7. Apply row limit.
	limit := 0
	if scanProperties.ExecuteProperties.ReturnedRowLimit > 0 {
		limit = scanProperties.ExecuteProperties.ReturnedRowLimit
	}

	return &rtreeScanCursor{
		iter:              iter,
		index:             m.index,
		prefix:            prefix,
		limit:             limit,
		pointFilter:       pointFilter,
		outerContinuation: outerContinuation,
		props:             scanProperties.ExecuteProperties,
		startTime:         time.Now(),
	}
}

// buildMBRPredicate extracts dimensional bounds from scanRange and creates an
// MBR overlap predicate for R-tree subtree pruning. Returns nil if scanRange
// does not contain dimensional bounds.
func (m *multidimensionalIndexMaintainer) buildMBRPredicate(dimExpr *DimensionsKeyExpression, scanRange TupleRange) func(MBR) bool {
	if dimExpr.DimensionsSize <= 0 {
		return nil
	}

	dimStart := dimExpr.PrefixSize
	dimEnd := dimStart + dimExpr.DimensionsSize

	hasLow := scanRange.Low != nil && len(scanRange.Low) >= dimEnd
	hasHigh := scanRange.High != nil && len(scanRange.High) >= dimEnd

	if !hasLow && !hasHigh {
		return nil
	}

	queryMBR := MBR{
		Low:  make([]int64, dimExpr.DimensionsSize),
		High: make([]int64, dimExpr.DimensionsSize),
	}
	for d := 0; d < dimExpr.DimensionsSize; d++ {
		queryMBR.Low[d] = math.MinInt64
		queryMBR.High[d] = math.MaxInt64
		if hasLow {
			if v, ok := asInt64(scanRange.Low[dimStart+d]); ok {
				queryMBR.Low[d] = v
			}
		}
		if hasHigh {
			if v, ok := asInt64(scanRange.High[dimStart+d]); ok {
				queryMBR.High[d] = v
			}
		}
	}

	return func(nodeMBR MBR) bool {
		return nodeMBR.Overlaps(queryMBR)
	}
}

// buildPointFilter creates an exact point-in-range filter from the scanRange
// dimensional bounds. This is applied per-item after MBR subtree pruning,
// matching Java's SpatialPredicate.containsPosition() post-filter.
// Returns nil if scanRange doesn't specify dimensional bounds.
func (m *multidimensionalIndexMaintainer) buildPointFilter(dimExpr *DimensionsKeyExpression, scanRange TupleRange) func(Point) bool {
	if dimExpr.DimensionsSize <= 0 {
		return nil
	}

	dimStart := dimExpr.PrefixSize
	dimEnd := dimStart + dimExpr.DimensionsSize

	hasLow := scanRange.Low != nil && len(scanRange.Low) >= dimEnd
	hasHigh := scanRange.High != nil && len(scanRange.High) >= dimEnd

	if !hasLow && !hasHigh {
		return nil
	}

	type bound struct {
		low, high       int64
		hasLow, hasHigh bool
	}
	bounds := make([]bound, dimExpr.DimensionsSize)
	for d := 0; d < dimExpr.DimensionsSize; d++ {
		bounds[d].low = math.MinInt64
		bounds[d].high = math.MaxInt64
		if hasLow {
			if v, ok := asInt64(scanRange.Low[dimStart+d]); ok {
				bounds[d].low = v
				bounds[d].hasLow = true
			}
		}
		if hasHigh {
			if v, ok := asInt64(scanRange.High[dimStart+d]); ok {
				bounds[d].high = v
				bounds[d].hasHigh = true
			}
		}
	}

	return func(p Point) bool {
		for d := 0; d < len(bounds) && d < p.NumDimensions(); d++ {
			c := p.Coordinate(d)
			if bounds[d].hasLow && c < bounds[d].low {
				return false
			}
			if bounds[d].hasHigh && c > bounds[d].high {
				return false
			}
		}
		return true
	}
}

// DeleteWhere clears all R-tree data for the given prefix.
func (m *multidimensionalIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	rtSubspace := m.indexSubspace
	if len(prefix) > 0 {
		rtSubspace = m.indexSubspace.Sub(prefix...)
	}
	storage := newRTreeStorage(rtSubspace, m.rTreeConfig)
	rtree, err := NewRTree(storage, m.rTreeConfig)
	if err != nil {
		return fmt.Errorf("MULTIDIMENSIONAL index %q: %w", m.index.Name, err)
	}
	return rtree.Clear(m.tx)
}

// rtreeScanCursor wraps an RTreeIterator into a RecordCursor with support
// for row limits, proto-wrapped continuation tokens, and exact point filtering.
// Items are fetched lazily — only the leaf nodes needed to satisfy the row limit are read.
//
// The R-tree iterator applies MBR overlap pruning at the subtree level (approximate,
// false positives allowed). This cursor applies exact point-in-range filtering on each
// item, matching Java's containsPosition() post-filter.
type rtreeScanCursor struct {
	iter      *RTreeIterator
	index     *Index
	prefix    tuple.Tuple
	limit     int // 0 = unlimited
	delivered int
	lastHV    *big.Int
	lastKey   tuple.Tuple
	// Exact point filter: checks each item's coordinates against the scan range.
	// nil means no filtering (return all items).
	pointFilter func(Point) bool
	// outerContinuation carries prefix skip-scan state for FlatMapContinuation.
	// nil for single-prefix (non-skip-scan) scans.
	outerContinuation []byte
	closed            bool

	// RFC-106a scan governance. props carries the scan/byte/time limits +
	// FailOnScanLimitReached; scanned/bytesScanned count EVERY item read from the
	// R-tree (including point-filtered ones — reading them is the scan cost).
	props        ExecuteProperties
	scanned      int
	bytesScanned int64
	startTime    time.Time
}

func (c *rtreeScanCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		// Row limit check FIRST — clean ReturnLimitReached stop, and avoids
		// wasting an FDB read when the limit is already reached (ordering matches
		// index_scan so a satisfied row cap isn't turned into 54F01 by an equal
		// scan-record cap — RFC-106a).
		if c.limit > 0 && c.delivered >= c.limit {
			cont, cerr := c.buildContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return NewResultNoNext[*IndexEntry](ReturnLimitReached, &BytesContinuation{bytes: cont}), nil
		}

		// Scan governance (RFC-106a): bound the spatial scan by scanned records /
		// time / bytes, counting EVERY item read below (incl. point-filtered ones —
		// reading them is the scan cost). noNextOrFail → 54F01 in fail mode.
		if c.props.ScannedRecordsLimit > 0 && c.scanned >= c.props.ScannedRecordsLimit {
			cont, cerr := c.buildContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return noNextOrFail[*IndexEntry](c.props, ScanLimitReached, &BytesContinuation{bytes: cont})
		}
		if c.props.TimeLimit > 0 && c.scanned > 0 && time.Since(c.startTime) >= c.props.TimeLimit {
			cont, cerr := c.buildContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return NewResultNoNext[*IndexEntry](TimeLimitReached, &BytesContinuation{bytes: cont}), nil
		}
		if c.props.ScannedBytesLimit > 0 && c.scanned > 0 && c.bytesScanned >= c.props.ScannedBytesLimit {
			cont, cerr := c.buildContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return noNextOrFail[*IndexEntry](c.props, ByteLimitReached, &BytesContinuation{bytes: cont})
		}

		// Get next item from iterator.
		item, ok, err := c.iter.Next()
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		if !ok {
			return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
		}

		// Track position for continuation (even if filtered out, so resume
		// skips past this item).
		c.lastHV = item.HilbertValue
		c.lastKey = item.ItemKey()
		c.scanned++
		c.bytesScanned += int64(len(c.lastKey.Pack())) // approx scanned bytes (RFC-106a)

		// Exact point filter: skip items whose coordinates don't match the
		// scan range. This matches Java's containsPosition() post-filter.
		if c.pointFilter != nil && !c.pointFilter(item.Point) {
			continue
		}

		c.delivered++

		// Reconstruct the full index key: prefix + dims + suffix.
		key := make(tuple.Tuple, 0, len(c.prefix)+len(item.Point.Coordinates)+len(item.KeySuffix))
		if len(c.prefix) > 0 {
			key = append(key, c.prefix...)
		}
		key = append(key, item.Point.Coordinates...)
		key = append(key, item.KeySuffix...)

		entry := &IndexEntry{
			Index: c.index,
			Key:   key,
			Value: item.Value,
		}
		cont, cerr := c.buildContinuation()
		if cerr != nil {
			return RecordCursorResult[*IndexEntry]{}, cerr
		}
		return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
	}
}

// buildContinuation serializes the current position into a FlatMapContinuation proto
// wrapping a MultidimensionalIndexScanContinuation, matching Java's flatMapPipelined cursor.
//
// Returns an error rather than nil bytes on either failure path (no item has
// EVER been read yet, or MarshalVT fails). nil bytes would be indistinguishable
// from "no continuation" once wrapped by prefixSkipScanCursor's outer
// FlatMapContinuation — exactly the empty-placeholder shape Bug 1 was: a
// continuation that cannot resume must never be handed back as if it can.
// Unreachable today (every call site only reaches this after c.lastHV has been
// set by at least one scanned item — see the call sites' comments), but making
// the failure loud rather than silently degrading is what keeps that true.
func (c *rtreeScanCursor) buildContinuation() ([]byte, error) {
	if c.lastHV == nil {
		return nil, fmt.Errorf("MULTIDIMENSIONAL index %q: buildContinuation called before any item was read", c.index.Name)
	}
	hvBytes := c.lastHV.Bytes()
	if len(hvBytes) == 0 {
		// big.Int(0).Bytes() returns empty; protobuf treats empty bytes as nil.
		// Use [0x00] so the round-trip preserves the zero value.
		hvBytes = []byte{0}
	} else if hvBytes[0]&0x80 != 0 {
		// Prepend 0x00 to indicate positive (Java's BigInteger.toByteArray() two's-complement format).
		hvBytes = append([]byte{0x00}, hvBytes...)
	}
	inner := &gen.MultidimensionalIndexScanContinuation{
		LastHilbertValue: hvBytes,
		LastKey:          c.lastKey.Pack(),
	}
	innerBytes, err := inner.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("MULTIDIMENSIONAL index %q: marshal inner continuation: %w", c.index.Name, err)
	}

	// Wrap in FlatMapContinuation (matching Java's flatMapPipelined cursor).
	outer := &gen.FlatMapContinuation{
		InnerContinuation: innerBytes,
	}
	if c.outerContinuation != nil {
		outer.OuterContinuation = c.outerContinuation
	}
	data, err := outer.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("MULTIDIMENSIONAL index %q: marshal outer continuation: %w", c.index.Name, err)
	}
	return data, nil
}

func (c *rtreeScanCursor) Close() error {
	c.closed = true
	return nil
}

func (c *rtreeScanCursor) IsClosed() bool { return c.closed }

// prefixSkipScanCursor enumerates all distinct prefixes in the index subspace
// and scans each prefix's R-tree in sequence. Used when PrefixSize > 0 but the
// scanRange does not specify a prefix.
//
// Ported from Java's MultidimensionalIndexMaintainer.scan()
// (MultidimensionalIndexMaintainer.java:130-179), which always drives this
// shape through RecordCursor.flatMapPipelined: the outer cursor is a
// ChainedCursor of distinct prefix Tuples (prefixSkipScan/nextPrefixTuple,
// :189-246 — Go's findNextPrefix below) and the inner is one R-tree scan per
// prefix. FlatMapPipelinedCursor's own continuation
// (FlatMapPipelinedCursor.java:372-434) pairs the position AT the current
// prefix with the inner R-tree position:
//   - inner not exhausted (mid-prefix stop): outer = the prefix BEFORE the one
//     being resumed, inner = the R-tree position. Resuming re-derives the SAME
//     prefix (nextPrefixTuple is a deterministic "first distinct prefix after
//     this Tuple" query, ChainedCursor.java:224-234) and continues its R-tree
//     scan from the saved position.
//   - inner exhausted (moving to the next prefix): outer = the prefix just
//     fully consumed, no inner. Resuming finds the NEXT prefix after it.
//
// Row limiting is NOT done here — matching Java's innerScanProperties =
// scanProperties.with(ExecuteProperties::clearSkipAndLimit) (:124) and the
// .skipThenLimit() applied to the whole flattened cursor exactly once (:179).
// Scan() wraps this cursor in LimitRowsCursor instead: whichever row
// LimitRowsCursor stops after, its ReturnLimitReached continuation is simply
// THIS cursor's own (already prefix-aware) continuation for that row — the
// row-limit boundary needs no separate encoding, which is what the previous
// empty-placeholder continuation got wrong (it could never resume, so
// cross-prefix pagination silently replayed prefix #1 forever).
type prefixSkipScanCursor struct {
	m              *multidimensionalIndexMaintainer
	dimExpr        *DimensionsKeyExpression
	scanRange      TupleRange
	scanProperties ScanProperties

	// Parsed from the incoming continuation once, at construction (Scan()):
	// the outer_continuation / inner_continuation fields of the
	// FlatMapContinuation the previous page's last row returned.
	resumePrefixBytes []byte // Tuple-packed prefix to resume from/after; nil = start
	resumeInnerBytes  []byte // raw MultidimensionalIndexScanContinuation for the
	// first (resumed) prefix; nil = start that prefix fresh. Consumed once.

	// Current per-prefix cursor being drained.
	currentCursor RecordCursor[*IndexEntry]
	// FDB key position for finding the next prefix.
	nextPrefixStart fdb.Key

	// Cross-prefix continuation state — Java's priorOuterContinuation /
	// outerContinuation fields (FlatMapPipelinedCursor.java:70,207,222), kept as
	// Tuple-packed prefix bytes (ChainedCursor's own continuation encoding,
	// ChainedCursor.java:78,224-234).
	priorPrefixBytes   []byte // Tuple-packed bytes of the prefix BEFORE currentPrefix; nil = start
	currentPrefixBytes []byte // Tuple-packed bytes of currentPrefix

	// Shared RFC-106a scan budget across all prefixes (per-prefix cursor scans +
	// findNextPrefix enumeration reads) so a many-small-prefix skip-scan can't
	// bypass the cap by resetting a fresh per-prefix counter each prefix.
	totalScanned      int       // records: ScannedRecordsLimit
	totalBytesScanned int64     // bytes:   ScannedBytesLimit
	startTime         time.Time // wall-clock origin for TimeLimit (set on first OnNext)

	initialized bool
	exhausted   bool
	closed      bool
}

func (c *prefixSkipScanCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	// Checked before the lazy init below (which dereferences c.m): a cancelled
	// context must short-circuit even a zero-valued cursor (RFC-106a; pinned by
	// index_scan_unit_test.go's "honor ctx cancellation" sweep, which
	// constructs a bare &prefixSkipScanCursor{} with m left nil).
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	if !c.initialized {
		c.initialized = true
		c.startTime = time.Now() // shared time-budget origin (RFC-106a)
		if c.resumePrefixBytes != nil {
			lastPrefix, err := fastUnpack(c.resumePrefixBytes)
			if err != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf(
					"MULTIDIMENSIONAL prefix skip-scan: invalid continuation prefix: %w", err)
			}
			prefixSubspace := c.m.indexSubspace.Sub(tupleToElements(lastPrefix)...)
			end, err := fdb.Strinc(prefixSubspace.Bytes())
			if err != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf(
					"MULTIDIMENSIONAL prefix skip-scan: strinc resume prefix subspace: %w", err)
			}
			c.nextPrefixStart = fdb.Key(end)
			c.priorPrefixBytes = c.resumePrefixBytes
		} else {
			c.nextPrefixStart = fdb.Key(c.m.indexSubspace.Bytes())
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}

		// Aggregate scan budgets across all prefixes (RFC-106a). Without these a
		// skip-scan over many small prefixes — each under the per-prefix limit —
		// would never trip the cap because each per-prefix cursor resets its own
		// counter. These remain a terminal error regardless of
		// FailOnScanLimitReached: unlike the row limit above (now resumable via
		// the prefix-aware continuation), this is deliberately unpaginated —
		// tracked in TODO.md if that changes.
		scanLimit := c.scanProperties.ExecuteProperties.ScannedRecordsLimit
		if scanLimit > 0 && c.totalScanned >= scanLimit {
			return RecordCursorResult[*IndexEntry]{}, &ScanLimitReachedError{Reason: ScanLimitReached}
		}
		bytesLimit := c.scanProperties.ExecuteProperties.ScannedBytesLimit
		if bytesLimit > 0 && c.totalScanned > 0 && c.totalBytesScanned >= bytesLimit {
			return RecordCursorResult[*IndexEntry]{}, &ScanLimitReachedError{Reason: ByteLimitReached}
		}
		timeLimit := c.scanProperties.ExecuteProperties.TimeLimit
		if timeLimit > 0 && c.totalScanned > 0 && time.Since(c.startTime) >= timeLimit {
			return RecordCursorResult[*IndexEntry]{}, &ScanLimitReachedError{Reason: TimeLimitReached}
		}

		// If we have an active per-prefix cursor, delegate to it.
		if c.currentCursor != nil {
			result, err := c.currentCursor.OnNext(ctx)
			if err != nil {
				return RecordCursorResult[*IndexEntry]{}, err
			}
			if result.HasNext() {
				cont, cerr := c.buildValueContinuation(result.GetContinuation())
				if cerr != nil {
					return RecordCursorResult[*IndexEntry]{}, cerr
				}
				return NewResultWithValue(result.GetValue(), cont), nil
			}
			// Per-prefix cursor done — fold its scan counts into the shared budget
			// before closing, then close. (A non-rtree cursor — only *errorCursor,
			// whose OnNext already returned an error above — never reaches here, so
			// the count is never silently dropped.)
			if rc, ok := c.currentCursor.(*rtreeScanCursor); ok {
				c.totalScanned += rc.scanned
				c.totalBytesScanned += rc.bytesScanned
			}
			_ = c.currentCursor.Close()
			c.currentCursor = nil
			reason := result.GetNoNextReason()
			if reason.IsOutOfBand() {
				// The per-prefix cursor consumed the (remaining) shared scan/byte/time
				// budget mid-prefix. Terminal, same as the aggregate checks above.
				return RecordCursorResult[*IndexEntry]{}, &ScanLimitReachedError{Reason: reason}
			}
			// SourceExhausted → genuinely move to the next prefix. The prefix that
			// was just fully consumed becomes the "prior" the next one resumes from.
			c.priorPrefixBytes = c.currentPrefixBytes
		}

		if c.exhausted {
			return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
		}

		// Find the next prefix and create a cursor for it.
		prefix, found, err := c.findNextPrefix()
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		if !found {
			c.exhausted = true
			return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
		}
		c.totalScanned++ // the prefix-enumeration read counts against the scan budget
		c.currentPrefixBytes = prefix.Pack()

		// Build a per-prefix scanRange with this prefix in the Low/High bounds.
		prefixScanRange := c.scanRange
		if prefixScanRange.Low == nil || len(prefixScanRange.Low) < c.dimExpr.PrefixSize {
			prefixScanRange.Low = make(tuple.Tuple, c.dimExpr.PrefixSize)
			copy(prefixScanRange.Low, prefix)
		}
		if prefixScanRange.High == nil || len(prefixScanRange.High) < c.dimExpr.PrefixSize {
			prefixScanRange.High = make(tuple.Tuple, c.dimExpr.PrefixSize)
			copy(prefixScanRange.High, prefix)
		}

		// clearSkipAndLimit (Java :124): the per-prefix cursor is never row
		// limited — LimitRowsCursor (Scan()) is the only layer that stops
		// delivery, exactly like Java's outer .skipThenLimit(). A per-prefix
		// ReturnLimitReached would carry no prefix identity, so it must never
		// happen here.
		perPrefixProps := c.scanProperties
		perPrefixProps.ExecuteProperties = perPrefixProps.ExecuteProperties.WithReturnedRowLimit(0)
		// Decrement the shared scan budgets so this prefix's cursor can only consume
		// what's left of each aggregate cap (RFC-106a) — records, bytes, and time.
		if scanLimit > 0 {
			remainingScan := scanLimit - c.totalScanned
			if remainingScan <= 0 {
				continue // caught by the aggregate scan-budget check at the top
			}
			perPrefixProps.ExecuteProperties = perPrefixProps.ExecuteProperties.WithScannedRecordsLimit(remainingScan)
		}
		if bytesLimit > 0 {
			remainingBytes := bytesLimit - c.totalBytesScanned
			if remainingBytes <= 0 {
				continue
			}
			perPrefixProps.ExecuteProperties = perPrefixProps.ExecuteProperties.WithScannedBytesLimit(remainingBytes)
		}
		if timeLimit > 0 {
			remainingTime := timeLimit - time.Since(c.startTime)
			if remainingTime <= 0 {
				continue
			}
			perPrefixProps.ExecuteProperties = perPrefixProps.ExecuteProperties.WithTimeLimit(remainingTime)
		}

		// Use the saved inner continuation only for the first (resumed) prefix;
		// every later prefix starts its R-tree scan fresh. Re-wrap it in a
		// FlatMapContinuation so m.Scan()'s bound-prefix parser (which always
		// tries that shape first) decodes it unambiguously, rather than handing
		// it raw MultidimensionalIndexScanContinuation bytes that could
		// coincidentally parse as a (wrong) FlatMapContinuation — both messages
		// share field numbers 1/2 as bytes.
		var innerCont []byte
		if c.resumeInnerBytes != nil {
			wrapped := &gen.FlatMapContinuation{InnerContinuation: c.resumeInnerBytes}
			b, werr := wrapped.MarshalVT()
			if werr != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf(
					"MULTIDIMENSIONAL prefix skip-scan: rewrap resume continuation: %w", werr)
			}
			innerCont = b
			c.resumeInnerBytes = nil // only the first (resumed) prefix gets this
		}

		c.currentCursor = c.m.Scan(prefixScanRange, innerCont, perPrefixProps)
	}
}

// buildValueContinuation wraps the current per-prefix cursor's own
// continuation with this cursor's prefix-tracking layer, producing exactly
// the ONE-LEVEL FlatMapContinuation nesting Java's flatMapPipelined produces
// (FlatMapPipelinedCursor.java:409-434): outer = the position needed to
// re-derive (or move past) the current prefix, inner = the raw R-tree
// position within it. m.Scan()'s bound-prefix path always self-wraps its own
// continuation in a FlatMapContinuation too (matching Java's trivial
// single-prefix flatMapPipelined case, MultidimensionalIndexMaintainer.java:135-178)
// — unwrapMultidimensionalInner strips that layer back off so the two never
// nest.
func (c *prefixSkipScanCursor) buildValueContinuation(innerResultCont RecordCursorContinuation) (RecordCursorContinuation, error) {
	rawInner, err := unwrapMultidimensionalInner(innerResultCont)
	if err != nil {
		return nil, fmt.Errorf("MULTIDIMENSIONAL prefix skip-scan: %w", err)
	}
	fmc := &gen.FlatMapContinuation{
		OuterContinuation: c.priorPrefixBytes,
		InnerContinuation: rawInner,
	}
	b, err := fmc.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("MULTIDIMENSIONAL prefix skip-scan: marshal continuation: %w", err)
	}
	return &BytesContinuation{bytes: b}, nil
}

// unwrapMultidimensionalInner extracts the raw MultidimensionalIndexScanContinuation
// bytes from a bound-prefix scan's own continuation. m.Scan()'s single/bound-prefix
// path (case 2, used here for each per-prefix cursor) always wraps its position in
// exactly one FlatMapContinuation layer — the same wrapping Java applies to every
// multidimensional scan, skip-scan or not (MultidimensionalIndexMaintainer.java:135-178).
func unwrapMultidimensionalInner(cont RecordCursorContinuation) ([]byte, error) {
	if cont == nil || cont.IsEnd() {
		return nil, nil
	}
	b, err := cont.ToBytes()
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	var fmc gen.FlatMapContinuation
	if err := fmc.UnmarshalVT(b); err != nil {
		return nil, fmt.Errorf("invalid per-prefix continuation: %w", err)
	}
	return fmc.InnerContinuation, nil
}

// findNextPrefix discovers the next distinct prefix by reading one key from the
// index subspace at or after nextPrefixStart. Extracts the first PrefixSize
// tuple elements as the prefix, then advances nextPrefixStart past this prefix's
// entire subspace.
func (c *prefixSkipScanCursor) findNextPrefix() (tuple.Tuple, bool, error) {
	indexEnd, err := fdb.Strinc(c.m.indexSubspace.Bytes())
	if err != nil {
		return nil, false, fmt.Errorf("MULTIDIMENSIONAL prefix skip-scan: strinc index subspace: %w", err)
	}

	rng := fdb.KeyRange{
		Begin: c.nextPrefixStart,
		End:   fdb.Key(indexEnd),
	}
	kvs, err := c.m.tx.GetRange(rng, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return nil, false, fmt.Errorf("MULTIDIMENSIONAL prefix skip-scan: range read: %w", err)
	}
	if len(kvs) == 0 {
		return nil, false, nil
	}
	// The enumeration read counts against the shared byte budget too, not just
	// the record budget (RFC-106a) — otherwise many-prefix overhead bypasses
	// ScannedBytesLimit. (The caller adds 1 to totalScanned for the record count.)
	c.totalBytesScanned += int64(len(kvs[0].Key) + len(kvs[0].Value))

	// Unpack the key relative to the index subspace.
	t, err := fastSubspaceUnpack(kvs[0].Key, len(c.m.indexSubspace.Bytes()))
	if err != nil {
		// Key is not in our subspace — shouldn't happen, but skip gracefully.
		return nil, false, nil
	}

	if len(t) < c.dimExpr.PrefixSize {
		return nil, false, nil
	}

	// Extract the prefix (first PrefixSize elements).
	prefix := make(tuple.Tuple, c.dimExpr.PrefixSize)
	copy(prefix, t[:c.dimExpr.PrefixSize])

	// Advance nextPrefixStart past this prefix's entire subspace.
	prefixSubspace := c.m.indexSubspace.Sub(tupleToElements(prefix)...)
	prefixEnd, err := fdb.Strinc(prefixSubspace.Bytes())
	if err != nil {
		return nil, false, fmt.Errorf("MULTIDIMENSIONAL prefix skip-scan: strinc prefix subspace: %w", err)
	}
	c.nextPrefixStart = fdb.Key(prefixEnd)

	return prefix, true, nil
}

// tupleToElements converts a tuple.Tuple to []tuple.TupleElement for use with
// subspace.Sub(). This is needed because Sub takes variadic TupleElement, not Tuple.
func tupleToElements(t tuple.Tuple) []tuple.TupleElement {
	elems := make([]tuple.TupleElement, len(t))
	for i, v := range t {
		elems[i] = v
	}
	return elems
}

func (c *prefixSkipScanCursor) Close() error {
	c.closed = true
	if c.currentCursor != nil {
		return c.currentCursor.Close()
	}
	return nil
}

func (c *prefixSkipScanCursor) IsClosed() bool {
	return c.closed
}
