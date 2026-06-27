package metadata

import (
	"sort"

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
// Views / invoked routines / temporary routines are stub-empty —
// record-layer has no native equivalents, and the SQL-level stores
// for them land in a later phase (catalog storage layer). Same for
// the transaction-bound diagnostic string.
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

// Views always returns nil — record-layer has no views today.
func (s *RecordLayerSchemaTemplate) Views() ([]api.View, error) { return nil, nil }

// FindView returns (nil, nil) — same rationale as Views.
func (s *RecordLayerSchemaTemplate) FindView(_ string) (api.View, error) { return nil, nil }

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

// InvokedRoutines is always empty — stored routines (UDFs) are a
// SQL-layer concept not backed by record-layer.
func (s *RecordLayerSchemaTemplate) InvokedRoutines() ([]api.InvokedRoutine, error) {
	return nil, nil
}

// FindInvokedRoutine returns (nil, nil).
func (s *RecordLayerSchemaTemplate) FindInvokedRoutine(_ string) (api.InvokedRoutine, error) {
	return nil, nil
}

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
	// Invoked routines + views are empty in this bridge today —
	// leaving the loops in so the pattern survives when we add them.
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
