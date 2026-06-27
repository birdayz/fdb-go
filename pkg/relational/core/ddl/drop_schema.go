package ddl

import (
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// DropSchemaConstantAction removes one schema from the catalog and, when a
// keyspace is provided, also deletes the underlying FDB record store.
// Mirrors Java's DropSchemaConstantAction.
type DropSchemaConstantAction struct {
	dbPath     string
	schemaName string
	catalog    api.StoreCatalog
	ks         *keyspace.RelationalKeyspace // nil = catalog-only mode
}

func NewDropSchemaConstantAction(
	dbPath, schemaName string,
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
) *DropSchemaConstantAction {
	return &DropSchemaConstantAction{
		dbPath:     dbPath,
		schemaName: schemaName,
		catalog:    cat,
		ks:         ks,
	}
}

func (a *DropSchemaConstantAction) Execute(txn api.Transaction) error {
	if a.dbPath == SysDatabasePath {
		return api.NewErrorf(api.ErrCodeInsufficientPrivilege, "cannot drop %s schemas", SysDatabasePath)
	}
	if a.ks != nil {
		if err := a.deleteFDBStore(txn); err != nil {
			return err
		}
	}
	return a.catalog.DeleteSchema(txn, a.dbPath, a.schemaName)
}

func (a *DropSchemaConstantAction) deleteFDBStore(txn api.Transaction) error {
	rctx, ok := txn.Unwrap().(*recordlayer.FDBRecordContext)
	if !ok {
		return api.NewErrorf(api.ErrCodeInternalError,
			"DropSchema FDB store deletion requires a transaction whose Unwrap() returns *recordlayer.FDBRecordContext, got %T from %T",
			txn.Unwrap(), txn)
	}
	ss, err := a.ks.SchemaSubspace(a.dbPath, a.schemaName)
	if err != nil {
		return err
	}
	return recordlayer.DeleteStore(rctx, ss)
}
