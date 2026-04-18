package ddl

import (
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// CreateSchemaConstantAction creates a new schema in the catalog by
// loading its template and generating the schema from it.
// NOTE: This implementation creates only the catalog entry. Creation of
// the underlying FDB record store is deferred until RelationalKeyspaceProvider
// is wired up.
// Mirrors Java's RecordLayerCreateSchemaConstantAction.
type CreateSchemaConstantAction struct {
	dbPath     string
	schemaName string
	templateID string
	catalog    api.StoreCatalog
}

func NewCreateSchemaConstantAction(dbPath, schemaName, templateID string, catalog api.StoreCatalog) *CreateSchemaConstantAction {
	return &CreateSchemaConstantAction{
		dbPath:     dbPath,
		schemaName: schemaName,
		templateID: templateID,
		catalog:    catalog,
	}
}

func (a *CreateSchemaConstantAction) Execute(txn api.Transaction) error {
	exists, err := a.catalog.DoesDatabaseExist(txn, a.dbPath)
	if err != nil {
		return err
	}
	if !exists {
		return api.NewErrorf(api.ErrCodeUndefinedDatabase, "database %q does not exist", a.dbPath)
	}

	// Check schema does not already exist.
	_, err = a.catalog.LoadSchema(txn, a.dbPath, a.schemaName)
	if err == nil {
		return api.NewErrorf(api.ErrCodeSchemaAlreadyExists, "schema %q already exists in %q", a.schemaName, a.dbPath)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedSchema {
		return fmt.Errorf("checking schema existence: %w", err)
	}

	// Load template and generate schema.
	template, err := a.catalog.SchemaTemplateCatalog().LoadSchemaTemplate(txn, a.templateID)
	if err != nil {
		return err
	}
	schema := template.GenerateSchema(a.dbPath, a.schemaName)
	return a.catalog.SaveSchema(txn, schema, false)
}
