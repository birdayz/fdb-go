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

	// SchemasTableName is the user-visible INFORMATION_SCHEMA name for
	// the Schema record type (Java: "SCHEMAS"). The Go proto descriptor
	// uses the Go-style message name "Schemas"; wire compatibility is
	// preserved because Java and Go both agree on record-type key 0
	// and on field numbers inside the Schemas/SCHEMAS message (both
	// generated programmatically with the same 1-based column layout).
	SchemasTableName = "SCHEMAS"
	// DatabaseTableName — Java-visible name for Databases records.
	DatabaseTableName = "DATABASES"
	// SchemaTemplateTableName — Java-visible name for Templates records.
	SchemaTemplateTableName = "TEMPLATES"

	// SchemasRecordName / DatabasesRecordName / TemplatesRecordName are
	// the internal proto descriptor names that recordlayer uses to
	// resolve record types. They match the message names in
	// proto/relational/catalog_data.proto.
	SchemasRecordName   = "Schemas"
	DatabasesRecordName = "Databases"
	TemplatesRecordName = "Templates"
)
