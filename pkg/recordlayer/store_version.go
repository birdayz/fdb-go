package recordlayer

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// saveRecordVersion stores the version for a record.
//
// In the modern layout (format version >= SAVE_VERSION_WITH_RECORD and the unsplit
// suffix is not omitted) the version is stored inline adjacent to the record at
// recordsSubspace.pack(primaryKey, -1) as a packed Tuple{Versionstamp}, matching
// Java's SplitHelper.writeVersion()/packVersion(). The record's sizeInfo counts the
// inline version key/value bytes.
//
// In the legacy layout (useOldVersionFormat) the version is stored in the separate
// RecordVersionKey(8) subspace keyed by the bare primary key, as raw 12-byte
// FDBRecordVersion bytes (complete) or 12 raw bytes + a 4-byte SET_VERSIONSTAMPED_VALUE
// offset of 0 (incomplete). This matches Java's saveVersionWithOldFormat(); the legacy
// version is in a different subspace, so it is NOT counted in the record's sizeInfo and
// sizeInfo.VersionedInline stays false.
func (store *FDBRecordStore) saveRecordVersion(primaryKey tuple.Tuple, version *FDBRecordVersion, si *sizeInfo) error {
	versionKey := store.versionKey(primaryKey)

	if store.useOldVersionFormat() {
		return store.saveVersionWithOldFormat(versionKey, version)
	}

	if version.IsComplete() {
		// Direct set for complete versions (rare — only when explicitly provided)
		// Pack as Tuple{Versionstamp} matching Java's SplitHelper.packVersion()
		packed, err := packVersion(version)
		if err != nil {
			return fmt.Errorf("pack version: %w", err)
		}
		store.context.Transaction().Set(versionKey, packed)
		if si != nil {
			si.VersionedInline = true
			si.KeyCount++
			si.KeySize += len(versionKey)
			si.ValueSize += len(packed)
		}
	} else {
		// Queue SET_VERSIONSTAMPED_VALUE for incomplete versions
		store.context.AddToLocalVersionCache(versionKey, version.GetLocalVersion())
		packed, err := buildVersionstampedValue(version)
		if err != nil {
			return fmt.Errorf("failed to build versionstamped value: %w", err)
		}
		store.context.AddVersionMutation(MutationTypeSetVersionstampedValue, versionKey, packed)
		if si != nil {
			si.VersionedInline = true
			si.KeyCount++
			si.KeySize += len(versionKey)
			// Subtract 4 bytes for the versionstamp offset appended by
			// PackWithVersionstamp — it is not made durable.
			// Matches Java's SplitHelper.writeVersion: sizeInfo.valueSize -= Integer.BYTES
			si.ValueSize += len(packed) - 4
		}
	}
	return nil
}

// saveVersionWithOldFormat stores a record version in the legacy RecordVersionKey(8)
// subspace. The complete value is the raw 12-byte FDBRecordVersion; the incomplete
// value is the raw 12 bytes followed by a 4-byte SET_VERSIONSTAMPED_VALUE offset of 0
// (the versionstamp overwrites the first 10 bytes at commit). Does not update sizeInfo
// — the legacy version lives outside the record's key range.
// Matches Java's FDBRecordStore.saveVersionWithOldFormat().
func (store *FDBRecordStore) saveVersionWithOldFormat(versionKey fdb.Key, version *FDBRecordVersion) error {
	if version.IsComplete() {
		store.context.Transaction().Set(versionKey, version.ToBytes())
		return nil
	}
	store.context.AddToLocalVersionCache(versionKey, version.GetLocalVersion())
	store.context.AddVersionMutation(MutationTypeSetVersionstampedValue, versionKey, buildOldFormatVersionstampedValue(version))
	return nil
}

// buildOldFormatVersionstampedValue builds the legacy SET_VERSIONSTAMPED_VALUE payload:
// the raw 12-byte (incomplete) version followed by a 4-byte little-endian offset of 0,
// so FDB writes the committed 10-byte versionstamp at the start of the value.
// Matches Java's saveVersionWithOldFormat (writeTo(VERSION_LENGTH + Integer.BYTES).putInt(0)).
func buildOldFormatVersionstampedValue(version *FDBRecordVersion) []byte {
	raw := version.ToBytes() // 12 bytes: incomplete global (0xFF*10) + local (2)
	out := make([]byte, 0, VersionBytes+4)
	out = append(out, raw...)
	out = append(out, 0, 0, 0, 0) // offset 0
	return out
}

// packVersion packs a complete FDBRecordVersion as a Tuple containing a Versionstamp.
// Matches Java's SplitHelper.packVersion().
func packVersion(version *FDBRecordVersion) ([]byte, error) {
	globalVer, err := version.GetGlobalVersion()
	if err != nil {
		return nil, err
	}
	var txVer [10]byte
	copy(txVer[:], globalVer)
	vs := tuple.Versionstamp{
		TransactionVersion: txVer,
		UserVersion:        uint16(version.GetLocalVersion()),
	}
	return tuple.Tuple{vs}.Pack(), nil
}

// unpackVersion unpacks a stored version value (a packed Tuple with a Versionstamp)
// into an FDBRecordVersion. Matches Java's SplitHelper.unpackVersion().
func unpackVersion(value []byte) (*FDBRecordVersion, error) {
	t, err := fastUnpack(fdb.Key(value))
	if err != nil {
		return nil, fmt.Errorf("failed to unpack version tuple: %w", err)
	}
	if len(t) < 1 {
		return nil, fmt.Errorf("version tuple is empty")
	}
	vs, ok := t[0].(tuple.Versionstamp)
	if !ok {
		return nil, fmt.Errorf("version tuple element is not a Versionstamp: %T", t[0])
	}
	return NewCompleteVersion(vs.TransactionVersion[:], int(vs.UserVersion))
}

// LoadRecordVersion loads the version associated with a record.
// Returns nil if no version is stored or versioning is not enabled.
// Matches Java's FDBRecordStore.loadRecordVersionAsync().
func (store *FDBRecordStore) LoadRecordVersion(primaryKey tuple.Tuple, snapshot bool) (*FDBRecordVersion, error) {
	oldFormat := store.useOldVersionFormat()

	// In the legacy layout the version subspace is cleared whenever the store is not
	// configured to keep versions, so we can answer without any I/O. Matches Java's
	// loadRecordVersionAsync: `useOldVersionFormat() && !metaData.isStoreRecordVersions()`.
	if oldFormat && !store.metaData.IsStoreRecordVersions() {
		return nil, nil
	}

	versionKey := store.versionKey(primaryKey)

	// Check local cache first (for versions saved in the current transaction)
	if localVer, ok := store.context.GetLocalVersion(versionKey); ok {
		v, err := IncompleteVersion(localVer)
		if err != nil {
			return nil, err
		}
		return v, nil
	}

	// Read from FDB
	var value []byte
	var getErr error
	if snapshot {
		value, getErr = store.context.Transaction().Snapshot().Get(fdb.Key(versionKey)).Get()
	} else {
		value, getErr = store.context.Transaction().Get(fdb.Key(versionKey)).Get()
	}
	if getErr != nil {
		return nil, fmt.Errorf("failed to load record version: %w", getErr)
	}

	if value == nil {
		return nil, nil
	}

	if oldFormat {
		// Legacy value is the raw 12-byte FDBRecordVersion. Matches Java's
		// FDBRecordVersion.complete(valueBytes, false) — wrap as complete without
		// re-validating (a stored legacy version is always committed/complete).
		return completeVersionFromBytesUnchecked(value)
	}
	// Modern value is a packed Tuple containing a Versionstamp (SplitHelper.unpackVersion()).
	return unpackVersion(value)
}

// versionKey returns the FDB key for storing a record's version.
//
// Modern layout (format >= SAVE_VERSION_WITH_RECORD and suffix not omitted): inline at
// recordsSubspace.pack(primaryKey, recordVersionSuffix(-1)), matching SplitHelper.RECORD_VERSION.
// Legacy layout (useOldVersionFormat): the separate RecordVersionKey(8) subspace keyed by
// the bare primary key, matching Java's recordVersionKey(): Tuple.from(RECORD_VERSION_KEY).addAll(pk).
func (store *FDBRecordStore) versionKey(primaryKey tuple.Tuple) fdb.Key {
	if store.useOldVersionFormat() {
		return store.subspace.Sub(RecordVersionKey).Pack(primaryKey)
	}
	recordsSubspace := store.subspace.Sub(RecordKey)
	keyTuple := make(tuple.Tuple, len(primaryKey)+1)
	copy(keyTuple, primaryKey)
	keyTuple[len(primaryKey)] = recordVersionSuffix
	return recordsSubspace.Pack(keyTuple)
}
