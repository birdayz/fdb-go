// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// RecordMetaData.java, RecordType.java, KeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"maps"
	"math/big"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Build creates the final RecordMetaData.
//
// Returns an error if any record type has no primary key set, and refuses any
// metadata whose index registry and record-type associations do not agree in
// both directions -- see the bijection check below.
//
// WHAT BUILD WRITES INTO THE BUILDER'S OWN OBJECTS. Exactly one:
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
	if err := b.validateFormerIndexKeys(); err != nil {
		return nil, err
	}

	// The record types are walked by name wherever Build checks them, so
	// which of several faults is reported does not depend on map order. Java
	// walks a HashMap (RecordMetaDataBuilder.java:147, :1473), whose order is
	// its own; the name order is Go's fixed stand-in for it.
	recordTypeNames := slices.Sorted(maps.Keys(b.recordTypes))
	if err := b.validateRecordTypePrimaryKeys(recordTypeNames); err != nil {
		return nil, err
	}
	if err := b.validateUnionDescriptor(); err != nil {
		return nil, err
	}
	// Validate at least one record type is defined.
	// Matches Java's MetaDataValidator.validate() (MetaDataValidator.java:61-63).
	if len(b.recordTypes) == 0 {
		return nil, &MetaDataError{Message: "No record types defined in meta-data"}
	}
	if err := b.validateRecordTypes(recordTypeNames); err != nil {
		return nil, err
	}
	if err := b.validateIndexRoots(); err != nil {
		return nil, err
	}
	if err := b.validateIndexAssociations(); err != nil {
		return nil, err
	}
	// Java's MetaDataValidator.validateCurrentAndFormerIndexes, after every
	// record type (MetaDataValidator.java:64-65, :103-116): each index's
	// validator, subspace key, versions and replacements, then each former
	// index, then an index and a former index sharing a key.
	if err := b.validateCurrentAndFormerIndexes(recordTypeNames); err != nil {
		return nil, err
	}

	// Must run on the builder's objects before the container snapshots below;
	// see assignPrimaryKeyComponentPositions.
	b.assignPrimaryKeyComponentPositions()

	types := b.snapshotRecordTypes()
	indexes := b.snapshotIndexes()
	universalIndexes := append([]*Index(nil), b.universalIndexes...)
	formerIndexes := b.snapshotFormerIndexes()
	preserved := b.snapshotPreservedFields()
	recordsSourceProto, dependencySourceProtos := b.snapshotSourceProtos()

	// No record-type-key binding happens here, deliberately. A key expression
	// graph can be shared by more than one metadata build, so anything this
	// builder wrote onto a RecordTypeKeyExpression would be visible to the
	// OTHER metadata built from the same graph — last build wins, for both.
	// RecordTypeKeyExpression resolves the key from the record's own type at
	// evaluation time, exactly as Java's does, which needs no binding at all.
	fnToRT := b.populateRecordTypeRuntime(types)

	md := &RecordMetaData{
		recordTypes:                types,
		fileDescriptor:             b.fileDescriptor,
		recordsSourceProto:         recordsSourceProto,
		dependencySourceProtos:     dependencySourceProtos,
		version:                    b.version,
		recordCountKey:             b.recordCountKey,
		storeRecordVersions:        b.storeRecordVersions,
		splitLongRecords:           b.splitLongRecords,
		indexes:                    indexes,
		universalIndexes:           universalIndexes,
		formerIndexes:              formerIndexes,
		unionDescriptor:            b.unionDescriptor,
		fieldNumberToRecordType:    fnToRT,
		subspaceKeyCounter:         b.subspaceKeyCounter,
		usesSubspaceKeyCounter:     b.counterBasedSubspaceKeys,
		preserved:                  preserved,
		usesLocalRecordsDescriptor: b.usesLocalRecordsDescriptor,
	}

	// Derived here, beside fieldNumberToRecordType above, so the value is
	// unambiguously the declared set THIS Build saw. See the field comment.
	// Last, after every arm that can reject the metadata: a Build that returns
	// an error derives nothing, and a Build that succeeds derives from final
	// state rather than from state a later arm might still change.
	md.ambiguousNames, md.ambiguousFound = md.computeAmbiguousDeclaredNames()

	return md, nil
}

// assignPrimaryKeyComponentPositions computes primaryKeyComponentPositions ON
// THE BUILDER'S OBJECTS, and before the containers are copied by snapshot*.
//
// Java sets these positions on the very Index objects the caller registered
// (RecordMetaDataBuilder.build calls index.setPrimaryKeyComponentPositions),
// and callers rely on that: the composite-index conformance store keeps the
// *Index it passed to AddIndex and hands that same object to ScanIndex.
// Positions computed on a copy would leave the caller's object with nil
// positions while entries are written with the primary key deduped, so the
// scan decodes `pk=[]` where Java produces `pk=[1]`. Build expresses this
// ordering by calling assignPrimaryKeyComponentPositions before snapshot*.
//
// SINGLE-TYPE INDEXES ONLY. Java's loop is over
// `recordTypeBuilder.getIndexes()` (RecordMetaDataBuilder.java:1465-1467),
// and `getIndexes()` / `getMultiTypeIndexes()` are two distinct lists
// (RecordTypeIndexesBuilder.java:43 and :45); universal indexes are in neither.
// So Java NEVER assigns positions to a multi-type or universal index, and
// its `Index.trimPrimaryKey` therefore returns those indexes' primary keys
// untrimmed. Trimming a multi-type index on a field its types are keyed on
// would write `(price)` where Java writes `(price, pk)`, and for a universal
// index there is no single record type whose primary key could be chosen
// deterministically. Pinned by
// TestPositionsAreAssignedOnlyToSingleTypeIndexes.
//
// Positions are derived here and never persisted, so entries an older build
// wrote trimmed for a multi-type or universal index are neither detected nor
// cleared: they decode to an empty (full overlap) or short wrong (partial
// overlap) primary key, and the affected indexes need a rebuild. Scan-level
// symptoms are in DIVERGENCES.md, "UPGRADING BREAKS EXISTING DATA FOR THE
// AFFECTED INDEXES, SILENTLY". The read behaviour is pinned by:
//
//	TestPreUpgradeTrimmedEntryReadsBackWithAnEmptyPrimaryKey
//	TestPreUpgradeTrimmedEntryWithAPartialOverlapReadsBackAShortWrongPrimaryKey
func (b *RecordMetaDataBuilder) assignPrimaryKeyComponentPositions() {
	for _, rt := range b.recordTypes {
		for _, idx := range rt.indexes {
			if idx.primaryKeyComponentPositions == nil {
				idx.primaryKeyComponentPositions = buildPrimaryKeyComponentPositions(idx.RootExpression, rt.PrimaryKey)
			}
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
// emits an EMPTY record-type list that a reload reads as UNIVERSAL. Go
// copies both, so it is coherent in the other direction: a snapshot at Build
// rather than Java's live view. Neither is obviously better; what this is
// NOT is a port, and it must not drift into a mixture. DIVERGENCES.md
// ("Go snapshots a record type's index lists; Java shares everything") has
// the full analysis.

// snapshotRecordTypes copies record type structs, index slices, and []byte keys
// so post-Build builder mutations cannot alter the built metadata.
func (b *RecordMetaDataBuilder) snapshotRecordTypes() map[string]*RecordType {
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
		// to its union field number. Pinned by
		// TestRecordTypeKey_EmptyBytesSurvivesProtoRoundTrip.
		if raw, ok := v.explicitRecordTypeKey.([]byte); ok && raw != nil {
			dup := make([]byte, len(raw))
			copy(dup, raw)
			rt.explicitRecordTypeKey = dup
		}
		types[k] = &rt
	}
	return types
}

// snapshotIndexes copies the index registry map while sharing *Index pointers.
func (b *RecordMetaDataBuilder) snapshotIndexes() map[string]*Index {
	indexes := make(map[string]*Index, len(b.indexes))
	for k, v := range b.indexes {
		indexes[k] = v
	}
	return indexes
}

// snapshotFormerIndexes copies former index structs and deep-copies their subspace keys.
func (b *RecordMetaDataBuilder) snapshotFormerIndexes() []*FormerIndex {
	formerIndexes := make([]*FormerIndex, len(b.formerIndexes))
	for i, fi := range b.formerIndexes {
		f := *fi
		// Same trap one field over: SubspaceKey is `any`, so `f := *fi` copies
		// the interface header and shares whatever it points at.
		f.SubspaceKey = deepCopySubspaceKey(fi.SubspaceKey)
		formerIndexes[i] = &f
	}
	return formerIndexes
}

// snapshotPreservedFields copies unmodelled proto slices and unknown bytes.
// preserved is a struct of slices, so it is copied for the same reason
// formerIndexes is: nothing mutates it in place TODAY, and that is luck
// rather than a property. Both proto boundaries already clone, so this costs
// nothing and removes the one remaining container Build shared silently.
func (b *RecordMetaDataBuilder) snapshotPreservedFields() preservedMetaDataFields {
	preserved := b.preserved
	preserved.joinedRecordTypes = append([]*metadatapb.JoinedRecordType(nil), b.preserved.joinedRecordTypes...)
	preserved.unnestedRecordTypes = append([]*metadatapb.UnnestedRecordType(nil), b.preserved.unnestedRecordTypes...)
	preserved.userDefinedFunctions = append([]*metadatapb.PUserDefinedFunction(nil), b.preserved.userDefinedFunctions...)
	preserved.views = append([]*metadatapb.PView(nil), b.preserved.views...)
	preserved.storedQueries = append([]*metadatapb.PStoredQuery(nil), b.preserved.storedQueries...)
	if b.preserved.unknown != nil {
		preserved.unknown = make([]byte, len(b.preserved.unknown))
		copy(preserved.unknown, b.preserved.unknown)
	}
	return preserved
}

// snapshotSourceProtos clones retained FileDescriptorProtos for serialization.
func (b *RecordMetaDataBuilder) snapshotSourceProtos() (*descriptorpb.FileDescriptorProto, map[string]*descriptorpb.FileDescriptorProto) {
	var recordsSourceProto *descriptorpb.FileDescriptorProto
	if b.recordsSourceProto != nil {
		recordsSourceProto = proto.Clone(b.recordsSourceProto).(*descriptorpb.FileDescriptorProto)
	}
	var dependencySourceProtos map[string]*descriptorpb.FileDescriptorProto
	if len(b.dependencySourceProtos) > 0 {
		dependencySourceProtos = make(map[string]*descriptorpb.FileDescriptorProto, len(b.dependencySourceProtos))
		for name, file := range b.dependencySourceProtos {
			dependencySourceProtos[name] = proto.Clone(file).(*descriptorpb.FileDescriptorProto)
		}
	}
	return recordsSourceProto, dependencySourceProtos
}

// populateRecordTypeRuntime pre-computes union field numbers, reachability plans,
// and message factories for direct wire format encoding/decoding (skips
// UnionDescriptor allocation entirely).
func (b *RecordMetaDataBuilder) populateRecordTypeRuntime(types map[string]*RecordType) map[protowire.Number]*RecordType {
	fnToRT := make(map[protowire.Number]*RecordType, len(types))
	var roots []protoreflect.MessageDescriptor
	for _, rt := range types {
		if rt.Descriptor != nil {
			roots = append(roots, rt.Descriptor)
		}
	}
	reach := newMapReach(roots...)
	stringTypes := newStringReach(roots...)
	enumReach := newClosedEnumReach(false, roots...)
	for _, rt := range types {
		if rt.Descriptor != nil {
			rt.reachesMap, rt.mapReach = reach.Reaches(rt.Descriptor), reach
			rt.reachesString, rt.stringReach = stringTypes.Reaches(rt.Descriptor), stringTypes
			rt.reachesClosedEnum, rt.closedEnumReach = enumReach.reaches(rt.Descriptor), enumReach
			rt.closedEnumRoot, _, _ = enumReach.planFor(rt.Descriptor)
		}
		if rt.UnionFieldDescriptor != nil {
			rt.unionFieldNumber = rt.UnionFieldDescriptor.Number()
			msgType := GeneratedMessageType(rt.Descriptor)
			if msgType == nil {
				// Dynamic or revised schemas must decode against the current descriptor.
				// This allows runtime-constructed schemas (e.g. from DDL) to be used
				// for both serialization and deserialization.
				desc := rt.Descriptor
				rt.newMessage = func() proto.Message { return dynamicpb.NewMessage(desc) }
			} else {
				rt.newMessage = func() proto.Message { return msgType.New().Interface() }
				_, hasVT := msgType.New().Interface().(interface{ UnmarshalVT([]byte) error })
				rt.decodeVT = hasVT && vtBounded(rt.Descriptor, javaRecordRule.limit)
			}
			for i := 0; i < b.unionDescriptor.Fields().Len(); i++ {
				field := b.unionDescriptor.Fields().Get(i)
				if field.Message() == rt.Descriptor {
					fnToRT[field.Number()] = rt
				}
			}
		}
	}
	return fnToRT
}

// GeneratedMessageType is the compiled Go type whose descriptor is desc, nil
// when there is none: a registered type that only shares desc's full name is
// another message. Build decodes a record type as it when there is one, and
// as dynamicpb over desc otherwise.
func GeneratedMessageType(desc protoreflect.MessageDescriptor) protoreflect.MessageType {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(desc.FullName())
	if err != nil || mt.Descriptor() != desc {
		return nil
	}
	return mt
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
