// Portions derived from FoundationDB Record Layer (Index.java,
// RecordMetaDataBuilder.java, LiteralKeyExpression.java, FormerIndex.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"sort"

	"fdb.dev/internal/javahashmap"
	"fdb.dev/internal/recordlayer/protovalue"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ToProto serializes RecordMetaData to its protobuf representation.
// Matches Java's RecordMetaData.toProto().
//
// EVERY field of the MetaData message is accounted for here, in one of two
// ways:
//
// MODELLED — parsed into Go structures and re-serialized from them:
//
//	1  records                    9  dependencies
//	2  indexes                   10  subspace_key_counter
//	3  record_types              11  uses_subspace_key_counter
//	4  split_long_records
//	5  version
//	6  former_indexes
//	7  record_count_key
//	8  store_record_versions
//
// CARRIED VERBATIM — held as opaque protos and re-emitted unchanged, never
// interpreted (see preservedMetaDataFields):
//
//	12  joined_record_types      14  user_defined_functions
//	13  unnested_record_types    15  views
//	16  stored_queries
//
// Carried is not supported. The port does not model synthetic record types,
// user-defined functions, views or stored queries; it declines to DELETE them from metadata it
// did not author. Anything whose behaviour would depend on interpreting them
// must refuse rather than proceed on the partial view — see
// RecordMetaData.DeclaresSyntheticRecordTypes.
//
// Extension ranges (1000-2000) are genuine unknown fields, and they are carried
// here too — as preservedMetaDataFields.unknown. Protobuf's own unknown-field
// preservation does NOT cover them across this round trip: it keeps unknown
// bytes attached to the message they were parsed into, and ToProto builds a
// FRESH MetaData, so nothing carries them over unless this does.
//
// Fields 12-16 are not covered by unknown-field preservation either, for the
// opposite reason: they are declared fields of a message this code parses, so
// they are never unknown. Both halves of the message need explicit carrying.
func (m *RecordMetaData) ToProto() (*metadatapb.MetaData, error) {
	if m.usesLocalRecordsDescriptor {
		return nil, &MetaDataError{Message: "cannot serialize meta-data with a local records descriptor to proto"}
	}
	md := &metadatapb.MetaData{}

	// 1. File descriptor. The retained source proto (when present) is
	// emitted verbatim — Java's toProto() returns the original
	// FileDescriptorProto, and the protodesc reconstruction would diverge
	// byte-wise (absolutized type names, json_name).
	if m.recordsSourceProto != nil {
		md.Records = proto.Clone(m.recordsSourceProto).(*descriptorpb.FileDescriptorProto)
	} else {
		md.Records = protodesc.ToFileDescriptorProto(m.fileDescriptor)
	}

	// Java emits all transitive dependencies from a filename-keyed TreeMap.
	deps := collectDependencies(m.fileDescriptor)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Path() < deps[j].Path() })
	for _, dep := range deps {
		if source := m.dependencySourceProtos[dep.Path()]; source != nil {
			md.Dependencies = append(md.Dependencies, proto.Clone(source).(*descriptorpb.FileDescriptorProto))
		} else {
			md.Dependencies = append(md.Dependencies, protodesc.ToFileDescriptorProto(dep))
		}
	}

	// 3-4. Build index → record type name mapping and serialize indexes.
	indexes, err := m.indexesToProto()
	if err != nil {
		return nil, err
	}
	md.Indexes = indexes

	// 5. Record types in union declaration / expected-size order.
	recordTypes, err := m.recordTypesToProto()
	if err != nil {
		return nil, err
	}
	md.RecordTypes = recordTypes

	// 6. Former indexes
	for _, fi := range m.formerIndexes {
		fiProto, err := formerIndexToProto(fi)
		if err != nil {
			return nil, fmt.Errorf("former index %s: %w", fi.FormerName, err)
		}
		md.FormerIndexes = append(md.FormerIndexes, fiProto)
	}

	// 7. Flags
	md.SplitLongRecords = proto.Bool(m.splitLongRecords)
	md.StoreRecordVersions = proto.Bool(m.storeRecordVersions)
	md.Version = proto.Int32(int32(m.version))

	if m.recordCountKey != nil {
		md.RecordCountKey = m.recordCountKey.ToKeyExpression()
	}

	// 8. Subspace-key counter (fields 10, 11). Emitted as a PAIR and only when
	// the scheme is enabled — Java's RecordMetaData.toProto():710-713:
	//
	//	if (usesSubspaceKeyCounter()) {
	//	    builder.setSubspaceKeyCounter(subspaceKeyCounter);
	//	    builder.setUsesSubspaceKeyCounter(true);
	//	}
	//
	// Emitting one without the other is not a lesser version of this; Java's
	// loader REJECTS either half alone (RecordMetaDataBuilder.java:279-285), so
	// a half-written pair is metadata Java cannot read back.
	if m.usesSubspaceKeyCounter {
		md.SubspaceKeyCounter = proto.Int64(m.subspaceKeyCounter)
		md.UsesSubspaceKeyCounter = proto.Bool(true)
	}

	// 9-10. Unmodelled fields and unknown extension bytes carried verbatim.
	m.appendPreservedFieldsToProto(md)

	return md, nil
}

func (m *RecordMetaData) indexesToProto() ([]*metadatapb.Index, error) {
	indexRecordTypes := m.buildIndexRecordTypeMap()

	// Indexes (sorted by name for determinism)
	indexNames := make([]string, 0, len(m.indexes))
	for name := range m.indexes {
		indexNames = append(indexNames, name)
	}
	sort.Strings(indexNames)

	indexes := make([]*metadatapb.Index, 0, len(indexNames))
	for _, name := range indexNames {
		idx := m.indexes[name]
		idxProto, err := indexToProto(idx)
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", name, err)
		}
		if rtNames, ok := indexRecordTypes[name]; ok {
			sort.Strings(rtNames)
			idxProto.RecordType = rtNames
		}
		indexes = append(indexes, idxProto)
	}
	return indexes, nil
}

func (m *RecordMetaData) recordTypesToProto() ([]*metadatapb.RecordType, error) {
	// Java inserts types in union declaration order, then rebuilds from that
	// iteration in an expected-size map (fillUnionFields/build), not field-number order.
	rtNames := make([]string, 0, len(m.recordTypes))
	for name := range m.recordTypes {
		rtNames = append(rtNames, name)
	}
	sort.Slice(rtNames, func(i, j int) bool {
		a, b := m.recordTypes[rtNames[i]], m.recordTypes[rtNames[j]]
		if a.RecordTypeIndex != b.RecordTypeIndex {
			return a.RecordTypeIndex < b.RecordTypeIndex
		}
		return rtNames[i] < rtNames[j]
	})
	// Hand-built types absent from the union follow in index/name order.
	insertionOrder := make([]string, 0, len(rtNames))
	seen := make(map[string]bool, len(rtNames))
	if m.unionDescriptor != nil {
		fields := m.unionDescriptor.Fields()
		for i := 0; i < fields.Len(); i++ {
			message := fields.Get(i).Message()
			if message == nil {
				continue
			}
			name := string(message.Name())
			if m.recordTypes[name] != nil && !seen[name] {
				insertionOrder = append(insertionOrder, name)
				seen[name] = true
			}
		}
	}
	for _, name := range rtNames {
		if !seen[name] {
			insertionOrder = append(insertionOrder, name)
		}
	}
	rtNames = javahashmap.ExpectedSizeOrder(javahashmap.Order(nil, insertionOrder))

	recordTypes := make([]*metadatapb.RecordType, 0, len(rtNames))
	for _, name := range rtNames {
		rt := m.recordTypes[name]
		rtProto := &metadatapb.RecordType{
			Name: proto.String(rt.Name),
		}
		if rt.PrimaryKey != nil {
			rtProto.PrimaryKey = rt.PrimaryKey.ToKeyExpression()
		}
		if rt.SinceVersion > 0 {
			rtProto.SinceVersion = proto.Int32(int32(rt.SinceVersion))
		}
		if rt.explicitRecordTypeKey != nil {
			v, err := valueToProto(rt.explicitRecordTypeKey)
			if err != nil {
				return nil, fmt.Errorf("record type %s explicit key: %w", name, err)
			}
			rtProto.ExplicitKey = v
		}
		recordTypes = append(recordTypes, rtProto)
	}
	return recordTypes, nil
}

func (m *RecordMetaData) appendPreservedFieldsToProto(md *metadatapb.MetaData) {
	// The fields this port carries but does not model (12-16). Re-emitted
	// verbatim so a Go round-trip is not a silent deletion of another
	// application's joined types, functions, views or stored queries. See
	// preservedMetaDataFields.
	//
	// Cloned rather than aliased: ToProto's result is the caller's to mutate,
	// and sharing these would let a caller edit the metadata's own state through
	// a proto it believes it owns.
	for _, jt := range m.preserved.joinedRecordTypes {
		md.JoinedRecordTypes = append(md.JoinedRecordTypes, proto.Clone(jt).(*metadatapb.JoinedRecordType))
	}
	for _, ut := range m.preserved.unnestedRecordTypes {
		md.UnnestedRecordTypes = append(md.UnnestedRecordTypes, proto.Clone(ut).(*metadatapb.UnnestedRecordType))
	}
	for _, fn := range m.preserved.userDefinedFunctions {
		md.UserDefinedFunctions = append(md.UserDefinedFunctions, proto.Clone(fn).(*metadatapb.PUserDefinedFunction))
	}
	for _, vw := range m.preserved.views {
		md.Views = append(md.Views, proto.Clone(vw).(*metadatapb.PView))
	}
	for _, query := range m.preserved.storedQueries {
		md.StoredQueries = append(md.StoredQueries, proto.Clone(query).(*metadatapb.PStoredQuery))
	}

	// Whatever the generated type has no field for — the extension range
	// above all. Copied, for the same reason the messages above are cloned.
	if len(m.preserved.unknown) > 0 {
		md.ProtoReflect().SetUnknown(
			protoreflect.RawFields(append([]byte(nil), m.preserved.unknown...)),
		)
	}
}

// buildIndexRecordTypeMap returns a map of index name → record type names.
// Universal indexes are NOT included (they have no record type association).
func (m *RecordMetaData) buildIndexRecordTypeMap() map[string][]string {
	result := make(map[string][]string)
	for _, rt := range m.recordTypes {
		for _, idx := range rt.indexes {
			result[idx.Name] = append(result[idx.Name], rt.Name)
		}
		for _, idx := range rt.multiTypeIndexes {
			result[idx.Name] = append(result[idx.Name], rt.Name)
		}
	}
	return result
}

// LiteralToProtoValue serializes a Go literal into the key-expression Value
// proto encoding — Java's LiteralKeyExpression.toProtoValue
// (LiteralKeyExpression.java:176-197). Exported for the index-predicate
// producer (RFC-202 S5): a SimpleComparison's operand is stored in exactly
// this encoding (IndexComparison.java:253).
func LiteralToProtoValue(v any) (*metadatapb.Value, error) {
	return valueToProto(v)
}

// UnsupportedValueTypeError is the RecordCoreException Java's
// LiteralKeyExpression.toProtoValue throws for a value no Value field holds
// (LiteralKeyExpression.java:192-194), with its message: "Unsupported value
// type " and the value's Class (`class java.util.UUID`). JavaClass is the Java
// class the value would be in the target, or the Go type where it has none.
type UnsupportedValueTypeError struct {
	JavaClass string
}

func (e *UnsupportedValueTypeError) Error() string {
	return "Unsupported value type class " + e.JavaClass
}

func (*UnsupportedValueTypeError) JavaRecordCoreException() {}

// valueToProto serializes a Go value to a Value proto.
// Matches Java's LiteralKeyExpression.toProtoValue().
func valueToProto(v any) (*metadatapb.Value, error) {
	if v == nil {
		return &metadatapb.Value{}, nil
	}
	p := &metadatapb.Value{}
	switch val := v.(type) {
	case int:
		p.LongValue = proto.Int64(int64(val))
	case int32:
		p.IntValue = proto.Int32(val)
	case int64:
		p.LongValue = proto.Int64(val)
	case float32:
		p.FloatValue = proto.Float32(val)
	case float64:
		p.DoubleValue = proto.Float64(val)
	case bool:
		p.BoolValue = proto.Bool(val)
	case string:
		p.StringValue = proto.String(val)
	case []byte:
		p.BytesValue = val
	case [16]byte, tuple.UUID:
		// The value layer's UUID; Java's is a java.util.UUID.
		return nil, &UnsupportedValueTypeError{JavaClass: "java.util.UUID"}
	default:
		return nil, &UnsupportedValueTypeError{JavaClass: fmt.Sprintf("%T", v)}
	}
	return p, nil
}

// valueFromProto deserializes a Value proto to a Go value, as Java's
// LiteralKeyExpression.fromProtoValue does (protovalue.FromProto): the one
// field set, nil when none is, and a RecordCoreError "More than one value
// encoded in value" when several are.
func valueFromProto(p *metadatapb.Value) (any, error) {
	v, err := protovalue.FromProto(p)
	if err != nil {
		return nil, &RecordCoreError{Message: err.Error()}
	}
	return v, nil
}

// defaultExcludedDependencies matches Java's RecordMetaData.defaultExcludedDependencies.
// These are Apple Record Layer protos that Java resolves from its classpath at build time,
// so they must not be included in serialized metadata.
var defaultExcludedDependencies = map[string]bool{
	"record_metadata.proto":         true,
	"record_metadata_options.proto": true,
	"tuple_fields.proto":            true,
}

// collectDependencies returns all transitive file descriptor dependencies,
// excluding Apple Record Layer protos (matching Java's defaultExcludedDependencies).
// Excluded protos and their transitive deps are skipped, matching Java's getDependencies().
func collectDependencies(fd protoreflect.FileDescriptor) []protoreflect.FileDescriptor {
	seen := make(map[string]bool)
	var deps []protoreflect.FileDescriptor

	var walk func(f protoreflect.FileDescriptor)
	walk = func(f protoreflect.FileDescriptor) {
		imports := f.Imports()
		for i := 0; i < imports.Len(); i++ {
			dep := imports.Get(i).FileDescriptor
			name := string(dep.Path())
			if seen[name] {
				continue
			}
			// Skip excluded protos and don't recurse into them (matches Java)
			if defaultExcludedDependencies[name] {
				continue
			}
			seen[name] = true
			deps = append(deps, dep)
			walk(dep)
		}
	}
	walk(fd)
	return deps
}
