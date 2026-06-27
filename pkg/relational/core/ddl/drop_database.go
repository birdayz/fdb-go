package ddl

import (
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
)

// DropDatabaseConstantAction removes a database and all its schemas.
// Mirrors Java's DropDatabaseConstantAction.
type DropDatabaseConstantAction struct {
	dbPath              string
	throwIfDoesNotExist bool
	catalog             api.StoreCatalog
	opsFactory          apiddl.MetadataOperationsFactory
	options             api.Options
}

func NewDropDatabaseConstantAction(
	dbPath string,
	throwIfDoesNotExist bool,
	catalog api.StoreCatalog,
	opsFactory apiddl.MetadataOperationsFactory,
	options api.Options,
) *DropDatabaseConstantAction {
	return &DropDatabaseConstantAction{
		dbPath:              dbPath,
		throwIfDoesNotExist: throwIfDoesNotExist,
		catalog:             catalog,
		opsFactory:          opsFactory,
		options:             options,
	}
}

func (a *DropDatabaseConstantAction) Execute(txn api.Transaction) error {
	if a.dbPath == SysDatabasePath {
		return api.NewErrorf(api.ErrCodeInsufficientPrivilege, "cannot drop %s database", SysDatabasePath)
	}
	// Check existence before iterating schemas — needed for the
	// throwIfDoesNotExist=false path.
	exists, err := a.catalog.DoesDatabaseExist(txn, a.dbPath)
	if err != nil {
		return err
	}
	if !exists {
		if a.throwIfDoesNotExist {
			return api.NewErrorf(api.ErrCodeUnknownDatabase, "database %q does not exist", a.dbPath)
		}
		return nil
	}

	rs, err := a.catalog.ListSchemasInDatabase(txn, a.dbPath, nil)
	if err != nil {
		return err
	}
	defer rs.Close() //nolint:errcheck
	for rs.Next() {
		schemaName, err := rs.StringByName(colSchemaName)
		if err != nil {
			return err
		}
		if execErr := a.opsFactory.DropSchema(a.dbPath, schemaName, a.options).Execute(txn); execErr != nil {
			return execErr
		}
	}
	if err := rs.Err(); err != nil {
		return err
	}
	_, err = a.catalog.DeleteDatabase(txn, a.dbPath, a.throwIfDoesNotExist)
	return err
}
