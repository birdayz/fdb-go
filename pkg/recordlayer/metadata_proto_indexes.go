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

	"fdb.dev/internal/recordlayer/tuplefast"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
)

// indexToProto serializes an Index to its protobuf representation.
// SubspaceKey is stored as tuple-packed bytes matching Java's Tuple.from(key).pack().
func indexToProto(idx *Index) (*metadatapb.Index, error) {
	p := &metadatapb.Index{
		Name: proto.String(idx.Name),
		Type: proto.String(idx.Type),
	}
	// An index with no root is never written: every reader refuses it
	// ("Exactly one root must be specified for an index"), and Build refuses
	// to build one.
	if idx.RootExpression == nil {
		return nil, &MetaDataError{Message: fmt.Sprintf("Index %s has no root expression", idx.Name)}
	}
	p.RootExpression = idx.RootExpression.ToKeyExpression()

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

	// Options, in stored order (Index.toProto, Index.java:661-663).
	for _, k := range idx.OptionKeys() {
		p.Options = append(p.Options, &metadatapb.Index_Option{
			Key:   proto.String(k),
			Value: proto.String(idx.Options[k]),
		})
	}

	// Predicate (proto round-trip)
	if idx.predicateProto != nil {
		// The metadata proto is caller-owned output. Do not expose the Index's
		// internal predicate authority through a mutable protobuf pointer.
		p.Predicate = proto.Clone(idx.predicateProto).(*metadatapb.Predicate)
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
func legacyIndexTypeToType(t metadatapb.Index_Type) string {
	switch t {
	case metadatapb.Index_RANK, metadatapb.Index_RANK_UNIQUE:
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
func legacyIndexTypeIsUnique(t metadatapb.Index_Type) bool {
	switch t {
	case metadatapb.Index_UNIQUE, metadatapb.Index_RANK_UNIQUE:
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
func indexFromProto(p *metadatapb.Index) (*Index, error) {
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
	if err := applyIndexProtoTypeAndOptions(idx, p); err != nil {
		return nil, err
	}

	// The root, as Index(proto) reads it (Index.java:205): KeyExpression.fromProto
	// of the stored root, where an ABSENT root is the empty message and is
	// refused with "Exactly one root must be specified for an index"
	// (KeyExpression.java:404-405). Go used to skip an absent root and load an
	// index Java refuses, reporting a malformed key beside it first.
	rootProto := p.RootExpression
	if rootProto == nil {
		rootProto = &metadatapb.KeyExpression{}
	}
	expr, err := KeyExpressionFromProto(rootProto)
	if err != nil {
		return nil, fmt.Errorf("root expression: %w", err)
	}
	idx.RootExpression = groupingFixupForOldMetaData(idx.Type, expr)
	// The DEPRECATED `value_expression` is folded into the root as Java folds
	// it (Index.java:215-218, toKeyWithValueExpression :245-251): a value of
	// no columns leaves the root alone, and otherwise the root becomes
	// keyWithValue(concat(root, value), root's column size). Ignoring it would
	// maintain the index under the bare root, writing no value columns where
	// Java writes them.
	if p.ValueExpression != nil {
		value, err := KeyExpressionFromProto(p.ValueExpression)
		if err != nil {
			return nil, fmt.Errorf("value expression: %w", err)
		}
		if value.ColumnSize() > 0 {
			idx.RootExpression = KeyWithValue(Concat(idx.RootExpression, value), idx.RootExpression.ColumnSize())
		}
	}

	if err := applyIndexProtoSubspaceKey(idx, p); err != nil {
		return nil, err
	}

	if p.LastModifiedVersion != nil {
		idx.LastModifiedVersion = int(p.GetLastModifiedVersion())
	}
	if p.AddedVersion != nil {
		idx.AddedVersion = int(p.GetAddedVersion())
	} else {
		// Java (Index.java:227-233): an index stored before the field existed
		// must appear old, so its added version is the first valid version, not
		// the last-modified version the builder would otherwise default it to.
		idx.AddedVersion = 1
	}

	// Predicate: store proto and build evaluator
	if p.Predicate != nil {
		if err := idx.SetPredicateProto(p.Predicate); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

func applyIndexProtoTypeAndOptions(idx *Index, p *metadatapb.Index) error {
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
			idx.SetOption(IndexOptionUnique, "true")
		}
		return nil
	}
	if p.Type != nil {
		idx.Type = p.GetType()
	}
	// Index.buildOptions (Index.java:253-266) puts every option into an
	// ImmutableMap.Builder, so the stored order is kept and a key stored
	// twice fails the build with Guava's IllegalArgumentException. Read
	// here, with the type, as Java's constructor reads both (Index.java:
	// 198-204) before the root and the subspace key, so a repeated option
	// beside a malformed root or key is reported as Java reports it.
	for _, opt := range p.Options {
		if prev, dup := idx.Options[opt.GetKey()]; dup {
			return &DuplicateIndexOptionError{Key: opt.GetKey(), First: prev, Second: opt.GetValue()}
		}
		idx.SetOption(opt.GetKey(), opt.GetValue())
	}
	return nil
}

func applyIndexProtoSubspaceKey(idx *Index, p *metadatapb.Index) error {
	// The subspace key, as Java's Index(proto) reads it (Index.java:221-225): a
	// stored key, present even when empty, must pack exactly one item
	// (decodeSubspaceKey, :80-86, RecordCoreException) and that item must not be
	// null (normalizeSubspaceKey, :88-97, RecordCoreArgumentException); an
	// absent key is the index's name. Go fell back to the name for an empty key,
	// a key of several items and a null item, so it loaded metadata Java refuses
	// and maintained the index under a subspace Java never reads.
	if p.SubspaceKey != nil {
		t, err := tuplefast.Unpack(p.SubspaceKey)
		if err != nil {
			return fmt.Errorf("subspace key: %w", err)
		}
		if len(t) != 1 {
			return &RecordCoreError{Message: "subspace key must encode a single item tuple"}
		}
		if t[0] == nil {
			return &RecordCoreArgumentError{Message: "Index subspace key cannot be null", IndexName: idx.Name, HasSubspaceKey: true}
		}
		idx.subspaceKey = t[0]
	} else {
		idx.subspaceKey = idx.Name
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
	return nil
}

// formerIndexToProto serializes a FormerIndex to protobuf as Java's
// FormerIndex.toProto does (FormerIndex.java:119-133): the key always, the
// versions when positive, and the name only when there is one. Go spells
// Java's null name as "", so an empty name is not written; Java would read a
// written empty name as a name.
func formerIndexToProto(fi *FormerIndex) (*metadatapb.FormerIndex, error) {
	p := &metadatapb.FormerIndex{SubspaceKey: tuple.Tuple{fi.SubspaceKey}.Pack()}
	if fi.FormerName != "" {
		p.FormerName = proto.String(fi.FormerName)
	}
	if fi.RemovedVersion > 0 {
		p.RemovedVersion = proto.Int32(int32(fi.RemovedVersion))
	}
	if fi.AddedVersion > 0 {
		p.AddedVersion = proto.Int32(int32(fi.AddedVersion))
	}
	return p, nil
}

// formerIndexFromProto deserializes a FormerIndex from protobuf as Java's
// FormerIndex(proto) does (FormerIndex.java:51-68): the stored key, absent or
// present, must pack exactly one item (decodeSubspaceKey, Index.java:80-86,
// RecordCoreException; an absent key is the empty tuple), and that item must
// not be null (RecordCoreArgumentException). Go read all four as a nil key, so
// it loaded meta-data Java refuses and, on a store's upgrade, cleared the null
// item's index subspace instead of the dropped index's.
func formerIndexFromProto(p *metadatapb.FormerIndex) (*FormerIndex, error) {
	fi := &FormerIndex{
		FormerName:     p.GetFormerName(),
		RemovedVersion: int(p.GetRemovedVersion()),
		AddedVersion:   int(p.GetAddedVersion()),
	}
	t, err := tuplefast.Unpack(p.SubspaceKey)
	if err != nil {
		return nil, fmt.Errorf("subspace key: %w", err)
	}
	if len(t) != 1 {
		return nil, &RecordCoreError{Message: "subspace key must encode a single item tuple"}
	}
	if t[0] == nil {
		return nil, &RecordCoreArgumentError{Message: "FormerIndex initialized with null subspace key", IndexName: fi.FormerName, HasSubspaceKey: true}
	}
	fi.SubspaceKey = t[0]
	return fi, nil
}
