package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// saveRecordVersion stores the version for a record using the new inline format.
// Version is stored adjacent to the record at recordsSubspace.pack(primaryKey, -1),
// matching Java's SplitHelper.RECORD_VERSION for format version >= 6.
// The value is a packed Tuple containing a Versionstamp, matching Java's
// SplitHelper.packVersion(). For incomplete versions, queues a SET_VERSIONSTAMPED_VALUE.
// If si is non-nil, updates it with version key/value byte counts.
// Matches Java's SplitHelper.writeVersion(context, subspace, key, version, sizeInfo).
func (store *FDBRecordStore) saveRecordVersion(primaryKey tuple.Tuple, version *FDBRecordVersion, si *sizeInfo) error {
	versionKey := store.versionKey(primaryKey)

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
	t, err := tuple.Unpack(fdb.Key(value))
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

	// Value is a packed Tuple containing a Versionstamp (matching Java's SplitHelper.unpackVersion())
	return unpackVersion(value)
}

// versionKey returns the FDB key for storing a record's version.
// Uses the new inline format: recordsSubspace.pack(primaryKey, recordVersionSuffix).
// Matches Java's SplitHelper.RECORD_VERSION = -1L for format version >= 6.
func (store *FDBRecordStore) versionKey(primaryKey tuple.Tuple) fdb.Key {
	recordsSubspace := store.subspace.Sub(RecordKey)
	keyTuple := make(tuple.Tuple, len(primaryKey)+1)
	copy(keyTuple, primaryKey)
	keyTuple[len(primaryKey)] = recordVersionSuffix
	return recordsSubspace.Pack(keyTuple)
}
