package recordlayer

import (
	"context"
	"fmt"
	"iter"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// IndexScanType identifies the type of index scan.
// Matches Java's com.apple.foundationdb.record.IndexScanType.
type IndexScanType string

const (
	// IndexScanByValue scans a VALUE index by its indexed values.
	IndexScanByValue IndexScanType = "BY_VALUE"
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

// IndexEntry represents a single entry from an index scan.
// Matches Java's com.apple.foundationdb.record.IndexEntry.
type IndexEntry struct {
	Index *Index
	Key   tuple.Tuple // Full key (indexed values + primary key components)
	Value tuple.Tuple // Value tuple (empty for VALUE indexes)

	primaryKey tuple.Tuple // Lazily extracted
}

// PrimaryKey extracts the primary key portion from the index entry key.
// Index entries store keys as [indexValues..., primaryKeyValues...].
// Matches Java's IndexEntry.getPrimaryKey().
func (e *IndexEntry) PrimaryKey() tuple.Tuple {
	if e.primaryKey == nil {
		colSize := keyExpressionColumnSize(e.Index.RootExpression)
		if colSize < len(e.Key) {
			e.primaryKey = e.Key[colSize:]
		} else {
			e.primaryKey = tuple.Tuple{}
		}
	}
	return e.primaryKey
}

// IndexValues extracts the indexed values portion from the entry key.
// Returns the prefix of Key up to the index expression's column count.
func (e *IndexEntry) IndexValues() tuple.Tuple {
	colSize := keyExpressionColumnSize(e.Index.RootExpression)
	if colSize <= len(e.Key) {
		return e.Key[:colSize]
	}
	return e.Key
}

// keyExpressionColumnSize returns the number of tuple elements a key expression
// produces. Used to split index entry keys into indexed values and primary key.
// Matches Java's KeyExpression.getColumnSize().
func keyExpressionColumnSize(expr KeyExpression) int {
	switch e := expr.(type) {
	case *FieldKeyExpression:
		return 1
	case *CompositeKeyExpression:
		total := 0
		for _, child := range e.expressions {
			total += keyExpressionColumnSize(child)
		}
		return total
	case *RecordTypeKeyExpression:
		if e.nested != nil {
			return 1 + keyExpressionColumnSize(e.nested)
		}
		return 1
	case *NestingKeyExpression:
		// NestingKeyExpression column size is the child's column size (parent message
		// field doesn't contribute a tuple element). Matches Java's getColumnSize().
		return keyExpressionColumnSize(e.child)
	case *EmptyKeyExpression:
		return 0
	default:
		return 0
	}
}

// ScanIndex scans a secondary index and returns a cursor over IndexEntry results.
// Returns an error cursor if the index is not in a scannable state (DISABLED or WRITE_ONLY).
// Matches Java's FDBRecordStore.scanIndex().
func (store *FDBRecordStore) ScanIndex(
	index *Index,
	scanRange TupleRange,
	continuation []byte,
	scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	if !store.IsIndexScannable(index.Name) {
		return &errorCursor[*IndexEntry]{
			err: fmt.Errorf("%w: %s is %s", ErrIndexNotReadable, index.Name, store.GetIndexState(index.Name)),
		}
	}
	idxSubspace := store.indexSubspace(index)
	return newIndexCursor(index, idxSubspace, store.context.Transaction(), scanRange, continuation, scanProperties)
}

// indexCursor iterates key-value pairs from an index subspace and maps
// them to IndexEntry objects. This is simpler than keyValueCursor — no
// split record handling, no deserialization. Each FDB KV maps to one IndexEntry.
// Matches Java's KeyValueCursor.map(unpackKeyValue) pattern from StandardIndexMaintainer.
type indexCursor struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	tupleRange    TupleRange
	continuation  []byte
	scanProps     ScanProperties

	iterator     *fdb.RangeIterator
	closed       bool
	recordsRead  int
	bytesScanned int64
	prefixLength int
	lastCont     []byte
}

func newIndexCursor(
	index *Index,
	indexSubspace subspace.Subspace,
	tx fdb.Transaction,
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
	}
}

// OnNext returns the next IndexEntry or indicates why iteration stopped.
func (c *indexCursor) OnNext(_ context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("cursor is closed")
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
		return NewResultNoNext[*IndexEntry](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	if !c.iterator.Advance() {
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

	c.bytesScanned += int64(len(kv.Key) + len(kv.Value))

	// Check byte limit
	if executeProps.ScannedBytesLimit > 0 && c.bytesScanned > executeProps.ScannedBytesLimit {
		cont, wrapErr := c.makeContinuation(kv.Key)
		if wrapErr != nil {
			return RecordCursorResult[*IndexEntry]{}, wrapErr
		}
		return NewResultNoNext[*IndexEntry](
			ByteLimitReached,
			&BytesContinuation{bytes: cont},
		), nil
	}

	c.recordsRead++

	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.lastCont = cont

	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *indexCursor) unpackKeyValue(kv fdb.KeyValue) (*IndexEntry, error) {
	keyTuple, err := c.indexSubspace.Unpack(kv.Key)
	if err != nil {
		return nil, fmt.Errorf("unpack index key: %w", err)
	}

	var valueTuple tuple.Tuple
	if len(kv.Value) > 0 {
		valueTuple, err = tuple.Unpack(kv.Value)
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
		options.Limit = c.scanProps.ExecuteProperties.ReturnedRowLimit - c.recordsRead + 1
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

// Seq returns an iterator sequence over IndexEntry values.
func (c *indexCursor) Seq(ctx context.Context) iter.Seq[*IndexEntry] {
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

// Seq2 returns an iterator sequence over (IndexEntry, error) pairs.
func (c *indexCursor) Seq2(ctx context.Context) iter.Seq2[*IndexEntry, error] {
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

// SeqWithContinuation returns an iterator sequence over (IndexEntry, continuation) pairs.
func (c *indexCursor) SeqWithContinuation(ctx context.Context) iter.Seq2[*IndexEntry, RecordCursorContinuation] {
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
