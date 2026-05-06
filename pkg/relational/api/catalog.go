package api

// Package api types in this file mirror Java's
// com.apple.foundationdb.relational.api.catalog.{StoreCatalog,
// SchemaTemplateCatalog}.
//
// Java uses java.net.URI for database identifiers; in Go we use plain
// string for the identifier path (e.g. "/system" or "/foo/bar").
// That's a documented divergence from Java's strict validation, but
// the catalog layer doesn't exercise URI features beyond path
// equality — adding net/url.URL here would only add marshalling
// overhead. Call sites MUST still normalise paths before comparing
// (leading "/", no trailing "/").

// SchemaTemplateCatalog stores schema-template metadata keyed by
// template name and version. Mirrors Java's
// com.apple.foundationdb.relational.api.catalog.SchemaTemplateCatalog.
type SchemaTemplateCatalog interface {
	// DoesSchemaTemplateExist checks whether any version of the given
	// template name exists in the catalog.
	DoesSchemaTemplateExist(txn Transaction, templateName string) (bool, error)
	// DoesSchemaTemplateExistAtVersion checks whether a specific
	// (name, version) is present. Java overloads the method name; Go
	// splits the two paths to avoid optional-argument hacks.
	DoesSchemaTemplateExistAtVersion(txn Transaction, templateName string, version int) (bool, error)
	// LoadSchemaTemplate loads the latest version of templateName.
	// Returns an *Error with Code == ErrCodeUnknownSchemaTemplate
	// when templateName is not found.
	LoadSchemaTemplate(txn Transaction, templateName string) (SchemaTemplate, error)
	// LoadSchemaTemplateAtVersion loads a specific version. Same
	// not-found semantics as LoadSchemaTemplate.
	LoadSchemaTemplateAtVersion(txn Transaction, templateName string, version int) (SchemaTemplate, error)
	// CreateTemplate persists a new template version. Returns an
	// *Error with ErrCodeDuplicateSchemaTemplate when (name, version)
	// already exists.
	CreateTemplate(txn Transaction, newTemplate SchemaTemplate) error
	// ListTemplates returns a ResultSet over every template version
	// in the catalog.
	ListTemplates(txn Transaction) (ResultSet, error)
	// DeleteTemplate removes ALL versions of templateName. When
	// throwIfDoesNotExist is true and the template is missing, an
	// *Error with ErrCodeUnknownSchemaTemplate is returned.
	DeleteTemplate(txn Transaction, templateName string, throwIfDoesNotExist bool) error
	// DeleteTemplateVersion removes one specific (name, version).
	DeleteTemplateVersion(txn Transaction, templateName string, version int, throwIfDoesNotExist bool) error
}

// StoreCatalog is the top-level relational catalog: maps database +
// schema identifiers to their Schema metadata.
//
// Mirrors Java's
// com.apple.foundationdb.relational.api.catalog.StoreCatalog. Method
// names drop Java's get/is prefixes per Go idiom; signatures follow
// Java's semantics exactly.
type StoreCatalog interface {
	// SchemaTemplateCatalog returns the nested template catalog used
	// by this store. Schemas reference their template by (name,
	// version) via the template catalog.
	SchemaTemplateCatalog() SchemaTemplateCatalog

	// LoadSchema returns the Schema for (databaseID, schemaName).
	// Returns *Error with ErrCodeUndefinedSchema if not found.
	LoadSchema(txn Transaction, databaseID, schemaName string) (Schema, error)

	// SaveSchema persists or updates a Schema. If
	// createDatabaseIfNecessary is true and the owning database does
	// not yet exist, it is created atomically with the schema. Java
	// raises on transaction-level conflicts; in Go those surface as
	// errors returned by the Transaction.Commit call, not from here.
	SaveSchema(txn Transaction, dataToWrite Schema, createDatabaseIfNecessary bool) error

	// RepairSchema rebinds schemaName in databaseID to the latest
	// version of its owning template. Matches Java's
	// repairSchema — a no-op when the schema is already on the
	// latest template version.
	RepairSchema(txn Transaction, databaseID, schemaName string) error

	// CreateDatabase records a new database entry keyed by dbURI.
	// Returns an error when the database already exists.
	CreateDatabase(txn Transaction, dbURI string) error

	// ListDatabases returns a ResultSet over every database entry.
	ListDatabases(txn Transaction, continuation Continuation) (ResultSet, error)

	// ListSchemas returns a ResultSet over every schema in every
	// database. Matches Java's listSchemas(txn, continuation).
	ListSchemas(txn Transaction, continuation Continuation) (ResultSet, error)

	// ListSchemasInDatabase narrows ListSchemas to a single database.
	// Java overloads listSchemas; Go splits for clarity.
	ListSchemasInDatabase(txn Transaction, databaseID string, continuation Continuation) (ResultSet, error)

	// DeleteSchema removes schemaName from databaseID. Underlying
	// record store data is NOT purged — callers are responsible for
	// that per Java's documented contract.
	DeleteSchema(txn Transaction, dbURI, schemaName string) error

	// DoesDatabaseExist reports whether dbURI is present in the
	// catalog.
	DoesDatabaseExist(txn Transaction, dbURI string) (bool, error)

	// DoesSchemaExist reports whether (dbURI, schemaName) resolves
	// to a Schema.
	DoesSchemaExist(txn Transaction, dbURI, schemaName string) (bool, error)

	// DeleteDatabase removes the database and every schema within.
	// Underlying record-store data is NOT purged — the caller must
	// clear it separately if needed. When throwIfDoesNotExist is
	// false, a missing database is a no-op.
	//
	// Returns (true, nil) on success, (false, nil) when the
	// transaction runs out of time partway through and the caller
	// should retry — matches Java's boolean return.
	DeleteDatabase(txn Transaction, dbURI string, throwIfDoesNotExist bool) (bool, error)
}
