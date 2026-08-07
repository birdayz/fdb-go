package recordlayer

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// RecordMetaData describes the schema for records stored in a record store.
// This is a simplified version for our MVP - just enough to define record types
// and their primary keys.
type RecordMetaData struct {
	// Map of record type names to their definitions
	recordTypes map[string]*RecordType

	// The protobuf file descriptor
	fileDescriptor protoreflect.FileDescriptor

	// recordsSourceProto, when non-nil, is the ORIGINAL FileDescriptorProto
	// fileDescriptor was built from; ToProto() emits it verbatim (Java:
	// FileDescriptor.toProto() returns the source proto unchanged). Nil for
	// metadata built from compiled Go protos, where the
	// protodesc.ToFileDescriptorProto reconstruction is the only source.
	recordsSourceProto *descriptorpb.FileDescriptorProto

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

	// unionDescriptor is the protobuf message descriptor for UnionDescriptor.
	// Nil if the schema has no union (single-type).
	// Matches Java's RecordMetaData.getUnionDescriptor().
	unionDescriptor protoreflect.MessageDescriptor

	// fieldNumberToRecordType maps union field numbers to record types for
	// direct wire format decoding (avoids UnionDescriptor allocation).
	fieldNumberToRecordType map[protowire.Number]*RecordType

	// subspaceKeyCounter / usesSubspaceKeyCounter are the counter-based
	// subspace-key ASSIGNMENT SCHEME state (proto fields 10 and 11). They are
	// not decoration: they decide the on-disk prefix every future index gets.
	//
	// Per-index subspace keys round-trip on their own, so existing data is safe
	// either way. What is lost by dropping these is the SCHEME: a metadata that
	// went through a Go round-trip without them comes back with the counter
	// disabled, and the next index added — by Java or by Go — is keyed by NAME
	// instead of by counter. Nothing errors; the assignment discipline just
	// silently changes underneath the store.
	// Java: RecordMetaData.subspaceKeyCounter / usesSubspaceKeyCounter.
	subspaceKeyCounter     int64
	usesSubspaceKeyCounter bool

	// preserved holds the proto fields this port does not model, kept verbatim
	// so a Go round-trip re-emits them. See preservedMetaDataFields.
	preserved preservedMetaDataFields
}

// preservedMetaDataFields carries the MetaData proto fields the Go port does
// not model (12: joined_record_types, 13: unnested_record_types,
// 14: user_defined_functions, 15: views), so that ToProto re-emits exactly what
// FromProto was given.
//
// This is CLAUDE.md's promise made real rather than a new feature. The port
// scopes out synthetic record types, user-defined functions and views on the
// stated grounds that protobuf "round-trips them via unknown-field
// preservation" — but these are KNOWN fields of a message the port parses, so
// unknown-field preservation never applied to them and they were being dropped
// on the floor. A Go tool that loaded a Java application's metadata and saved it
// back deleted the application's joined types.
//
// Carrying them opaquely is deliberately NOT the same as supporting them. The
// contents are never interpreted, and anything whose behaviour would depend on
// interpreting them must refuse rather than proceed — see
// RecordMetaData.DeclaresSyntheticRecordTypes and its callers.
type preservedMetaDataFields struct {
	joinedRecordTypes    []*gen.JoinedRecordType
	unnestedRecordTypes  []*gen.UnnestedRecordType
	userDefinedFunctions []*gen.PUserDefinedFunction
	views                []*gen.PView

	// unknown holds everything the generated Go type has no field for —
	// principally the MetaData extension range (1000-2000), which is where
	// applications and downstream layers hang their own metadata.
	//
	// These really are unknown fields, but that does NOT mean protobuf carries
	// them across this round trip on its own. Unknown-field preservation keeps
	// them attached to the message they were parsed into; ToProto constructs a
	// FRESH gen.MetaData and copies modelled fields onto it, so the original's
	// unknown bytes have no route to the result and were being dropped. That is
	// the same defect as fields 12-15, arrived at from the opposite direction:
	// there the fields were assumed unknown and were not, here they are genuinely
	// unknown and the mechanism still does not reach them.
	//
	// The bytes-level path (FDBMetaDataStore) never had this problem, because it
	// stores the serialized proto verbatim and never builds a RecordMetaData.
	unknown []byte
}

// FormerIndex tracks a deleted index for schema evolution safety.
// Prevents accidental reuse of an index's subspace key after deletion.
// Matches Java's com.apple.foundationdb.record.metadata.FormerIndex.
type FormerIndex struct {
	SubspaceKey    any
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

	// unionFieldNumber is the proto field number in the UnionDescriptor for this type.
	// Pre-computed at Build() time for direct wire format encoding/decoding.
	unionFieldNumber protowire.Number

	// newMessage creates a new empty instance of this record type's proto message.
	// Pre-computed at Build() time via protoregistry. Returns concrete Go type
	// (e.g. *gen.Order), not dynamicpb.
	newMessage func() proto.Message

	// indexes defined for this record type (single-type)
	indexes []*Index

	// multiTypeIndexes span multiple record types.
	// Java equivalent: RecordType.getMultiTypeIndexes()
	multiTypeIndexes []*Index

	// explicitRecordTypeKey overrides the auto-derived record type key.
	// If nil, RecordTypeIndex is used. Matches Java's RecordType.getRecordTypeKey().
	explicitRecordTypeKey any
}

// KeyExpression represents an expression that extracts key components from a record.
// Matches Java's KeyExpression interface which returns List<Key.Evaluated>.
type KeyExpression interface {
	// Evaluate extracts key tuples from a record.
	// Returns a list of key tuples (each tuple is a []any).
	// Single-valued expressions return one tuple; fan-out expressions
	// (e.g. repeated fields) return multiple tuples.
	//
	// record is the top-level stored record context (provides version, PK, etc.).
	// msg is the current message being evaluated (changes during nesting into sub-messages).
	// Either or both may be nil.
	//
	// Matches Java's KeyExpression.evaluateMessage(FDBRecord, Message) -> List<Key.Evaluated>.
	Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error)

	// FieldNames returns the field names this expression accesses
	FieldNames() []string

	// ColumnSize returns the number of tuple elements this expression produces.
	// Matches Java's KeyExpression.getColumnSize().
	ColumnSize() int

	// ToKeyExpression serializes this expression to its protobuf representation.
	// Matches Java's KeyExpression.toKeyExpression().
	ToKeyExpression() *gen.KeyExpression
}

// RecordMetaDataBuilder provides a builder pattern for creating RecordMetaData
// This matches the Java RecordMetaDataBuilder pattern
type RecordMetaDataBuilder struct {
	recordTypes              map[string]*RecordType
	fileDescriptor           protoreflect.FileDescriptor
	recordsSourceProto       *descriptorpb.FileDescriptorProto
	version                  int
	recordCountKey           KeyExpression
	storeRecordVersions      bool
	splitLongRecords         bool
	indexes                  map[string]*Index
	universalIndexes         []*Index
	formerIndexes            []*FormerIndex
	counterBasedSubspaceKeys bool
	subspaceKeyCounter       int64
	buildErrors              []error
	unionDescriptor          protoreflect.MessageDescriptor
	// preserved carries the unmodelled proto fields through to the built
	// metadata. See preservedMetaDataFields.
	preserved preservedMetaDataFields
}

// NewRecordMetaDataBuilder creates a new builder
func NewRecordMetaDataBuilder() *RecordMetaDataBuilder {
	return &RecordMetaDataBuilder{
		recordTypes: make(map[string]*RecordType),
		version:     0, // Start with version 0 to match Java defaults
	}
}

// SetRecordsWithUnionName is SetRecords with an explicit union message
// name. Use this when the proto file's union message is not called
// "UnionDescriptor" — e.g. schemas that must coexist with another
// RecordMetaData in the same Go package (gen.*), where duplicate
// UnionDescriptor symbols would clash. Behaviour is identical to
// SetRecords in every other respect.
func (b *RecordMetaDataBuilder) SetRecordsWithUnionName(fd protoreflect.FileDescriptor, unionName string) *RecordMetaDataBuilder {
	return b.setRecordsWithUnionName(fd, unionName)
}

// SetRecords sets the protobuf file descriptor containing record definitions.
// Uses the default union message name "UnionDescriptor".
func (b *RecordMetaDataBuilder) SetRecords(fd protoreflect.FileDescriptor) *RecordMetaDataBuilder {
	return b.setRecordsWithUnionName(fd, "UnionDescriptor")
}

func (b *RecordMetaDataBuilder) setRecordsWithUnionName(fd protoreflect.FileDescriptor, unionName string) *RecordMetaDataBuilder {
	b.fileDescriptor = fd

	// Find the named union message to map fields to record types.
	unionDesc := fd.Messages().ByName(protoreflect.Name(unionName))
	if unionDesc == nil {
		// If no UnionDescriptor, treat each message as a separate record type
		b.setRecordsWithoutUnion(fd)
		return b
	}
	b.unionDescriptor = unionDesc

	unionFields := unionDesc.Fields()

	for i := 0; i < unionFields.Len(); i++ {
		field := unionFields.Get(i)
		fieldName := string(field.Name())

		var recordTypeName string
		var recordMsgDesc protoreflect.MessageDescriptor
		switch {
		case len(fieldName) > 1 && fieldName[0] == '_':
			// RecordLayer convention: `_TypeName`.
			recordTypeName = fieldName[1:]
			recordMsgDesc = fd.Messages().ByName(protoreflect.Name(recordTypeName))
		case field.Kind() == protoreflect.MessageKind:
			// fdb-relational convention: derive type name from the
			// field's type reference rather than the field name.
			recordMsgDesc = field.Message()
			if recordMsgDesc != nil {
				recordTypeName = string(recordMsgDesc.Name())
			}
		}
		if recordMsgDesc == nil || recordTypeName == "" {
			continue
		}

		// Use the proto field number as the record type index.
		// Matches Java: RecordType.getRecordTypeKey() returns the smallest
		// union field number matching this message type.
		recordType := &RecordType{
			Name:                 recordTypeName,
			Descriptor:           recordMsgDesc,
			PrimaryKey:           nil, // Will be set explicitly
			SinceVersion:         0,   // Matches Java's null default
			RecordTypeIndex:      int(field.Number()),
			UnionFieldDescriptor: field, // Store the union field for reflection
		}
		b.recordTypes[recordTypeName] = recordType
	}

	return b
}

// FileDescriptor returns the schema's protobuf file descriptor.
func (m *RecordMetaData) FileDescriptor() protoreflect.FileDescriptor { return m.fileDescriptor }

// SetRecordsSourceProto retains the ORIGINAL FileDescriptorProto the file
// descriptor was built from, so ToProto() can emit it verbatim. Java keeps
// the source proto inside Descriptors.FileDescriptor and toProto() returns
// it unchanged; Go's protodesc.ToFileDescriptorProto instead RECONSTRUCTS
// the proto — absolutizing type names (".T" where Java stored "T") and
// materializing json_name — which changes the stored bytes. Descriptor
// bytes are wire (the catalog persists RecordMetaData.toProto()), so the
// source proto must survive to serialization untouched.
func (b *RecordMetaDataBuilder) SetRecordsSourceProto(fdp *descriptorpb.FileDescriptorProto) *RecordMetaDataBuilder {
	b.recordsSourceProto = fdp
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
				SinceVersion:         0,   // Matches Java's null default
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
	if !keyExpressionsEqualNilSafe(b.recordCountKey, key) {
		b.version++ // Matches Java: bumps version when value changes
	}
	b.recordCountKey = key
	return b
}

// SetStoreRecordVersions enables or disables automatic record versioning.
// When enabled, each save assigns an FDBRecordVersion to the record.
// Java equivalent: RecordMetaDataBuilder.setStoreRecordVersions(boolean)
func (b *RecordMetaDataBuilder) SetStoreRecordVersions(store bool) *RecordMetaDataBuilder {
	if b.storeRecordVersions != store {
		b.version++ // Matches Java: bumps version when value changes
	}
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

// UsesSubspaceKeyCounter reports whether counter-based subspace-key assignment
// is in effect. Matches Java's RecordMetaDataBuilder.usesSubspaceKeyCounter().
func (b *RecordMetaDataBuilder) UsesSubspaceKeyCounter() bool {
	return b.counterBasedSubspaceKeys
}

// GetSubspaceKeyCounter returns the current counter value; 0 when the scheme is
// not enabled. Matches Java's RecordMetaDataBuilder.getSubspaceKeyCounter().
func (b *RecordMetaDataBuilder) GetSubspaceKeyCounter() int64 {
	return b.subspaceKeyCounter
}

// SetSubspaceKeyCounter sets the counter's starting value, for callers whose
// indexes already carry keys that would collide with counter-based assignment.
// Matches Java's RecordMetaDataBuilder.setSubspaceKeyCounter(long), including
// both of its refusals: the scheme must already be enabled, and the counter may
// only move FORWARD. Moving it backwards would hand a fresh index a key some
// existing index already owns, which is a silent data collision rather than an
// error, so the guard is not a convenience.
func (b *RecordMetaDataBuilder) SetSubspaceKeyCounter(counter int64) *RecordMetaDataBuilder {
	if !b.counterBasedSubspaceKeys {
		b.buildErrors = append(b.buildErrors, &MetaDataError{
			Message: "Counter-based subspace keys not enabled",
		})
		return b
	}
	if counter <= b.subspaceKeyCounter {
		b.buildErrors = append(b.buildErrors, &MetaDataError{
			Message: fmt.Sprintf(
				"Subspace key counter must be set to a value greater than its current value: expected greater than %d, actual %d",
				b.subspaceKeyCounter, counter),
		})
		return b
	}
	b.subspaceKeyCounter = counter
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
	if b.splitLongRecords != split {
		b.version++ // Matches Java: bumps version when value changes
	}
	b.splitLongRecords = split
	return b
}

// GetVersion returns the current metadata version on the builder.
// Matches Java's RecordMetaDataBuilder.getVersion().
func (b *RecordMetaDataBuilder) GetVersion() int {
	return b.version
}

// IsSplitLongRecords returns whether split long records is enabled on the builder.
// Matches Java's RecordMetaDataBuilder.isSplitLongRecords().
func (b *RecordMetaDataBuilder) IsSplitLongRecords() bool {
	return b.splitLongRecords
}

// IsStoreRecordVersions returns whether record versioning is enabled on the builder.
// Matches Java's RecordMetaDataBuilder.isStoreRecordVersions().
func (b *RecordMetaDataBuilder) IsStoreRecordVersions() bool {
	return b.storeRecordVersions
}

// GetRecordCountKey returns the record count key expression on the builder.
func (b *RecordMetaDataBuilder) GetRecordCountKey() KeyExpression {
	return b.recordCountKey
}

// GetRecordTypes returns the record types map on the builder.
func (b *RecordMetaDataBuilder) GetRecordTypes() map[string]*RecordType {
	return b.recordTypes
}

// AddIndex adds a secondary index for a specific record type.
// Matches Java's RecordMetaDataBuilder.addIndex(String recordType, Index index).
func (b *RecordMetaDataBuilder) AddIndex(recordTypeName string, index *Index) *RecordMetaDataBuilder {
	rt, ok := b.recordTypes[recordTypeName]
	if !ok {
		b.buildErrors = append(b.buildErrors, &MetaDataError{
			Message: fmt.Sprintf("Unknown record type %s", recordTypeName),
		})
		return b
	}
	b.addIndexCommon(index)
	rt.indexes = append(rt.indexes, index)
	return b
}

// assignSubspaceKey sets a counter-based subspace key if enabled AND the index
// does not already carry a chosen one.
//
// Matches Java's RecordMetaDataBuilder.addIndexCommon (:1101-1102):
//
//	if (usesSubspaceKeyCounter && !index.hasExplicitSubspaceKey()) {
//	    index.setSubspaceKey(++subspaceKeyCounter);
//	}
//
// The hasExplicitSubspaceKey half is load-bearing twice over, and Go had
// neither. A caller who set a key on an index and then added it to a
// counter-based builder had that key OVERWRITTEN — the index's on-disk prefix
// silently moved off its data. And every index deserialized from proto counts
// as explicit, so reloading a counter-keyed metadata would otherwise re-number
// all of them and orphan every entry in the store.
func (b *RecordMetaDataBuilder) assignSubspaceKey(index *Index) {
	if b.counterBasedSubspaceKeys && !index.HasExplicitSubspaceKey() {
		b.subspaceKeyCounter++
		index.SetSubspaceKey(b.subspaceKeyCounter)
	}
}

// addIndexCommon performs the shared setup for all AddIndex variants.
// Sets LastModifiedVersion and AddedVersion on the index and registers it
// in the builder's index map. Matches Java's RecordMetaDataBuilder.addIndexCommon().
func (b *RecordMetaDataBuilder) addIndexCommon(index *Index) {
	if b.indexes == nil {
		b.indexes = make(map[string]*Index)
	}
	if _, exists := b.indexes[index.Name]; exists {
		b.buildErrors = append(b.buildErrors, &MetaDataError{
			Message: fmt.Sprintf("Index %s already defined", index.Name),
		})
		return
	}
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
			b.buildErrors = append(b.buildErrors, &MetaDataError{
				Message: fmt.Sprintf("Unknown record type %s", name),
			})
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

	// Pre-increment version before recording RemovedVersion.
	// Matches Java: formerIndexes.add(new FormerIndex(..., ++version, name))
	b.version++
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
// Panics with MetaDataError if the record type does not exist, matching Java's
// RecordMetaDataBuilder.getRecordType() which throws MetaDataException.
func (b *RecordMetaDataBuilder) GetRecordType(name string) *RecordTypeBuilder {
	recordType := b.recordTypes[name]
	if recordType == nil {
		panic(&MetaDataError{Message: fmt.Sprintf("unknown record type %q", name)})
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
	// Check for errors accumulated during builder method calls.
	if len(b.buildErrors) > 0 {
		return nil, errors.Join(b.buildErrors...)
	}

	// Validate at least one record type is defined.
	// Matches Java's MetaDataValidator.validate() which throws "No record types defined in meta-data".
	if len(b.recordTypes) == 0 {
		return nil, &MetaDataError{Message: "no record types defined in meta-data"}
	}

	// Validate union descriptor oneof structure.
	// Matches Java's MetaDataValidator.validateUnionDescriptor():
	//   - Must have at most 1 oneof
	//   - If a oneof exists, it must contain all fields
	if b.unionDescriptor != nil {
		oneofs := b.unionDescriptor.Oneofs()
		if oneofs.Len() > 1 {
			return nil, &MetaDataError{Message: "union descriptor has more than one oneof"}
		}
		if oneofs.Len() == 1 {
			oneof := oneofs.Get(0)
			if oneof.Fields().Len() != b.unionDescriptor.Fields().Len() {
				return nil, &MetaDataError{Message: "union descriptor oneof must contain every field"}
			}
		}
	}

	// Validate primary keys: must be set, must produce at least one column,
	// and must not create duplicates.
	// Matches Java's MetaDataValidator.validatePrimaryKey().
	for name, rt := range b.recordTypes {
		if rt.PrimaryKey == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("record type %q has no primary key set", name)}
		}
		if rt.PrimaryKey.ColumnSize() == 0 {
			return nil, &MetaDataError{Message: fmt.Sprintf("record type %q has a primary key that produces no columns (EmptyKeyExpression or empty Concat are not valid primary keys)", name)}
		}
		if createsDuplicates(rt.PrimaryKey) {
			return nil, &MetaDataError{Message: fmt.Sprintf("record type %q has a primary key that can create duplicates (fan-out not allowed on primary keys)", name)}
		}
	}

	// Validate primary key and index expressions against proto message descriptors.
	// Matches Java's MetaDataValidator.validatePrimaryKeyForRecordType() and
	// MetaDataValidator.validateIndexForRecordType() which call KeyExpression.validate(Descriptor).
	for name, rt := range b.recordTypes {
		if rt.Descriptor != nil && rt.PrimaryKey != nil {
			if err := validateKeyExpression(rt.PrimaryKey, rt.Descriptor); err != nil {
				return nil, &MetaDataError{Message: fmt.Sprintf("record type %q: primary key validation failed: %v", name, err)}
			}
		}
		if rt.Descriptor != nil {
			for _, idx := range rt.indexes {
				if err := validateKeyExpression(idx.RootExpression, rt.Descriptor); err != nil {
					return nil, &MetaDataError{Message: fmt.Sprintf("record type %q: index %q validation failed: %v", name, idx.Name, err)}
				}
			}
		}
	}
	// Validate universal indexes against all record types.
	for _, idx := range b.universalIndexes {
		for name, rt := range b.recordTypes {
			if rt.Descriptor != nil {
				if err := validateKeyExpression(idx.RootExpression, rt.Descriptor); err != nil {
					return nil, &MetaDataError{Message: fmt.Sprintf("record type %q: universal index %q validation failed: %v", name, idx.Name, err)}
				}
			}
		}
	}

	// Validate no duplicate record type keys.
	// Matches Java's MetaDataValidator which checks for duplicate type keys.
	//
	// Two record types collide exactly when their keys occupy the same BYTES,
	// so the seen-set is keyed on the tuple encoding rather than on the value.
	// Keying on the value missed real collisions — int64(7) and uint(7) are
	// distinct values with identical encodings, and a record-type-prefixed
	// primary key built from them puts two types in one key space, where a
	// save silently overwrites the other type's record.
	typeKeySeen := make(map[string]string)
	for name, rt := range b.recordTypes {
		key := rt.GetRecordTypeKey()
		dedup, ok := recordTypeKeyIdentity(key)
		if !ok {
			// Unreachable while both doors canonicalize and Build reports
			// builder errors before this loop; stated as an error rather than
			// left to pack, which would panic.
			return nil, &MetaDataError{Message: fmt.Sprintf(
				"record type %q: record type key %v (%T) cannot be used as a key", name, key, key)}
		}
		if prevName, exists := typeKeySeen[dedup]; exists {
			return nil, &MetaDataError{Message: fmt.Sprintf(
				"record types %q and %q have the same record type key %v", prevName, name, key)}
		}
		typeKeySeen[dedup] = name
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
	// Use normalizeSubspaceKey to handle type mismatches after proto round-trip.
	indexSubspaceKeySeen := make(map[any]string)
	for _, idx := range indexes {
		sk := normalizeSubspaceKey(idx.SubspaceTupleKey())
		if prevName, exists := indexSubspaceKeySeen[sk]; exists {
			return nil, &MetaDataError{Message: fmt.Sprintf("indexes %q and %q have the same subspace key %v", prevName, idx.Name, sk)}
		}
		indexSubspaceKeySeen[sk] = idx.Name
	}

	// Validate no former index subspace key conflicts with current indexes.
	// Use normalizeSubspaceKey to handle type mismatches after proto round-trip
	// (e.g. int vs int64 from FDB tuple unpack). Bug 13 fix.
	for _, fi := range b.formerIndexes {
		for _, idx := range indexes {
			if normalizeSubspaceKey(fi.SubspaceKey) == normalizeSubspaceKey(idx.SubspaceTupleKey()) {
				return nil, &MetaDataError{Message: fmt.Sprintf("index %q reuses subspace key of former index %q", idx.Name, fi.FormerName)}
			}
		}
	}

	// Validate former index version ordering.
	// Matches Java's MetaDataValidator: addedVersion ≤ removedVersion, both ≤ metadata version.
	for _, fi := range b.formerIndexes {
		if fi.AddedVersion > fi.RemovedVersion {
			return nil, &MetaDataError{Message: fmt.Sprintf("former index %q has addedVersion (%d) > removedVersion (%d)", fi.FormerName, fi.AddedVersion, fi.RemovedVersion)}
		}
		if fi.AddedVersion > b.version {
			return nil, &MetaDataError{Message: fmt.Sprintf("former index %q has addedVersion (%d) > metadata version (%d)", fi.FormerName, fi.AddedVersion, b.version)}
		}
		if fi.RemovedVersion > b.version {
			return nil, &MetaDataError{Message: fmt.Sprintf("former index %q has removedVersion (%d) > metadata version (%d)", fi.FormerName, fi.RemovedVersion, b.version)}
		}
	}

	// Validate index addedVersion ≤ lastModifiedVersion.
	// Matches Java's IndexValidator: addedVersion ≤ lastModifiedVersion.
	for _, idx := range indexes {
		if idx.AddedVersion > 0 && idx.LastModifiedVersion > 0 && idx.AddedVersion > idx.LastModifiedVersion {
			return nil, &MetaDataError{Message: fmt.Sprintf("index %q has addedVersion (%d) > lastModifiedVersion (%d)", idx.Name, idx.AddedVersion, idx.LastModifiedVersion)}
		}
	}

	// Validate index versions do not exceed the metadata version.
	// Matches Java's MetaDataValidator.validateIndex() (MetaDataValidator.java:124-133).
	// An index whose lastModifiedVersion is ahead of the metadata version reads as
	// "added since" every store header version forever, so each later version bump
	// re-decides its rebuild policy and can clear an already-built index.
	for _, idx := range indexes {
		if idx.AddedVersion > b.version {
			return nil, &IndexVersionTooNewError{
				IndexName:           idx.Name,
				Kind:                IndexVersionAdded,
				AddedVersion:        idx.AddedVersion,
				LastModifiedVersion: idx.LastModifiedVersion,
				MetaDataVersion:     b.version,
			}
		}
		if idx.LastModifiedVersion > b.version {
			return nil, &IndexVersionTooNewError{
				IndexName:           idx.Name,
				Kind:                IndexVersionLastModified,
				AddedVersion:        idx.AddedVersion,
				LastModifiedVersion: idx.LastModifiedVersion,
				MetaDataVersion:     b.version,
			}
		}
	}

	// Validate record type since-versions do not exceed the metadata version.
	// Matches Java's MetaDataValidator.validateRecordType() (MetaDataValidator.java:88-92).
	for name, rt := range b.recordTypes {
		if rt.SinceVersion > b.version {
			return nil, &MetaDataError{Message: fmt.Sprintf(
				"record type %q has since version %d which is greater than the meta-data version %d",
				name, rt.SinceVersion, b.version)}
		}
	}

	// Validate atomic index types require GroupingKeyExpression as root.
	// Matches Java's AtomicMutationIndexMaintainerFactory.getIndexValidator() which calls
	// validateGrouping(), and IndexValidator.validateGrouping() which throws if the root
	// expression is not a GroupingKeyExpression.
	// Without this, indexGroupingCount() silently treats all columns as "grouping" and
	// zero as "grouped/aggregated", causing the index to malfunction (bugs #26-28).
	for _, idx := range indexes {
		switch canonicalIndexType(idx.Type) {
		case IndexTypeCount, IndexTypeCountNotNull, IndexTypeCountUpdates,
			IndexTypeSum,
			IndexTypeMinEverLong, IndexTypeMaxEverLong,
			IndexTypeMinEverTuple, IndexTypeMaxEverTuple:
			if _, ok := idx.RootExpression.(*GroupingKeyExpression); !ok {
				return nil, &MetaDataError{Message: fmt.Sprintf(
					"%s index %q requires a GroupingKeyExpression as root expression; "+
						"wrap with Ungrouped(), GroupAll(), or GroupBy()",
					idx.Type, idx.Name)}
			}
		}
	}

	// Validate BITMAP_VALUE indexes.
	// Matches Java's BitmapValueIndexMaintainerFactory.getIndexValidator() which calls
	// validateGrouping(1) and validateNotVersion(). The root expression must be a
	// GroupingKeyExpression with exactly 1 grouped column (the position field).
	for _, idx := range indexes {
		if idx.Type != IndexTypeBitmapValue {
			continue
		}
		gke, ok := idx.RootExpression.(*GroupingKeyExpression)
		if !ok {
			return nil, &MetaDataError{Message: fmt.Sprintf(
				"BITMAP_VALUE index %q requires a GroupingKeyExpression as root expression; "+
					"wrap with GroupBy()",
				idx.Name)}
		}
		if gke.GetGroupedCount() != 1 {
			return nil, &MetaDataError{Message: fmt.Sprintf(
				"BITMAP_VALUE index %q must have exactly 1 grouped column (the position field), got %d",
				idx.Name, gke.GetGroupedCount())}
		}
	}

	// Validate VERSION indexes.
	// Matches Java's VersionIndexMaintainerFactory.getIndexValidator() which calls:
	//   validateNotGrouping(), validateStoresRecordVersions(), validateVersionKey(), validateNotUnique().
	for _, idx := range indexes {
		if idx.Type != IndexTypeVersion {
			continue
		}
		if !b.storeRecordVersions {
			return nil, &MetaDataError{Message: fmt.Sprintf("VERSION index %q requires SetStoreRecordVersions(true)", idx.Name)}
		}
		if idx.IsUnique() {
			return nil, &MetaDataError{Message: fmt.Sprintf("VERSION index %q does not support unique", idx.Name)}
		}
		if _, ok := idx.RootExpression.(*GroupingKeyExpression); ok {
			return nil, &MetaDataError{Message: fmt.Sprintf("VERSION index %q does not support grouping", idx.Name)}
		}
		if countVersionColumns(idx.RootExpression) != 1 {
			return nil, &MetaDataError{Message: fmt.Sprintf("VERSION index %q: there must be exactly 1 version entry in index", idx.Name)}
		}
	}

	// Validate MAX_EVER_VERSION indexes.
	// Matches Java's AtomicMutationIndexMaintainerFactory validator:
	//   validateGrouping(1), validateVersionInGroupedKeys(), validateStoresRecordVersions().
	// Must have exactly 1 version column in the grouped (aggregated) portion,
	// no version columns in the grouping portion, and storeRecordVersions enabled.
	for _, idx := range indexes {
		if idx.Type != IndexTypeMaxEverVersion {
			continue
		}
		if !b.storeRecordVersions {
			return nil, &MetaDataError{Message: fmt.Sprintf("MAX_EVER_VERSION index %q requires SetStoreRecordVersions(true)", idx.Name)}
		}
		gke, ok := idx.RootExpression.(*GroupingKeyExpression)
		if !ok {
			return nil, &MetaDataError{Message: fmt.Sprintf("MAX_EVER_VERSION index %q must use a GroupingKeyExpression", idx.Name)}
		}
		// Check version columns in grouping vs grouped portions by examining the
		// child expressions of the whole key's composite. The first groupingCount
		// columns are grouping; the rest are grouped.
		groupingCount := gke.GetGroupingCount()
		groupedCount := gke.GetGroupedCount()
		if groupedCount < 1 {
			return nil, &MetaDataError{Message: fmt.Sprintf("MAX_EVER_VERSION index %q must have at least 1 grouped column", idx.Name)}
		}
		// Count version columns in grouping vs grouped portions.
		groupingVersionCount, groupedVersionCount := countVersionColumnsInGroupParts(gke.wholeKey, groupingCount)
		if groupingVersionCount != 0 {
			return nil, &MetaDataError{Message: fmt.Sprintf("MAX_EVER_VERSION index %q: there must be no version entries in grouping key", idx.Name)}
		}
		if groupedVersionCount != 1 {
			return nil, &MetaDataError{Message: fmt.Sprintf("MAX_EVER_VERSION index %q: there must be exactly 1 version entry in grouped key", idx.Name)}
		}
	}

	// Validate index replacement chains.
	// Matches Java's MetaDataValidator.validateIndex(): replacement indexes must exist
	// and must not themselves have replacements (no multi-level chains).
	for _, idx := range indexes {
		replacements := idx.GetReplacedByIndexNames()
		for _, replacementName := range replacements {
			replacement, exists := indexes[replacementName]
			if !exists {
				return nil, &MetaDataError{Message: fmt.Sprintf("index %q has replacement index %q that is not in the metadata", idx.Name, replacementName)}
			}
			if len(replacement.GetReplacedByIndexNames()) > 0 {
				return nil, &MetaDataError{Message: fmt.Sprintf("index %q has replacement index %q that itself has replacement indexes", idx.Name, replacementName)}
			}
		}
	}

	// No record-type-key binding happens here, deliberately. A key expression
	// graph can be shared by more than one metadata build, so anything this
	// builder wrote onto a RecordTypeKeyExpression would be visible to the
	// OTHER metadata built from the same graph — last build wins, for both.
	// RecordTypeKeyExpression resolves the key from the record's own type at
	// evaluation time, exactly as Java's does, which needs no binding at all.

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
		for _, idx := range rt.multiTypeIndexes {
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

	// Pre-compute union field numbers and message factories for direct wire
	// format encoding/decoding (skips UnionDescriptor allocation entirely).
	fnToRT := make(map[protowire.Number]*RecordType, len(types))
	for _, rt := range types {
		if rt.UnionFieldDescriptor != nil {
			rt.unionFieldNumber = rt.UnionFieldDescriptor.Number()
			msgType, err := protoregistry.GlobalTypes.FindMessageByName(rt.Descriptor.FullName())
			if err != nil {
				// Dynamic schemas (not in global proto registry) fall back to dynamicpb.
				// This allows runtime-constructed schemas (e.g. from DDL) to be used
				// for both serialization and deserialization.
				desc := rt.Descriptor
				rt.newMessage = func() proto.Message { return dynamicpb.NewMessage(desc) }
			} else {
				rt.newMessage = func() proto.Message { return msgType.New().Interface() }
			}
			fnToRT[rt.unionFieldNumber] = rt
		}
	}

	return &RecordMetaData{
		recordTypes:             types,
		fileDescriptor:          b.fileDescriptor,
		recordsSourceProto:      b.recordsSourceProto,
		version:                 b.version,
		recordCountKey:          b.recordCountKey,
		storeRecordVersions:     b.storeRecordVersions,
		splitLongRecords:        b.splitLongRecords,
		indexes:                 indexes,
		universalIndexes:        b.universalIndexes,
		formerIndexes:           b.formerIndexes,
		unionDescriptor:         b.unionDescriptor,
		fieldNumberToRecordType: fnToRT,
		subspaceKeyCounter:      b.subspaceKeyCounter,
		usesSubspaceKeyCounter:  b.counterBasedSubspaceKeys,
		preserved:               b.preserved,
	}, nil
}

// FindRegisteredMessageType returns the message type registered for fullName
// in the process-global protobuf registry (protoregistry.GlobalTypes) — the
// GENERATED Go type (e.g. *gen.Order) — or nil when the name is not registered
// (a dynamic, metadata-built schema type). This is the same registry-first
// policy Build applies to record-type message factories above: a caller that
// rebuilds a proto value from serialized bytes must prefer the generated type
// so the restored value has the SAME concrete Go type as a freshly-read
// record, falling back to dynamicpb over metadata descriptors only for
// dynamic schemas (where fresh records are dynamicpb too).
func FindRegisteredMessageType(fullName protoreflect.FullName) protoreflect.MessageType {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(fullName)
	if err != nil {
		return nil
	}
	return mt
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
// Matches Java's RecordTypeBuilder.setRecordTypeKey(Key.Evaluated), which
// delegates to RecordTypeIndexesBuilder.setRecordTypeKey: reject anything that
// is not a primitive, then store TupleTypeUtil.toTupleEquivalentValue(key).
//
// Canonicalizing HERE, not at every read, is the whole point. The key is read
// by duplicate-key validation, by proto serialization, by DeleteRecordsWhere's
// prefix comparison and by the tuple packer, and each of those asks a
// different question of it — equality, encodability, wire bytes. A value
// normalized only on the evaluation path leaves the others looking at a
// different representation of the same key, which is how int64(7) and uint(7)
// came to be two "distinct" keys that encode to identical bytes.
//
// A rejected key is reported from Build() rather than panicking: Java throws
// MetaDataException from the setter, but a Go builder setter returns the
// builder for chaining and library code must not panic, so the error is
// deferred to the one call that already returns one.
func (rtb *RecordTypeBuilder) SetRecordTypeKey(key any) *RecordTypeBuilder {
	canonical, err := canonicalRecordTypeKey(key)
	if err != nil {
		// Wrapped with %w, not flattened into a message: the caller has to be
		// able to match the cause with errors.As, which is how Go says
		// `catch (MetaDataException e)`.
		rtb.builder.buildErrors = append(rtb.builder.buildErrors,
			fmt.Errorf("record type %q: %w", rtb.recordType.Name, err))
		return rtb
	}
	rtb.recordType.explicitRecordTypeKey = canonical
	return rtb
}

// RecordTypeKeyTypeError reports a record type key whose Go type cannot be a
// record type key. Java's equivalent is the MetaDataException "Only primitive
// types are allowed as record type key" thrown by
// RecordTypeIndexesBuilder.setRecordTypeKey.
type RecordTypeKeyTypeError struct {
	// Key is the offending value.
	Key any
	// Reason distinguishes the two ways a key is refused. Empty means the Go
	// TYPE is not one a record type key may have, which is Java's wording
	// verbatim. A non-empty reason means the type is fine but this VALUE
	// cannot be represented, where Java's message would be actively
	// misleading — a uint64 above max int64 is a primitive, and saying
	// otherwise sends the reader looking for the wrong mistake.
	Reason string
}

func (e *RecordTypeKeyTypeError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("record type key %v (%T) cannot be used: %s", e.Key, e.Key, e.Reason)
	}
	return fmt.Sprintf("only primitive types are allowed as record type key, got %T (%v)", e.Key, e.Key)
}

// canonicalRecordTypeKey validates and canonicalizes a record type key, the
// port of Java's setRecordTypeKey guard followed by
// TupleTypeUtil.toTupleEquivalentValue.
//
// The accepted set is Java's — null, Number, Boolean, String, byte[] —
// intersected with what the tuple encoder can encode AND what the metadata
// proto can carry. Everything else is refused HERE, where the caller learns
// about it from Build(), rather than at pack time: the encoder's default arm
// panics ("unencodable element"), so a value the builder accepts but the
// encoder cannot write turns a metadata mistake into a panic on the save path.
// A named integer type (`type k int`) is refused for that exact reason — it is
// not one of the encoder's cases.
//
// The proto clause is the second half of that rule and is what excludes two
// values the ENCODER would take. RecordKeyExpressionProto.Value has exactly
// one integer field per width (double, float, int64, bool, string, bytes,
// int32) and no unsigned or big-integer field, so:
//
//   - a big.Int is refused even though the encoder writes it, because no
//     metadata carrying one could ever be exported or read back; and
//   - a uint/uint64 above math.MaxInt64 is refused for the same reason. It
//     packs, and it used to build and save — but ToProto then failed on it
//     ("unsupported value type uint64"), which is the accepted-here /
//     broken-there split this whole function exists to remove. Java cannot
//     express it either: its only unsigned-capable Number is BigInteger, and
//     LiteralKeyExpression.toProtoValue funnels every non-Integer Number
//     through longValue(), silently TRUNCATING it to a wrong key.
//
// Refusing at the door is what keeps validation, serialization, prefix
// comparison and packing looking at one representation.
//
// Canonicalization is limited to what leaves the tuple encoding IDENTICAL:
// every integer that fits in an int64 becomes an int64, because the tuple
// encoding of an integer does not depend on the signedness or width of the Go
// type it arrived in. uint/uint64 above math.MaxInt64 stay unsigned — the
// encoder writes them natively and no int64 can represent them. float32 and
// float64 are NOT folded together: they have different tuple type codes.
func canonicalRecordTypeKey(key any) (any, error) {
	switch k := key.(type) {
	case nil:
		return nil, nil
	case int:
		return int64(k), nil
	case int8:
		return int64(k), nil
	case int16:
		return int64(k), nil
	case int32:
		return int64(k), nil
	case int64:
		return k, nil
	case uint8:
		return int64(k), nil
	case uint16:
		return int64(k), nil
	case uint32:
		return int64(k), nil
	case uint:
		if uint64(k) > math.MaxInt64 {
			return nil, &RecordTypeKeyTypeError{Key: key, Reason: "above max int64, and the metadata proto has no unsigned field to carry it"}
		}
		return int64(k), nil
	case uint64:
		if k > math.MaxInt64 {
			return nil, &RecordTypeKeyTypeError{Key: key, Reason: "above max int64, and the metadata proto has no unsigned field to carry it"}
		}
		return int64(k), nil
	case string:
		return k, nil
	case []byte:
		// A nil slice is Java's null byte[] reference, which its setter's
		// `recordTypeKey == null` arm accepts as "no explicit key" — so it
		// must land as an untyped nil here. Returning a typed-nil []byte
		// instead made HasExplicitRecordTypeKey report true for a key that
		// proto serialization then dropped, so the type silently fell back to
		// its union field number on reload.
		if k == nil {
			return nil, nil
		}
		// Copied, as Java's ByteString.copyFrom copies: the key must not
		// change under the metadata if the caller reuses its slice.
		//
		// make+copy, NOT append([]byte(nil), k...): append returns a NIL
		// slice for an empty input, and nil is how this field spells "absent".
		// An empty bytes key is a real key — Java's ByteString.EMPTY is
		// non-null and its reader tests hasBytesValue(), field PRESENCE, not
		// emptiness — and the proto layer here preserves that presence too (a
		// non-nil empty slice marshals to the tag with length 0). Nilling it
		// in the copy is what made the key vanish across ToProto/FromProto,
		// leaving every record written under the empty-bytes prefix
		// unreachable.
		out := make([]byte, len(k))
		copy(out, k)
		return out, nil
	case bool:
		return k, nil
	case float32:
		return k, nil
	case float64:
		return k, nil
	default:
		return nil, &RecordTypeKeyTypeError{Key: key}
	}
}

// recordTypeKeyDedupKey renders a record type key as the bytes it will occupy
// in a key, which is the only thing that decides whether two record types
// collide. Comparing the values themselves cannot answer that: int64(7) and
// uint(7) are different values that encode identically (a real collision),
// while "abc" and []byte("abc") are equal-looking and encode differently
// (tuple type codes 0x02 and 0x01, not a collision), and []byte is not even
// comparable in Go.
// recordTypeKeyIdentity renders a record type key as the bytes it occupies in
// a key. It is the ONE identity every comparison of record type keys goes
// through — duplicate detection at Build, lookup by key, and the evolution
// validator's old-vs-new matching — because those questions are the same
// question and answering them with different functions is how they came to
// disagree.
//
// Two keys are the same key exactly when they encode to the same bytes. That
// is what Java compares, by a different route: it stores a byte[] key as a
// ByteString and asks .equals(), so a ByteString never equals a String and
// int64(7) never coexists with a colliding spelling. Folding bytes into a
// string to make them comparable — which the general-purpose subspace-key
// normalizer does — merges "k" and []byte("k"), two keys with DIFFERENT tuple
// type codes that live in different key spaces. Build admits both, so any
// comparison that folds them lets a lookup return either type depending on map
// iteration order.
//
// Reports false for a value that cannot be a record type key, so callers can
// answer "no match" instead of packing something the encoder would panic on.
// Every stored key is already canonical (both doors into the field
// canonicalize), so false here means the CALLER supplied a non-key value.
func recordTypeKeyIdentity(key any) (string, bool) {
	canonical, err := canonicalRecordTypeKey(key)
	if err != nil {
		return "", false
	}
	return string(tuple.Tuple{canonical}.Pack()), true
}

// GetRecordType returns the record type for the given name.
//
// Record types are keyed by their STORAGE name — the ProtoUtils-escaped
// descriptor name, which is what the wire carries. A caller holding a USER
// identifier (SQL text: a table declared `"foo$table"` is stored as
// FOO__1TABLE) resolves through the escape on a miss.
//
// Java does not need this because its relational layer keeps its OWN table
// map keyed by the user name (RecordLayerSchemaTemplate.getTable) and never
// addresses RecordMetaData with a user identifier; Go's relational layer
// uses RecordMetaData directly as its table catalog, so the translation
// lives here — ONE boundary that every SQL path already funnels through —
// rather than being sprinkled over the 25 call sites. Storage names stay
// canonical: the fallback fires only when the direct key misses, so it can
// never shadow a real type, and ToProtoBufCompliantName is deterministic.
func (m *RecordMetaData) GetRecordType(name string) *RecordType {
	if rt, ok := m.recordTypes[name]; ok {
		return rt
	}
	if storage, err := ToProtoBufCompliantName(name); err == nil && storage != name {
		return m.recordTypes[storage]
	}
	return nil
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

// GetIndexes returns the indexes defined for this record type (single-type only).
// Does not include multi-type or universal indexes.
// Matches Java's RecordType.getIndexes().
func (rt *RecordType) GetIndexes() []*Index {
	return rt.indexes
}

// GetMultiTypeIndexes returns the multi-type indexes for this record type.
// Matches Java's RecordType.getMultiTypeIndexes().
func (rt *RecordType) GetMultiTypeIndexes() []*Index {
	return rt.multiTypeIndexes
}

// GetAllIndexes returns all indexes for this record type (single-type + multi-type).
// Does not include universal indexes.
// Matches Java's RecordType.getAllIndexes().
func (rt *RecordType) GetAllIndexes() []*Index {
	if len(rt.multiTypeIndexes) == 0 {
		return rt.indexes
	}
	all := make([]*Index, 0, len(rt.indexes)+len(rt.multiTypeIndexes))
	all = append(all, rt.indexes...)
	all = append(all, rt.multiTypeIndexes...)
	return all
}

// HasExplicitRecordTypeKey returns true if the record type key was explicitly set.
// Matches Java's RecordType.hasExplicitRecordTypeKey().
func (rt *RecordType) HasExplicitRecordTypeKey() bool {
	return rt.explicitRecordTypeKey != nil
}

// GetRecordTypeKey returns the explicit record type key if set, or falls back
// to the record type index. Matches Java's RecordType.getRecordTypeKey().
//
// The stored value is ALREADY canonical — SetRecordTypeKey and the proto
// reader canonicalize on the way in, as Java's setter does — so this returns
// it verbatim. Normalizing on the way out instead would leave every other
// consumer of the field (duplicate validation, proto export, prefix
// comparison) looking at the raw value while only the evaluation path saw the
// canonical one, which is the split that let equal-encoding keys pass
// validation as distinct.
//
// The derived arm converts the record type index because RecordTypeIndex is a
// declared int field, not a caller-supplied value: the conversion is exact and
// total, and it gives int and int64 spellings of a key one representation.
func (rt *RecordType) GetRecordTypeKey() any {
	if rt.explicitRecordTypeKey != nil {
		return rt.explicitRecordTypeKey
	}
	return int64(rt.RecordTypeIndex)
}

// PrimaryKeyHasRecordTypePrefix returns true if this record type's primary key
// starts with a RecordTypeKeyExpression — i.e. its records live in a contiguous
// record-type-keyed sub-range of the records space.
// Matches Java's RecordType.primaryKeyHasRecordTypePrefix().
func (rt *RecordType) PrimaryKeyHasRecordTypePrefix() bool {
	return primaryKeyStartsWithRecordType(rt.PrimaryKey)
}

// IsSynthetic reports whether this is a synthetic record type (one assembled from
// other records, e.g. a joined type). The Go port does not model synthetic record
// types — they are out of scope (see CLAUDE.md) — so this is always false. Kept as
// a method for 1:1 fidelity with Java's RecordType.isSynthetic() so callers (e.g.
// the typed-records range preset) read identically to the Java algorithm.
//
// The constant false is SOUND ONLY BECAUSE no synthetic type can reach a
// *RecordType. Record types are built from the union descriptor's fields, and a
// joined or unnested type is declared in the metadata proto rather than in the
// union — so it never becomes one of these. The dangerous reading is that
// synthetic types are absent from the METADATA, which is not what this says and
// is not true once fields 12/13 are carried. Callers that need the metadata-level
// question must ask RecordMetaData.DeclaresSyntheticRecordTypes.
func (rt *RecordType) IsSynthetic() bool {
	return false
}

// DeclaresSyntheticRecordTypes reports whether the metadata DECLARES joined or
// unnested record types — types this port carries verbatim (see
// preservedMetaDataFields) but does not model.
//
// It exists so that "Go does not model synthetic types" cannot be silently
// mistaken for "this metadata has none". Those are different claims, and only
// the second one licenses treating the record-type set as complete. A caller
// that computes over "all record types" — a scan range, a coverage decision, a
// count — is computing over a set that omits the synthetic ones, and must
// refuse rather than answer from the partial set.
func (m *RecordMetaData) DeclaresSyntheticRecordTypes() bool {
	return len(m.preserved.joinedRecordTypes) > 0 || len(m.preserved.unnestedRecordTypes) > 0
}

// SyntheticRecordTypeNames returns the declared joined/unnested type names, for
// diagnostics. The port does not model these types; this reads their names off
// the carried protos so an error can say which ones it refused for.
func (m *RecordMetaData) SyntheticRecordTypeNames() []string {
	names := make([]string, 0,
		len(m.preserved.joinedRecordTypes)+len(m.preserved.unnestedRecordTypes))
	for _, jt := range m.preserved.joinedRecordTypes {
		names = append(names, jt.GetName())
	}
	for _, ut := range m.preserved.unnestedRecordTypes {
		names = append(names, ut.GetName())
	}
	sort.Strings(names)
	return names
}

// GetSubspaceKeyCounter returns the counter-based subspace-key counter value.
// Matches Java's RecordMetaData.getSubspaceKeyCounter().
func (m *RecordMetaData) GetSubspaceKeyCounter() int64 {
	return m.subspaceKeyCounter
}

// UsesSubspaceKeyCounter reports whether index subspace keys are assigned from a
// counter rather than from index names.
// Matches Java's RecordMetaData.usesSubspaceKeyCounter().
func (m *RecordMetaData) UsesSubspaceKeyCounter() bool {
	return m.usesSubspaceKeyCounter
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

// RecordTypesForIndex returns the record types that the given index covers.
// Universal indexes cover all record types. Type-specific indexes cover only
// the record types they are associated with.
// Matches Java's RecordMetaData.recordTypesForIndex(Index).
func (m *RecordMetaData) RecordTypesForIndex(idx *Index) []*RecordType {
	// Check if it's a universal index.
	for _, ui := range m.universalIndexes {
		if ui.Name == idx.Name {
			result := make([]*RecordType, 0, len(m.recordTypes))
			for _, rt := range m.recordTypes {
				result = append(result, rt)
			}
			return result
		}
	}
	// Type-specific: find which types have this index.
	var result []*RecordType
	for _, rt := range m.recordTypes {
		for _, i := range m.GetIndexesForRecordType(rt.Name) {
			if i.Name == idx.Name {
				result = append(result, rt)
				break
			}
		}
	}
	return result
}

// GetFormerIndexes returns all former (deleted) indexes.
// Matches Java's RecordMetaData.getFormerIndexes().
func (m *RecordMetaData) GetFormerIndexes() []*FormerIndex {
	return m.formerIndexes
}

// GetRecordTypeFromRecordTypeKey returns the record type with the given type key.
// Returns nil if no record type matches.
// Matches Java's RecordMetaData.getRecordTypeFromRecordTypeKey(), which scans
// the record types comparing getRecordTypeKey().equals(recordTypeKey).
//
// Comparison goes through recordTypeKeyIdentity — the same identity the
// duplicate check uses — so a lookup can never answer a question Build did not
// already settle. Comparing through the general subspace-key normalizer folded
// a []byte key into the string of the same bytes, and Build admits that pair
// as two distinct types, so the lookup returned whichever of them the map
// happened to yield first.
func (m *RecordMetaData) GetRecordTypeFromRecordTypeKey(key any) *RecordType {
	wanted, ok := recordTypeKeyIdentity(key)
	if !ok {
		return nil
	}
	for _, rt := range m.recordTypes {
		if id, idOK := recordTypeKeyIdentity(rt.GetRecordTypeKey()); idOK && id == wanted {
			return rt
		}
	}
	return nil
}

// GetFormerIndexesSince returns former indexes removed since the given version.
// Matches Java's RecordMetaData.getFormerIndexesSince(int).
func (m *RecordMetaData) GetFormerIndexesSince(version int) []*FormerIndex {
	var result []*FormerIndex
	for _, fi := range m.formerIndexes {
		if fi.RemovedVersion > version {
			result = append(result, fi)
		}
	}
	return result
}

// GetIndexFromSubspaceKey returns the index with the given subspace key, or nil.
// Matches Java's RecordMetaData.getIndexFromSubspaceKey().
func (m *RecordMetaData) GetIndexFromSubspaceKey(key any) *Index {
	normalized := normalizeSubspaceKey(key)
	for _, idx := range m.indexes {
		if normalizeSubspaceKey(idx.SubspaceTupleKey()) == normalized {
			return idx
		}
	}
	return nil
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

// GetUnionDescriptor returns the protobuf message descriptor for UnionDescriptor.
// Returns nil if the schema has no union (single-type schema).
// Matches Java's RecordMetaData.getUnionDescriptor().
func (m *RecordMetaData) GetUnionDescriptor() protoreflect.MessageDescriptor {
	return m.unionDescriptor
}

// GetUnionFieldForRecordType returns the union field descriptor for a record type.
// Returns nil if the record type has no union field (single-type schema).
// Matches Java's RecordMetaData.getUnionFieldForRecordType().
func (m *RecordMetaData) GetUnionFieldForRecordType(rt *RecordType) protoreflect.FieldDescriptor {
	return rt.UnionFieldDescriptor
}

// CommonPrimaryKey returns the primary key expression if all record types share
// the same one, or nil if they differ. Uses structural comparison via
// keyExpressionEquals. Matches Java's RecordMetaData.commonPrimaryKey().
func (m *RecordMetaData) CommonPrimaryKey() KeyExpression {
	var common KeyExpression
	first := true
	for _, rt := range m.recordTypes {
		if first {
			common = rt.PrimaryKey
			first = false
		} else if !keyExpressionEquals(common, rt.PrimaryKey) {
			return nil
		}
	}
	return common
}

// CommonPrimaryKeyLength returns the number of columns in the primary key if
// all record types have the same PK length, or -1 if they differ.
// Matches Java's RecordMetaData.commonPrimaryKeyLength().
func (m *RecordMetaData) CommonPrimaryKeyLength() int {
	common := -1
	first := true
	for _, rt := range m.recordTypes {
		size := rt.PrimaryKey.ColumnSize()
		if first {
			common = size
			first = false
		} else if common != size {
			return -1
		}
	}
	return common
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

// normalizeSubspaceKey normalizes a subspace key for comparison. All integer
// types (int, int32, int64) are normalized to int64 so that Go's any equality
// works correctly after proto round-trip (FDB tuple unpack returns int64,
// valueFromProto may return int32, and Go code may use int). Without this,
// int64(42) != int(42) in Go's any comparison. Fixes bug 13.
//
// `[]byte` keys are normalized to `string` because byte slices are
// unhashable in Go and would panic when used as a map key. Adversarial
// proto inputs (e.g. via the FuzzRecordMetaDataFromProto fuzz target)
// can carry `[]byte` subspace keys; without this branch,
// `RecordMetaDataBuilder.Build` would panic with "hash of unhashable
// type: []uint8" rather than returning a typed error. The string-cast
// preserves byte-equality semantics for keys with the same byte
// content, but does collapse `[]byte("x")` and `"x"` into the same
// equivalence class — that's a harmless conflation here because any
// metadata that mixes the two for the same logical subspace is
// already malformed.
func normalizeSubspaceKey(key any) any {
	switch k := key.(type) {
	case int:
		return int64(k)
	case int32:
		return int64(k)
	case int64:
		return k
	case []byte:
		return string(k)
	case tuple.Tuple:
		// Nested tuples are produced by `fastDecodeTuple` when the
		// proto-encoded subspace key carries an FDB nested-tuple type
		// code. Like `[]byte`, a `tuple.Tuple` (= []any) is unhashable
		// in Go and would panic on map insert. Java doesn't currently
		// emit nested tuples as subspace keys, so this is preemptive
		// hardening rather than a known-triggering case (the fuzz has
		// run 16M+ iterations without finding one), but the cost is
		// trivial and the alternative is a surprise panic if the input
		// ever takes that shape.
		return fmt.Sprintf("%v", k)
	default:
		return key
	}
}

// countVersionColumnsInGroupParts counts version columns in the grouping
// (first groupingCount columns) and grouped (remaining) portions of a key expression.
// Used by MAX_EVER_VERSION validation. Works by walking composite children left-to-right,
// accumulating column sizes.
func countVersionColumnsInGroupParts(expr KeyExpression, groupingCount int) (groupingVersions, groupedVersions int) {
	if comp, ok := expr.(*CompositeKeyExpression); ok {
		colsSoFar := 0
		for _, child := range comp.expressions {
			childCols := child.ColumnSize()
			childVersions := countVersionColumns(child)
			if colsSoFar+childCols <= groupingCount {
				groupingVersions += childVersions
			} else if colsSoFar >= groupingCount {
				groupedVersions += childVersions
			} else {
				// Child spans the boundary — shouldn't happen with well-formed
				// expressions, but handle conservatively.
				groupingVersions += childVersions
			}
			colsSoFar += childCols
		}
		return
	}
	// Non-composite: if groupingCount > 0, all columns are grouping
	totalVersions := countVersionColumns(expr)
	if groupingCount > 0 {
		return totalVersions, 0
	}
	return 0, totalVersions
}

// countVersionColumns returns the number of VersionKeyExpression columns in a
// key expression tree. Matches Java's KeyExpression.versionColumns() which
// defaults to 0 and sums through composite/grouping/nesting/keyWithValue.
func countVersionColumns(expr KeyExpression) int {
	if expr == nil {
		return 0
	}
	switch e := expr.(type) {
	case *VersionKeyExpression:
		return 1
	case *CompositeKeyExpression:
		total := 0
		for _, child := range e.expressions {
			total += countVersionColumns(child)
		}
		return total
	case *GroupingKeyExpression:
		return countVersionColumns(e.wholeKey)
	case *KeyWithValueExpression:
		return countVersionColumns(e.innerKey)
	case *NestingKeyExpression:
		return countVersionColumns(e.child)
	case *RecordTypeKeyExpression:
		if e.nested != nil {
			return countVersionColumns(e.nested)
		}
		return 0
	case *FunctionKeyExpression:
		return countVersionColumns(e.arguments)
	default:
		return 0
	}
}
