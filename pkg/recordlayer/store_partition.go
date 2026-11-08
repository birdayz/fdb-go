package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// PartitionCleanupError reports a partition clear that cannot be performed safely.
type PartitionCleanupError struct {
	RecordType string
	IndexName  string
	Reason     string
}

func (e *PartitionCleanupError) Error() string {
	return fmt.Sprintf("partition cleanup: %s (record_type=%q, index=%q)", e.Reason, e.RecordType, e.IndexName)
}

// ClearPartitionData clears physical interval data, not partition existence or lifecycle state.
// Callers must exclude concurrent writers; this primitive is not DROP PARTITION.
func (store *FDBRecordStore) ClearPartitionData(recordTypeName string, coordinate int64) error {
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return err
	}
	defer finishWrite()
	if err := store.validateRecordUpdateAllowed(); err != nil {
		return err
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	store.indexStateView.maintenance.RLock()
	defer store.indexStateView.maintenance.RUnlock()

	rt := store.metaData.GetRecordType(recordTypeName)
	if rt == nil {
		return &PartitionCleanupError{RecordType: recordTypeName, Reason: "unknown record type"}
	}
	spec, err := rt.PartitionSpec()
	if err != nil {
		return err
	}
	if spec == nil {
		return &PartitionCleanupError{RecordType: recordTypeName, Reason: "record type is not partitioned"}
	}
	for _, other := range store.metaData.RecordTypes() {
		if err := validatePartitionTypeIsolation(other.Name, other.PrimaryKey); err != nil {
			return err
		}
	}
	if store.metaData.recordCountKey != nil || len(store.metaData.universalIndexes) != 0 || len(rt.multiTypeIndexes) != 0 {
		return &PartitionCleanupError{RecordType: recordTypeName, Reason: "unsupported counts or index coverage"}
	}

	tx := store.context.Transaction()
	physicalPrefix := tuple.Tuple{rt.GetRecordTypeKey(), coordinate}
	var ranges []fdb.KeyRange
	addRange := func(key []byte) error {
		r, err := fdb.PrefixRange(key)
		if err != nil {
			return err
		}
		ranges = append(ranges, r)
		return nil
	}
	if err := addRange(store.recordsSubspace.Pack(physicalPrefix)); err != nil {
		return err
	}
	if err := addRange(store.subspace.Sub(RecordVersionKey).Pack(physicalPrefix)); err != nil {
		return err
	}
	for _, idx := range rt.indexes {
		if err := validatePartitionIndex(recordTypeName, rt.PrimaryKey, spec, idx); err != nil {
			return err
		}
		state, err := store.readIndexState(idx.Name)
		if err != nil {
			return err
		}
		if state.IsWriteOnly() {
			return &PartitionCleanupError{RecordType: recordTypeName, IndexName: idx.Name, Reason: "index is WRITE_ONLY"}
		}
		// Refuse even stale/invalid heartbeat entries rather than infer that an
		// unfenced builder is harmless. The empty read conflicts with admission.
		heartbeats, err := tx.GetRange(heartbeatSubspace(store.subspace, idx), fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		if err != nil {
			return err
		}
		if len(heartbeats) != 0 {
			return &PartitionCleanupError{RecordType: recordTypeName, IndexName: idx.Name, Reason: "index build heartbeat is present"}
		}
		empty, err := store.isIndexPendingQueueEmpty(idx)
		if err != nil {
			return err
		}
		if !empty {
			return &PartitionCleanupError{RecordType: recordTypeName, IndexName: idx.Name, Reason: "pending index writes are present"}
		}
		if err := addRange(store.indexSubspace(idx).Pack(tuple.Tuple{coordinate})); err != nil {
			return err
		}
		if idx.Type == IndexTypeText {
			if err := addRange(store.indexSecondarySubspace(idx).Sub(tokenizerVersionSubspaceTuple...).Pack(physicalPrefix)); err != nil {
				return err
			}
		}
		if idx.IsUnique() {
			if err := addRange(store.subspace.Sub(IndexUniquenessViolationsKey, idx.SubspaceTupleKey()).Pack(tuple.Tuple{coordinate})); err != nil {
				return err
			}
		}
	}

	// All validation, serializable exclusion reads and range construction finish
	// before the first clear, so a caller may safely commit a refused operation.
	finishPhysical, err := store.beginPhysicalBulk()
	if err != nil {
		return err
	}
	defer finishPhysical()
	for _, r := range ranges {
		tx.ClearRange(r)
		store.removeVersionDataInRange(r)
	}
	return nil
}
