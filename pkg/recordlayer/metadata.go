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
	recordTypes    map[string]*RecordType
	fileDescriptor protoreflect.FileDescriptor
	version        int
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

// Build creates the final RecordMetaData
func (b *RecordMetaDataBuilder) Build() *RecordMetaData {
	return &RecordMetaData{
		recordTypes:    b.recordTypes,
		fileDescriptor: b.fileDescriptor,
		version:        b.version,
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

// GetRecordTypeIndex returns the record type index for this record type
func (rt *RecordType) GetRecordTypeIndex() int {
	return rt.RecordTypeIndex
}