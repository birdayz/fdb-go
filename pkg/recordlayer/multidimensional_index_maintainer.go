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
	storage.env = m.store.Env()
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
	storage.env = m.store.Env()
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
			// Resolved ONCE for the whole leg and shared (by pointer) with every
			// per-prefix rtreeScanCursor this leg opens — see the struct doc
			// comment and scanBoundPrefix's freePassUsed parameter.
			scanState: resolveScanLimiterState(scanProperties.ExecuteProperties),
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

	return m.scanBoundPrefix(dimExpr, scanRange, continuation, scanProperties, nil)
}

// scanBoundPrefix builds the R-tree scan cursor for a single, fully-bound
// prefix (case 2 of Scan(): the scanRange's Low tuple covers the whole
// PrefixSize, so there is exactly one R-tree to search). It is also the
// per-prefix inner cursor prefixSkipScanCursor.OnNext opens for each distinct
// prefix its skip-scan discovers, mirroring how
// MultidimensionalIndexMaintainer.scan()'s flatMapPipelined inner factory
// (MultidimensionalIndexMaintainer.java:139-165) builds one RTree scan per
// prefix from the SAME closed-over dimensionsKeyExpression (:119-120).
//
// freePassUsed threads the record-scan free-initial-pass gate shared across
// every prefix within one leg — nil for a standalone (non-skip-scan) scan,
// which owns its own free pass exactly as before; non-nil when called from
// prefixSkipScanCursor, which passes the SAME *bool to every prefix in the
// leg. See rtreeScanCursor.hadFreePass and prefixSkipScanCursor's doc comment
// for why: Java shares ONE CursorLimitManager (MultidimensionalIndexMaintainer.java:125)
// across every R-tree scan the flatMapPipelined loop opens for one top-level
// scan() call (:155,162), so usedInitialPass is granted once per LEG, not once
// per prefix.
func (m *multidimensionalIndexMaintainer) scanBoundPrefix(
	dimExpr *DimensionsKeyExpression,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
	freePassUsed *bool,
) RecordCursor[*IndexEntry] {
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
	storage.env = m.store.Env()
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
		scanState:         resolveScanLimiterState(scanProperties.ExecuteProperties),
		freePassUsed:      freePassUsed,
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
	storage.env = m.store.Env()
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
	// FailOnScanLimitReached; scanned counts EVERY item read from the R-tree
	// (including point-filtered ones — reading them is the scan cost).
	// scanState is the (possibly shared) scanned-records/scanned-bytes/time
	// counter set the limits above are checked against — see
	// ScanLimiterState's doc comment. Never nil.
	props     ExecuteProperties
	scanned   int
	scanState *ScanLimiterState

	// freePassUsed, when non-nil, is a leg-wide free-initial-pass gate shared
	// (by pointer) with every other prefix's rtreeScanCursor within the SAME
	// prefixSkipScanCursor — the Go analog of Java sharing ONE CursorLimitManager
	// (and so one usedInitialPass) across every R-tree scan a single
	// MultidimensionalIndexMaintainer.scan() call opens
	// (MultidimensionalIndexMaintainer.java:125,155,162), rather than minting a
	// fresh CursorLimitManager per prefix. nil for a standalone (non-skip-scan)
	// scan, which owns its own free pass via c.scanned alone, exactly like every
	// other leaf cursor in this package. See hadFreePass.
	freePassUsed *bool
}

// hadFreePass reports whether the one free record-scan Java's CursorLimitManager
// grants before honoring an already-exceeded limit (CursorLimitManager.java:134-138)
// has already been spent — by THIS cursor, or by an earlier prefix in the same leg
// when freePassUsed is shared. A fresh per-prefix rtreeScanCursor must NOT get its
// own fresh grant on top of one a sibling prefix already used; Java's single
// CursorLimitManager instance (shared across every prefix's R-tree scan within one
// scan() call) makes that structurally impossible, so Go must too.
func (c *rtreeScanCursor) hadFreePass() bool {
	if c.scanned > 0 {
		return true
	}
	return c.freePassUsed != nil && *c.freePassUsed
}

// limitContinuation returns the continuation to report on a scan/byte/time
// halt. Once this cursor has returned at least one item, it delegates to
// buildContinuation for a genuinely resumable position (unchanged behavior).
//
// Sharing rtreeFreePassUsed across a leg's prefixes (hadFreePass) makes a NEW
// case reachable: a freshly-opened per-prefix cursor whose very first check
// halts before it has read anything at all (c.lastHV still nil) — either a
// sibling prefix already spent the leg's free pass, or (standalone scans,
// records limit only) FailOnScanLimitReached denies the free pass outright
// (CursorLimitManager.java:135-136 throws immediately in this situation,
// never returning a graceful result at all). Both of that call's ONLY two
// possible callers discard the continuation unconditionally in this exact
// state: noNextOrFail's fail-mode branch returns a bare ScanLimitReachedError
// (no continuation field to put it in), and prefixSkipScanCursor's
// IsOutOfBand handling never reads the inner cursor's continuation either way
// — this aggregate stop is deliberately terminal. So nil is correct here, not
// a workaround: there is no resumable position to report because nothing was
// ever read, and nothing downstream looks at it in that state.
func (c *rtreeScanCursor) limitContinuation() (RecordCursorContinuation, error) {
	if c.lastHV == nil {
		// A literal nil interface value, NOT &BytesContinuation{bytes: nil} —
		// BytesContinuation treats a nil byte slice as IsEnd()==true (its own
		// end-of-cursor marker), which NewResultNoNext would reject for any
		// reason other than SourceExhausted. nil itself carries no such
		// meaning to NewResultNoNext/noNextOrFail.
		return nil, nil
	}
	cont, err := c.buildContinuation()
	if err != nil {
		return nil, err
	}
	return &BytesContinuation{bytes: cont}, nil
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
		// reading them is the scan cost). noNextOrFail → 54F01 in fail mode. The
		// scanned-records free pass (hadFreePass) is bypassed under
		// FailOnScanLimitReached, matching Java's CursorLimitManager.java:135-136;
		// the time and byte free passes stay unconditional (:137-138).
		if c.props.ScannedRecordsLimit > 0 &&
			(c.hadFreePass() || c.props.FailOnScanLimitReached) &&
			c.scanState.RecordsScanned() >= c.props.ScannedRecordsLimit {
			cont, cerr := c.limitContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return noNextOrFail[*IndexEntry](c.props, ScanLimitReached, cont)
		}
		if c.props.TimeLimit > 0 && c.hadFreePass() && time.Since(c.scanState.StartTime()) >= c.props.TimeLimit {
			cont, cerr := c.limitContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return noNextOrFail[*IndexEntry](c.props, TimeLimitReached, cont)
		}
		if c.props.ScannedBytesLimit > 0 && c.hadFreePass() && c.scanState.BytesScanned() >= c.props.ScannedBytesLimit {
			cont, cerr := c.limitContinuation()
			if cerr != nil {
				return RecordCursorResult[*IndexEntry]{}, cerr
			}
			return noNextOrFail[*IndexEntry](c.props, ByteLimitReached, cont)
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
		if c.freePassUsed != nil {
			*c.freePassUsed = true
		}
		c.scanState.AddRecordScanned()
		c.scanState.AddBytesScanned(int64(len(c.lastKey.Pack()))) // approx scanned bytes (RFC-106a)

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
// Unreachable today: the row-limit check calls this directly only after
// c.delivered has reached a positive limit (so c.lastHV is already set), and
// the scan/byte/time governance checks go through limitContinuation, which
// guards the c.lastHV == nil case itself rather than ever calling in here
// with it. Still erroring loudly (not silently degrading) keeps that true.
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
//
// Scan/byte/time governance (RFC-106a) charges the SAME scanState this leg was
// constructed with (resolveScanLimiterState in Scan(), stored on the field
// below) — the caller's shared *ScanLimiterState for an IN-join/IN-union leg,
// or a private fallback the leg owns alone. Every prefix's findNextPrefix
// enumeration read and every prefix's rtreeScanCursor item read charges that
// SAME instance directly (no separate local total to fold back in), so a
// many-legged IN-join/IN-union over a MULTIDIMENSIONAL index now aggregates
// its scan/byte/time budget across every leg exactly like every other leaf
// cursor in this package (key_value_cursor.go, index_scan.go,
// count_index_maintainer.go, bitmap_value_index_maintainer.go,
// text_cursor.go) — matching Java's ExecuteState being shared by reference
// through every cursor in the tree (ExecuteState.java:44-58).
//
// The free-initial-pass gate (rtreeFreePassUsed) is scoped to this ONE leg,
// not to each prefix within it: Java instantiates exactly ONE CursorLimitManager
// per top-level MultidimensionalIndexMaintainer.scan() call
// (MultidimensionalIndexMaintainer.java:125) and reuses it — by reference, via
// the OnRead listener and ItemSlotCursor constructors closed over inside the
// flatMapPipelined lambda (:155,162) — for every prefix's R-tree scan. Its
// usedInitialPass field is therefore granted ONCE for the whole leg, not once
// per prefix; a fresh per-prefix rtreeScanCursor sharing rtreeFreePassUsed
// (via scanBoundPrefix's freePassUsed parameter) reproduces that exactly. The
// prefix-ENUMERATION reads (findNextPrefix) are a separate matter: Java's
// nextPrefixTuple builds a brand-new KeyValueCursor — and so a brand-new,
// independent CursorLimitManager — on every call
// (KeyValueCursorBase.java:359, MultidimensionalIndexMaintainer.java:213-246),
// so each enumeration read is checked against ITS OWN fresh usedInitialPass,
// never against rtreeFreePassUsed. That does not mean the enumeration read is
// always ungated, though: CursorLimitManager.tryRecordScan()'s halt condition
// is "!recordScanLimiter.tryRecordScan() && (usedInitialPass ||
// failOnScanLimitReached)" (CursorLimitManager.java:134-136) — a fresh
// usedInitialPass=false grants a free pass in the DEFAULT (paginating) mode
// (the "usedInitialPass ||" side never becomes true), but the
// "|| failOnScanLimitReached" side means FailOnScanLimitReached denies that
// free pass regardless of usedInitialPass. So in fail mode, findNextPrefix
// below is gated on the shared record-scan budget exactly like every other
// leaf cursor's first read; in paginating mode it is intentionally left
// ungated, matching Java's always-fresh free pass for this read. Bytes/time
// limits stay unconditionally free-passed for this read in BOTH modes —
// their halt conditions are gated on usedInitialPass alone, with no
// failOnScanLimitReached override, so a fresh manager never halts on them.
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

	// scanState is the (possibly shared) scanned-records/scanned-bytes/time
	// counter set for this whole leg — see the struct doc comment above and
	// ScanLimiterState's own doc comment. Resolved once in Scan() and handed
	// unchanged to every per-prefix rtreeScanCursor (scanBoundPrefix), so
	// there is exactly one running total for the leg, never a local copy to
	// reconcile with it. Never nil.
	scanState *ScanLimiterState
	// rtreeFreePassUsed is the leg-wide free-initial-pass gate shared with
	// every prefix's rtreeScanCursor — see the struct doc comment above.
	rtreeFreePassUsed bool

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
			// Per-prefix cursor done — close it. It already charged c.scanState
			// directly (scanBoundPrefix hands it the SAME instance, not a private
			// copy to fold back in), so there is nothing left to reconcile here.
			_ = c.currentCursor.Close()
			c.currentCursor = nil
			reason := result.GetNoNextReason()
			if reason.IsOutOfBand() {
				// The per-prefix cursor's own scan/byte/time check (shared scanState
				// + shared rtreeFreePassUsed) tripped mid-prefix. Terminal — this
				// aggregate stop is deliberately unpaginated, unlike the row limit
				// above (resumable via the prefix-aware continuation).
				return RecordCursorResult[*IndexEntry]{}, &ScanLimitReachedError{Reason: reason}
			}
			// SourceExhausted → genuinely move to the next prefix. The prefix that
			// was just fully consumed becomes the "prior" the next one resumes from.
			c.priorPrefixBytes = c.currentPrefixBytes
		}

		if c.exhausted {
			return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
		}

		// Ask before reading: under FailOnScanLimitReached the shared record-scan
		// budget must be checked BEFORE findNextPrefix issues its GetRange, not
		// charged after the read has already come back. A hard limit that only
		// surfaces once the read has escaped is not a hard limit — see
		// noNextOrFail's own doc comment, and CursorLimitManager.tryRecordScan()
		// (CursorLimitManager.java:134-136), which is called by
		// KeyValueCursorBase.onNext() BEFORE it awaits iterator.onHasNext()
		// (KeyValueCursorBase.java:99-100), not after. Scoped to
		// FailOnScanLimitReached only — see the struct doc comment above for why
		// the default (paginating) mode leaves this read's free pass alone.
		if c.scanProperties.ExecuteProperties.FailOnScanLimitReached &&
			c.scanProperties.ExecuteProperties.ScannedRecordsLimit > 0 &&
			c.scanState.RecordsScanned() >= c.scanProperties.ExecuteProperties.ScannedRecordsLimit {
			return noNextOrFail[*IndexEntry](c.scanProperties.ExecuteProperties, ScanLimitReached, nil)
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
		// findNextPrefix already charged c.scanState.AddRecordScanned() for this
		// read (on every return path, hit or miss) — nothing left to charge here.
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
		// Hand the per-prefix cursor THIS leg's own scanState (never a private
		// copy) so every prefix's item reads charge the SAME running total —
		// see the struct doc comment above. Because it's the same counter (not
		// a remaining-budget delta), the ORIGINAL ScannedRecordsLimit/
		// ScannedBytesLimit/TimeLimit pass through unchanged below: comparing
		// the shared cumulative count against the original limit is already
		// the correct absolute check, no per-prefix "remaining budget" needed.
		perPrefixProps.ExecuteProperties.ScanState = c.scanState

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

		c.currentCursor = c.m.scanBoundPrefix(c.dimExpr, prefixScanRange, innerCont, perPrefixProps, &c.rtreeFreePassUsed)
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
	// Charge the shared record-scan budget for this ATTEMPT immediately before
	// issuing the GetRange — matching CursorLimitManager.tryRecordScan(), which
	// decrements recordScanLimiter BEFORE KeyValueCursorBase.onNext() awaits
	// iterator.onHasNext() (CursorLimitManager.java:134-136,
	// KeyValueCursorBase.java:99-100). The charge reflects the decision to
	// read, not the read's outcome: every return path below this point (error,
	// miss, unparseable key, short prefix, or a genuine next prefix) has
	// already performed the GetRange, so every one of them costs the same one
	// record — matching Java, where a miss decrements recordScanLimiter
	// exactly like a hit. Charging only the success path (as this used to)
	// silently undercounts: an enumeration that terminates by exhaustion still
	// issued a real read, and across a fan-out plan sharing one ScanLimiterState
	// that is one uncharged read per exhausted leg, not a one-off. Only the
	// Strinc call above this point returns without ever reaching this line —
	// pure in-memory key-encoding, no read attempted, so it correctly charges
	// nothing.
	c.scanState.AddRecordScanned()
	kvs, err := c.m.tx.GetRange(rng, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	if err != nil {
		return nil, false, fmt.Errorf("MULTIDIMENSIONAL prefix skip-scan: range read: %w", err)
	}
	if len(kvs) == 0 {
		return nil, false, nil
	}
	// The enumeration read counts against the shared byte budget too, not just
	// the record budget (RFC-106a) — otherwise many-prefix overhead bypasses
	// ScannedBytesLimit. Bytes are charged only on an actual hit, matching
	// Java's reportScannedBytes call, which KeyValueCursorBase.onNext() only
	// reaches inside its `if (hasNext)` branch (KeyValueCursorBase.java:107) —
	// unlike the record charge above, a miss costs no bytes in Java either.
	c.scanState.AddBytesScanned(int64(len(kvs[0].Key) + len(kvs[0].Value)))

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
