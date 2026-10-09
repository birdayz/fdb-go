package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// standardIndexMaintainerWithQueue is Java's StandardIndexMaintainerWithQueue:
// a standard maintainer that supports the pending write queue by enqueuing the
// serialized old and new records and replaying them through
// UpdateWhileWriteOnly, so a replayed update is not queued again.
type standardIndexMaintainerWithQueue struct {
	StandardIndexMaintainer
}

// pendingRecordStore is the part of the store a queued record round-trips
// through: Java's state.store.getSerializer() and getRecordMetaData().
type pendingRecordStore interface {
	serializePendingRecord(record *FDBStoredRecord[proto.Message]) ([]byte, error)
	deserializePendingRecord(serialized []byte) (*FDBStoredRecord[proto.Message], error)
	GetRecordMetaData() *RecordMetaData
}

// IsPendingWriteQueueAllowed is Java's isPendingWriteQueueAllowed: the
// maintainer is idempotent, which a standard maintainer is
// (StandardIndexMaintainer.isIdempotent), and the index maintains no synthetic
// record type.
func (m *standardIndexMaintainerWithQueue) IsPendingWriteQueueAllowed() bool {
	store, ok := m.store.(pendingRecordStore)
	if !ok {
		return false
	}
	for _, rt := range store.GetRecordMetaData().RecordTypesForIndex(m.index) {
		if rt.IsSynthetic() {
			return false
		}
	}
	return true
}

// SerializePendingWriteQueue is Java's serializePendingWrites.
func (m *standardIndexMaintainerWithQueue) SerializePendingWriteQueue(oldRecord, newRecord *FDBStoredRecord[proto.Message]) (*anypb.Any, error) {
	store, ok := m.store.(pendingRecordStore)
	if !ok {
		return nil, &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
	}
	records := &gen.OldAndNewRecords{}
	if oldRecord != nil {
		b, err := store.serializePendingRecord(oldRecord)
		if err != nil {
			return nil, err
		}
		records.OldRecords = b
	}
	if newRecord != nil {
		b, err := store.serializePendingRecord(newRecord)
		if err != nil {
			return nil, err
		}
		records.NewRecord = b
	}
	return anypb.New(records)
}

// UpdateFromQueue is Java's updateFromQueue: it applies the queued records
// through UpdateWhileWriteOnly, lest the update be queued again.
func (m *standardIndexMaintainerWithQueue) UpdateFromQueue(data *anypb.Any) error {
	store, ok := m.store.(pendingRecordStore)
	if !ok {
		return &UnsupportedOperationError{Message: m.index.Name + " does not support the pending write queue"}
	}
	records := &gen.OldAndNewRecords{}
	if err := UnmarshalPendingQueueAny(data, records); err != nil {
		return &RecordCoreError{Message: "failed to parse pending write queue entry data", Cause: err}
	}
	var oldRecord, newRecord *FDBStoredRecord[proto.Message]
	var err error
	if records.OldRecords != nil {
		if oldRecord, err = store.deserializePendingRecord(records.OldRecords); err != nil {
			return err
		}
	}
	if records.NewRecord != nil {
		if newRecord, err = store.deserializePendingRecord(records.NewRecord); err != nil {
			return err
		}
	}
	return m.UpdateWhileWriteOnly(oldRecord, newRecord)
}

// serializePendingRecord is Java's serializePendingRecord: the record as the
// store's serializer writes it.
func (store *FDBRecordStore) serializePendingRecord(record *FDBStoredRecord[proto.Message]) ([]byte, error) {
	union, err := serializeUnionOver(record.Record, record.RecordType, nil)
	if err != nil {
		return nil, &RecordSerializationError{Cause: err}
	}
	return store.writeStoredRecord(union, record.RecordType, record.PrimaryKey)
}

// deserializePendingRecord is Java's deserializePendingRecord: the record
// read back through the store's serializer, its type from its descriptor and
// its primary key evaluated from that type.
func (store *FDBRecordStore) deserializePendingRecord(serialized []byte) (*FDBStoredRecord[proto.Message], error) {
	recordType, msg, wire, err := store.deserializeAndDiscover(serialized)
	if err != nil {
		return nil, err
	}
	rec := &FDBStoredRecord[proto.Message]{RecordType: recordType, Record: msg, Store: store, wire: wire}
	keyValues, err := evaluateKeyFlat(recordType.PrimaryKey, rec, msg)
	if err != nil {
		return nil, fmt.Errorf("failed to extract primary key: %w", err)
	}
	rec.PrimaryKey = make(tuple.Tuple, len(keyValues))
	for i, v := range keyValues {
		rec.PrimaryKey[i] = v
	}
	return rec, nil
}
