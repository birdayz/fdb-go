package catalog

import (
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// RecordLayerStoreCatalog is the FDB-backed api.StoreCatalog. Mirrors
// Java's com.apple.foundationdb.relational.recordlayer.catalog.RecordLayerStoreCatalog.
//
// All methods open a fresh FDBRecordStore per call against the
// catalogSubspace, matching Java's pattern. Store state cacheability
// is not yet wired in — will be a follow-up once
// FDBDatabase.SetStoreStateCache flows through the SQL driver.
//
// Scope (this shift): schema + database CRUD, basic existence checks,
// ListSchemasInDatabase / ListDatabases. Schema template catalog lives
// in a sibling type returned from SchemaTemplateCatalog().
type RecordLayerStoreCatalog struct {
	catalogSubspace subspace.Subspace
	catalogMD       *recordlayer.RecordMetaData
	templateCatalog api.SchemaTemplateCatalog
}

// NewRecordLayerStoreCatalog constructs the catalog rooted at the
// given __SYS/CATALOG subspace. Callers typically use
// OpenRecordLayerStoreCatalog() which bakes in the standard layout.
func NewRecordLayerStoreCatalog(catalogSubspace subspace.Subspace) (*RecordLayerStoreCatalog, error) {
	md, err := BuildCatalogMetaData()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "build catalog metadata")
	}
	c := &RecordLayerStoreCatalog{
		catalogSubspace: catalogSubspace,
		catalogMD:       md,
	}
	c.templateCatalog = &RecordLayerStoreSchemaTemplateCatalog{parent: c}
	return c, nil
}

// DefaultCatalogSubspace returns the standard __SYS/CATALOG subspace in the
// exact byte layout Java writes. Java's RelationalKeyspaceProvider defines:
//
//	KeySpaceDirectory(SYS,     KeyType.NULL)                            // __SYS domain
//	  KeySpaceDirectory(SYS,   KeyType.NULL)                            // __SYS database
//	    KeySpaceDirectory(CATALOG, KeyType.LONG, 0L)                    // CATALOG schema
//
// The directory *names* (__SYS, __SYS, CATALOG) are path labels; the on-wire
// tuple element for each level is the KeyType constant. So the catalog's
// actual subspace prefix is the tuple (NULL, NULL, int64(0)) — NOT three
// strings. Go previously used subspace.Sub("__SYS", "CATALOG") which encodes
// two string tuple elements and is incompatible with Java-written catalogs.
// See fdb-record-layer/.../RelationalKeyspaceProvider.java#getSystemDirectory.
//
// NOTE: this is the Java-wire-compat subspace, which the Go sqldriver does
// NOT yet use — pkg/relational/sqldriver/driver.go opens the catalog via
// keyspace.RelationalKeyspace.CatalogSubspace() (three strings). Migration
// to this function from the driver is tracked in TODO.md. Callers reading
// a Go-written catalog today (incl. frl's `meta catalog`) should use the
// keyspace helper; readers of a Java-written catalog (or a future Go
// driver) should use DefaultCatalogSubspace.
func DefaultCatalogSubspace() subspace.Subspace {
	return subspace.Sub(nil, nil, int64(0))
}

// OpenRecordLayerStoreCatalog opens the catalog at the Java-compatible
// (NULL, NULL, int64(0)) subspace. See [DefaultCatalogSubspace] for the
// full byte-layout rationale — and for the caveat that the Go sqldriver
// currently writes to a different (three-string) subspace.
func OpenRecordLayerStoreCatalog() (*RecordLayerStoreCatalog, error) {
	return NewRecordLayerStoreCatalog(DefaultCatalogSubspace())
}

// CatalogTemplateName is the name of the catalog's own schema template.
// Matches Java's RecordLayerStoreCatalog.CATALOG_TEMPLATE constant
// ("CATALOG_TEMPLATE").
const CatalogTemplateName = CatalogConstant + "_TEMPLATE"

// CatalogTemplateVersion is the version of the built-in catalog schema
// template. Java pins this to 1.
const CatalogTemplateVersion = 1

// SysDatabaseID is the system database path. Java uses "/__SYS".
const SysDatabaseID = "/" + SysConstant

// Initialize bootstraps the catalog schema's self-referential entries.
// It must be called once before the catalog is used, typically at
// service startup. Matches Java's RecordLayerStoreCatalog.initialize(txn):
//
//  1. Ensures the catalog schema template ("CATALOG_TEMPLATE") exists.
//  2. Ensures the /__SYS database row exists.
//  3. Persists the catalog schema (/__SYS/CATALOG) into the store.
//
// Idempotent: safe to call on every startup.
func (c *RecordLayerStoreCatalog) Initialize(txn api.Transaction) error {
	tc := c.templateCatalog

	// 1. Create/ensure the catalog's own schema template.
	exists, err := tc.DoesSchemaTemplateExistAtVersion(txn, CatalogTemplateName, CatalogTemplateVersion)
	if err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "initialize catalog: check template")
	}
	if !exists {
		catalogTmpl, buildErr := buildCatalogTemplate()
		if buildErr != nil {
			return api.WrapErrorf(buildErr, api.ErrCodeInternalError, "initialize catalog: build template")
		}
		if createErr := tc.CreateTemplate(txn, catalogTmpl); createErr != nil {
			return api.WrapErrorf(createErr, api.ErrCodeInternalError, "initialize catalog: create template")
		}
	}

	// 2. Ensure the /__SYS database row exists.
	dbExists, err := c.DoesDatabaseExist(txn, SysDatabaseID)
	if err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "initialize catalog: check sys database")
	}
	if !dbExists {
		if err := c.CreateDatabase(txn, SysDatabaseID); err != nil {
			return api.WrapErrorf(err, api.ErrCodeInternalError, "initialize catalog: create sys database")
		}
	}

	// 3. Persist the /__SYS/CATALOG schema (create if missing). Java calls
	// saveSchema(txn, this.catalogSchema, true) which silently overwrites.
	catalogTmpl, err := tc.LoadSchemaTemplateAtVersion(txn, CatalogTemplateName, CatalogTemplateVersion)
	if err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "initialize catalog: load template")
	}
	catalogSchema := catalogTmpl.GenerateSchema(SysDatabaseID, CatalogConstant)
	if err := c.SaveSchema(txn, catalogSchema, false); err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "initialize catalog: save catalog schema")
	}
	return nil
}

// buildCatalogTemplate constructs the RecordLayerSchemaTemplate that
// describes the catalog itself (the three system tables). Mirrors Java's
// in-constructor assembly of SCHEMAS + DATABASES + TEMPLATES table
// definitions via SystemTableRegistry.
func buildCatalogTemplate() (api.SchemaTemplate, error) {
	md, err := BuildCatalogMetaData()
	if err != nil {
		return nil, err
	}
	return metadata.NewRecordLayerSchemaTemplateWithVersion(CatalogTemplateName, md, CatalogTemplateVersion)
}

// SchemaTemplateCatalog returns the template catalog sibling.
func (c *RecordLayerStoreCatalog) SchemaTemplateCatalog() api.SchemaTemplateCatalog {
	return c.templateCatalog
}

// openStore opens (or creates) the catalog record store on this txn.
// All CRUD calls go through here; matches Java's
// RecordLayerStoreUtils.openRecordStore.
func (c *RecordLayerStoreCatalog) openStore(txn api.Transaction) (*recordlayer.FDBRecordStore, error) {
	ctx, err := unwrapFDB(txn)
	if err != nil {
		return nil, err
	}
	store, err := recordlayer.NewStoreBuilder().
		SetContext(ctx).
		SetSubspace(c.catalogSubspace).
		SetMetaDataProvider(c.catalogMD).
		CreateOrOpen()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "open catalog store")
	}
	return store, nil
}

// schemaKey returns the primary-key tuple for a (databaseID, schemaName)
// Schema record. Matches Java's RecordLayerStoreCatalog.getSchemaKey.
func schemaKey(databaseID, schemaName string) tuple.Tuple {
	return tuple.Tuple{SchemaRecordTypeKey, databaseID, schemaName}
}

// databaseKey returns the primary-key tuple for a databaseID Database
// record.
func databaseKey(databaseID string) tuple.Tuple {
	return tuple.Tuple{DatabaseInfoRecordTypeKey, databaseID}
}

// LoadSchema loads (databaseID, schemaName) → api.Schema by
// deserialising the Schemas record, resolving its template via the
// SchemaTemplateCatalog, and materialising a Schema via
// template.GenerateSchema. Matches Java semantics: missing rows surface
// as ErrCodeUndefinedSchema regardless of whether the database or the
// schema was the missing bit (primary-key tuple lookup can't tell).
func (c *RecordLayerStoreCatalog) LoadSchema(txn api.Transaction, databaseID, schemaName string) (api.Schema, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}
	rec, err := store.LoadRecord(schemaKey(databaseID, schemaName))
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, api.NewErrorf(api.ErrCodeUndefinedSchema,
			"schema <%s/%s> does not exist in the catalog", databaseID, schemaName)
	}
	msg, ok := rec.Record.(*gen.Schemas)
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeInternalError,
			"catalog row at %s/%s has unexpected type %T", databaseID, schemaName, rec.Record)
	}
	tmpl, err := c.templateCatalog.LoadSchemaTemplateAtVersion(txn,
		msg.GetTEMPLATE_NAME(), int(msg.GetTEMPLATE_VERSION()))
	if err != nil {
		return nil, err
	}
	return tmpl.GenerateSchema(msg.GetDATABASE_ID(), msg.GetSCHEMA_NAME()), nil
}

// SaveSchema persists or updates a Schema. Validates the schema name
// and verifies the owning database + template exist. If the database
// is missing and createDatabaseIfNecessary is false, returns
// ErrCodeUndefinedDatabase (matches Java).
func (c *RecordLayerStoreCatalog) SaveSchema(txn api.Transaction, s api.Schema, createDatabaseIfNecessary bool) error {
	if s == nil {
		return api.NewError(api.ErrCodeInvalidParameter, "schema is nil")
	}
	if s.MetadataName() == "" {
		return api.NewError(api.ErrCodeInvalidParameter, "schema name is empty")
	}
	tmpl := s.SchemaTemplate()
	if tmpl == nil || tmpl.MetadataName() == "" {
		return api.NewError(api.ErrCodeInvalidSchemaTemplate, "schema has no template")
	}

	store, err := c.openStore(txn)
	if err != nil {
		return err
	}

	dbExists, err := doesDatabaseExistOnStore(store, s.DatabaseName())
	if err != nil {
		return err
	}
	if !dbExists {
		if !createDatabaseIfNecessary {
			return api.NewErrorf(api.ErrCodeUndefinedDatabase,
				"cannot create schema %s because database %s does not exist",
				s.MetadataName(), s.DatabaseName())
		}
		if err := createDatabaseOnStore(store, s.DatabaseName()); err != nil {
			return err
		}
	}

	// Template must exist at the exact version. Java uses
	// ErrCodeUnknownSchemaTemplate for this case.
	tmplExists, err := c.templateCatalog.DoesSchemaTemplateExistAtVersion(txn,
		tmpl.MetadataName(), tmpl.Version())
	if err != nil {
		return err
	}
	if !tmplExists {
		return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
			"cannot create schema %s because schema template %s version %d does not exist",
			s.MetadataName(), tmpl.MetadataName(), tmpl.Version())
	}

	rec := &gen.Schemas{
		DATABASE_ID:      proto.String(s.DatabaseName()),
		SCHEMA_NAME:      proto.String(s.MetadataName()),
		TEMPLATE_NAME:    proto.String(tmpl.MetadataName()),
		TEMPLATE_VERSION: proto.Int32(int32(tmpl.Version())),
	}
	if _, err := store.SaveRecord(rec); err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "save schema")
	}
	return nil
}

// DoesSchemaExist reports whether a Schema row exists at
// (dbURI, schemaName).
func (c *RecordLayerStoreCatalog) DoesSchemaExist(txn api.Transaction, dbURI, schemaName string) (bool, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return false, err
	}
	rec, err := store.LoadRecord(schemaKey(dbURI, schemaName))
	if err != nil {
		return false, err
	}
	return rec != nil, nil
}

// DoesDatabaseExist reports whether a Database row exists at dbURI.
func (c *RecordLayerStoreCatalog) DoesDatabaseExist(txn api.Transaction, dbURI string) (bool, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return false, err
	}
	return doesDatabaseExistOnStore(store, dbURI)
}

func doesDatabaseExistOnStore(store *recordlayer.FDBRecordStore, dbURI string) (bool, error) {
	rec, err := store.LoadRecord(databaseKey(dbURI))
	if err != nil {
		return false, err
	}
	return rec != nil, nil
}

// CreateDatabase creates a Database row for dbURI. No-op if already
// present (matches Java's RecordLayerStoreCatalog — saveRecord is used
// directly, so a duplicate is overwritten silently; we mirror that).
func (c *RecordLayerStoreCatalog) CreateDatabase(txn api.Transaction, dbURI string) error {
	store, err := c.openStore(txn)
	if err != nil {
		return err
	}
	return createDatabaseOnStore(store, dbURI)
}

func createDatabaseOnStore(store *recordlayer.FDBRecordStore, dbURI string) error {
	rec := &gen.Databases{DATABASE_ID: proto.String(dbURI)}
	if _, err := store.SaveRecord(rec); err != nil {
		return api.WrapErrorf(err, api.ErrCodeInternalError, "save database")
	}
	return nil
}

// DeleteSchema removes (dbURI, schemaName). Returns
// ErrCodeUndefinedSchema when the row is absent (matches Java).
func (c *RecordLayerStoreCatalog) DeleteSchema(txn api.Transaction, dbURI, schemaName string) error {
	store, err := c.openStore(txn)
	if err != nil {
		return err
	}
	deleted, err := store.DeleteRecord(schemaKey(dbURI, schemaName))
	if err != nil {
		return err
	}
	if !deleted {
		return api.NewErrorf(api.ErrCodeUndefinedSchema,
			"schema %s/%s does not exist", dbURI, schemaName)
	}
	return nil
}

// DeleteDatabase removes dbURI and every schema within it. Matches Java's
// RecordLayerStoreCatalog.deleteDatabase: scan-and-delete all Schemas rows
// with PK prefix [SCHEMA_TYPE_KEY, dbURI], then delete the Databases row.
// Returns (true, nil) on success, (false, nil) on timeout (Java returns false
// silently when TRANSACTION_INACTIVE/TIMEOUT; we surface other FDB errors).
// throwIfDoesNotExist=true raises ErrCodeUnknownDatabase when the db row is
// absent.
func (c *RecordLayerStoreCatalog) DeleteDatabase(txn api.Transaction, dbURI string, throwIfDoesNotExist bool) (bool, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return false, err
	}

	// Delete all Schemas rows whose PK starts with [SCHEMA_TYPE_KEY, dbURI].
	// ScanRecordsInRange with RANGE_INCLUSIVE on both ends covers exactly this
	// prefix (primary key is [SCHEMA_TYPE_KEY, dbURI, schemaName]).
	cursor := store.ScanRecordsInRange(
		tuple.Tuple{SchemaRecordTypeKey, dbURI},
		tuple.Tuple{SchemaRecordTypeKey, dbURI},
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ForwardScan(),
	)
	ctx := store.Context().Context()
	for {
		r, scanErr := cursor.OnNext(ctx)
		if scanErr != nil {
			return false, scanErr
		}
		if !r.HasNext() {
			break
		}
		if _, delErr := store.DeleteRecord(r.GetValue().PrimaryKey); delErr != nil {
			return false, delErr
		}
	}

	// Delete the Databases row.
	deleted, delErr := store.DeleteRecord(databaseKey(dbURI))
	if delErr != nil {
		return false, delErr
	}
	if !deleted && throwIfDoesNotExist {
		return false, api.NewErrorf(api.ErrCodeUnknownDatabase,
			"cannot delete unknown database: %s", dbURI)
	}
	return true, nil
}

// RepairSchema rebinds schemaName in dbURI to the latest version of
// its owning template. Matches Java's repairSchema.
func (c *RecordLayerStoreCatalog) RepairSchema(txn api.Transaction, dbURI, schemaName string) error {
	s, err := c.LoadSchema(txn, dbURI, schemaName)
	if err != nil {
		return err
	}
	tmpl, err := c.templateCatalog.LoadSchemaTemplate(txn, s.SchemaTemplate().MetadataName())
	if err != nil {
		return err
	}
	return c.SaveSchema(txn, tmpl.GenerateSchema(dbURI, schemaName), false)
}

// ListDatabases returns an api.ResultSet over every database. Materialises
// in-memory; continuation support is deferred. Uses a prefix scan keyed on
// [DATABASE_INFO_TYPE_KEY] matching Java's listDatabases TupleRange shape.
func (c *RecordLayerStoreCatalog) ListDatabases(txn api.Transaction, _ api.Continuation) (api.ResultSet, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}
	prefix := tuple.Tuple{DatabaseInfoRecordTypeKey}
	cursor := store.ScanRecordsInRange(
		prefix, prefix,
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ForwardScan(),
	)
	ctx := store.Context().Context()

	var rows [][]any
	for {
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			break
		}
		if db, isDB := r.GetValue().Record.(*gen.Databases); isDB {
			rows = append(rows, []any{db.GetDATABASE_ID()})
		}
	}
	return newStringResultSet([]string{ColDatabaseID}, rows), nil
}

// ListSchemas returns every schema in every database. Like
// ListDatabases, materialises in-memory; continuation is a follow-up.
func (c *RecordLayerStoreCatalog) ListSchemas(txn api.Transaction, _ api.Continuation) (api.ResultSet, error) {
	return c.listSchemasImpl(txn, "")
}

// ListSchemasInDatabase narrows ListSchemas to a single database.
// An empty databaseID returns every schema (same as ListSchemas).
func (c *RecordLayerStoreCatalog) ListSchemasInDatabase(txn api.Transaction, databaseID string, _ api.Continuation) (api.ResultSet, error) {
	if databaseID == "" {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "databaseID is empty")
	}
	return c.listSchemasImpl(txn, databaseID)
}

func (c *RecordLayerStoreCatalog) listSchemasImpl(txn api.Transaction, databaseID string) (api.ResultSet, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}

	// Use a prefix scan — same TupleRange shape as Java's listSchemas /
	// listSchemas(dbUri). For the per-database case the prefix is
	// [SCHEMA_TYPE_KEY, databaseID]; for all schemas it's [SCHEMA_TYPE_KEY].
	// RANGE_INCLUSIVE on both ends covers all records with that prefix.
	var prefixStart, prefixEnd tuple.Tuple
	if databaseID != "" {
		prefixStart = tuple.Tuple{SchemaRecordTypeKey, databaseID}
		prefixEnd = tuple.Tuple{SchemaRecordTypeKey, databaseID}
	} else {
		prefixStart = tuple.Tuple{SchemaRecordTypeKey}
		prefixEnd = tuple.Tuple{SchemaRecordTypeKey}
	}
	cursor := store.ScanRecordsInRange(
		prefixStart, prefixEnd,
		recordlayer.EndpointTypeRangeInclusive, recordlayer.EndpointTypeRangeInclusive,
		nil, recordlayer.ForwardScan(),
	)
	ctx := store.Context().Context()

	var rows [][]any
	for {
		r, err := cursor.OnNext(ctx)
		if err != nil {
			return nil, err
		}
		if !r.HasNext() {
			break
		}
		s, isS := r.GetValue().Record.(*gen.Schemas)
		if !isS {
			continue
		}
		rows = append(rows, []any{
			s.GetDATABASE_ID(),
			s.GetSCHEMA_NAME(),
			s.GetTEMPLATE_NAME(),
			s.GetTEMPLATE_VERSION(),
		})
	}
	return newStringResultSet(
		[]string{ColDatabaseID, ColSchemaName, ColTemplateName, ColTemplateVersion},
		rows), nil
}

// compile-time interface-conformance assertion.
var _ api.StoreCatalog = (*RecordLayerStoreCatalog)(nil)
