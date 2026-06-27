package catalog

import (
	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// Catalog column / index constants. Mirror Java's
// com.apple.foundationdb.relational.recordlayer.catalog.systables.SystemTable
// + sibling table classes.
const (
	ColDatabaseID      = "DATABASE_ID"
	ColSchemaName      = "SCHEMA_NAME"
	ColTemplateName    = "TEMPLATE_NAME"
	ColTemplateVersion = "TEMPLATE_VERSION"
	ColMetaData        = "META_DATA"

	// Indexes on SCHEMAS.
	IdxTemplatesCount = "TEMPLATES_COUNT_INDEX"
	IdxTemplatesValue = "TEMPLATES_VALUE_INDEX"
	// Index on DATABASES.
	IdxDatabasesCount = "DATABASES_COUNT_INDEX"
)

// BuildCatalogMetaData materialises the RecordMetaData for the three
// catalog record types (SCHEMAS, DATABASES, TEMPLATES).
//
// Wire layout (byte-for-byte with Java RecordLayerStoreCatalog):
//
//	SCHEMAS   — record type key 0, PK [typeKey, DATABASE_ID, SCHEMA_NAME]
//	DATABASES — record type key 1, PK [typeKey, DATABASE_ID]
//	TEMPLATES — record type key 2, PK [typeKey, TEMPLATE_NAME, TEMPLATE_VERSION]
//
// Indexes:
//
//	SCHEMAS.TEMPLATES_COUNT_INDEX — COUNT grouped by (TEMPLATE_NAME, TEMPLATE_VERSION)
//	SCHEMAS.TEMPLATES_VALUE_INDEX — VALUE on (TEMPLATE_NAME, TEMPLATE_VERSION, DATABASE_ID, SCHEMA_NAME)
//	DATABASES.DATABASES_COUNT_INDEX — ungrouped COUNT
//
// Returns an error only if the underlying RecordMetaDataBuilder rejects
// the proto descriptor; under normal conditions this cannot fail.
func BuildCatalogMetaData() (*recordlayer.RecordMetaData, error) {
	b := recordlayer.NewRecordMetaDataBuilder().
		SetRecordsWithUnionName(gen.File_catalog_data_proto, "CatalogUnion").
		// SetVersion(1): Java's CATALOG_TEMPLATE_VERSION=1; 3 addIndex bumps → v4. Without this, Go starts at 0 → v3 → StaleMetaDataVersionError on cross-engine read.
		SetVersion(1)

	b.GetRecordType(SchemasRecordName).
		SetRecordTypeKey(SchemaRecordTypeKey).
		SetPrimaryKey(recordlayer.Concat(
			recordlayer.RecordTypeKey(),
			recordlayer.Field(ColDatabaseID),
			recordlayer.Field(ColSchemaName),
		))

	b.GetRecordType(DatabasesRecordName).
		SetRecordTypeKey(DatabaseInfoRecordTypeKey).
		SetPrimaryKey(recordlayer.Concat(
			recordlayer.RecordTypeKey(),
			recordlayer.Field(ColDatabaseID),
		))

	b.GetRecordType(TemplatesRecordName).
		SetRecordTypeKey(SchemaTemplateRecordTypeKey).
		SetPrimaryKey(recordlayer.Concat(
			recordlayer.RecordTypeKey(),
			recordlayer.Field(ColTemplateName),
			recordlayer.Field(ColTemplateVersion),
		))

	schemasCount := recordlayer.NewIndex(IdxTemplatesCount,
		recordlayer.GroupAll(recordlayer.Concat(
			recordlayer.Field(ColTemplateName),
			recordlayer.Field(ColTemplateVersion),
		)))
	schemasCount.Type = recordlayer.IndexTypeCount
	b.AddIndex(SchemasRecordName, schemasCount)

	schemasValue := recordlayer.NewIndex(IdxTemplatesValue, recordlayer.Concat(
		recordlayer.Field(ColTemplateName),
		recordlayer.Field(ColTemplateVersion),
		recordlayer.Field(ColDatabaseID),
		recordlayer.Field(ColSchemaName),
	))
	b.AddIndex(SchemasRecordName, schemasValue)

	databasesCount := recordlayer.NewIndex(IdxDatabasesCount,
		recordlayer.GroupAll(recordlayer.EmptyKey()))
	databasesCount.Type = recordlayer.IndexTypeCount
	b.AddIndex(DatabasesRecordName, databasesCount)

	return b.Build()
}
