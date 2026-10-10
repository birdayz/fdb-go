// Portions derived from FoundationDB Record Layer (
// MetadataOperationsFactory.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package ddl

import (
	"fdb.dev/pkg/relational/api"
)

// DropSchemaTemplateConstantAction removes all versions of a schema template.
// Mirrors Java's DropSchemaTemplateConstantAction (implied by MetadataOperationsFactory).
type DropSchemaTemplateConstantAction struct {
	templateID          string
	throwIfDoesNotExist bool
	catalog             api.SchemaTemplateCatalog
}

func NewDropSchemaTemplateConstantAction(templateID string, throwIfDoesNotExist bool, catalog api.SchemaTemplateCatalog) *DropSchemaTemplateConstantAction {
	return &DropSchemaTemplateConstantAction{templateID: templateID, throwIfDoesNotExist: throwIfDoesNotExist, catalog: catalog}
}

func (a *DropSchemaTemplateConstantAction) Execute(txn api.Transaction) error {
	return a.catalog.DeleteTemplate(txn, a.templateID, a.throwIfDoesNotExist)
}
