package ddl

import (
	"strings"

	"fdb.dev/pkg/relational/api"
)

// isSystemDatabasePath reports whether p names the system catalog database or
// anything nested inside it. Segment-granular, so /__SYSTEM is not caught.
func isSystemDatabasePath(p string) bool {
	return p == SysDatabasePath || strings.HasPrefix(p, SysDatabasePath+"/")
}

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
	// The system catalog lives under /__SYS (Java: RecordLayerStoreCatalog's
	// catalogSchemaPath is /__SYS/CATALOG). DROP already refuses that space;
	// CREATE did not, so a nested path like /__SYS/anything could be planted
	// inside the catalog's own keyspace.
	//
	// Unconditional, and it cannot conflict with Java parity: Java's
	// CreateDatabaseConstantAction has no reserved-name guard at all, so
	// CREATE DATABASE /__SYS there fails only incidentally, via
	// doesDatabaseExist → DATABASE_ALREADY_EXISTS. Refusing it as 42501 states
	// the actual reason and covers the nested paths that no existence check
	// catches.
	if isSystemDatabasePath(a.dbPath) {
		return api.NewErrorf(api.ErrCodeInsufficientPrivilege,
			"cannot create database %q inside the %s system catalog space", a.dbPath, SysDatabasePath)
	}
	exists, err := a.catalog.DoesDatabaseExist(txn, a.dbPath)
	if err != nil {
		return err
	}
	if exists {
		return api.NewErrorf(api.ErrCodeDatabaseAlreadyExists, "database %q already exists", a.dbPath)
	}
	return a.catalog.CreateDatabase(txn, a.dbPath)
}
