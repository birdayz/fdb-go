package recordlayer

import (
	"fmt"

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

	// formerIndexes tracks deleted indexes for schema evolution safety.
	// Java equivalent: RecordMetaData.getFormerIndexes()
	formerIndexes []*FormerIndex
}

// FormerIndex tracks a deleted index for schema evolution safety.
// Prevents accidental reuse of an index's subspace key after deletion.
// Matches Java's com.apple.foundationdb.record.metadata.FormerIndex.
type FormerIndex struct {
	SubspaceKey    interface{}
	AddedVersion   int
	RemovedVersion int
	FormerName     string
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

	// indexes defined for this record type (single-type)
	indexes []*Index

	// multiTypeIndexes span multiple record types.
	// Java equivalent: RecordType.getMultiTypeIndexes()
	multiTypeIndexes []*Index

	// explicitRecordTypeKey overrides the auto-derived record type key.
	// If nil, RecordTypeIndex is used. Matches Java's RecordType.getRecordTypeKey().
	explicitRecordTypeKey interface{}
}

// KeyExpression represents an expression that extracts key components from a record.
// Matches Java's KeyExpression interface which returns List<Key.Evaluated>.
type KeyExpression interface {
	// Evaluate extracts key tuples from a record.
	// Returns a list of key tuples (each tuple is a []interface{}).
	// Single-valued expressions return one tuple; fan-out expressions
	// (e.g. repeated fields) return multiple tuples.
	// Matches Java's KeyExpression.evaluateMessage() -> List<Key.Evaluated>.
	Evaluate(msg proto.Message) ([][]interface{}, error)

	// FieldNames returns the field names this expression accesses
	FieldNames() []string
}

// RecordMetaDataBuilder provides a builder pattern for creating RecordMetaData
// This matches the Java RecordMetaDataBuilder pattern
type RecordMetaDataBuilder struct {
	recordTypes              map[string]*RecordType
	fileDescriptor           protoreflect.FileDescriptor
	version                  int
	recordCountKey           KeyExpression
	storeRecordVersions      bool
	splitLongRecords         bool
	indexes                  map[string]*Index
	universalIndexes         []*Index
	formerIndexes            []*FormerIndex
	counterBasedSubspaceKeys bool
	subspaceKeyCounter       int64
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

			// Use the proto field number as the record type index.
			// Matches Java: RecordType.getRecordTypeKey() returns the smallest
			// union field number matching this message type.
			recordType := &RecordType{
				Name:                 recordTypeName,
				Descriptor:           recordMsgDesc,
				PrimaryKey:           nil, // Will be set explicitly
				SinceVersion:         1,
				RecordTypeIndex:      int(field.Number()),
				UnionFieldDescriptor: field, // Store the union field for reflection
			}
			b.recordTypes[recordTypeName] = recordType
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

// EnableCounterBasedSubspaceKeys switches index subspace keys from name-based (string)
// to counter-based (int64). Each index added after this call gets an auto-incrementing
// integer subspace key instead of the index name. Matches Java's
// RecordMetaDataBuilder.enableCounterBasedSubspaceKeys().
func (b *RecordMetaDataBuilder) EnableCounterBasedSubspaceKeys() *RecordMetaDataBuilder {
	b.counterBasedSubspaceKeys = true
	return b
}

// SetVersion sets the metadata schema version.
// This should be bumped when the schema changes for evolution tracking.
// Matches Java's RecordMetaDataBuilder.setVersion(int).
func (b *RecordMetaDataBuilder) SetVersion(version int) *RecordMetaDataBuilder {
	b.version = version
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
	b.addIndexCommon(index)
	rt.indexes = append(rt.indexes, index)
	return b
}

// assignSubspaceKey sets a counter-based subspace key if enabled.
func (b *RecordMetaDataBuilder) assignSubspaceKey(index *Index) {
	if b.counterBasedSubspaceKeys {
		b.subspaceKeyCounter++
		index.SetSubspaceKey(b.subspaceKeyCounter)
	}
}

// addIndexCommon performs the shared setup for all AddIndex variants.
// Sets LastModifiedVersion and AddedVersion on the index and registers it
// in the builder's index map. Matches Java's RecordMetaDataBuilder.addIndexCommon().
func (b *RecordMetaDataBuilder) addIndexCommon(index *Index) {
	b.assignSubspaceKey(index)
	if index.LastModifiedVersion <= 0 {
		b.version++
		index.LastModifiedVersion = b.version
	} else if index.LastModifiedVersion > b.version {
		b.version = index.LastModifiedVersion
	}
	if index.AddedVersion <= 0 {
		index.AddedVersion = index.LastModifiedVersion
	}
	if b.indexes == nil {
		b.indexes = make(map[string]*Index)
	}
	b.indexes[index.Name] = index
}

// AddMultiTypeIndex adds an index spanning multiple record types.
// If recordTypeNames is nil or empty, treats as universal index.
// If only one name, adds as single-type index.
// Matches Java's RecordMetaDataBuilder.addMultiTypeIndex().
func (b *RecordMetaDataBuilder) AddMultiTypeIndex(recordTypeNames []string, index *Index) *RecordMetaDataBuilder {
	if len(recordTypeNames) == 0 {
		return b.AddUniversalIndex(index)
	}
	if len(recordTypeNames) == 1 {
		return b.AddIndex(recordTypeNames[0], index)
	}
	b.addIndexCommon(index)
	for _, name := range recordTypeNames {
		rt, ok := b.recordTypes[name]
		if !ok {
			continue
		}
		rt.multiTypeIndexes = append(rt.multiTypeIndexes, index)
	}
	return b
}

// AddUniversalIndex adds an index that applies to all record types.
// Matches Java's RecordMetaDataBuilder.addUniversalIndex(Index index).
func (b *RecordMetaDataBuilder) AddUniversalIndex(index *Index) *RecordMetaDataBuilder {
	b.addIndexCommon(index)
	b.universalIndexes = append(b.universalIndexes, index)
	return b
}

// RemoveIndex removes an index by name and records it as a FormerIndex
// to prevent subspace key reuse. Matches Java's RecordMetaDataBuilder.removeIndex(String).
func (b *RecordMetaDataBuilder) RemoveIndex(indexName string) *RecordMetaDataBuilder {
	idx, ok := b.indexes[indexName]
	if !ok {
		return b
	}

	former := &FormerIndex{
		SubspaceKey:    idx.SubspaceTupleKey(),
		AddedVersion:   idx.AddedVersion,
		RemovedVersion: b.version,
		FormerName:     idx.Name,
	}
	b.formerIndexes = append(b.formerIndexes, former)
	delete(b.indexes, indexName)

	// Remove from record type single-type indexes
	for _, rt := range b.recordTypes {
		rt.indexes = removeIndexFromSlice(rt.indexes, indexName)
		rt.multiTypeIndexes = removeIndexFromSlice(rt.multiTypeIndexes, indexName)
	}
	// Remove from universal indexes
	b.universalIndexes = removeIndexFromSlice(b.universalIndexes, indexName)

	return b
}

func removeIndexFromSlice(indexes []*Index, name string) []*Index {
	result := indexes[:0]
	for _, idx := range indexes {
		if idx.Name != name {
			result = append(result, idx)
		}
	}
	return result
}

// GetFormerIndexes returns the builder's former indexes (for testing/inspection).
func (b *RecordMetaDataBuilder) GetFormerIndexes() []*FormerIndex {
	return b.formerIndexes
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
// Returns an error if any record type has no primary key set.
// The record types map is copied to prevent the builder from mutating the built metadata.
func (b *RecordMetaDataBuilder) Build() (*RecordMetaData, error) {
	// Validate primary keys: must be set and must not create duplicates.
	// Matches Java's MetaDataValidator.validatePrimaryKey().
	for name, rt := range b.recordTypes {
		if rt.PrimaryKey == nil {
			return nil, fmt.Errorf("record type %q has no primary key set", name)
		}
		if createsDuplicates(rt.PrimaryKey) {
			return nil, fmt.Errorf("record type %q has a primary key that can create duplicates (fan-out not allowed on primary keys)", name)
		}
	}

	// Validate no duplicate record type keys.
	// Matches Java's MetaDataValidator which checks for duplicate type keys.
	typeKeySeen := make(map[interface{}]string)
	for name, rt := range b.recordTypes {
		key := rt.GetRecordTypeKey()
		if prevName, exists := typeKeySeen[key]; exists {
			return nil, fmt.Errorf("record types %q and %q have the same record type key %v", prevName, name, key)
		}
		typeKeySeen[key] = name
	}

	types := make(map[string]*RecordType, len(b.recordTypes))
	for k, v := range b.recordTypes {
		types[k] = v
	}
	indexes := make(map[string]*Index, len(b.indexes))
	for k, v := range b.indexes {
		indexes[k] = v
	}

	// Validate no duplicate subspace keys among current indexes.
	// Matches Java's MetaDataValidator.validateIndexes().
	indexSubspaceKeySeen := make(map[interface{}]string)
	for _, idx := range indexes {
		sk := idx.SubspaceTupleKey()
		if prevName, exists := indexSubspaceKeySeen[sk]; exists {
			return nil, fmt.Errorf("indexes %q and %q have the same subspace key %v", prevName, idx.Name, sk)
		}
		indexSubspaceKeySeen[sk] = idx.Name
	}

	// Validate no former index subspace key conflicts with current indexes
	for _, fi := range b.formerIndexes {
		for _, idx := range indexes {
			if fi.SubspaceKey == idx.SubspaceTupleKey() {
				return nil, fmt.Errorf("index %q reuses subspace key of former index %q", idx.Name, fi.FormerName)
			}
		}
	}

	// Validate former index version ordering.
	// Matches Java's MetaDataValidator: addedVersion ≤ removedVersion, both ≤ metadata version.
	for _, fi := range b.formerIndexes {
		if fi.AddedVersion > fi.RemovedVersion {
			return nil, fmt.Errorf("former index %q has addedVersion (%d) > removedVersion (%d)", fi.FormerName, fi.AddedVersion, fi.RemovedVersion)
		}
	}

	// Build type keys map (message name → record type key as int64) and bind
	// all RecordTypeKeyExpression instances so they evaluate to the correct
	// integer type key instead of the string name. Matches Java's
	// RecordTypeKeyExpression.evaluateMessage() → record.getRecordType().getRecordTypeKey().
	typeKeys := make(map[string]int64, len(types))
	for _, rt := range types {
		key := rt.GetRecordTypeKey()
		switch k := key.(type) {
		case int:
			typeKeys[rt.Name] = int64(k)
		case int64:
			typeKeys[rt.Name] = k
		}
	}
	bindRecordTypeKeyExpressions := func(expr KeyExpression) {
		if expr == nil {
			return
		}
		if rt, ok := expr.(*RecordTypeKeyExpression); ok {
			rt.bindTypeKeys(typeKeys)
		}
		if comp, ok := expr.(*CompositeKeyExpression); ok {
			for _, child := range comp.expressions {
				if rt, ok := child.(*RecordTypeKeyExpression); ok {
					rt.bindTypeKeys(typeKeys)
				}
			}
		}
	}
	for _, rt := range types {
		bindRecordTypeKeyExpressions(rt.PrimaryKey)
	}
	if b.recordCountKey != nil {
		bindRecordTypeKeyExpressions(b.recordCountKey)
	}
	for _, idx := range indexes {
		bindRecordTypeKeyExpressions(idx.RootExpression)
	}

	// Compute primaryKeyComponentPositions for each index.
	// For each record type that has this index, compute the overlap between
	// the index key expression and the primary key. If a primary key component
	// already appears in the index key, it is deduplicated from the index entry.
	// Matches Java's RecordMetaDataBuilder which calls buildPrimaryKeyComponentPositions().
	for _, rt := range types {
		for _, idx := range rt.indexes {
			if idx.primaryKeyComponentPositions == nil {
				idx.primaryKeyComponentPositions = buildPrimaryKeyComponentPositions(idx.RootExpression, rt.PrimaryKey)
			}
		}
	}
	// Universal indexes: use the first record type's primary key (they should all match)
	for _, idx := range b.universalIndexes {
		if idx.primaryKeyComponentPositions == nil {
			for _, rt := range types {
				idx.primaryKeyComponentPositions = buildPrimaryKeyComponentPositions(idx.RootExpression, rt.PrimaryKey)
				break
			}
		}
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
		formerIndexes:       b.formerIndexes,
	}, nil
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

// SetRecordTypeKey overrides the auto-derived record type key for this record type.
// By default, the record type index (proto field number order) is used.
// Matches Java's RecordTypeBuilder.setRecordTypeKey(Key.Evaluated).
func (rtb *RecordTypeBuilder) SetRecordTypeKey(key interface{}) *RecordTypeBuilder {
	rtb.recordType.explicitRecordTypeKey = key
	return rtb
}

// NewRecordMetaData creates metadata from a protobuf file descriptor.
// This is a convenience function that matches the Java pattern.
// Note: This will return nil if any record type has no primary key set.
// Prefer using NewRecordMetaDataBuilder() and Build() directly for proper error handling.
func NewRecordMetaData(fd protoreflect.FileDescriptor) *RecordMetaData {
	md, _ := NewRecordMetaDataBuilder().SetRecords(fd).Build()
	return md
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

// GetRecordTypeKey returns the explicit record type key if set, or falls back
// to the record type index. Matches Java's RecordType.getRecordTypeKey().
func (rt *RecordType) GetRecordTypeKey() interface{} {
	if rt.explicitRecordTypeKey != nil {
		return rt.explicitRecordTypeKey
	}
	return rt.RecordTypeIndex
}

// GetIndexesForRecordType returns the indexes defined for a specific record type,
// including both single-type and multi-type indexes.
// Does NOT include universal indexes — use GetUniversalIndexes() for those.
// Matches Java's RecordType.getAllIndexes().
func (m *RecordMetaData) GetIndexesForRecordType(name string) []*Index {
	rt := m.recordTypes[name]
	if rt == nil {
		return nil
	}
	if len(rt.multiTypeIndexes) == 0 {
		return rt.indexes
	}
	all := make([]*Index, 0, len(rt.indexes)+len(rt.multiTypeIndexes))
	all = append(all, rt.indexes...)
	all = append(all, rt.multiTypeIndexes...)
	return all
}

// GetUniversalIndexes returns indexes that apply to all record types.
func (m *RecordMetaData) GetUniversalIndexes() []*Index {
	return m.universalIndexes
}

// HasIndexes returns true if any indexes are defined.
func (m *RecordMetaData) HasIndexes() bool {
	return len(m.indexes) > 0
}

// GetIndex returns the index with the given name, or nil if not found.
// Matches Java's RecordMetaData.getIndex(String).
func (m *RecordMetaData) GetIndex(name string) *Index {
	return m.indexes[name]
}

// GetAllIndexes returns all indexes by name.
func (m *RecordMetaData) GetAllIndexes() map[string]*Index {
	return m.indexes
}

// GetFormerIndexes returns all former (deleted) indexes.
// Matches Java's RecordMetaData.getFormerIndexes().
func (m *RecordMetaData) GetFormerIndexes() []*FormerIndex {
	return m.formerIndexes
}

// GetIndexesToBuildSince returns indexes that were added or modified since the
// given metadata version. Used by CreateOrOpen to detect new indexes that need
// to be built when opening an existing store with updated metadata.
// Matches Java's RecordMetaData.getIndexesToBuildSince(int).
func (m *RecordMetaData) GetIndexesToBuildSince(version int) []*Index {
	var result []*Index
	for _, idx := range m.indexes {
		if idx.LastModifiedVersion > version {
			result = append(result, idx)
		}
	}
	return result
}

// PrimaryKeyHasRecordTypePrefix returns true if all record types have a
// primary key that starts with a RecordTypeKeyExpression.
// Matches Java's RecordMetaData.primaryKeyHasRecordTypePrefix().
func (m *RecordMetaData) PrimaryKeyHasRecordTypePrefix() bool {
	for _, rt := range m.recordTypes {
		if !primaryKeyStartsWithRecordType(rt.PrimaryKey) {
			return false
		}
	}
	return true
}

// primaryKeyStartsWithRecordType checks if a key expression starts with RecordTypeKeyExpression.
func primaryKeyStartsWithRecordType(expr KeyExpression) bool {
	if expr == nil {
		return false
	}
	if _, ok := expr.(*RecordTypeKeyExpression); ok {
		return true
	}
	if comp, ok := expr.(*CompositeKeyExpression); ok && len(comp.expressions) > 0 {
		_, ok := comp.expressions[0].(*RecordTypeKeyExpression)
		return ok
	}
	return false
}