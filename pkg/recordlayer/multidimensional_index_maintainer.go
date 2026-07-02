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
		return &prefixSkipScanCursor{
			m:               m,
			dimExpr:         dimExpr,
			scanRange:       scanRange,
			continuation:    continuation,
			scanProperties:  scanProperties,
			nextPrefixStart: fdb.Key(m.indexSubspace.Bytes()),
		}
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
		// scan-record cap — codex RFC-106a).
		if c.limit > 0 && c.delivered >= c.limit {
			cont := c.buildContinuation()
			return NewResultNoNext[*IndexEntry](ReturnLimitReached, &BytesContinuation{bytes: cont}), nil
		}

		// Scan governance (RFC-106a): bound the spatial scan by scanned records /
		// time / bytes, counting EVERY item read below (incl. point-filtered ones —
		// reading them is the scan cost). noNextOrFail → 54F01 in fail mode.
		if c.props.ScannedRecordsLimit > 0 && c.scanned >= c.props.ScannedRecordsLimit {
			return noNextOrFail[*IndexEntry](c.props, ScanLimitReached, &BytesContinuation{bytes: c.buildContinuation()})
		}
		if c.props.TimeLimit > 0 && c.scanned > 0 && time.Since(c.startTime) >= c.props.TimeLimit {
			return NewResultNoNext[*IndexEntry](TimeLimitReached, &BytesContinuation{bytes: c.buildContinuation()}), nil
		}
		if c.props.ScannedBytesLimit > 0 && c.scanned > 0 && c.bytesScanned >= c.props.ScannedBytesLimit {
			return noNextOrFail[*IndexEntry](c.props, ByteLimitReached, &BytesContinuation{bytes: c.buildContinuation()})
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
		cont := c.buildContinuation()
		return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
	}
}

// buildContinuation serializes the current position into a FlatMapContinuation proto
// wrapping a MultidimensionalIndexScanContinuation, matching Java's flatMapPipelined cursor.
func (c *rtreeScanCursor) buildContinuation() []byte {
	if c.lastHV == nil {
		return nil
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
		return nil
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
		return nil
	}
	return data
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
// Limitation: continuation tokens work within a single prefix but cross-prefix
// resume is not supported (the MultidimensionalIndexScanContinuation proto does
// not encode the prefix). When a prefix is exhausted, the cursor moves to the
// next prefix and resets the continuation.
type prefixSkipScanCursor struct {
	m              *multidimensionalIndexMaintainer
	dimExpr        *DimensionsKeyExpression
	scanRange      TupleRange
	continuation   []byte
	scanProperties ScanProperties

	// Current per-prefix cursor being drained.
	currentCursor RecordCursor[*IndexEntry]
	// FDB key position for finding the next prefix.
	nextPrefixStart fdb.Key
	// Total entries delivered across all prefixes.
	totalDelivered int
	// Shared RFC-106a scan budget across all prefixes (per-prefix cursor scans +
	// findNextPrefix enumeration reads) so a many-small-prefix skip-scan can't
	// bypass the cap by resetting a fresh per-prefix counter each prefix.
	totalScanned      int       // records: ScannedRecordsLimit
	totalBytesScanned int64     // bytes:   ScannedBytesLimit
	startTime         time.Time // wall-clock origin for TimeLimit (set on first OnNext)
	// Row limit (0 = unlimited).
	limit int
	// Whether we've computed the limit yet.
	limitComputed bool
	exhausted     bool
	closed        bool
}

func (c *prefixSkipScanCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if !c.limitComputed {
		c.limitComputed = true
		c.startTime = time.Now() // shared time-budget origin (RFC-106a)
		if c.scanProperties.ExecuteProperties.ReturnedRowLimit > 0 {
			c.limit = c.scanProperties.ExecuteProperties.ReturnedRowLimit
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		// Check row limit before proceeding.
		if c.limit > 0 && c.totalDelivered >= c.limit {
			// We've hit the global row limit. Return limit-reached.
			// Cross-prefix continuation is not supported, so we use an opaque
			// non-end continuation (empty bytes) to satisfy the cursor invariant
			// that ReturnLimitReached must not carry an end continuation.
			return NewResultNoNext[*IndexEntry](ReturnLimitReached, &BytesContinuation{bytes: []byte{}}), nil
		}

		// Aggregate scan budgets across all prefixes (RFC-106a). Without these a
		// skip-scan over many small prefixes — each under the per-prefix limit —
		// would never trip the cap because each per-prefix cursor resets its own
		// counter.
		//
		// These stops are ALWAYS a terminal error, even when FailOnScanLimitReached
		// is off: cross-prefix resume is unsupported (see the type comment),
		// so the skip-scan cannot hand back a valid continuation — a paginating
		// no-next here carries empty bytes, which a resuming caller reads as "no
		// continuation" and restarts from the first prefix (an infinite re-scan).
		// Erroring is the only honest option; a bound-prefix scan (single
		// rtreeScanCursor) still paginates these limits normally.
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
				c.totalDelivered++
				return result, nil
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
			if reason == ReturnLimitReached {
				// The per-prefix cursor hit the (shared) row limit. Propagate.
				return result, nil
			}
			if reason.IsOutOfBand() {
				// The per-prefix cursor consumed the (remaining) shared scan/byte/time
				// budget mid-prefix. Like the aggregate checks above, this is a TERMINAL
				// error even in non-fail mode: the per-prefix continuation has no
				// cross-prefix state, so propagating it as a paginating no-next would
				// resume from the first prefix (infinite re-scan). FailOnScanLimitReached
				// is moot here — the per-prefix cursor would have errored upstream if set.
				return RecordCursorResult[*IndexEntry]{}, &ScanLimitReachedError{Reason: reason}
			}
			// SourceExhausted → genuinely move to the next prefix.
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

		// Compute remaining row limit for this prefix's cursor.
		perPrefixProps := c.scanProperties
		if c.limit > 0 {
			remaining := c.limit - c.totalDelivered
			if remaining <= 0 {
				continue // will be caught by limit check at top
			}
			perPrefixProps.ExecuteProperties = perPrefixProps.ExecuteProperties.WithReturnedRowLimit(remaining)
		}
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

		// Use the continuation only for the first prefix (subsequent prefixes
		// start from the beginning since cross-prefix continuation is not supported).
		var cont []byte
		if c.continuation != nil {
			cont = c.continuation
			c.continuation = nil // only use once
		}

		c.currentCursor = c.m.Scan(prefixScanRange, cont, perPrefixProps)
	}
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
