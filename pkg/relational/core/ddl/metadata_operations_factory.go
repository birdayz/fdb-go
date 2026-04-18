package ddl

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/keyspace"
)

// RecordLayerMetadataOperationsFactory is the concrete
// MetadataOperationsFactory backed by a StoreCatalog.
// When ks is non-nil, CreateSchema and DropSchema also create/delete
// the underlying FDB record store.
// Mirrors Java's RecordLayerMetadataOperationsFactory.
type RecordLayerMetadataOperationsFactory struct {
	catalog api.StoreCatalog
	ks      *keyspace.RelationalKeyspace // nil = catalog-only mode
}

// NewRecordLayerMetadataOperationsFactory constructs a factory in
// catalog-only mode (no FDB store creation/deletion).
func NewRecordLayerMetadataOperationsFactory(catalog api.StoreCatalog) *RecordLayerMetadataOperationsFactory {
	return &RecordLayerMetadataOperationsFactory{catalog: catalog}
}

// NewRecordLayerMetadataOperationsFactoryWithKeyspace constructs a factory
// that also creates/deletes FDB record stores for schema operations.
func NewRecordLayerMetadataOperationsFactoryWithKeyspace(
	cat api.StoreCatalog,
	ks *keyspace.RelationalKeyspace,
) *RecordLayerMetadataOperationsFactory {
	return &RecordLayerMetadataOperationsFactory{catalog: cat, ks: ks}
}

var _ apiddl.MetadataOperationsFactory = (*RecordLayerMetadataOperationsFactory)(nil)

func (f *RecordLayerMetadataOperationsFactory) SaveSchemaTemplate(template api.SchemaTemplate, _ api.Options) apiddl.ConstantAction {
	return NewSaveSchemaTemplateConstantAction(template, f.catalog.SchemaTemplateCatalog())
}

func (f *RecordLayerMetadataOperationsFactory) DropSchemaTemplate(templateID string, throwIfDoesNotExist bool, _ api.Options) apiddl.ConstantAction {
	return NewDropSchemaTemplateConstantAction(templateID, throwIfDoesNotExist, f.catalog.SchemaTemplateCatalog())
}

func (f *RecordLayerMetadataOperationsFactory) CreateDatabase(dbPath string, _ api.Options) apiddl.ConstantAction {
	return NewCreateDatabaseConstantAction(dbPath, f.catalog)
}

func (f *RecordLayerMetadataOperationsFactory) CreateSchema(dbPath, schemaName, templateID string, _ api.Options) apiddl.ConstantAction {
	return NewCreateSchemaConstantAction(dbPath, schemaName, templateID, f.catalog, f.ks)
}

func (f *RecordLayerMetadataOperationsFactory) DropDatabase(dbPath string, throwIfDoesNotExist bool, options api.Options) apiddl.ConstantAction {
	return NewDropDatabaseConstantAction(dbPath, throwIfDoesNotExist, f.catalog, f, options)
}

func (f *RecordLayerMetadataOperationsFactory) DropSchema(dbPath, schemaName string, _ api.Options) apiddl.ConstantAction {
	return NewDropSchemaConstantAction(dbPath, schemaName, f.catalog, f.ks)
}
