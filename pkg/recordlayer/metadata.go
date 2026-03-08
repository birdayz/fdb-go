package recordlayer

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// RecordMetaData describes the schema for records stored in a record store.
// This is a simplified version for our MVP - just enough to define record types
// and their primary keys.
type RecordMetaData struct {
	// Map of record type names to their definitions
	recordTypes map[string]*RecordType

	// The protobuf file descriptor
	fileDescriptor protoreflect.FileDescriptor

	// Schema version
	version int

	// RecordCountKey is the key expression used for maintaining record counts.
	// If nil, record counting is disabled (matching Java's behavior).
	// Java equivalent: RecordMetaData.getRecordCountKey()
	recordCountKey KeyExpression

	// storeRecordVersions controls whether record versions are stored.
	// When true, each save assigns an FDBRecordVersion using SET_VERSIONSTAMPED_VALUE.
	// Java equivalent: RecordMetaData.isStoreRecordVersions()
	storeRecordVersions bool

	// splitLongRecords controls whether records >100KB are split across
	// multiple FDB key-value pairs. When true, records exceeding
	// SplitRecordSize (100KB) are split into chunks. When false,
	// attempting to save a record >100KB returns an error.
	// Java equivalent: RecordMetaData.isSplitLongRecords()
	splitLongRecords bool

	// indexes holds all indexes by name (for lookup and HasIndexes check).
	// Java equivalent: RecordMetaData.getAllIndexes()
	indexes map[string]*Index

	// universalIndexes apply to all record types.
	// Java equivalent: RecordMetaData.getUniversalIndexes()
	universalIndexes []*Index
}

// RecordType represents a type of record that can be stored
type RecordType struct {
	// Name of the record type (usually the protobuf message name)
	Name string

	// Protobuf message descriptor
	Descriptor protoreflect.MessageDescriptor

	// Primary key definition
	PrimaryKey KeyExpression

	// Since version (for schema evolution)
	SinceVersion int

	// Record type index in union descriptor (for key construction)
	RecordTypeIndex int

	// Union field descriptor for reflection-based access
	UnionFieldDescriptor protoreflect.FieldDescriptor

	// indexes defined for this record type
	indexes []*Index
}

// KeyExpression represents an expression that extracts key components from a record.
// For MVP, we'll just support simple field access.
type KeyExpression interface {
	// Evaluate extracts the key value(s) from a record
	Evaluate(msg proto.Message) ([]interface{}, error)

	// FieldNames returns the field names this expression accesses
	FieldNames() []string
}

// RecordMetaDataBuilder provides a builder pattern for creating RecordMetaData
// This matches the Java RecordMetaDataBuilder pattern
type RecordMetaDataBuilder struct {
	recordTypes         map[string]*RecordType
	fileDescriptor      protoreflect.FileDescriptor
	version             int
	recordCountKey      KeyExpression
	storeRecordVersions bool
	splitLongRecords    bool
	indexes             map[string]*Index
	universalIndexes    []*Index
}

// NewRecordMetaDataBuilder creates a new builder
func NewRecordMetaDataBuilder() *RecordMetaDataBuilder {
	return &RecordMetaDataBuilder{
		recordTypes: make(map[string]*RecordType),
		version:     0, // Start with version 0 to match Java defaults
	}
}

// SetRecords sets the protobuf file descriptor containing record definitions
func (b *RecordMetaDataBuilder) SetRecords(fd protoreflect.FileDescriptor) *RecordMetaDataBuilder {
	b.fileDescriptor = fd
	
	// Find the UnionDescriptor to map fields to record types
	unionDesc := fd.Messages().ByName("UnionDescriptor")
	if unionDesc == nil {
		// If no UnionDescriptor, treat each message as a separate record type
		b.setRecordsWithoutUnion(fd)
		return b
	}
	
	// Auto-discover record types from UnionDescriptor fields
	unionFields := unionDesc.Fields()
	recordTypeIndex := 0
	
	for i := 0; i < unionFields.Len(); i++ {
		field := unionFields.Get(i)
		fieldName := string(field.Name())
		
		// Skip non-record fields (field names like "_Order" map to "Order" record type)
		if len(fieldName) > 1 && fieldName[0] == '_' {
			recordTypeName := fieldName[1:] // "_Order" -> "Order"
			
			// Find the actual message descriptor for this record type
			recordMsgDesc := fd.Messages().ByName(protoreflect.Name(recordTypeName))
			if recordMsgDesc == nil {
				continue // Skip if message not found
			}
			
			recordType := &RecordType{
				Name:                 recordTypeName,
				Descriptor:           recordMsgDesc,
				PrimaryKey:           nil, // Will be set explicitly
				SinceVersion:         1,
				RecordTypeIndex:      recordTypeIndex,
				UnionFieldDescriptor: field, // Store the union field for reflection
			}
			b.recordTypes[recordTypeName] = recordType
			recordTypeIndex++
		}
	}
	
	return b
}

// setRecordsWithoutUnion handles schemas without UnionDescriptor (fallback)
func (b *RecordMetaDataBuilder) setRecordsWithoutUnion(fd protoreflect.FileDescriptor) {
	messages := fd.Messages()
	recordTypeIndex := 0
	for i := 0; i < messages.Len(); i++ {
		msg := messages.Get(i)
		// Skip UnionDescriptor and other internal messages
		if msg.Name() != "UnionDescriptor" {
			recordType := &RecordType{
				Name:                 string(msg.Name()),
				Descriptor:           msg,
				PrimaryKey:           nil, // Will be set explicitly
				SinceVersion:         1,
				RecordTypeIndex:      recordTypeIndex,
				UnionFieldDescriptor: nil, // No union field
			}
			b.recordTypes[string(msg.Name())] = recordType
			recordTypeIndex++
		}
	}
}

// SetRecordCountKey sets the key expression for partitioning record counts.
// If set, the store will maintain record counts using FDB atomic ADD mutations.
// If nil (default), record counting is disabled.
// Java equivalent: RecordMetaDataBuilder.setRecordCountKey(KeyExpression)
func (b *RecordMetaDataBuilder) SetRecordCountKey(key KeyExpression) *RecordMetaDataBuilder {
	b.recordCountKey = key
	return b
}

// SetStoreRecordVersions enables or disables automatic record versioning.
// When enabled, each save assigns an FDBRecordVersion to the record.
// Java equivalent: RecordMetaDataBuilder.setStoreRecordVersions(boolean)
func (b *RecordMetaDataBuilder) SetStoreRecordVersions(store bool) *RecordMetaDataBuilder {
	b.storeRecordVersions = store
	return b
}

// SetSplitLongRecords enables or disables splitting records >100KB across
// multiple FDB key-value pairs. Matches Java's RecordMetaDataBuilder.setSplitLongRecords(boolean).
func (b *RecordMetaDataBuilder) SetSplitLongRecords(split bool) *RecordMetaDataBuilder {
	b.splitLongRecords = split
	return b
}

// AddIndex adds a secondary index for a specific record type.
// Matches Java's RecordMetaDataBuilder.addIndex(String recordType, Index index).
func (b *RecordMetaDataBuilder) AddIndex(recordTypeName string, index *Index) *RecordMetaDataBuilder {
	rt, ok := b.recordTypes[recordTypeName]
	if !ok {
		return b
	}
	rt.indexes = append(rt.indexes, index)
	if b.indexes == nil {
		b.indexes = make(map[string]*Index)
	}
	b.indexes[index.Name] = index
	return b
}

// AddUniversalIndex adds an index that applies to all record types.
// Matches Java's RecordMetaDataBuilder.addUniversalIndex(Index index).
func (b *RecordMetaDataBuilder) AddUniversalIndex(index *Index) *RecordMetaDataBuilder {
	b.universalIndexes = append(b.universalIndexes, index)
	if b.indexes == nil {
		b.indexes = make(map[string]*Index)
	}
	b.indexes[index.Name] = index
	return b
}

// GetRecordType returns the record type builder for setting primary keys, etc.
func (b *RecordMetaDataBuilder) GetRecordType(name string) *RecordTypeBuilder {
	recordType := b.recordTypes[name]
	if recordType == nil {
		return nil
	}
	return &RecordTypeBuilder{
		recordType: recordType,
		builder:    b,
	}
}

// Build creates the final RecordMetaData.
// The record types map is copied to prevent the builder from mutating the built metadata.
func (b *RecordMetaDataBuilder) Build() *RecordMetaData {
	types := make(map[string]*RecordType, len(b.recordTypes))
	for k, v := range b.recordTypes {
		types[k] = v
	}
	indexes := make(map[string]*Index, len(b.indexes))
	for k, v := range b.indexes {
		indexes[k] = v
	}
	return &RecordMetaData{
		recordTypes:         types,
		fileDescriptor:      b.fileDescriptor,
		version:             b.version,
		recordCountKey:      b.recordCountKey,
		storeRecordVersions: b.storeRecordVersions,
		splitLongRecords:    b.splitLongRecords,
		indexes:             indexes,
		universalIndexes:    b.universalIndexes,
	}
}

// RecordTypeBuilder provides methods to configure a specific record type
type RecordTypeBuilder struct {
	recordType *RecordType
	builder    *RecordMetaDataBuilder
}

// SetPrimaryKey sets the primary key expression for this record type
func (rtb *RecordTypeBuilder) SetPrimaryKey(keyExpr KeyExpression) *RecordTypeBuilder {
	rtb.recordType.PrimaryKey = keyExpr
	return rtb
}

// NewRecordMetaData creates metadata from a protobuf file descriptor
// This is a convenience function that matches the Java pattern
func NewRecordMetaData(fd protoreflect.FileDescriptor) *RecordMetaData {
	return NewRecordMetaDataBuilder().SetRecords(fd).Build()
}

// GetRecordType returns the record type for the given name
func (m *RecordMetaData) GetRecordType(name string) *RecordType {
	return m.recordTypes[name]
}

// RecordTypes returns all record types
func (m *RecordMetaData) RecordTypes() map[string]*RecordType {
	return m.recordTypes
}

// Version returns the metadata version
func (m *RecordMetaData) Version() int {
	return m.version
}

// GetRecordCountKey returns the key expression used for record counting.
// Returns nil if counting is disabled.
func (m *RecordMetaData) GetRecordCountKey() KeyExpression {
	return m.recordCountKey
}

// IsStoreRecordVersions returns whether record versioning is enabled.
func (m *RecordMetaData) IsStoreRecordVersions() bool {
	return m.storeRecordVersions
}

// IsSplitLongRecords returns whether records >100KB are split across multiple KV pairs.
func (m *RecordMetaData) IsSplitLongRecords() bool {
	return m.splitLongRecords
}

// GetRecordTypeIndex returns the record type index for this record type
func (rt *RecordType) GetRecordTypeIndex() int {
	return rt.RecordTypeIndex
}

// GetIndexesForRecordType returns the indexes defined for a specific record type.
// Does NOT include universal indexes — use GetUniversalIndexes() for those.
func (m *RecordMetaData) GetIndexesForRecordType(name string) []*Index {
	rt := m.recordTypes[name]
	if rt == nil {
		return nil
	}
	return rt.indexes
}

// GetUniversalIndexes returns indexes that apply to all record types.
func (m *RecordMetaData) GetUniversalIndexes() []*Index {
	return m.universalIndexes
}

// HasIndexes returns true if any indexes are defined.
func (m *RecordMetaData) HasIndexes() bool {
	return len(m.indexes) > 0
}

// GetAllIndexes returns all indexes by name.
func (m *RecordMetaData) GetAllIndexes() map[string]*Index {
	return m.indexes
}