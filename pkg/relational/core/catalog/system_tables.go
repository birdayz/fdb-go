package catalog

// System-table identity constants. Mirror Java's
// com.apple.foundationdb.relational.recordlayer.catalog.systables.
// SystemTableRegistry byte-for-byte.
//
// RECORD_TYPE_KEY values are the leading tuple element on every
// catalog record in FDB — they're what distinguishes a Schema record
// from a Database record at the key level. Java hard-codes the
// numeric values; changing them breaks wire compatibility for any
// catalog Java has written to.
//
// The table-name constants are the user-visible names in
// INFORMATION_SCHEMA / SHOW TABLES output for the system schema
// (__SYS/CATALOG).
const (
	// SchemaRecordTypeKey is the RECORD_TYPE_KEY prefix for Schema
	// records in the catalog store.
	SchemaRecordTypeKey int64 = 0
	// DatabaseInfoRecordTypeKey prefixes DatabaseInfo records.
	DatabaseInfoRecordTypeKey int64 = 1
	// SchemaTemplateRecordTypeKey prefixes SchemaTemplate records.
	SchemaTemplateRecordTypeKey int64 = 2

	// SchemasTableName is the INFORMATION_SCHEMA view name for
	// Schema records.
	SchemasTableName = "SCHEMAS"
	// DatabaseTableName is the view name for DatabaseInfo records.
	DatabaseTableName = "DATABASES"
	// SchemaTemplateTableName is the view name for SchemaTemplate
	// records.
	SchemaTemplateTableName = "TEMPLATES"
)
