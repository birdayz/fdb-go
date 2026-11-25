// Portions derived from FoundationDB Record Layer (ConstantAction.java,
// MetadataOperationsFactory.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

// ConstantAction is a DDL action that executes within a transaction
// without returning a value. Mirrors Java's ConstantAction interface
// (com.apple.foundationdb.relational.api.ddl.ConstantAction).
type ConstantAction interface {
	Execute(txn Transaction) error
}

// MetadataOperationsFactory constructs ConstantAction instances for
// each DDL operation. Mirrors Java's MetadataOperationsFactory
// (com.apple.foundationdb.relational.api.ddl.MetadataOperationsFactory).
type MetadataOperationsFactory interface {
	// SaveSchemaTemplate creates or updates a schema template in the catalog.
	SaveSchemaTemplate(template SchemaTemplate, options Options) ConstantAction

	// DropSchemaTemplate removes a schema template by name.
	DropSchemaTemplate(templateID string, throwIfDoesNotExist bool, options Options) ConstantAction

	// CreateDatabase creates a new database entry in the catalog.
	CreateDatabase(dbPath string, options Options) ConstantAction

	// CreateSchema creates a new schema in the given database.
	CreateSchema(dbPath string, schemaName string, templateID string, options Options) ConstantAction

	// DropDatabase removes a database and all its schemas.
	DropDatabase(dbPath string, throwIfDoesNotExist bool, options Options) ConstantAction

	// DropSchema removes one schema from a database.
	DropSchema(dbPath string, schemaName string, options Options) ConstantAction
}
