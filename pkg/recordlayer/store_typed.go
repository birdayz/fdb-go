// Portions derived from FoundationDB Record Layer (FDBTypedRecordStore.java,
// TypedRecordSerializer.java), Copyright 2015-2018 Apple Inc. and the
// FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// GetTypedRecordStore is Java's FDBRecordStore.getTypedRecordStore for a
// TypedRecordSerializer over recordTypeName's union field, with T its record
// message type. The union is the metadata's, whatever it is named.
func GetTypedRecordStore[T proto.Message](store *FDBRecordStore, recordTypeName string) (*TypedFDBRecordStore[T], error) {
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, unknownRecordTypeError(recordTypeName)
	}
	return newTypedRecordStore[T](store, recordType)
}

// typedRecordSerializer is Java's TypedRecordSerializer as the store applies
// it: recordType's union field is its fieldDescriptor, and a record decoded or
// saved through it must be held in that field.
type typedRecordSerializer struct {
	recordType *RecordType
	// builderMatches is false when the metadata decodes recordType as another
	// Go type than the typed store's, Java's union builder of another type.
	builderMatches bool
}

// checkDeserialized is TypedRecordSerializer.deserialize's refusal of a record
// stored as recordType, wrapped as Java's deserializeRecord wraps it.
func (s *typedRecordSerializer) checkDeserialized(primaryKey tuple.Tuple, recordType *RecordType) error {
	switch {
	case !s.builderMatches:
		return &RecordDeserializationError{PrimaryKey: primaryKey, Cause: serializationError("Builder does not match union type")}
	case recordType != s.recordType:
		return &RecordDeserializationError{PrimaryKey: primaryKey, Cause: serializationError("Specified union field was not set")}
	}
	return nil
}

// checkSerialize is TypedRecordSerializer.serialize's refusal of a record of
// recordType.
func (s *typedRecordSerializer) checkSerialize(recordType *RecordType, metaDataVersion int) error {
	switch {
	case !s.builderMatches:
		return &RecordSerializationError{Message: "Builder does not match union type", MetaDataVersion: metaDataVersion}
	case recordType != s.recordType:
		return &RecordSerializationError{Message: "fieldDescriptor does not match recordType", MetaDataVersion: metaDataVersion}
	}
	return nil
}

// TypedFDBRecordStore is Java's FDBTypedRecordStore: the untyped store, read
// and written through a typedRecordSerializer, so records come back as T.
type TypedFDBRecordStore[T proto.Message] struct {
	baseStore *FDBRecordStore
	typedRecordSerializer
}

// NewTypedRecordStore is a typed store over baseStore for recordType, which
// must be a record type of baseStore's metadata. A T of another message than
// recordType's is refused, as Java's TypedRecordSerializer constructor refuses
// a getter of another field (TypedRecordSerializer.java:77-80).
func NewTypedRecordStore[T proto.Message](baseStore *FDBRecordStore, recordType *RecordType) (*TypedFDBRecordStore[T], error) {
	if recordType == nil {
		return nil, &RecordCoreArgumentError{Message: "record type must not be nil"}
	}
	own := baseStore.metaData.GetRecordType(recordType.Name)
	if own == nil {
		return nil, unknownRecordTypeError(recordType.Name)
	}
	// The serializer's checks compare record types by identity.
	if own != recordType {
		return nil, &RecordCoreArgumentError{Message: "fieldDescriptor does not match recordType"}
	}
	return newTypedRecordStore[T](baseStore, recordType)
}

// newTypedRecordStore is NewTypedRecordStore for a record type of baseStore's metadata.
func newTypedRecordStore[T proto.Message](baseStore *FDBRecordStore, recordType *RecordType) (*TypedFDBRecordStore[T], error) {
	var zero T
	switch any(zero).(type) {
	case nil, *dynamicpb.Message:
		// An interface or dynamicpb T holds any message; its records are checked as read.
	default:
		if zero.ProtoReflect().Descriptor().FullName() != recordType.Descriptor.FullName() {
			return nil, &RecordCoreArgumentError{Message: "fieldDescriptor and getter do not match"}
		}
	}
	matches := false
	if recordType.newMessage != nil {
		_, matches = recordType.newMessage().(T)
	}
	return &TypedFDBRecordStore[T]{
		baseStore:             baseStore,
		typedRecordSerializer: typedRecordSerializer{recordType: recordType, builderMatches: matches},
	}, nil
}

// typed is stored as T, the record the serializer accepted.
func (ts *TypedFDBRecordStore[T]) typed(stored *FDBStoredRecord[proto.Message]) (*FDBStoredRecord[T], error) {
	if err := ts.checkDeserialized(stored.PrimaryKey, stored.RecordType); err != nil {
		return nil, err
	}
	record, ok := stored.Record.(T)
	if !ok {
		return nil, &RecordDeserializationError{PrimaryKey: stored.PrimaryKey, Cause: fmt.Errorf("record of type %T is not a %T", stored.Record, record)}
	}
	return withRecord(stored, record), nil
}

// withRecord is stored holding record.
func withRecord[T proto.Message](stored *FDBStoredRecord[proto.Message], record T) *FDBStoredRecord[T] {
	return &FDBStoredRecord[T]{
		PrimaryKey:      stored.PrimaryKey,
		RecordType:      stored.RecordType,
		Record:          record,
		wire:            stored.wire,
		Version:         stored.Version,
		Store:           stored.Store,
		KeyCount:        stored.KeyCount,
		KeySize:         stored.KeySize,
		ValueSize:       stored.ValueSize,
		Split:           stored.Split,
		VersionedInline: stored.VersionedInline,
	}
}

// LoadRecord loads the record with primaryKey, nil when there is none.
func (ts *TypedFDBRecordStore[T]) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord[T], error) {
	storedRecord, err := ts.baseStore.LoadRecord(primaryKey)
	if err != nil || storedRecord == nil {
		return nil, err
	}
	return ts.typed(storedRecord)
}

// SaveRecord saves a typed record to the store
func (ts *TypedFDBRecordStore[T]) SaveRecord(record T) (*FDBStoredRecord[T], error) {
	return ts.SaveRecordWithOptions(record, RecordExistenceCheckNone)
}

// DeleteRecord deletes the record with primaryKey, false when there is none.
func (ts *TypedFDBRecordStore[T]) DeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	return ts.baseStore.deleteRecord(primaryKey, &ts.typedRecordSerializer)
}

// RecordExists checks if a record exists with the given primary key
func (ts *TypedFDBRecordStore[T]) RecordExists(primaryKey tuple.Tuple, isolationLevel IsolationLevel) (bool, error) {
	return ts.baseStore.RecordExists(primaryKey, isolationLevel)
}

// SaveRecordWithOptions saves a typed record with existence checking
func (ts *TypedFDBRecordStore[T]) SaveRecordWithOptions(
	record T,
	existenceCheck RecordExistenceCheck,
) (*FDBStoredRecord[T], error) {
	storedRecord, err := ts.baseStore.saveRecordInternal(record, existenceCheck, false, nil, &ts.typedRecordSerializer)
	if err != nil {
		return nil, err
	}
	return withRecord(storedRecord, record), nil
}

// InsertRecord saves a typed record and throws an error if it already exists
func (ts *TypedFDBRecordStore[T]) InsertRecord(record T) (*FDBStoredRecord[T], error) {
	return ts.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfExists)
}

// UpdateRecord saves a typed record and throws an error if it does not already exist or if its type has changed
func (ts *TypedFDBRecordStore[T]) UpdateRecord(record T) (*FDBStoredRecord[T], error) {
	return ts.SaveRecordWithOptions(record, RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
}

// AddRecordReadConflict adds a read conflict range for the given primary key
func (ts *TypedFDBRecordStore[T]) AddRecordReadConflict(primaryKey tuple.Tuple) error {
	return ts.baseStore.AddRecordReadConflict(primaryKey)
}

// AddRecordWriteConflict adds a write conflict range for the given primary key
func (ts *TypedFDBRecordStore[T]) AddRecordWriteConflict(primaryKey tuple.Tuple) error {
	return ts.baseStore.AddRecordWriteConflict(primaryKey)
}

// Context returns the record context this store is using
func (ts *TypedFDBRecordStore[T]) Context() *FDBRecordContext {
	return ts.baseStore.Context()
}

// Subspace returns the subspace this store is using
func (ts *TypedFDBRecordStore[T]) Subspace() subspace.Subspace {
	return ts.baseStore.Subspace()
}

// DeleteAllRecords deletes all records from the store
func (ts *TypedFDBRecordStore[T]) DeleteAllRecords() error {
	return ts.baseStore.DeleteAllRecords()
}

// GetRecordCount returns the total record count
func (ts *TypedFDBRecordStore[T]) GetRecordCount() (int64, error) {
	return ts.baseStore.GetRecordCount()
}

// GetSnapshotRecordCount returns the count for a specific count key
func (ts *TypedFDBRecordStore[T]) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) {
	return ts.baseStore.GetSnapshotRecordCount(countKey)
}

// ScanRecords scans every record of the store as T, Java's typed scanRecords:
// a record of another type fails the scan where it is read, with
// RecordDeserializationError ("Specified union field was not set").
func (ts *TypedFDBRecordStore[T]) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[T]] {
	inner := ts.baseStore.ScanRecords(continuation, scanProperties)
	return MapErrCursor(inner, ts.typed)
}
