package ddl

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	apiddl "github.com/birdayz/fdb-record-layer-go/pkg/relational/api/ddl"
)

// RecordLayerMetadataOperationsFactory is the concrete
// MetadataOperationsFactory backed by a StoreCatalog.
// Mirrors Java's RecordLayerMetadataOperationsFactory.
type RecordLayerMetadataOperationsFactory struct {
	catalog api.StoreCatalog
}

// NewRecordLayerMetadataOperationsFactory constructs a factory.
func NewRecordLayerMetadataOperationsFactory(catalog api.StoreCatalog) *RecordLayerMetadataOperationsFactory {
	return &RecordLayerMetadataOperationsFactory{catalog: catalog}
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
	return NewCreateSchemaConstantAction(dbPath, schemaName, templateID, f.catalog)
}

func (f *RecordLayerMetadataOperationsFactory) DropDatabase(dbPath string, throwIfDoesNotExist bool, options api.Options) apiddl.ConstantAction {
	return NewDropDatabaseConstantAction(dbPath, throwIfDoesNotExist, f.catalog, f, options)
}

func (f *RecordLayerMetadataOperationsFactory) DropSchema(dbPath, schemaName string, _ api.Options) apiddl.ConstantAction {
	return NewDropSchemaConstantAction(dbPath, schemaName, f.catalog)
}
