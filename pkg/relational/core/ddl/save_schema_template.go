package ddl

import (
	"fdb.dev/pkg/relational/api"
)

// SaveSchemaTemplateConstantAction persists a schema template.
// If a previous version of the same template already exists, it runs the
// RelationalSchemaEvolutionValidator to ensure the change is backward-compatible.
// Mirrors Java's SaveSchemaTemplateConstantAction (api/ddl package).
type SaveSchemaTemplateConstantAction struct {
	template  api.SchemaTemplate
	catalog   api.SchemaTemplateCatalog
	validator *RelationalSchemaEvolutionValidator
}

func NewSaveSchemaTemplateConstantAction(template api.SchemaTemplate, catalog api.SchemaTemplateCatalog) *SaveSchemaTemplateConstantAction {
	return &SaveSchemaTemplateConstantAction{
		template:  template,
		catalog:   catalog,
		validator: NewRelationalSchemaEvolutionValidator(),
	}
}

func (a *SaveSchemaTemplateConstantAction) Execute(txn api.Transaction) error {
	// If a previous version exists, validate evolution compatibility.
	exists, err := a.catalog.DoesSchemaTemplateExist(txn, a.template.MetadataName())
	if err != nil {
		return err
	}
	if exists {
		oldTemplate, loadErr := a.catalog.LoadSchemaTemplate(txn, a.template.MetadataName())
		if loadErr != nil {
			return loadErr
		}
		if a.template.Version() <= oldTemplate.Version() {
			return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"template %q: new version %d must be greater than current version %d",
				a.template.MetadataName(), a.template.Version(), oldTemplate.Version())
		}
		if valErr := a.validator.Validate(oldTemplate, a.template); valErr != nil {
			return valErr
		}
	}
	return a.catalog.CreateTemplate(txn, a.template)
}
