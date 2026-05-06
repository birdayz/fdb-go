package recordlayer

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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
	if err := msg.UnmarshalVT(rawBytes); err != nil {
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
	recordsRead    int // Records returned to the caller
	recordsScanned int // Records scanned (including skipped ones)
	bytesScanned   int64
	prefixLength   int       // Length of the subspace prefix for continuation handling
	startTime      time.Time // For time limit enforcement

	// Cached subspace to avoid recomputing store.subspace.Sub(RecordKey) per record.
	recordsSubspace subspace.Subspace

	// Whether the metadata has record versioning enabled. Cached to avoid
	// per-record method calls and to skip version key lookups entirely when false.
	storeRecordVersions bool

	// Buffered KV pair for split record handling.
	// When collecting split chunks, we may read the first KV of the next record.
	// That KV is buffered here for the next OnNext() call.
	bufferedKV *fdb.KeyValue

	// pendingVersion holds a version captured from a recordVersionSuffix key
	// that hasn't been attached to a record yet. In forward scans, the version
	// key (suffix -1) appears before the record data (suffix 0 or 1+), so we
	// store it here until the record is read.
	pendingVersion   *FDBRecordVersion
	pendingVersionPK tuple.Tuple // PK of the record this pending version belongs to
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
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			TimeLimitReached,
			c.limitContinuation(),
		), nil
	}

	// Check scanned records limit BEFORE reading next record.
	// Continuation points to the last RETURNED record, so resumption starts after it.
	// Matches Java's CursorLimitManager.tryRecordScan() which checks limits pre-read.
	if executeProps.ScannedRecordsLimit > 0 && c.recordsScanned >= executeProps.ScannedRecordsLimit {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			ScanLimitReached,
			c.limitContinuation(),
		), nil
	}

	// Check byte limit BEFORE reading next record (matching Java's CursorLimitManager.tryRecordScan).
	// Java's tryRecordScan() calls byteScanLimiter.hasBytesRemaining() before the read.
	// Allow at least one record (free initial pass — usedInitialPass in Java).
	if executeProps.ScannedBytesLimit > 0 && c.recordsScanned > 0 && c.bytesScanned >= executeProps.ScannedBytesLimit {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](
			ByteLimitReached,
			c.limitContinuation(),
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

	// Accumulate bytes scanned — checked pre-read on next call
	c.bytesScanned += int64(record.KeySize + record.ValueSize)

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
	recordsSubspace := c.recordsSubspace

	prefixLen := c.prefixLength

	for {
		// Get the next KV pair (from buffer or iterator)
		kv, ok, err := c.nextKV()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, nil // exhausted
		}

		// Fast path: extract suffix via zero-alloc tuple scan.
		// Only call full tuple.Unpack for the PK when building the returned record.
		tupleBytes := kv.Key[prefixLen:]
		suffix, pkEnd, splitErr := splitKeySuffix(tupleBytes)
		if splitErr != nil {
			return nil, nil, fmt.Errorf("failed to parse key suffix: %w", splitErr)
		}

		switch {
		case suffix == recordVersionSuffix:
			// Capture version data for the next record.
			// Only decode PK when versioning is enabled (we need it for pendingVersionPK).
			if c.storeRecordVersions {
				if ver, verErr := unpackVersion(kv.Value); verErr == nil {
					pk, pkErr := fastUnpack(tupleBytes[:pkEnd])
					if pkErr != nil {
						return nil, nil, fmt.Errorf("failed to unpack version primary key: %w", pkErr)
					}
					c.pendingVersion = ver
					c.pendingVersionPK = pk
				}
			}
			continue

		case suffix == unsplitRecord:
			// Unsplit record — decode PK only now that we need it
			primaryKey, pkErr := fastUnpack(tupleBytes[:pkEnd])
			if pkErr != nil {
				return nil, nil, fmt.Errorf("failed to unpack primary key: %w", pkErr)
			}
			recordType, protoMessage, deserErr := c.store.deserializeAndDiscover(kv.Value)
			if deserErr != nil {
				return nil, nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
			}
			// Attach version if versioning is enabled
			var version *FDBRecordVersion
			if c.storeRecordVersions {
				// From pendingVersion (forward scan) or peek ahead (reverse scan)
				version = c.takePendingVersion(primaryKey)
				if version == nil {
					version, err = c.peekVersionKey(recordsSubspace, primaryKey)
					if err != nil {
						return nil, nil, err
					}
				}
				// Fallback: check local version cache for records saved but not yet
				// committed in this transaction. The version key is a pending
				// SET_VERSIONSTAMPED_VALUE mutation, not in FDB yet.
				// Matches Java's SplitHelper.KeyValueUnsplitter (line 890-899).
				if version == nil {
					version = c.localVersionFallback(primaryKey)
				}
			}
			return &FDBStoredRecord[proto.Message]{
				PrimaryKey: primaryKey,
				RecordType: recordType,
				Record:     protoMessage,
				Version:    version,
				Store:      c.store,
				KeyCount:   1,
				ValueSize:  len(kv.Value),
				KeySize:    len(kv.Key),
				Split:      false,
			}, kv.Key, nil

		case suffix >= startSplitRecord:
			// Split record — need full PK for chunk collection
			primaryKey, pkErr := fastUnpack(tupleBytes[:pkEnd])
			if pkErr != nil {
				return nil, nil, fmt.Errorf("failed to unpack primary key: %w", pkErr)
			}
			return c.readSplitRecord(recordsSubspace, primaryKey, kv, suffix)

		default:
			// Unknown suffix — skip
			continue
		}
	}
}

// takePendingVersion returns and clears the pending version if it belongs to
// the given primary key. In forward scans, the version key (suffix -1) appears
// before the record data, so pendingVersion is set before the record is read.
// In reverse scans for split records, readSplitRecord captures the version
// while collecting chunks.
//
// The PK check prevents version leakage across continuation boundaries in
// reverse scans: when resuming, the stale version key from the previous
// record falls within the scan range and gets captured. Without the PK check,
// it would be incorrectly attached to the next record.
func (c *keyValueCursor) takePendingVersion(currentPK tuple.Tuple) *FDBRecordVersion {
	if c.pendingVersion != nil {
		if sameTuple(c.pendingVersionPK, currentPK) {
			ver := c.pendingVersion
			c.pendingVersion = nil
			c.pendingVersionPK = nil
			return ver
		}
		// Version belongs to a different record (reverse scan continuation
		// boundary). Discard it.
		c.pendingVersion = nil
		c.pendingVersionPK = nil
	}
	return nil
}

// peekVersionKey peeks at the next KV to check if it's a version key for the
// same primary key. Used in reverse scans where the version key (suffix -1)
// appears after the record data. If found, returns the version and consumes the
// KV. Otherwise, buffers the peeked KV for the next call.
func (c *keyValueCursor) peekVersionKey(recordsSubspace subspace.Subspace, primaryKey tuple.Tuple) (*FDBRecordVersion, error) {
	kv, ok, err := c.nextKV()
	if err != nil {
		return nil, fmt.Errorf("peek version key: %w", err)
	}
	if !ok {
		return nil, nil
	}
	if len(kv.Key) <= c.prefixLength {
		c.bufferedKV = &kv
		return nil, nil
	}
	tupleBytes := kv.Key[c.prefixLength:]
	suffix, pkEnd, err := splitKeySuffix(tupleBytes)
	if err != nil {
		c.bufferedKV = &kv
		return nil, nil
	}
	if suffix == recordVersionSuffix {
		kvPK, pkErr := fastUnpack(tupleBytes[:pkEnd])
		if pkErr != nil {
			c.bufferedKV = &kv
			return nil, nil
		}
		if sameTuple(kvPK, primaryKey) {
			ver, verErr := unpackVersion(kv.Value)
			if verErr != nil {
				return nil, nil
			}
			return ver, nil
		}
	}
	c.bufferedKV = &kv
	return nil, nil
}

// localVersionFallback checks the local version cache for records saved in the
// current transaction whose version key hasn't been committed yet (it's a pending
// SET_VERSIONSTAMPED_VALUE mutation). Returns an incomplete version if found.
// Matches Java's SplitHelper.KeyValueUnsplitter which checks context.getLocalVersion()
// after assembling each record.
func (c *keyValueCursor) localVersionFallback(primaryKey tuple.Tuple) *FDBRecordVersion {
	versionKey := c.store.versionKey(primaryKey)
	localVer, ok := c.store.context.GetLocalVersion(versionKey)
	if !ok {
		return nil
	}
	ver, err := IncompleteVersion(localVer)
	if err != nil {
		return nil
	}
	return ver
}

// splitChunk holds a split record chunk with its suffix index for proper ordering.
type splitChunk struct {
	suffix int64
	key    fdb.Key
	value  []byte
}

// readSplitRecord collects all split chunks for a primary key and reassembles the record.
// firstKV is the first chunk already read (with suffix >= startSplitRecord).
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
		kv, ok, kvErr := c.nextKV()
		if kvErr != nil {
			return nil, nil, kvErr
		}
		if !ok {
			break // Iterator exhausted
		}

		if len(kv.Key) <= c.prefixLength {
			c.bufferedKV = &kv
			break
		}
		chunkTuple := kv.Key[c.prefixLength:]
		suffix, chunkPKEnd, splitErr := splitKeySuffix(chunkTuple)
		if splitErr != nil {
			c.bufferedKV = &kv
			break
		}

		// Check if this KV belongs to the same primary key by comparing
		// the raw PK bytes (avoids full tuple decode for non-matching keys).
		kvPrimaryKey, pkErr := fastUnpack(chunkTuple[:chunkPKEnd])
		if pkErr != nil {
			c.bufferedKV = &kv
			break
		}
		if !sameTuple(primaryKey, kvPrimaryKey) {
			c.bufferedKV = &kv
			break
		}

		// Capture version key within this primary key (for reverse scans,
		// version at suffix -1 appears after split chunks)
		if suffix == recordVersionSuffix {
			if ver, verErr := unpackVersion(kv.Value); verErr == nil {
				c.pendingVersion = ver
				c.pendingVersionPK = primaryKey
			}
			continue
		}

		// Skip unsplit key (suffix 0) if encountered — shouldn't happen but be safe
		if suffix == unsplitRecord {
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
		expected := startSplitRecord + int64(i)
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
		return nil, nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: err}
	}

	// Attach version captured during chunk collection (forward or reverse scan)
	var version *FDBRecordVersion
	if c.storeRecordVersions {
		version = c.takePendingVersion(primaryKey)
		if version == nil {
			version = c.localVersionFallback(primaryKey)
		}
	}

	return &FDBStoredRecord[proto.Message]{
		PrimaryKey: primaryKey,
		RecordType: recordType,
		Record:     protoMessage,
		Version:    version,
		Store:      c.store,
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
// Returns (kv, true, nil) on success, (zero, false, nil) when exhausted,
// or (zero, false, err) on FDB error.
func (c *keyValueCursor) nextKV() (fdb.KeyValue, bool, error) {
	// Return buffered KV if available
	if c.bufferedKV != nil {
		kv := *c.bufferedKV
		c.bufferedKV = nil
		return kv, true, nil
	}

	// Advance iterator
	if !c.iterator.Advance() {
		return fdb.KeyValue{}, false, nil
	}

	kv, err := c.iterator.Get()
	if err != nil {
		return fdb.KeyValue{}, false, fmt.Errorf("key-value cursor: iterator get: %w", err)
	}
	return kv, true, nil
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
// Used for the limit-reached vs source-exhausted check. Best-effort: FDB errors
// during the probe are treated as "no more" since we're just distinguishing
// ReturnLimitReached from SourceExhausted.
func (c *keyValueCursor) hasMoreKVs() bool {
	if c.bufferedKV != nil {
		return true
	}
	return c.iterator.Advance()
}

// limitContinuation returns the appropriate continuation when a limit is hit.
// If we have a continuation (from a previously scanned record), use it.
// Otherwise, use StartContinuation (no position information available but NOT end of iteration).
func (c *keyValueCursor) limitContinuation() RecordCursorContinuation {
	if c.continuation != nil {
		return &BytesContinuation{bytes: c.continuation}
	}
	return &StartContinuation{}
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
		// Use Strinc to skip past ALL keys with this prefix.
		// append(0x00) is wrong because pack(pk)\x00 < pack(pk, suffix)
		// so the boundary record would still be included.
		// Matches Java's ByteArrayUtil.strinc() for exclusive low endpoints.
		strincKey, strincErr := fdb.Strinc(packedKey)
		if strincErr != nil {
			return fmt.Errorf("failed to compute strinc for exclusive low endpoint: %w", strincErr)
		}
		begin = strincKey
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
		end = endKey.FDBKey()
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
			end = endKey.FDBKey()
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
		limit := saturatingAdd(c.scanProperties.ExecuteProperties.ReturnedRowLimit, skip) - c.recordsRead
		if limit <= 0 {
			limit = 1
		}
		recordLimit := saturatingAdd(limit, 1)
		// When versioning is enabled, each record has 2 KV pairs
		// (version at suffix -1, data at suffix 0). Double the FDB limit
		// to account for version KVs.
		// Matches Java's FDBRecordStore scanRecords which uses 2 * returnedRowLimit.
		if c.storeRecordVersions {
			if recordLimit > math.MaxInt/2 {
				recordLimit = math.MaxInt
			} else {
				recordLimit *= 2
			}
		}
		options.Limit = recordLimit
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

func (c *keyValueCursor) IsClosed() bool { return c.closed }
