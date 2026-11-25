// Portions derived from FoundationDB Record Layer (SemanticAnalyzer.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

import "fdb.dev/pkg/relational/sqlapi"

// CrossDatabaseDDLError reports a DDL statement whose target database is
// outside the session's own database.
type CrossDatabaseDDLError = sqlapi.CrossDatabaseDDLError

// SchemaTemplateDDLRestrictedError reports a CREATE / DROP SCHEMA TEMPLATE
// refused because the connection has OptRestrictDDLToSessionDatabase set.
type SchemaTemplateDDLRestrictedError = sqlapi.SchemaTemplateDDLRestrictedError

// NewSchemaTemplateDDLRestrictedError builds the 42501 error for schema-template
// DDL attempted on a connection restricted to its session database.
func NewSchemaTemplateDDLRestrictedError(operation, sessionDatabase string) *Error {
	return sqlapi.NewSchemaTemplateDDLRestrictedError(operation, sessionDatabase)
}

// NewCrossDatabaseDDLError builds the 42501 error for a cross-database DDL attempt.
func NewCrossDatabaseDDLError(operation, sessionDatabase, targetDatabase string) *Error {
	return sqlapi.NewCrossDatabaseDDLError(operation, sessionDatabase, targetDatabase)
}
