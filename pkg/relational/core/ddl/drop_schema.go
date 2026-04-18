package ddl

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// DropSchemaConstantAction removes one schema from the catalog.
// NOTE: This implementation deletes only the catalog entry. Deletion
// of the underlying FDB record store data is deferred until
// RelationalKeyspaceProvider is wired up.
// Mirrors Java's DropSchemaConstantAction.
type DropSchemaConstantAction struct {
	dbPath     string
	schemaName string
	catalog    api.StoreCatalog
}

func NewDropSchemaConstantAction(dbPath, schemaName string, catalog api.StoreCatalog) *DropSchemaConstantAction {
	return &DropSchemaConstantAction{dbPath: dbPath, schemaName: schemaName, catalog: catalog}
}

func (a *DropSchemaConstantAction) Execute(txn api.Transaction) error {
	if a.dbPath == SysDatabasePath {
		return api.NewErrorf(api.ErrCodeInsufficientPrivilege, "cannot drop %s schemas", SysDatabasePath)
	}
	return a.catalog.DeleteSchema(txn, a.dbPath, a.schemaName)
}
