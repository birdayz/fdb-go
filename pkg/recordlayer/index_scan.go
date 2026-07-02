package recordlayer

import (
	"context"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// IndexScanType identifies the type of index scan.
// Matches Java's com.apple.foundationdb.record.IndexScanType.
type IndexScanType string

const (
	// IndexScanByValue scans a VALUE index by its indexed values.
	IndexScanByValue IndexScanType = "BY_VALUE"
	// IndexScanByRank scans a RANK index by rank position.
	// The range bounds contain [group..., rank] where rank is an int64.
	// Matches Java's IndexScanType.BY_RANK.
	IndexScanByRank IndexScanType = "BY_RANK"

	// IndexScanByTextToken scans a TEXT index by text token.
	// Matches Java's IndexScanType.BY_TEXT_TOKEN.
	IndexScanByTextToken IndexScanType = "BY_TEXT_TOKEN"

	// IndexScanByGroup scans a PERMUTED_MIN/MAX index by group in the
	// secondary (permuted) subspace. Returns one entry per group with the
	// extremum value, ordered by [groupPrefix, value, groupSuffix].
	// Matches Java's IndexScanType.BY_GROUP.
	IndexScanByGroup IndexScanType = "BY_GROUP"

	// IndexScanByTimeWindow scans a TIME_WINDOW_LEADERBOARD index within a
	// specific time window. The scan range contains score bounds; the
	// leaderboard type and timestamp are provided via TimeWindowScanRange.
	// Matches Java's IndexScanType.BY_TIME_WINDOW.
	IndexScanByTimeWindow IndexScanType = "BY_TIME_WINDOW"

	// IndexScanByDistance scans a VECTOR index by distance to a query vector.
	// Returns results sorted by distance (closest first) as a kNN search.
	// Must be used with ScanVectorIndex which provides VectorScanBounds.
	// Matches Java's IndexScanType.BY_DISTANCE.
	IndexScanByDistance IndexScanType = "BY_DISTANCE"

	// IndexScanByDistanceOrderedStream is the RFC-156 Phase C VBASE distance-
	// ordered STREAMING scan: a demand-driven cursor that widens its scanned
	// horizon in batches as the consumer (Filter → Limit) pulls, bounded by a
	// budget cap (honest ScanLimitReached truncation, never a silent < k). Go-only
	// read-side extension; no wire-format change.
	IndexScanByDistanceOrderedStream IndexScanType = "BY_DISTANCE_ORDERED_STREAM"
)

// TupleRange specifies a range of tuples for index scanning.
// Matches Java's com.apple.foundationdb.record.TupleRange.
type TupleRange struct {
	Low          tuple.Tuple
	High         tuple.Tuple
	LowEndpoint  EndpointType
	HighEndpoint EndpointType
}

// TupleRangeAll covers all entries in the index.
// Matches Java's TupleRange.ALL.
var TupleRangeAll = TupleRange{
	LowEndpoint:  EndpointTypeTreeStart,
	HighEndpoint: EndpointTypeTreeEnd,
}

// TupleRangeAllOf returns a range for all entries with the given tuple prefix.
// Matches Java's TupleRange.allOf(Tuple).
// For example, TupleRangeAllOf(tuple.Tuple{"alice"}) returns all index entries
// where the first indexed value is "alice".
func TupleRangeAllOf(prefix tuple.Tuple) TupleRange {
	if prefix == nil {
		return TupleRangeAll
	}
	return TupleRange{
		Low:          prefix,
		High:         prefix,
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeInclusive,
	}
}

// TupleRangeBetween returns a range [low, high) — low inclusive, high exclusive.
// Matches Java's TupleRange.between(Tuple, Tuple).
func TupleRangeBetween(low, high tuple.Tuple) TupleRange {
	return TupleRange{
		Low:          low,
		High:         high,
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeExclusive,
	}
}

// TupleRangeBetweenInclusive returns a range [low, high] — both inclusive.
// Matches Java's TupleRange.betweenInclusive(Tuple, Tuple).
func TupleRangeBetweenInclusive(low, high tuple.Tuple) TupleRange {
	return TupleRange{
		Low:          low,
		High:         high,
		LowEndpoint:  EndpointTypeRangeInclusive,
		HighEndpoint: EndpointTypeRangeInclusive,
	}
}

// TupleRangePrefixString creates a range scanning all entries whose string
// element starts with the given prefix. Both endpoints use
// EndpointTypePrefixString so that the tuple-packed trailing null byte is
// stripped (low) or strinc'd (high).
// Matches Java's TupleRange.prefixedBy(String).
func TupleRangePrefixString(token string) TupleRange {
	return TupleRange{
		Low:          tuple.Tuple{token},
		High:         tuple.Tuple{token},
		LowEndpoint:  EndpointTypePrefixString,
		HighEndpoint: EndpointTypePrefixString,
	}
}

// Prepend prepends a tuple prefix to both Low and High bounds.
// Matches Java's TupleRange.prepend(Tuple).
func (r TupleRange) Prepend(prefix tuple.Tuple) TupleRange {
	prependTuple := func(t tuple.Tuple) tuple.Tuple {
		if t == nil {
			return prefix
		}
		result := make(tuple.Tuple, 0, len(prefix)+len(t))
		result = append(result, prefix...)
		result = append(result, t...)
		return result
	}
	return TupleRange{
		Low:          prependTuple(r.Low),
		High:         prependTuple(r.High),
		LowEndpoint:  r.LowEndpoint,
		HighEndpoint: r.HighEndpoint,
	}
}

// ToFDBRange converts this TupleRange to an fdb.KeyRange relative to a subspace.
// Matches Java's TupleRange.toRange(Subspace).
func (r TupleRange) ToFDBRange(ss subspace.Subspace) fdb.KeyRange {
	var begin, end fdb.Key

	switch r.LowEndpoint {
	case EndpointTypeTreeStart:
		begin = ss.FDBKey()
	case EndpointTypeRangeInclusive:
		begin = ss.Pack(r.Low)
	case EndpointTypeRangeExclusive:
		// Exclusive low = Strinc(pack(low)), matching Java TupleRange.toRange()
		// (TupleRange.java:485: `lowBytes = ByteArrayUtil.strinc(lowBytes)`). This is
		// the prefix-exclusion boundary, NOT firstGreaterThan(pack(low)) (append
		// 0x00). The distinction is observable for byte/string keys: `col > X` skips
		// not only the exact `X` entry but also any entry whose encoding is `pack(X)`
		// followed by bytes that sort below Strinc's increment — most notably a stored
		// value equal to X plus a trailing 0x00 (e.g. `b > X'01'` excludes the stored
		// value X'0100', whose encoding `01 01 00 FF 00` sorts before Strinc's
		// `01 01 01`). Java behaves identically, so this is REQUIRED for wire-level
		// cross-engine result consistency — do NOT "correct" it to append-0x00, which
		// would make Go return rows Java omits on a shared cluster. Pinned by
		// bytes_gt_index_conformance_probe_test.go.
		packed := ss.Pack(r.Low)
		inc, err := fdb.Strinc(packed)
		if err != nil {
			begin = ss.FDBKey()
		} else {
			begin = inc
		}
	case EndpointTypePrefixString:
		// Strip the trailing null byte (string terminator in FDB tuple encoding).
		// Matches Java's TupleRange.toRange() PREFIX_STRING low handling:
		//   lowBytes = Arrays.copyOfRange(lowBytes, 0, lowBytes.length - 1)
		packed := ss.Pack(r.Low)
		begin = packed[:len(packed)-1]
	default:
		begin = ss.FDBKey()
	}

	switch r.HighEndpoint {
	case EndpointTypeTreeEnd:
		_, endKey := ss.FDBRangeKeys()
		end = endKey.FDBKey()
	case EndpointTypeRangeInclusive:
		packed := ss.Pack(r.High)
		end = append(packed, 0xFF)
	case EndpointTypeRangeExclusive:
		end = ss.Pack(r.High)
	case EndpointTypePrefixString:
		// Strip the trailing null byte, then strinc the result.
		// Matches Java's TupleRange.toRange() PREFIX_STRING high handling:
		//   strip trailing 0xFF bytes, then increment last byte.
		packed := ss.Pack(r.High)
		stripped := packed[:len(packed)-1]
		// Remove trailing 0xFF bytes
		newLen := len(stripped)
		for newLen >= 1 && stripped[newLen-1] == 0xFF {
			newLen--
		}
		if newLen == 0 {
			end = fdb.Key{0xFF}
		} else {
			dest := make([]byte, newLen)
			copy(dest, stripped[:newLen])
			dest[newLen-1]++
			end = dest
		}
	default:
		_, endKey := ss.FDBRangeKeys()
		end = endKey.FDBKey()
	}

	return fdb.KeyRange{Begin: begin, End: end}
}

// IndexEntry represents a single entry from an index scan.
// Matches Java's com.apple.foundationdb.record.IndexEntry.
type IndexEntry struct {
	Index *Index
	Key   tuple.Tuple // Full key (indexed values + primary key components)
	Value tuple.Tuple // Value tuple (empty for VALUE indexes)

	primaryKey tuple.Tuple // Lazily extracted
}

// PrimaryKey extracts the primary key portion from the index entry key.
// When the index has primaryKeyComponentPositions, some PK components are
// pulled from the index key portion (deduplicated) and the rest from the
// appended tail. Matches Java's IndexEntry.getPrimaryKey() → Index.getEntryPrimaryKey().
func (e *IndexEntry) PrimaryKey() tuple.Tuple {
	if e.Index == nil {
		return tuple.Tuple{}
	}
	if e.primaryKey == nil {
		e.primaryKey = e.Index.getEntryPrimaryKey(e.Key)
	}
	return e.primaryKey
}

// IndexValues extracts the indexed values portion from the entry key.
// Returns the prefix of Key up to the index expression's column count.
func (e *IndexEntry) IndexValues() tuple.Tuple {
	if e.Index == nil {
		return tuple.Tuple{}
	}
	colSize := e.Index.RootExpression.ColumnSize()
	if colSize <= len(e.Key) {
		return e.Key[:colSize]
	}
	return e.Key
}

// ScanIndex scans a secondary index and returns a cursor over IndexEntry results.
// Returns an error cursor if the index is not in a scannable state (DISABLED or WRITE_ONLY).
// Dispatches to the appropriate maintainer's Scan() method (VALUE vs COUNT).
// Matches Java's FDBRecordStore.scanIndex().
func (store *FDBRecordStore) ScanIndex(
	index *Index,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	startTime := time.Now()
	if !store.IsIndexScannable(index.Name) {
		return &errorCursor[*IndexEntry]{
			err: &IndexNotReadableError{IndexName: index.Name, CurrentState: store.GetIndexState(index.Name)},
		}
	}
	// BITMAP_VALUE indexes must be scanned with BY_GROUP via ScanIndexByType.
	if index.Type == IndexTypeBitmapValue {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("BITMAP_VALUE index %q must be scanned with BY_GROUP scan type", index.Name),
		}
	}
	// TEXT indexes must be scanned with BY_TEXT_TOKEN via ScanIndexByType.
	if index.Type == IndexTypeText {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("TEXT index %q must be scanned with BY_TEXT_TOKEN scan type", index.Name),
		}
	}
	// VECTOR indexes must be scanned with BY_DISTANCE via ScanVectorIndex.
	// Matches Java's VectorIndexMaintainer.scan(IndexScanType, TupleRange, ...) which
	// throws IllegalStateException("index maintainer does not support this scan api").
	if index.Type == IndexTypeVector {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("VECTOR index %q must be scanned with BY_DISTANCE via ScanVectorIndex", index.Name),
		}
	}
	// TIME_WINDOW_LEADERBOARD indexes should use ScanTimeWindowLeaderboard.
	// Standard ScanIndex falls back to all-time BY_VALUE which is acceptable.
	// No rejection here — matches Java's behavior where plain scan() works on all-time.
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	cursor := maintainer.Scan(scanRange, continuation, scanProperties)
	store.context.Timer().RecordSince(EventScanIndex, startTime)
	return cursor
}

// ScanIndexByType scans a secondary index with an explicit scan type.
// For BY_VALUE, delegates to the maintainer's Scan. For BY_RANK, converts rank
// range to score range and scans the B-tree.
// Matches Java's FDBRecordStore.scanIndex(index, scanType, range, ...).
func (store *FDBRecordStore) ScanIndexByType(
	index *Index,
	scanType IndexScanType,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	if !store.IsIndexScannable(index.Name) {
		return &errorCursor[*IndexEntry]{
			err: &IndexNotReadableError{IndexName: index.Name, CurrentState: store.GetIndexState(index.Name)},
		}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	switch scanType {
	case IndexScanByRank:
		switch rm := maintainer.(type) {
		case *rankIndexMaintainer:
			return rm.ScanByRank(scanRange, continuation, scanProperties)
		case *timeWindowLeaderboardIndexMaintainer:
			// BY_RANK on leaderboard: uses all-time leaderboard.
			return rm.ScanByRankInTimeWindow(AllTimeLeaderboardType, 0, scanRange, continuation, scanProperties)
		default:
			return &errorCursor[*IndexEntry]{
				err: fmt.Errorf("index %q (type %s) does not support BY_RANK scan", index.Name, index.Type),
			}
		}
	case IndexScanByTextToken:
		tm, ok := maintainer.(*textIndexMaintainer)
		if !ok {
			return &errorCursor[*IndexEntry]{
				err: fmt.Errorf("index %q (type %s) does not support BY_TEXT_TOKEN scan", index.Name, index.Type),
			}
		}
		return tm.Scan(scanRange, continuation, scanProperties)
	case IndexScanByGroup:
		switch m := maintainer.(type) {
		case *permutedMinMaxIndexMaintainer:
			return m.ScanByGroup(scanRange, continuation, scanProperties)
		case *bitmapValueIndexMaintainer:
			return m.ScanByGroup(scanRange, continuation, scanProperties)
		default:
			return &errorCursor[*IndexEntry]{
				err: fmt.Errorf("index %q (type %s) does not support BY_GROUP scan", index.Name, index.Type),
			}
		}
	case IndexScanByDistance:
		// HNSW and SPFresh share the BY_DISTANCE TupleRange/IndexEntry
		// contract (RFC-094 §10) — dispatch by interface, not concrete type.
		vm, ok := maintainer.(byDistanceScanner)
		if !ok {
			return &errorCursor[*IndexEntry]{
				err: fmt.Errorf("index %q (type %s) does not support BY_DISTANCE scan", index.Name, index.Type),
			}
		}
		return vm.ScanByDistance(scanRange, continuation, scanProperties)
	case IndexScanByDistanceOrderedStream:
		// RFC-156 Phase C: the VBASE distance-ordered STREAMING scan (demand-driven
		// widening + budget-bounded honest truncation). Only SPFresh implements
		// widening; an HNSW ordered scan has no posting cells to widen, so it falls
		// back to the fixed-horizon ScanByDistance (Phase B, unchanged).
		if sm, ok := maintainer.(orderedStreamScanner); ok {
			return sm.ScanByDistanceOrderedStream(scanRange, continuation, scanProperties)
		}
		vm, ok := maintainer.(byDistanceScanner)
		if !ok {
			return &errorCursor[*IndexEntry]{
				err: fmt.Errorf("index %q (type %s) does not support BY_DISTANCE scan", index.Name, index.Type),
			}
		}
		return vm.ScanByDistance(scanRange, continuation, scanProperties)
	default:
		return maintainer.Scan(scanRange, continuation, scanProperties)
	}
}

// ScanTimeWindowLeaderboard scans a TIME_WINDOW_LEADERBOARD index within a specific
// time window. The scanType determines how the range is interpreted:
//   - BY_TIME_WINDOW / BY_VALUE: scanRange contains score bounds
//   - BY_RANK: scanRange contains rank bounds (converted to score bounds via RankedSet)
//
// Matches Java's FDBRecordStore.scanIndex() with TimeWindowScanRange.
func (store *FDBRecordStore) ScanTimeWindowLeaderboard(
	index *Index,
	scanType IndexScanType,
	leaderboardType int,
	leaderboardTimestamp int64,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	if !store.IsIndexScannable(index.Name) {
		return &errorCursor[*IndexEntry]{
			err: &IndexNotReadableError{IndexName: index.Name, CurrentState: store.GetIndexState(index.Name)},
		}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}
	}
	lm, ok := maintainer.(*timeWindowLeaderboardIndexMaintainer)
	if !ok {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("index %q (type %s) is not a TIME_WINDOW_LEADERBOARD", index.Name, index.Type),
		}
	}
	switch scanType {
	case IndexScanByTimeWindow, IndexScanByValue:
		return lm.ScanByTimeWindow(leaderboardType, leaderboardTimestamp, scanRange, continuation, scanProperties)
	case IndexScanByRank:
		return lm.ScanByRankInTimeWindow(leaderboardType, leaderboardTimestamp, scanRange, continuation, scanProperties)
	default:
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("TIME_WINDOW_LEADERBOARD does not support %s scan type", scanType),
		}
	}
}

// indexCursor iterates key-value pairs from an index subspace and maps
// them to IndexEntry objects. This is simpler than keyValueCursor — no
// split record handling, no deserialization. Each FDB KV maps to one IndexEntry.
// Matches Java's KeyValueCursor.map(unpackKeyValue) pattern from StandardIndexMaintainer.
type indexCursor struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.WritableTransaction
	tupleRange    TupleRange
	continuation  []byte
	scanProps     ScanProperties

	iterator     rangeIterator
	closed       bool
	recordsRead  int
	bytesScanned int64
	prefixLength int
	lastCont     []byte
	startTime    time.Time
}

func newIndexCursor(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.WritableTransaction,
	tupleRange TupleRange,
	continuation []byte,
	scanProps ScanProperties,
) *indexCursor {
	return &indexCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    tupleRange,
		continuation:  continuation,
		scanProps:     scanProps,
		prefixLength:  len(indexSubspace.FDBKey()),
		startTime:     time.Now(),
	}
}

// OnNext returns the next IndexEntry or indicates why iteration stopped.
func (c *indexCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("cursor is closed")
	}

	// Honor a statement deadline (RFC-106a): draining an already-fetched range
	// batch must still return on ctx cancellation/timeout, not run to the
	// per-page time limit. context.DeadlineExceeded → 54F01 "statement timeout".
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
	}

	executeProps := c.scanProps.GetExecuteProperties()

	// Check row limit — peek ahead to distinguish ReturnLimitReached vs SourceExhausted
	if executeProps.ReturnedRowLimit > 0 && c.recordsRead >= executeProps.ReturnedRowLimit {
		if c.iterator.Advance() {
			return NewResultNoNext[*IndexEntry](
				ReturnLimitReached,
				&BytesContinuation{bytes: c.lastCont},
			), nil
		}
		// Advance() returns false on exhaustion OR error — check Get() for the stored
		// error so a transient transaction_too_old (1007) / timeout at the row-limit
		// boundary surfaces instead of being read as end-of-data (silent row loss).
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("index scan at row-limit boundary: %w", err)
		}
		return NewResultNoNext[*IndexEntry](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	// Check scanned records limit (free initial pass for first record).
	if executeProps.ScannedRecordsLimit > 0 && c.recordsRead >= executeProps.ScannedRecordsLimit {
		return noNextOrFail[*IndexEntry](executeProps, ScanLimitReached, c.limitContinuation())
	}

	// Check time limit before reading next entry (free initial pass for first record).
	if executeProps.TimeLimit > 0 && c.recordsRead > 0 && time.Since(c.startTime) >= executeProps.TimeLimit {
		return NewResultNoNext[*IndexEntry](
			TimeLimitReached,
			c.limitContinuation(),
		), nil
	}

	// Check byte limit BEFORE reading next entry (matching Java's CursorLimitManager.tryRecordScan).
	// Allow at least one entry (free initial pass).
	if executeProps.ScannedBytesLimit > 0 && c.recordsRead > 0 && c.bytesScanned >= executeProps.ScannedBytesLimit {
		return noNextOrFail[*IndexEntry](executeProps, ByteLimitReached, c.limitContinuation())
	}

	if !c.iterator.Advance() {
		// Advance() returns false on exhaustion OR error — Get() returns the stored
		// error in the error case. Surface it instead of reporting SourceExhausted,
		// which would silently truncate the index scan.
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("index scan: %w", err)
		}
		return NewResultNoNext[*IndexEntry](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("index scan get: %w", err)
	}

	entry, err := c.unpackKeyValue(kv)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	// Accumulate bytes scanned — checked pre-read on next call
	c.bytesScanned += int64(len(kv.Key) + len(kv.Value))

	c.recordsRead++

	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.lastCont = cont

	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *indexCursor) unpackKeyValue(kv fdb.KeyValue) (*IndexEntry, error) {
	// Use fastUnpack instead of tuple.Unpack for the index key.
	// fastUnpack avoids heap allocations on the integer decode path,
	// which matters heavily in tight scan loops (2x speedup on index scans).
	prefixLen := len(c.indexSubspace.Bytes())
	if len(kv.Key) < prefixLen {
		return nil, fmt.Errorf("index key shorter than subspace prefix")
	}
	keyTuple, err := fastUnpack(kv.Key[prefixLen:])
	if err != nil {
		return nil, fmt.Errorf("unpack index key: %w", err)
	}

	var valueTuple tuple.Tuple
	if len(kv.Value) > 0 {
		valueTuple, err = fastUnpack(kv.Value)
		if err != nil {
			return nil, fmt.Errorf("unpack index value: %w", err)
		}
	}

	return &IndexEntry{
		Index: c.index,
		Key:   keyTuple,
		Value: valueTuple,
	}, nil
}

// limitContinuation returns the appropriate continuation when a limit is hit.
func (c *indexCursor) limitContinuation() RecordCursorContinuation {
	if c.lastCont != nil {
		return &BytesContinuation{bytes: c.lastCont}
	}
	return &StartContinuation{}
}

func (c *indexCursor) makeContinuation(key fdb.Key) ([]byte, error) {
	var keySuffix []byte
	if len(key) > c.prefixLength {
		keySuffix = key[c.prefixLength:]
	} else {
		keySuffix = key
	}
	return wrapContinuation(keySuffix)
}

func (c *indexCursor) initIterator() error {
	// Compute begin from TupleRange low endpoint
	var begin fdb.Key
	switch c.tupleRange.LowEndpoint {
	case EndpointTypeTreeStart:
		begin = c.indexSubspace.FDBKey()
	case EndpointTypeRangeInclusive:
		begin = c.indexSubspace.Pack(c.tupleRange.Low)
	case EndpointTypeRangeExclusive:
		packed := c.indexSubspace.Pack(c.tupleRange.Low)
		var err error
		begin, err = fdb.Strinc(packed)
		if err != nil {
			return fmt.Errorf("strinc for exclusive low endpoint: %w", err)
		}
	case EndpointTypePrefixString:
		packed := c.indexSubspace.Pack(c.tupleRange.Low)
		begin = packed[:len(packed)-1]
	default:
		begin = c.indexSubspace.FDBKey()
	}

	// Compute end from TupleRange high endpoint
	var end fdb.Key
	switch c.tupleRange.HighEndpoint {
	case EndpointTypeTreeEnd:
		_, endKey := c.indexSubspace.FDBRangeKeys()
		end = endKey.FDBKey()
	case EndpointTypeRangeInclusive:
		packed := c.indexSubspace.Pack(c.tupleRange.High)
		end = append(packed, 0xFF)
	case EndpointTypeRangeExclusive:
		end = c.indexSubspace.Pack(c.tupleRange.High)
	case EndpointTypePrefixString:
		packed := c.indexSubspace.Pack(c.tupleRange.High)
		stripped := packed[:len(packed)-1]
		newLen := len(stripped)
		for newLen >= 1 && stripped[newLen-1] == 0xFF {
			newLen--
		}
		if newLen == 0 {
			end = fdb.Key{0xFF}
		} else {
			dest := make([]byte, newLen)
			copy(dest, stripped[:newLen])
			dest[newLen-1]++
			end = dest
		}
	default:
		_, endKey := c.indexSubspace.FDBRangeKeys()
		end = endKey.FDBKey()
	}

	// Apply continuation — overrides one endpoint
	if c.continuation != nil {
		innerCont := unwrapContinuation(c.continuation)
		fullKey := append(append(fdb.Key(nil), c.indexSubspace.FDBKey()...), innerCont...)

		if c.scanProps.IsReverse() {
			end = fullKey // exclusive (FDB range end is exclusive)
		} else {
			begin = append(fullKey, 0x00) // start after the continuation key
		}
	}

	rng := fdb.KeyRange{Begin: begin, End: end}
	options := fdb.RangeOptions{
		Mode:    c.scanProps.CursorStreamingMode.ToFDB(),
		Reverse: c.scanProps.IsReverse(),
	}

	// Each index entry is one KV, so FDB-level limit is safe (no split handling needed).
	if c.scanProps.ExecuteProperties.ReturnedRowLimit > 0 {
		limit := c.scanProps.ExecuteProperties.ReturnedRowLimit - c.recordsRead
		if limit <= 0 {
			limit = 1
		}
		options.Limit = saturatingAdd(limit, 1)
	}

	var rangeResult fdb.RangeResult
	if c.scanProps.ExecuteProperties.IsolationLevel == SnapshotIsolation {
		rangeResult = c.tx.Snapshot().GetRange(rng, options)
	} else {
		rangeResult = c.tx.GetRange(rng, options)
	}

	c.iterator = rangeResult.Iterator()
	return nil
}

// Close releases resources held by the cursor.
func (c *indexCursor) Close() error {
	c.closed = true
	return nil
}

func (c *indexCursor) IsClosed() bool { return c.closed }

// FDBIndexedRecord wraps a record that was found via an index scan.
// Contains both the index entry used to locate the record and the record itself.
// Matches Java's com.apple.foundationdb.record.provider.foundationdb.FDBIndexedRecord.
type FDBIndexedRecord struct {
	IndexEntry *IndexEntry
	Record     *FDBStoredRecord[proto.Message]
}

// ScanIndexRecords scans a secondary index and fetches the actual records.
// For each index entry, it loads the record by primary key.
// Orphan index entries (pointing to deleted records) are skipped.
// Matches Java's FDBRecordStore.scanIndexRecords().
func (store *FDBRecordStore) ScanIndexRecords(
	indexName string,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*FDBIndexedRecord] {
	index := store.metaData.GetIndex(indexName)
	if index == nil {
		return &errorCursor[*FDBIndexedRecord]{
			err: &IndexNotFoundError{IndexName: indexName},
		}
	}

	indexCursor := store.ScanIndex(index, scanRange, continuation, scanProperties)
	return &indexRecordCursor{
		inner: indexCursor,
		store: store,
	}
}

// indexRecordCursor maps index entries to stored records by loading each record
// via its primary key. Skips orphan entries (where the record no longer exists).
type indexRecordCursor struct {
	inner RecordCursor[*IndexEntry]
	store *FDBRecordStore
}

func (c *indexRecordCursor) OnNext(ctx context.Context) (RecordCursorResult[*FDBIndexedRecord], error) {
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*FDBIndexedRecord]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return RecordCursorResult[*FDBIndexedRecord]{}, err
		}
		if !result.HasNext() {
			return NewResultNoNext[*FDBIndexedRecord](
				result.GetNoNextReason(),
				result.GetContinuation(),
			), nil
		}

		entry := result.GetValue()
		pk := entry.PrimaryKey()

		rec, err := c.store.LoadRecord(pk)
		if err != nil {
			return RecordCursorResult[*FDBIndexedRecord]{}, fmt.Errorf("load record for index entry %v: %w", pk, err)
		}
		if rec == nil {
			// Orphan index entry — record was deleted but index not cleaned up.
			// Skip it (matches Java's IndexOrphanBehavior.SKIP).
			continue
		}

		return NewResultWithValue(&FDBIndexedRecord{
			IndexEntry: entry,
			Record:     rec,
		}, result.GetContinuation()), nil
	}
}

func (c *indexRecordCursor) Close() error {
	return c.inner.Close()
}

func (c *indexRecordCursor) IsClosed() bool { return c.inner.IsClosed() }

// byDistanceScanner is the BY_DISTANCE access-method contract every vector
// index maintainer implements (RFC-094 §10): Low = (serialized query vector
// [, prefix...]), High = (k [, tuning...]); entries ascend by distance.
// Compile-time assertions catch signature drift at build time (Graefe 094.6).
type byDistanceScanner interface {
	ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
}

// orderedStreamScanner is the RFC-156 Phase C distance-ordered STREAMING scan
// (demand-driven widening + budget-bounded honest truncation). Same TupleRange/
// IndexEntry contract as ScanByDistance; only partition indices with a widenable
// posting structure (SPFresh) implement it — others use the ScanByDistance
// fixed-horizon fallback.
type orderedStreamScanner interface {
	ScanByDistanceOrderedStream(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
}

var (
	_ byDistanceScanner    = (*vectorIndexMaintainer)(nil)
	_ byDistanceScanner    = (*spfreshIndexMaintainer)(nil)
	_ orderedStreamScanner = (*spfreshIndexMaintainer)(nil)
)
