package recordlayer

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Default and maximum bitmap entry sizes matching Java's BitmapValueIndexMaintainer.
const (
	bitmapValueDefaultEntrySize = 10000
	bitmapValueMaxEntrySize     = 250000
)

// bitmapValueIndexMaintainer maintains a BITMAP_VALUE index.
//
// Instead of one FDB key per record, it stores one BIT per record in a
// fixed-size bitmap. The position field (last grouped column) is an integer.
// Bitmaps are aligned on multiples of entrySize.
//
// FDB Key: indexSubspace.Pack(groupKey..., alignedPosition)
// FDB Value: raw byte array of size (entrySize+7)/8 with the bit set.
//
// Uses FDB atomic BitOr (insert) and BitAnd+CompareAndClear (delete).
//
// Matches Java's BitmapValueIndexMaintainer.
type bitmapValueIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
	entrySize     int64
	unique        bool
}

func newBitmapValueIndexMaintainer(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
	store indexStoreContext,
) *bitmapValueIndexMaintainer {
	entrySize := int64(bitmapValueDefaultEntrySize)
	if v, ok := index.Options[IndexOptionBitmapValueEntrySize]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= bitmapValueMaxEntrySize {
			entrySize = n
		}
	}
	return &bitmapValueIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
		entrySize:     entrySize,
		unique:        index.IsUnique(),
	}
}

// floorMod computes the floor modulus matching Java's Math.floorMod.
// Go's % operator truncates toward zero; this always returns a non-negative result.
func floorMod(x, y int64) int64 {
	return ((x % y) + y) % y
}

// bitmapByteSize returns the byte count for a bitmap with the given entry size.
func bitmapByteSize(entrySize int64) int {
	return int((entrySize + 7) / 8)
}

// evaluateIndex evaluates the index expression to produce index entries.
// Reuses the standard evaluateIndex from standardIndexMaintainer.
func (m *bitmapValueIndexMaintainer) evaluateIndex(record *FDBStoredRecord[proto.Message]) ([]indexEntry, error) {
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}
	tuples, err := m.index.RootExpression.Evaluate(record, record.Record)
	if err != nil {
		return nil, err
	}
	entries := make([]indexEntry, len(tuples))
	for i, values := range tuples {
		key := make(tuple.Tuple, len(values))
		for j, v := range values {
			key[j] = v
		}
		entries[i] = indexEntry{key: key, primaryKey: record.PrimaryKey}
	}
	return entries, nil
}

// groupPrefixSize returns the number of leading grouping (GROUP BY) columns.
func (m *bitmapValueIndexMaintainer) groupPrefixSize() int {
	return indexGroupingCount(m.index.RootExpression)
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// Matches Java's BitmapValueIndexMaintainer.update().
func (m *bitmapValueIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var oldEntries, newEntries []indexEntry

	if oldRecord != nil {
		entries, err := m.evaluateIndex(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate bitmap_value index %q for old record: %w", m.index.Name, err)
		}
		oldEntries = entries
	}

	if newRecord != nil {
		entries, err := m.evaluateIndex(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate bitmap_value index %q for new record: %w", m.index.Name, err)
		}
		newEntries = entries
	}

	// Skip unchanged entries.
	if oldEntries != nil && newEntries != nil {
		var err error
		oldEntries, newEntries, err = removeCommonEntries(m.index, oldEntries, newEntries)
		if err != nil {
			return err
		}
	}

	isWriteOnly := m.store != nil && m.store.isIndexWriteOnly(m.index)

	// Remove old entries.
	for _, e := range oldEntries {
		if err := m.removeEntry(e, isWriteOnly); err != nil {
			return err
		}
	}

	// Add new entries.
	for _, e := range newEntries {
		if err := m.addEntry(e); err != nil {
			return err
		}
	}

	return nil
}

// addEntry sets a bit in the bitmap for the given index entry.
func (m *bitmapValueIndexMaintainer) addEntry(entry indexEntry) error {
	groupSize := m.groupPrefixSize()
	if groupSize >= len(entry.key) {
		// No position column — skip.
		return nil
	}

	groupKey := entry.key[:groupSize]
	positionRaw := entry.key[groupSize]
	if positionRaw == nil {
		return nil
	}

	position, err := toInt64(positionRaw)
	if err != nil {
		return fmt.Errorf("bitmap_value index %q: position column: %w", m.index.Name, err)
	}

	offset := floorMod(position, m.entrySize)
	alignedPos := position - offset

	fdbTupleKey := make(tuple.Tuple, 0, len(groupKey)+1)
	fdbTupleKey = append(fdbTupleKey, groupKey...)
	fdbTupleKey = append(fdbTupleKey, alignedPos)
	fdbKey := m.indexSubspace.Pack(fdbTupleKey)
	byteSize := bitmapByteSize(m.entrySize)

	if m.unique {
		if err := m.checkBitmapUniqueness(fdbKey, offset, entry); err != nil {
			return err
		}
	}

	bitmap := make([]byte, byteSize)
	bitmap[offset/8] |= 1 << (offset % 8)
	m.tx.BitOr(fdb.Key(fdbKey), bitmap)

	return nil
}

// removeEntry clears a bit in the bitmap for the given index entry.
func (m *bitmapValueIndexMaintainer) removeEntry(entry indexEntry, isWriteOnly bool) error {
	groupSize := m.groupPrefixSize()
	if groupSize >= len(entry.key) {
		return nil
	}

	positionRaw := entry.key[groupSize]
	if positionRaw == nil {
		return nil
	}

	groupKey := entry.key[:groupSize]
	position, err := toInt64(positionRaw)
	if err != nil {
		return fmt.Errorf("bitmap_value index %q: position column: %w", m.index.Name, err)
	}

	offset := floorMod(position, m.entrySize)
	alignedPos := position - offset

	fdbTupleKey := make(tuple.Tuple, 0, len(groupKey)+1)
	fdbTupleKey = append(fdbTupleKey, groupKey...)
	fdbTupleKey = append(fdbTupleKey, alignedPos)
	fdbKey := fdb.Key(m.indexSubspace.Pack(fdbTupleKey))
	byteSize := bitmapByteSize(m.entrySize)

	// In WRITE_ONLY mode, ensure the key exists before BIT_AND.
	// Matches Java's BitmapValueIndexMaintainer behavior.
	if isWriteOnly {
		zeroBitmap := make([]byte, byteSize)
		m.tx.BitOr(fdbKey, zeroBitmap)
	}

	// Create mask with all bits set except the one to clear.
	mask := make([]byte, byteSize)
	for i := range mask {
		mask[i] = 0xFF
	}
	mask[offset/8] &= ^(1 << (offset % 8))
	m.tx.BitAnd(fdbKey, mask)

	// Remove entry if all zeros.
	zeroBitmap := make([]byte, byteSize)
	m.tx.CompareAndClear(fdbKey, zeroBitmap)

	return nil
}

// checkBitmapUniqueness checks that the bit position is not already set.
// Uses snapshot read + read/write conflict keys for isolation.
// Matches Java's BitmapValueIndexMaintainer.checkUniqueness().
func (m *bitmapValueIndexMaintainer) checkBitmapUniqueness(fdbKey []byte, offset int64, entry indexEntry) error {
	existing, err := m.tx.Snapshot().Get(fdb.Key(fdbKey)).Get()
	if err != nil {
		return fmt.Errorf("bitmap_value index %q uniqueness check: %w", m.index.Name, err)
	}

	if existing != nil {
		byteIdx := offset / 8
		if int(byteIdx) < len(existing) && (existing[byteIdx]&(1<<(offset%8))) != 0 {
			// Bit is already set — uniqueness violation.
			return &RecordIndexUniquenessViolationError{
				IndexName:  m.index.Name,
				IndexKey:   entry.key,
				PrimaryKey: entry.primaryKey,
			}
		}
	}

	// Add read/write conflict key at Subspace(fdbKey).Pack(offset) for isolation.
	conflictKey := fdb.Key(subspace.FromBytes(fdbKey).Pack(tuple.Tuple{offset}))
	if err := m.tx.AddReadConflictKey(conflictKey); err != nil {
		return fmt.Errorf("bitmap_value index %q: add read conflict key: %w", m.index.Name, err)
	}
	if err := m.tx.AddWriteConflictKey(conflictKey); err != nil {
		return fmt.Errorf("bitmap_value index %q: add write conflict key: %w", m.index.Name, err)
	}

	return nil
}

// UpdateWhileWriteOnly updates the index during WRITE_ONLY state.
// BIT_OR and BIT_AND are idempotent, so this is a pass-through to Update().
// Matches Java's BitmapValueIndexMaintainer — bitmap atomics are idempotent.
func (m *bitmapValueIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

// Scan scans the bitmap index entries.
// For BITMAP_VALUE, the standard Scan returns raw bitmap entries via bitmapKVCursor.
// Matches Java's BitmapValueIndexMaintainer.scan().
func (m *bitmapValueIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newBitmapKVCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties, m.entrySize)
}

// ScanByGroup is the primary scan method for BITMAP_VALUE indexes.
// It scans the index and returns entries with raw bitmap bytes as values.
// The scan range specifies group key bounds and optionally position bounds.
//
// When the range specifies position bounds that don't align to entrySize,
// the scan adjusts the FDB range to aligned boundaries and trims the returned
// bitmaps. Empty bitmaps after trimming are filtered out.
//
// Matches Java's BitmapValueIndexMaintainer.scan(IndexScanType.BY_GROUP, ...).
func (m *bitmapValueIndexMaintainer) ScanByGroup(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	groupSize := m.groupPrefixSize()
	adjustedRange := scanRange

	// Extract requested position bounds for trimming.
	startPosition := int64(math.MinInt64)
	if adjustedRange.Low != nil && len(adjustedRange.Low) > groupSize && adjustedRange.Low[groupSize] != nil {
		pos, err := toInt64(adjustedRange.Low[groupSize])
		if err == nil {
			if adjustedRange.LowEndpoint == EndpointTypeRangeExclusive {
				startPosition = pos + 1
			} else {
				startPosition = pos
			}
			// Adjust low to aligned boundary.
			if floorMod(startPosition, m.entrySize) != 0 {
				aligned := startPosition - floorMod(startPosition, m.entrySize)
				newLow := make(tuple.Tuple, groupSize+1)
				copy(newLow, adjustedRange.Low[:groupSize])
				newLow[groupSize] = aligned
				adjustedRange = TupleRange{
					Low:          newLow,
					High:         adjustedRange.High,
					LowEndpoint:  EndpointTypeRangeInclusive,
					HighEndpoint: adjustedRange.HighEndpoint,
				}
			}
		}
	}

	endPosition := int64(math.MaxInt64)
	if adjustedRange.High != nil && len(adjustedRange.High) > groupSize && adjustedRange.High[groupSize] != nil {
		pos, err := toInt64(adjustedRange.High[groupSize])
		if err == nil {
			if adjustedRange.HighEndpoint == EndpointTypeRangeInclusive {
				endPosition = pos + 1
			} else {
				endPosition = pos
			}
			// Adjust high to aligned boundary.
			if floorMod(endPosition, m.entrySize) != 0 {
				aligned := endPosition + floorMod(m.entrySize-endPosition, m.entrySize)
				newHigh := make(tuple.Tuple, groupSize+1)
				copy(newHigh, adjustedRange.High[:groupSize])
				newHigh[groupSize] = aligned
				adjustedRange = TupleRange{
					Low:          adjustedRange.Low,
					High:         newHigh,
					LowEndpoint:  adjustedRange.LowEndpoint,
					HighEndpoint: EndpointTypeRangeInclusive,
				}
			}
		}
	}

	cursor := m.Scan(adjustedRange, continuation, scanProperties)

	// If no position trimming needed, return raw cursor.
	if startPosition == int64(math.MinInt64) && endPosition == int64(math.MaxInt64) {
		return cursor
	}

	// Map entries through position trimming, filter out empty bitmaps.
	sp := startPosition
	ep := endPosition
	gs := groupSize

	return &filterCursor[*IndexEntry]{
		inner: MapErrCursor(cursor, func(entry *IndexEntry) (*IndexEntry, error) {
			if gs >= len(entry.Key) || len(entry.Value) == 0 {
				return entry, nil
			}
			entryStart, err := toInt64(entry.Key[gs])
			if err != nil {
				return entry, nil
			}
			bitmapBytes, ok := entry.Value[0].([]byte)
			if !ok {
				return entry, nil
			}
			entryEnd := entryStart + int64(len(bitmapBytes))*8

			// Check if trimming is needed.
			if entryStart >= sp && entryEnd <= ep {
				return entry, nil
			}

			trimmedStart := max64(entryStart, sp)
			trimmedEnd := min64(entryEnd, ep)
			if trimmedStart >= trimmedEnd {
				return nil, nil // Empty after trim — will be filtered.
			}

			trimmedBitmap := make([]byte, (trimmedEnd-trimmedStart+7)/8)
			for i := trimmedStart; i < trimmedEnd; i++ {
				srcOffset := int(i - entryStart)
				if (bitmapBytes[srcOffset/8] & (1 << (srcOffset % 8))) != 0 {
					dstOffset := int(i - trimmedStart)
					trimmedBitmap[dstOffset/8] |= 1 << (dstOffset % 8)
				}
			}

			trimmedKey := make(tuple.Tuple, len(entry.Key))
			copy(trimmedKey, entry.Key)
			trimmedKey[gs] = trimmedStart

			return &IndexEntry{
				Index: entry.Index,
				Key:   trimmedKey,
				Value: tuple.Tuple{trimmedBitmap},
			}, nil
		}),
		predicate: func(entry *IndexEntry) bool {
			return entry != nil
		},
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// DeleteWhere clears all bitmap index entries whose key starts with the given prefix.
// Matches Java's BitmapValueIndexMaintainer.deleteWhere().
func (m *bitmapValueIndexMaintainer) DeleteWhere(prefix tuple.Tuple) error {
	return deleteWhereRange(m.tx, m.indexSubspace, prefix)
}

// bitmapKVCursor scans a BITMAP_VALUE index and returns IndexEntry values.
// Values are raw bitmap bytes wrapped as tuple.Tuple{[]byte{...}}.
// Unlike the standard indexCursor, it does NOT tuple.Unpack the FDB value.
type bitmapKVCursor struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	tupleRange    TupleRange
	continuation  []byte
	scanProps     ScanProperties
	entrySize     int64

	iterator     rangeIterator
	closed       bool
	recordsRead  int
	bytesScanned int64
	prefixLength int
	lastCont     []byte
	startTime    time.Time
}

func newBitmapKVCursor(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
	tupleRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
	entrySize int64,
) *bitmapKVCursor {
	return &bitmapKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    tupleRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		entrySize:     entrySize,
		prefixLength:  len(indexSubspace.FDBKey()),
		startTime:     time.Now(),
	}
}

func (c *bitmapKVCursor) initIterator() error {
	rng := c.tupleRange.ToFDBRange(c.indexSubspace)

	// Apply continuation — overrides one endpoint.
	if c.continuation != nil {
		innerCont := unwrapContinuation(c.continuation)
		fullKey := append(append(fdb.Key(nil), c.indexSubspace.FDBKey()...), innerCont...)

		if c.scanProps.IsReverse() {
			rng.End = fullKey
		} else {
			rng.Begin = append(fullKey, 0x00)
		}
	}

	options := fdb.RangeOptions{
		Mode:    c.scanProps.CursorStreamingMode.ToFDB(),
		Reverse: c.scanProps.IsReverse(),
	}

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

func (c *bitmapKVCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("cursor is closed")
	}

	// Honor a statement deadline / cancellation (RFC-106a).
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
	}

	executeProps := c.scanProps.GetExecuteProperties()

	// Check row limit FIRST so a MAX_ROWS/LIMIT-bounded scan stops cleanly with
	// ReturnLimitReached before the scan-record backstop can fire (codex RFC-106a:
	// match index_scan ordering).
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
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("bitmap index scan at row-limit boundary: %w", err)
		}
		return NewResultNoNext[*IndexEntry](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	// Check scanned-records limit (RFC-106a parity with the other leaf cursors —
	// index_scan/record_key honor ScannedRecordsLimit; bitmap omitted it).
	if executeProps.ScannedRecordsLimit > 0 && c.recordsRead >= executeProps.ScannedRecordsLimit {
		return noNextOrFail[*IndexEntry](executeProps, ScanLimitReached, c.limitContinuation())
	}

	// Check time limit.
	if executeProps.TimeLimit > 0 && c.recordsRead > 0 && time.Since(c.startTime) >= executeProps.TimeLimit {
		return NewResultNoNext[*IndexEntry](
			TimeLimitReached,
			c.limitContinuation(),
		), nil
	}

	// Check byte limit.
	if executeProps.ScannedBytesLimit > 0 && c.recordsRead > 0 && c.bytesScanned >= executeProps.ScannedBytesLimit {
		return noNextOrFail[*IndexEntry](executeProps, ByteLimitReached, c.limitContinuation())
	}

	if !c.iterator.Advance() {
		// Advance() returns false on exhaustion OR error — surface the stored Get()
		// error rather than reporting SourceExhausted (silent row loss).
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("bitmap index scan: %w", err)
		}
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}

	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("bitmap index scan: %w", err)
	}

	keyTuple, err := fastSubspaceUnpack(kv.Key, len(c.indexSubspace.Bytes()))
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack bitmap index key: %w", err)
	}

	// Value is raw bitmap bytes, NOT tuple-packed. Wrap as tuple.Tuple{[]byte{...}}.
	entry := &IndexEntry{
		Index: c.index,
		Key:   keyTuple,
		Value: tuple.Tuple{kv.Value},
	}

	c.bytesScanned += int64(len(kv.Key) + len(kv.Value))
	c.recordsRead++

	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.lastCont = cont

	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *bitmapKVCursor) limitContinuation() RecordCursorContinuation {
	if c.lastCont != nil {
		return &BytesContinuation{bytes: c.lastCont}
	}
	return &StartContinuation{}
}

func (c *bitmapKVCursor) makeContinuation(key fdb.Key) ([]byte, error) {
	var keySuffix []byte
	if len(key) > c.prefixLength {
		keySuffix = key[c.prefixLength:]
	} else {
		keySuffix = key
	}
	return wrapContinuation(keySuffix)
}

func (c *bitmapKVCursor) Close() error {
	c.closed = true
	return nil
}

func (c *bitmapKVCursor) IsClosed() bool { return c.closed }

// evaluateBitmapValueAggregate accumulates bitmap entries across aligned positions
// into a single combined bitmap. Returns the combined bitmap as tuple.Tuple{[]byte{...}}.
//
// Each entry's position MUST be on an even byte boundary (position % 8 == 0) for the
// simple copy to work. This matches Java's BitmapAggregator which checks position % 8.
//
// Matches Java's BitmapValueIndexMaintainer.evaluateAggregateFunction().
func evaluateBitmapValueAggregate(
	ctx context.Context,
	m *bitmapValueIndexMaintainer,
	scanRange TupleRange,
	isolationLevel IsolationLevel,
) (tuple.Tuple, error) {
	props := ScanProperties{
		ExecuteProperties: ExecuteProperties{
			IsolationLevel: isolationLevel,
		},
	}

	cursor := m.ScanByGroup(scanRange, nil, props)
	defer func() { _ = cursor.Close() }()

	groupSize := m.groupPrefixSize()

	// Determine initial offset from range.
	var startPosition int64
	if scanRange.Low != nil && len(scanRange.Low) > groupSize {
		pos, err := toInt64(scanRange.Low[groupSize])
		if err == nil {
			startPosition = pos
		}
	}

	// Initial buffer size — Java allocates entrySize bytes (not bits/8).
	// This matches Java's BitmapAggregator(offset, size) which does
	// ByteBuffer.allocate(size) where size = entrySize.
	bufSize := int(m.entrySize)
	if scanRange.High != nil && len(scanRange.High) > groupSize {
		endPos, err := toInt64(scanRange.High[groupSize])
		if err == nil && endPos > startPosition {
			sz := int(endPos - startPosition)
			if sz < bufSize {
				// Narrow size to what can actually be passed through from scan.
				bufSize = sz
			}
		}
	}
	buf := make([]byte, bufSize)
	wrote := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, fmt.Errorf("evaluate bitmap_value aggregate: %w", err)
		}
		if !r.HasNext() {
			break
		}
		entry := r.GetValue()

		if groupSize >= len(entry.Key) || len(entry.Value) == 0 {
			continue
		}

		pos, err := toInt64(entry.Key[groupSize])
		if err != nil {
			continue
		}
		bitmapBytes, ok := entry.Value[0].([]byte)
		if !ok || len(bitmapBytes) == 0 {
			continue
		}

		relativePos := pos - startPosition
		if relativePos < 0 {
			return nil, fmt.Errorf("for negative positions, must specify negative range start")
		}
		if relativePos%8 != 0 {
			return nil, fmt.Errorf("position must be on even byte boundary")
		}
		bytePosition := int(relativePos / 8)

		// Grow buffer if needed.
		needed := bytePosition + len(bitmapBytes)
		if needed > len(buf) {
			newBuf := make([]byte, needed)
			copy(newBuf, buf)
			buf = newBuf
		}

		// Copy bitmap bytes into result at the correct byte position.
		for i, b := range bitmapBytes {
			buf[bytePosition+i] |= b
		}
		wrote = true
	}

	if !wrote {
		return tuple.Tuple{buf}, nil // Return zero-filled buffer matching Java
	}

	return tuple.Tuple{buf}, nil
}

var _ IndexMaintainer = (*bitmapValueIndexMaintainer)(nil)
