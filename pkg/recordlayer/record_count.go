package recordlayer

import (
	"encoding/binary"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

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

// GetSnapshotRecordCount returns the count of records matching the given key.
// Uses snapshot reads (non-conflicting) matching Java's getSnapshotRecordCount().
// Only allowed when RecordCountState is READABLE.
//
// For ungrouped counting (EmptyKeyExpression), pass an empty tuple.
// For per-type counting, pass a tuple with the record type name/index.
//
// Returns 0 if no count exists (null in FDB means 0, matching Java).
func (store *FDBRecordStore) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) {
	if store.metaData.GetRecordCountKey() == nil {
		return 0, fmt.Errorf("record counting is not enabled (recordCountKey is nil)")
	}
	store.stateMu.RLock()
	countDisabled := store.storeHeader != nil && store.storeHeader.GetRecordCountState() != gen.DataStoreInfo_READABLE
	var countState gen.DataStoreInfo_RecordCountState
	if store.storeHeader != nil {
		countState = store.storeHeader.GetRecordCountState()
	}
	store.stateMu.RUnlock()
	if countDisabled {
		return 0, fmt.Errorf("record count is not readable (state: %s)", countState)
	}

	countSubspace := store.subspace.Sub(RecordCountKey)
	fdbKey := countSubspace.Pack(countKey)

	// Use snapshot read (non-conflicting), matching Java
	value, err := store.context.Transaction().Snapshot().Get(fdbKey).Get()
	if err != nil {
		return 0, fmt.Errorf("failed to read record count: %w", err)
	}
	return decodeRecordCount(value), nil
}

// GetRecordCount returns the total count across all groups by reading
// the count for the evaluated count key of the given record.
// For ungrouped counting, this returns the total count.
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
