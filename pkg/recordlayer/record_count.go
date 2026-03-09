package recordlayer

import (
	"encoding/binary"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Little-endian int64 constants for atomic ADD mutations.
// Must match Java's FDBRecordStore constants exactly.
var (
	littleEndianInt64One      = encodeRecordCount(1)
	littleEndianInt64MinusOne = encodeRecordCount(-1)
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

// addRecordCount atomically increments or decrements the record count.
// Uses FDB's atomic ADD mutation for lock-free concurrent counting.
// Matches Java's FDBRecordStore.addRecordCount().
//
// Key format: subspace[RecordCountKey] + evaluated_count_key_tuple
func (store *FDBRecordStore) addRecordCount(record proto.Message, increment []byte) {
	countKey := store.metaData.GetRecordCountKey()
	if countKey == nil {
		return // Counting disabled
	}

	// Evaluate the count key expression against the record.
	// Count keys should produce exactly one tuple.
	subkeys, err := countKey.Evaluate(record)
	if err != nil {
		// Silently skip counting on evaluation errors (matches Java behavior
		// where this is logged but doesn't fail the operation)
		return
	}
	if len(subkeys) != 1 {
		return // Count key must evaluate to exactly one tuple
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
}

// GetSnapshotRecordCount returns the count of records matching the given key.
// Uses snapshot reads (non-conflicting) matching Java's getSnapshotRecordCount().
//
// For ungrouped counting (EmptyKeyExpression), pass an empty tuple.
// For per-type counting, pass a tuple with the record type name/index.
//
// Returns 0 if no count exists (null in FDB means 0, matching Java).
func (store *FDBRecordStore) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) {
	if store.metaData.GetRecordCountKey() == nil {
		return 0, fmt.Errorf("record counting is not enabled (recordCountKey is nil)")
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
		return 0, fmt.Errorf("unknown record type %q", recordTypeName)
	}
	return store.GetSnapshotRecordCount(tuple.Tuple{rt.GetRecordTypeKey()})
}
