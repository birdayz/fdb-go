package ddl

import (
	"errors"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// CreateSchemaConstantAction creates a new schema in the catalog and,
// when a keyspace is provided, also creates the underlying FDB record store.
// Mirrors Java's RecordLayerCreateSchemaConstantAction.
type CreateSchemaConstantAction struct {
	dbPath     string
	schemaName string
	templateID string
	catalog    api.StoreCatalog
	ks         *keyspace.RelationalKeyspace // nil = catalog-only mode
}

func NewCreateSchemaConstantAction(
	dbPath, schemaName, templateID string,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
) *CreateSchemaConstantAction {
	return &CreateSchemaConstantAction{
		dbPath:     dbPath,
		schemaName: schemaName,
		templateID: templateID,
		catalog:    cat,
		ks:         ks,
	}
}

// Execute runs Java's RecordLayerCreateSchemaConstantAction.execute, in its
// order and with its messages: the database exists (UNDEFINED_DATABASE); the
// template's latest version loads (UNKNOWN_SCHEMA_TEMPLATE); the catalog saves
// the schema with ERROR, which refuses a stored schema of that name with
// SCHEMA_ALREADY_EXISTS (and one bound to a gone template version with
// UNKNOWN_SCHEMA_TEMPLATE, as its load fails first); then the record store is
// created, and a store already there is SCHEMA_ALREADY_EXISTS.
func (a *CreateSchemaConstantAction) Execute(txn api.Transaction) error {
	exists, err := a.catalog.DoesDatabaseExist(txn, a.dbPath)
	if err != nil {
		return err
	}
	if !exists {
		return api.NewErrorf(api.ErrCodeUndefinedDatabase, "Database %s does not exist", a.dbPath)
	}

	template, err := a.catalog.SchemaTemplateCatalog().LoadSchemaTemplate(txn, a.templateID)
	if err != nil {
		return err
	}
	schema := template.GenerateSchema(a.dbPath, a.schemaName)
	if err := a.catalog.SaveSchema(txn, schema, false, api.SchemaExistsError); err != nil {
		return err
	}

	if a.ks != nil {
		if err := a.createFDBStore(txn, template); err != nil {
			var exists *recordlayer.RecordStoreAlreadyExistsError
			if errors.As(err, &exists) {
				return api.NewErrorf(api.ErrCodeSchemaAlreadyExists, "Schema <%s> already exists", a.schemaName)
			}
			return err
		}
	}
	return nil
}

func (a *CreateSchemaConstantAction) createFDBStore(txn api.Transaction, tmpl api.SchemaTemplate) error {
	// Unwrap returns the underlying FDBRecordContext for FDB-backed txns,
	// letting future Transaction decorators keep working.
	rctx, ok := txn.Unwrap().(*recordlayer.FDBRecordContext)
	if !ok {
		return api.NewErrorf(api.ErrCodeInternalError,
			"CreateSchema FDB store creation requires a transaction whose Unwrap() returns *recordlayer.FDBRecordContext, got %T from %T",
			txn.Unwrap(), txn)
	}
	rlTmpl, ok := tmpl.(*metadata.RecordLayerSchemaTemplate)
	if !ok {
		return api.NewErrorf(api.ErrCodeInternalError,
			"CreateSchema requires *metadata.RecordLayerSchemaTemplate, got %T", tmpl)
	}
	ss, err := a.ks.SchemaSubspace(a.dbPath, a.schemaName)
	if err != nil {
		return err
	}
	_, err = recordlayer.NewStoreBuilder().
		SetContext(rctx).
		SetSubspace(ss).
		SetMetaDataProvider(rlTmpl.Underlying()).
		Create()
	return err
}
