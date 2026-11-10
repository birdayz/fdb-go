package recordlayer

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func (store *FDBRecordStore) partitionLegacyVersionsPending() bool {
	prefix := store.subspace.Sub(RecordVersionKey).Bytes()
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return true
	}
	if store.context.HasVersionMutationsInRange(r.Begin.FDBKey(), r.End.FDBKey()) {
		return true
	}
	store.context.versionMu.Lock()
	defer store.context.versionMu.Unlock()
	for key := range store.context.localVersionCache {
		if bytes.HasPrefix([]byte(key), prefix) {
			return true
		}
	}
	return false
}

// Partition conversion validates committed bodies before taking preparation or
// physical locks. The enclosing reconciliation guard lasts through header publication.
func (store *FDBRecordStore) convertPartitionRecordVersionsToInline(parent *partitionMutation) (resultErr error) {
	if store.partitionLegacyVersionsPending() {
		return &PartitionStateError{Reason: "format conversion requires committed legacy versions"}
	}
	legacy := store.subspace.Sub(RecordVersionKey)
	rows, err := store.context.Transaction().GetRange(legacy, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return err
	}
	type converted struct {
		pk     tuple.Tuple
		value  []byte
		rt     *RecordType
		record proto.Message
	}
	convertedRows := make([]converted, 0, len(rows))
	for _, row := range rows {
		pk, err := legacy.Unpack(row.Key)
		if err != nil {
			return fmt.Errorf("unpack legacy version key: %w", err)
		}
		version, err := completeVersionFromBytesUnchecked(row.Value)
		if err != nil {
			return fmt.Errorf("decode legacy version: %w", err)
		}
		value, err := packVersion(version)
		if err != nil {
			return err
		}
		var rt *RecordType
		if len(pk) != 0 {
			for _, candidate := range store.metaData.RecordTypes() {
				if recordTypeKeyEquals(pk[0], candidate.GetRecordTypeKey()) {
					rt = candidate
					break
				}
			}
		}
		if rt == nil {
			return &PartitionStateError{Reason: "legacy version has no known record type prefix"}
		}
		var record proto.Message
		if len(partitionFunctions(rt.PrimaryKey)) != 0 {
			load := store.startRecordLoad(store.context.Transaction(), pk)
			var sizes sizeInfo
			data, _, err := load.raw.finish(&sizes)
			if err != nil {
				return err
			}
			if data == nil {
				return partitionStateError(rt, 0, "legacy version has no record body", nil)
			}
			actualType, decoded, _, err := store.deserializeAndDiscover(data)
			if err != nil {
				return &RecordDeserializationError{PrimaryKey: pk, Cause: err}
			}
			if actualType != rt {
				return partitionStateError(rt, 0, "legacy version body has the wrong record type", nil)
			}
			record = decoded
		}
		convertedRows = append(convertedRows, converted{pk: pk, value: value, rt: rt, record: record})
	}
	finishWrite, err := store.context.beginRecordWrite()
	if err != nil {
		return err
	}
	defer finishWrite()
	op, err := store.beginPartitionMutation()
	if err != nil {
		return err
	}
	defer func() { op.finish(resultErr) }()
	for _, row := range convertedRows {
		if row.rt != nil {
			if err := op.prepareRoute(row.rt, row.pk, row.record, false); err != nil {
				return err
			}
		}
	}
	if err := parent.collectHeaders(op); err != nil {
		return err
	}
	finishPhysical, err := store.beginPhysicalBulk()
	if err != nil {
		return err
	}
	defer finishPhysical()
	if store.partitionLegacyVersionsPending() {
		return &PartitionStateError{Reason: "format conversion requires committed legacy versions"}
	}
	op.apply()
	if parent != nil {
		parent.effects = true
	}
	for _, row := range convertedRows {
		store.context.Transaction().Set(store.versionKey(row.pk), row.value)
		if err := store.context.CheckTransactionSize(); err != nil {
			return err
		}
	}
	store.context.Transaction().ClearRange(legacy)
	op.succeed()
	return nil
}
