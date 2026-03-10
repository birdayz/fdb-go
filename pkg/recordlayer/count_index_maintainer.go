package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// CountIndexMaintainer handles COUNT index maintenance using FDB atomic ADD.
// The index stores the count of records matching each grouping key.
// Key format: [indexSubspace].pack(groupingTuple)
// Value format: little-endian int64 count
// Matches Java's AtomicMutationIndexMaintainer with COUNT mutation.
type CountIndexMaintainer struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	store         indexStoreContext
}

func newCountIndexMaintainer(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction, store indexStoreContext) *CountIndexMaintainer {
	return &CountIndexMaintainer{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		store:         store,
	}
}

// Update handles insert (old=nil), delete (new=nil), or update (both non-nil).
// For inserts: atomically adds +1 to each grouping key entry.
// For deletes: atomically adds -1 to each grouping key entry.
// For updates: decrements old grouping keys, increments new grouping keys.
// Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys().
func (m *CountIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var oldKeys, newKeys []tuple.Tuple

	if oldRecord != nil {
		entries, err := m.evaluateGroupingKeys(oldRecord)
		if err != nil {
			return fmt.Errorf("evaluate count index %q for old record: %w", m.index.Name, err)
		}
		oldKeys = entries
	}

	if newRecord != nil {
		entries, err := m.evaluateGroupingKeys(newRecord)
		if err != nil {
			return fmt.Errorf("evaluate count index %q for new record: %w", m.index.Name, err)
		}
		newKeys = entries
	}

	// Skip common grouping keys on update — no-op mutations waste transaction bytes.
	// Matches Java's AtomicMutationIndexMaintainer.updateIndexKeys() which filters
	// out keys present in both old and new via commonKeys().
	if oldKeys != nil && newKeys != nil {
		oldKeys, newKeys = removeCommonGroupingKeys(oldKeys, newKeys)
	}

	clearWhenZero := m.index.IsClearWhenZero()

	for _, key := range oldKeys {
		fdbKey := m.indexSubspace.Pack(key)
		m.tx.Add(fdb.Key(fdbKey), littleEndianInt64MinusOne)
		if clearWhenZero {
			m.tx.CompareAndClear(fdb.Key(fdbKey), littleEndianInt64Zero)
		}
	}

	for _, key := range newKeys {
		fdbKey := m.indexSubspace.Pack(key)
		if newRecord != nil {
			if err := checkKeyValueSizes(m.index, newRecord.PrimaryKey, fdbKey, littleEndianInt64One); err != nil {
				return err
			}
		}
		m.tx.Add(fdb.Key(fdbKey), littleEndianInt64One)
	}

	return nil
}

// removeCommonGroupingKeys removes grouping keys present in both old and new.
// This avoids no-op -1/+1 atomic mutations when the grouping key didn't change.
func removeCommonGroupingKeys(old, new []tuple.Tuple) ([]tuple.Tuple, []tuple.Tuple) {
	newSet := make(map[string]struct{}, len(new))
	for _, t := range new {
		newSet[string(t.Pack())] = struct{}{}
	}

	common := make(map[string]struct{})
	var filteredOld []tuple.Tuple
	for _, t := range old {
		p := string(t.Pack())
		if _, ok := newSet[p]; ok {
			common[p] = struct{}{}
		} else {
			filteredOld = append(filteredOld, t)
		}
	}

	var filteredNew []tuple.Tuple
	for _, t := range new {
		if _, ok := common[string(t.Pack())]; !ok {
			filteredNew = append(filteredNew, t)
		}
	}

	return filteredOld, filteredNew
}

// UpdateWhileWriteOnly checks the index build range set before updating.
// COUNT is non-idempotent — blindly updating would cause double-counting.
// Matches Java's StandardIndexMaintainer.updateWriteOnlyByRecords().
func (m *CountIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	return updateWhileWriteOnlyNonIdempotent(oldRecord, newRecord, m.index, m.store, "COUNT", m.Update)
}

// Scan scans count index entries within the given tuple range.
// Returns IndexEntry where Key = grouping tuple and Value = count as tuple.
// Matches Java's StandardIndexMaintainer.scan() with BY_GROUP semantics.
func (m *CountIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// evaluateGroupingKeys extracts the grouping key tuple(s) from a record.
func (m *CountIndexMaintainer) evaluateGroupingKeys(record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	return evaluateGroupingKeys(m.index, record)
}

// countKVCursor scans an aggregate index and returns IndexEntry values.
// By default, decodes values as little-endian int64 (for COUNT/SUM/MIN_EVER_LONG/MAX_EVER_LONG).
// Set tupleValues=true to decode values as tuple-packed bytes (for MIN_EVER_TUPLE/MAX_EVER_TUPLE).
type countKVCursor struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	tupleRange    TupleRange
	continuation  []byte
	scanProps     ScanProperties

	iterator     *fdb.RangeIterator
	closed       bool
	returned     int
	prefixLength int
	lastCont     []byte
	tupleValues  bool // if true, decode values as tuple-packed bytes
}

// newCountIndexCursor creates a cursor that scans a COUNT index.
// Each entry's Value is the count decoded from the little-endian int64 FDB value.
func newCountIndexCursor(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction,
	scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {

	return &countKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    scanRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		prefixLength:  len(indexSubspace.FDBKey()),
	}
}

// newTupleValueIndexCursor creates a cursor that scans an aggregate index with tuple-packed values.
// Each entry's Value is decoded from tuple-packed bytes (for MIN_EVER_TUPLE/MAX_EVER_TUPLE).
func newTupleValueIndexCursor(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction,
	scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {

	return &countKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    scanRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		prefixLength:  len(indexSubspace.FDBKey()),
		tupleValues:   true,
	}
}

func (c *countKVCursor) initIterator() error {
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
	default:
		begin = c.indexSubspace.FDBKey()
	}

	// Compute end from TupleRange high endpoint
	var end fdb.Key
	switch c.tupleRange.HighEndpoint {
	case EndpointTypeTreeEnd:
		_, endKey := c.indexSubspace.FDBRangeKeys()
		end = endKey.(fdb.Key)
	case EndpointTypeRangeInclusive:
		packed := c.indexSubspace.Pack(c.tupleRange.High)
		end = append(packed, 0xFF)
	case EndpointTypeRangeExclusive:
		end = c.indexSubspace.Pack(c.tupleRange.High)
	default:
		_, endKey := c.indexSubspace.FDBRangeKeys()
		end = endKey.(fdb.Key)
	}

	// Apply continuation — overrides one endpoint
	if c.continuation != nil {
		innerCont := unwrapContinuation(c.continuation)
		fullKey := append(append(fdb.Key(nil), c.indexSubspace.FDBKey()...), innerCont...)

		if c.scanProps.IsReverse() {
			end = fullKey
		} else {
			begin = append(fullKey, 0x00)
		}
	}

	rng := fdb.KeyRange{Begin: begin, End: end}
	options := fdb.RangeOptions{
		Reverse: c.scanProps.IsReverse(),
	}

	if c.scanProps.ExecuteProperties.ReturnedRowLimit > 0 {
		options.Limit = c.scanProps.ExecuteProperties.ReturnedRowLimit - c.returned + 1
	}

	c.iterator = c.tx.GetRange(rng, options).Iterator()
	return nil
}

func (c *countKVCursor) OnNext(_ context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("cursor is closed")
	}

	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
	}

	executeProps := c.scanProps.GetExecuteProperties()

	// Check row limit
	if executeProps.ReturnedRowLimit > 0 && c.returned >= executeProps.ReturnedRowLimit {
		if c.iterator.Advance() {
			return NewResultNoNext[*IndexEntry](
				ReturnLimitReached,
				&BytesContinuation{bytes: c.lastCont},
			), nil
		}
		return NewResultNoNext[*IndexEntry](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	if !c.iterator.Advance() {
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}

	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan: %w", err)
	}

	// Unpack key to get grouping tuple
	keyTuple, err := c.indexSubspace.Unpack(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack count index key: %w", err)
	}

	// Decode value based on index type
	var valueTuple tuple.Tuple
	if c.tupleValues {
		// TUPLE variants: decode value as tuple-packed bytes
		if len(kv.Value) > 0 {
			var err2 error
			valueTuple, err2 = tuple.Unpack(kv.Value)
			if err2 != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack tuple value: %w", err2)
			}
		}
	} else {
		// COUNT/SUM/LONG variants: decode value as little-endian int64
		count := int64(0)
		if len(kv.Value) >= 8 {
			count = int64(binary.LittleEndian.Uint64(kv.Value))
		}
		valueTuple = tuple.Tuple{count}
	}

	entry := &IndexEntry{
		Index: c.index,
		Key:   keyTuple,
		Value: valueTuple,
	}

	c.returned++

	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.lastCont = cont

	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *countKVCursor) makeContinuation(key fdb.Key) ([]byte, error) {
	var keySuffix []byte
	if len(key) > c.prefixLength {
		keySuffix = key[c.prefixLength:]
	} else {
		keySuffix = key
	}
	return wrapContinuation(keySuffix)
}

func (c *countKVCursor) Close() error {
	c.closed = true
	return nil
}
