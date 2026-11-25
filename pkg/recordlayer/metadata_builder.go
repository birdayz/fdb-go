// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// RecordMetaData.java, RecordType.java, KeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// RecordMetaDataBuilder provides a builder pattern for creating RecordMetaData
// This matches the Java RecordMetaDataBuilder pattern
type RecordMetaDataBuilder struct {
	recordTypes              map[string]*RecordType
	fileDescriptor           protoreflect.FileDescriptor
	recordsSourceProto       *descriptorpb.FileDescriptorProto
	dependencySourceProtos   map[string]*descriptorpb.FileDescriptorProto
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

	usesLocalRecordsDescriptor bool
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

// SetDependenciesSourceProtos retains original imported descriptors for serialization.
// Only transitive dependencies of the records descriptor are emitted.
func (b *RecordMetaDataBuilder) SetDependenciesSourceProtos(files []*descriptorpb.FileDescriptorProto) *RecordMetaDataBuilder {
	b.dependencySourceProtos = make(map[string]*descriptorpb.FileDescriptorProto, len(files))
	for _, file := range files {
		if file != nil {
			b.dependencySourceProtos[file.GetName()] = file
		}
	}
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

// AddUserDefinedFunction appends a SQL function.
func (b *RecordMetaDataBuilder) AddUserDefinedFunction(fn *metadatapb.PUserDefinedFunction) *RecordMetaDataBuilder {
	b.preserved.userDefinedFunctions = append(b.preserved.userDefinedFunctions, fn)
	return b
}

// AddStoredQuery appends a stored query (MetaData field 16).
func (b *RecordMetaDataBuilder) AddStoredQuery(q *metadatapb.PStoredQuery) *RecordMetaDataBuilder {
	b.preserved.storedQueries = append(b.preserved.storedQueries, q)
	return b
}

// AddView appends a SQL view (name and query text), as Java's RecordLayerView.asRawView.
func (b *RecordMetaDataBuilder) AddView(name, definition string) *RecordMetaDataBuilder {
	b.preserved.views = append(b.preserved.views, &metadatapb.PView{Name: &name, Definition: &definition})
	return b
}

// SetStoreRecordVersions controls automatic version assignment under DEFAULT.
// Per-save behaviors and supplied versions override this metadata setting.
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
