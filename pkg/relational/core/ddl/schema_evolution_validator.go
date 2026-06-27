package ddl

import (
	"fdb.dev/pkg/relational/api"
)

// RelationalSchemaEvolutionValidator validates that a new schema template version
// is backward-compatible with the current version stored in the catalog.
//
// Rules (conservative by default — matches Java's relational evolution semantics):
//   - Tables may be added; tables may NOT be removed.
//   - For each existing table: columns may be added; columns may NOT be removed.
//   - Column data types must not change (type widening is not yet implemented).
//   - Primary key column order must not change (PKs are the first N columns in
//     the StructDataType; we compare column names in declared order).
type RelationalSchemaEvolutionValidator struct{}

// NewRelationalSchemaEvolutionValidator returns a validator with default (strict) settings.
func NewRelationalSchemaEvolutionValidator() *RelationalSchemaEvolutionValidator {
	return &RelationalSchemaEvolutionValidator{}
}

// Validate checks that newTemplate is backward-compatible with oldTemplate.
// Returns a non-nil error describing the first violation found.
func (v *RelationalSchemaEvolutionValidator) Validate(oldTemplate, newTemplate api.SchemaTemplate) error {
	oldTables, err := oldTemplate.Tables()
	if err != nil {
		return api.NewErrorf(api.ErrCodeInternalError, "load old template tables: %v", err)
	}
	newTables, err := newTemplate.Tables()
	if err != nil {
		return api.NewErrorf(api.ErrCodeInternalError, "load new template tables: %v", err)
	}

	newByName := make(map[string]api.Table, len(newTables))
	for _, t := range newTables {
		newByName[t.MetadataName()] = t
	}

	for _, oldTbl := range oldTables {
		newTbl, ok := newByName[oldTbl.MetadataName()]
		if !ok {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q was removed; removing tables is not allowed", oldTbl.MetadataName())
		}
		if err := v.validateTable(oldTbl, newTbl); err != nil {
			return err
		}
	}
	return nil
}

func (v *RelationalSchemaEvolutionValidator) validateTable(oldTbl, newTbl api.Table) error {
	oldCols := oldTbl.Columns()
	newCols := newTbl.Columns()

	newByName := make(map[string]api.Column, len(newCols))
	for _, c := range newCols {
		newByName[c.MetadataName()] = c
	}

	for i, oldCol := range oldCols {
		newCol, ok := newByName[oldCol.MetadataName()]
		if !ok {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q: column %q was removed; removing columns is not allowed",
				oldTbl.MetadataName(), oldCol.MetadataName())
		}
		if !oldCol.DataType().Equal(newCol.DataType()) {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q: column %q type changed from %v to %v; type changes are not allowed",
				oldTbl.MetadataName(), oldCol.MetadataName(), oldCol.DataType(), newCol.DataType())
		}
		// Primary key ordering: the first len(oldCols) columns must remain in the
		// same relative order in the new template (additional columns may be appended).
		if newCols[i].MetadataName() != oldCol.MetadataName() {
			// Only flag if this is a PK column (position 0 to len(pk)-1).
			// Heuristic: if the old table's PK is inferred from declared column order,
			// a reorder of any existing column is breaking.
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"table %q: column order changed — existing column %q is now at a different position; reordering columns is not allowed",
				oldTbl.MetadataName(), oldCol.MetadataName())
		}
	}
	return nil
}
