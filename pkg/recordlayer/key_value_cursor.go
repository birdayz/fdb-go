package recordlayer

import (
	"context"
	"fmt"
	"iter"
	
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// keyValueCursor implements RecordCursor for scanning key-value pairs from FDB
type keyValueCursor struct {
	store          *FDBRecordStore
	low            tuple.Tuple
	high           tuple.Tuple
	lowEndpoint    EndpointType
	highEndpoint   EndpointType
	continuation   []byte
	scanProperties ScanProperties

	// Internal state
	iterator      *fdb.RangeIterator
	closed        bool
	recordsRead   int
	bytesScanned  int64
	prefixLength  int  // Length of the subspace prefix for continuation handling
}

// OnNext returns the next record or indicates why iteration stopped
func (c *keyValueCursor) OnNext(ctx context.Context) (RecordCursorResult[*FDBStoredRecord[proto.Message]], error) {
	if c.closed {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, fmt.Errorf("cursor is closed")
	}
	
	// Initialize iterator on first call
	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
	}
	
	// Check limits before fetching next record
	executeProps := c.scanProperties.GetExecuteProperties()
	if executeProps.ReturnedRowLimit > 0 && c.recordsRead >= executeProps.ReturnedRowLimit {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			ReturnLimitReached,
			&BytesContinuation{bytes: c.continuation},
		), nil
	}
	
	// Advance iterator
	if !c.iterator.Advance() {
		// No more records
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}
	
	// Get the key-value pair
	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, fmt.Errorf("failed to get key-value: %w", err)
	}
	
	// Update scan metrics
	c.bytesScanned += int64(len(kv.Key) + len(kv.Value))
	
	// Check byte limit
	if executeProps.ScannedBytesLimit > 0 && c.bytesScanned > executeProps.ScannedBytesLimit {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			ByteLimitReached,
			&BytesContinuation{bytes: kv.Key},
		), nil
	}
	
	// Deserialize the record
	record, err := c.deserializeKeyValue(kv)
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	
	c.recordsRead++
	
	// Create continuation token with only the key suffix (Java compatibility)
	// Java expects: key[prefixLength:] not the full key
	var continuationBytes []byte
	if len(kv.Key) > c.prefixLength {
		continuationBytes = kv.Key[c.prefixLength:]
	} else {
		// Should not happen, but handle gracefully
		continuationBytes = kv.Key
	}
	
	// Return the record with continuation
	return NewResultWithValue(
		record,
		&BytesContinuation{bytes: continuationBytes},
	), nil
}

// initIterator sets up the FDB range iterator
func (c *keyValueCursor) initIterator() error {
	recordsSubspace := c.store.subspace.Sub(RecordKey)
	
	// Build range based on endpoints
	var begin, end fdb.Key
	
	// Handle low endpoint
	switch c.lowEndpoint {
	case EndpointTypeTreeStart:
		begin = recordsSubspace.FDBKey()
	case EndpointTypeRangeInclusive:
		begin = recordsSubspace.Pack(c.low)
	case EndpointTypeRangeExclusive:
		begin = fdb.Key(recordsSubspace.Pack(c.low))
	case EndpointTypeContinuation:
		if c.continuation != nil {
			// Reconstruct the full key from prefix + continuation
			// Java sends only the suffix, so we need to prepend the subspace prefix
			fullKey := append(recordsSubspace.FDBKey(), c.continuation...)
			// Start after the continuation key by appending 0x00
			begin = append(fullKey, 0x00)
		} else {
			begin = recordsSubspace.FDBKey()
		}
	default:
		begin = recordsSubspace.Pack(c.low)
	}
	
	// Handle high endpoint
	switch c.highEndpoint {
	case EndpointTypeTreeEnd:
		_, endKey := recordsSubspace.FDBRangeKeys()
		end = endKey.(fdb.Key)
	case EndpointTypeRangeInclusive:
		packedKey := recordsSubspace.Pack(c.high)
		// Create end key by appending 0x00 to include the key
		end = append(fdb.Key(packedKey), 0x00)
	case EndpointTypeRangeExclusive:
		end = recordsSubspace.Pack(c.high)
	default:
		end = recordsSubspace.Pack(c.high)
	}
	
	// Create range
	rng := fdb.KeyRange{Begin: begin, End: end}
	
	// Get range options
	options := fdb.RangeOptions{
		Mode:    c.scanProperties.CursorStreamingMode.ToFDB(),
		Reverse: c.scanProperties.IsReverse(),
	}
	
	// Apply row limit if specified
	if c.scanProperties.ExecuteProperties.ReturnedRowLimit > 0 {
		options.Limit = c.scanProperties.ExecuteProperties.ReturnedRowLimit - c.recordsRead
	}
	
	// Create iterator
	tx := c.store.context.Transaction()
	var rangeResult fdb.RangeResult
	if c.scanProperties.ExecuteProperties.IsolationLevel == SnapshotIsolation {
		rangeResult = tx.Snapshot().GetRange(rng, options)
	} else {
		rangeResult = tx.GetRange(rng, options)
	}
	
	c.iterator = rangeResult.Iterator()
	return nil
}

// deserializeKeyValue converts a key-value pair to a stored record
func (c *keyValueCursor) deserializeKeyValue(kv fdb.KeyValue) (*FDBStoredRecord[proto.Message], error) {
	recordsSubspace := c.store.subspace.Sub(RecordKey)
	
	// Unpack the key to get primary key and record type
	keyTuple, err := recordsSubspace.Unpack(kv.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack key: %w", err)
	}
	
	if len(keyTuple) < 1 {
		return nil, fmt.Errorf("invalid key structure: %v", keyTuple)
	}
	
	// Extract record type index (last element)
	recordTypeIndex, ok := keyTuple[len(keyTuple)-1].(int64)
	if !ok {
		return nil, fmt.Errorf("invalid record type index in key: %v", keyTuple[len(keyTuple)-1])
	}
	
	// Find record type by index
	var recordType *RecordType
	for _, rt := range c.store.metaData.recordTypes {
		if rt.GetRecordTypeIndex() == int(recordTypeIndex) {
			recordType = rt
			break
		}
	}
	
	if recordType == nil {
		return nil, fmt.Errorf("unknown record type index: %d", recordTypeIndex)
	}
	
	// Extract primary key (all elements except the last)
	primaryKey := keyTuple[:len(keyTuple)-1]
	
	// Deserialize the record
	protoMessage, err := c.store.deserializeRecord(kv.Value, recordType)
	if err != nil {
		return nil, fmt.Errorf("failed to deserialize record: %w", err)
	}
	
	return &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     protoMessage,
		ValueSize:  len(kv.Value),
		KeySize:    len(kv.Key),
		Split:      false,
	}, nil
}

// Close releases resources held by the cursor
func (c *keyValueCursor) Close() error {
	c.closed = true
	// FDB iterators don't need explicit cleanup
	return nil
}

// Seq returns an iterator sequence over values only
// This implements Go 1.23+ iter.Seq for idiomatic iteration
func (c *keyValueCursor) Seq(ctx context.Context) iter.Seq[*FDBStoredRecord[proto.Message]] {
	return func(yield func(*FDBStoredRecord[proto.Message]) bool) {
		defer func() { _ = c.Close() }()
		
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				// Can't yield errors in Seq, so we stop iteration
				return
			}
			
			if !result.HasNext() {
				return
			}
			
			if !yield(result.GetValue()) {
				return
			}
		}
	}
}

// Seq2 returns an iterator sequence over (value, error) pairs
// This implements Go 1.23+ iter.Seq2 for error-aware iteration
func (c *keyValueCursor) Seq2(ctx context.Context) iter.Seq2[*FDBStoredRecord[proto.Message], error] {
	return func(yield func(*FDBStoredRecord[proto.Message], error) bool) {
		defer func() { _ = c.Close() }()
		
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				if !yield(nil, err) {
					return
				}
				continue
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

// SeqWithContinuation returns an iterator sequence over (value, continuation) pairs
// Useful for implementing pagination or resumable iteration
func (c *keyValueCursor) SeqWithContinuation(ctx context.Context) iter.Seq2[*FDBStoredRecord[proto.Message], RecordCursorContinuation] {
	return func(yield func(*FDBStoredRecord[proto.Message], RecordCursorContinuation) bool) {
		defer func() { _ = c.Close() }()
		
		for {
			result, err := c.OnNext(ctx)
			if err != nil {
				// Can't yield errors here, so we stop
				return
			}
			
			if !result.HasNext() {
				return
			}
			
			if !yield(result.GetValue(), result.GetContinuation()) {
				return
			}
		}
	}
}