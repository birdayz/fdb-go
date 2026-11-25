// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// RecordMetaData.java, RecordType.java, KeyExpression.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync/atomic"
)

// validateFormerIndexKeys rejects a former index with the null key Java's
// FormerIndex constructor refuses (FormerIndex.java:51-58): nothing in Go
// builds such a former index, but FormerIndex.SubspaceKey is an exported field.
func (b *RecordMetaDataBuilder) validateFormerIndexKeys() error {
	for _, fi := range b.formerIndexes {
		if isNilSubspaceKey(fi.SubspaceKey) {
			return &RecordCoreArgumentError{
				Message:        "FormerIndex initialized with null subspace key",
				IndexName:      fi.FormerName,
				SubspaceKey:    fi.SubspaceKey,
				HasSubspaceKey: true,
			}
		}
	}
	return nil
}

// validateRecordTypePrimaryKeys checks in name order that every record type
// has a primary key: Java's build refuses it while it builds the record types,
// before it validates anything (RecordMetaDataBuilder.java:1480-1491).
func (b *RecordMetaDataBuilder) validateRecordTypePrimaryKeys(recordTypeNames []string) error {
	for _, name := range recordTypeNames {
		if b.recordTypes[name].PrimaryKey == nil {
			return &MetaDataError{Message: fmt.Sprintf("Record type %s must have a primary key", name)}
		}
	}
	return nil
}

// validateUnionDescriptor validates union descriptor oneof structure.
// Matches Java's MetaDataValidator.validateUnionDescriptor()
// (MetaDataValidator.java:60, :68-78), which runs first:
//   - Must have at most 1 oneof
//   - If a oneof exists, it must contain all fields
func (b *RecordMetaDataBuilder) validateUnionDescriptor() error {
	if b.unionDescriptor == nil {
		return nil
	}
	oneofs := b.unionDescriptor.Oneofs()
	if oneofs.Len() > 1 {
		return &MetaDataError{Message: "Union descriptor has more than one oneof"}
	}
	if oneofs.Len() == 1 {
		oneof := oneofs.Get(0)
		if oneof.Fields().Len() != b.unionDescriptor.Fields().Len() {
			return &MetaDataError{Message: "Union descriptor oneof must contain every field"}
		}
	}
	return nil
}

// validateRecordTypes runs Java's validateRecordType, per record type, in its
// order (MetaDataValidator.java:64, :80-101): the primary key validated against
// the descriptor and refused if it can produce more than one entry, the record
// type key unique, the since version not past the meta-data version. All of it
// runs before any index is validated, as Java's validateRecordType runs for
// every type before validateCurrentAndFormerIndexes.
func (b *RecordMetaDataBuilder) validateRecordTypes(recordTypeNames []string) error {
	// Two record types collide exactly when their keys occupy the same BYTES,
	// so the seen-set is keyed on the tuple encoding rather than on the value.
	// Keying on the value missed real collisions — int64(7) and uint(7) are
	// distinct values with identical encodings, and a record-type-prefixed
	// primary key built from them puts two types in one key space, where a
	// save silently overwrites the other type's record.
	typeKeySeen := make(map[string]string)
	for _, name := range recordTypeNames {
		rt := b.recordTypes[name]
		if countVersionColumns(rt.PrimaryKey) != 0 {
			return &MetaDataError{Message: "Version in primary key not supported"}
		}
		// Go-only: an empty primary key. Java's validator has no such check;
		// Go refuses it because such a record's split clear range is the whole
		// records subspace, every other record type's records included
		// (DIVERGENCES.md, "Build's record-type checks: the order, and one Go-only refusal").
		if rt.PrimaryKey.ColumnSize() == 0 {
			return &MetaDataError{Message: fmt.Sprintf("record type %q has a primary key that produces no columns (EmptyKeyExpression is not a valid primary key)", name)}
		}
		if rt.Descriptor != nil {
			if err := validateKeyExpression(rt.PrimaryKey, rt.Descriptor); err != nil {
				// Java's KeyExpression.validate throws its exception unwrapped
				// (MetaDataValidator.java:97, :190).
				return err
			}
		}
		if createsDuplicates(rt.PrimaryKey) {
			return &MetaDataError{Message: fmt.Sprintf("Primary key for %s can generate more than one entry", name)}
		}
		key := rt.GetRecordTypeKey()
		dedup, ok := recordTypeKeyIdentity(key)
		if !ok {
			// Unreachable while both doors canonicalize and Build reports
			// builder errors before this loop; stated as an error rather than
			// left to pack, which would panic.
			return &MetaDataError{Message: fmt.Sprintf(
				"record type %q: record type key %v (%T) cannot be used as a key", name, key, key)}
		}
		if prevName, exists := typeKeySeen[dedup]; exists {
			return &MetaDataError{Message: fmt.Sprintf(
				"Same record type key %v used by both %s and %s", key, name, prevName)}
		}
		typeKeySeen[dedup] = name
		if rt.SinceVersion > b.version {
			return &MetaDataError{Message: fmt.Sprintf(
				"Record type %s has since version of %d which is greater than the meta-data version %d",
				name, rt.SinceVersion, b.version)}
		}
	}
	return nil
}

// validateIndexRoots refuses an index without a root first. Go-only as a
// refusal: Java's constructors take a @Nonnull root, and a null one fails its
// validation with a NullPointerException, so no Java program builds such
// meta-data; Go's struct literal can, and indexToProto would store an index
// with no root, which Java's reader and Go's refuse ("Exactly one root must be
// specified for an index").
func (b *RecordMetaDataBuilder) validateIndexRoots() error {
	for _, name := range slices.Sorted(maps.Keys(b.indexes)) {
		if b.indexes[name].RootExpression == nil {
			return &MetaDataError{Message: fmt.Sprintf("Index %s has no root expression", name)}
		}
	}
	return nil
}

// validateIndexAssociations checks that EVERY REGISTERED INDEX IS UNIVERSAL OR
// ASSOCIATED, checked here rather than argued about at the call sites that
// could break it.
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
// shared behaviour, booked rather than unilaterally diverged from.
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
func (b *RecordMetaDataBuilder) validateIndexAssociations() error {
	universalObjs := make(map[*Index]struct{}, len(b.universalIndexes))
	for _, idx := range b.universalIndexes {
		universalObjs[idx] = struct{}{}
	}
	associatedObjs, alsoUniversal, unregistered, mismatched := b.collectRecordTypeIndexRefs(universalObjs)
	renamed, renamedUniversal, orphaned := b.collectRegistryIndexIssues(universalObjs, associatedObjs)

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
		return &MetaDataError{Message: fmt.Sprintf(bad.format, bad.names[0])}
	}
	return nil
}

// collectRecordTypeIndexRefs walks record-type index lists and classifies universal overlaps,
// unregistered associations, and pointer mismatches against b.indexes.
func (b *RecordMetaDataBuilder) collectRecordTypeIndexRefs(universalObjs map[*Index]struct{}) (associatedObjs map[*Index]struct{}, alsoUniversal, unregistered, mismatched []string) {
	associatedObjs = make(map[*Index]struct{}, len(b.indexes))
	for _, rt := range b.recordTypes {
		claimed := make(map[*Index]struct{}, len(rt.indexes)+len(rt.multiTypeIndexes))
		for _, group := range [][]*Index{rt.indexes, rt.multiTypeIndexes} {
			for _, idx := range group {
				if _, dup := claimed[idx]; dup {
					// Accepted: Java accepts it. See validateIndexAssociations.
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
	return associatedObjs, alsoUniversal, unregistered, mismatched
}

// collectRegistryIndexIssues checks b.indexes for renamed keys and orphaned entries.
func (b *RecordMetaDataBuilder) collectRegistryIndexIssues(universalObjs, associatedObjs map[*Index]struct{}) (renamed, renamedUniversal, orphaned []string) {
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
	return renamed, renamedUniversal, orphaned
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

// countVersionColumns is Java's declared versionColumns, not a search for VersionKey leaves.
// FunctionKeyExpression inherits the default 0 even when its arguments contain a version.
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
	case *ListKeyExpression:
		total := 0
		for _, child := range e.children {
			total += countVersionColumns(child)
		}
		return total
	case *GroupingKeyExpression:
		return countVersionColumns(e.wholeKey)
	case *KeyWithValueExpression:
		return countVersionColumns(e.innerKey)
	case *NestingKeyExpression:
		return countVersionColumns(e.child)
	case *SplitKeyExpression:
		return countVersionColumns(e.joined)
	case *DimensionsKeyExpression:
		return countVersionColumns(e.WholeKey)
	default:
		return 0
	}
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
