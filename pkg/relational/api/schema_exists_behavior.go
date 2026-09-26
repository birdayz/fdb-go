package api

import "fmt"

// SchemaExistsBehavior is what StoreCatalog.SaveSchema does when a schema is
// already stored at (databaseID, schemaName). Mirrors Java's
// com.apple.foundationdb.relational.api.catalog.SchemaExistsBehavior: the
// same four values, the same ShouldWrite contract and the same messages, all
// SCHEMA_ALREADY_EXISTS (42F06).
type SchemaExistsBehavior int

const (
	// SchemaExistsError always refuses: "Schema <db>/<name> already exists."
	SchemaExistsError SchemaExistsBehavior = iota
	// SchemaExistsErrorIfDifferent is a no-op when the stored schema names the
	// same template name and version, and refuses otherwise.
	SchemaExistsErrorIfDifferent
	// SchemaExistsDoNothing is a no-op, with no comparison.
	SchemaExistsDoNothing
	// SchemaExistsUpgrade refuses a different template name or a lower
	// version, is a no-op on an equal version and writes on a greater one.
	SchemaExistsUpgrade
)

// String is the Java constant's name.
func (b SchemaExistsBehavior) String() string {
	switch b {
	case SchemaExistsError:
		return "ERROR"
	case SchemaExistsErrorIfDifferent:
		return "ERROR_IF_DIFFERENT"
	case SchemaExistsDoNothing:
		return "DO_NOTHING"
	case SchemaExistsUpgrade:
		return "UPGRADE"
	}
	return fmt.Sprintf("SchemaExistsBehavior(%d)", int(b))
}

// ShouldWrite decides a save over an existing schema, as Java's shouldWrite:
// true overwrites the stored row with newSchema, false leaves it untouched
// (the save writes nothing), and an error refuses the save.
func (b SchemaExistsBehavior) ShouldWrite(newSchema, existing Schema) (bool, error) {
	newT, oldT := newSchema.SchemaTemplate(), existing.SchemaTemplate()
	switch b {
	case SchemaExistsError:
		return false, NewErrorf(ErrCodeSchemaAlreadyExists, "Schema %s/%s already exists.",
			newSchema.DatabaseName(), newSchema.MetadataName())
	case SchemaExistsErrorIfDifferent:
		if newT.MetadataName() == oldT.MetadataName() && newT.Version() == oldT.Version() {
			return false, nil
		}
		return false, NewErrorf(ErrCodeSchemaAlreadyExists,
			"Schema %s/%s already exists with a different template (%s@%d vs %s@%d).",
			newSchema.DatabaseName(), newSchema.MetadataName(),
			oldT.MetadataName(), oldT.Version(), newT.MetadataName(), newT.Version())
	case SchemaExistsDoNothing:
		return false, nil
	case SchemaExistsUpgrade:
		if oldT.MetadataName() != newT.MetadataName() {
			return false, NewErrorf(ErrCodeSchemaAlreadyExists,
				"Cannot upgrade schema %s/%s: existing template %s does not match new template %s.",
				newSchema.DatabaseName(), newSchema.MetadataName(), oldT.MetadataName(), newT.MetadataName())
		}
		if newT.Version() < oldT.Version() {
			return false, NewErrorf(ErrCodeSchemaAlreadyExists,
				"Cannot upgrade schema %s/%s: new template version %d is lower than existing version %d.",
				newSchema.DatabaseName(), newSchema.MetadataName(), newT.Version(), oldT.Version())
		}
		return newT.Version() > oldT.Version(), nil
	}
	return false, NewErrorf(ErrCodeInternalError, "unknown schema exists behavior %d", int(b))
}
