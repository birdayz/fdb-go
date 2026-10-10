// Portions derived from FoundationDB Record Layer (
// RecordMetadataDeserializer.java, RecordLayerSchemaTemplate.java,
// RecordMetaData.java, SchemaTemplate.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package metadata

import (
	"sort"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// RecordLayerSchemaTemplate is the concrete api.SchemaTemplate backed
// by a *recordlayer.RecordMetaData.
//
// Construction materialises every table + index up front so lookups
// are O(1) and the template is safe for concurrent reads. The
// underlying RecordMetaData is assumed immutable (matching Java's
// RecordMetaData invariant).
//
// Views, routines and stored queries are read from the stored metadata as
// Java's RecordMetadataDeserializer reads them. Temporary routines and the
// transaction-bound diagnostic string are empty.
type RecordLayerSchemaTemplate struct {
	name       string
	version    int
	underlying *recordlayer.RecordMetaData
	tables     []api.Table
	tablesByN  map[string]api.Table // table name → Table
	indexNames []string             // all index names, deterministic order
}

// NewRecordLayerSchemaTemplate builds the bridge with the underlying
// RecordMetaData's version. Equivalent to Java's
// RecordLayerSchemaTemplate.fromRecordMetadata(md, name, md.getVersion()).
// Use NewRecordLayerSchemaTemplateWithVersion when the catalog-level
// version should differ from the storage-level version.
//
// Returns an error when md is nil so callers at boundary layers
// (DSN parsing, RPC handlers) get a clean failure rather than a panic.
func NewRecordLayerSchemaTemplate(name string, md *recordlayer.RecordMetaData) (*RecordLayerSchemaTemplate, error) {
	if md == nil {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate, "record metadata is nil")
	}
	return NewRecordLayerSchemaTemplateWithVersion(name, md, md.Version())
}

// NewRecordLayerSchemaTemplateWithVersion mirrors Java's
// RecordMetadataDeserializer.getSchemaTemplate(name, version): the
// schema-template version is independent of RecordMetaData.Version(),
// which the record-layer storage engine uses for its own bookkeeping.
// The catalog bumps the template version on every DDL change, and
// that number is what api.SchemaTemplate.Version() reports.
func NewRecordLayerSchemaTemplateWithVersion(name string, md *recordlayer.RecordMetaData, version int) (*RecordLayerSchemaTemplate, error) {
	if md == nil {
		return nil, api.NewError(api.ErrCodeInvalidSchemaTemplate, "record metadata is nil")
	}
	if err := checkTableGenerations(md); err != nil {
		return nil, err
	}
	tmpl := &RecordLayerSchemaTemplate{
		name:       name,
		version:    version,
		underlying: md,
		tablesByN:  make(map[string]api.Table),
	}

	// Deterministic iteration: RecordTypes is a map.
	typeNames := make([]string, 0, len(md.RecordTypes()))
	for n := range md.RecordTypes() {
		typeNames = append(typeNames, n)
	}
	sort.Strings(typeNames)

	// Per-table indexes only. Matches Java's
	// RecordMetadataDeserializer.generateTableBuilder (line 145 of that
	// file in the 4.10.6.0 tree) which populates each RecordLayerTable
	// from recordType.getIndexes() — per-type + multi-type, but NOT
	// universal indexes. Universal indexes still show up in the flat
	// Indexes() result because they live in md.GetAllIndexes().
	tmpl.tables = make([]api.Table, 0, len(typeNames))
	for _, n := range typeNames {
		rt := md.GetRecordType(n)
		typeIdx := md.GetIndexesForRecordType(n)
		apiIdxs := make([]api.Index, 0, len(typeIdx))
		for _, idx := range typeIdx {
			apiIdxs = append(apiIdxs, newIndex(idx, n))
		}
		tbl, err := newTable(rt, apiIdxs)
		if err != nil {
			return nil, err
		}
		tmpl.tables = append(tmpl.tables, tbl)
		tmpl.tablesByN[n] = tbl
	}

	// Flat list of all index names (per-table + universal). GetAllIndexes
	// returns a map keyed by name so uniqueness is already guaranteed;
	// sort for deterministic output.
	tmpl.indexNames = make([]string, 0, len(md.GetAllIndexes()))
	for n := range md.GetAllIndexes() {
		tmpl.indexNames = append(tmpl.indexNames, n)
	}
	sort.Strings(tmpl.indexNames)

	return tmpl, nil
}

// checkTableGenerations is the check Java's RecordMetadataDeserializer makes
// while it turns the union's fields into table generations
// (RecordLayerTable.Builder.addGeneration): each message field of the union is
// a generation of the table its message names, and two generations of one
// table may share neither a field number nor their FieldOptions. Equal options
// are TABLE_ALREADY_EXISTS "Duplicated options for different generations of
// Table <name>". The options compare as protobuf messages do in Java, so an
// extension (or an unknown field) is what tells two generations apart.
func checkTableGenerations(md *recordlayer.RecordMetaData) error {
	union := md.GetUnionDescriptor()
	if union == nil {
		return nil
	}
	options := map[string][]proto.Message{}
	fields := union.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Kind() != protoreflect.MessageKind {
			continue
		}
		table := recordlayer.ToUserIdentifier(string(f.Message().Name()))
		opts := f.Options()
		for _, seen := range options[table] {
			if proto.Equal(seen, opts) {
				return api.NewErrorf(api.ErrCodeTableAlreadyExists,
					"Duplicated options for different generations of Table %s", table)
			}
		}
		options[table] = append(options[table], opts)
	}
	return nil
}

// MetadataName returns the template name provided at construction.
func (s *RecordLayerSchemaTemplate) MetadataName() string { return s.name }

// Version returns the schema-template version. Matches Java's
// SchemaTemplate.getVersion() — independent from
// RecordMetaData.getVersion(); the caller passes the catalog-level
// version to NewRecordLayerSchemaTemplateWithVersion, or
// NewRecordLayerSchemaTemplate uses RecordMetaData.Version() as a
// sensible default.
func (s *RecordLayerSchemaTemplate) Version() int { return s.version }

// EnableLongRows delegates to the underlying metadata's
// splitLongRecords flag.
func (s *RecordLayerSchemaTemplate) EnableLongRows() bool {
	return s.underlying.IsSplitLongRecords()
}

// StoreRowVersions delegates to the underlying metadata's
// storeRecordVersions flag.
func (s *RecordLayerSchemaTemplate) StoreRowVersions() bool {
	return s.underlying.IsStoreRecordVersions()
}

// IntermingleTables mirrors Java's
// RecordLayerSchemaTemplate.isIntermingleTables() which is
// !RecordMetaData.primaryKeyHasRecordTypePrefix(). When the
// underlying metadata has no RecordTypeKey prefix on primary keys,
// rows from different record types share the same keyspace prefix
// and the SQL layer treats them as intermingled.
func (s *RecordLayerSchemaTemplate) IntermingleTables() bool {
	return !s.underlying.PrimaryKeyHasRecordTypePrefix()
}

// Tables returns the tables in deterministic (sorted-by-name) order.
// Error slot is reserved for future catalog-backed implementations;
// this bridge never returns an error.
func (s *RecordLayerSchemaTemplate) Tables() ([]api.Table, error) {
	return s.tables, nil
}

// FindTable looks up a table by exact name; returns (nil, nil) when
// not found.
func (s *RecordLayerSchemaTemplate) FindTable(name string) (api.Table, error) {
	t, ok := s.tablesByN[name]
	if !ok {
		return nil, nil
	}
	return t, nil
}

// Views returns the stored views, each described by its definition.
func (s *RecordLayerSchemaTemplate) Views() ([]api.View, error) {
	var out []api.View
	for _, v := range s.underlying.Views() {
		out = append(out, &storedView{name: v.GetName(), description: v.GetDefinition()})
	}
	return out, nil
}

// FindView returns the stored view of that name, or nil.
func (s *RecordLayerSchemaTemplate) FindView(name string) (api.View, error) {
	views, _ := s.Views()
	for _, v := range views {
		if v.MetadataName() == name {
			return v, nil
		}
	}
	return nil, nil
}

// TableIndexMapping returns a map of tableName → index names.
// Deterministic: both outer keys and inner slices are sorted.
func (s *RecordLayerSchemaTemplate) TableIndexMapping() (map[string][]string, error) {
	out := make(map[string][]string, len(s.tables))
	for _, t := range s.tables {
		names := make([]string, 0, len(t.Indexes()))
		for _, idx := range t.Indexes() {
			names = append(names, idx.MetadataName())
		}
		sort.Strings(names)
		out[t.MetadataName()] = names
	}
	return out, nil
}

// Indexes returns every index name declared in this template, in
// sorted order. Matches Java's flat-list semantics.
func (s *RecordLayerSchemaTemplate) Indexes() ([]string, error) {
	return s.indexNames, nil
}

// InvokedRoutines returns the stored SQL functions. A SQL-bodied function is
// described by its stored definition; a macro by its name, where Java renders
// the macro object's identity (DIVERGENCES.md, "Macro routine description").
func (s *RecordLayerSchemaTemplate) InvokedRoutines() ([]api.InvokedRoutine, error) {
	var out []api.InvokedRoutine
	for _, f := range s.underlying.UserDefinedFunctions() {
		if sql := f.GetSqlFunction(); sql != nil {
			out = append(out, &storedRoutine{name: sql.GetName(), description: sql.GetDefinition()})
		} else if m := f.GetUserDefinedMacroFunction(); m != nil {
			out = append(out, &storedRoutine{name: m.GetFunctionName(), description: m.GetFunctionName()})
		}
	}
	return out, nil
}

// FindInvokedRoutine returns the stored routine of that name, or nil.
func (s *RecordLayerSchemaTemplate) FindInvokedRoutine(name string) (api.InvokedRoutine, error) {
	routines, _ := s.InvokedRoutines()
	for _, r := range routines {
		if r.MetadataName() == name {
			return r, nil
		}
	}
	return nil, nil
}

// StoredQueries returns the stored queries by name.
func (s *RecordLayerSchemaTemplate) StoredQueries() (map[string]api.StoredQuery, error) {
	out := map[string]api.StoredQuery{}
	for _, q := range s.underlying.StoredQueries() {
		out[q.GetName()] = api.StoredQuery{Query: q.GetQuery(), TempFunctions: q.GetTempFunctions()}
	}
	return out, nil
}

// storedView and storedRoutine are Java's RecordLayerView and
// RecordLayerInvokedRoutine as the deserializer builds them: never temporary,
// and a stored routine has no normalized description.
type storedView struct{ name, description string }

func (v *storedView) MetadataName() string   { return v.name }
func (v *storedView) Accept(vis api.Visitor) { vis.VisitView(v) }
func (v *storedView) Description() string    { return v.description }
func (v *storedView) IsTemporary() bool      { return false }

type storedRoutine struct{ name, description string }

func (r *storedRoutine) MetadataName() string          { return r.name }
func (r *storedRoutine) Accept(vis api.Visitor)        { vis.VisitInvokedRoutine(r) }
func (r *storedRoutine) Description() string           { return r.description }
func (r *storedRoutine) NormalizedDescription() string { return "" }
func (r *storedRoutine) IsTemporary() bool             { return false }

// TemporaryInvokedRoutines is always empty.
func (s *RecordLayerSchemaTemplate) TemporaryInvokedRoutines() ([]api.InvokedRoutine, error) {
	return nil, nil
}

// TransactionBoundMetadataAsString is a diagnostic string. The Java
// side tags each transaction-bound piece; we have none, so return an
// empty string.
func (s *RecordLayerSchemaTemplate) TransactionBoundMetadataAsString() (string, error) {
	return "", nil
}

// GenerateSchema materialises an api.Schema bound to databaseID +
// schemaName. Mirrors Java's factory method.
func (s *RecordLayerSchemaTemplate) GenerateSchema(databaseID, schemaName string) api.Schema {
	return &recordLayerSchema{
		databaseID: databaseID,
		name:       schemaName,
		template:   s,
	}
}

// Accept runs Java's visitor cascade:
//
//	startVisit → visit → <tables.accept> → <routines.accept> → <views.accept> → finishVisit
//
// Matches RecordLayerSchemaTemplate.accept() in the Java tree. The
// package-level api.VisitSchemaTemplateTree only handles the
// start/visit/finish triple — it cannot iterate children because
// api.SchemaTemplate returns typed child collections (Tables/Views/
// Routines) via methods that can error. We override here so the
// cascade matches Java behaviour.
func (s *RecordLayerSchemaTemplate) Accept(v api.Visitor) {
	v.StartVisitSchemaTemplate(s)
	v.VisitSchemaTemplate(s)
	for _, t := range s.tables {
		t.Accept(v)
	}
	if rs, _ := s.InvokedRoutines(); rs != nil {
		for _, r := range rs {
			r.Accept(v)
		}
	}
	if vs, _ := s.Views(); vs != nil {
		for _, view := range vs {
			view.Accept(v)
		}
	}
	v.FinishVisitSchemaTemplate(s)
}

// Underlying exposes the record-layer metadata for callers that need
// proto-descriptor-level access (e.g. the query executor).
func (s *RecordLayerSchemaTemplate) Underlying() *recordlayer.RecordMetaData { return s.underlying }

// recordLayerSchema is the trivial api.Schema impl returned from
// GenerateSchema. It holds no state beyond the template pointer.
type recordLayerSchema struct {
	databaseID string
	name       string
	template   *RecordLayerSchemaTemplate
}

func (s *recordLayerSchema) MetadataName() string               { return s.name }
func (s *recordLayerSchema) SchemaTemplate() api.SchemaTemplate { return s.template }
func (s *recordLayerSchema) DatabaseName() string               { return s.databaseID }
func (s *recordLayerSchema) Accept(v api.Visitor)               { v.VisitSchema(s) }

// Tables / Views / Indexes / InvokedRoutines mirror the default method
// bodies on Java's Schema interface — each one just delegates to the
// owning SchemaTemplate. Kept explicit here because Go interfaces have
// no default methods.
func (s *recordLayerSchema) Tables() ([]api.Table, error) { return s.template.Tables() }
func (s *recordLayerSchema) Views() ([]api.View, error)   { return s.template.Views() }

// Indexes matches Java's Schema.getIndexes() which returns the
// (table → index names) multimap, NOT the SchemaTemplate's flat
// []string Indexes() list.
func (s *recordLayerSchema) Indexes() (map[string][]string, error) {
	return s.template.TableIndexMapping()
}

func (s *recordLayerSchema) InvokedRoutines() ([]api.InvokedRoutine, error) {
	return s.template.InvokedRoutines()
}
