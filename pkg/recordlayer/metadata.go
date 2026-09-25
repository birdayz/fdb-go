package recordlayer

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"sort"
	"sync/atomic"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/protoname"
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

	// Whether two declared record types collide across the SQL and storage
	// namespaces, DERIVED AT CONSTRUCTION by Build alongside
	// fieldNumberToRecordType, not memoised on first ask.
	//
	// A lazy memo would answer whatever the declared set happened to be the
	// first time anyone asked, so its staleness under mutation would depend on
	// call ORDER -- mutate-then-ask and ask-then-mutate-then-ask giving
	// different answers for the same object. Deriving it here makes the value
	// unambiguously "the declared set Build saw", which is the same contract
	// every other derived field on this struct already has.
	//
	// It is computed rather than recomputed per call because the CLI asks once
	// per rendered name: a record scan is O(records x types) and a type listing
	// O(types^2), re-deriving one answer for every row it prints. O(types) once.
	ambiguousNames []string
	ambiguousFound bool

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

	// unionDescriptor is the records file's union message, found as Java's
	// fetchUnionDescriptor finds it. Every built RecordMetaData has one: Java has
	// no union-less mode, and neither does Build.
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
	storedQueries        []*gen.PStoredQuery

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

	// reachesMap is whether the record message can hold a map field at any
	// depth, computed at Build (record_wire_map_order.go), and mapReach the
	// answer for every message type the meta-data's record types reach, shared
	// read-only by the walks of its records.
	reachesMap bool
	mapReach   mapReach

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
	// buildErrors are the faults the builder's calls recorded, in call order,
	// and buildErrorSeqs their places in program order (nextBuildFaultSeq),
	// which Build compares with the SetSubspaceKey refusals of addedIndexes.
	buildErrors    []error
	buildErrorSeqs []uint64
	// addedIndexes is every index handed to an AddIndex, kept even when the
	// add was refused or the index later removed: Java threw a refused
	// SetSubspaceKey at the set, so the refusal ends the program wherever the
	// index went afterwards.
	addedIndexes []*Index
	// handedKeys is every primary key and record count key handed to a
	// setter, kept after a later set replaces it: Java threw a refused
	// constructor where the key was built, so its fault ends the program
	// whatever the builder holds afterwards, a placeholder type's included.
	handedKeys      []KeyExpression
	unionDescriptor protoreflect.MessageDescriptor
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

// SetRecordsWithUnionName is SetRecords for a caller that names the union
// message it expects. The union is still found as Java finds it
// (fetchUnionDescriptor: the one message with (record).usage = UNION, else the
// one named RecordTypeUnion), and a name that differs from the one found is
// refused: metadata built around any other message would be refused by this
// same binary when it is loaded back (RecordMetaDataFromProto runs the same
// search), and by the target. An empty name names no union and is refused: it
// would otherwise accept whatever union the search finds, which is SetRecords.
func (b *RecordMetaDataBuilder) SetRecordsWithUnionName(fd protoreflect.FileDescriptor, unionName string) *RecordMetaDataBuilder {
	if unionName == "" {
		b.recordBuildError(&MetaDataError{Message: "union message name is empty"})
		return b
	}
	return b.setRecords(fd, unionName, true)
}

// SetRecords sets the protobuf file descriptor containing record definitions.
// The union is found as Java's setRecords finds it (fetchUnionDescriptor: the one
// message with (record).usage = UNION, else the one named RecordTypeUnion).
func (b *RecordMetaDataBuilder) SetRecords(fd protoreflect.FileDescriptor) *RecordMetaDataBuilder {
	return b.setRecords(fd, "", true)
}

// A SECOND CALL IS REFUSED, WHICH IS WHY THE ORPHAN ROUTE BELOW IS CLOSED.
// Java's two setRecords overloads both open with
//
//	if (recordsDescriptor != null) { throw new MetaDataException("Records already set."); }
//
// (RecordMetaDataBuilder.java:384, :423; setLocalFileDescriptor and
// addDependency carry the same guard), so a second setter call is not something
// Java tolerates and then repairs downstream — it is not reachable at all. Go
// permitted it, and that divergence is one route to an orphaned index: the
// overwrite replaced every RecordType the SECOND descriptor also declares with a
// fresh one whose index slices are nil -- a type present only in the first
// survived, indexes intact -- while the flat registry b.indexes kept its entry,
// leaving an index registered and associated with nothing. It is not the only
// route, which is why Build now checks the property directly; see the
// association check there. Build succeeded, RecordTypesForIndex came back empty,
// GetIndexesForRecordType lost it, and ToProto then emitted it with an EMPTY
// RecordType list — which a reload reads as UNIVERSAL, because that is Java's
// intended encoding for a universal index. So the index came back either
// maintained for every record type (a silent widening, when its key is valid on
// all of them) or unloadable (when it is not: Build validates a universal index
// against every type and refuses).
//
// The guard records a build error rather than throwing because a SETTER has no
// error channel: it returns *RecordMetaDataBuilder for chaining, so it defers to
// Build, which is what every other rejecting setter here does, and what
// GetRecordType does for an unknown name. The second
// descriptor is NOT applied, so a caller that ignores the Build error still sees
// the first descriptor rather than a half-merged one.
// JAVA HAS AN ESCAPE HATCH THIS PACKAGE DOES NOT: updateRecords
// (RecordMetaDataBuilder.java:451, :476) evolves a descriptor after the first
// one is set, validating the union against the evolution validator and bumping
// the meta-data version. Go has never had it, so refusing the second call
// leaves no way to change a descriptor at all. That is a pre-existing gap, not
// one this guard opened, and it is booked in TODO.md under "The metadata builder
// diverges from Java in three places" with the Java line numbers.
//
// RFC-238 §7f carries the analysis. Pinned by TestSetRecordsRefusesASecondCall,
// TestRefusedSetRecordsLeavesTheFirstDescriptorInPlace (which is the only arm
// that catches a guard written one line lower) and
// TestUniversalIndexRoundTripsThroughAnEmptyRecordTypeList.
//
// setRecords is the one place the guard lives: SetRecords, SetRecordsWithUnionName
// and RecordMetaDataFromProto all come through it. Java refuses a second
// descriptor before it looks for a union (RecordMetaDataBuilder.java:384, then
// fetchUnionDescriptor), so the guard comes first here too. wantUnion, when not
// empty, is the union name a caller expects; a different union is refused.
// processExtensionOptions is Java's flag of the same name: true for a records
// file a program hands the builder, which reads the file's extension options
// (metadata_extension_options.go), and false for stored meta-data, whose
// primary keys and indexes are its own.
func (b *RecordMetaDataBuilder) setRecords(fd protoreflect.FileDescriptor, wantUnion string, processExtensionOptions bool) *RecordMetaDataBuilder {
	if b.fileDescriptor != nil {
		b.recordBuildError(&MetaDataError{Message: "Records already set."})
		return b
	}
	union, err := fetchUnionDescriptor(fd)
	if err != nil {
		b.recordBuildError(err)
		return b
	}
	if wantUnion != "" && string(union.Name()) != wantUnion {
		b.recordBuildError(&MetaDataError{Message: fmt.Sprintf(
			"union message %s is not the union descriptor of the records file (found %s)", wantUnion, union.Name())})
		return b
	}
	b.fileDescriptor = fd
	// Java's validateRecords (RecordMetaDataBuilder.java:635-638) runs whenever a
	// records descriptor is set: data types first, then the union.
	if err := validateRecordDataTypes(fd); err != nil {
		b.recordBuildError(err)
	}

	b.unionDescriptor = union
	if err := validateRecordUnion(fd, union); err != nil {
		b.recordBuildError(err)
	}

	unionFields := union.Fields()

	for i := 0; i < unionFields.Len(); i++ {
		field := unionFields.Get(i)
		if field.Kind() != protoreflect.MessageKind {
			// validateRecordUnion refused it above, in Java's field order.
			continue
		}
		recordMsgDesc := field.Message()
		recordTypeName := string(recordMsgDesc.Name())
		if existing := b.recordTypes[recordTypeName]; existing != nil {
			if existing.Descriptor != recordMsgDesc {
				// Java's processRecordType (RecordMetaDataBuilder.java:893-895).
				b.recordBuildError(&MetaDataError{Message: "There is already a record type named " + recordTypeName})
				continue
			}
			if int(field.Number()) < existing.RecordTypeIndex {
				existing.RecordTypeIndex = int(field.Number())
			}
			canonical := protoreflect.Name("_" + recordTypeName)
			preferred := existing.UnionFieldDescriptor
			if field.Name() == canonical || preferred.Name() != canonical && field.Number() > preferred.Number() {
				existing.UnionFieldDescriptor = field
			}
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
		if processExtensionOptions {
			b.processRecordTypeOptions(recordType)
		}
	}
	if processExtensionOptions {
		b.processSchemaOptions(fd)
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

// SetRecordCountKey sets the key expression for partitioning record counts.
// If set, the store will maintain record counts using FDB atomic ADD mutations.
// If nil (default), record counting is disabled.
// Java equivalent: RecordMetaDataBuilder.setRecordCountKey(KeyExpression)
func (b *RecordMetaDataBuilder) SetRecordCountKey(key KeyExpression) *RecordMetaDataBuilder {
	if !keyExpressionsEqualNilSafe(b.recordCountKey, key) {
		b.version++ // Matches Java: bumps version when value changes
	}
	b.recordCountKey = key
	b.handedKeys = append(b.handedKeys, key)
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
		b.recordBuildError(&MetaDataError{
			Message: "Counter-based subspace keys not enabled",
		})
		return b
	}
	if counter <= b.subspaceKeyCounter {
		b.recordBuildError(&MetaDataError{
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
		// The index was handed to AddIndex even though the add is refused: a
		// SetSubspaceKey refusal recorded on it before this call is a Java
		// throw at that earlier set, so firstFault must still see it.
		b.addedIndexes = append(b.addedIndexes, index)
		b.recordBuildError(&MetaDataError{
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
	b.addedIndexes = append(b.addedIndexes, index)
	if _, exists := b.indexes[index.Name]; exists {
		b.recordBuildError(&MetaDataError{
			Message: fmt.Sprintf("Index %s already defined", index.Name),
		})
		return
	}
	if index.subspaceKey == nil && index.subspaceKeyErr == nil && !index.useExplicitSubspaceKey {
		// A struct literal carries no key. Every Java constructor sets the key
		// to the index name (Index.java:89-97, :132), so the Go index gets it
		// here, before the counter, which replaces a defaulted key.
		index.subspaceKey = index.Name
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
	// A Java program resolves every name before it calls addMultiTypeIndex
	// (getRecordType, RecordMetaDataBuilder.java:986-996, :1177), so an unknown
	// name is the fault and the index is not added. The index is still kept
	// as handed to the builder, as AddIndex keeps a refused one.
	types := make([]*RecordType, 0, len(recordTypeNames))
	for _, name := range recordTypeNames {
		rt, ok := b.recordTypes[name]
		if !ok {
			b.addedIndexes = append(b.addedIndexes, index)
			b.recordBuildError(&MetaDataError{
				Message: fmt.Sprintf("Unknown record type %s", name),
			})
			return b
		}
		types = append(types, rt)
	}
	b.addIndexCommon(index)
	for _, rt := range types {
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
// to prevent subspace key reuse. Matches Java's RecordMetaDataBuilder.removeIndex(String),
// including its refusal of a name no index has (RecordMetaDataBuilder.java:1199-1203),
// which Go records in program order for Build to return, as every builder
// fault is.
func (b *RecordMetaDataBuilder) RemoveIndex(indexName string) *RecordMetaDataBuilder {
	idx, ok := b.indexes[indexName]
	if !ok {
		b.recordBuildError(&MetaDataError{
			Message: fmt.Sprintf("No index named %s defined", indexName),
		})
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
//
// A name no record type has is Java's MetaDataException "Unknown record type
// <name>" (RecordMetaDataBuilder.java:986-996), thrown at the call. Go records
// it in program order, as every builder fault is, for Build to return, and
// hands back a builder over a record type the meta-data does not hold, so a
// chained setter changes nothing.
func (b *RecordMetaDataBuilder) GetRecordType(name string) *RecordTypeBuilder {
	recordType := b.recordTypes[name]
	if recordType == nil {
		b.recordBuildError(&MetaDataError{Message: fmt.Sprintf("Unknown record type %s", name)})
		return &RecordTypeBuilder{recordType: &RecordType{Name: name}, builder: b}
	}
	return &RecordTypeBuilder{
		recordType: recordType,
		builder:    b,
	}
}

// Build creates the final RecordMetaData.
//
// Returns an error if any record type has no primary key set, and refuses any
// metadata whose index registry and record-type associations do not agree in
// both directions -- see the bijection check below.
//
// WHAT BUILD WRITES INTO THE BUILDER'S OWN OBJECTS -- first, because two
// successive revisions of this comment claimed it wrote nothing, and the
// revision that fixed THAT over-claimed in the other direction by listing three
// writes where there is one. Exactly one:
//
//   - `idx.primaryKeyComponentPositions`, on every SINGLE-TYPE index, because
//     `indexes[k] = v` shares the pointer. That is the point: Java sets
//     positions on the objects the caller registered and the scan call sites
//     read them off those same objects. (Multi-type and universal indexes are
//     deliberately excluded -- see the loop.)
//
// `rt.explicitRecordTypeKey` and `fi.SubspaceKey` are NOT writes, which is easy
// to misread because the assignments look like ones: both sites open with a
// struct copy (`rt := *v`, `f := *fi`) and then assign the COPY. The builder's
// RecordType and FormerIndex objects come out of Build unchanged. They belong
// to the copied set below.
//
// WHAT STAYS SHARED with the builder after Build returns: the `*Index` objects
// themselves, and therefore everything reachable through them -- `Options`
// (a map, with the exported in-place mutators SetUnique and SetClearWhenZero),
// `primaryKeyComponentPositions`, `Predicate`, `predicateProto`, `subspaceKey`,
// and the KeyExpression graphs behind `RootExpression`. Also shared, both
// descriptors: `fileDescriptor` and `unionDescriptor`; and the KeyExpression
// graphs behind each record type's `PrimaryKey` and behind `recordCountKey`.
//
// WHAT IS COPIED: every container Build owns directly -- the index registry
// map, the universal and former index slices, each record type's index slices,
// the RecordType and FormerIndex structs themselves (with their `[]byte` and
// subspace-key fields deep-copied), the `preserved` struct and all FIVE of its
// slices (`unknown` included), and `recordsSourceProto` (a proto.Clone; note
// this is the RETAINED verbatim descriptor, not the shared `fileDescriptor`
// named above).
//
// WHAT IS DERIVED HERE and shared with nothing: `fieldNumberToRecordType` and
// `ambiguousNames`, both computed from the metadata this Build assembled.
//
// Copying the containers keeps a later builder mutation from rewriting what
// Build returned; sharing the objects is what callers depend on when they hand
// a pre-Build *Index to ScanIndex, RebuildIndex or SetIndex, and what keeps
// OnlineIndexer's containment check an identity check rather than a name check.
// The objects therefore stay mutable through their exported fields and through
// those two setters, which is a divergence from Java's `private final` plus
// `ImmutableMap.copyOf`: DIVERGENCES.md has the analysis and the fix.
func (b *RecordMetaDataBuilder) Build() (*RecordMetaData, error) {
	// A setter records its fault instead of throwing (it has no error channel);
	// Java throws at the FIRST fault, so the first recorded one in program
	// order is the error, and nothing a later call recorded can mask or reorder
	// it. That includes a refused SetSubspaceKey, recorded on its Index at the
	// set, before or after AddIndex, of an index later removed or refused as a
	// duplicate too (firstFault).
	if err := b.firstFault(); err != nil {
		return nil, err
	}
	// A former index with the null key Java's FormerIndex constructor refuses
	// (FormerIndex.java:51-58): nothing in Go builds such a former index, but
	// FormerIndex.SubspaceKey is an exported field.
	for _, fi := range b.formerIndexes {
		if isNilSubspaceKey(fi.SubspaceKey) {
			return nil, &RecordCoreArgumentError{Message: "FormerIndex initialized with null subspace key", IndexName: fi.FormerName, SubspaceKey: fi.SubspaceKey, HasSubspaceKey: true}
		}
	}

	// The record types are walked by name wherever Build checks them, so
	// which of several faults is reported does not depend on map order. Java
	// walks a HashMap (RecordMetaDataBuilder.java:147, :1473), whose order is
	// its own; the name order is Go's fixed stand-in for it.
	recordTypeNames := slices.Sorted(maps.Keys(b.recordTypes))

	// A record type without a primary key: Java's build refuses it while it
	// builds the record types, before it validates anything
	// (RecordMetaDataBuilder.java:1480-1491).
	for _, name := range recordTypeNames {
		if b.recordTypes[name].PrimaryKey == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("Record type %s must have a primary key", name)}
		}
	}

	// Validate union descriptor oneof structure.
	// Matches Java's MetaDataValidator.validateUnionDescriptor()
	// (MetaDataValidator.java:60, :68-78), which runs first:
	//   - Must have at most 1 oneof
	//   - If a oneof exists, it must contain all fields
	if b.unionDescriptor != nil {
		oneofs := b.unionDescriptor.Oneofs()
		if oneofs.Len() > 1 {
			return nil, &MetaDataError{Message: "Union descriptor has more than one oneof"}
		}
		if oneofs.Len() == 1 {
			oneof := oneofs.Get(0)
			if oneof.Fields().Len() != b.unionDescriptor.Fields().Len() {
				return nil, &MetaDataError{Message: "Union descriptor oneof must contain every field"}
			}
		}
	}

	// Validate at least one record type is defined.
	// Matches Java's MetaDataValidator.validate() (MetaDataValidator.java:61-63).
	if len(b.recordTypes) == 0 {
		return nil, &MetaDataError{Message: "No record types defined in meta-data"}
	}

	// Java's validateRecordType, per record type, in its order
	// (MetaDataValidator.java:64, :80-101): the primary key validated against the
	// descriptor and refused if it can produce more than one entry, the record
	// type key unique, the since version not past the meta-data version. All
	// of it runs before any index is validated, as Java's validateRecordType
	// runs for every type before validateCurrentAndFormerIndexes.
	//
	// Two record types collide exactly when their keys occupy the same BYTES,
	// so the seen-set is keyed on the tuple encoding rather than on the value.
	// Keying on the value missed real collisions — int64(7) and uint(7) are
	// distinct values with identical encodings, and a record-type-prefixed
	// primary key built from them puts two types in one key space, where a
	// save silently overwrites the other type's record.
	typeKeySeen := make(map[string]string)
	for _, name := range recordTypeNames {
		rt := b.recordTypes[name]
		// Go-only: an empty primary key. Java's validator has no such check;
		// Go refuses it because such a record's split clear range is the whole
		// records subspace, every other record type's records included
		// (DIVERGENCES.md, "Build's record-type checks: the order, and one Go-only refusal").
		if rt.PrimaryKey.ColumnSize() == 0 {
			return nil, &MetaDataError{Message: fmt.Sprintf("record type %q has a primary key that produces no columns (EmptyKeyExpression is not a valid primary key)", name)}
		}
		if rt.Descriptor != nil {
			if err := validateKeyExpression(rt.PrimaryKey, rt.Descriptor); err != nil {
				// Java's KeyExpression.validate throws its exception unwrapped
				// (MetaDataValidator.java:97, :190).
				return nil, err
			}
		}
		if createsDuplicates(rt.PrimaryKey) {
			return nil, &MetaDataError{Message: fmt.Sprintf("Primary key for %s can generate more than one entry", name)}
		}
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
				"Same record type key %v used by both %s and %s", key, name, prevName)}
		}
		typeKeySeen[dedup] = name
		if rt.SinceVersion > b.version {
			return nil, &MetaDataError{Message: fmt.Sprintf(
				"Record type %s has since version of %d which is greater than the meta-data version %d",
				name, rt.SinceVersion, b.version)}
		}
	}

	// An index without a root is refused first. Go-only as a refusal: Java's
	// constructors take a @Nonnull root, and a null one fails its validation
	// with a NullPointerException, so no Java program builds such meta-data;
	// Go's struct literal can, and indexToProto would store an index with no
	// root, which Java's reader and Go's refuse ("Exactly one root must be
	// specified for an index").
	for _, name := range slices.Sorted(maps.Keys(b.indexes)) {
		if b.indexes[name].RootExpression == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("Index %s has no root expression", name)}
		}
	}

	// EVERY REGISTERED INDEX IS UNIVERSAL OR ASSOCIATED, checked here rather
	// than argued about at the call sites that could break it.
	//
	// An index in b.indexes that no record type claims and that is not
	// universal is an ORPHAN: RecordTypesForIndex returns nothing for it,
	// GetIndexesForRecordType loses it, the aggregate-index plan derives its
	// result columns from a nil descriptor, and ToProto emits it with an EMPTY
	// RecordType list -- which a reload reads as UNIVERSAL, so it returns
	// either maintained for every record type or as metadata that will not load
	// at all. RFC-238 §7f carries the analysis.
	//
	// THE REGISTRY AND THE ASSOCIATIONS MUST AGREE, and "agree" has more parts
	// than it looks. Each class below is a state that serializes wrong or is
	// maintained wrong, and each was reached through the exported API.
	//
	// UNIVERSAL AND ASSOCIATED ARE MUTUALLY EXCLUSIVE. One proto field carries
	// coverage: an empty recordType list means universal, a non-empty one names
	// types. An index that is both serializes as the non-empty form, so a
	// reload DROPS it from universalIndexes -- coverage narrows on the wire --
	// and in memory the store maintains it twice, which for COUNT/SUM is a
	// double atomic ADD and those are not idempotent. Java reaches neither
	// state: addUniversalIndex and addMultiTypeIndex write to disjoint places.
	//
	// A DUPLICATE ASSOCIATION IS ACCEPTED, because Java accepts it. Its
	// addMultiTypeIndex appends per name with no dedup, MetaDataValidator has no
	// duplicate check, and loadFromProto preserves a repeated record-type name,
	// so metadata Java WROTE would fail to open here if this refused it -- and
	// refusing metadata Java writes is the one line this port may not cross. The
	// resulting double maintenance (each copy maintained separately on write,
	// which for COUNT/SUM is a non-idempotent double atomic ADD) is therefore a
	// shared behaviour, booked rather than unilaterally diverged from. An
	// earlier revision of this check refused it.
	//
	// THE REGISTRY KEY IS THE INDEX'S OWN NAME. Index.Name is exported, so a
	// caller can rename the object it registered. buildIndexRecordTypeMap keys
	// by idx.Name while ToProto looks the list up by the MAP key, so for a
	// type-specific index the mismatch serializes an empty recordType list --
	// the universal encoding. For a universal index the empty list is already
	// correct; the harm there is that the registry key and the name disagree,
	// so GetIndex(oldKey) answers with an index calling itself something else
	// and the reload rekeys it. Different harms, different messages.
	//
	// AND EVERY ASSOCIATION IS THE REGISTERED OBJECT. Registered as a DIFFERENT
	// object means write and read paths hold two definitions of one index;
	// registered as NOTHING means it is maintained on write, never scanned,
	// never serialized, and never subspace-key-checked. Also different.
	universalObjs := make(map[*Index]struct{}, len(b.universalIndexes))
	for _, idx := range b.universalIndexes {
		universalObjs[idx] = struct{}{}
	}

	associatedObjs := make(map[*Index]struct{}, len(b.indexes))
	var alsoUniversal, unregistered, mismatched []string
	for _, rt := range b.recordTypes {
		claimed := make(map[*Index]struct{}, len(rt.indexes)+len(rt.multiTypeIndexes))
		for _, group := range [][]*Index{rt.indexes, rt.multiTypeIndexes} {
			for _, idx := range group {
				if _, dup := claimed[idx]; dup {
					// Accepted: Java accepts it. See above.
					continue
				}
				claimed[idx] = struct{}{}
				associatedObjs[idx] = struct{}{}
				if _, uni := universalObjs[idx]; uni {
					alsoUniversal = append(alsoUniversal, idx.Name)
					continue
				}
				switch reg, ok := b.indexes[idx.Name]; {
				case !ok:
					unregistered = append(unregistered, idx.Name)
				case reg != idx:
					mismatched = append(mismatched, idx.Name)
				}
			}
		}
	}

	var renamed, renamedUniversal, orphaned []string
	for name, idx := range b.indexes {
		if idx.Name != name {
			if _, uni := universalObjs[idx]; uni {
				renamedUniversal = append(renamedUniversal, name)
			} else {
				renamed = append(renamed, name)
			}
			continue
		}
		if _, ok := universalObjs[idx]; ok {
			continue
		}
		if _, ok := associatedObjs[idx]; ok {
			continue
		}
		orphaned = append(orphaned, name)
	}

	// Reported one at a time, first by sorted name, so the message does not
	// depend on map order.
	for _, bad := range []struct {
		names  []string
		format string
	}{
		{alsoUniversal, "index %q is universal AND associated with a record type; " +
			"the record-type list encodes one or the other, so this narrows to type-specific " +
			"on reload while being maintained twice in memory"},
		{renamed, "index registered under %q reports a different name; the association " +
			"would serialize as an empty record-type list, which reloads as a universal index"},
		{renamedUniversal, "universal index registered under %q reports a different name; " +
			"the registry key and the index name would disagree across a reload"},
		{unregistered, "index %q is associated with a record type but is not registered; " +
			"it is maintained on write, never scanned, and never serialized"},
		{mismatched, "index %q is associated as one object and registered as a different one; " +
			"the write path and the read path would use different definitions"},
		{orphaned, "index %q is registered but associated with no record type and is not universal"},
	} {
		if len(bad.names) == 0 {
			continue
		}
		sort.Strings(bad.names)
		return nil, &MetaDataError{Message: fmt.Sprintf(bad.format, bad.names[0])}
	}

	// Java's MetaDataValidator.validateCurrentAndFormerIndexes, after every
	// record type (MetaDataValidator.java:64-65, :103-116): each index's
	// validator, subspace key, versions and replacements, then each former
	// index, then an index and a former index sharing a key.
	if err := b.validateCurrentAndFormerIndexes(recordTypeNames); err != nil {
		return nil, err
	}

	// Compute primaryKeyComponentPositions ON THE BUILDER'S OBJECTS, and before
	// the containers are copied below.
	//
	// The order is load-bearing and only the cross-engine conformance suite
	// proved it. Java sets these positions on the very Index objects the caller
	// registered (RecordMetaDataBuilder.build calls
	// index.setPrimaryKeyComponentPositions), and callers rely on that: the
	// composite-index conformance store keeps the *Index it passed to AddIndex
	// and hands that same object to ScanIndex. A revision that copied the index
	// OBJECTS and computed positions afterwards left the caller's object with
	// nil positions while the metadata wrote entries with the primary key
	// DEDUPED, so the scan decoded `pk=[]` where Java produced `pk=[1]` -- a
	// wire-visible disagreement from a change that looked purely in-memory.
	// The object copy is gone, so the two are one pointer again and the hazard
	// is latent rather than live; the order still matters the moment anyone
	// reintroduces a copy, which is why it is stated rather than assumed.
	//
	// A COMMENT IS THE ONLY THING HOLDING THIS. Expressing it as call order
	// instead is what the TODO.md entry "RecordMetaDataBuilder.Build does six
	// jobs in one frame" is for; that entry names this site, and this names it
	// back, so neither half can rot alone.
	//
	// SINGLE-TYPE INDEXES ONLY. Java's loop is over
	// `recordTypeBuilder.getIndexes()` (RecordMetaDataBuilder.java:1465-1467),
	// and `getIndexes()` / `getMultiTypeIndexes()` are two distinct lists
	// (RecordTypeIndexesBuilder.java:43 and :45); universal indexes are in neither.
	// So Java NEVER assigns positions to a multi-type or universal index, and
	// its `Index.trimPrimaryKey` therefore returns those indexes' primary keys
	// untrimmed.
	//
	// Go used to assign them to all three, and that reached the wire in both
	// directions:
	//
	//   - Multi-type. Two record types keyed on the same field, with a
	//     multi-type index on that field, gave positions [0] and trimmed the
	//     primary key to NOTHING -- Go wrote `(price)` where Java writes
	//     `(price, pk)`. Different index entry keys for the same metadata.
	//   - Universal. The old code took "the first record type's primary key",
	//     by `break`ing out of a range over `b.recordTypes`, which is a MAP.
	//     With record types whose primary keys differ, the chosen type -- and
	//     so the entry key -- varied per Build within a single process: 40
	//     builds of one metadata produced positions [0] 33 times and nil 7
	//     times. That is worse than a Java divergence, because two Go stores
	//     built from identical metadata could disagree with each other.
	//
	// Both are pinned by TestPositionsAreAssignedOnlyToSingleTypeIndexes.
	//
	// THIS CHANGES EXISTING DATA. Positions are derived here and never
	// persisted, so entries an older build wrote for a multi-type or universal
	// index are trimmed while this one writes them whole. The DECODE of such an
	// entry does not error -- a full overlap yields an empty primary key, a
	// partial overlap a short and plausible wrong one -- and the old entries are
	// never cleared. Nothing detects any of that automatically; the affected
	// indexes need a rebuild, and the procedure is considerably more than a
	// lastModifiedVersion bump.
	//
	// WHAT THAT LOOKS LIKE AT THE SCAN IS NOT DESCRIBED HERE. An earlier version
	// of this comment finished the sentence with "so an unremediated store
	// returns duplicate rows", which is true of the index ENTRIES and false of
	// most rows, and it survived here after being refuted in DIVERGENCES.md
	// because that fold swept for a different claim in the same commit. Two
	// claims were refuted; one sweep was run. The scan-level symptoms live in
	// the operator-facing copy and nowhere else.
	//
	// That copy is DIVERGENCES.md, "UPGRADING BREAKS EXISTING DATA FOR THE
	// AFFECTED INDEXES, SILENTLY", which names this file back. The read
	// behaviour is pinned by these two, each on its own line so that grep finds
	// them -- wrapping a test name across a line break is exactly how a sweep
	// comes back empty, which happened to this very comment:
	//   TestPreUpgradeTrimmedEntryReadsBackWithAnEmptyPrimaryKey
	//   TestPreUpgradeTrimmedEntryWithAPartialOverlapReadsBackAShortWrongPrimaryKey
	for _, rt := range b.recordTypes {
		for _, idx := range rt.indexes {
			if idx.primaryKeyComponentPositions == nil {
				idx.primaryKeyComponentPositions = buildPrimaryKeyComponentPositions(idx.RootExpression, rt.PrimaryKey)
			}
		}
	}

	// WHICH CONTAINERS ARE COPIED, AND WHY IT IS NOT "THE ONES JAVA COPIES".
	//
	// Java shares every index container with its builder, so the obvious rule
	// is "share what Java shares". That rule is wrong here, and checking it at
	// the level of WHICH FIELDS are passed rather than WHAT KIND OF CONTAINER
	// is what made it look right: sharing is safe only when mutation is not
	// in-place-destructive. Java's universalIndexes is a Map<String,Index> and
	// removeIndex does a map removal, which both sides see cleanly. Go's is a
	// SLICE and removeIndexFromSlice compacts it in place (`indexes[:0]`), so
	// sharing the backing array means a post-Build RemoveIndex rewrites the
	// built metadata's slice under it -- leaving one universal index DUPLICATED
	// (store.go iterates GetUniversalIndexes on every save, so its maintainer
	// runs twice, and for COUNT/SUM a double atomic ADD is not idempotent) and
	// another ORPHANED, which is the very class the validation above refuses.
	//
	// So the containers are copied and the OBJECTS are shared. The pointer
	// identity is what the call sites depend on when they hand a pre-Build
	// *Index to ScanIndex, RebuildIndex or SetIndex, and it is what keeps
	// OnlineIndexer's containment check an identity check rather than a name
	// check. Copying the containers costs nothing and changes none of that.
	//
	// formerIndexes is copied for the same reason even though it is append-only
	// today: it is correct by luck, one in-place mutation away from identical
	// breakage, and the luck is not written down anywhere.
	//
	// The record-type slices are copied too, and that one IS a Go-only
	// divergence rather than alignment. Java shares BOTH halves: the RecordType
	// constructor assigns the builder's live lists (RecordType.java:70-71), and
	// the registry map itself is stored by reference (RecordMetaData.java:155),
	// so `indexes.remove(name)` takes the index out of the built metadata's
	// registry too and `toProto` -- which seeds from `indexes.entrySet()`
	// (RecordMetaData.java:664-665) -- never emits it. Java's post-build
	// removeIndex is therefore a COHERENT removal, not a lossy one.
	//
	// COPY BOTH OR SHARE BOTH; THE MIXTURE IS WHAT BREAKS. Copying the registry
	// while sharing the record-type slices is exactly the state that produces
	// an index registered under a name no record type claims, whose ToProto
	// emits an EMPTY record-type list that a reload reads as UNIVERSAL. This
	// branch built that state once and spent three commits on it. Go now copies
	// both, so it is coherent in the other direction: a snapshot at Build
	// rather than Java's live view. Neither is obviously better; what this is
	// NOT is a port, and it must not drift into a mixture. DIVERGENCES.md
	// ("Go snapshots a record type's index lists; Java shares everything") has
	// the full analysis.
	types := make(map[string]*RecordType, len(b.recordTypes))
	for k, v := range b.recordTypes {
		rt := *v
		rt.indexes = append([]*Index(nil), v.indexes...)
		rt.multiTypeIndexes = append([]*Index(nil), v.multiTypeIndexes...)
		// A []byte record-type key shares its backing array through the struct
		// copy, and that array is the type's on-disk PREFIX: mutating it after
		// the duplicate-key check has passed moves every record of that type,
		// or collides it with another. Java stores an immutable ByteString.
		// make+copy, never append([]byte(nil), …), which returns NIL for an
		// empty input -- nil is how this field spells "absent", so an
		// empty-bytes key would stop serializing and the type would fall back
		// to its union field number. TestRecordTypeKey_EmptyBytesSurvives
		// ProtoRoundTrip pins that and has already caught this exact idiom here.
		if raw, ok := v.explicitRecordTypeKey.([]byte); ok && raw != nil {
			dup := make([]byte, len(raw))
			copy(dup, raw)
			rt.explicitRecordTypeKey = dup
		}
		types[k] = &rt
	}
	indexes := make(map[string]*Index, len(b.indexes))
	for k, v := range b.indexes {
		indexes[k] = v
	}
	universalIndexes := append([]*Index(nil), b.universalIndexes...)
	formerIndexes := make([]*FormerIndex, len(b.formerIndexes))
	for i, fi := range b.formerIndexes {
		f := *fi
		// Same trap one field over: SubspaceKey is `any`, so `f := *fi` copies
		// the interface header and shares whatever it points at.
		f.SubspaceKey = deepCopySubspaceKey(fi.SubspaceKey)
		formerIndexes[i] = &f
	}
	// preserved is a struct of slices, so it is copied for the same reason
	// formerIndexes is: nothing mutates it in place TODAY, and that is luck
	// rather than a property. Both proto boundaries already clone, so this costs
	// nothing and removes the one remaining container Build shared silently.
	preserved := b.preserved
	preserved.joinedRecordTypes = append([]*gen.JoinedRecordType(nil), b.preserved.joinedRecordTypes...)
	preserved.unnestedRecordTypes = append([]*gen.UnnestedRecordType(nil), b.preserved.unnestedRecordTypes...)
	preserved.userDefinedFunctions = append([]*gen.PUserDefinedFunction(nil), b.preserved.userDefinedFunctions...)
	preserved.views = append([]*gen.PView(nil), b.preserved.views...)
	preserved.storedQueries = append([]*gen.PStoredQuery(nil), b.preserved.storedQueries...)
	if b.preserved.unknown != nil {
		preserved.unknown = make([]byte, len(b.preserved.unknown))
		copy(preserved.unknown, b.preserved.unknown)
	}

	var recordsSourceProto *descriptorpb.FileDescriptorProto
	if b.recordsSourceProto != nil {
		recordsSourceProto = proto.Clone(b.recordsSourceProto).(*descriptorpb.FileDescriptorProto)
	}

	// No record-type-key binding happens here, deliberately. A key expression
	// graph can be shared by more than one metadata build, so anything this
	// builder wrote onto a RecordTypeKeyExpression would be visible to the
	// OTHER metadata built from the same graph — last build wins, for both.
	// RecordTypeKeyExpression resolves the key from the record's own type at
	// evaluation time, exactly as Java's does, which needs no binding at all.

	// Pre-compute union field numbers and message factories for direct wire
	// format encoding/decoding (skips UnionDescriptor allocation entirely).
	fnToRT := make(map[protowire.Number]*RecordType, len(types))
	var roots []protoreflect.MessageDescriptor
	for _, rt := range types {
		if rt.Descriptor != nil {
			roots = append(roots, rt.Descriptor)
		}
	}
	reach := newMapReach(roots...)
	for _, rt := range types {
		if rt.Descriptor != nil {
			rt.reachesMap, rt.mapReach = reach.reaches(rt.Descriptor), reach
		}
		if rt.UnionFieldDescriptor != nil {
			rt.unionFieldNumber = rt.UnionFieldDescriptor.Number()
			msgType, err := protoregistry.GlobalTypes.FindMessageByName(rt.Descriptor.FullName())
			if err != nil || msgType.Descriptor() != rt.Descriptor {
				// Dynamic or revised schemas must decode against the current descriptor.
				// This allows runtime-constructed schemas (e.g. from DDL) to be used
				// for both serialization and deserialization.
				desc := rt.Descriptor
				rt.newMessage = func() proto.Message { return dynamicpb.NewMessage(desc) }
			} else {
				rt.newMessage = func() proto.Message { return msgType.New().Interface() }
			}
			for i := 0; i < b.unionDescriptor.Fields().Len(); i++ {
				field := b.unionDescriptor.Fields().Get(i)
				if field.Message() == rt.Descriptor {
					fnToRT[field.Number()] = rt
				}
			}
		}
	}

	md := &RecordMetaData{
		recordTypes:             types,
		fileDescriptor:          b.fileDescriptor,
		recordsSourceProto:      recordsSourceProto,
		version:                 b.version,
		recordCountKey:          b.recordCountKey,
		storeRecordVersions:     b.storeRecordVersions,
		splitLongRecords:        b.splitLongRecords,
		indexes:                 indexes,
		universalIndexes:        universalIndexes,
		formerIndexes:           formerIndexes,
		unionDescriptor:         b.unionDescriptor,
		fieldNumberToRecordType: fnToRT,
		subspaceKeyCounter:      b.subspaceKeyCounter,
		usesSubspaceKeyCounter:  b.counterBasedSubspaceKeys,
		preserved:               preserved,
	}

	// Derived here, beside fieldNumberToRecordType above, so the value is
	// unambiguously the declared set THIS Build saw. See the field comment.
	// Last, after every arm that can reject the metadata: a Build that returns
	// an error derives nothing, and a Build that succeeds derives from final
	// state rather than from state a later arm might still change.
	md.ambiguousNames, md.ambiguousFound = md.computeAmbiguousDeclaredNames()

	return md, nil
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
	rtb.builder.handedKeys = append(rtb.builder.handedKeys, keyExpr)
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
		rtb.builder.recordBuildError(fmt.Errorf("record type %q: %w", rtb.recordType.Name, err))
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
// rather than being sprinkled over the 25 call sites.
//
// The fallback fires only when the direct key misses, so it never SHADOWS a
// stored type and ToProtoBufCompliantName is deterministic. It is not,
// however, unambiguous: the escaping is not injective across the two
// namespaces, so a schema declaring both MY__1TABLE (the storage form of SQL
// MY$TABLE) and MY__01TABLE (the storage form of SQL MY__1TABLE) resolves a
// lookup for the SQL name MY__1TABLE to the FIRST of those — the wrong entry.
// No ordering fixes it; either order prices one of the pair wrong. See
// AmbiguousDeclaredNames, which is how a caller computing over all record
// types detects that case, and TestGetRecordTypeMisResolvesAnAmbiguousPair,
// which pins the behaviour so a reordering "fix" fails loudly.
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
//
// NOTE: this returns the LIVE map, not a copy, and the map IS mutated after
// Build.
//
// The census below is over changes to its KEY SET, because that is all the
// derived field depends on: computeAmbiguousDeclaredNames ranges the keys and
// probes declared[escaped], and never reads a *RecordType value. Mutating a
// value already in the map is irrelevant here.
//
// ENUMERATED per site rather than counted by regex, and by SHAPE rather than by
// one shape mistaken for all of them. Both mistakes were made here in
// succession: first a count that included the sentence stating it, then an
// assignment census presented as a mutation census -- which survived a round of
// review, because the two read alike and the missing shape was delete().
//
//	Insertions, subscript form -- 7:
//	  1 on the BUILDER, before any RecordMetaData exists, in this file:
//	    setRecords (the union-less fallback that held a second one
//	    is gone: Java has no union-less mode)
//	  6 post-Build, every one in a test:
//	    1 in record_type_key_identity_test.go
//	    5 in metadata_evolution_validator_test.go
//
//	Removals, delete() form -- 4, ALL post-Build, every one in a test:
//	    1 in record_type_key_identity_test.go
//	    3 in metadata_evolution_validator_test.go
//	  Two of those three are bare removals with no paired insertion (the
//	  "rejects removed type" cases); the third is half of a rename. A removal
//	  invalidates the derived field exactly as an insertion does.
//
//	clear(), the maps.* helpers, and whole-map assignment: none AFTER
//	  construction. online_indexer.go does assign a whole recordTypes, but that
//	  is a []string on a different struct -- it was once miscounted into this
//	  census.
//
//	CONSTRUCTION is a separate route with the same fail-open, and is not a
//	mutation of this map at all, which is why it sits outside the counts above:
//	metadata_evolution_validator_test.go and online_indexer_preset_test.go each
//	build &RecordMetaData{recordTypes: ...} directly, so Build never runs and
//	ambiguousFound stays false -- reported as "no collision" over a set nobody
//	derived.
//
// Twelve sites, ten of them after Build.
//
// This matters because Build DERIVES ambiguousNames from this map, so any
// post-Build key-set change leaves the derived field describing a declared set
// that no longer exists. Latent today, and measured so: none of the THREE files
// named above calls AmbiguousDeclaredNames, so nothing reads the stale value. A
// test that starts doing both re-arms it.
//
// Copying the map here would NOT close that. All ten post-Build sites touch the
// private field directly rather than through this accessor, so a copy prevents
// none of them while adding an O(types) allocation to every caller -- including
// computeAmbiguousDeclaredNames itself.
//
// The staleness is silent either way, but it is at least deterministic: the
// field always means "what Build saw", never "whatever the set was the first
// time somebody happened to ask", which is what the sync.Once it replaced meant.
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
//
// Resolves through GetRecordType rather than indexing the map, so a caller
// holding the SQL identifier for a type stored under its escaped protobuf name
// gets that type's indexes instead of nil. A raw lookup here returns "this type
// has no indexes", which is a silent wrong answer wherever the result feeds an
// index-selection decision.
func (m *RecordMetaData) GetIndexesForRecordType(name string) []*Index {
	rt := m.GetRecordType(name)
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
//
// The types come in name order, so a caller reporting the first of them that
// violates something names the same one on every run.
func (m *RecordMetaData) RecordTypesForIndex(idx *Index) []*RecordType {
	names := slices.Sorted(maps.Keys(m.recordTypes))
	// Check if it's a universal index.
	for _, ui := range m.universalIndexes {
		if ui.Name == idx.Name {
			result := make([]*RecordType, 0, len(m.recordTypes))
			for _, name := range names {
				result = append(result, m.recordTypes[name])
			}
			return result
		}
	}
	// Type-specific: find which types have this index.
	var result []*RecordType
	for _, name := range names {
		rt := m.recordTypes[name]
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
// Java's RecordMetaData.getIndexFromSubspaceKey (RecordMetaData.java:329-336)
// compares each index's normalized key with the ARGUMENT AS GIVEN by equals. Go
// normalizes the argument too (subspaceKeyIdentity), so two arguments differ
// from Java's: an int argument finds an index whose key is int64, where Java's
// Integer argument misses a Long key, and a tuple argument finds an index whose
// key is the same list, where Java's Tuple argument never equals a List key.
// The second is reached by the one non-test caller, savedSourceIndex, which
// passes a decoded stamp item as Java's OnlineIndexer passes
// Index.decodeSubspaceKey's (OnlineIndexer.java:205-208): for a source index
// keyed by a nested tuple Java fails the BY_INDEX resume with an unknown key and
// Go resumes from the index the stamp names (DIVERGENCES.md, "BY_INDEX resume
// over a nested-tuple source key"). Java throws MetaDataException on a miss; Go
// returns nil.
func (m *RecordMetaData) GetIndexFromSubspaceKey(key any) *Index {
	want := subspaceKeyIdentity(key)
	for _, idx := range m.indexes {
		if subspaceKeyIdentity(idx.SubspaceTupleKey()) == want {
			return idx
		}
	}
	return nil
}

// GetIndexesSince returns all indexes modified since the given metadata version,
// including replaced originals whose state still needs reconciliation.
// Matches Java's RecordMetaData.getIndexesSince(int).
func (m *RecordMetaData) GetIndexesSince(version int) []*Index {
	var result []*Index
	for _, idx := range m.indexes {
		if idx.LastModifiedVersion > version {
			result = append(result, idx)
		}
	}
	return result
}

// GetIndexesToBuildSince excludes replaced originals from changed indexes.
// Matches Java's RecordMetaData.getIndexesToBuildSince(int).
func (m *RecordMetaData) GetIndexesToBuildSince(version int) []*Index {
	var result []*Index
	for _, index := range m.GetIndexesSince(version) {
		if len(index.GetReplacedByIndexNames()) == 0 {
			result = append(result, index)
		}
	}
	return result
}

// GetUnionDescriptor returns the records file's union message (never nil on a
// built RecordMetaData: a records file without one is refused).
// Matches Java's RecordMetaData.getUnionDescriptor().
func (m *RecordMetaData) GetUnionDescriptor() protoreflect.MessageDescriptor {
	return m.unionDescriptor
}

// GetUnionFieldForRecordType returns the union field descriptor for a record type.
// Every record type of a built RecordMetaData is a union field (validateRecordUnion
// refuses a RECORD-usage message that is not), so this is nil only for a record
// type the metadata does not hold.
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

// formerIndexNameSuffix is Java's rendering of a former index's name after
// "Former index" and "former index": a space and the name, or nothing for an
// unnamed one (MetaDataValidator.java:110-112, :163-178).
func formerIndexNameSuffix(fi *FormerIndex) string {
	if fi.FormerName == "" {
		return ""
	}
	return " " + fi.FormerName
}

// formerIndexNameOrUnknown is how MetaDataValidator names a former index that
// may have no name.
func formerIndexNameOrUnknown(fi *FormerIndex) string {
	if fi.FormerName == "" {
		return "<unknown>"
	}
	return fi.FormerName
}

// deepCopySubspaceKey returns a subspace key that shares no mutable state with
// its argument.
//
// THE SHAPES ARE TAKEN FROM THE DECODER, NOT FROM subspaceKeyIdentity. That
// function normalizes for comparison and its `default` arm keeps the key as
// given, so it bounds nothing -- reading a normalizer as the authority on
// "which shapes are reachable" is how the []byte-only version of this copy came
// to describe itself as complete. The producers are `formerIndexFromProto`
// (`fi.SubspaceKey = t[0]` off a decoded tuple) and `Index.SubspaceTupleKey`,
// so the reachable set is what `fastDecodeTuple` can return for one element,
// which is TWELVE shapes: nil, []byte, string, int64, uint64, *big.Int,
// float32, float64, bool, tuple.UUID, tuple.Versionstamp, and a nested
// tuple.Tuple. (`uint64` is easy to miss and was: `fastDecodeInt` returns it
// for a positive value that overflows int64. It is immutable, so the code below
// is unaffected -- but an enumeration is offered because it is checkable, and
// the conclusion surviving does not make the list true.)
//
// Of those, exactly three carry mutable state: []byte, *big.Int (which has
// in-place mutators -- Set, SetBytes, Add), and tuple.Tuple, which is []any and
// so needs recursion rather than a one-level copy. UUID is [16]byte and
// Versionstamp is [10]byte plus a uint16 -- both pure values that copy with the
// struct.
//
// NOT COVERED: `Index.SetSubspaceKey` takes `any` and will accept a type no
// decoder produces. A caller-supplied pointer type therefore stays shared, and
// nothing here can fix that without reflection; it is bounded instead by the
// key having to survive tuple encoding to reach FDB at all.
func deepCopySubspaceKey(key any) any {
	switch k := key.(type) {
	case []byte:
		if k == nil {
			return key
		}
		dup := make([]byte, len(k))
		copy(dup, k)
		return dup
	case *big.Int:
		if k == nil {
			return key
		}
		return new(big.Int).Set(k)
	case tuple.Tuple:
		if k == nil {
			return key
		}
		dup := make(tuple.Tuple, len(k))
		for i, elem := range k {
			dup[i] = deepCopySubspaceKey(elem)
		}
		return dup
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
	case *FunctionKeyExpression:
		return countVersionColumns(e.arguments)
	default:
		return 0
	}
}

// AmbiguousDeclaredNames reports a pair of declared record types whose names
// collide across the SQL and storage namespaces, in USER identifiers, or
// ok=false when none collide.
//
// Descriptor names are ESCAPED (protoname.ToProtoBufCompliantName) and the
// escaping is NOT injective across the two namespaces: MY$TABLE is stored as
// MY__1TABLE, while a table whose SQL name IS MY__1TABLE is stored as
// MY__01TABLE. Any lookup that tries a name as given and then its escaped form
// resolves the first of those to the wrong entry, and no ordering fixes it —
// either order prices one of the two wrong.
//
// It lives here, beside DeclaresSyntheticRecordTypes, because it is the same
// KIND of fact: a property of the declarations that decides whether a caller
// computing over "all record types" can trust the answer. Both the statistics
// reader and both collection entry points need it, and a shared property with
// three consumers does not belong in any one of them.
//
// CASE-SENSITIVE, and every gate that cites this function inherits that. The
// collision test is a map lookup on the escaped name, so a schema declaring
// quoted "MY$TABLE" (stored MY__1TABLE) alongside quoted "my__1table" (stored
// my__01table) is NOT reported: the spellings differ only in case. That gap
// does not reach a destructive path, because GetRecordType is case-sensitive
// too and never resolves one to the other -- but a renderer that lowercases
// before comparing would re-open it. Tracked in TODO.md.
//
// Returns USER identifiers because the operator has to act on the SQL names;
// the collision itself is detected in storage space, where it lives.
func (m *RecordMetaData) AmbiguousDeclaredNames() ([]string, bool) {
	if m == nil {
		return nil, false
	}
	return m.ambiguousNames, m.ambiguousFound
}

// computeAmbiguousDeclaredNames is the derivation. Build calls it once and
// stores the result; read AmbiguousDeclaredNames rather than calling this.
func (m *RecordMetaData) computeAmbiguousDeclaredNames() ([]string, bool) {
	if m == nil {
		return nil, false
	}
	declared := m.RecordTypes()
	var worst []string
	for name := range declared {
		escaped, err := protoname.ToProtoBufCompliantName(name)
		if err != nil || escaped == name {
			continue
		}
		if _, collides := declared[escaped]; !collides {
			continue
		}
		pair := []string{
			protoname.ToUserIdentifier(name),
			protoname.ToUserIdentifier(escaped),
		}
		// Deterministic across map iteration order: an operator comparing two
		// runs must not see the pair change.
		if worst == nil || pair[0] < worst[0] {
			worst = pair
		}
	}
	return worst, worst != nil
}

// buildFaultSeq orders every recorded builder fault and SetSubspaceKey
// refusal across builders and indexes: Java throws each at its call, so the
// one recorded first is the one a Java program would have died on.
var buildFaultSeq atomic.Uint64

func nextBuildFaultSeq() uint64 { return buildFaultSeq.Add(1) }

// recordBuildError records a fault a builder call found, for Build to return
// if nothing recorded earlier did.
func (b *RecordMetaDataBuilder) recordBuildError(err error) {
	b.buildErrors = append(b.buildErrors, err)
	b.buildErrorSeqs = append(b.buildErrorSeqs, nextBuildFaultSeq())
}

// firstFault is the fault recorded first in program order among the
// builder's calls, the SetSubspaceKey refusals of every index ever handed to
// AddIndex, and the constructor refusals in every key ever handed to it, or
// nil.
func (b *RecordMetaDataBuilder) firstFault() error {
	var first error
	var firstSeq uint64
	consider := func(err error, seq uint64) {
		if err != nil && (first == nil || seq < firstSeq) {
			first, firstSeq = err, seq
		}
	}
	for i, err := range b.buildErrors {
		consider(err, b.buildErrorSeqs[i])
	}
	considerKey := func(expr KeyExpression) {
		seq, err := keyConstructionFault(expr)
		consider(err, seq)
	}
	for _, idx := range b.addedIndexes {
		consider(idx.subspaceKeyErr, idx.subspaceKeyErrSeq)
		considerKey(idx.RootExpression)
	}
	for _, key := range b.handedKeys {
		considerKey(key)
	}
	// A key assigned without a setter (the loader, the records file's
	// primary_key option) is walked where it sits.
	for _, rt := range b.recordTypes {
		considerKey(rt.PrimaryKey)
	}
	considerKey(b.recordCountKey)
	return first
}

// errThenArity is Java's refusal of a Then of fewer than two children
// (ThenKeyExpression.java:63-65), a RecordCoreException thrown where the Then
// is built.
func errThenArity() error {
	return &RecordCoreError{Message: "Then must have at least 2 children"}
}

// keyConstructionFault is the earliest refusal in program order that Java's
// constructors would have thrown building expr, with its place, or (0, nil):
// a Then of fewer than two children (Concat) or a function create refuses
// (FunctionExpr, CardinalityExpr).
func keyConstructionFault(expr KeyExpression) (uint64, error) {
	var firstSeq uint64
	var first error
	consider := func(seq uint64, err error) {
		if err != nil && (first == nil || seq < firstSeq) {
			firstSeq, first = seq, err
		}
	}
	switch e := expr.(type) {
	case *CompositeKeyExpression:
		if e.arityFaultSeq != 0 {
			consider(e.arityFaultSeq, errThenArity())
		}
		for _, child := range e.expressions {
			consider(keyConstructionFault(child))
		}
	case *NestingKeyExpression:
		consider(keyConstructionFault(e.child))
	case *GroupingKeyExpression:
		consider(keyConstructionFault(e.wholeKey))
	case *KeyWithValueExpression:
		consider(keyConstructionFault(e.innerKey))
	case *FunctionKeyExpression:
		consider(e.faultSeq, e.fault)
		consider(keyConstructionFault(e.arguments))
	case *CardinalityFunctionKeyExpression:
		consider(e.faultSeq, e.fault)
		consider(keyConstructionFault(e.arguments))
	case *DimensionsKeyExpression:
		consider(keyConstructionFault(e.WholeKey))
	case *SplitKeyExpression:
		consider(keyConstructionFault(e.joined))
	case *ListKeyExpression:
		for _, child := range e.children {
			consider(keyConstructionFault(child))
		}
	}
	return firstSeq, first
}
