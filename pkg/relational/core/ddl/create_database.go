package ddl

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// CreateDatabaseConstantAction creates a new database entry in the catalog.
// Mirrors Java's CreateDatabaseConstantAction.
type CreateDatabaseConstantAction struct {
	dbPath  string
	catalog api.StoreCatalog
}

func NewCreateDatabaseConstantAction(dbPath string, catalog api.StoreCatalog) *CreateDatabaseConstantAction {
	return &CreateDatabaseConstantAction{dbPath: dbPath, catalog: catalog}
}

func (a *CreateDatabaseConstantAction) Execute(txn api.Transaction) error {
	exists, err := a.catalog.DoesDatabaseExist(txn, a.dbPath)
	if err != nil {
		return err
	}
	if exists {
		return api.NewErrorf(api.ErrCodeDatabaseAlreadyExists, "database %q already exists", a.dbPath)
	}
	return a.catalog.CreateDatabase(txn, a.dbPath)
}
