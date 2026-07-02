package recordlayer

import (
	"context"
	"fmt"
	"math"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// continuationMagicNumber is the magic number Java's KeyValueCursorBase writes into
// the protobuf-wrapped continuation (SerializationMode.TO_NEW) so the reader can
// distinguish a wrapped token from a legacy raw-byte token.
const continuationMagicNumber int64 = 6_773_487_359_078_157_740

// wrapContinuation produces a protobuf-wrapped continuation token (TO_NEW format),
// matching Java Record Layer 4.11.1.0: KeyValueCursorBase.Builder defaults
// SerializationMode to TO_NEW and no production path selects TO_OLD, so Java emits
// KeyValueCursorContinuation{inner_continuation, magic_number} — never raw bytes.
// Go previously emitted raw bytes, a wire divergence (a Go-written continuation read
// back by Java, or compared against Java's, would not round-trip identically).
//
// innerBytes is the FDB key suffix relative to the scan subspace and is always a
// real (non-end) position — the end-of-scan case uses EndContinuation, never this.
// An empty-but-present suffix still yields a proto carrying the magic number, which
// is wire-distinguishable from an end/start token (nil), matching Java and avoiding
// the old raw format's ambiguity where an empty suffix produced an empty, end-looking
// token. The dual-read unwrapContinuation still accepts legacy raw tokens.
func wrapContinuation(innerBytes []byte) ([]byte, error) {
	// A nil suffix is an end position; Java returns the end marker, not a wrapped
	// token. The cursor already represents end via EndContinuation, so this is
	// defensive — never fabricate a proto for nil. (An empty-but-non-nil suffix
	// IS a real position and is wrapped below.)
	if innerBytes == nil {
		return nil, nil
	}
	magic := continuationMagicNumber
	msg := &gen.KeyValueCursorContinuation{
		InnerContinuation: innerBytes,
		MagicNumber:       &magic,
	}
	out, err := msg.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("marshal key-value cursor continuation: %w", err)
	}
	return out, nil
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
		// Parse failed — treat as old-format raw bytes. This restart-free
		// tolerance is Java-verified, NOT the swallow-and-restart bug class:
		// KeyValueCursorBase.Continuation.getInnerContinuation
		// (KeyValueCursorBase.java:218-223) deliberately returns rawBytes on
		// InvalidProtocolBufferException so TO_OLD-serialized tokens keep
		// working across the TO_NEW migration.
		return rawBytes
	}
	if msg.MagicNumber == nil || *msg.MagicNumber != continuationMagicNumber {
		// Magic number doesn't match — treat as old-format raw bytes
		// (KeyValueCursorBase.java:212-216, same deliberate tolerance).
		return rawBytes
	}
	return msg.InnerContinuation
}

// rangeIterator is the minimal FDB range-scan iterator the cursor depends on.
// *fdb.RangeIterator satisfies it; the interface exists so tests can inject a fake
// that returns an error at a chosen position (the row-limit boundary), which a
// concrete *fdb.RangeIterator can't be made to do deterministically.
type rangeIterator interface {
	Advance() bool
	Get() (fdb.KeyValue, error)
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
	iterator       rangeIterator
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

	// omitUnsplitRecordSuffix is true for legacy stores whose unsplit records are
	// written at the bare primary key with no suffix. Only meaningful when the
	// metadata does NOT split long records. When set, each KV is a complete record.
	omitUnsplitRecordSuffix bool

	// useOldVersionFormat is true for legacy stores whose record versions live in
	// the separate RecordVersionKey(8) subspace rather than inline at pk+-1. When
	// set, the scan does not look for inline version keys; the version (if any) is
	// loaded with a separate read per record, matching Java's scanTypedRecords.
	useOldVersionFormat bool

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
		more, err := c.hasMoreKVs()
		if err != nil {
			// A transient error here must surface (and be retried) — not be silently
			// collapsed into SourceExhausted, which would truncate the scan.
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
		if more {
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
		return noNextOrFail[*FDBStoredRecord[proto.Message]](executeProps, ScanLimitReached, c.limitContinuation())
	}

	// Check byte limit BEFORE reading next record (matching Java's CursorLimitManager.tryRecordScan).
	// Java's tryRecordScan() calls byteScanLimiter.hasBytesRemaining() before the read.
	// Allow at least one record (free initial pass — usedInitialPass in Java).
	if executeProps.ScannedBytesLimit > 0 && c.recordsScanned > 0 && c.bytesScanned >= executeProps.ScannedBytesLimit {
		return noNextOrFail[*FDBStoredRecord[proto.Message]](executeProps, ByteLimitReached, c.limitContinuation())
	}

	// Read the next complete record (handles unsplit, split, and version-skip)
	record, lastKey, err := c.readNextRecord(ctx)
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
func (c *keyValueCursor) readNextRecord(ctx context.Context) (*FDBStoredRecord[proto.Message], fdb.Key, error) {
	// Legacy bare-key layout: every KV is a complete record with no suffix.
	if c.omitUnsplitRecordSuffix {
		return c.readNextBareRecord(ctx)
	}

	recordsSubspace := c.recordsSubspace

	prefixLen := c.prefixLength

	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		// Get the next KV pair (from buffer or iterator)
		kv, ok, err := c.nextKV()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, nil // exhausted
		}

		// A record key is always prefix + PK-tuple + suffix, i.e. strictly longer than
		// the records-subspace prefix. A key at or under the prefix is a stray or
		// malformed key (corruption, a foreign client, or a scan range whose begin
		// included the bare prefix) — return a typed error rather than slice-panicking
		// on kv.Key[prefixLen:] (key shorter than the prefix) or index-panicking inside
		// splitKeySuffix on the empty suffix. The other splitKeySuffix callers
		// (peekVersionKey, the chunk-reassembly loop) already guard this length; this is
		// the primary record-scan path's matching guard.
		if len(kv.Key) <= prefixLen {
			return nil, nil, fmt.Errorf("record cursor: key length %d <= subspace prefix length %d (malformed or out-of-range key under the records subspace)", len(kv.Key), prefixLen)
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
			// Attach version if versioning is enabled. In the modern layout this uses
			// the inline version (pendingVersion / peek-ahead / local cache); in the
			// legacy layout resolveVersion does a separate read from subspace 8.
			version, verErr := c.resolveVersion(recordsSubspace, primaryKey, true)
			if verErr != nil {
				return nil, nil, verErr
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
			return c.readSplitRecord(ctx, recordsSubspace, primaryKey, kv, suffix)

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

// isSnapshot reports whether this scan reads at snapshot isolation (no read conflicts).
func (c *keyValueCursor) isSnapshot() bool {
	return c.scanProperties.ExecuteProperties.IsolationLevel == SnapshotIsolation
}

// resolveVersion returns the version for the record with the given primary key, or nil.
//
// Legacy layout (useOldVersionFormat): the version lives in the separate
// RecordVersionKey(8) subspace, so it is fetched with a per-record read via
// LoadRecordVersion (which also consults the local version cache and applies the
// skip-I/O optimization). Matches Java's scanTypedRecords, which calls
// loadRecordVersionAsync per record when useOldVersionFormat().
//
// Modern layout: the version was captured inline while scanning (pendingVersion);
// for reverse scans of unsplit records it may follow the data key, so allowPeek
// enables a one-KV look-ahead. The local version cache covers same-transaction saves
// whose inline SET_VERSIONSTAMPED_VALUE has not been committed yet.
func (c *keyValueCursor) resolveVersion(recordsSubspace subspace.Subspace, primaryKey tuple.Tuple, allowPeek bool) (*FDBRecordVersion, error) {
	if !c.storeRecordVersions {
		return nil, nil
	}
	if c.useOldVersionFormat {
		return c.store.LoadRecordVersion(primaryKey, c.isSnapshot())
	}
	version := c.takePendingVersion(primaryKey)
	if version == nil && allowPeek {
		v, err := c.peekVersionKey(recordsSubspace, primaryKey)
		if err != nil {
			return nil, err
		}
		version = v
	}
	if version == nil {
		version = c.localVersionFallback(primaryKey)
	}
	return version, nil
}

// readNextBareRecord reads the next record from a legacy store that omits the unsplit
// record suffix: every KV under the records subspace is a complete record stored at the
// bare primary key (no trailing suffix), and versions (if any) live in the separate
// RecordVersionKey(8) subspace. Matches Java's scanTypedRecords omitUnsplitRecordSuffix
// branch (each KV mapped directly to an FDBRawRecord with a null inline version).
func (c *keyValueCursor) readNextBareRecord(ctx context.Context) (*FDBStoredRecord[proto.Message], fdb.Key, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	kv, ok, err := c.nextKV()
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, nil // exhausted
	}
	if len(kv.Key) <= c.prefixLength {
		return nil, nil, fmt.Errorf("record cursor: key length %d <= subspace prefix length %d (malformed or out-of-range key under the records subspace)", len(kv.Key), c.prefixLength)
	}
	// The whole tuple after the prefix is the primary key — there is no suffix.
	primaryKey, pkErr := fastUnpack(kv.Key[c.prefixLength:])
	if pkErr != nil {
		return nil, nil, fmt.Errorf("failed to unpack legacy primary key: %w", pkErr)
	}
	recordType, protoMessage, deserErr := c.store.deserializeAndDiscover(kv.Value)
	if deserErr != nil {
		return nil, nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: deserErr}
	}
	version, verErr := c.resolveVersion(c.recordsSubspace, primaryKey, false)
	if verErr != nil {
		return nil, nil, verErr
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
	ctx context.Context,
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
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
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

	// Attach version captured during chunk collection (forward or reverse scan). In
	// the legacy layout resolveVersion does a separate read from subspace 8 instead.
	version, verErr := c.resolveVersion(recordsSubspace, primaryKey, false)
	if verErr != nil {
		return nil, nil, verErr
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
		// Advance returns false on exhaustion OR error. Check Get() for
		// the stored error — this surfaces transaction_too_old (1007)
		// instead of silently treating timeout as end-of-data.
		if _, err := c.iterator.Get(); err != nil {
			return fdb.KeyValue{}, false, fmt.Errorf("key-value cursor: iterator advance: %w", err)
		}
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

// hasMoreKVs reports whether more KV pairs are available (from the buffer or the
// iterator), used to distinguish ReturnLimitReached from SourceExhausted. It
// surfaces any FDB error encountered probing the iterator (transaction_too_old,
// timeout) rather than treating it as "no more" — swallowing it would silently end
// the scan at the row-limit boundary and lose the remaining rows.
func (c *keyValueCursor) hasMoreKVs() (bool, error) {
	if c.bufferedKV != nil {
		return true, nil
	}
	if c.iterator.Advance() {
		return true, nil
	}
	// Advance returns false on exhaustion OR error. Check Get() for the stored
	// error (transaction_too_old 1007, timeout) — otherwise a transient error
	// landing exactly on the row-limit boundary is silently read as end-of-data,
	// permanently ending the scan with SourceExhausted and LOSING the remaining
	// rows. Mirrors nextKV's post-Advance error check.
	if _, err := c.iterator.Get(); err != nil {
		return false, fmt.Errorf("key-value cursor: iterator advance at row-limit boundary: %w", err)
	}
	return false, nil
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
		// When versioning is enabled AND versions are inline, each record has 2 KV
		// pairs (version at suffix -1, data at suffix 0). Double the FDB limit to
		// account for the version KVs. Matches Java's FDBRecordStore scanRecords which
		// uses 2 * returnedRowLimit. Legacy stores keep versions in a separate subspace
		// (or omit the suffix entirely → 1 KV per record), so no doubling is needed.
		if c.storeRecordVersions && !c.useOldVersionFormat {
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
