// Portions derived from FoundationDB Record Layer (
// RecordLayerMetadataOperationsFactory.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package ddl

import (
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
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

// SetStoreState forces a schema store's index states, Java's
// getSetStoreStateConstantAction (with the config Java's factory carries passed
// here instead).
func (f *RecordLayerMetadataOperationsFactory) SetStoreState(dbPath, schemaName string, config RecordLayerConfig) apiddl.ConstantAction {
	return NewSetStoreStateConstantAction(dbPath, schemaName, config, f.catalog, f.ks)
}

func (f *RecordLayerMetadataOperationsFactory) DropSchema(dbPath, schemaName string, _ api.Options) apiddl.ConstantAction {
	return NewDropSchemaConstantAction(dbPath, schemaName, f.catalog, f.ks)
}
