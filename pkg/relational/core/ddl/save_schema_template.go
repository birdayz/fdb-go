package ddl

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// SaveSchemaTemplateConstantAction persists a schema template.
// Mirrors Java's SaveSchemaTemplateConstantAction (api/ddl package).
type SaveSchemaTemplateConstantAction struct {
	template api.SchemaTemplate
	catalog  api.SchemaTemplateCatalog
}

func NewSaveSchemaTemplateConstantAction(template api.SchemaTemplate, catalog api.SchemaTemplateCatalog) *SaveSchemaTemplateConstantAction {
	return &SaveSchemaTemplateConstantAction{template: template, catalog: catalog}
}

func (a *SaveSchemaTemplateConstantAction) Execute(txn api.Transaction) error {
	return a.catalog.CreateTemplate(txn, a.template)
}
