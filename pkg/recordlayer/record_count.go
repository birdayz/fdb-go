package recordlayer

import (
	"encoding/binary"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// RecordCountKeySizeMismatchError is Java's
// recordCoreException("key and value are not the same size") thrown by
// getSnapshotRecordCount (FDBRecordStore.java:2295-2297) when the evaluated value
// has a different number of columns than the key expression it is supposed to be a
// value of.
//
// It is a typed error rather than a plain message because the two sizes are the
// whole diagnosis: the caller either passed a value for a different count key or
// truncated one, and the alternative to failing is answering from a slot nothing
// ever writes — a confident, wrong 0.
type RecordCountKeySizeMismatchError struct {
	KeyColumnSize int
	ValueSize     int
}

func (e *RecordCountKeySizeMismatchError) Error() string {
	return fmt.Sprintf("key and value are not the same size: keyColumnSize=%d, valueSize=%d",
		e.KeyColumnSize, e.ValueSize)
}

// Little-endian int64 constants for atomic ADD mutations.
// Must match Java's FDBRecordStore constants exactly.
var (
	littleEndianInt64One      = encodeRecordCount(1)
	littleEndianInt64MinusOne = encodeRecordCount(-1)
	littleEndianInt64Zero     = encodeRecordCount(0)
)

// encodeRecordCount encodes a count as little-endian int64 bytes.
// Matches Java's FDBRecordStore.encodeRecordCount().
func encodeRecordCount(count int64) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(count))
	return buf
}

// decodeRecordCount decodes a little-endian int64 count from bytes.
// Returns 0 for nil (matching Java's behavior where null means 0).
// Matches Java's FDBRecordStore.decodeRecordCount().
func decodeRecordCount(b []byte) int64 {
	if len(b) < 8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(b))
}

// isRecordCountDisabled returns true if the record count state is DISABLED.
// Goroutine-safe via stateMu (read lock).
// Matches Java's addRecordCount which wraps the check in beginRecordStoreStateRead().
func (store *FDBRecordStore) isRecordCountDisabled() bool {
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	if store.storeHeader == nil {
		return false
	}
	return store.storeHeader.GetRecordCountState() == gen.DataStoreInfo_DISABLED
}

// addRecordCount atomically increments or decrements the record count.
// Uses FDB's atomic ADD mutation for lock-free concurrent counting.
// Skips mutation when RecordCountState is DISABLED.
// Matches Java's FDBRecordStore.addRecordCount().
//
// Key format: subspace[RecordCountKey] + evaluated_count_key_tuple
func (store *FDBRecordStore) addRecordCount(record proto.Message, increment []byte) error {
	countKey := store.metaData.GetRecordCountKey()
	if countKey == nil {
		return nil // Counting not configured
	}
	if store.isRecordCountDisabled() {
		return nil // Count state is DISABLED — skip mutation
	}

	// Evaluate the count key expression against the record.
	// Count keys should produce exactly one tuple.
	subkeys, err := countKey.Evaluate(nil, record)
	if err != nil {
		return fmt.Errorf("record count key evaluation failed: %w", err)
	}
	if len(subkeys) != 1 {
		return fmt.Errorf("record count key must evaluate to exactly one tuple, got %d", len(subkeys))
	}
	subkey := subkeys[0]

	// Build the FDB key: subspace + RecordCountKey + evaluated_subkey
	countSubspace := store.subspace.Sub(RecordCountKey)
	keyTuple := make(tuple.Tuple, len(subkey))
	for i, v := range subkey {
		keyTuple[i] = v
	}
	fdbKey := countSubspace.Pack(keyTuple)

	// Atomic ADD — no read needed, no conflicts generated
	store.context.Transaction().Add(fdbKey, increment)
	return nil
}

// snapshotRecordCountFromCountKey reads the store's MAINTAINED record counts — the
// atomic counters under the RecordCountKey subspace — for the given evaluated value
// of the store's record-count key. ok=false means the counters are not a usable
// source (no record-count key configured, or its state is not READABLE), which is
// Java's "skip the whole `if` block and fall through to the index path".
//
// Port of the header branch of Java's getSnapshotRecordCount(key, value, filter)
// (FDBRecordStore.java:2286-2317). Java's `key` argument is implicit in this
// signature: countKey is Key.Evaluated, and the KeyExpression it was evaluated
// against is the store's own record-count key — or EmptyKeyExpression.EMPTY when
// countKey is empty, which is the only shape a caller asking for a store-wide total
// can mean.
//
// That gives Java's three arms exactly:
//
//   - A countKey whose length disagrees with the column size of the expression it
//     was evaluated against is Java's `key.getColumnSize() != value.size()` throw
//     (FDBRecordStore.java:2295-2297), raised BEFORE either read and only once the
//     counters are READABLE — a caller error, never a count. Without it the
//     packed slot for a wrong-width value simply does not exist and the store
//     answers a confident 0.
//
//   - A non-empty countKey, or an empty one against an EmptyKeyExpression count key,
//     is `recordMetaData.getRecordCountKey().equals(key)` — one get of one slot.
//
//   - An empty countKey against a NON-empty (grouped) count key is
//     `key.isPrefixKey(recordMetaData.getRecordCountKey())`, which is always true
//     because EmptyKeyExpression is a prefix of everything
//     (BaseKeyExpression.java:73-75), and Java answers it by summing EVERY group in
//     the count subspace. There is no Java call shape where that combination reads
//     the ungrouped slot instead: `equals` has already failed, so a store counting
//     by record type would report the ungrouped slot's 0 as its total.
//
// The range excludes the count subspace prefix itself, as Java's
// `getSubspace().range(Tuple.from(RECORD_COUNT_KEY))` does. Only the phantom zero
// that DeleteAllRecords writes lives exactly there, and it contributes nothing.
func (store *FDBRecordStore) snapshotRecordCountFromCountKey(countKey tuple.Tuple) (int64, bool, error) {
	recordCountKey := store.metaData.GetRecordCountKey()
	if recordCountKey == nil || !store.recordCountStateIsReadable() {
		return 0, false, nil
	}

	// Java's `key` is implicit in this signature (see above): an empty countKey came
	// from EmptyKeyExpression.EMPTY, whose column size is 0 and therefore always
	// agrees; a non-empty one can only have been evaluated against the store's own
	// record-count key, so that expression's column size is what it must match.
	if len(countKey) > 0 && recordCountKey.ColumnSize() != len(countKey) {
		return 0, false, &RecordCountKeySizeMismatchError{
			KeyColumnSize: recordCountKey.ColumnSize(),
			ValueSize:     len(countKey),
		}
	}

	countSubspace := store.subspace.Sub(RecordCountKey)
	// Snapshot read (non-conflicting), matching Java's context.readTransaction(true).
	tr := store.context.Transaction().Snapshot()

	_, ungroupedCountKey := recordCountKey.(*EmptyKeyExpression)
	if len(countKey) > 0 || ungroupedCountKey {
		value, err := tr.Get(countSubspace.Pack(countKey)).Get()
		if err != nil {
			return 0, false, fmt.Errorf("failed to read record count: %w", err)
		}
		return decodeRecordCount(value), true, nil
	}

	begin, end := countSubspace.FDBRangeKeys()
	kvs, err := tr.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
	if err != nil {
		return 0, false, fmt.Errorf("read grouped record counts: %w", err)
	}
	var total int64
	for _, kv := range kvs {
		total += decodeRecordCount(kv.Value)
	}
	return total, true, nil
}

// GetSnapshotRecordCount returns the count of records matching the given evaluated
// value of the store's record-count key. Uses snapshot (non-conflicting) reads.
//
// For ungrouped counting (EmptyKeyExpression), pass an empty tuple.
// For per-type counting, pass a tuple with the record type key.
//
// Port of Java's getSnapshotRecordCount(key, value, filter)
// (FDBRecordStore.java:2283-2323). The maintained counters answer when they can (see
// snapshotRecordCountFromCountKey); otherwise Java does NOT fail — it falls through
// to evaluateAggregateFunction(emptyList(), count(key), allOf(value), SNAPSHOT,
// filter) (FDBRecordStore.java:2320-2322), so a store with no record-count key at
// all, or whose counters are WRITE_ONLY/DISABLED, is still counted from a universal
// COUNT index. The empty record-type list is what restricts that to universal
// indexes (IndexFunctionHelper.java:180-181).
//
// Returns 0 if no count exists (null in FDB means 0, matching Java).
func (store *FDBRecordStore) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) {
	count, fromCountKey, err := store.snapshotRecordCountFromCountKey(countKey)
	if err != nil || fromCountKey {
		return count, err
	}

	// Java's `key`. An empty evaluated value came from EmptyKeyExpression.EMPTY; a
	// non-empty one can only have come from the store's own record-count key, so
	// without one there is no expression to count by and no Java call shape to port.
	countKeyExpr := EmptyKey()
	scanRange := TupleRangeAll
	if len(countKey) > 0 {
		recordCountKey := store.metaData.GetRecordCountKey()
		if recordCountKey == nil {
			return 0, fmt.Errorf("record counting is not enabled (recordCountKey is nil), so the count key %v cannot be interpreted", countKey)
		}
		countKeyExpr = recordCountKey
		scanRange = TupleRangeAllOf(countKey)
	}

	fn := NewCountAggregateFunction(GroupAll(countKeyExpr))
	result, err := store.EvaluateAggregateFunction(store.context.ctx, nil, fn, scanRange, IsolationLevelSnapshot)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, fmt.Errorf("count aggregate returned an empty tuple")
	}
	total, isInt := result[0].(int64)
	if !isInt {
		return 0, fmt.Errorf("count aggregate returned %T, want int64", result[0])
	}
	return total, nil
}

// GetRecordCount returns the total number of records in the store.
//
// This is Java's getSnapshotRecordCount() (FDBRecordStore.java:2283, called with
// EmptyKeyExpression.EMPTY / Key.Evaluated.EMPTY), and it is a TOTAL under every
// record-count key: a GROUPED count key is rolled up by summing every group, not
// read out of the ungrouped slot that no grouped store ever writes to.
func (store *FDBRecordStore) GetRecordCount() (int64, error) {
	return store.GetSnapshotRecordCount(tuple.Tuple{})
}

// GetSnapshotRecordCountForRecordType returns the count of records for a specific record type.
// Requires that the metadata uses RecordTypeKeyExpression as the count key.
// Matches Java's getSnapshotRecordCountForRecordType().
func (store *FDBRecordStore) GetSnapshotRecordCountForRecordType(recordTypeName string) (int64, error) {
	countKey := store.metaData.GetRecordCountKey()
	if countKey == nil {
		return 0, fmt.Errorf("record counting is not enabled (recordCountKey is nil)")
	}
	if !IsRecordTypeExpression(countKey) {
		return 0, fmt.Errorf("per-type counting requires RecordTypeKeyExpression as count key")
	}
	// Use the record type key (matching Java), not the string name.
	rt := store.metaData.GetRecordType(recordTypeName)
	if rt == nil {
		return 0, &MetaDataError{Message: fmt.Sprintf("unknown record type %q", recordTypeName)}
	}
	return store.GetSnapshotRecordCount(tuple.Tuple{rt.GetRecordTypeKey()})
}

// UpdateRecordCountState transitions the record count state.
// Valid transitions: READABLE↔WRITE_ONLY, any→DISABLED. DISABLED is terminal.
// When transitioning to DISABLED, clears all count data.
// Goroutine-safe via stateMu (write lock).
// Matches Java's FDBRecordStore.updateRecordCountStateAsync().
func (store *FDBRecordStore) UpdateRecordCountState(newState gen.DataStoreInfo_RecordCountState) error {
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	if store.storeHeader == nil {
		return &RecordStoreStateNotLoadedError{}
	}

	existing := store.storeHeader.GetRecordCountState()
	if existing == newState {
		return nil // No-op
	}

	// Validate transition. Matches Java's state machine:
	// READABLE → WRITE_ONLY: allowed
	// WRITE_ONLY → READABLE: allowed
	// any → DISABLED: allowed
	// DISABLED → anything: forbidden (terminal state)
	if existing == gen.DataStoreInfo_DISABLED {
		return fmt.Errorf("invalid state transition for RecordCountState: DISABLED → %s (DISABLED is terminal)", newState)
	}
	toWriteOnly := existing == gen.DataStoreInfo_READABLE && newState == gen.DataStoreInfo_WRITE_ONLY
	toReadable := existing == gen.DataStoreInfo_WRITE_ONLY && newState == gen.DataStoreInfo_READABLE
	toDisabled := newState == gen.DataStoreInfo_DISABLED
	if !toWriteOnly && !toReadable && !toDisabled {
		return fmt.Errorf("invalid state transition for RecordCountState: %s → %s", existing, newState)
	}

	// When transitioning to DISABLED, clear all count data.
	// Use PrefixRange to include the exact prefix key — ungrouped counts
	// are stored at countSubspace.Pack(tuple.Tuple{}) which equals the prefix.
	if toDisabled {
		countSubspace := store.subspace.Sub(RecordCountKey)
		pr, err := fdb.PrefixRange(countSubspace.Bytes())
		if err != nil {
			return fmt.Errorf("record count prefix range: %w", err)
		}
		store.context.Transaction().ClearRange(pr)
	}

	store.storeHeader.RecordCountState = &newState
	return store.writeStoreHeader(store.storeHeader)
}
