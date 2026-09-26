package recordlayer

import (
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// The index validators of Java's index maintainer factories
// (IndexValidator.java and each factory's getIndexValidator), which
// MetaDataValidator.validateIndex runs per index at build. Each Java validator
// is two hooks, run in this order by validateIndex below:
//   - validateIndexForRecordType, per record type of the index: the root key
//     expression validated against the type's descriptor, then the
//     validator's own check of the fields it returned;
//   - validate: the per-record-type hook over every type, then the added
//     version against the last modified version, then the type's own checks.
//
// The texts and classes are Java's: KeyExpression.InvalidExpressionException
// is KeyExpressionError, MetaDataException is MetaDataError. Java's log info
// (index name, type, key) is not part of its message, so it is not part of
// Go's.
//
// A type with no Go validator: an index type Java's classpath does not know is
// refused by Java's registry ("Unknown index type for ..."), and Go does not
// refuse it, because Go must load meta-data a Java program wrote with a module
// Go does not implement (DIVERGENCES.md, "Build does not refuse an index type
// Go does not maintain"). VECTOR has no Go validator at Build: Java's
// VectorIndexValidator (its structure and its options) is not ported, except
// the option check a windowed VECTOR index runs (DIVERGENCES.md, "VECTOR index
// metadata validation").

// validateIndexForRecordType is Java's validateIndexForRecordType for idx over
// rt: MetaDataValidator's key validation, then the type validator's check of
// the fields.
func validateIndexForRecordType(idx *Index, rt *RecordType) error {
	fields, err := validateKeyExpressionFields(idx.RootExpression, rt.Descriptor)
	if err != nil {
		// Java's KeyExpression.validate throws its exception unwrapped
		// (MetaDataValidator.java:190).
		return err
	}
	switch canonicalIndexType(idx.Type) {
	case IndexTypeText:
		return validateTextIndexFields(idx, fields)
	case IndexTypeBitmapValue:
		// BitmapValueIndexMaintainerFactory.java:85-105.
		if !lastFieldIsInteger(fields, true) {
			return &KeyExpressionError{Message: "index type only supports integer position key"}
		}
	case IndexTypeSum, IndexTypeMinEverLong, IndexTypeMaxEverLong:
		// The mutations whose value is a long (AtomicMutation.hasLongValue),
		// AtomicMutationIndexMaintainerFactory.java:128-148.
		if !lastFieldIsInteger(fields, false) {
			return &KeyExpressionError{Message: "index type only supports integer field"}
		}
	}
	return nil
}

// lastFieldIsInteger is the switch over the last validated field's protobuf
// type of the atomic and bitmap validators. The bitmap's admits the fixed
// kinds, the atomic's does not. Java reads fields.get(size - 1) unguarded, so
// an index whose key reads no field throws IndexOutOfBoundsException there; Go
// reports it as not an integer field.
func lastFieldIsInteger(fields []protoreflect.FieldDescriptor, fixed bool) bool {
	if len(fields) == 0 {
		return false
	}
	switch fields[len(fields)-1].Kind() {
	case protoreflect.Int64Kind, protoreflect.Uint64Kind, protoreflect.Int32Kind,
		protoreflect.Uint32Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind:
		return true
	case protoreflect.Fixed32Kind, protoreflect.Fixed64Kind, protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind:
		return fixed
	}
	return false
}

// validateTextIndexFields is the TEXT validator's validateIndexForRecordType
// (TextIndexMaintainerFactory.java:113-128): the first ungrouped leaf is a
// string that is not repeated.
func validateTextIndexFields(idx *Index, fields []protoreflect.FieldDescriptor) error {
	position := textFieldPosition(idx.RootExpression)
	// Java's strict '>' guard falls through to List.get at size; Go returns
	// the guard's error at that boundary instead of panicking.
	if position < 0 || position >= len(fields) {
		return &KeyExpressionError{Message: "text index does not have text field after grouped fields"}
	}
	field := fields[position]
	if field.Kind() != protoreflect.StringKind {
		return &KeyExpressionError{Message: "text index has non-string type as text field"}
	}
	if field.IsList() {
		return &KeyExpressionError{Message: "text index does not allow a repeated field for text body"}
	}
	return nil
}

// validateIndexType is the type validator's validate after its base part
// (super.validate), in each factory's order.
func validateIndexType(idx *Index, storeRecordVersions bool) error {
	root := idx.RootExpression
	switch canonicalIndexType(idx.Type) {
	case IndexTypeValue:
		// ValueIndexMaintainerFactory.java:60-63.
		return firstError(
			func() error { return validateNotGrouping(root) },
			func() error { return validateNotVersion(root) })
	case IndexTypeCount, IndexTypeCountUpdates, IndexTypeCountNotNull, IndexTypeSum,
		IndexTypeMinEverTuple, IndexTypeMaxEverTuple, IndexTypeMinEverLong, IndexTypeMaxEverLong,
		IndexTypeMaxEverVersion:
		return validateAtomicIndex(idx)
	case IndexTypeRank, IndexTypeTimeWindowLeaderboard:
		// RankIndexMaintainerFactory and TimeWindowLeaderboardIndexMaintainerFactory.
		return firstError(
			func() error { return validateGrouping(root, 1) },
			func() error { return validateNotVersion(root) })
	case IndexTypePermutedMin, IndexTypePermutedMax:
		return validatePermutedIndex(idx)
	case IndexTypeBitmapValue:
		// BitmapValueIndexMaintainerFactory.java:70-81.
		if err := validateGrouping(root, 1); err != nil {
			return err
		}
		if root.(*GroupingKeyExpression).GetGroupedCount() != 1 {
			return &KeyExpressionError{Message: "index type needs grouped position"}
		}
		return validateNotVersion(root)
	case IndexTypeText:
		return validateTextIndex(idx)
	case IndexTypeVersion:
		// VersionIndexMaintainerFactory.java:60-66.
		return firstError(
			func() error { return validateNotGrouping(root) },
			func() error { return validateStoresRecordVersions(storeRecordVersions) },
			func() error { return validateVersionKey(root) },
			func() error { return validateNotUnique(idx) })
	case IndexTypeMultidimensional:
		// MultidimensionalIndexMaintainerFactory.java:67-72.
		return firstError(
			func() error { return validateNotGrouping(root) },
			func() error { return validateNotVersion(root) },
			func() error { return validateMultidimensionalStructure(root) })
	}
	return nil
}

// validateAtomicIndex is AtomicMutationIndexMaintainerFactory's validate
// (:94-121) over the mutation its index type and clearWhenZero choose
// (AtomicMutationIndexMaintainer.getAtomicMutation, :81-110).
func validateAtomicIndex(idx *Index) error {
	root := idx.RootExpression
	clearWhenZero := idx.IsClearWhenZero()
	t := canonicalIndexType(idx.Type)
	// AtomicMutation.Standard's hasValues, hasSingleValue and
	// getCompareAndClearParam for the mutation. With clearWhenZero,
	// COUNT_NOT_NULL is COUNT_NOT_NULL_CLEAR_WHEN_ZERO, which Java lists among
	// the mutations without values.
	var hasValues, hasSingleValue, clears bool
	switch t {
	case IndexTypeCount:
		clears = clearWhenZero
	case IndexTypeCountUpdates:
	case IndexTypeCountNotNull:
		hasValues, clears = !clearWhenZero, clearWhenZero
	case IndexTypeSum:
		hasValues, hasSingleValue, clears = true, true, clearWhenZero
	case IndexTypeMinEverLong, IndexTypeMaxEverLong:
		hasValues, hasSingleValue = true, true
	case IndexTypeMinEverTuple, IndexTypeMaxEverTuple, IndexTypeMaxEverVersion:
		hasValues = true
	}
	switch {
	case !hasValues:
		if err := validateGrouping(root, 0); err != nil {
			return err
		}
		if root.(*GroupingKeyExpression).GetGroupedCount() != 0 {
			return &KeyExpressionError{Message: "index type does not support non-group fields; use COUNT_NOT_NULL"}
		}
	case !hasSingleValue:
		if err := validateGrouping(root, 1); err != nil {
			return err
		}
	default:
		if err := validateGrouping(root, 1); err != nil {
			return err
		}
		if root.(*GroupingKeyExpression).GetGroupedCount() != 1 {
			return &KeyExpressionError{Message: "index type only supports single field"}
		}
	}
	if t == IndexTypeMaxEverVersion {
		if err := validateVersionInGroupedKeys(root); err != nil {
			return err
		}
	} else if err := validateNotVersion(root); err != nil {
		return err
	}
	if clearWhenZero && !clears {
		return &MetaDataError{Message: "index type does not support clearWhenZero"}
	}
	return nil
}

// validateTextIndex is TextIndexMaintainerFactory's validate (:101-111).
func validateTextIndex(idx *Index) error {
	root := idx.RootExpression
	if err := firstError(
		func() error { return validateNotVersion(root) },
		func() error { return validateNotUnique(idx) },
		// Java's TODO here reads "allow value expressions for covering text
		// indexes"; until Java allows it, Go does not either.
		func() error { return validateNoValue(root) },
	); err != nil {
		return err
	}
	// Java's getOption is null for an absent option, which the registry
	// reads as the default tokenizer, and "" for an empty one, which it does
	// not know.
	if name, ok := idx.Options[IndexOptionTextTokenizerName]; ok && name == "" {
		return &MetaDataError{Message: "unrecognized text tokenizer"}
	}
	tok, err := getTextTokenizer(idx)
	if err != nil {
		return err
	}
	version, err := getTextTokenizerVersion(idx)
	if err != nil {
		return err
	}
	return ValidateTokenizerVersion(tok, version)
}

// validateMultidimensionalStructure is the multidimensional validator's
// validateStructure (MultidimensionalIndexMaintainerFactory.java:87-141): the
// root is a DimensionsKeyExpression, or a KeyWithValueExpression whose key
// begins with one that covers exactly the key's columns.
func validateMultidimensionalStructure(root KeyExpression) error {
	misplaced := &KeyExpressionError{Message: "no dimensions key expression or at incorrect place in index"}
	key := root
	if kwv, ok := key.(*KeyWithValueExpression); ok {
		key = kwv.innerKey
		for {
			then, ok := key.(*CompositeKeyExpression)
			if !ok || len(then.expressions) == 0 {
				break
			}
			key = then.expressions[0]
		}
		dims, ok := key.(*DimensionsKeyExpression)
		if !ok {
			return misplaced
		}
		if err := validateDimensions(dims); err != nil {
			return err
		}
		if dims.ColumnSize() != kwv.splitPoint {
			return &KeyExpressionError{Message: "dimensions key expression must cover exactly all key parts in index"}
		}
		return nil
	}
	dims, ok := key.(*DimensionsKeyExpression)
	if !ok {
		return misplaced
	}
	return validateDimensions(dims)
}

func validateDimensions(dims *DimensionsKeyExpression) error {
	if dims.PrefixSize+dims.DimensionsSize > dims.ColumnSize() {
		return &KeyExpressionError{Message: "dimensions key expression declares wider prefix/dimensions than it covers in index"}
	}
	return nil
}

// The shared checks of Java's IndexValidator (IndexValidator.java:58-160).

func validateGrouping(root KeyExpression, minGrouped int) error {
	grouping, ok := root.(*GroupingKeyExpression)
	if !ok {
		return &KeyExpressionError{Message: "index type requires grouping"}
	}
	if grouping.GetGroupedCount() < minGrouped {
		return &KeyExpressionError{Message: fmt.Sprintf("index type requires grouping at least %d fields", minGrouped)}
	}
	return nil
}

func validateNotGrouping(root KeyExpression) error {
	if _, ok := root.(*GroupingKeyExpression); ok {
		return &KeyExpressionError{Message: "grouping not possible in index type"}
	}
	return nil
}

func validateStoresRecordVersions(storeRecordVersions bool) error {
	if !storeRecordVersions {
		return &MetaDataError{Message: "index type requires metadata store record version"}
	}
	return nil
}

func validateVersionKey(root KeyExpression) error {
	if countVersionColumns(root) != 1 {
		return &KeyExpressionError{Message: "there must be exactly 1 version entry in index"}
	}
	return nil
}

func validateVersionInGroupedKeys(root KeyExpression) error {
	if err := validateGrouping(root, 1); err != nil {
		return err
	}
	grouping := root.(*GroupingKeyExpression)
	groupingVersions, groupedVersions := countVersionColumnsInGroupParts(grouping.wholeKey, grouping.GetGroupingCount())
	if groupingVersions != 0 {
		return &KeyExpressionError{Message: "there must be no version entries in grouping key in index"}
	}
	if groupedVersions != 1 {
		return &KeyExpressionError{Message: "there must be exactly 1 version entry in grouped key in index"}
	}
	return nil
}

func validateNotUnique(idx *Index) error {
	if idx.IsUnique() {
		return &MetaDataError{Message: "index type does not allow unique indexes"}
	}
	return nil
}

func validateNotVersion(root KeyExpression) error {
	if countVersionColumns(root) > 0 {
		return &KeyExpressionError{Message: "version key not possible in index type"}
	}
	return nil
}

func validateNoValue(root KeyExpression) error {
	if _, ok := root.(*KeyWithValueExpression); ok {
		return &KeyExpressionError{Message: "no value expression allowed in index type"}
	}
	return nil
}

// firstError runs checks in order and returns the first error, as a Java
// validator's statements throw the first.
func firstError(checks ...func() error) error {
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// validateCurrentAndFormerIndexes is Java's
// MetaDataValidator.validateCurrentAndFormerIndexes (MetaDataValidator.java:
// 103-165). Each index, one at a time, runs validateIndex; then each former
// index its duplicate key and versions; then a key both an index and a former
// index hold is refused.
//
// Keys are compared by subspaceKeyIdentity, the normalization and equality
// Java's maps get from TupleTypeUtil and Object.equals: an int key and an int64
// key of the same value collide, and a []byte key and a string key with the
// same content do not, since they are different prefixes. Two parts of each
// message are Go's, not Java's: Java visits its indexes in HashMap order
// (RecordMetaDataBuilder.java:148, RecordMetaData.java:318-320) and Go in name
// order, so where several indexes are at fault the one a message names can
// differ, and for a colliding pair such as "c" and "ba" Java can name the two
// the other way round; and the key renders with Go's %v, so a []byte or list
// key prints unlike Java's ByteString and List toString (a ByteString's
// includes its identity hash, which nothing can reproduce). The text around
// them is Java's.
func (b *RecordMetaDataBuilder) validateCurrentAndFormerIndexes(recordTypeNames []string) error {
	indexNames := slices.Sorted(maps.Keys(b.indexes))
	assigned := make(map[any]*Index, len(b.indexes))
	for _, name := range indexNames {
		if err := b.validateIndex(b.indexes[name], recordTypeNames, assigned); err != nil {
			return err
		}
	}

	// MetaDataValidator.validateFormerIndex, in the former indexes' order.
	assignedFormer := make(map[any]*FormerIndex, len(b.formerIndexes))
	for _, fi := range b.formerIndexes {
		sk := subspaceKeyIdentity(fi.SubspaceKey)
		if other, exists := assignedFormer[sk]; exists {
			return &MetaDataError{Message: fmt.Sprintf("Same subspace key %v used by two former indexes %s and %s", fi.SubspaceKey, formerIndexNameOrUnknown(fi), formerIndexNameOrUnknown(other))}
		}
		assignedFormer[sk] = fi
		if fi.AddedVersion > fi.RemovedVersion {
			return &MetaDataError{Message: fmt.Sprintf("Former index%s has added version %d which is greater than the removed version %d", formerIndexNameSuffix(fi), fi.AddedVersion, fi.RemovedVersion)}
		}
		if fi.AddedVersion > b.version {
			return &MetaDataError{Message: fmt.Sprintf("Former index%s has added version %d which is greater than the meta-data version %d", formerIndexNameSuffix(fi), fi.AddedVersion, b.version)}
		}
		if fi.RemovedVersion > b.version {
			return &MetaDataError{Message: fmt.Sprintf("Former index%s has removed version %d which is greater than the meta-data version %d", formerIndexNameSuffix(fi), fi.RemovedVersion, b.version)}
		}
	}
	for _, name := range indexNames {
		idx := b.indexes[name]
		if fi, exists := assignedFormer[subspaceKeyIdentity(idx.SubspaceTupleKey())]; exists {
			return &MetaDataError{Message: fmt.Sprintf("Same subspace key %v used by index %s and former index%s", idx.SubspaceTupleKey(), idx.Name, formerIndexNameSuffix(fi))}
		}
	}
	return nil
}

// validateIndex is Java's MetaDataValidator.validateIndex
// (MetaDataValidator.java:117-156) for one index: its validator (the base
// part, then the type's checks), its subspace key against every index
// validated before it, its versions against the meta-data version, and its
// replacements.
func (b *RecordMetaDataBuilder) validateIndex(idx *Index, recordTypeNames []string, assigned map[any]*Index) error {
	recordTypes := b.recordTypesForIndex(idx, recordTypeNames)

	// A windowed VECTOR index's validator is the sliding-window decorator's,
	// whose checks come first and which ends by running the VECTOR validator
	// (SlidingWindowIndexMaintainerFactory.java:212-238).
	sliding := isSlidingWindowIndex(idx)
	if sliding {
		if err := validateSlidingWindowIndex(recordTypes, idx); err != nil {
			return err
		}
	}

	// IndexValidator.validate (IndexValidator.java:48-54): every record type,
	// then the added version against the last modified version.
	for _, rt := range recordTypes {
		if rt.Descriptor == nil {
			continue
		}
		if err := validateIndexForRecordType(idx, rt); err != nil {
			return err
		}
	}
	if idx.AddedVersion > idx.LastModifiedVersion {
		return &MetaDataError{Message: fmt.Sprintf("Index %s has added version %d which is greater than the last modified version %d", idx.Name, idx.AddedVersion, idx.LastModifiedVersion)}
	}
	if sliding {
		// The VECTOR validator's option half; its structural half is not
		// ported (DIVERGENCES.md, "VECTOR index metadata validation").
		if err := validateVectorIndexOptionsAtBuild(idx); err != nil {
			return err
		}
	} else if err := validateIndexType(idx, b.storeRecordVersions); err != nil {
		return err
	}

	sk := subspaceKeyIdentity(idx.SubspaceTupleKey())
	if other, exists := assigned[sk]; exists {
		return &MetaDataError{Message: fmt.Sprintf("Same subspace key %v used by both %s and %s", idx.SubspaceTupleKey(), idx.Name, other.Name)}
	}
	assigned[sk] = idx

	// An index whose lastModifiedVersion is ahead of the meta-data version
	// reads as "added since" every store header version forever, so each later
	// version bump re-decides its rebuild policy and can clear an already-built
	// index.
	if idx.AddedVersion > b.version {
		return &IndexVersionTooNewError{IndexName: idx.Name, Kind: IndexVersionAdded, AddedVersion: idx.AddedVersion, LastModifiedVersion: idx.LastModifiedVersion, MetaDataVersion: b.version}
	}
	if idx.LastModifiedVersion > b.version {
		return &IndexVersionTooNewError{IndexName: idx.Name, Kind: IndexVersionLastModified, AddedVersion: idx.AddedVersion, LastModifiedVersion: idx.LastModifiedVersion, MetaDataVersion: b.version}
	}

	for _, replacementName := range idx.GetReplacedByIndexNames() {
		replacement, exists := b.indexes[replacementName]
		if !exists {
			return &MetaDataError{Message: fmt.Sprintf("Index %s has replacement index %s that is not in the meta-data", idx.Name, replacementName)}
		}
		if len(replacement.GetReplacedByIndexNames()) > 0 {
			return &MetaDataError{Message: fmt.Sprintf("Index %s has replacement index %s that itself has replacement indexes", idx.Name, replacementName)}
		}
	}
	return nil
}

// recordTypesForIndex is RecordMetaData.recordTypesForIndex over the builder:
// every record type for a universal index, else each type holding it as a
// single-type or multi-type index, once. The types are in name order, Go's
// stand-in for Java's HashMap order.
func (b *RecordMetaDataBuilder) recordTypesForIndex(idx *Index, recordTypeNames []string) []*RecordType {
	universal := slices.Contains(b.universalIndexes, idx)
	var out []*RecordType
	for _, name := range recordTypeNames {
		rt := b.recordTypes[name]
		if universal || slices.Contains(rt.indexes, idx) || slices.Contains(rt.multiTypeIndexes, idx) {
			out = append(out, rt)
		}
	}
	return out
}
