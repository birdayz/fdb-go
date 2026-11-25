// Portions derived from FoundationDB Record Layer (Index.java,
// RecordMetaDataBuilder.java, LiteralKeyExpression.java, FormerIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// RecordMetaDataFromProto deserializes a MetaData proto back into a RecordMetaData.
// Matches Java's RecordMetaDataBuilder.loadFromProto(), with opts the builder
// settings Java applies first (WithLocalFileDescriptor).
func RecordMetaDataFromProto(md *metadatapb.MetaData, opts ...MetaDataProtoOption) (*RecordMetaData, error) {
	var options metaDataProtoOptions
	for _, opt := range opts {
		opt(&options)
	}
	if md == nil {
		return nil, &MetaDataError{Message: "nil metadata proto"}
	}
	// Java's reading of the bytes: a closed enum's undeclared number is an
	// unknown field (proto_closed_enums.go), so a fan type Java cannot read is a
	// missing one here too. Stored bytes were decoded with that reading
	// already (UnmarshalAsJava); a proto built in memory that holds such a
	// number is read from a clone, so the caller's proto is not changed.
	if x := generatedReach(md.ProtoReflect().Descriptor()); holdsUndeclared(md.ProtoReflect(), x) {
		md = proto.Clone(md).(*metadatapb.MetaData)
		closedEnumsAsJava(md.ProtoReflect(), x, true)
	}

	// Retain the stored records proto VERBATIM: a re-save must emit the same
	// bytes Java (or a previous Go) wrote. It is cloned so that nothing the
	// caller does later can disturb it; the rebuild below is given its own
	// clones for the separate reason that it mutates what it is handed.
	var recordsSource *descriptorpb.FileDescriptorProto
	if md.Records != nil {
		recordsSource = proto.Clone(md.Records).(*descriptorpb.FileDescriptorProto)
	}

	// 1. Rebuild file descriptor from proto
	// THE REBUILD MUTATES WHAT IT IS GIVEN, so it is handed CLONES.
	//
	// rebuildFileDescriptor opens with absolutizeFieldTypeNames over the records
	// proto and every dependency, rewriting f.TypeName and ext.TypeName IN PLACE.
	// SaveRecordMetaData marshals the same proto it passes here, so mutating the
	// caller's descriptor would persist absolutized names instead of the relative
	// names the DDL builder emits.
	rebuildRecords := md.Records
	if md.Records != nil {
		rebuildRecords = proto.Clone(md.Records).(*descriptorpb.FileDescriptorProto)
	}
	rebuildDeps := make([]*descriptorpb.FileDescriptorProto, len(md.Dependencies))
	for i, dp := range md.Dependencies {
		rebuildDeps[i] = proto.Clone(dp).(*descriptorpb.FileDescriptorProto)
	}
	fd, err := rebuildFileDescriptor(rebuildRecords, rebuildDeps)
	if err != nil {
		return nil, fmt.Errorf("rebuild file descriptor: %w", err)
	}

	// 2. Java's loadFromProto order (RecordMetaDataBuilder.java:272-278): the
	// subspace-key settings, then the records and their union, then everything
	// else. Each step's first fault is the one Java throws, so a records fault is
	// returned before any index is read: an index naming an unknown record type
	// must not hide it. The settings also precede every index for a behavioural
	// reason: addIndexCommon consults the scheme to decide whether an index gets a
	// counter-assigned key, so loading them afterwards would apply them to nothing.
	builder := NewRecordMetaDataBuilder()
	if err := builder.loadSubspaceKeySettingsFromProto(md); err != nil {
		return nil, err
	}
	// The union is found as Java finds it (fetchUnionDescriptor: the usage=UNION
	// message, else RecordTypeUnion), so files like catalog_data.proto (whose
	// union is "CatalogUnion") round-trip.
	builder.setRecords(fd, "", false)
	if len(builder.buildErrors) > 0 {
		return nil, builder.buildErrors[0]
	}
	builder.SetRecordsSourceProto(recordsSource)
	builder.SetDependenciesSourceProtos(md.Dependencies)

	// 2b. The unmodelled fields, carried verbatim for re-emission.
	builder.preserved = preservedMetaDataFieldsFromProto(md)

	// 3. Indexes
	if err := loadIndexesFromProto(builder, md.Indexes); err != nil {
		return nil, err
	}

	// 5. Record type properties
	if err := loadRecordTypesFromProto(builder, md.RecordTypes); err != nil {
		return nil, err
	}

	// 6. Former indexes
	for _, fiProto := range md.FormerIndexes {
		fi, err := formerIndexFromProto(fiProto)
		if err != nil {
			return nil, fmt.Errorf("former index: %w", err)
		}
		builder.formerIndexes = append(builder.formerIndexes, fi)
	}

	// 7. Flags
	if err := loadMetaDataFlagsFromProto(builder, md); err != nil {
		return nil, err
	}

	if options.localFileDescriptor != nil {
		if err := builder.useLocalFileDescriptor(options.localFileDescriptor, options.evolutionValidator); err != nil {
			return nil, err
		}
	}
	return builder.Build()
}

func loadIndexesFromProto(builder *RecordMetaDataBuilder, indexes []*metadatapb.Index) error {
	// Indexes, one at a time in Java's order (loadProtoExceptRecords,
	// RecordMetaDataBuilder.java:187-219): the index's record types are resolved
	// BEFORE the index itself is read, so an unknown record type is the fault
	// Java throws even when the same index (or a later one) also carries a
	// malformed option or key expression, and each index is added before the next
	// is read, so a duplicate name is refused where Java refuses it. Java also
	// looks a name up among the synthetic (joined and unnested) record types; Go
	// carries those verbatim without modelling them, so an index over one is
	// refused here as an unknown record type, the out-of-scope synthetic-type gap
	// recorded in DIVERGENCES.md.
	for _, idxProto := range indexes {
		rtNames := idxProto.RecordType
		rts := make([]*RecordType, 0, len(rtNames))
		for _, name := range rtNames {
			rt := builder.recordTypes[name]
			if rt == nil {
				// RecordMetaDataBuilder.throwUnknownRecordType (:994-996).
				return &MetaDataError{Message: "Unknown record type " + name}
			}
			rts = append(rts, rt)
		}
		idx, err := indexFromProto(idxProto)
		if err != nil {
			return protoDeserializationError(fmt.Errorf("index %s: %w", idxProto.GetName(), err))
		}
		builder.addIndexCommon(idx)
		if len(builder.buildErrors) > 0 {
			return builder.buildErrors[0]
		}
		switch len(rts) {
		case 0:
			builder.universalIndexes = append(builder.universalIndexes, idx)
		case 1:
			rts[0].indexes = append(rts[0].indexes, idx)
		default:
			for _, rt := range rts {
				rt.multiTypeIndexes = append(rt.multiTypeIndexes, idx)
			}
		}
	}
	return nil
}

func loadRecordTypesFromProto(builder *RecordMetaDataBuilder, recordTypes []*metadatapb.RecordType) error {
	// Load record type properties (primary keys, explicit keys, since versions)
	for _, rtProto := range recordTypes {
		rt := builder.recordTypes[rtProto.GetName()]
		if rt == nil {
			// Java's getRecordType (RecordMetaDataBuilder.java:221, :986-990).
			return &MetaDataError{Message: "Unknown record type " + rtProto.GetName()}
		}
		if rtProto.PrimaryKey != nil {
			pk, err := KeyExpressionFromProto(rtProto.PrimaryKey)
			if err != nil {
				return protoDeserializationError(fmt.Errorf("record type %s primary key: %w", rtProto.GetName(), err))
			}
			if countVersionColumns(pk) != 0 {
				return &MetaDataError{Message: "Version in primary key not supported"}
			}
			rt.PrimaryKey = pk
		}
		if rtProto.SinceVersion != nil {
			rt.SinceVersion = int(rtProto.GetSinceVersion())
		}
		if rtProto.ExplicitKey != nil {
			// Canonicalized on the way in, exactly as SetRecordTypeKey does:
			// deserialization is the OTHER door into this field, and a key
			// that arrives through it must land in the same representation.
			// The proto carries an int32 for a key written as one, so without
			// this a round-tripped metadata would compare unequal to the
			// metadata it came from while encoding to identical bytes.
			key, keyErr := valueFromProto(rtProto.ExplicitKey)
			if keyErr != nil {
				return keyErr
			}
			canonical, keyErr := canonicalRecordTypeKey(key)
			if keyErr != nil {
				return fmt.Errorf("record type %q: %w", rt.Name, keyErr)
			}
			rt.explicitRecordTypeKey = canonical
		}
	}
	return nil
}

func loadMetaDataFlagsFromProto(builder *RecordMetaDataBuilder, md *metadatapb.MetaData) error {
	if md.SplitLongRecords != nil {
		builder.splitLongRecords = md.GetSplitLongRecords()
	}
	if md.StoreRecordVersions != nil {
		builder.storeRecordVersions = md.GetStoreRecordVersions()
	}
	if md.Version != nil {
		builder.version = int(md.GetVersion())
	}
	if md.RecordCountKey != nil {
		ck, err := KeyExpressionFromProto(md.RecordCountKey)
		if err != nil {
			return protoDeserializationError(fmt.Errorf("record count key: %w", err))
		}
		builder.recordCountKey = ck
	}
	return nil
}

// loadSubspaceKeySettingsFromProto ports Java's
// RecordMetaDataBuilder.loadSubspaceKeySettingsFromProto (:278-292) exactly,
// including both halves of its pair validation.
//
// The pair is validated rather than tolerated because either half alone is
// ambiguous in a way that decides on-disk key assignment. A counter with the
// flag clear would be read as "name-based, and here is a number nobody uses";
// the flag set with no counter would restart assignment from 0 and hand a new
// index a key an existing one already owns. Java refuses both, so metadata Go
// accepted leniently would be metadata Java cannot load — a one-way door.
//
// The counter takes the MAX of what the caller already set and what the proto
// carries, and the flag is only adopted when the caller has not already enabled
// it: Java's comment is "User might have set the counter already", and the
// asymmetry means loading can raise the counter but never lower it.
func (b *RecordMetaDataBuilder) loadSubspaceKeySettingsFromProto(md *metadatapb.MetaData) error {
	hasCounter := md.SubspaceKeyCounter != nil
	usesCounter := md.GetUsesSubspaceKeyCounter()
	if hasCounter && !usesCounter {
		return &MetaDataProtoDeserializationError{Cause: &MetaDataError{
			Message: "subspaceKeyCounter is set but usesSubspaceKeyCounter is not set in the meta-data proto",
		}}
	}
	if usesCounter && !hasCounter {
		return &MetaDataProtoDeserializationError{Cause: &MetaDataError{
			Message: "usesSubspaceKeyCounter is set but subspaceKeyCounter is not set in the meta-data proto",
		}}
	}
	if !b.counterBasedSubspaceKeys {
		// Only read from the proto if the caller has not already enabled it.
		b.counterBasedSubspaceKeys = usesCounter
	}
	if c := md.GetSubspaceKeyCounter(); c > b.subspaceKeyCounter {
		b.subspaceKeyCounter = c
	}
	return nil
}

// preservedMetaDataFieldsFromProto captures the proto fields this port does not
// model, cloned so that later mutation of the caller's proto cannot reach into
// the built metadata. See preservedMetaDataFields.
func preservedMetaDataFieldsFromProto(md *metadatapb.MetaData) preservedMetaDataFields {
	var p preservedMetaDataFields
	for _, jt := range md.JoinedRecordTypes {
		p.joinedRecordTypes = append(p.joinedRecordTypes, proto.Clone(jt).(*metadatapb.JoinedRecordType))
	}
	for _, ut := range md.UnnestedRecordTypes {
		p.unnestedRecordTypes = append(p.unnestedRecordTypes, proto.Clone(ut).(*metadatapb.UnnestedRecordType))
	}
	for _, fn := range md.UserDefinedFunctions {
		p.userDefinedFunctions = append(p.userDefinedFunctions, proto.Clone(fn).(*metadatapb.PUserDefinedFunction))
	}
	for _, vw := range md.Views {
		p.views = append(p.views, proto.Clone(vw).(*metadatapb.PView))
	}
	for _, query := range md.StoredQueries {
		p.storedQueries = append(p.storedQueries, proto.Clone(query).(*metadatapb.PStoredQuery))
	}
	if u := md.ProtoReflect().GetUnknown(); len(u) > 0 {
		p.unknown = append([]byte(nil), u...)
	}
	return p
}

// protoDeserializationError is err as RecordMetaData.build(proto) throws it:
// a KeyExpressionDeserializationError wrapped in Java's
// MetaDataProtoDeserializationException, which loadProtoExceptRecords catches
// around every key expression it reads (RecordMetaDataBuilder.java:210-227,
// :261-265); any other failure as it is.
func protoDeserializationError(err error) error {
	var de *KeyExpressionDeserializationError
	if errors.As(err, &de) {
		return &MetaDataProtoDeserializationError{Cause: err}
	}
	return err
}
