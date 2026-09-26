package recordlayer

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// MetaDataEvolutionValidator validates that schema changes between an old and new
// RecordMetaData are safe. Prevents accidental data corruption from incompatible
// schema evolution.
//
// Matches Java's com.apple.foundationdb.record.metadata.MetaDataEvolutionValidator.
type MetaDataEvolutionValidator struct {
	allowNoVersionChange              bool
	allowIndexRebuilds                bool
	allowUnsplitToSplit               bool
	allowOlderFormerIndexAddedVersion bool
	allowMissingFormerIndexNames      bool
	disallowTypeRenames               bool
	allowNoSinceVersion               bool
	ignoredIndexOptions               map[string]bool

	// Field-rename options, added in Java 4.12 (#4034 / #4119). All default false,
	// so an unconfigured validator rejects every field-name change (legacy behaviour).
	allowFieldRenames           bool
	allowDeprecatedFieldRenames bool
	allowUndeprecatingFields    bool
}

// allowsAnyFieldRenames reports whether any field-rename option is enabled, gating the
// RenameFieldsVisitor rewrite of primary-key/index key expressions. Matches Java's
// MetaDataEvolutionValidator.allowsAnyFieldRenames().
func (v *MetaDataEvolutionValidator) allowsAnyFieldRenames() bool {
	return v.allowFieldRenames || v.allowDeprecatedFieldRenames
}

// fieldDeprecated reports whether a field carries the `deprecated` proto option.
func fieldDeprecated(fd protoreflect.FieldDescriptor) bool {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil {
		return false
	}
	return opts.GetDeprecated()
}

// MetaDataEvolutionValidatorBuilder builds a MetaDataEvolutionValidator with custom options.
type MetaDataEvolutionValidatorBuilder struct {
	v MetaDataEvolutionValidator
}

// NewMetaDataEvolutionValidator returns a builder for configuring the validator.
func NewMetaDataEvolutionValidator() *MetaDataEvolutionValidatorBuilder {
	return &MetaDataEvolutionValidatorBuilder{}
}

// DefaultMetaDataEvolutionValidator returns the strictest validator (all options false).
// Matches Java's MetaDataEvolutionValidator.getDefaultInstance().
func DefaultMetaDataEvolutionValidator() *MetaDataEvolutionValidator {
	return &MetaDataEvolutionValidator{}
}

func (b *MetaDataEvolutionValidatorBuilder) SetAllowNoVersionChange(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowNoVersionChange = v
	return b
}

func (b *MetaDataEvolutionValidatorBuilder) SetAllowIndexRebuilds(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowIndexRebuilds = v
	return b
}

func (b *MetaDataEvolutionValidatorBuilder) SetAllowUnsplitToSplit(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowUnsplitToSplit = v
	return b
}

func (b *MetaDataEvolutionValidatorBuilder) SetDisallowTypeRenames(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.disallowTypeRenames = v
	return b
}

func (b *MetaDataEvolutionValidatorBuilder) SetAllowOlderFormerIndexAddedVersion(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowOlderFormerIndexAddedVersion = v
	return b
}

func (b *MetaDataEvolutionValidatorBuilder) SetAllowMissingFormerIndexNames(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowMissingFormerIndexNames = v
	return b
}

func (b *MetaDataEvolutionValidatorBuilder) SetAllowNoSinceVersion(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowNoSinceVersion = v
	return b
}

// SetAllowFieldRenames permits any field to be renamed (same field number, new name)
// across a metadata evolution. Matches Java's setAllowFieldRenames.
func (b *MetaDataEvolutionValidatorBuilder) SetAllowFieldRenames(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowFieldRenames = v
	return b
}

// SetAllowDeprecatedFieldRenames permits renaming a field when the old or new field is
// deprecated (a narrower allowance than SetAllowFieldRenames). Matches Java's
// setAllowDeprecatedFieldRenames.
func (b *MetaDataEvolutionValidatorBuilder) SetAllowDeprecatedFieldRenames(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowDeprecatedFieldRenames = v
	return b
}

// SetAllowUndeprecatingFields permits a field that was deprecated to become
// non-deprecated. Matches Java's setAllowUndeprecatingFields.
func (b *MetaDataEvolutionValidatorBuilder) SetAllowUndeprecatingFields(v bool) *MetaDataEvolutionValidatorBuilder {
	b.v.allowUndeprecatingFields = v
	return b
}

// SetIgnoredIndexOptions copies the exact, case-sensitive option names excluded
// from evolution validation. A nil slice clears the set.
func (b *MetaDataEvolutionValidatorBuilder) SetIgnoredIndexOptions(options []string) *MetaDataEvolutionValidatorBuilder {
	b.v.ignoredIndexOptions = make(map[string]bool, len(options))
	for _, option := range options {
		b.v.ignoredIndexOptions[option] = true
	}
	return b
}

// GetIgnoredIndexOptions returns a sorted, independent copy of the ignored names.
func (v *MetaDataEvolutionValidator) GetIgnoredIndexOptions() []string {
	return slices.Sorted(maps.Keys(v.ignoredIndexOptions))
}

// GetIgnoredIndexOptions returns a sorted, independent copy of the ignored names.
func (b *MetaDataEvolutionValidatorBuilder) GetIgnoredIndexOptions() []string {
	return b.v.GetIgnoredIndexOptions()
}

// AsBuilder copies all configuration without sharing mutable option storage.
func (v *MetaDataEvolutionValidator) AsBuilder() *MetaDataEvolutionValidatorBuilder {
	copy := *v
	copy.ignoredIndexOptions = maps.Clone(v.ignoredIndexOptions)
	return &MetaDataEvolutionValidatorBuilder{v: copy}
}

func (b *MetaDataEvolutionValidatorBuilder) Build() *MetaDataEvolutionValidator {
	v := b.v
	v.ignoredIndexOptions = maps.Clone(b.v.ignoredIndexOptions)
	return &v
}

// MetaDataEvolutionError describes a schema evolution violation.
type MetaDataEvolutionError struct {
	Message string
}

func (e *MetaDataEvolutionError) Error() string {
	return e.Message
}

// Validate checks that evolving from oldMetaData to newMetaData is safe.
// Returns nil if the evolution is valid, or an error describing the violation.
// Matches Java's MetaDataEvolutionValidator.validate().
func (v *MetaDataEvolutionValidator) Validate(oldMetaData, newMetaData *RecordMetaData) error {
	// 1. Version check
	if err := v.validateVersion(oldMetaData, newMetaData); err != nil {
		return err
	}

	// 2. Split record changes
	if err := v.validateSplitLongRecords(oldMetaData, newMetaData); err != nil {
		return err
	}

	// Build type rename map before record type and index validation.
	// Matches Java's MetaDataEvolutionValidator.getTypeRenames().
	typeRenames, err := v.getTypeRenames(oldMetaData, newMetaData)
	if err != nil {
		return err
	}

	// 3. Record type validation
	if err := v.validateRecordTypes(oldMetaData, newMetaData, typeRenames); err != nil {
		return err
	}

	// 4. Indexes and former indexes, paired by subspace key.
	if err := v.validateCurrentAndFormerIndexes(oldMetaData, newMetaData, typeRenames); err != nil {
		return err
	}

	return nil
}

// getTypeRenames pairs each old record type with the new one of the union
// field of the same number, as Java's getTypeRenames does
// (MetaDataEvolutionValidator.java:354-377), walking the old union's fields
// in order, so the first renamed type in union order is the one
// disallowTypeRenames names. unionCorrespondence paired every field's message.
func (v *MetaDataEvolutionValidator) getTypeRenames(old, new *RecordMetaData) (map[string]string, error) {
	pairs, err := v.unionCorrespondence(old, new)
	if err != nil {
		return nil, err
	}
	renames := make(map[string]string, len(old.RecordTypes()))
	oldUnion := old.GetUnionDescriptor()
	for i := 0; i < oldUnion.Fields().Len(); i++ {
		oldDesc := oldUnion.Fields().Get(i).Message()
		newDesc := pairs[oldDesc]
		oldName, newName := string(oldDesc.Name()), string(newDesc.Name())
		if v.disallowTypeRenames && oldName != newName {
			return nil, &MetaDataEvolutionError{Message: fmt.Sprintf("record type name changed (old=%q, new=%q)", oldName, newName)}
		}
		renames[oldName] = newName
	}
	return renames, nil
}

// validateVersion is the version check of Java's validate (MetaDataEvolutionValidator
// .java:154): an OLDER new version is refused whatever the options say, and an equal
// one unless allowNoVersionChange. A store opened with metadata older than its header
// fails with StaleMetaDataVersionError, so admitting a downgrade here would leave every
// store of the evolved metadata unopenable.
func (v *MetaDataEvolutionValidator) validateVersion(old, new *RecordMetaData) error {
	if new.Version() < old.Version() || (!v.allowNoVersionChange && new.Version() == old.Version()) {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("new meta-data does not have newer version than old meta-data (old=%d, new=%d)",
				old.Version(), new.Version()),
		}
	}
	return nil
}

func (v *MetaDataEvolutionValidator) validateSplitLongRecords(old, new *RecordMetaData) error {
	if old.IsSplitLongRecords() && !new.IsSplitLongRecords() {
		return &MetaDataEvolutionError{
			Message: "new meta-data no longer splits long records",
		}
	}
	if !old.IsSplitLongRecords() && new.IsSplitLongRecords() && !v.allowUnsplitToSplit {
		return &MetaDataEvolutionError{
			Message: "new meta-data splits long records",
		}
	}
	return nil
}

// validateUnion checks the union descriptor for record type splits, merges, and removals.
// Ensures a one-to-one mapping between old and new record types in the union.
// Matches Java's MetaDataEvolutionValidator.validateUnion().
func (v *MetaDataEvolutionValidator) validateUnion(old, new *RecordMetaData) error {
	_, err := v.unionCorrespondence(old, new)
	return err
}

func (v *MetaDataEvolutionValidator) unionCorrespondence(old, new *RecordMetaData) (map[protoreflect.MessageDescriptor]protoreflect.MessageDescriptor, error) {
	oldUnion, newUnion := old.GetUnionDescriptor(), new.GetUnionDescriptor()
	if oldUnion == nil || newUnion == nil {
		// Every built RecordMetaData has a union (the builder refuses a records
		// file without one, "Union descriptor is required"), as every Java one
		// has; only a zero-value RecordMetaData, never built, lacks it.
		return nil, &MetaDataEvolutionError{Message: "meta-data has no union descriptor (it was not built by RecordMetaDataBuilder)"}
	}
	oldToNew := make(map[protoreflect.MessageDescriptor]protoreflect.MessageDescriptor)
	newToOld := make(map[protoreflect.MessageDescriptor]protoreflect.MessageDescriptor)
	seen := make(map[descriptorPair]bool)
	for i := 0; i < oldUnion.Fields().Len(); i++ {
		oldField := oldUnion.Fields().Get(i)
		if oldField.Kind() != protoreflect.MessageKind {
			return nil, &MetaDataEvolutionError{Message: "field in union is not a message type"}
		}
		newField := newUnion.Fields().ByNumber(oldField.Number())
		if newField == nil {
			return nil, &MetaDataEvolutionError{Message: "record type removed from union: " + string(oldField.Message().Name())}
		}
		if newField.Kind() != protoreflect.MessageKind {
			return nil, &MetaDataEvolutionError{Message: "field in new union is not a message type"}
		}
		oldDesc, newDesc := oldField.Message(), newField.Message()
		if prior := oldToNew[oldDesc]; prior != nil && prior != newDesc {
			return nil, &MetaDataEvolutionError{Message: "record type corresponds to multiple types in new meta-data"}
		}
		if prior := newToOld[newDesc]; prior != nil && prior != oldDesc {
			return nil, &MetaDataEvolutionError{Message: "record type corresponds to multiple types in old meta-data"}
		}
		oldToNew[oldDesc], newToOld[newDesc] = newDesc, oldDesc
		if err := v.validateMessageDescriptor(oldDesc, newDesc, seen); err != nil {
			return nil, err
		}
	}
	return oldToNew, nil
}

// validateRecordTypes is Java's method of the same name
// (MetaDataEvolutionValidator.java:376-438), in its order: a removed type, then
// the since version, the primary key and the record type key of each old type,
// then the since version of each new one. Java walks its HashMaps; Go walks the
// names sorted, so which of several violations is named does not depend on map
// order.
func (v *MetaDataEvolutionValidator) validateRecordTypes(old, new *RecordMetaData, typeRenames map[string]string) error {
	oldTypes := old.RecordTypes()
	for _, name := range slices.Sorted(maps.Keys(oldTypes)) {
		oldRT := oldTypes[name]
		newName := typeRenames[name]
		newRT := new.GetRecordType(newName)
		if newRT == nil {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type removed from meta-data (old=%q, new=%q)", name, newName),
			}
		}

		// SinceVersion must not change on existing record types.
		if oldRT.SinceVersion != newRT.SinceVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type since version changed (record type=%q, old=%d, new=%d)",
					newName, oldRT.SinceVersion, newRT.SinceVersion),
			}
		}

		// Primary key must not change
		if err := v.comparePrimaryKeys(newName, oldRT, newRT); err != nil {
			return err
		}

		// Record type key must not change. Compared by key identity: a change
		// from []byte("k") to "k" moves every record of the type to a
		// different key space, so it is exactly the kind of change this
		// check exists to refuse, and a folding comparison called it equal.
		oldKeyID, oldKeyOK := recordTypeKeyIdentity(oldRT.GetRecordTypeKey())
		newKeyID, newKeyOK := recordTypeKeyIdentity(newRT.GetRecordTypeKey())
		if !oldKeyOK || !newKeyOK || oldKeyID != newKeyID {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type key changed (record type=%q, old=%v, new=%v)",
					newName, oldRT.GetRecordTypeKey(), newRT.GetRecordTypeKey()),
			}
		}
	}

	// Build set of new names that correspond to old types (via rename map).
	olderNames := make(map[string]bool, len(oldTypes))
	for _, newName := range typeRenames {
		olderNames[newName] = true
	}

	// Validate new record types have SinceVersion set.
	newTypes := new.RecordTypes()
	for _, name := range slices.Sorted(maps.Keys(newTypes)) {
		newRT := newTypes[name]
		if olderNames[name] {
			continue // Existing type, already validated above
		}
		if newRT.SinceVersion == 0 {
			if !v.allowNoSinceVersion {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("new record type is missing since version (record type=%q)", name),
				}
			}
		} else if newRT.SinceVersion <= old.Version() {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new record type has since version older than old meta-data (record type=%q, since=%d, old=%d)",
					name, newRT.SinceVersion, old.Version()),
			}
		}
	}

	return nil
}

func (v *MetaDataEvolutionValidator) comparePrimaryKeys(name string, oldRT, newRT *RecordType) error {
	oldPK := oldRT.PrimaryKey
	newPK := newRT.PrimaryKey

	if oldPK == nil && newPK == nil {
		return nil
	}
	if oldPK == nil || newPK == nil {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("record type primary key changed (record type=%q)", name),
		}
	}

	// The primary key must be unchanged, modulo allowed field renames. When renames are
	// allowed, rewrite the old key expression onto the new descriptor and compare that.
	// Matches Java's MetaDataEvolutionValidator.validateRecordTypes lines 397-416.
	expectedPK := oldPK
	if v.allowsAnyFieldRenames() && oldRT.Descriptor != nil && newRT.Descriptor != nil {
		renamed, err := renameFields(oldPK, oldRT.Descriptor, newRT.Descriptor)
		if err != nil {
			return err
		}
		expectedPK = renamed
	}

	// Compared by keyExpressionEquals, Java's KeyExpression.equals, as the
	// index roots are (validateIndex).
	if !keyExpressionEquals(expectedPK, newPK) {
		// Distinguish "the key genuinely changed" from "a rename was required but the new
		// key doesn't match the rewritten one" — matching Java's two-message split.
		if keyExpressionEquals(expectedPK, oldPK) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type primary key changed (record type=%q)", name),
			}
		}
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("record type primary key does not match required (record type=%q, after field renames)", name),
		}
	}
	return nil
}

// expectedRenamedIndexExpression rewrites an index's root expression onto each old
// record type it covers, mapping to the new descriptor via typeRenames. All rewrites
// must agree (a multi-type index whose field renames disagree across types is invalid).
// Returns the agreed expression, or nil if the index covers no record types (so the
// caller falls back to the original). Matches Java's MetaDataEvolutionValidator.validateIndex
// lines 689-707.
func (v *MetaDataEvolutionValidator) expectedRenamedIndexExpression(old, new *RecordMetaData, oldIdx *Index, typeRenames map[string]string) (KeyExpression, error) {
	var expected KeyExpression
	for _, oldRT := range old.RecordTypesForIndex(oldIdx) {
		newName, ok := typeRenames[oldRT.Name]
		if !ok {
			newName = oldRT.Name
		}
		newRT := new.GetRecordType(newName)
		if oldRT.Descriptor == nil || newRT == nil || newRT.Descriptor == nil {
			continue
		}
		renamed, err := renameFields(oldIdx.RootExpression, oldRT.Descriptor, newRT.Descriptor)
		if err != nil {
			return nil, err
		}
		if expected == nil {
			expected = renamed
		} else if !keyExpressionEquals(renamed, expected) {
			return nil, &MetaDataEvolutionError{
				Message: fmt.Sprintf("field renames result in inconsistent index definition for multi-type index (index=%q)", oldIdx.Name),
			}
		}
	}
	return expected, nil
}

// validateCurrentAndFormerIndexes is Java's method of the same name
// (MetaDataEvolutionValidator.java:479-555). Indexes and former indexes are
// paired across the two meta-data by SUBSPACE KEY, never by name: the key is
// where an index's entries live, so an index that keeps its name and changes
// its key is a new index beside a missing one, and one that keeps its key and
// changes its name is the same index renamed, which validateIndex refuses. The
// four maps are keyed by subspaceKeyIdentity, the normalization and equality
// Java's HashMaps get from TupleTypeUtil and Object.equals. Java walks its
// HashMaps in hash order; Go walks indexes in name order and former indexes in
// the order the meta-data lists them, so a meta-data pair with more than one
// violation may be refused for a different one of them than Java names.
func (v *MetaDataEvolutionValidator) validateCurrentAndFormerIndexes(old, new *RecordMetaData, typeRenames map[string]string) error {
	oldFormerMap, oldFormers := formerIndexesBySubspaceKey(old)
	oldIndexMap, oldIndexes := indexesBySubspaceKey(old)
	newFormerMap, newFormers := formerIndexesBySubspaceKey(new)
	newIndexMap, newIndexes := indexesBySubspaceKey(new)

	// Every former index stays a former index.
	for _, oldFormer := range oldFormers {
		key := subspaceKeyIdentity(oldFormer.SubspaceKey)
		if newIdx, ok := newIndexMap[key]; ok {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("former index key used for new index in meta-data (subspace key=%v, index=%q)",
					oldFormer.SubspaceKey, newIdx.Name),
			}
		}
		if _, ok := newFormerMap[key]; !ok {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("former index removed from meta-data (subspace key=%v)", oldFormer.SubspaceKey),
			}
		}
	}
	// Every old index is still an index, or is now a former index.
	for _, oldIdx := range oldIndexes {
		key := subspaceKeyIdentity(oldIdx.SubspaceTupleKey())
		_, isIndex := newIndexMap[key]
		_, isFormer := newFormerMap[key]
		if !isIndex && !isFormer {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("index missing in new meta-data (subspace key=%v, index=%q)",
					oldIdx.SubspaceTupleKey(), oldIdx.Name),
			}
		}
	}
	// A new former index either continues an old one unchanged, or replaces an
	// old index (or one added and dropped since the old meta-data) with versions
	// that make every store drop it on its next upgrade.
	for _, newFormer := range newFormers {
		key := subspaceKeyIdentity(newFormer.SubspaceKey)
		if oldFormer, ok := oldFormerMap[key]; ok {
			if err := v.validateFormerIndex(oldFormer, newFormer); err != nil {
				return err
			}
			continue
		}
		if newFormer.RemovedVersion <= old.Version() {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new former index has removed version that is not newer than the old meta-data version (subspace key=%v, removed=%d, old=%d)",
					newFormer.SubspaceKey, newFormer.RemovedVersion, old.Version()),
			}
		}
		oldIdx, ok := oldIndexMap[key]
		if !ok {
			if !v.allowOlderFormerIndexAddedVersion && newFormer.AddedVersion <= old.Version() {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("former index without existing index has added version prior to old meta-data version (subspace key=%v, added=%d, old=%d)",
						newFormer.SubspaceKey, newFormer.AddedVersion, old.Version()),
				}
			}
			continue
		}
		if err := v.validateFormerIndexFromIndex(oldIdx, newFormer); err != nil {
			return err
		}
	}
	// A new index either continues an old one, or is new since the old
	// meta-data's version.
	for _, newIdx := range newIndexes {
		if oldIdx, ok := oldIndexMap[subspaceKeyIdentity(newIdx.SubspaceTupleKey())]; ok {
			if err := v.validateIndex(old, oldIdx, new, newIdx, typeRenames); err != nil {
				return err
			}
			continue
		}
		if newIdx.LastModifiedVersion <= old.Version() {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index has version that is not newer than the old meta-data version (index=%q, version=%d, old=%d)",
					newIdx.Name, newIdx.LastModifiedVersion, old.Version()),
			}
		}
	}
	return nil
}

// indexesBySubspaceKey is Java's getIndexMap (MetaDataEvolutionValidator.java:
// 463-476) with the indexes also returned in name order. As in Java's HashMap,
// a later index with an equal key replaces an earlier one; a built meta-data
// has none, since its validator refuses them.
func indexesBySubspaceKey(md *RecordMetaData) (map[any]*Index, []*Index) {
	all := md.GetAllIndexes()
	byKey := make(map[any]*Index, len(all))
	ordered := make([]*Index, 0, len(all))
	for _, name := range slices.Sorted(maps.Keys(all)) {
		idx := all[name]
		byKey[subspaceKeyIdentity(idx.SubspaceTupleKey())] = idx
		ordered = append(ordered, idx)
	}
	return byKey, ordered
}

// formerIndexesBySubspaceKey is Java's getFormerIndexMap
// (MetaDataEvolutionValidator.java:449-461), with the former indexes also
// returned in the meta-data's order.
func formerIndexesBySubspaceKey(md *RecordMetaData) (map[any]*FormerIndex, []*FormerIndex) {
	formers := md.GetFormerIndexes()
	byKey := make(map[any]*FormerIndex, len(formers))
	for _, fi := range formers {
		byKey[subspaceKeyIdentity(fi.SubspaceKey)] = fi
	}
	return byKey, formers
}

// validateFormerIndex is Java's validateFormerIndex
// (MetaDataEvolutionValidator.java:590-623): a former index kept from the old
// meta-data must keep both versions and its name. The name check does not
// depend on allowMissingFormerIndexNames, which governs only a former index
// that replaces an index.
func (v *MetaDataEvolutionValidator) validateFormerIndex(oldFormer, newFormer *FormerIndex) error {
	if oldFormer.RemovedVersion != newFormer.RemovedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("removed version of former index differs from prior version (subspace key=%v, old=%d, new=%d)",
				newFormer.SubspaceKey, oldFormer.RemovedVersion, newFormer.RemovedVersion),
		}
	}
	if oldFormer.AddedVersion != newFormer.AddedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("added version of former index differs from prior version (subspace key=%v, old=%d, new=%d)",
				newFormer.SubspaceKey, oldFormer.AddedVersion, newFormer.AddedVersion),
		}
	}
	if oldFormer.FormerName != newFormer.FormerName {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("name of former index differs from prior version (subspace key=%v, old=%q, new=%q)",
				newFormer.SubspaceKey, oldFormer.FormerName, newFormer.FormerName),
		}
	}
	return nil
}

// validateFormerIndexFromIndex is Java's validateFormerIndexFromIndex
// (MetaDataEvolutionValidator.java:557-588). allowMissingFormerIndexNames
// admits a former index with NO name; one that has a name must name the index
// it replaces either way. Go spells Java's null name as "".
func (v *MetaDataEvolutionValidator) validateFormerIndexFromIndex(oldIdx *Index, newFormer *FormerIndex) error {
	if (!v.allowMissingFormerIndexNames || newFormer.FormerName != "") && newFormer.FormerName != oldIdx.Name {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("former index has different name than old index (subspace key=%v, old=%q, new=%q)",
				newFormer.SubspaceKey, oldIdx.Name, newFormer.FormerName),
		}
	}
	if newFormer.AddedVersion > oldIdx.AddedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("former index added after old index (subspace key=%v, index=%q, old=%d, new=%d)",
				newFormer.SubspaceKey, oldIdx.Name, oldIdx.AddedVersion, newFormer.AddedVersion),
		}
	}
	if !v.allowOlderFormerIndexAddedVersion && newFormer.AddedVersion != oldIdx.AddedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("former index reports added version older than replacing index (subspace key=%v, index=%q, old=%d, new=%d)",
				newFormer.SubspaceKey, oldIdx.Name, oldIdx.AddedVersion, newFormer.AddedVersion),
		}
	}
	if newFormer.RemovedVersion <= oldIdx.LastModifiedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("former index removed before old index's last modification (subspace key=%v, index=%q, old=%d, new=%d)",
				newFormer.SubspaceKey, oldIdx.Name, oldIdx.LastModifiedVersion, newFormer.RemovedVersion),
		}
	}
	return nil
}

// validateIndex is Java's validateIndex (MetaDataEvolutionValidator.java:
// 625-737), over an old and a new index with the same subspace key.
func (v *MetaDataEvolutionValidator) validateIndex(old *RecordMetaData, oldIdx *Index, new *RecordMetaData, newIdx *Index, typeRenames map[string]string) error {
	name := newIdx.Name
	if oldIdx.Name != newIdx.Name {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("index name changed (old=%q, new=%q)", oldIdx.Name, newIdx.Name),
		}
	}
	if oldIdx.AddedVersion != newIdx.AddedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("new index added version does not match old index added version (index=%q, old=%d, new=%d)",
				name, oldIdx.AddedVersion, newIdx.AddedVersion),
		}
	}
	if oldIdx.LastModifiedVersion > newIdx.LastModifiedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("old index has last-modified version newer than new index (index=%q, old=%d, new=%d)",
				name, oldIdx.LastModifiedVersion, newIdx.LastModifiedVersion),
		}
	}
	if !v.allowIndexRebuilds && oldIdx.LastModifiedVersion != newIdx.LastModifiedVersion {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("last modified version of index changed (index=%q, old=%d, new=%d)",
				name, oldIdx.LastModifiedVersion, newIdx.LastModifiedVersion),
		}
	}

	// When allowIndexRebuilds is true and lastModifiedVersion changed,
	// skip type/expression checks — the index will be rebuilt.
	// Matches Java's MetaDataEvolutionValidator.validateIndex() lines 606-610.
	if v.allowIndexRebuilds && oldIdx.LastModifiedVersion < newIdx.LastModifiedVersion {
		return nil
	}

	if oldIdx.Type != newIdx.Type {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("index type changed (index=%q, old=%q, new=%q)", name, oldIdx.Type, newIdx.Type),
		}
	}

	// Validate index record type scope.
	// Old types (renamed) must still be covered; new types must have SinceVersion > old version.
	// Matches Java's MetaDataEvolutionValidator lines 623-648.
	if err := v.validateIndexRecordTypes(old, new, oldIdx, newIdx, typeRenames); err != nil {
		return err
	}

	// Compare root expressions, modulo allowed field renames. When renames are
	// allowed, rewrite the old expression onto each covered record type's new
	// descriptor; all rewrites must agree. Matches Java's
	// MetaDataEvolutionValidator.validateIndex lines 689-720. The roots are
	// compared by keyExpressionEquals, Java's KeyExpression.equals, not by
	// proto: a field's null interpretation is in its proto and not in
	// FieldKeyExpression.equals, so a root that changes only that is the same
	// root to Java and must be to Go. A literal's carrier is part of the root, as
	// in Java, whose LiteralKeyExpression.equals compares protos.
	expectedExpr := oldIdx.RootExpression
	if v.allowsAnyFieldRenames() {
		renamed, err := v.expectedRenamedIndexExpression(old, new, oldIdx, typeRenames)
		if err != nil {
			return err
		}
		if renamed != nil {
			expectedExpr = renamed
		}
	}
	if !keyExpressionEquals(newIdx.RootExpression, expectedExpr) {
		if keyExpressionEquals(oldIdx.RootExpression, expectedExpr) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("index key expression changed (index=%q)", name),
			}
		}
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("index key expression does not match required (index=%q, after field renames)", name),
		}
	}

	// primaryKeyComponentPositions must not change.
	// Matches Java's MetaDataEvolutionValidator lines 717-737.
	oldHasPositions := oldIdx.HasPrimaryKeyComponentPositions()
	newHasPositions := newIdx.HasPrimaryKeyComponentPositions()
	if oldHasPositions && newHasPositions {
		if !slices.Equal(oldIdx.PrimaryKeyComponentPositions(), newIdx.PrimaryKeyComponentPositions()) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index changes primary key component positions (index=%q)", name),
			}
		}
	} else if oldHasPositions {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("new index drops primary key component positions (index=%q)", name),
		}
	} else if newHasPositions {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("new index adds primary key component positions (index=%q)", name),
		}
	}

	// Compute the mutable remainder once; type-specific validators must not
	// reintroduce options explicitly excluded by the evolution policy.
	changed := computeChangedOptions(oldIdx.Options, newIdx.Options)
	for option := range v.ignoredIndexOptions {
		delete(changed, option)
	}
	if err := ValidateChangedIndexOptions(oldIdx, newIdx, changed); err != nil {
		return err
	}
	return nil
}

// validateIndexRecordTypes checks that the record type scope of an index has not
// lost any old types and that new types have appropriate SinceVersion.
// Matches Java's MetaDataEvolutionValidator lines 623-648.
func (v *MetaDataEvolutionValidator) validateIndexRecordTypes(
	old, new *RecordMetaData,
	oldIdx, newIdx *Index,
	typeRenames map[string]string,
) error {
	// Get old record types for this index, mapped through renames.
	oldTypes := old.RecordTypesForIndex(oldIdx)
	oldRenamedNames := make(map[string]bool, len(oldTypes))
	for _, rt := range oldTypes {
		newName := typeRenames[rt.Name]
		oldRenamedNames[newName] = true
	}

	// Get new record types for this index.
	newTypes := new.RecordTypesForIndex(newIdx)
	newTypeNames := make(map[string]bool, len(newTypes))
	for _, rt := range newTypes {
		newTypeNames[rt.Name] = true
	}

	// Every old type (renamed) must still be present in new index. Java walks
	// a HashSet; Go walks the names sorted, so which of several removed types
	// the message names does not depend on map order.
	for _, renamedName := range slices.Sorted(maps.Keys(oldRenamedNames)) {
		if !newTypeNames[renamedName] {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index removes record type (index=%q, record type=%q)", newIdx.Name, renamedName),
			}
		}
	}

	// New types not in old must have SinceVersion > old metadata version.
	// Unlike new-type admission, expanding an existing index always requires
	// a newer since-version: older records are absent from its persisted entries.
	for _, rt := range newTypes {
		if oldRenamedNames[rt.Name] {
			continue
		}
		if rt.SinceVersion <= old.Version() {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index adds record type that is not newer than old meta-data (index=%q, record type=%q, since=%d, old=%d)",
					newIdx.Name, rt.Name, rt.SinceVersion, old.Version()),
			}
		}
	}

	return nil
}

// computeChangedOptions returns the set of option names whose values differ
// between old and new. An option is "changed" if added, removed, or modified.
func computeChangedOptions(old, new map[string]string) map[string]bool {
	changed := make(map[string]bool)
	for k, v := range old {
		if nv, ok := new[k]; !ok || v != nv {
			changed[k] = true
		}
	}
	for k := range new {
		if _, ok := old[k]; !ok {
			changed[k] = true
		}
	}
	return changed
}

// optionValueOrDefault returns the option value if present, otherwise the default.
func optionValueOrDefault(opts map[string]string, key, defaultValue string) string {
	if v, ok := opts[key]; ok {
		return v
	}
	return defaultValue
}

// ValidateChangedIndexOptions validates only the supplied option names, without
// recomputing differences. Type-specific validators may remove handled names from
// the caller's mutable set before base validation. A nil or empty set is valid.
// Matches Java's IndexValidator.validateChangedOptions(Index, Set).
func ValidateChangedIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	if len(changed) == 0 {
		return nil
	}

	// Type-specific validation: each handler removes options it handles from changed.
	var err error
	switch newIdx.Type {
	case IndexTypeText:
		err = validateTextIndexOptions(oldIdx, newIdx, changed)
	case IndexTypeRank:
		err = validateRankIndexOptions(oldIdx, newIdx, changed)
	case IndexTypePermutedMin, IndexTypePermutedMax:
		err = validatePermutedIndexOptions(oldIdx, newIdx, changed)
	case IndexTypeVector:
		err = validateVectorIndexOptions(oldIdx, newIdx, changed)
	case IndexTypeVectorSPFresh:
		err = validateSPFreshIndexOptions(oldIdx, newIdx, changed)
	case IndexTypeMultidimensional:
		err = validateMultidimensionalIndexOptions(oldIdx, newIdx, changed)
	}
	if err != nil {
		return err
	}

	// Base validation on remaining (unhandled) options.
	return validateBaseIndexOptions(oldIdx, newIdx, changed)
}

// validateTextIndexOptions validates TEXT index option changes.
// Matches Java's TextIndexValidator.validateChangedOptions().
//
// Java walks a HashSet; Go walks the names sorted, so which of a changed
// tokenizer name and version is reported does not depend on map order.
func validateTextIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	for _, opt := range slices.Sorted(maps.Keys(changed)) {
		switch opt {
		case IndexOptionTextAddAggressiveConflictRanges, IndexOptionTextOmitPositions:
			// Always safe to change.
		case IndexOptionTextTokenizerName:
			// Compare resolved names: an explicit default and an omitted option
			// select the same tokenizer, and supplied sets may include unchanged names.
			oldTokenizer, err := getTextTokenizer(oldIdx)
			if err != nil {
				return err
			}
			newTokenizer, err := getTextTokenizer(newIdx)
			if err != nil {
				return err
			}
			if oldTokenizer.Name() != newTokenizer.Name() {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("text tokenizer changed (index=%q)", newIdx.Name),
				}
			}
		case IndexOptionTextTokenizerVersion:
			// The tokenizer version should always go up.
			oldVer, err := getTextTokenizerVersion(oldIdx)
			if err != nil {
				return err
			}
			newVer, err := getTextTokenizerVersion(newIdx)
			if err != nil {
				return err
			}
			if oldVer > newVer {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("text tokenizer version downgraded (index=%q, old=%d, new=%d)",
						newIdx.Name, oldVer, newVer),
				}
			}
		}
	}
	// Remove all text options from changed — handled above.
	delete(changed, IndexOptionTextTokenizerName)
	delete(changed, IndexOptionTextTokenizerVersion)
	delete(changed, IndexOptionTextAddAggressiveConflictRanges)
	delete(changed, IndexOptionTextOmitPositions)
	return nil
}

// validateRankIndexOptions validates RANK index option changes.
// Structural options (nLevels, hashFunction, countDuplicates) cannot change
// effective value without a rebuild. Matches Java's
// RankIndexValidator.validateChangedOptions (RankIndexMaintainerFactory.java:
// 75-100): it compares the EFFECTIVE configuration, so a change from an
// unspecified option to its default (or back) is admitted, and its messages
// are Java's ("rank count duplicate changed" is Java's spelling).
//
// Both configurations are read first, the old one first, as Java's are, so an
// option neither parser accepts (an unknown hash function name, a level count
// that is not an int or out of range) refuses the change with the parser's
// error whichever option changed.
func validateRankIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	oldConfig, err := parseRankedSetConfig(oldIdx)
	if err != nil {
		return err
	}
	newConfig, err := parseRankedSetConfig(newIdx)
	if err != nil {
		return err
	}
	for _, o := range []struct {
		key, message string
		same         bool
	}{
		{IndexOptionRankNLevels, "rank levels changed", oldConfig.NLevels == newConfig.NLevels},
		{IndexOptionRankHashFunction, "rank hash function changed", oldConfig.HashFunctionName == newConfig.HashFunctionName},
		{IndexOptionRankCountDuplicates, "rank count duplicate changed", oldConfig.CountDuplicates == newConfig.CountDuplicates},
	} {
		if !changed[o.key] {
			continue
		}
		if !o.same {
			return &MetaDataEvolutionError{Message: fmt.Sprintf("%s (index=%q)", o.message, newIdx.Name)}
		}
		delete(changed, o.key)
	}
	return nil
}

// validatePermutedIndexOptions validates PERMUTED_MIN/PERMUTED_MAX option changes.
// The permuted size is structural and cannot change.
// Matches Java's PermutedMinMaxIndexValidator.validateChangedOptions().
func validatePermutedIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	if changed[IndexOptionPermutedSize] {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("permuted size changed (index=%q)", newIdx.Name),
		}
	}
	return nil
}

// validateSPFreshIndexOptions enforces RFC-094 §10: every structural SPFresh
// option is immutable for an existing index — the lifecycle invariants
// (topology, posting sizes, closure replication, single-tx split budget) are
// derived from them, and a changed value would silently invalidate data
// written under the old one (the PR #278 lesson: immutability is what makes
// config-derived invariants sound). There are deliberately NO runtime-mutable
// SPFresh options: query/maintenance knobs are never stored.
func validateSPFreshIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	structural := []string{
		IndexOptionSPFreshNumDimensions,
		IndexOptionSPFreshMetric,
		IndexOptionSPFreshLmax,
		IndexOptionSPFreshLminRatio,
		IndexOptionSPFreshCellTarget,
		IndexOptionSPFreshCellMax,
		IndexOptionSPFreshReplication,
		IndexOptionSPFreshAlpha,
		IndexOptionSPFreshKn,
		IndexOptionSPFreshCooldownSec,
		IndexOptionSPFreshRaBitQNumExBits,
		IndexOptionSPFreshSidecar,
	}
	for _, key := range structural {
		if !changed[key] {
			continue
		}
		oldVal := optionValueOrDefault(oldIdx.Options, key, "")
		newVal := optionValueOrDefault(newIdx.Options, key, "")
		if oldVal != newVal {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("SPFresh option %q changed for index %q", key, newIdx.Name),
			}
		}
		delete(changed, key)
	}
	return nil
}

// validateVectorIndexOptions validates VECTOR (HNSW) index option changes.
// Structural options (metric, dimensions, graph parameters) cannot change.
// Runtime-only options (concurrency limits, stats) are safe to change.
// Matches Java's VectorIndexValidator.validateChangedOptions().
func validateVectorIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	// Structural options: disallow EFFECTIVE value changes, as Java's
	// VectorIndexOptionsHelper.disallowChange compares the parsed and defaulted
	// values (VectorIndexOptionsHelper.java:120-147), so an option set to its
	// default beside one left unset is no change; with Java's message.
	oldOpts, err := readHNSWOptions(oldIdx, true)
	if err != nil {
		return err
	}
	newOpts, err := readHNSWOptions(newIdx, true)
	if err != nil {
		return err
	}
	oldConfig, newConfig := oldOpts.config, newOpts.config
	for _, o := range []struct {
		key  string
		same bool
	}{
		{IndexOptionVectorMetric, oldOpts.metric == newOpts.metric},
		{IndexOptionVectorNumDimensions, oldConfig.NumDimensions == newConfig.NumDimensions},
		{IndexOptionHNSWUseInlining, oldConfig.UseInlining == newConfig.UseInlining},
		{IndexOptionHNSWM, oldConfig.M == newConfig.M},
		{IndexOptionHNSWMMax, oldConfig.MMax == newConfig.MMax},
		{IndexOptionHNSWMMax0, oldConfig.MMax0 == newConfig.MMax0},
		{IndexOptionHNSWEfConstruction, oldConfig.EfConstruction == newConfig.EfConstruction},
		{IndexOptionHNSWEfRepair, oldConfig.EfRepair == newConfig.EfRepair},
		{IndexOptionVectorExtendCandidates, oldConfig.ExtendCandidates == newConfig.ExtendCandidates},
		{IndexOptionVectorKeepPrunedConnections, oldConfig.KeepPrunedConnections == newConfig.KeepPrunedConnections},
		{IndexOptionHNSWUseRaBitQ, oldOpts.useRaBitQ == newOpts.useRaBitQ},
		{IndexOptionHNSWRaBitQNumExBits, oldOpts.raBitQNumExBits == newOpts.raBitQNumExBits},
	} {
		// A key covers its alias (VectorOptionKey's names), as Java's
		// validateChangedOptions reads it: disallowChange visits the key's
		// names in order, its hnsw* name then its alias, and a refusal names
		// the first of them that changed.
		alias := hnswOptionAliases[o.key]
		name := o.key
		switch {
		case changed[o.key]:
		case alias != "" && changed[alias]:
			name = alias
		default:
			continue
		}
		if !o.same {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("attempted to change immutable vector index option (index=%q, option=%q)", newIdx.Name, name),
			}
		}
		delete(changed, o.key)
		delete(changed, alias)
	}

	// Runtime-only options: always safe to change, just remove from changed.
	runtime := []string{
		IndexOptionHNSWSampleVectorStatsProbability,
		IndexOptionHNSWMaintainStatsProbability,
		IndexOptionHNSWStatsThreshold,
		IndexOptionHNSWMaxNumConcurrentNodeFetches,
		IndexOptionHNSWMaxNumConcurrentNeighborhoodFetches,
		IndexOptionHNSWMaxNumConcurrentDeleteFromLayer,
	}
	for _, key := range runtime {
		delete(changed, key)
		delete(changed, hnswOptionAliases[key])
	}

	return nil
}

// validateMultidimensionalIndexOptions validates MULTIDIMENSIONAL (R-tree) option changes.
// Structural options cannot change effective value without a rebuild.
// Matches Java's MultidimensionalIndexValidator.validateChangedOptions().
func validateMultidimensionalIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	// The EFFECTIVE configuration, as Java compares it
	// (MultidimensionalIndexMaintainerFactory.java:143-193), so an unspecified
	// option and its default are the same; with Java's messages, including its
	// "rtree minM changed" for a changed maxM.
	oldConfig, err := parseRTreeConfig(oldIdx, 0)
	if err != nil {
		return err
	}
	newConfig, err := parseRTreeConfig(newIdx, 0)
	if err != nil {
		return err
	}
	for _, o := range []struct {
		key, message string
		same         bool
	}{
		{IndexOptionRTreeMinM, "rtree minM changed", oldConfig.MinM == newConfig.MinM},
		{IndexOptionRTreeMaxM, "rtree minM changed", oldConfig.MaxM == newConfig.MaxM},
		{IndexOptionRTreeSplitS, "rtree splitS changed", oldConfig.SplitS == newConfig.SplitS},
		{IndexOptionRTreeStorage, "rtree storage changed", oldConfig.Storage == newConfig.Storage},
		{IndexOptionRTreeStoreHilbertValues, "rtree store Hilbert values changed", oldConfig.StoreHilbertValues == newConfig.StoreHilbertValues},
		{IndexOptionRTreeUseNodeSlotIndex, "rtree use node slot index changed", oldConfig.UseNodeSlotIndex == newConfig.UseNodeSlotIndex},
	} {
		if !changed[o.key] {
			continue
		}
		if !o.same {
			return &MetaDataEvolutionError{Message: fmt.Sprintf("%s (index=%q)", o.message, newIdx.Name)}
		}
		delete(changed, o.key)
	}
	return nil
}

// validateBaseIndexOptions validates remaining option changes after type-specific
// validation. Handles options common to all index types.
// Matches Java's IndexValidator.validateChangedOptions().
func validateBaseIndexOptions(oldIdx, newIdx *Index, changed map[string]bool) error {
	// Java walks a HashSet; Go walks the names sorted, so which of several
	// changed options is named does not depend on map order.
	for _, opt := range slices.Sorted(maps.Keys(changed)) {
		// "replacedBy*" options are always safe to change.
		if strings.HasPrefix(opt, IndexOptionReplacedByPrefix) {
			continue
		}
		// "allowedForQuery" is runtime-only, safe to change.
		if opt == IndexOptionAllowedForQuery {
			continue
		}
		// "unique": dropping uniqueness is allowed, adding is not.
		if opt == IndexOptionUnique {
			oldUnique, newUnique := oldIdx.IsUnique(), newIdx.IsUnique()
			if !oldUnique && newUnique {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("index adds uniqueness constraint (index=%q)", newIdx.Name),
				}
			}
			// Dropping unique (was true, now false or absent) is allowed.
			continue
		}
		// Any other option: reject.
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("index option changed (index=%q, option=%q, old=%q, new=%q)", newIdx.Name, opt, oldIdx.Options[opt], newIdx.Options[opt]),
		}
	}
	return nil
}

type descriptorPair struct {
	old, new protoreflect.MessageDescriptor
}

func (v *MetaDataEvolutionValidator) validateMessageDescriptor(
	oldDesc, newDesc protoreflect.MessageDescriptor,
	seen map[descriptorPair]bool,
) error {
	if oldDesc == newDesc {
		return nil
	}
	if oldDesc == nil || newDesc == nil {
		return &MetaDataEvolutionError{Message: "message descriptor presence changed"}
	}
	pair := descriptorPair{old: oldDesc, new: newDesc}
	if seen[pair] {
		return nil
	}
	seen[pair] = true

	// Check proto syntax/edition hasn't changed.
	// Matches Java's MetaDataEvolutionValidator.validateProtoSyntax() (lines 255-260).
	if err := validateProtoSyntax(oldDesc, newDesc); err != nil {
		return err
	}

	// Check all old fields still exist
	oldFields := oldDesc.Fields()
	newFields := newDesc.Fields()
	for i := 0; i < oldFields.Len(); i++ {
		oldField := oldFields.Get(i)
		newField := newFields.ByNumber(oldField.Number())
		if newField == nil {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("field removed from message descriptor (field=%q, number=%d, message=%q)",
					oldField.Name(), oldField.Number(), oldDesc.FullName()),
			}
		}

		if err := v.validateField(oldField, newField, oldDesc.FullName(), seen); err != nil {
			return err
		}
	}

	// Check for new required fields (proto2 only)
	for i := 0; i < newFields.Len(); i++ {
		newField := newFields.Get(i)
		if oldFields.ByNumber(newField.Number()) != nil {
			continue // Existing field
		}
		if newField.Cardinality() == protoreflect.Required {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("required field added to record type (field=%q, message=%q)",
					newField.Name(), newDesc.FullName()),
			}
		}
	}

	return nil
}

func (v *MetaDataEvolutionValidator) validateField(
	oldField, newField protoreflect.FieldDescriptor,
	msgName protoreflect.FullName,
	seen map[descriptorPair]bool,
) error {
	oldDeprecated := fieldDeprecated(oldField)
	newDeprecated := fieldDeprecated(newField)

	// Name check. A rename (same number, new name) is allowed only if we allow all
	// field renames, or we allow deprecated-field renames and either side is deprecated
	// (so a name change is fine both when deprecating and un-deprecating). Matches Java's
	// MetaDataEvolutionValidator.validateField lines 282-294.
	if string(oldField.Name()) != string(newField.Name()) {
		if !(v.allowFieldRenames || (v.allowDeprecatedFieldRenames && (oldDeprecated || newDeprecated))) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("field renamed (old=%q, new=%q, message=%q)",
					oldField.Name(), newField.Name(), msgName),
			}
		}
	}

	// A field that was deprecated must stay deprecated unless explicitly allowed.
	// Matches Java's MetaDataEvolutionValidator.validateField line 296.
	if !v.allowUndeprecatingFields && oldDeprecated && !newDeprecated {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("field is no longer deprecated (field=%q, message=%q)", oldField.Name(), msgName),
		}
	}

	// Then Java's order (MetaDataEvolutionValidator.java:300-328): the type,
	// then the label, then the enum values and the message. The type check
	// allows only int32 to int64 and sint32 to sint64 (validateTypeChange).
	if oldField.Kind() != newField.Kind() && !isSafeTypePromotion(oldField.Kind(), newField.Kind()) {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("field type changed (field=%q, message=%q, old=%s, new=%s)",
				oldField.Name(), msgName, oldField.Kind(), newField.Kind()),
		}
	}

	// The label, as Java checks it: a required field must stay required and a
	// repeated one repeated, and a field must keep whether it tracks presence.
	// An optional field made required keeps its presence and is admitted, as
	// Java admits it; Go refused every change of cardinality.
	switch {
	case oldField.Cardinality() == protoreflect.Required && newField.Cardinality() != protoreflect.Required:
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("required field is no longer required (field=%q, message=%q, now %s)",
				oldField.Name(), msgName, cardinalityString(newField.Cardinality())),
		}
	case oldField.Cardinality() == protoreflect.Repeated && newField.Cardinality() != protoreflect.Repeated:
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("repeated field is no longer repeated (field=%q, message=%q, now %s)",
				oldField.Name(), msgName, cardinalityString(newField.Cardinality())),
		}
	case oldField.HasPresence() != newField.HasPresence():
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("field changed whether default values are stored if set explicitly (field=%q, message=%q)",
				oldField.Name(), msgName),
		}
	}

	// Enum validation
	if oldField.Kind() == protoreflect.EnumKind && newField.Kind() == protoreflect.EnumKind {
		if err := v.validateEnum(oldField.Enum(), newField.Enum()); err != nil {
			return err
		}
	}

	// Recurse into nested messages
	if (oldField.Kind() == protoreflect.MessageKind || oldField.Kind() == protoreflect.GroupKind) && newField.Message() != nil {
		return v.validateMessageDescriptor(oldField.Message(), newField.Message(), seen)
	}

	return nil
}

func (v *MetaDataEvolutionValidator) validateEnum(
	oldEnum, newEnum protoreflect.EnumDescriptor,
) error {
	oldValues := oldEnum.Values()
	newValues := newEnum.Values()

	for i := 0; i < oldValues.Len(); i++ {
		oldVal := oldValues.Get(i)
		newVal := newValues.ByNumber(oldVal.Number())
		if newVal == nil {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("enum removes value (enum=%q, value=%q, number=%d)",
					oldEnum.FullName(), oldVal.Name(), oldVal.Number()),
			}
		}
	}
	return nil
}

// isSafeTypePromotion checks if a field type change is safe (widening only).
// Matches Java's MetaDataEvolutionValidator.validateTypeChange().
func isSafeTypePromotion(old, new protoreflect.Kind) bool {
	// int32 → int64 is safe
	if old == protoreflect.Int32Kind && new == protoreflect.Int64Kind {
		return true
	}
	// sint32 → sint64 is safe
	if old == protoreflect.Sint32Kind && new == protoreflect.Sint64Kind {
		return true
	}
	return false
}

func cardinalityString(c protoreflect.Cardinality) string {
	switch c {
	case protoreflect.Required:
		return "required"
	case protoreflect.Optional:
		return "optional"
	case protoreflect.Repeated:
		return "repeated"
	default:
		return c.String()
	}
}

// validateProtoSyntax checks that the old and new message descriptors use the same
// proto syntax and edition. Matches Java's MetaDataEvolutionValidator.validateProtoSyntax().
func validateProtoSyntax(oldDesc, newDesc protoreflect.MessageDescriptor) error {
	oldFile := protodesc.ToFileDescriptorProto(oldDesc.ParentFile())
	newFile := protodesc.ToFileDescriptorProto(newDesc.ParentFile())
	if oldFile.GetSyntax() != newFile.GetSyntax() || oldFile.GetEdition() != newFile.GetEdition() {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("message descriptor proto syntax changed (record type=%q)", oldDesc.Name()),
		}
	}
	return nil
}

// ValidateEvolution is a convenience function using the default (strictest) validator.
func ValidateEvolution(oldMetaData, newMetaData *RecordMetaData) error {
	return DefaultMetaDataEvolutionValidator().Validate(oldMetaData, newMetaData)
}
