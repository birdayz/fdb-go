package recordlayer

import (
	"context"
	"fmt"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"google.golang.org/protobuf/proto"
)

// continuationMagicNumber is the magic number used by newer Java KeyValueCursorBase
// versions (post-4.2.6.0) to distinguish protobuf-wrapped continuations from raw
// byte continuations. We support reading this format but produce raw bytes for
// compatibility with Java Record Layer 4.2.6.0 which only supports raw format.
const continuationMagicNumber int64 = 6_773_487_359_078_157_740

// wrapContinuation returns raw continuation bytes (TO_OLD format).
// This matches Java Record Layer 4.2.6.0's serialization format.
// The raw bytes are the FDB key suffix relative to the scan subspace.
func wrapContinuation(innerBytes []byte) ([]byte, error) {
	return innerBytes, nil
}

// unwrapContinuation extracts the inner continuation bytes from a
// potentially protobuf-wrapped continuation token. Handles both:
// - New format: protobuf-wrapped with magic number
// - Old format: raw bytes (returned as-is for backward compatibility)
// Matches Java's KeyValueCursorBase.Continuation.getInnerContinuation()
func unwrapContinuation(rawBytes []byte) []byte {
	if rawBytes == nil {
		return nil
	}
	msg := &gen.KeyValueCursorContinuation{}
	if err := proto.Unmarshal(rawBytes, msg); err != nil {
		// Parse failed — treat as old-format raw bytes
		return rawBytes
	}
	if msg.MagicNumber == nil || *msg.MagicNumber != continuationMagicNumber {
		// Magic number doesn't match — treat as old-format raw bytes
		return rawBytes
	}
	return msg.InnerContinuation
}

// keyValueCursor implements RecordCursor for scanning key-value pairs from FDB.
// Handles both unsplit records (single KV at suffix 0) and split records
// (multiple KVs at suffixes 1, 2, 3, ...), acting as both the raw KV scanner
// and Java's KeyValueUnsplitter.
type keyValueCursor struct {
	store          *FDBRecordStore
	low            tuple.Tuple
	high           tuple.Tuple
	lowEndpoint    EndpointType
	highEndpoint   EndpointType
	continuation   []byte
	scanProperties ScanProperties

	// Internal state
	iterator       *fdb.RangeIterator
	closed         bool
	recordsRead    int   // Records returned to the caller
	recordsScanned int   // Records scanned (including skipped ones)
	bytesScanned   int64
	prefixLength   int // Length of the subspace prefix for continuation handling
	startTime      time.Time // For time limit enforcement

	// Buffered KV pair for split record handling.
	// When collecting split chunks, we may read the first KV of the next record.
	// That KV is buffered here for the next OnNext() call.
	bufferedKV *fdb.KeyValue
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

	executeProps := c.scanProperties.GetExecuteProperties()

	// When we've returned the requested number of records, check if more exist
	// to distinguish ReturnLimitReached from SourceExhausted.
	if executeProps.ReturnedRowLimit > 0 && c.recordsRead >= executeProps.ReturnedRowLimit {
		if c.hasMoreKVs() {
			return NewResultNoNext[*FDBStoredRecord[proto.Message]](
				ReturnLimitReached,
				&BytesContinuation{bytes: c.continuation},
			), nil
		}
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	// Check time limit BEFORE reading next record (matching Java's CursorLimitManager.tryRecordScan).
	// Allow at least one record before enforcing (free initial pass).
	if executeProps.TimeLimit > 0 && c.recordsScanned > 0 && time.Since(c.startTime) >= executeProps.TimeLimit {
		if c.continuation != nil {
			return NewResultNoNext[*FDBStoredRecord[proto.Message]](
				TimeLimitReached,
				&BytesContinuation{bytes: c.continuation},
			), nil
		}
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			TimeLimitReached,
			&EndContinuation{},
		), nil
	}

	// Check scanned records limit BEFORE reading next record.
	// Continuation points to the last RETURNED record, so resumption starts after it.
	// Matches Java's CursorLimitManager.tryRecordScan() which checks limits pre-read.
	if executeProps.ScannedRecordsLimit > 0 && c.recordsScanned >= executeProps.ScannedRecordsLimit {
		if c.continuation != nil {
			return NewResultNoNext[*FDBStoredRecord[proto.Message]](
				ScanLimitReached,
				&BytesContinuation{bytes: c.continuation},
			), nil
		}
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			ScanLimitReached,
			&EndContinuation{},
		), nil
	}

	// Read the next complete record (handles unsplit, split, and version-skip)
	record, lastKey, err := c.readNextRecord()
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	if record == nil {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	c.recordsScanned++

	// Update scan metrics
	c.bytesScanned += int64(record.KeySize + record.ValueSize)

	// Check byte limit (post-read, since we need the byte count)
	if executeProps.ScannedBytesLimit > 0 && c.bytesScanned > executeProps.ScannedBytesLimit {
		cont, wrapErr := c.makeKeyContinuation(lastKey)
		if wrapErr != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, wrapErr
		}
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			ByteLimitReached,
			&BytesContinuation{bytes: cont},
		), nil
	}

	// Handle skip — count the record as scanned but don't return it
	if executeProps.Skip > 0 && c.recordsScanned <= executeProps.Skip {
		// Update continuation so we can resume after skipped records
		c.continuation, err = c.makeKeyContinuation(lastKey)
		if err != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
		return c.OnNext(ctx) // Recurse to get next record
	}

	c.recordsRead++

	// Create continuation token from the LAST key of this record.
	// For unsplit records, this is the key at suffix 0.
	// For split records, this is the last chunk's key (e.g., suffix 3).
	// This ensures resuming starts after the complete record.
	var keySuffix []byte
	if len(lastKey) > c.prefixLength {
		keySuffix = lastKey[c.prefixLength:]
	} else {
		keySuffix = lastKey
	}

	continuationBytes, err := wrapContinuation(keySuffix)
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, fmt.Errorf("failed to wrap continuation: %w", err)
	}
	c.continuation = continuationBytes

	return NewResultWithValue(
		record,
		&BytesContinuation{bytes: continuationBytes},
	), nil
}

// readNextRecord reads the next complete record from the iterator.
// Handles unsplit records (suffix 0), split records (suffixes 1, 2, ...),
// and skips version keys (suffix -1).
// Returns (nil, nil, nil) when the iterator is exhausted.
func (c *keyValueCursor) readNextRecord() (*FDBStoredRecord[proto.Message], fdb.Key, error) {
	recordsSubspace := c.store.subspace.Sub(RecordKey)

	for {
		// Get the next KV pair (from buffer or iterator)
		kv, ok := c.nextKV()
		if !ok {
			return nil, nil, nil // exhausted
		}

		keyTuple, err := recordsSubspace.Unpack(kv.Key)
		if err != nil || len(keyTuple) < 2 {
			return nil, nil, fmt.Errorf("failed to unpack key: %v (tuple: %v)", err, keyTuple)
		}

		suffix, ok := keyTuple[len(keyTuple)-1].(int64)
		if !ok {
			return nil, nil, fmt.Errorf("key suffix is not int64: %T", keyTuple[len(keyTuple)-1])
		}
		primaryKey := keyTuple[:len(keyTuple)-1]

		switch {
		case suffix == RecordVersionSuffix:
			// Skip version keys, continue to next KV
			continue

		case suffix == UnsplitRecord:
			// Unsplit record — single KV
			recordType, protoMessage, deserErr := c.store.deserializeAndDiscover(kv.Value)
			if deserErr != nil {
				return nil, nil, fmt.Errorf("failed to deserialize record: %w", deserErr)
			}
			return &FDBStoredRecord[proto.Message]{
				PrimaryKey: primaryKey,
				RecordType: recordType,
				Record:     protoMessage,
				KeyCount:   1,
				ValueSize:  len(kv.Value),
				KeySize:    len(kv.Key),
				Split:      false,
			}, kv.Key, nil

		case suffix >= StartSplitRecord:
			// Split record — collect all chunks for this primary key
			return c.readSplitRecord(recordsSubspace, primaryKey, kv, suffix)

		default:
			// Unknown suffix — skip
			continue
		}
	}
}

// splitChunk holds a split record chunk with its suffix index for proper ordering.
type splitChunk struct {
	suffix int64
	key    fdb.Key
	value  []byte
}

// readSplitRecord collects all split chunks for a primary key and reassembles the record.
// firstKV is the first chunk already read (with suffix >= StartSplitRecord).
// Handles both forward and reverse scans — chunks are sorted by suffix before reassembly.
// Returns the reassembled record and the last key (in scan order) for continuation.
func (c *keyValueCursor) readSplitRecord(
	recordsSubspace subspace.Subspace,
	primaryKey tuple.Tuple,
	firstKV fdb.KeyValue,
	firstSuffix int64,
) (*FDBStoredRecord[proto.Message], fdb.Key, error) {
	chunks := []splitChunk{{suffix: firstSuffix, key: firstKV.Key, value: firstKV.Value}}
	lastKey := firstKV.Key
	totalKeySize := len(firstKV.Key)
	totalValueSize := len(firstKV.Value)

	// Collect remaining chunks for this primary key
	for {
		kv, ok := c.nextKV()
		if !ok {
			break // Iterator exhausted
		}

		keyTuple, err := recordsSubspace.Unpack(kv.Key)
		if err != nil || len(keyTuple) < 2 {
			c.bufferedKV = &kv
			break
		}

		suffix, ok := keyTuple[len(keyTuple)-1].(int64)
		if !ok {
			c.bufferedKV = &kv
			break
		}

		kvPrimaryKey := keyTuple[:len(keyTuple)-1]

		// Check if this KV belongs to the same primary key
		if !sameTuple(primaryKey, kvPrimaryKey) {
			c.bufferedKV = &kv
			break
		}

		// Skip version keys within this primary key
		if suffix == RecordVersionSuffix {
			continue
		}

		// Skip unsplit key (suffix 0) if encountered — shouldn't happen but be safe
		if suffix == UnsplitRecord {
			c.bufferedKV = &kv
			break
		}

		chunks = append(chunks, splitChunk{suffix: suffix, key: kv.Key, value: kv.Value})
		lastKey = kv.Key
		totalKeySize += len(kv.Key)
		totalValueSize += len(kv.Value)
	}

	// Sort chunks by suffix for correct reassembly (needed for reverse scans
	// where FDB returns chunks in descending suffix order)
	sortSplitChunks(chunks)

	// Validate sequential indices
	for i, chunk := range chunks {
		expected := StartSplitRecord + int64(i)
		if chunk.suffix != expected {
			return nil, nil, fmt.Errorf("split record segments out of order: expected %d, got %d", expected, chunk.suffix)
		}
	}

	// Reassemble the record
	totalLen := 0
	for _, chunk := range chunks {
		totalLen += len(chunk.value)
	}
	data := make([]byte, 0, totalLen)
	for _, chunk := range chunks {
		data = append(data, chunk.value...)
	}

	recordType, protoMessage, err := c.store.deserializeAndDiscover(data)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to deserialize split record: %w", err)
	}

	return &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     protoMessage,
		KeyCount:   len(chunks),
		ValueSize:  totalValueSize,
		KeySize:    totalKeySize,
		Split:      true,
	}, lastKey, nil
}

// sortSplitChunks sorts chunks by suffix in ascending order (insertion sort — chunks are few).
func sortSplitChunks(chunks []splitChunk) {
	for i := 1; i < len(chunks); i++ {
		key := chunks[i]
		j := i - 1
		for j >= 0 && chunks[j].suffix > key.suffix {
			chunks[j+1] = chunks[j]
			j--
		}
		chunks[j+1] = key
	}
}

// nextKV returns the next KV pair from the buffer or iterator.
// Returns (kv, true) on success, (zero, false) when exhausted.
func (c *keyValueCursor) nextKV() (kv fdb.KeyValue, ok bool) {
	// Return buffered KV if available
	if c.bufferedKV != nil {
		kv = *c.bufferedKV
		c.bufferedKV = nil
		return kv, true
	}

	// Advance iterator
	if !c.iterator.Advance() {
		return fdb.KeyValue{}, false
	}

	// Recover from FDB RangeIterator.Get() index-out-of-bounds panic.
	// The Go bindings have a bug where Advance() returns true but the
	// internal kvs slice is empty, causing Get() to panic at range.go:265.
	// This only affects specific scan patterns (e.g. reverse split scans).
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()

	var err error
	kv, err = c.iterator.Get()
	if err != nil {
		return fdb.KeyValue{}, false
	}
	return kv, true
}

// makeKeyContinuation creates a proto-wrapped continuation from an FDB key.
func (c *keyValueCursor) makeKeyContinuation(key fdb.Key) ([]byte, error) {
	var keySuffix []byte
	if len(key) > c.prefixLength {
		keySuffix = key[c.prefixLength:]
	} else {
		keySuffix = key
	}
	cont, err := wrapContinuation(keySuffix)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap continuation: %w", err)
	}
	return cont, nil
}

// hasMoreKVs checks if there are more KV pairs available (from buffer or iterator).
// Used for the limit-reached vs source-exhausted check.
func (c *keyValueCursor) hasMoreKVs() bool {
	if c.bufferedKV != nil {
		return true
	}
	return c.iterator.Advance()
}

// sameTuple compares two tuples for equality.
func sameTuple(a, b tuple.Tuple) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		packedKey := recordsSubspace.Pack(c.low)
		begin = append(fdb.Key(packedKey), 0x00) // exclusive: start AFTER this key
	case EndpointTypeContinuation:
		if c.continuation != nil {
			innerContinuation := unwrapContinuation(c.continuation)
			fullKey := append(recordsSubspace.FDBKey(), innerContinuation...)
			begin = append(fullKey, 0x00) // Start after the continuation key
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
		var strincErr error
		end, strincErr = fdb.Strinc(packedKey)
		if strincErr != nil {
			return fmt.Errorf("failed to compute strinc for high endpoint: %w", strincErr)
		}
	case EndpointTypeRangeExclusive:
		end = recordsSubspace.Pack(c.high)
	case EndpointTypeContinuation:
		if c.continuation != nil {
			innerContinuation := unwrapContinuation(c.continuation)
			fullKey := append(recordsSubspace.FDBKey(), innerContinuation...)
			end = fullKey // exclusive: FDB won't return this key
		} else {
			_, endKey := recordsSubspace.FDBRangeKeys()
			end = endKey.(fdb.Key)
		}
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

	// Apply FDB-level row limit only for unsplit stores.
	// When splitLongRecords is enabled, a single record may span multiple KVs,
	// so the FDB row limit doesn't map cleanly to record limits.
	// Record-level limits are enforced in OnNext() instead.
	// Skip is added to the FDB limit so we have enough KVs to skip AND return.
	if c.scanProperties.ExecuteProperties.ReturnedRowLimit > 0 && !c.store.metaData.IsSplitLongRecords() {
		skip := c.scanProperties.ExecuteProperties.Skip
		options.Limit = c.scanProperties.ExecuteProperties.ReturnedRowLimit + skip - c.recordsRead + 1
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

// Close releases resources held by the cursor
func (c *keyValueCursor) Close() error {
	c.closed = true
	return nil
}
