package ddl

import (
	"fdb.dev/pkg/relational/api"
)

// SaveSchemaTemplateConstantAction persists a schema template. Mirrors Java's
// SaveSchemaTemplateConstantAction (api/ddl package), which calls the
// catalog's createTemplate and nothing else. Go's CreateTemplate also refuses
// a version at or below the latest stored one, runs the relational evolution
// validator against it, and carries the stored numbering into a new version
// (ws-j-design.md section 4); every build-path writer gets those checks there.
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
