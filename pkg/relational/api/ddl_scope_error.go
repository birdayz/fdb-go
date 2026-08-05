package api

import "fmt"

// CrossDatabaseDDLError reports a DDL statement whose target database is
// outside the session's own database, rejected because the connection has
// OptRestrictDDLToSessionDatabase set.
//
// It is always wrapped in an *Error carrying ErrCodeInsufficientPrivilege, so
// the statement surfaces as SQLSTATE 42501 while callers that need to tell this
// rejection apart from the unconditional /__SYS guard can errors.As for this
// type and read which database was asked for.
//
// There is no Java counterpart: Java's SemanticAnalyzer resolves a qualified
// "/db/SCHEMA" identifier lexically and never consults the connection, so no
// Java exception describes this condition. The option that produces it is
// off by default for exactly that reason.
type CrossDatabaseDDLError struct {
	// Operation names the rejected statement, e.g. "DROP DATABASE".
	Operation string
	// SessionDatabase is the database path the connection is bound to.
	SessionDatabase string
	// TargetDatabase is the database path the statement resolved to.
	TargetDatabase string
}

func (e *CrossDatabaseDDLError) Error() string {
	return fmt.Sprintf("%s targets database %q, outside this connection's database %q",
		e.Operation, e.TargetDatabase, e.SessionDatabase)
}

// SchemaTemplateDDLRestrictedError reports a CREATE / DROP SCHEMA TEMPLATE
// refused because the connection has OptRestrictDDLToSessionDatabase set.
//
// Unlike CrossDatabaseDDLError this names no target database, because there is
// none to name: a schema template is cluster-global in the catalog wire format
// (the Templates record carries TEMPLATE_NAME, TEMPLATE_VERSION and META_DATA
// and nothing that identifies an owner). Templates therefore have no ownership
// model to check a session database against, and refusal is the only sound
// semantics — see the guard in checkSchemaTemplateDDLAllowed for why permitting
// it would let a restricted tenant reach another tenant's schemas.
//
// Like CrossDatabaseDDLError it is wrapped in an *Error carrying
// ErrCodeInsufficientPrivilege, and it has no Java counterpart.
type SchemaTemplateDDLRestrictedError struct {
	// Operation names the rejected statement, e.g. "DROP SCHEMA TEMPLATE".
	Operation string
	// SessionDatabase is the database path the connection is bound to.
	SessionDatabase string
}

func (e *SchemaTemplateDDLRestrictedError) Error() string {
	return fmt.Sprintf("%s is not permitted on a connection restricted to database %q: "+
		"schema templates are cluster-global and have no owning database",
		e.Operation, e.SessionDatabase)
}

// NewSchemaTemplateDDLRestrictedError builds the 42501 error for schema-template
// DDL attempted on a connection restricted to its session database.
func NewSchemaTemplateDDLRestrictedError(operation, sessionDatabase string) *Error {
	cause := &SchemaTemplateDDLRestrictedError{
		Operation:       operation,
		SessionDatabase: sessionDatabase,
	}
	return WrapErrorf(cause, ErrCodeInsufficientPrivilege,
		"%s is not permitted on a connection restricted to its session database", operation).
		WithContext("session_database", sessionDatabase)
}

// NewCrossDatabaseDDLError builds the 42501 error for a cross-database DDL
// attempt, with the CrossDatabaseDDLError as its cause and the same facts
// mirrored into the error context the way Java's addLogInfo() carries them.
func NewCrossDatabaseDDLError(operation, sessionDatabase, targetDatabase string) *Error {
	cause := &CrossDatabaseDDLError{
		Operation:       operation,
		SessionDatabase: sessionDatabase,
		TargetDatabase:  targetDatabase,
	}
	return WrapErrorf(cause, ErrCodeInsufficientPrivilege,
		"%s is not permitted outside this connection's database", operation).
		WithContext("session_database", sessionDatabase).
		WithContext("target_database", targetDatabase)
}
