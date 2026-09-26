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
	// catalogTemplate and catalogSchema are the catalog's own template and
	// its /__SYS/CATALOG schema, built once, as Java's constructor builds
	// catalogSchemaTemplate and catalogSchema.
	catalogTemplate api.SchemaTemplate
	catalogSchema   api.Schema
}

// NewRecordLayerStoreCatalog constructs the catalog rooted at the
// given __SYS/CATALOG subspace. Callers typically use
// OpenRecordLayerStoreCatalog() which bakes in the standard layout.
func NewRecordLayerStoreCatalog(catalogSubspace subspace.Subspace) (*RecordLayerStoreCatalog, error) {
	md, err := BuildCatalogMetaData()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "build catalog metadata")
	}
	catalogTmpl, err := buildCatalogTemplate()
	if err != nil {
		return nil, api.WrapErrorf(err, api.ErrCodeInternalError, "build catalog template")
	}
	c := &RecordLayerStoreCatalog{
		catalogSubspace: catalogSubspace,
		catalogMD:       md,
		catalogTemplate: catalogTmpl,
		catalogSchema:   catalogTmpl.GenerateSchema(SysDatabaseID, CatalogConstant),
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
// to this function from the driver is tracked in TODO.md, "Go SQL driver
// stores the relational catalog and user schemas on a Go-only keyspace". Callers reading
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
//  2. Persists the catalog schema (/__SYS/CATALOG), creating the /__SYS
//     database row if necessary, with ERROR_IF_DIFFERENT.
//
// Java's initialize, in its order: open the catalog store, create the
// catalog's template when no version of it is stored, then saveSchema(txn,
// catalogSchema, true, ERROR_IF_DIFFERENT), which writes nothing over an
// identical stored row, so concurrent initializations of an initialized
// catalog do not conflict, and refuses a row bound to a different template.
// Errors propagate as Java's do, with their own codes.
//
// Idempotent: safe to call on every startup.
func (c *RecordLayerStoreCatalog) Initialize(txn api.Transaction) error {
	if _, err := c.openStore(txn); err != nil {
		return err
	}
	tc := c.templateCatalog
	exists, err := tc.DoesSchemaTemplateExist(txn, CatalogTemplateName)
	if err != nil {
		return err
	}
	if !exists {
		if err := tc.CreateTemplate(txn, c.catalogTemplate); err != nil {
			return err
		}
	}
	return c.SaveSchema(txn, c.catalogSchema, true, api.SchemaExistsErrorIfDifferent)
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
	s, err := c.loadSchemaIfExists(txn, store, databaseID, schemaName)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, api.NewErrorf(api.ErrCodeUndefinedSchema,
			"Schema <%s/%s> does not exist in the catalog!", databaseID, schemaName)
	}
	return s, nil
}

// loadSchemaIfExists is Java's loadSchemaIfExists: the stored row with its
// template (parseSchemaTable), or nil when no row is stored. A row whose
// template version is gone fails with Java's "SchemaTemplate=<n>,
// version=<v> is not in catalog" (UNKNOWN_SCHEMA_TEMPLATE).
func (c *RecordLayerStoreCatalog) loadSchemaIfExists(txn api.Transaction, store *recordlayer.FDBRecordStore, databaseID, schemaName string) (api.Schema, error) {
	rec, err := store.LoadRecord(schemaKey(databaseID, schemaName))
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
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

// SaveSchema persists a Schema, in Java's saveSchema order: validate the
// schema; the database exists (created when createDatabaseIfNecessary,
// else UNDEFINED_DATABASE); the template exists at the schema's version
// (UNKNOWN_SCHEMA_TEMPLATE); load the stored row with its template, which
// fails when that template version is gone; existsBehavior decides a save
// over a stored row; write. A save that existsBehavior makes a no-op writes
// nothing, so it adds no write conflict range.
func (c *RecordLayerStoreCatalog) SaveSchema(txn api.Transaction, s api.Schema, createDatabaseIfNecessary bool, existsBehavior api.SchemaExistsBehavior) error {
	store, err := c.openStore(txn)
	if err != nil {
		return err
	}
	if err := validateSchema(s); err != nil {
		return err
	}

	dbExists, err := doesDatabaseExistOnStore(store, s.DatabaseName())
	if err != nil {
		return err
	}
	if !dbExists {
		if !createDatabaseIfNecessary {
			return api.NewErrorf(api.ErrCodeUndefinedDatabase,
				"Cannot create schema %s because database %s does not exist.",
				s.MetadataName(), s.DatabaseName())
		}
		if err := createDatabaseOnStore(store, s.DatabaseName()); err != nil {
			return err
		}
	}

	tmpl := s.SchemaTemplate()
	tmplExists, err := c.templateCatalog.DoesSchemaTemplateExistAtVersion(txn,
		tmpl.MetadataName(), tmpl.Version())
	if err != nil {
		return err
	}
	if !tmplExists {
		return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
			"Cannot create schema %s because schema template %s version %d does not exist.",
			s.MetadataName(), tmpl.MetadataName(), tmpl.Version())
	}

	existing, err := c.loadSchemaIfExists(txn, store, s.DatabaseName(), s.MetadataName())
	if err != nil {
		return err
	}
	if existing != nil {
		write, err := existsBehavior.ShouldWrite(s, existing)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		if err := validateSchemaRebind(existing, s); err != nil {
			return err
		}
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

// validateSchemaRebind is Go's check on a save that overwrites a stored
// schema row, a Go extension beside Java's saveSchema. existsBehavior has
// already decided the save: only UPGRADE writes over a stored row, and only
// onto a strictly greater version of the same template, so the names agree
// and the version advances. What it adds is the ported
// MetaDataEvolutionValidator between the bound metadata and the new one: the
// record type key is the LEADING element of every stored record key, so a
// key-changing evolution makes the store read old type A's rows as new type
// B's, silent data corruption, not an error. This is the validator the frl
// CLI's `meta evolve-check` runs (the MetaDataEvolutionValidator.java:418
// port), with the options set here; `--allow-no-version-change` reproduces
// them. Index rebuilds are allowed, as CreateTemplate's carry allows them:
// the new version's CHANGED and NEW indexes sit above the stored metadata
// version, and the store rebuilds exactly those when it next opens under
// the rebound template (checkRebuildIndexes). The template VERSION advances
// while the record-layer METADATA version may not (it is seeded from the
// template version and bumped per index, RecordMetaDataBuilder
// .addIndexCommon:1093-1097), and the validator refuses a lower metadata
// version as Java's does (MetaDataEvolutionValidator.java:154): a store
// opened under metadata older than its header cannot be opened at all.
// Both catalogs run it.
func validateSchemaRebind(existing, s api.Schema) error {
	oldTmpl, newTmpl := existing.SchemaTemplate(), s.SchemaTemplate()
	oldRL, oldOK := oldTmpl.(*metadata.RecordLayerSchemaTemplate)
	newRL, newOK := newTmpl.(*metadata.RecordLayerSchemaTemplate)
	if !oldOK || !newOK {
		return api.NewErrorf(api.ErrCodeInternalError,
			"schema rebind of %s/%s cannot be validated: template types %T -> %T",
			s.DatabaseName(), s.MetadataName(), oldTmpl, newTmpl)
	}
	validator := recordlayer.NewMetaDataEvolutionValidator().SetAllowNoVersionChange(true).SetAllowIndexRebuilds(true).Build()
	if verr := validator.Validate(oldRL.Underlying(), newRL.Underlying()); verr != nil {
		return api.WrapErrorf(verr, api.ErrCodeInvalidSchemaTemplate,
			"cannot rebind schema %s/%s from template %s@%d to %s@%d: metadata evolution rejected",
			s.DatabaseName(), s.MetadataName(),
			oldTmpl.MetadataName(), oldTmpl.Version(),
			newTmpl.MetadataName(), newTmpl.Version())
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
// its owning template. Matches Java's repairSchema: load the schema with
// its template (refused when the bound version is gone), load the latest
// version, and save onto it with UPGRADE, a no-op when the schema is
// already on the latest version.
func (c *RecordLayerStoreCatalog) RepairSchema(txn api.Transaction, dbURI, schemaName string) error {
	s, err := c.LoadSchema(txn, dbURI, schemaName)
	if err != nil {
		return err
	}
	tmpl, err := c.templateCatalog.LoadSchemaTemplate(txn, s.SchemaTemplate().MetadataName())
	if err != nil {
		return err
	}
	return c.SaveSchema(txn, tmpl.GenerateSchema(dbURI, schemaName), false, api.SchemaExistsUpgrade)
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
