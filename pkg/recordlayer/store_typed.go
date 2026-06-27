package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
)

// GetTypedRecordStore creates a type-safe wrapper for a specific record type
// This follows Java's FDBRecordStore.getTypedRecordStore() pattern
func GetTypedRecordStore[T proto.Message](store *FDBRecordStore, recordTypeName string) (*TypedFDBRecordStore[T], error) {
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("record type '%s' not found in metadata", recordTypeName)}
	}

	// Use reflection to create the wrap/unwrap functions automatically
	return NewTypedRecordStore[T](
		store,
		recordType,
		createUnwrapFunc[T](recordType),
		createWrapFunc[T](recordType),
	), nil
}

// createUnwrapFunc creates an unwrap function using reflection
func createUnwrapFunc[T proto.Message](recordType *RecordType) func(*gen.UnionDescriptor) (T, error) {
	return func(union *gen.UnionDescriptor) (T, error) {
		var zero T

		if recordType.UnionFieldDescriptor == nil {
			return zero, fmt.Errorf("no union field descriptor for record type: %s", recordType.Name)
		}

		// Get the union message reflection
		unionReflect := union.ProtoReflect()

		// Get the field value using reflection
		fieldValue := unionReflect.Get(recordType.UnionFieldDescriptor)
		if !fieldValue.IsValid() || !fieldValue.Message().IsValid() {
			return zero, fmt.Errorf("union descriptor does not contain %s record", recordType.Name)
		}

		// Type assert to T
		result, ok := fieldValue.Message().Interface().(T)
		if !ok {
			return zero, fmt.Errorf("union field type mismatch: expected %T, got %T", zero, fieldValue.Message().Interface())
		}

		return result, nil
	}
}

// createWrapFunc creates a wrap function using reflection
func createWrapFunc[T proto.Message](recordType *RecordType) func(T) (*gen.UnionDescriptor, error) {
	return func(record T) (*gen.UnionDescriptor, error) {
		if recordType.UnionFieldDescriptor == nil {
			return nil, fmt.Errorf("no union field descriptor for record type: %s", recordType.Name)
		}

		// Create a UnionDescriptor
		union := &gen.UnionDescriptor{}

		// Get the union message reflection
		unionReflect := union.ProtoReflect()

		// Set the field using reflection
		unionReflect.Set(recordType.UnionFieldDescriptor, protoreflect.ValueOfMessage(record.ProtoReflect()))

		return union, nil
	}
}

// TypedFDBRecordStore provides type-safe record operations with generics
type TypedFDBRecordStore[T proto.Message] struct {
	baseStore  *FDBRecordStore
	recordType *RecordType
	unwrapFunc func(*gen.UnionDescriptor) (T, error)
	wrapFunc   func(T) (*gen.UnionDescriptor, error)
}

// NewTypedRecordStore creates a new typed record store that wraps the base store
func NewTypedRecordStore[T proto.Message](
	baseStore *FDBRecordStore,
	recordType *RecordType,
	unwrapFunc func(*gen.UnionDescriptor) (T, error),
	wrapFunc func(T) (*gen.UnionDescriptor, error),
) *TypedFDBRecordStore[T] {
	return &TypedFDBRecordStore[T]{
		baseStore:  baseStore,
		recordType: recordType,
		unwrapFunc: unwrapFunc,
		wrapFunc:   wrapFunc,
	}
}

// LoadRecord loads a record by its primary key with compile-time type safety
func (ts *TypedFDBRecordStore[T]) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord[T], error) {
	// Use base store to load the raw record
	storedRecord, err := ts.baseStore.LoadRecord(primaryKey)
	if err != nil || storedRecord == nil {
		return nil, err
	}

	// The base store returns the unwrapped record (e.g., *gen.Order)
	// We need to type-assert it to our generic type T
	typedRecord, ok := storedRecord.Record.(T)
	if !ok {
		return nil, fmt.Errorf("loaded record type %T does not match expected type %T", storedRecord.Record, *new(T))
	}

	return &FDBStoredRecord[T]{
		PrimaryKey: storedRecord.PrimaryKey,
		RecordType: storedRecord.RecordType,
		Record:     typedRecord,
		Version:    storedRecord.Version,
		Store:      storedRecord.Store,
		KeyCount:   storedRecord.KeyCount,
		KeySize:    storedRecord.KeySize,
		ValueSize:  storedRecord.ValueSize,
		Split:      storedRecord.Split,
	}, nil
}

// SaveRecord saves a typed record to the store
func (ts *TypedFDBRecordStore[T]) SaveRecord(record T) (*FDBStoredRecord[T], error) {
	// Use base store to save - it will handle UnionDescriptor wrapping
	storedRecord, err := ts.baseStore.SaveRecord(record)
	if err != nil {
		return nil, err
	}

	return &FDBStoredRecord[T]{
		PrimaryKey: storedRecord.PrimaryKey,
		RecordType: storedRecord.RecordType,
		Record:     record, // Return the original typed record
		Version:    storedRecord.Version,
		Store:      storedRecord.Store,
		KeyCount:   storedRecord.KeyCount,
		KeySize:    storedRecord.KeySize,
		ValueSize:  storedRecord.ValueSize,
		Split:      storedRecord.Split,
	}, nil
}

// DeleteRecord deletes a typed record by its primary key
func (ts *TypedFDBRecordStore[T]) DeleteRecord(primaryKey tuple.Tuple) (bool, error) {
	// Delegate to the base store for the actual deletion logic
	return ts.baseStore.DeleteRecord(primaryKey)
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
	// Use base store to save with options
	storedRecord, err := ts.baseStore.SaveRecordWithOptions(record, existenceCheck)
	if err != nil {
		return nil, err
	}

	return &FDBStoredRecord[T]{
		PrimaryKey: storedRecord.PrimaryKey,
		RecordType: storedRecord.RecordType,
		Record:     record, // Return the original typed record
		Version:    storedRecord.Version,
		Store:      storedRecord.Store,
		KeyCount:   storedRecord.KeyCount,
		KeySize:    storedRecord.KeySize,
		ValueSize:  storedRecord.ValueSize,
		Split:      storedRecord.Split,
	}, nil
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

// ScanRecords returns a typed cursor scanning all records of this store's type.
// Records are auto-filtered to the store's type and type-asserted to T.
func (ts *TypedFDBRecordStore[T]) ScanRecords(continuation []byte, scanProperties ScanProperties) RecordCursor[*FDBStoredRecord[T]] {
	inner := ts.baseStore.ScanRecordsByType(ts.recordType.Name, continuation, scanProperties)
	return MapCursor(inner, func(r *FDBStoredRecord[proto.Message]) *FDBStoredRecord[T] {
		typed, ok := r.Record.(T)
		if !ok {
			panic("unreachable: ScanRecordsByType returned record that doesn't match type parameter T")
		}
		return &FDBStoredRecord[T]{
			PrimaryKey: r.PrimaryKey,
			RecordType: r.RecordType,
			Record:     typed,
			Version:    r.Version,
			Store:      r.Store,
			KeyCount:   r.KeyCount,
			KeySize:    r.KeySize,
			ValueSize:  r.ValueSize,
			Split:      r.Split,
		}
	})
}
