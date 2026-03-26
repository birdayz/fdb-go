package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
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

func (b *MetaDataEvolutionValidatorBuilder) Build() *MetaDataEvolutionValidator {
	v := b.v
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

	// 2b. Union descriptor validation (splits, merges, removals)
	if err := v.validateUnion(oldMetaData, newMetaData); err != nil {
		return err
	}

	// 3. Record type validation
	if err := v.validateRecordTypes(oldMetaData, newMetaData); err != nil {
		return err
	}

	// 4. Index validation
	if err := v.validateIndexes(oldMetaData, newMetaData); err != nil {
		return err
	}

	// 5. Former index validation
	if err := v.validateFormerIndexes(oldMetaData, newMetaData); err != nil {
		return err
	}

	// 6. Message descriptor validation
	if err := v.validateMessages(oldMetaData, newMetaData); err != nil {
		return err
	}

	return nil
}

func (v *MetaDataEvolutionValidator) validateVersion(old, new *RecordMetaData) error {
	if !v.allowNoVersionChange && new.Version() <= old.Version() {
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
	oldUnion := getUnionDescriptor(old)
	newUnion := getUnionDescriptor(new)
	if oldUnion == nil || newUnion == nil {
		return nil // No union descriptor — skip validation
	}
	if oldUnion.FullName() == newUnion.FullName() && oldUnion == newUnion {
		return nil // Same descriptor — no changes
	}

	// Track bidirectional mapping: oldMsgFullName ↔ newMsgFullName
	// Forward: old message → new message
	// Reverse: new message → old message
	oldToNew := make(map[protoreflect.FullName]protoreflect.FullName)
	newToOld := make(map[protoreflect.FullName]protoreflect.FullName)

	oldFields := oldUnion.Fields()
	newFields := newUnion.Fields()

	for i := 0; i < oldFields.Len(); i++ {
		oldField := oldFields.Get(i)
		if oldField.Kind() != protoreflect.MessageKind {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("field in union is not a message type: %s", oldField.Name()),
			}
		}

		// Find corresponding field in new union by field number
		newField := newFields.ByNumber(oldField.Number())
		if newField == nil {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type removed from union: %s", oldField.Message().Name()),
			}
		}
		if newField.Kind() != protoreflect.MessageKind {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("field in new union is not a message type: %s", newField.Name()),
			}
		}

		oldMsgName := oldField.Message().FullName()
		newMsgName := newField.Message().FullName()

		// Check for split: old message already mapped to a different new message
		if prev, ok := oldToNew[oldMsgName]; ok && prev != newMsgName {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type corresponds to multiple types in new meta-data: %s",
					oldField.Message().Name()),
			}
		}

		// Check for merge: new message already mapped from a different old message
		if prev, ok := newToOld[newMsgName]; ok && prev != oldMsgName {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type corresponds to multiple types in old meta-data: %s",
					newField.Message().Name()),
			}
		}

		oldToNew[oldMsgName] = newMsgName
		newToOld[newMsgName] = oldMsgName
	}

	return nil
}

// getUnionDescriptor returns the UnionDescriptor message from the metadata's file descriptor.
func getUnionDescriptor(m *RecordMetaData) protoreflect.MessageDescriptor {
	if m.fileDescriptor == nil {
		return nil
	}
	return m.fileDescriptor.Messages().ByName("UnionDescriptor")
}

func (v *MetaDataEvolutionValidator) validateRecordTypes(old, new *RecordMetaData) error {
	for name, oldRT := range old.RecordTypes() {
		newRT := new.GetRecordType(name)
		if newRT == nil {
			// Check if it was renamed
			if v.disallowTypeRenames {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("record type %q removed from meta-data", name),
				}
			}
			// Try to find by type key
			found := false
			for _, nrt := range new.RecordTypes() {
				if normalizeSubspaceKey(nrt.GetRecordTypeKey()) == normalizeSubspaceKey(oldRT.GetRecordTypeKey()) {
					found = true
					newRT = nrt
					break
				}
			}
			if !found {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("record type %q removed from meta-data", name),
				}
			}
		}

		// Primary key must not change
		if err := v.comparePrimaryKeys(name, oldRT, newRT); err != nil {
			return err
		}

		// Record type key must not change
		if normalizeSubspaceKey(oldRT.GetRecordTypeKey()) != normalizeSubspaceKey(newRT.GetRecordTypeKey()) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type key changed for %q (old=%v, new=%v)",
					name, oldRT.GetRecordTypeKey(), newRT.GetRecordTypeKey()),
			}
		}

		// SinceVersion must not change on existing record types.
		// Matches Java's MetaDataEvolutionValidator line 361.
		if oldRT.SinceVersion != newRT.SinceVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("record type %q since version changed (old=%d, new=%d)",
					name, oldRT.SinceVersion, newRT.SinceVersion),
			}
		}
	}

	// Build set of old record type names, accounting for renames via type key.
	olderNames := make(map[string]bool, len(old.RecordTypes()))
	for name, oldRT := range old.RecordTypes() {
		// If this type was renamed, map old name to the new name
		renamed := name
		for newName, newRT := range new.RecordTypes() {
			if normalizeSubspaceKey(newRT.GetRecordTypeKey()) == normalizeSubspaceKey(oldRT.GetRecordTypeKey()) {
				renamed = newName
				break
			}
		}
		olderNames[renamed] = true
	}

	// Validate new record types have SinceVersion set.
	// Matches Java's MetaDataEvolutionValidator lines 365-380.
	for name, newRT := range new.RecordTypes() {
		if olderNames[name] {
			continue // Existing type, already validated above
		}
		if newRT.SinceVersion == 0 {
			if !v.allowNoSinceVersion {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("new record type %q is missing since version", name),
				}
			}
		} else if newRT.SinceVersion <= old.Version() {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new record type %q has since version older than old meta-data (since=%d, old=%d)",
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
			Message: fmt.Sprintf("record type %q primary key changed", name),
		}
	}

	// Compare by proto serialization for deep equality
	oldProto := oldPK.ToKeyExpression()
	newProto := newPK.ToKeyExpression()
	if !proto.Equal(oldProto, newProto) {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("record type %q primary key changed", name),
		}
	}
	return nil
}

func (v *MetaDataEvolutionValidator) validateIndexes(old, new *RecordMetaData) error {
	newFormerIndexMap := buildFormerIndexMap(new.GetFormerIndexes())

	for name, oldIdx := range old.GetAllIndexes() {
		newIdx := new.GetIndex(name)
		if newIdx == nil {
			// Must have become a FormerIndex
			subKey := subspaceKeyString(oldIdx.SubspaceTupleKey())
			if _, ok := newFormerIndexMap[subKey]; !ok {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("index %q missing in new meta-data (not replaced by former index)", name),
				}
			}
			continue
		}

		// Validate unchanged properties
		if oldIdx.Name != newIdx.Name {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("index name changed (old=%q, new=%q)", oldIdx.Name, newIdx.Name),
			}
		}
		if oldIdx.AddedVersion != newIdx.AddedVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index %q added version does not match old index added version (old=%d, new=%d)",
					name, oldIdx.AddedVersion, newIdx.AddedVersion),
			}
		}
		if !v.allowIndexRebuilds && oldIdx.LastModifiedVersion != newIdx.LastModifiedVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("last modified version of index %q changed (old=%d, new=%d)",
					name, oldIdx.LastModifiedVersion, newIdx.LastModifiedVersion),
			}
		}
		if oldIdx.LastModifiedVersion > newIdx.LastModifiedVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("old index %q has last-modified version newer than new index (old=%d, new=%d)",
					name, oldIdx.LastModifiedVersion, newIdx.LastModifiedVersion),
			}
		}

		// When allowIndexRebuilds is true and lastModifiedVersion changed,
		// skip type/expression checks — the index will be rebuilt.
		// Matches Java's MetaDataEvolutionValidator.validateIndex() lines 606-610.
		if v.allowIndexRebuilds && oldIdx.LastModifiedVersion < newIdx.LastModifiedVersion {
			continue
		}

		if oldIdx.Type != newIdx.Type {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("index %q type changed (old=%q, new=%q)", name, oldIdx.Type, newIdx.Type),
			}
		}

		// Compare root expressions
		oldExpr := oldIdx.RootExpression.ToKeyExpression()
		newExpr := newIdx.RootExpression.ToKeyExpression()
		if !proto.Equal(oldExpr, newExpr) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("index %q key expression changed", name),
			}
		}

		// primaryKeyComponentPositions must not change.
		// Matches Java's MetaDataEvolutionValidator lines 649-667.
		oldHasPositions := oldIdx.HasPrimaryKeyComponentPositions()
		newHasPositions := newIdx.HasPrimaryKeyComponentPositions()
		if oldHasPositions && !newHasPositions {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index %q drops primary key component positions", name),
			}
		}
		if !oldHasPositions && newHasPositions {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new index %q adds primary key component positions", name),
			}
		}
		if oldHasPositions && newHasPositions {
			oldPos := oldIdx.PrimaryKeyComponentPositions()
			newPos := newIdx.PrimaryKeyComponentPositions()
			if len(oldPos) != len(newPos) {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("new index %q changes primary key component positions", name),
				}
			}
			for i := range oldPos {
				if oldPos[i] != newPos[i] {
					return &MetaDataEvolutionError{
						Message: fmt.Sprintf("new index %q changes primary key component positions", name),
					}
				}
			}
		}
	}

	// New indexes must have version > old metadata version
	for name, newIdx := range new.GetAllIndexes() {
		if old.GetIndex(name) == nil {
			if newIdx.LastModifiedVersion <= old.Version() {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("new index %q has version that is not newer than the old meta-data version (index=%d, old=%d)",
						name, newIdx.LastModifiedVersion, old.Version()),
				}
			}
		}
	}

	return nil
}

func (v *MetaDataEvolutionValidator) validateFormerIndexes(old, new *RecordMetaData) error {
	oldFormerMap := buildFormerIndexMap(old.GetFormerIndexes())

	// Old FormerIndexes must remain
	for key, oldFormer := range oldFormerMap {
		newFormerMap := buildFormerIndexMap(new.GetFormerIndexes())
		newFormer, ok := newFormerMap[key]
		if !ok {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("former index (subspace key=%s) removed from meta-data", key),
			}
		}

		// Versions must not change
		if oldFormer.RemovedVersion != newFormer.RemovedVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("removed version of former index (subspace key=%s) differs from prior version (old=%d, new=%d)",
					key, oldFormer.RemovedVersion, newFormer.RemovedVersion),
			}
		}
		if oldFormer.AddedVersion != newFormer.AddedVersion {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("added version of former index (subspace key=%s) differs from prior version (old=%d, new=%d)",
					key, oldFormer.AddedVersion, newFormer.AddedVersion),
			}
		}
		if !v.allowMissingFormerIndexNames && oldFormer.FormerName != newFormer.FormerName {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("name of former index (subspace key=%s) differs from prior version (old=%q, new=%q)",
					key, oldFormer.FormerName, newFormer.FormerName),
			}
		}
	}

	// New FormerIndexes created from dropped indexes
	newFormerMap := buildFormerIndexMap(new.GetFormerIndexes())
	for key, newFormer := range newFormerMap {
		if _, ok := oldFormerMap[key]; ok {
			continue // Already validated above
		}

		// Check that the removed version is > old metadata version
		if newFormer.RemovedVersion <= old.Version() {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("new former index (subspace key=%s) has removed version that is not newer than the old meta-data version (removed=%d, old=%d)",
					key, newFormer.RemovedVersion, old.Version()),
			}
		}

		// Check against the old index if it existed
		oldIdx := old.GetIndex(newFormer.FormerName)
		if oldIdx != nil {
			if !v.allowMissingFormerIndexNames && newFormer.FormerName != oldIdx.Name {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("former index has different name than old index (former=%q, old=%q)",
						newFormer.FormerName, oldIdx.Name),
				}
			}
			// Unconditional check: former's addedVersion must NOT be > old index's addedVersion.
			// Matches Java line 522: unconditional check before the conditional one.
			if newFormer.AddedVersion > oldIdx.AddedVersion {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("former index reports added version newer than old index (former=%d, old=%d)",
						newFormer.AddedVersion, oldIdx.AddedVersion),
				}
			}
			// Conditional check: when !allowOlder, former's addedVersion must equal old index's.
			// Matches Java line 528: if (!allowOlder && newFormer.addedVersion != oldIndex.addedVersion)
			if !v.allowOlderFormerIndexAddedVersion && newFormer.AddedVersion != oldIdx.AddedVersion {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("former index reports added version different from old index (former=%d, old=%d)",
						newFormer.AddedVersion, oldIdx.AddedVersion),
				}
			}
			if newFormer.RemovedVersion <= oldIdx.LastModifiedVersion {
				return &MetaDataEvolutionError{
					Message: fmt.Sprintf("former index removed before old index's last modification (removed=%d, lastModified=%d)",
						newFormer.RemovedVersion, oldIdx.LastModifiedVersion),
				}
			}
		}
	}

	return nil
}

func (v *MetaDataEvolutionValidator) validateMessages(old, new *RecordMetaData) error {
	seen := make(map[string]bool)
	for name, oldRT := range old.RecordTypes() {
		newRT := new.GetRecordType(name)
		if newRT == nil {
			// Already validated in validateRecordTypes
			continue
		}
		if err := v.validateMessageDescriptor(oldRT.Descriptor, newRT.Descriptor, seen); err != nil {
			return err
		}
	}
	return nil
}

func (v *MetaDataEvolutionValidator) validateMessageDescriptor(
	oldDesc, newDesc protoreflect.MessageDescriptor,
	seen map[string]bool,
) error {
	fullName := string(oldDesc.FullName())
	if seen[fullName] {
		return nil // Break cycles
	}
	seen[fullName] = true

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
				Message: fmt.Sprintf("field %q (number %d) removed from message %q",
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
				Message: fmt.Sprintf("required field %q added to message %q",
					newField.Name(), newDesc.FullName()),
			}
		}
	}

	return nil
}

func (v *MetaDataEvolutionValidator) validateField(
	oldField, newField protoreflect.FieldDescriptor,
	msgName protoreflect.FullName,
	seen map[string]bool,
) error {
	// Name check
	if string(oldField.Name()) != string(newField.Name()) {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("field %q renamed to %q in message %q",
				oldField.Name(), newField.Name(), msgName),
		}
	}

	// Label/cardinality check
	if oldField.Cardinality() != newField.Cardinality() {
		oldLabel := cardinalityString(oldField.Cardinality())
		newLabel := cardinalityString(newField.Cardinality())
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("%s field %q is no longer %s in message %q (now %s)",
				oldLabel, oldField.Name(), oldLabel, msgName, newLabel),
		}
	}

	// Presence tracking check — field must not change whether it tracks explicit set vs default.
	// Matches Java's MetaDataEvolutionValidator line 280-283.
	if oldField.HasPresence() != newField.HasPresence() {
		return &MetaDataEvolutionError{
			Message: fmt.Sprintf("field %q changed whether default values are stored if set explicitly in message %q",
				oldField.Name(), msgName),
		}
	}

	// Type check (allow safe promotions: int32→int64, sint32→sint64)
	if oldField.Kind() != newField.Kind() {
		if !isSafeTypePromotion(oldField.Kind(), newField.Kind()) {
			return &MetaDataEvolutionError{
				Message: fmt.Sprintf("field %q type changed in message %q (old=%s, new=%s)",
					oldField.Name(), msgName, oldField.Kind(), newField.Kind()),
			}
		}
	}

	// Recurse into nested messages
	if oldField.Kind() == protoreflect.MessageKind && newField.Kind() == protoreflect.MessageKind {
		return v.validateMessageDescriptor(oldField.Message(), newField.Message(), seen)
	}

	// Enum validation
	if oldField.Kind() == protoreflect.EnumKind && newField.Kind() == protoreflect.EnumKind {
		return v.validateEnum(oldField.Enum(), newField.Enum())
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
				Message: fmt.Sprintf("enum %q removes value %q (number %d)",
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
			Message: fmt.Sprintf("message descriptor %q proto syntax changed", oldDesc.Name()),
		}
	}
	return nil
}

// subspaceKeyString returns a type-safe string representation of a subspace key
// for use as a map key. Normalizes integer types to int64 first so that
// int(42), int32(42), and int64(42) all produce the same string.
// Uses %T:%v format so that string("5") != int64(5). Fixes bug 19.
func subspaceKeyString(key any) string {
	normalized := normalizeSubspaceKey(key)
	return fmt.Sprintf("%T:%v", normalized, normalized)
}

func buildFormerIndexMap(indexes []*FormerIndex) map[string]*FormerIndex {
	m := make(map[string]*FormerIndex, len(indexes))
	for _, fi := range indexes {
		m[subspaceKeyString(fi.SubspaceKey)] = fi
	}
	return m
}

// ValidateEvolution is a convenience function using the default (strictest) validator.
func ValidateEvolution(oldMetaData, newMetaData *RecordMetaData) error {
	return DefaultMetaDataEvolutionValidator().Validate(oldMetaData, newMetaData)
}
