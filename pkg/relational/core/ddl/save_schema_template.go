// Portions derived from FoundationDB Record Layer (
// SaveSchemaTemplateConstantAction.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package ddl

import (
	"fdb.dev/pkg/relational/api"
)

// SaveSchemaTemplateConstantAction persists a schema template. Mirrors Java's
// SaveSchemaTemplateConstantAction (api/ddl package), which calls the
// catalog's createTemplate and nothing else. Go's CreateTemplate also refuses
// a version at or below the latest stored one, runs the relational evolution
// validator against it, and carries the stored numbering into a new version
// (DIVERGENCES.md "CreateTemplate refuses more than an exact duplicate, and
// carries a new version"); every build-path writer gets those checks there.
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
