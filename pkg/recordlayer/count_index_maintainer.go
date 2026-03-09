package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"
	"iter"

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

	for _, key := range oldKeys {
		fdbKey := m.indexSubspace.Pack(key)
		m.tx.Add(fdb.Key(fdbKey), littleEndianInt64MinusOne)
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

// UpdateWhileWriteOnly for COUNT indexes checks the index build range set
// before updating. COUNT is non-idempotent — blindly updating would cause
// double-counting when the online indexer has already processed the record.
// Only updates if the record's primary key is in the already-built range.
// If not in range, the online indexer will handle it when it gets there.
// Matches Java's StandardIndexMaintainer.updateWriteOnlyByRecords().
func (m *CountIndexMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
	var primaryKey tuple.Tuple
	if oldRecord != nil {
		primaryKey = oldRecord.PrimaryKey
	} else if newRecord != nil {
		primaryKey = newRecord.PrimaryKey
	} else {
		return nil
	}

	if m.store == nil {
		// No store context — fall back to direct update.
		return m.Update(oldRecord, newRecord)
	}

	inRange, err := m.store.isKeyInIndexBuildRange(m.index, primaryKey)
	if err != nil {
		return fmt.Errorf("check index build range for COUNT index %q: %w", m.index.Name, err)
	}

	if !inRange {
		return nil // Not yet built — the online indexer will handle it.
	}

	return m.Update(oldRecord, newRecord)
}

// Scan scans count index entries within the given tuple range.
// Returns IndexEntry where Key = grouping tuple and Value = count as tuple.
// Matches Java's StandardIndexMaintainer.scan() with BY_GROUP semantics.
func (m *CountIndexMaintainer) Scan(scanRange TupleRange, continuation []byte, scanProperties ScanProperties) RecordCursor[*IndexEntry] {
	return newCountIndexCursor(m.index, m.indexSubspace, m.tx, scanRange, continuation, scanProperties)
}

// evaluateGroupingKeys extracts the grouping key tuple(s) from a record.
// For a GroupingKeyExpression, takes only the leading grouping columns.
// For other expressions, uses all columns as the grouping key.
func (m *CountIndexMaintainer) evaluateGroupingKeys(record *FDBStoredRecord[proto.Message]) ([]tuple.Tuple, error) {
	// Check predicate
	if m.index.Predicate != nil && !m.index.Predicate(record.Record) {
		return nil, nil
	}

	tuples, err := m.index.RootExpression.Evaluate(record.Record)
	if err != nil {
		return nil, err
	}

	groupingCount := m.getGroupingCount()
	result := make([]tuple.Tuple, 0, len(tuples))
	for _, values := range tuples {
		// Take only the grouping columns (leading)
		groupKey := make(tuple.Tuple, groupingCount)
		for j := 0; j < groupingCount && j < len(values); j++ {
			groupKey[j] = values[j]
		}
		result = append(result, groupKey)
	}
	return result, nil
}

// getGroupingCount returns the number of grouping columns in the index expression.
func (m *CountIndexMaintainer) getGroupingCount() int {
	if g, ok := m.index.RootExpression.(*GroupingKeyExpression); ok {
		return g.GetGroupingCount()
	}
	// If not a GroupingKeyExpression, all columns are grouping columns
	return keyExpressionColumnSize(m.index.RootExpression)
}

// countKVCursor scans a COUNT index and returns IndexEntry with count values.
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

	// Decode value as little-endian int64 count
	count := int64(0)
	if len(kv.Value) >= 8 {
		count = int64(binary.LittleEndian.Uint64(kv.Value))
	}

	entry := &IndexEntry{
		Index: c.index,
		Key:   keyTuple,
		Value: tuple.Tuple{count},
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

func (c *countKVCursor) Seq(ctx context.Context) iter.Seq[*IndexEntry] {
	return func(yield func(*IndexEntry) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

func (c *countKVCursor) Seq2(ctx context.Context) iter.Seq2[*IndexEntry, error] {
	return func(yield func(*IndexEntry, error) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				yield(nil, err)
				return
			}
			if !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), nil) {
				return
			}
		}
	}
}

func (c *countKVCursor) SeqWithContinuation(ctx context.Context) iter.Seq2[*IndexEntry, RecordCursorContinuation] {
	return func(yield func(*IndexEntry, RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		for {
			result, err := c.OnNext(ctx)
			if err != nil || !result.HasNext() {
				return
			}
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}
