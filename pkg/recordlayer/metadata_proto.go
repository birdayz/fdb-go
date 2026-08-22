package recordlayer

import (
	"fmt"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ToProto serializes RecordMetaData to its protobuf representation.
// Matches Java's RecordMetaData.toProto().
//
// EVERY field of the MetaData message is accounted for here, in one of two
// ways, and the distinction is the point of this comment — the previous version
// said only "Matches Java's RecordMetaData.toProto()", which read as a
// completeness claim it did not have. Six fields were being dropped silently.
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
//
// Carried is not supported. The port does not model synthetic record types,
// user-defined functions or views; it declines to DELETE them from metadata it
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
// Unknown-field preservation is also the mechanism fields 12-15 were mistakenly
// assumed to be covered by, where it never applied for the opposite reason —
// they are declared fields of a message this code parses, so they were never
// unknown in the first place. Both halves of the message therefore need explicit
// carrying, and neither gets it for free.
func (m *RecordMetaData) ToProto() (*gen.MetaData, error) {
	md := &gen.MetaData{}

	// 1. File descriptor. The retained source proto (when present) is
	// emitted verbatim — Java's toProto() returns the original
	// FileDescriptorProto, and the protodesc reconstruction would diverge
	// byte-wise (absolutized type names, json_name).
	if m.recordsSourceProto != nil {
		md.Records = proto.Clone(m.recordsSourceProto).(*descriptorpb.FileDescriptorProto)
	} else {
		md.Records = protodesc.ToFileDescriptorProto(m.fileDescriptor)
	}

	// 2. Dependencies (transitive)
	deps := collectDependencies(m.fileDescriptor)
	for _, dep := range deps {
		md.Dependencies = append(md.Dependencies, protodesc.ToFileDescriptorProto(dep))
	}

	// 3. Build index → record type name mapping
	indexRecordTypes := m.buildIndexRecordTypeMap()

	// 4. Indexes (sorted by name for determinism)
	indexNames := make([]string, 0, len(m.indexes))
	for name := range m.indexes {
		indexNames = append(indexNames, name)
	}
	sort.Strings(indexNames)

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
		md.Indexes = append(md.Indexes, idxProto)
	}

	// 5. Record types, in DECLARATION order (the union field number):
	// Java's serializer emits them in visit order, so the stored bytes
	// carry union order, not name order — byte-golden-pinned. Name is the
	// deterministic tiebreak for equal indexes (hand-built metadata).
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

	for _, name := range rtNames {
		rt := m.recordTypes[name]
		rtProto := &gen.RecordType{
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
		md.RecordTypes = append(md.RecordTypes, rtProto)
	}

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

	// 9. The fields this port carries but does not model (12-15). Re-emitted
	// verbatim so a Go round-trip is not a silent deletion of another
	// application's joined types, functions or views. See
	// preservedMetaDataFields.
	//
	// Cloned rather than aliased: ToProto's result is the caller's to mutate,
	// and sharing these would let a caller edit the metadata's own state through
	// a proto it believes it owns.
	for _, jt := range m.preserved.joinedRecordTypes {
		md.JoinedRecordTypes = append(md.JoinedRecordTypes, proto.Clone(jt).(*gen.JoinedRecordType))
	}
	for _, ut := range m.preserved.unnestedRecordTypes {
		md.UnnestedRecordTypes = append(md.UnnestedRecordTypes, proto.Clone(ut).(*gen.UnnestedRecordType))
	}
	for _, fn := range m.preserved.userDefinedFunctions {
		md.UserDefinedFunctions = append(md.UserDefinedFunctions, proto.Clone(fn).(*gen.PUserDefinedFunction))
	}
	for _, vw := range m.preserved.views {
		md.Views = append(md.Views, proto.Clone(vw).(*gen.PView))
	}

	// 10. Whatever the generated type has no field for — the extension range
	// above all. Copied, for the same reason the messages above are cloned.
	if len(m.preserved.unknown) > 0 {
		md.ProtoReflect().SetUnknown(
			protoreflect.RawFields(append([]byte(nil), m.preserved.unknown...)))
	}

	return md, nil
}

// RecordMetaDataFromProto deserializes a MetaData proto back into a RecordMetaData.
// Matches Java's RecordMetaDataBuilder.loadFromProto().
func RecordMetaDataFromProto(md *gen.MetaData) (*RecordMetaData, error) {
	if md == nil {
		return nil, &MetaDataError{Message: "nil metadata proto"}
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
	// The comment above claimed the rebuild already operated on a second clone; it
	// did not -- it was handed md.Records and md.Dependencies directly, so this
	// function mutated its CALLER's proto. That reached the wire:
	// SaveRecordMetaData calls this and then marshals the same proto it passed in,
	// so a descriptor carrying relative type names -- which the DDL builder emits
	// -- was persisted absolutized.
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

	// 2. Create builder and set records. Detect the union descriptor by name
	// ("UnionDescriptor") OR by the usage=UNION proto annotation so that
	// files like catalog_data.proto (which uses "CatalogUnion") round-trip
	// correctly. Mirrors Java's RecordMetaData.build() which calls
	// RecordMetaDataBuilder.setRecordsWithUnionDescriptor using whichever
	// message is annotated with usage=UNION.
	unionName := findUnionDescriptorName(fd)
	builder := NewRecordMetaDataBuilder().setRecordsWithUnionName(fd, unionName)
	builder.SetRecordsSourceProto(recordsSource)

	// 2a. Subspace-key settings, BEFORE any index is added. Ordering is
	// behavioural, not cosmetic: addIndexCommon consults the scheme to decide
	// whether an index gets a counter-assigned key, so loading the settings
	// afterwards would apply them to nothing.
	if err := builder.loadSubspaceKeySettingsFromProto(md); err != nil {
		return nil, err
	}

	// 2b. The unmodelled fields, carried verbatim for re-emission.
	builder.preserved = preservedMetaDataFieldsFromProto(md)

	// 3. Load indexes first (need them before record type association)
	indexMap := make(map[string]*Index)
	for _, idxProto := range md.Indexes {
		idx, err := indexFromProto(idxProto)
		if err != nil {
			return nil, fmt.Errorf("index %s: %w", idxProto.GetName(), err)
		}
		indexMap[idx.Name] = idx
	}

	// 4. Associate indexes with record types
	for _, idxProto := range md.Indexes {
		idx := indexMap[idxProto.GetName()]
		rtNames := idxProto.RecordType
		if len(rtNames) == 0 {
			// Universal index
			builder.addIndexCommon(idx)
			builder.universalIndexes = append(builder.universalIndexes, idx)
		} else if len(rtNames) == 1 {
			// Single-type index
			rt := builder.recordTypes[rtNames[0]]
			if rt == nil {
				return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type %q referenced by index %q", rtNames[0], idxProto.GetName())}
			}
			builder.addIndexCommon(idx)
			rt.indexes = append(rt.indexes, idx)
		} else {
			// Multi-type index
			builder.addIndexCommon(idx)
			for _, name := range rtNames {
				rt := builder.recordTypes[name]
				if rt == nil {
					return nil, &MetaDataError{Message: fmt.Sprintf("unknown record type %q referenced by index %q", name, idxProto.GetName())}
				}
				rt.multiTypeIndexes = append(rt.multiTypeIndexes, idx)
			}
		}
	}

	// 5. Load record type properties (primary keys, explicit keys, since versions)
	for _, rtProto := range md.RecordTypes {
		rt := builder.recordTypes[rtProto.GetName()]
		if rt == nil {
			continue
		}
		if rtProto.PrimaryKey != nil {
			pk, err := KeyExpressionFromProto(rtProto.PrimaryKey)
			if err != nil {
				return nil, fmt.Errorf("record type %s primary key: %w", rtProto.GetName(), err)
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
			canonical, keyErr := canonicalRecordTypeKey(valueFromProto(rtProto.ExplicitKey))
			if keyErr != nil {
				return nil, fmt.Errorf("record type %q: %w", rt.Name, keyErr)
			}
			rt.explicitRecordTypeKey = canonical
		}
	}

	// 6. Load former indexes
	for _, fiProto := range md.FormerIndexes {
		fi, err := formerIndexFromProto(fiProto)
		if err != nil {
			return nil, fmt.Errorf("former index: %w", err)
		}
		builder.formerIndexes = append(builder.formerIndexes, fi)
	}

	// 7. Load flags
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
			return nil, fmt.Errorf("record count key: %w", err)
		}
		builder.recordCountKey = ck
	}

	return builder.Build()
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
func (b *RecordMetaDataBuilder) loadSubspaceKeySettingsFromProto(md *gen.MetaData) error {
	hasCounter := md.SubspaceKeyCounter != nil
	usesCounter := md.GetUsesSubspaceKeyCounter()
	if hasCounter && !usesCounter {
		return &MetaDataError{
			Message: "subspaceKeyCounter is set but usesSubspaceKeyCounter is not set in the meta-data proto",
		}
	}
	if usesCounter && !hasCounter {
		return &MetaDataError{
			Message: "usesSubspaceKeyCounter is set but subspaceKeyCounter is not set in the meta-data proto",
		}
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
func preservedMetaDataFieldsFromProto(md *gen.MetaData) preservedMetaDataFields {
	var p preservedMetaDataFields
	for _, jt := range md.JoinedRecordTypes {
		p.joinedRecordTypes = append(p.joinedRecordTypes, proto.Clone(jt).(*gen.JoinedRecordType))
	}
	for _, ut := range md.UnnestedRecordTypes {
		p.unnestedRecordTypes = append(p.unnestedRecordTypes, proto.Clone(ut).(*gen.UnnestedRecordType))
	}
	for _, fn := range md.UserDefinedFunctions {
		p.userDefinedFunctions = append(p.userDefinedFunctions, proto.Clone(fn).(*gen.PUserDefinedFunction))
	}
	for _, vw := range md.Views {
		p.views = append(p.views, proto.Clone(vw).(*gen.PView))
	}
	if u := md.ProtoReflect().GetUnknown(); len(u) > 0 {
		p.unknown = append([]byte(nil), u...)
	}
	return p
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

// indexToProto serializes an Index to its protobuf representation.
// SubspaceKey is stored as tuple-packed bytes matching Java's Tuple.from(key).pack().
func indexToProto(idx *Index) (*gen.Index, error) {
	p := &gen.Index{
		Name: proto.String(idx.Name),
		Type: proto.String(idx.Type),
	}
	if idx.RootExpression != nil {
		p.RootExpression = idx.RootExpression.ToKeyExpression()
	}

	// SubspaceKey → tuple-packed bytes
	subKey := idx.SubspaceTupleKey()
	if subKey != nil {
		p.SubspaceKey = tuple.Tuple{subKey}.Pack()
	}

	if idx.LastModifiedVersion > 0 {
		p.LastModifiedVersion = proto.Int32(int32(idx.LastModifiedVersion))
	}
	if idx.AddedVersion > 0 {
		p.AddedVersion = proto.Int32(int32(idx.AddedVersion))
	}

	// Options
	for k, v := range idx.Options {
		p.Options = append(p.Options, &gen.Index_Option{
			Key:   proto.String(k),
			Value: proto.String(v),
		})
	}

	// Predicate (proto round-trip)
	if idx.predicateProto != nil {
		// The metadata proto is caller-owned output. Do not expose the Index's
		// internal predicate authority through a mutable protobuf pointer.
		p.Predicate = proto.Clone(idx.predicateProto).(*gen.Predicate)
	}

	return p, nil
}

// legacyIndexTypeToType ports Java's Index.indexTypeToType (Index.java:269-279):
// the deprecated `index_type` enum names only two behaviours, RANK and VALUE.
//
//	case RANK: case RANK_UNIQUE:      return IndexTypes.RANK;
//	case INDEX: case UNIQUE: default: return IndexTypes.VALUE;
//
// The `default` arm is Java's and is kept: this enum is frozen deprecated
// metadata, so an unrecognised member can only come from a future writer, and
// Java reads it as VALUE. Failing closed HERE would be a divergence, not a
// safety improvement — the fail-closed judgement belongs to the maintainer
// dispatch, which sees the resulting type string.
func legacyIndexTypeToType(t gen.Index_Type) string {
	switch t {
	case gen.Index_RANK, gen.Index_RANK_UNIQUE:
		return IndexTypeRank
	default:
		return IndexTypeValue
	}
}

// legacyIndexTypeIsUnique ports Java's Index.indexTypeToOptions
// (Index.java:281-291): UNIQUE and RANK_UNIQUE carry IndexOptions.UNIQUE_OPTIONS,
// everything else carries EMPTY_OPTIONS. The uniqueness of a legacy index lives
// in the enum and NOWHERE else, so a reader that ignores it turns a unique index
// into an ordinary one and silently permits the duplicates Java rejects.
func legacyIndexTypeIsUnique(t gen.Index_Type) bool {
	switch t {
	case gen.Index_UNIQUE, gen.Index_RANK_UNIQUE:
		return true
	default:
		return false
	}
}

// groupingFixupForOldMetaData applies Java's compatibility wrap for aggregate
// index types whose serialized root expression is not a GroupingKeyExpression:
//
//	if (!(expr instanceof GroupingKeyExpression) &&
//	        (type.equals(RANK) || type.equals(COUNT) || type.equals(MAX_EVER) ||
//	         type.equals(MIN_EVER) || type.equals(SUM))) {
//	    expr = new GroupingKeyExpression(expr,
//	            type.equals(COUNT) ? expr.getColumnSize() : 1);
//	}
//
// (Index.java:205-215, under the same "Compatibility with old serialized
// meta-data" heading as the VALUE default two lines above it.)
//
// It belongs on the PROTO path and nowhere else, exactly as in Java: the
// programmatic constructors do not wrap, so an index built in code with a
// non-grouping root is still rejected by the validator. This is only for reading
// metadata somebody else already wrote.
//
// The type list is Java's, verbatim, and it is deliberately the BARE _EVER
// spellings rather than the canonical ones. Java compares the raw type string
// here, so `min_ever_long` with a non-grouping root is NOT wrapped and DOES get
// rejected downstream — which is right, because the wrap exists to repair the
// old metadata that used the old spellings, not to paper over new metadata that
// is simply wrong. Canonicalizing here would silently accept the second.
//
// Without this, canonicalizing the grouping validator turns a silent
// mis-dispatch into a REFUSAL to open Java-authored metadata: a bare
// `min_ever` index with an unwrapped root is precisely the shape the bare
// spellings exist to read, Java loads it by wrapping, and Go would reject it.
func groupingFixupForOldMetaData(indexType string, expr KeyExpression) KeyExpression {
	if _, already := expr.(*GroupingKeyExpression); already {
		return expr
	}
	switch indexType {
	case IndexTypeRank, IndexTypeCount, IndexTypeMaxEver, IndexTypeMinEver, IndexTypeSum:
	default:
		return expr
	}
	groupedCount := 1
	if indexType == IndexTypeCount {
		// Java: `expr.getColumnSize()` — every column is aggregated, none groups.
		groupedCount = expr.ColumnSize()
	}
	return &GroupingKeyExpression{wholeKey: expr, groupedCount: groupedCount}
}

// indexFromProto deserializes an Index from protobuf.
func indexFromProto(p *gen.Index) (*Index, error) {
	idx := &Index{
		Name: p.GetName(),
		// An ABSENT type means VALUE, exactly as Java reads it:
		// `type = proto.hasType() ? proto.getType() : IndexTypes.VALUE`
		// (Index.java:203). The `type` field carries no protobuf default, so an
		// index serialized by a Java app that never set it arrives here as nil,
		// and GetType() would flatten that to "".
		//
		// Presence, not emptiness, is the test — a type EXPLICITLY set to "" is a
		// type no maintainer implements, and it must reach the dispatch and be
		// refused there rather than be quietly promoted to VALUE. Java draws the
		// line in the same place, via hasType().
		//
		// This was invisible while the maintainer dispatch answered every
		// unrecognised type with the value-index maintainer: "" landed on the
		// default arm and came back correct by accident. With that arm failing
		// closed, the accident becomes a refusal to open Java-authored metadata,
		// so the default has to be stated where Java states it.
		Type:    IndexTypeValue,
		Options: make(map[string]string),
	}
	// The DEPRECATED `index_type` enum wins when present, and it supplies the
	// options too — Java checks it FIRST and takes the `type`/`options` branch
	// only in its else (Index.java:199-204):
	//
	//	if (proto.hasIndexType()) {
	//	    type = indexTypeToType(proto.getIndexType());
	//	    options = indexTypeToOptions(proto.getIndexType());
	//	} else {
	//	    type = proto.hasType() ? proto.getType() : IndexTypes.VALUE;
	//	    options = buildOptions(proto.getOptionsList(), false);
	//	}
	//
	// Skipping this branch is not cosmetic and it fails OPEN in the same
	// direction the dispatch used to. `index_type = UNIQUE` carries its
	// uniqueness in the ENUM, not in the options list, so reading only `type`
	// yields a plain value index and the uniqueness constraint silently
	// disappears — duplicate entries where Java raises. `RANK`/`RANK_UNIQUE`
	// are worse: they name a maintainer, and with `type` absent the index would
	// default to VALUE and be maintained in the wrong format, which is exactly
	// the corruption the fail-closed dispatch exists to stop, arriving through
	// the other door.
	// Java takes options from the enum ALONE on this branch and never reads
	// getOptionsList(), so neither does this — the options loop below belongs
	// to the else-branch.
	legacyIndexType := p.IndexType != nil
	if legacyIndexType {
		idx.Type = legacyIndexTypeToType(p.GetIndexType())
		if legacyIndexTypeIsUnique(p.GetIndexType()) {
			idx.Options[IndexOptionUnique] = "true"
		}
	} else if p.Type != nil {
		idx.Type = p.GetType()
	}

	if p.RootExpression != nil {
		expr, err := KeyExpressionFromProto(p.RootExpression)
		if err != nil {
			return nil, fmt.Errorf("root expression: %w", err)
		}
		idx.RootExpression = groupingFixupForOldMetaData(idx.Type, expr)
	}

	// SubspaceKey: decode tuple-packed bytes
	if len(p.SubspaceKey) > 0 {
		t, err := fastUnpack(p.SubspaceKey)
		if err != nil {
			return nil, fmt.Errorf("subspace key: %w", err)
		}
		if len(t) == 1 {
			idx.subspaceKey = t[0]
		}
	}
	if idx.subspaceKey == nil {
		idx.subspaceKey = idx.Name // Default
	}
	// EVERY index loaded from proto has an explicit subspace key, whether the
	// proto carried one or the name was defaulted in just above. Java sets the
	// bit unconditionally at the same point (Index.java:227), and the
	// unconditional part is what makes a round-trip safe: the builder's
	// counter-based assignment skips indexes whose key is explicit, so
	// reloading a counter-keyed metadata must not re-number the indexes it just
	// read. Setting this only when the proto carried a key would re-number every
	// index that had defaulted to its name — silently moving live data.
	idx.useExplicitSubspaceKey = true

	if p.LastModifiedVersion != nil {
		idx.LastModifiedVersion = int(p.GetLastModifiedVersion())
	}
	if p.AddedVersion != nil {
		idx.AddedVersion = int(p.GetAddedVersion())
	}

	if !legacyIndexType {
		for _, opt := range p.Options {
			idx.Options[opt.GetKey()] = opt.GetValue()
		}
	}

	// Predicate: store proto and build evaluator
	if p.Predicate != nil {
		if err := idx.SetPredicateProto(p.Predicate); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

// formerIndexToProto serializes a FormerIndex to protobuf.
func formerIndexToProto(fi *FormerIndex) (*gen.FormerIndex, error) {
	p := &gen.FormerIndex{
		FormerName: proto.String(fi.FormerName),
	}
	if fi.SubspaceKey != nil {
		p.SubspaceKey = tuple.Tuple{fi.SubspaceKey}.Pack()
	}
	if fi.RemovedVersion > 0 {
		p.RemovedVersion = proto.Int32(int32(fi.RemovedVersion))
	}
	if fi.AddedVersion > 0 {
		p.AddedVersion = proto.Int32(int32(fi.AddedVersion))
	}
	return p, nil
}

// formerIndexFromProto deserializes a FormerIndex from protobuf.
func formerIndexFromProto(p *gen.FormerIndex) (*FormerIndex, error) {
	fi := &FormerIndex{
		FormerName:     p.GetFormerName(),
		RemovedVersion: int(p.GetRemovedVersion()),
		AddedVersion:   int(p.GetAddedVersion()),
	}
	if len(p.SubspaceKey) > 0 {
		t, err := fastUnpack(p.SubspaceKey)
		if err != nil {
			return nil, fmt.Errorf("subspace key: %w", err)
		}
		if len(t) == 1 {
			fi.SubspaceKey = t[0]
		}
	}
	return fi, nil
}

// LiteralToProtoValue serializes a Go literal into the key-expression Value
// proto encoding — Java's LiteralKeyExpression.toProtoValue
// (LiteralKeyExpression.java:176-197). Exported for the index-predicate
// producer (RFC-202 S5): a SimpleComparison's operand is stored in exactly
// this encoding (IndexComparison.java:253).
func LiteralToProtoValue(v any) (*gen.Value, error) {
	return valueToProto(v)
}

// valueToProto serializes a Go value to a Value proto.
// Matches Java's LiteralKeyExpression.toProtoValue().
func valueToProto(v any) (*gen.Value, error) {
	if v == nil {
		return &gen.Value{}, nil
	}
	p := &gen.Value{}
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
	default:
		return nil, fmt.Errorf("unsupported value type %T", v)
	}
	return p, nil
}

// valueFromProto deserializes a Value proto to a Go value.
// Matches Java's LiteralKeyExpression.fromProtoValue().
func valueFromProto(p *gen.Value) any {
	if p == nil {
		return nil
	}
	if p.LongValue != nil {
		return p.GetLongValue()
	}
	if p.IntValue != nil {
		return p.GetIntValue()
	}
	if p.DoubleValue != nil {
		return p.GetDoubleValue()
	}
	if p.FloatValue != nil {
		return p.GetFloatValue()
	}
	if p.BoolValue != nil {
		return p.GetBoolValue()
	}
	if p.StringValue != nil {
		return p.GetStringValue()
	}
	if p.BytesValue != nil {
		return p.BytesValue
	}
	return nil
}

// defaultExcludedDependencies matches Java's RecordMetaData.defaultExcludedDependencies.
// These are Apple Record Layer protos that Java resolves from its classpath at build time,
// so they must not be included in serialized metadata.
var defaultExcludedDependencies = map[string]bool{
	"record_metadata.proto":         true,
	"record_metadata_options.proto": true,
	"tuple_fields.proto":            true,
}

// findUnionDescriptorName returns the union message name. Tries
// "UnionDescriptor" (record-layer-core), "RecordTypeUnion" (fdb-relational),
// then scans for a usage=UNION annotation (e.g. "CatalogUnion").
func findUnionDescriptorName(fd protoreflect.FileDescriptor) string {
	const defaultName = "UnionDescriptor"
	const fdbRelationalName = "RecordTypeUnion"
	if fd.Messages().ByName(protoreflect.Name(defaultName)) != nil {
		return defaultName
	}
	if fd.Messages().ByName(protoreflect.Name(fdbRelationalName)) != nil {
		return fdbRelationalName
	}
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		msg := msgs.Get(i)
		opts, ok := msg.Options().(*descriptorpb.MessageOptions)
		if !ok || opts == nil {
			continue
		}
		ext := proto.GetExtension(opts, gen.E_Record)
		if rto, ok := ext.(*gen.RecordTypeOptions); ok && rto != nil {
			if rto.GetUsage() == gen.RecordTypeOptions_UNION {
				return string(msg.Name())
			}
		}
	}
	return defaultName // Fall back; setRecordsWithUnionName handles missing gracefully.
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

// AbsolutizeFieldTypeNames is the exported form of
// absolutizeFieldTypeNames, for builders that retain a Java-shaped
// (relative-name) proto but need a protodesc-buildable clone.
func AbsolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto) {
	absolutizeFieldTypeNames(fd)
}

// absolutizeFieldTypeNames rewrites relative FieldDescriptorProto.type_name to absolute. protodesc.NewFile resolves relative type_names against the enclosing message scope; Java's buildFrom searches outward.
func absolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto) {
	pkg := fd.GetPackage()
	prefix := "."
	if pkg != "" {
		prefix = "." + pkg + "."
	}
	absolutize := func(f *descriptorpb.FieldDescriptorProto) {
		tn := f.GetTypeName()
		if tn != "" && tn[0] != '.' {
			absolute := prefix + tn
			f.TypeName = &absolute
		}
	}
	var visitMessage func(msg *descriptorpb.DescriptorProto)
	visitMessage = func(msg *descriptorpb.DescriptorProto) {
		for _, f := range msg.GetField() {
			absolutize(f)
		}
		// MESSAGE-SCOPED EXTENSIONS: `DescriptorProto.Extension`, the `extend`
		// block written INSIDE a message, which proto2 allows. This walk used to
		// cover fields and nested types here and extensions only at FILE level,
		// so a relative type name in this position reached the resolver as
		// written. It survived because no fixture could express it: every other
		// descriptor in the corpus is built from a compiled Go file via
		// protodesc.ToFileDescriptorProto, which emits absolute names, making
		// absolutization a no-op on all of them.
		for _, ext := range msg.GetExtension() {
			absolutize(ext)
		}
		for _, nested := range msg.GetNestedType() {
			visitMessage(nested)
		}
	}
	for _, m := range fd.GetMessageType() {
		visitMessage(m)
	}
	// Same for extensions at the file level.
	for _, ext := range fd.GetExtension() {
		absolutize(ext)
	}
}

// rebuildFileDescriptor reconstructs a FileDescriptor from a FileDescriptorProto
// and its dependencies. Uses topological ordering to handle transitive deps.
func rebuildFileDescriptor(
	recordsProto *descriptorpb.FileDescriptorProto,
	depsProto []*descriptorpb.FileDescriptorProto,
) (protoreflect.FileDescriptor, error) {
	absolutizeFieldTypeNames(recordsProto)
	for _, dp := range depsProto {
		absolutizeFieldTypeNames(dp)
	}
	// Build dependency resolver
	resolver := &descriptorResolver{files: make(map[string]protoreflect.FileDescriptor)}

	// Register well-known types from the global registry
	resolver.registerGlobalTypes()

	// Build a map for topological resolution
	depMap := make(map[string]*descriptorpb.FileDescriptorProto)
	for _, dp := range depsProto {
		depMap[dp.GetName()] = dp
	}

	// Topologically resolve dependencies: each dep may itself have deps.
	// `resolved` flags completed entries; `inProgress` detects cycles
	// (e.g. a crafted proto with A→B→A would otherwise recurse until
	// the stack overflows — Go fuzz hit this on a 4-byte adversarial
	// input to RecordMetaDataFromProto).
	resolved := make(map[string]bool)
	inProgress := make(map[string]bool)
	var resolve func(name string) error
	resolve = func(name string) error {
		if resolved[name] {
			return nil
		}
		if inProgress[name] {
			return fmt.Errorf("cyclic proto dependency involving %q", name)
		}
		if _, ok := resolver.files[name]; ok {
			resolved[name] = true
			return nil
		}
		// Check global registry for well-known types
		if fd, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
			resolver.files[name] = fd
			resolved[name] = true
			return nil
		}
		dp, ok := depMap[name]
		if !ok {
			return fmt.Errorf("dependency not found: %s", name)
		}
		inProgress[name] = true
		defer delete(inProgress, name)
		// Resolve transitive deps first
		for _, depName := range dp.Dependency {
			if err := resolve(depName); err != nil {
				return err
			}
		}
		fd, err := protodesc.NewFile(dp, resolver)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", name, err)
		}
		resolver.files[string(fd.Path())] = fd
		resolved[name] = true
		return nil
	}

	for _, dp := range depsProto {
		if err := resolve(dp.GetName()); err != nil {
			return nil, err
		}
	}

	// Build the main file
	fd, err := protodesc.NewFile(recordsProto, resolver)
	if err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}
	return fd, nil
}

// descriptorResolver implements protodesc.Resolver for rebuilding FileDescriptors.
type descriptorResolver struct {
	files map[string]protoreflect.FileDescriptor
}

func (r *descriptorResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := r.files[path]; ok {
		return fd, nil
	}
	// Fall back to global registry for well-known types
	fd, err := protoregistry.GlobalFiles.FindFileByPath(path)
	if err == nil {
		r.files[path] = fd
		return fd, nil
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

func (r *descriptorResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range r.files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	// Fall back to global registry
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err == nil {
		return d, nil
	}
	return nil, fmt.Errorf("descriptor not found: %s", name)
}

// findInFile searches for a descriptor by name in a file.
func findInFile(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.Descriptor {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		if m.FullName() == name {
			return m
		}
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		e := enums.Get(i)
		if e.FullName() == name {
			return e
		}
	}
	return nil
}

// registerGlobalTypes registers well-known proto types (google/protobuf/descriptor.proto, etc.)
// that are imported by user protos.
func (r *descriptorResolver) registerGlobalTypes() {
	descFD := descriptorpb.File_google_protobuf_descriptor_proto
	r.files[string(descFD.Path())] = descFD
}
