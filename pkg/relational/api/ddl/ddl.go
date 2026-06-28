// Package ddl defines the DDL action interfaces for the relational layer.
//
// Mirrors Java's com.apple.foundationdb.relational.api.ddl package.
package ddl

import (
	"fdb.dev/pkg/relational/api"
)

// ConstantAction is a DDL action that executes within a transaction
// without returning a value. Mirrors Java's ConstantAction interface.
type ConstantAction interface {
	Execute(txn api.Transaction) error
}

// MetadataOperationsFactory constructs ConstantAction instances for
// each DDL operation. Mirrors Java's MetadataOperationsFactory.
type MetadataOperationsFactory interface {
	// SaveSchemaTemplate creates or updates a schema template in the catalog.
	SaveSchemaTemplate(template api.SchemaTemplate, options api.Options) ConstantAction

	// DropSchemaTemplate removes a schema template by name.
	DropSchemaTemplate(templateID string, throwIfDoesNotExist bool, options api.Options) ConstantAction

	// CreateDatabase creates a new database entry in the catalog.
	CreateDatabase(dbPath string, options api.Options) ConstantAction

	// CreateSchema creates a new schema in the given database.
	CreateSchema(dbPath string, schemaName string, templateID string, options api.Options) ConstantAction

	// DropDatabase removes a database and all its schemas.
	DropDatabase(dbPath string, throwIfDoesNotExist bool, options api.Options) ConstantAction

	// DropSchema removes one schema from a database.
	DropSchema(dbPath string, schemaName string, options api.Options) ConstantAction
}
