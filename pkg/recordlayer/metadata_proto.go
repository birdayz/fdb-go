package recordlayer

import (
	"fmt"
	"sort"
	"strings"

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
//
// IT TAKES NO DEPENDENCY SET, and that is deliberate rather than an oversight
// mirroring the unexported form's variadic tail -- the question is worth
// answering here because the sole caller
// (pkg/relational/core/metadata's buildFileDescriptor) holds two
// protoreflect.FileDescriptors two lines before calling this and visibly does
// not pass them.
//
// It does not need to. That caller sets fd.Dependency to
// `tuple_fields.proto` and `record_metadata_options.proto`, both of which are
// generated Go and therefore in protoregistry.GlobalFiles, so
// absolutizeFieldTypeNames resolves and seeds them through the global-registry
// loop -- the same route stored metadata takes for the dependencies
// defaultExcludedDependencies strips. Threading them in explicitly as well
// would seed the identical symbols twice.
//
// A caller whose dependencies are NOT globally registered must use the
// unexported form and pass them, or the walk will not see their types.
func AbsolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto) {
	absolutizeFieldTypeNames(fd)
}

// absolutizeFieldTypeNames rewrites relative `type_name` values to absolute ones
// for builders that retain a Java-shaped (relative-name) proto but need a
// protodesc-buildable clone.
//
// IT RESOLVES BY LEXICAL SCOPE, and it has to. Protobuf resolves a relative name
// by walking OUTWARD from the declaring scope and taking the first match, so a
// field or extension declared inside `Host` that names `Inner` means
// `Host.Inner` whenever `Host` declares one -- even if a top-level `Inner` also
// exists. Rewriting every relative name to `.pkg.` + name ignores that and
// silently REBINDS: measured, `probe.Host.Inner` became `probe.Inner`, a
// different message with different fields, with no error from protodesc because
// the result is a valid name for a real type. Pinned by
// TestAbsolutizeFieldTypeNamesPreservesLexicalScope.
//
// A NAME NO SCOPE DECLARES FALLS BACK TO A ROOT LOOKUP -- `"." + name`, which is
// what Java does at the same point (Descriptors.java's lookupSymbol, the
// fall-through after the scope loop). An earlier version left such names
// RELATIVE and the cross-engine suite failed twelve SQL scenarios; a fallback is
// what repaired them, and it is pinned by
// TestAbsolutizeFieldTypeNamesFallsBackForNamesTheFileDoesNotDeclare.
//
// DO NOT DELETE THIS PASS. A review argued it should go: once
// descriptorResolver returns the NotFound sentinel protodesc's walk works, so
// the rewrite looks like pure risk. The argument is careful and the conclusion
// is wrong.
//
// MEASURED by disabling this function outright and running the cross-engine
// suite. A real stored schema template stops loading:
//
//	RFC-204 schema-template byte-goldens ... struct_uuid_vector
//	  42F59: build file descriptor: protodesc.NewFile: proto: message field
//	  "T.SV" cannot resolve type: unknown kind
//
// The sentinel fix does not cover this. The two changes repair different things
// and both are required.
//
// THE MECHANISM IS A FIELD-NAME / TYPE-NAME COLLISION. `struct_uuid_vector` is
// `CREATE TYPE AS STRUCT SV (...) CREATE TABLE T (id BIGINT, sv SV, ...)`, so
// message `T` carries a field named `SV` whose type is ALSO `SV`. protodesc
// registers fields into the same by-name map it resolves type references
// against (desc_init.go's makeBase), so relative `SV` from inside `T` finds
// `T.SV` -- the FIELD -- and dies at desc_resolve.go's `case 0` with "unknown
// kind". That error has one emitter and it is not a not-found: it fires when
// resolution SUCCEEDS and returns something that is neither message nor enum.
//
// protoc accepts the same source, because its type_name lookup skips non-type
// symbols and protodesc's does not. `declared` holding only messages and enums
// is exactly why this pass agrees with protoc here. Pinned by
// TestAbsolutizationIsRequiredWhenAFieldShadowsItsOwnType, whose control renames
// the field and shows the unrewritten descriptor then builds fine -- relative
// name, empty package and unset kind are each insufficient on their own, which
// is why a first attempt at reproducing this failed and wrongly concluded the
// mechanism was unknowable.
//
// WHY ABSOLUTIZE AT ALL, stated correctly because the first answer here was
// wrong: NOT because protodesc fails to walk outward. It does walk, and an
// earlier version of this comment blamed it for a failure caused by this
// package's own descriptorResolver returning a descriptive error instead of the
// protoregistry.NotFound sentinel -- which protodesc compares with `!=`, so
// anything else aborted its walk at the first candidate. That is fixed at
// FindDescriptorByName. The real reason is a genuine divergence: protodesc
// retries the WHOLE reference at each enclosing scope, while protoc and Java
// resolve the FIRST COMPONENT outward and then require the rest beneath it.
// Leaving names for protodesc would silently diverge from Java on compound
// names -- see TestAbsolutizeFieldTypeNamesResolvesTheFirstComponentOutward.
//
// WHAT CARRIES THE AGREEMENT IS THE SEEDING, NOT THE FALLBACK. Two independently
// built protoc differentials score this exact, and both also score the reason:
//
//	seeded walk + root fallback        0 divergences, 0 build failures
//	seeding removed, fallback kept     every divergence in the corpus
//	seeding kept, fallback prepends
//	  the file's own package           0 divergences
//
// The third row is the one that is easy to misread, so it is written down: with a
// COMPLETE dependency set the fallback is unreachable on any protoc-accepted
// descriptor, because the walk always finds the first component. So the corpora
// say nothing about root-vs-package, and cannot -- that choice is observable only
// where the dependency set is INCOMPLETE, which is the case
// `defaultExcludedDependencies` creates and which the global-registry seeding
// below exists to repair. Rooting the fallback is still what Java does
// (lookupSymbol falls through to a ROOT lookup, never to the file's own package),
// and the package-prefix version was fatal rather than merely divergent for a
// cross-package extendee, so it is right -- but it is right on Java's authority,
// not on the corpora's.
//
// POPULATION, because a bare "0 divergences" would go stale invisibly: both
// corpora are synthetic descriptors with COMPLETE dependency sets, generated by
// crossing package pairs (same, ancestor, sibling, root) with a shadow on/off and
// every write position, and scored against protoc 35.1 via `--descriptor_set_out`.
// Neither contains an excluded-dependency shape, so neither covers the
// global-registry path; that is pinned by
// TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports instead.
//
// WHICH PRODUCERS REACH ANY OF THIS -- an earlier revision of this comment said
// "no producer emits a file with a package" and was wrong, having looked at one
// producer of two:
//
//   - Metadata this port writes. Go's SQL layer emits a `records` file with no
//     package but with TWO declared imports. Measured over all 14 committed
//     schema-template goldens: `records.package` is empty in 14/14,
//     `records.dependency` is 2 in 14/14 (`tuple_fields.proto`,
//     `record_metadata_options.proto`), and `MetaData.dependencies` -- the
//     EMBEDDED descriptor set, a different field -- is 0 in 14/14, because
//     `defaultExcludedDependencies` strips exactly those two.
//
//     Those two numbers are the ones to keep apart: a file that DECLARES two
//     imports whose descriptors are absent from the stored set is an INCOMPLETE
//     dependency set, which is the shape the global-registry seeding below
//     exists for, so the loop does EXECUTE for this producer.
//
//     It is not OBSERVABLE for it, though, and the difference matters. Every
//     relative type_name across all 14 goldens is a bare name of a message
//     declared in that same file, plus one compound `com.apple.foundationdb
//     .record.UUID` in `struct_uuid_vector` -- and with no package on the file,
//     a walk that stops at root and the root fallback both answer `"." + name`,
//     so they are output-identical and the seeding changes nothing. Disabling it
//     leaves every golden byte-identical; what reddens is
//     TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports, whose fixture
//     is the PACKAGED-file-plus-bare-name shape this corpus does not contain.
//
//     So the seeding is load-bearing for the Java producer below, not for this
//     one. An earlier revision of this paragraph claimed `struct_uuid_vector`
//     resolved only because of it, which a mutation refutes -- the same
//     carried-without-re-deriving-its-population error the rest of this comment
//     was written to correct.
//
//   - Metadata Java writes, which is the whole reason the port exists. A user
//     proto's package round-trips into stored metadata through
//     RecordMetaData.toProto(), and ALL 95 protos under
//     fdb-record-layer-core/src/test/proto declare one -- 79 at the top level
//     and 16 more under `evolution/`. (An earlier revision said "79 of 95",
//     which paired a non-recursive file count with a recursive one and
//     presented the two as a ratio. Both counts were real; the relationship
//     between them was not.)
//     `test_records_tuple_fields.proto` is this file's ancestor-package shape
//     exactly: `package com.apple.foundationdb.record.testTupleFields`,
//     importing `tuple_fields.proto` at `package com.apple.foundationdb.record`,
//     writing a bare `UUID` (line 31) whose target sits in the ancestor package
//     AND whose import `defaultExcludedDependencies` strips.
//
// So the shapes below are Java's own protos rather than hypotheticals, and the
// seeding is load-bearing for the JAVA producer -- a user proto carries a
// package, which is what puts a scope on the walk's path to stop at. For this
// port's own producer the loop executes and its output is unobservable, per the
// measurement above; an earlier revision of this sentence claimed both, which
// contradicted the paragraph directly above it. Pinned by
// TestAbsolutizeFieldTypeNamesStopsAtAPackageScope and
// TestAbsolutizeFieldTypeNamesResolvesIntoAnAncestorPackageDependency, which are
// the only arms that separate a stopped walk from the fallback -- see their
// shared preamble for why the obvious fixtures cannot.
func absolutizeFieldTypeNames(fd *descriptorpb.FileDescriptorProto, deps ...*descriptorpb.FileDescriptorProto) {
	pkg := fd.GetPackage()
	pkgPrefix := "."
	if pkg != "" {
		pkgPrefix = "." + pkg + "."
	}

	// declared holds every fully-qualified name a candidate scope may resolve
	// against: the types this file declares, the types its DEPENDENCIES declare,
	// and every PACKAGE prefix in play.
	//
	// All three are needed, and each closes one way this walk used to disagree
	// with protoc:
	//
	//   - Dependency types. Without them a name binding into an import falls
	//     through to the fallback, which answers a ROOT name: `UUID` inside
	//     `package com.apple.foundationdb.record.testTupleFields` becomes
	//     `.UUID` where protoc walks out to
	//     `.com.apple.foundationdb.record.UUID`. That is Java's own
	//     test_records_tuple_fields.proto. (An earlier fallback prepended this
	//     file's own package instead, giving
	//     `.com.apple.foundationdb.record.testTupleFields.UUID`. Both are wrong
	//     here, which is the point: the fix is knowing the symbols, not picking
	//     a better guess.)
	//   - Packages. Java's first-part lookup consults package descriptors
	//     (Descriptors.java's AGGREGATES_ONLY). Without them the walk can never
	//     stop at a package scope, so `probe.Inner` inside package `probe`
	//     became `.probe.probe.Inner` instead of `.probe.Inner`.
	//   - Both together are what makes a cross-package EXTENDEE work. Routing
	//     `Extendee` through this walk without dependency symbols rewrote a valid
	//     `other.Host` (imported) to `.probe.other.Host` and made the descriptor
	//     fail to load — turning a latent divergence into a fatal one, in the
	//     commit that added the extendee rewrite.
	declared := map[string]bool{}
	addPackage := func(p string) {
		if p == "" {
			return
		}
		// Every prefix is a scope protoc can stop at: `a.b.c` declares `.a`,
		// `.a.b` and `.a.b.c`.
		full := "."
		for _, part := range strings.Split(p, ".") {
			full += part
			declared[full] = true
			full += "."
		}
	}
	var collect func(scope string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto)
	collect = func(scope string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto) {
		for _, e := range enums {
			declared[scope+e.GetName()] = true
		}
		for _, m := range msgs {
			full := scope + m.GetName()
			declared[full] = true
			collect(full+".", m.GetNestedType(), m.GetEnumType())
		}
	}
	// collectServices adds service names. Java's first-part lookup uses
	// AGGREGATES_ONLY, which is message|enum|package|SERVICE (Descriptors.java),
	// so a service shadows a compound name exactly as a message would: with
	// service `p.q.X` in scope and an imported `p.X.Y`, Java stops at `p.q.X`,
	// requires `Y` beneath it and REJECTS. Seeding messages, enums and packages
	// but not services makes this walk continue outward and bind `.p.X.Y` --
	// silently loading a descriptor Java refuses.
	//
	// AGAINST THE PINNED JAVA, WHICH MATTERS HERE because the rule changed.
	// Measured from the protobuf-java jar MODULE.bazel pins (4.29.3):
	// `isAggregate` is exactly four instanceof tests -- Descriptor, Enum,
	// Package, Service -- and `lookupSymbol` contains no isType call at all, so
	// a first-component hit is a hard stop whether or not the symbol is a type.
	// protobuf 34.x ADDS a `filter != TYPES_ONLY || isType(result)` branch that
	// keeps searching past a non-type hit; it sits after the compound-name
	// guard, so it changes BARE names only.
	//
	// The two versions therefore disagree about a bare name that lands on a
	// service, and this port follows the pin. Every shape the surrounding
	// comments rest on is COMPOUND (`X.Y`, `probe.Inner`, `A.B`) and so is
	// version-stable; do not "fix" the bare-name case against a newer Java
	// without moving the pin first.
	collectServices := func(scope string, svcs []*descriptorpb.ServiceDescriptorProto) {
		for _, s := range svcs {
			declared[scope+s.GetName()] = true
		}
	}

	addPackage(pkg)
	collect(pkgPrefix, fd.GetMessageType(), fd.GetEnumType())
	collectServices(pkgPrefix, fd.GetService())

	addDep := func(depPkg string, msgs []*descriptorpb.DescriptorProto,
		enums []*descriptorpb.EnumDescriptorProto, svcs []*descriptorpb.ServiceDescriptorProto,
	) {
		depPrefix := "."
		if depPkg != "" {
			depPrefix = "." + depPkg + "."
		}
		addPackage(depPkg)
		collect(depPrefix, msgs, enums)
		collectServices(depPrefix, svcs)
	}

	// ONLY THE VISIBLE DEPENDENCIES, which is not the same as all of them.
	//
	// `deps` is whatever the caller had, and rebuildFileDescriptor hands over the
	// entire TRANSITIVE closure from stored metadata. Java resolves against a
	// narrower set: DescriptorPool's constructor seeds itself with the file's
	// DIRECT dependencies and then recurses through `public_dependency` only, so
	// a transitively-imported file that nobody re-exports is invisible to name
	// resolution.
	//
	// Seeding the whole closure therefore over-exposes symbols: an invisible
	// file declaring `p.q.X` makes this walk stop at a scope Java never
	// considers and answer `.p.q.X.Y`, where Java climbs past and answers
	// `.p.X.Y`.
	//
	// IT FAILS LOUDLY, and saying so precisely matters because the obvious
	// guess is the opposite. Both names exist, so this looks like it should be
	// a silent mis-binding -- but protodesc enforces the SAME visibility rule
	// when it resolves, so the over-exposed name always points into the very
	// file whose invisibility created the stop, and protodesc refuses it:
	//
	//	cannot resolve type: resolved "p.q.X.Y",
	//	but "hidden.proto" is not imported
	//
	// Measured in both arrangements -- the hidden file outside the stored
	// closure, and inside it behind a private import -- and no silent case
	// appears to be constructible: for `.p.q.X.Y` to resolve at all, some
	// VISIBLE file must seed the `.p.q.X` scope, and then the stop is
	// legitimate and there is no divergence in the first place.
	//
	// So this is a real Java-parity divergence that manifests as metadata Java
	// loads and Go rejects, which is why it was never observed in the field
	// rather than why it was harmless.
	//
	// It applies to every symbol kind, not only services: messages and enums
	// from a private transitive import were over-exposed the same way, and for
	// longer.
	// One traversal, not two, because the visible set CROSSES the two sources.
	// A direct import may be globally registered while the file it publicly
	// re-exports is a stored dependency, or the reverse; walking the stored
	// protos and the registry separately loses exactly those hand-offs.
	walkVisibleImports(fd, deps,
		func(dep *descriptorpb.FileDescriptorProto) {
			addDep(dep.GetPackage(), dep.GetMessageType(), dep.GetEnumType(), dep.GetService())
		},
		func(gfd protoreflect.FileDescriptor) {
			collectFromFileDescriptor(gfd, declared, addPackage)
		},
		// PACKAGE ONLY, no types: a visible dependency's own imports contribute
		// their package descriptors to this file's resolution but not their
		// symbols. See walkVisibleImports for why the two exposures differ.
		addPackage,
		nil) // no visit instrumentation in production

	// IMPORTS RESOLVED THROUGH THE GLOBAL REGISTRY, which `deps` does not carry.
	// `defaultExcludedDependencies` strips the Apple record-layer protos from the
	// stored dependency list -- `tuple_fields.proto` above all -- so a descriptor
	// in `package com.apple.foundationdb.record` referencing a bare `UUID` has
	// nothing in `deps` declaring it. The walk then runs out of scopes and the
	// root fallback answers `.UUID`, which exists nowhere: metadata Java accepts
	// stops loading.
	//
	// No fallback rule is a substitute for knowing the symbols -- a package
	// prefix happens to be right for this one shape and wrong in general, a root
	// lookup the other way round -- so the imports are resolved and collected
	// properly instead. TestAbsolutizeFieldTypeNamesSeesGloballyRegisteredImports
	// pins it. Registry lookups happen inside walkVisibleImports above, which is
	// what lets a globally-registered import re-export a stored one.

	// resolveName returns the absolute form of a relative type reference as JAVA
	// would resolve it from `scope`, and reports whether it rewrote anything.
	// `scope` is the fully-qualified name of the declaring message plus a dot, or
	// the package prefix at file level. Already-absolute and empty names are left
	// alone.
	//
	// JAVA, not protoc, and the distinction is not pedantic -- the two disagree
	// and this port follows Java by design. Descriptors.java's lookupSymbol takes
	// the first component, searches enclosing scopes for it with
	// SearchFilter.AGGREGATES_ONLY, and on a hit requires the remainder beneath
	// it without resuming the outward search; isAggregate is
	// Descriptor|EnumDescriptor|PackageDescriptor|ServiceDescriptor, so FIELDS
	// ARE SKIPPED at every intermediate scope. This walk does the same, which is
	// why `declared` holds messages, enums, packages and services and nothing
	// else.
	//
	// MEASURED divergence from protoc, in the extendee position: given a message
	// `Host` with a FIELD named `A` and a top-level message `A`, `extend A`
	// inside `Host` is REJECTED by protoc ("A" is not a message type) and
	// ACCEPTED by protobuf-java. Go accepts, matching Java. Pinned by
	// TestExtendeeSkipsAFieldShadowAsJavaDoes.
	//
	// The mechanism, stated so it can be checked: protoc's CrossLinkField
	// resolves `type_name` with an explicit LOOKUP_TYPES and `extendee` with
	// LOOKUP_ALL -- but LOOKUP_ALL is the DEFAULT argument and is never written
	// at the extendee call site, so grepping for it near that call finds
	// nothing and appears to refute this. Read the default on the declaration
	// of ResolveMode instead. protobuf-java has no such asymmetry: crossLink
	// passes TYPES_ONLY on both axes, the same constant to the same overload.
	//
	// The converse case -- protodesc stopping on a field where BOTH protoc and
	// Java skip it -- is the field/type collision documented above, and it is why
	// this pass cannot simply be deleted.
	resolveName := func(name, scope string) (string, bool) {
		if name == "" || name[0] == '.' {
			return name, false
		}
		// Protobuf resolves the FIRST COMPONENT outward, then requires the rest
		// beneath it: `A.B` inside `.p.X` tries `.p.X.A` before `.p.A`.
		first := name
		if i := strings.IndexByte(name, '.'); i >= 0 {
			first = name[:i]
		}
		for s := scope; ; {
			if declared[s+first] {
				return s + name, true
			}
			// Strip the innermost component and try the enclosing scope. `s`
			// always ends in '.', so drop it before searching for the next.
			trimmed := strings.TrimSuffix(s, ".")
			i := strings.LastIndexByte(trimmed, '.')
			if i < 0 {
				// THE WALK FELL OFF THE END: nothing this file or its
				// dependencies declares matches the first component. Java does a
				// ROOT lookup here (Descriptors.java's lookupSymbol, the
				// fall-through after the scope loop) -- it does NOT prepend the
				// file's own package, and neither does this.
				//
				// The package-prefix version was a Go invention with no Java
				// counterpart, and it was FATAL rather than merely divergent for
				// a cross-package extendee: `extend probe.Ext` inside package
				// `probe` became `.probe.probe.Ext`, which exists nowhere.
				//
				// The committed differential cannot arbitrate root-vs-package --
				// every case in it has a complete dependency set, so this branch
				// is unreachable there and both spellings score 0. See its
				// NOT-covered list. What decides it is Java, and the divergent
				// shapes are pinned by
				// TestAbsolutizeFieldTypeNamesFallsBackToRootNotToThePackage.
				//
				// Leaving the name RELATIVE is still not an option -- that is
				// what the absolutization exists for, and the necessity case is
				// pinned by TestAbsolutizationIsRequiredWhenAFieldShadowsItsOwnType.
				return "." + name, true
			}
			s = trimmed[:i+1]
		}
	}

	// absolutize rewrites BOTH type references a field can carry.
	//
	// EXTENDEE IS NOT OPTIONAL HERE. The shape that needs it is a compound
	// extendee shadowed by a nested MESSAGE:
	//
	//	message Host { message A {} extend A.B { ... } }   // package probe
	//
	// protoc rejects it -- `"A.B" is resolved to "probe.Host.A.B", which is not
	// defined` -- while protodesc, left to itself, silently binds `.probe.A.B`,
	// a DIFFERENT message. Rewriting the extendee under the first-component rule
	// produces `.probe.Host.A.B`, which protodesc then refuses: protoc's answer,
	// symbol for symbol.
	//
	// AN EARLIER VERSION OF THIS COMMENT USED A FIELD, not a nested message, and
	// every engine agrees on that shape -- protoc, protobuf-java, protodesc
	// unrewritten and this pass all accept it. C++ descriptor.cc says why: for a
	// compound name whose first part is a non-aggregate, "We found a symbol but
	// it's not an aggregate. Continue the loop." The mechanism was real and the
	// example was wrong, which made the paragraph unfalsifiable rather than
	// merely imprecise.
	//
	// AND DO NOT ADD NON-TYPE SYMBOLS TO `declared` TO CHASE protoc. For a BARE
	// shadowed extendee (`extend A` where Host declares a FIELD `A`) protoc
	// rejects but protobuf-java 4.29.3 -- the MODULE.bazel pin, i.e. the spec
	// this port follows -- ACCEPTS and binds `probe.A`, because its lookupSymbol
	// probes the first part with AGGREGATES_ONLY and a FieldDescriptor can never
	// be returned. protoc uses LOOKUP_ALL for extendee and LOOKUP_TYPES for
	// type_name, which is where its asymmetry comes from; Java has no such
	// asymmetry, and neither does this. Matching protoc here would move Go AWAY
	// from Java. Measured across all three engines before it was written down.
	absolutize := func(f *descriptorpb.FieldDescriptorProto, scope string) {
		if abs, ok := resolveName(f.GetTypeName(), scope); ok {
			f.TypeName = &abs
		}
		if abs, ok := resolveName(f.GetExtendee(), scope); ok {
			f.Extendee = &abs
		}
	}

	var visitMessage func(scope string, msg *descriptorpb.DescriptorProto)
	visitMessage = func(scope string, msg *descriptorpb.DescriptorProto) {
		inner := scope + msg.GetName() + "."
		for _, f := range msg.GetField() {
			absolutize(f, inner)
		}
		// MESSAGE-SCOPED EXTENSIONS: `DescriptorProto.Extension`, the `extend`
		// block written INSIDE a message, which proto2 allows. This walk covered
		// fields and nested types here and extensions only at FILE level, so a
		// relative type name in this position reached the resolver as written.
		// It survived because no fixture could express it: every other descriptor
		// in the corpus is built from a compiled Go file via
		// protodesc.ToFileDescriptorProto, which emits absolute names, making
		// absolutization a no-op on all of them.
		for _, ext := range msg.GetExtension() {
			absolutize(ext, inner)
		}
		for _, nested := range msg.GetNestedType() {
			visitMessage(inner, nested)
		}
	}
	for _, m := range fd.GetMessageType() {
		visitMessage(pkgPrefix, m)
	}
	// Same for extensions at the file level, whose scope is the package.
	for _, ext := range fd.GetExtension() {
		absolutize(ext, pkgPrefix)
	}
}

// rebuildFileDescriptor reconstructs a FileDescriptor from a FileDescriptorProto
// and its dependencies. Uses topological ordering to handle transitive deps.
func rebuildFileDescriptor(
	recordsProto *descriptorpb.FileDescriptorProto,
	depsProto []*descriptorpb.FileDescriptorProto,
) (protoreflect.FileDescriptor, error) {
	// The dependency set is passed so the resolver can see imported types and
	// packages. Without it a name binding into an import falls through to the
	// root fallback, which answers a bare `.Name` that nothing declares -- wrong
	// for every cross-package reference, and FATAL for a cross-package extendee
	// because the descriptor then stops loading rather than merely binding
	// oddly.
	absolutizeFieldTypeNames(recordsProto, depsProto...)
	for _, dp := range depsProto {
		// Each dependency is resolved against the whole set too: dependencies
		// import one another, and `tuple_fields.proto` referencing a type from a
		// sibling is the same shape as the records proto doing it.
		absolutizeFieldTypeNames(dp, depsProto...)
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
	// global is the registry consulted for imports `files` does not carry. It is
	// a SEAM: nil means protoregistry.GlobalFiles, and a test can substitute its
	// own to drive error paths the real registry cannot be made to produce on
	// demand -- the AMBIGUITY error above all, which is the entire reason
	// FindFileByPath passes the registry's error through instead of flattening
	// it to NotFound. Without the seam that direction was untestable.
	//
	// The type is the INTERFACE, and that is load-bearing rather than
	// stylistic: a *protoregistry.Files cannot be driven into ambiguity at all.
	// RegisterFile refuses a second file at an already-registered path for every
	// receiver except GlobalFiles itself, so the `multiple files named` arm of
	// FindFileByPath is unreachable through any registry a test can construct.
	// Only a stub reaches it.
	global protodesc.Resolver
}

func (r *descriptorResolver) globalFiles() protodesc.Resolver {
	if r.global != nil {
		return r.global
	}
	return protoregistry.GlobalFiles
}

func (r *descriptorResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := r.files[path]; ok {
		return fd, nil
	}
	// Fall back to global registry for well-known types
	fd, err := r.globalFiles().FindFileByPath(path)
	if err == nil {
		r.files[path] = fd
		return fd, nil
	}
	// THE REGISTRY'S OWN ERROR, not an unconditional sentinel. protodesc compares
	// against protoregistry.NotFound with `==` (desc.go's import resolution), so
	// a genuine miss has to arrive as that exact value -- and GlobalFiles already
	// returns exactly it for a miss, so passing the error through is both correct
	// and simpler.
	//
	// Rewriting every failure to NotFound would go too far in the other
	// direction: FindFileByPath also reports AMBIGUITY when the registry holds
	// more than one descriptor at a path, and flattening that to "missing" lets
	// protodesc treat an ambiguous import as an allowed absent one. A miss is
	// tolerable; a conflict is not, and the two must stay distinguishable.
	return nil, err
}

func (r *descriptorResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range r.files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	// Fall back to global registry
	d, err := r.globalFiles().FindDescriptorByName(name)
	if err == nil {
		return d, nil
	}
	// THE SENTINEL, NOT A DESCRIPTIVE ERROR. protodesc's resolver walks a
	// relative name outward through enclosing scopes, and it decides whether to
	// KEEP WALKING by comparing this error to protoregistry.NotFound with `!=`
	// -- a direct equality (`desc_resolve.go`'s findDescriptor), not errors.Is,
	// so a wrapped sentinel does not survive it either. Returning anything else
	// is treated as fatal and ABORTS the walk at the very first candidate, which
	// turns "not here, try the enclosing scope" into "cannot resolve type".
	//
	// That is not hypothetical: it is why relative names appeared not to resolve
	// at all, and it was misdiagnosed once already as protodesc failing to walk
	// outward. protodesc walks correctly; this resolver was stopping it.
	return nil, protoregistry.NotFound
}

// walkVisibleImports visits every file `fd` may resolve names against, in
// Java's sense, and reports each with the exposure Java gives it.
//
// TYPES REACH ONE LEVEL; PACKAGES REACH TWO. That asymmetry is not a quirk to
// simplify away -- it falls out of how Descriptors.java searches, and getting it
// wrong diverges in both directions.
//
// A pool's `descriptorsByName` holds its own file's symbols AND a
// PackageDescriptor for every file in that pool's dependency set, because the
// DescriptorPool constructor calls addPackage over that set and addPackage
// writes into descriptorsByName. findSymbol then consults, in order, this
// pool's descriptorsByName and then `dependency.pool.descriptorsByName` for each
// dependency -- one level of pool indirection, no recursion.
//
// So for a file F whose visible set is V(F) = direct imports plus their
// recursive `public_dependency` closure:
//
//	TYPES visible to F    = F's own  U  { D's own types      : D in V(F) }
//	PACKAGES visible to F = packages(V(F))  U  { packages(V(D)) : D in V(F) }
//
// The second term of the package line is the one that is easy to miss: a
// dependency D that PRIVATELY imports H contributes H's PACKAGE to F, while
// H's messages, enums and services stay hidden. Seeding only V(F) drops that
// package shadow, so a `p.q` file referencing `X.Y` with a visible `.p.X.Y`
// climbs past `.p.q.X` and binds -- where Java stops at the package, requires
// `Y` beneath it, and REJECTS. That is Go accepting metadata Java cannot load,
// which is the direction the conformance rule forbids outright.
//
// `onPackageOnly` receives that second term. `onStored` and `onGlobal` receive
// the full-exposure set.
//
// ONE TRAVERSAL, TWO SOURCES, because the visible set crosses them. An import
// may be a stored descriptor in `pool`, or absent from stored metadata and
// recoverable only from the global registry -- `defaultExcludedDependencies`
// strips the Apple record-layer protos, so `tuple_fields.proto` always arrives
// the second way. Either kind may publicly re-export the other, so walking the
// two sources separately silently drops the hand-off.
//
// A path in neither source is skipped rather than treated as an error. That is
// not laxity: `allowUnknownDependencies` exists in Java for the same reason, and
// this is a pre-pass whose job is to seed a symbol table, not to validate the
// descriptor. protodesc validates, and rejects what it cannot resolve.
// onVisit, when non-nil, is called once per closure-walk step. It is an
// INSTRUMENTATION SEAM and production passes nil.
//
// It exists because the cost this function was rewritten to bound is not
// observable from any of the other callbacks. The rewrite replaced one closure
// walk per visible file with a single walk over the union of their imports; a
// regression that restores the per-file walk while still feeding the union is
// quadratic in TRAVERSAL and yet linear in `onPackageOnly` calls, because the
// final walk deduplicates. Counting emissions therefore cannot see it -- so the
// complexity guard counts visits instead, through here.
func walkVisibleImports(
	fd *descriptorpb.FileDescriptorProto,
	pool []*descriptorpb.FileDescriptorProto,
	onStored func(*descriptorpb.FileDescriptorProto),
	onGlobal func(protoreflect.FileDescriptor),
	onPackageOnly func(pkg string),
	onVisit func(path string),
) {
	byPath := make(map[string]*descriptorpb.FileDescriptorProto, len(pool))
	for _, d := range pool {
		// LAST writer wins, because rebuildFileDescriptor's own `depMap` is
		// built as `depMap[dp.GetName()] = dp` over this same slice and so keeps
		// the last entry at a duplicated path. protobuf-java agrees: its
		// FileDescriptor constructor fills nameToFileMap with HashMap.put in
		// array order.
		//
		// The two must agree or the failure is severe and confusing: the name
		// gets absolutized against one descriptor and then RESOLVED against a
		// different one carrying a different package, so BOTH orderings fail to
		// load -- the symbol table names a type the builder cannot supply.
		// Preferring the first here was an invented policy whose comment
		// asserted the opposite of what the resolver does.
		byPath[d.GetName()] = d
	}

	// importsOf reports a file's package, its DIRECT imports and the subset of
	// those that are PUBLIC, from whichever source holds it.
	importsOf := func(path string) (pkg string, direct, public []string, found bool) {
		if d, ok := byPath[path]; ok {
			direct = d.GetDependency()
			for _, idx := range d.GetPublicDependency() {
				// public_dependency holds INDICES into dependency. Java's
				// FileDescriptor constructor throws on an out-of-range one; this
				// is a pre-pass, so it skips and lets protodesc produce the
				// error ("invalid or duplicate public import index"), which it
				// does for exactly these descriptors.
				if idx < 0 || int(idx) >= len(direct) {
					continue
				}
				public = append(public, direct[idx])
			}
			return d.GetPackage(), direct, public, true
		}
		if g, err := protoregistry.GlobalFiles.FindFileByPath(path); err == nil {
			imps := g.Imports()
			for i := 0; i < imps.Len(); i++ {
				imp := imps.Get(i)
				direct = append(direct, imp.Path())
				if imp.IsPublic {
					public = append(public, imp.Path())
				}
			}
			return string(g.Package()), direct, public, true
		}
		return "", nil, nil, false
	}

	// visibleFrom is V(): the given direct imports, plus everything reachable
	// from them through public imports alone. Recursion is guarded on set-add,
	// exactly as importPublicDependencies is.
	visibleFrom := func(directs []string) []string {
		seen := map[string]bool{}
		var out []string
		var visit func(p string)
		visit = func(p string) {
			// Counted BEFORE the dedup check, because the work a quadratic form
			// does is the repeated arrival, not the repeated insertion.
			if onVisit != nil {
				onVisit(p)
			}
			if seen[p] {
				return
			}
			seen[p] = true
			out = append(out, p)
			_, _, public, ok := importsOf(p)
			if !ok {
				return
			}
			for _, q := range public {
				visit(q)
			}
		}
		for _, p := range directs {
			visit(p)
		}
		return out
	}

	visible := visibleFrom(fd.GetDependency())
	fullyExposed := make(map[string]bool, len(visible))
	for _, path := range visible {
		fullyExposed[path] = true
		if d, ok := byPath[path]; ok {
			onStored(d)
			continue
		}
		if g, err := protoregistry.GlobalFiles.FindFileByPath(path); err == nil {
			onGlobal(g)
		}
	}

	// The second package level: the packages of every visible file's OWN visible
	// set.
	//
	// ONE TRAVERSAL OVER THE UNION, not one per visible file, and the difference
	// is asymptotic rather than cosmetic. Written the obvious way -- call
	// visibleFrom once per entry of `visible` -- each call walks the remaining
	// suffix of a public-import chain, so a chain of n files costs O(n^2) here;
	// and rebuildFileDescriptor runs this whole pass once per dependency, making
	// the total O(n^3). A few hundred descriptors then take seconds, which turns
	// VALID metadata into a CPU exhaustion input.
	//
	// The union form is exactly equivalent because public closure distributes
	// over union: with P the directs of every visible file,
	//
	//	U_{p in visible} V(p) = U_p (directs(p) U pubclosure(directs(p)))
	//	                      = P U pubclosure(P)
	//
	// so one visibleFrom over P yields the same set in O(n), and `seen` inside
	// it keeps each file visited once.
	//
	// EQUALITY OF SETS, NOT OF CALL COUNTS, and the difference is load-bearing.
	// The per-file form could reach the same file from several visible parents
	// and call onPackageOnly once per arrival; the union form visits it once.
	// Differenced against the per-file form by
	// TestUnionSecondLevelMatchesPerFileSecondLevel, over randomized graphs with
	// cycles, self-imports, dangling paths and out-of-range public indices: 0
	// set mismatches, and a minority differing in CALL COUNT. That test is the
	// only thing in the tree that can check this -- the four hand-written
	// correctness arms stay green under the per-file form -- so it is committed
	// rather than quoted. That is safe only
	// because the sink is idempotent -- onPackageOnly is addPackage, which does
	// `declared[full] = true` -- so a dropped repeat cannot change the result.
	// A sink that counted, accumulated or logged would make this rewrite a
	// behaviour change rather than an optimisation.
	var union []string
	for _, path := range visible {
		if _, direct, _, ok := importsOf(path); ok {
			union = append(union, direct...)
		}
	}
	for _, q := range visibleFrom(union) {
		// Files already fully exposed contributed their package above. Skipping
		// them is presentational -- addPackage is idempotent -- but it keeps
		// this loop's output to the level it is responsible for.
		if fullyExposed[q] {
			continue
		}
		if pkg, _, _, ok := importsOf(q); ok && pkg != "" {
			onPackageOnly(pkg)
		}
	}
}

// findInFile searches for a descriptor by name in a file, INCLUDING nested
// types.
//
// It used to scan only the top-level messages and enums, so a legitimate
// `.pkg.Outer.Inner` was reported missing even though the file declares it --
// and, before the sentinel fix above, that miss was reported as a fatal error
// rather than a miss, aborting the caller's outward walk as well. Nested types
// are the common case for the descriptors this port loads.
func findInFile(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.Descriptor {
	if d := findInMessages(fd.Messages(), name); d != nil {
		return d
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

// findInMessages searches msgs and everything declared beneath them.
func findInMessages(msgs protoreflect.MessageDescriptors, name protoreflect.FullName) protoreflect.Descriptor {
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		if m.FullName() == name {
			return m
		}
		enums := m.Enums()
		for j := 0; j < enums.Len(); j++ {
			if e := enums.Get(j); e.FullName() == name {
				return e
			}
		}
		if d := findInMessages(m.Messages(), name); d != nil {
			return d
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

// collectFromFileDescriptor seeds `declared` from an already-built
// protoreflect.FileDescriptor -- the shape the global registry hands back.
//
// It exists because the stored dependency list is NOT the import list:
// defaultExcludedDependencies strips the Apple record-layer protos, so those
// imports arrive only through protoregistry.GlobalFiles and their symbols would
// otherwise be invisible to the resolution walk.
func collectFromFileDescriptor(fd protoreflect.FileDescriptor, declared map[string]bool, addPackage func(string)) {
	addPackage(string(fd.Package()))

	var msgs func(md protoreflect.MessageDescriptors)
	msgs = func(md protoreflect.MessageDescriptors) {
		for i := 0; i < md.Len(); i++ {
			m := md.Get(i)
			declared["."+string(m.FullName())] = true
			enums := m.Enums()
			for j := 0; j < enums.Len(); j++ {
				declared["."+string(enums.Get(j).FullName())] = true
			}
			msgs(m.Messages())
		}
	}
	msgs(fd.Messages())

	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		declared["."+string(enums.Get(i).FullName())] = true
	}
	svcs := fd.Services()
	for i := 0; i < svcs.Len(); i++ {
		declared["."+string(svcs.Get(i).FullName())] = true
	}
}
