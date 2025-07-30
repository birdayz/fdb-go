package recordlayer

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"google.golang.org/protobuf/proto"
	
	"github.com/birdayz/fdb-record-layer-go/gen"
)

// FDBRecordStore provides record storage operations within a transaction context.
// This is the main struct for storing and retrieving records.
type FDBRecordStore struct {
	context   *FDBRecordContext
	metaData  *RecordMetaData
	subspace  subspace.Subspace
}

// LoadRecord loads a record by its primary key
func (store *FDBRecordStore) LoadRecord(primaryKey tuple.Tuple) (*FDBStoredRecord, error) {
	// We need to try loading records with all possible record type indices
	// since we don't know which record type we're looking for
	recordsSubspace := store.subspace.Sub(RecordKey)
	
	for _, recordType := range store.metaData.RecordTypes() {
		// Construct the key including the record type index
		keyTuple := append(primaryKey, recordType.GetRecordTypeIndex())
		recordKey := recordsSubspace.Pack(keyTuple)
		
		// Get the value from FDB
		value := store.context.Transaction().Get(recordKey).MustGet()
		if value != nil {
			// Found the record! Now deserialize it
			protoMessage, err := store.deserializeRecord(value, recordType)
			if err != nil {
				return nil, fmt.Errorf("failed to deserialize record: %w", err)
			}
			
			return &FDBStoredRecord{
				PrimaryKey:   primaryKey,
				RecordType:   recordType,
				ProtoMessage: protoMessage,
				ValueSize:    len(value),
				KeySize:      len(recordKey),
				Split:        false,
			}, nil
		}
	}
	
	return nil, nil // Record not found with any record type
}

// SaveRecord saves a protobuf record to the store
func (store *FDBRecordStore) SaveRecord(record proto.Message) (*FDBStoredRecord, error) {
	// Extract the primary key from the record
	recordTypeName := string(record.ProtoReflect().Descriptor().Name())
	recordType := store.metaData.GetRecordType(recordTypeName)
	if recordType == nil {
		return nil, fmt.Errorf("unknown record type: %s", recordTypeName)
	}

	if recordType.PrimaryKey == nil {
		return nil, fmt.Errorf("no primary key defined for record type: %s", recordTypeName)
	}

	// Extract primary key values using the key expression
	keyValues, err := recordType.PrimaryKey.Evaluate(record)
	if err != nil {
		return nil, fmt.Errorf("failed to extract primary key: %w", err)
	}

	// Create primary key tuple  
	primaryKey := make(tuple.Tuple, len(keyValues))
	for i, v := range keyValues {
		primaryKey[i] = v
	}

	// Get record type index (position in union descriptor)
	recordTypeIndex := recordType.GetRecordTypeIndex()

	// Wrap the record in a UnionDescriptor like Java Record Layer does
	// This ensures compatibility with Java's serialization format
	unionRecord, err := store.wrapInUnion(record, recordType)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap record in union: %w", err)
	}

	// Serialize the union message
	data, err := proto.Marshal(unionRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal union record: %w", err)
	}

	// Construct the key for the record using the RECORD_KEY subspace
	// Key structure: [RecordKey, ...primaryKeyValues, recordTypeIndex]
	recordsSubspace := store.subspace.Sub(RecordKey)
	keyTuple := append(primaryKey, recordTypeIndex)
	recordKey := recordsSubspace.Pack(keyTuple)

	// Store the record in FDB
	store.context.Transaction().Set(recordKey, data)

	// Return the stored record
	return &FDBStoredRecord{
		PrimaryKey:   primaryKey,
		RecordType:   recordType,
		ProtoMessage: record,
		ValueSize:    len(data),
		KeySize:      len(recordKey),
		Split:        false,
	}, nil
}

// Context returns the record context this store is using
func (store *FDBRecordStore) Context() *FDBRecordContext {
	return store.context
}

// Subspace returns the subspace this store is using
func (store *FDBRecordStore) Subspace() subspace.Subspace {
	return store.subspace
}

// FDBStoredRecord represents a record that has been stored in or loaded from FDB
type FDBStoredRecord struct {
	// PrimaryKey is the record's primary key
	PrimaryKey tuple.Tuple

	// RecordType is the type of this record
	RecordType *RecordType

	// ProtoMessage is the actual record data
	ProtoMessage proto.Message

	// Version is the record's version (optional)
	Version *FDBRecordVersion

	// Storage size information
	KeyCount  int
	KeySize   int
	ValueSize int

	// Whether the record is split across multiple keys
	Split bool
}

// FDBRecordVersion represents version information for a record
type FDBRecordVersion struct {
	// Version is the actual version value
	Version int64

	// IsComplete indicates if this is a complete version
	IsComplete bool
}

// StoreBuilder builds an FDBRecordStore with configuration options.
// This follows the builder pattern from Java but adapted for Go.
type StoreBuilder struct {
	context      *FDBRecordContext
	metaData     *RecordMetaData
	subspace     subspace.Subspace
	createOrOpen bool
}

// NewStoreBuilder creates a new store builder
func NewStoreBuilder() *StoreBuilder {
	return &StoreBuilder{}
}

// SetContext sets the record context
func (b *StoreBuilder) SetContext(ctx *FDBRecordContext) *StoreBuilder {
	b.context = ctx
	return b
}

// SetMetaDataProvider sets the metadata
func (b *StoreBuilder) SetMetaDataProvider(metaData *RecordMetaData) *StoreBuilder {
	b.metaData = metaData
	return b
}

// SetSubspace sets the subspace for this store
func (b *StoreBuilder) SetSubspace(subspace subspace.Subspace) *StoreBuilder {
	b.subspace = subspace
	return b
}

// CreateOrOpen sets whether to create the store if it doesn't exist
func (b *StoreBuilder) CreateOrOpen() *StoreBuilder {
	b.createOrOpen = true
	return b
}

// Open opens an existing record store
func (b *StoreBuilder) Open() (*FDBRecordStore, error) {
	if b.context == nil {
		return nil, fmt.Errorf("context is required")
	}
	if b.metaData == nil {
		return nil, fmt.Errorf("metadata is required")
	}
	if b.subspace.Bytes() == nil {
		return nil, fmt.Errorf("subspace is required")
	}

	store := &FDBRecordStore{
		context:  b.context,
		metaData: b.metaData,
		subspace: b.subspace,
	}

	// TODO: If createOrOpen is true, initialize store metadata in FDB
	
	return store, nil
}

// Build is an alias for CreateOrOpen().Open() to match Java pattern
func (b *StoreBuilder) Build() (*FDBRecordStore, error) {
	if b.createOrOpen {
		return b.Open()
	}
	return b.Open()
}

// wrapInUnion wraps a record message in a UnionDescriptor for storage compatibility with Java
func (store *FDBRecordStore) wrapInUnion(record proto.Message, recordType *RecordType) (proto.Message, error) {
	// Create a UnionDescriptor and set the appropriate field
	union := &gen.UnionDescriptor{}
	
	switch recordType.Name {
	case "Order":
		// Cast the record to Order and set it in the union
		orderRecord, ok := record.(*gen.Order)
		if !ok {
			return nil, fmt.Errorf("expected *gen.Order, got %T", record)
		}
		union.XOrder = orderRecord
		return union, nil
	default:
		return nil, fmt.Errorf("unsupported record type for union wrapping: %s", recordType.Name)
	}
}

// deserializeRecord unwraps a UnionDescriptor and extracts the actual record
func (store *FDBRecordStore) deserializeRecord(data []byte, recordType *RecordType) (proto.Message, error) {
	// First, deserialize the UnionDescriptor
	union := &gen.UnionDescriptor{}
	if err := proto.Unmarshal(data, union); err != nil {
		return nil, fmt.Errorf("failed to unmarshal union descriptor: %w", err)
	}
	
	// Extract the specific record type from the union
	switch recordType.Name {
	case "Order":
		if union.XOrder == nil {
			return nil, fmt.Errorf("union descriptor does not contain Order record")
		}
		return union.XOrder, nil
	default:
		return nil, fmt.Errorf("unsupported record type for deserialization: %s", recordType.Name)
	}
}

// Alternative Go-idiomatic approach using functional options:
//
// type StoreOption func(*storeConfig)
//
// func WithMetadata(m *RecordMetaData) StoreOption {
//     return func(c *storeConfig) { c.metadata = m }
// }
//
// func NewFDBRecordStore(ctx FDBRecordContext, opts ...StoreOption) (FDBRecordStore, error)
//
// We'll stick with builder pattern for now to match Java more closely