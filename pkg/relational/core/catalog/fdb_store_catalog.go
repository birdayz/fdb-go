package catalog

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
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
		return nil, fmt.Errorf("build catalog metadata: %w", err)
	}
	c := &RecordLayerStoreCatalog{
		catalogSubspace: catalogSubspace,
		catalogMD:       md,
	}
	c.templateCatalog = &RecordLayerStoreSchemaTemplateCatalog{parent: c}
	return c, nil
}

// DefaultCatalogSubspace returns the standard __SYS/CATALOG subspace.
// Mirrors Java's RelationalKeyspaceProvider layout for cross-language
// compatibility.
func DefaultCatalogSubspace() subspace.Subspace {
	return subspace.Sub(SysConstant, CatalogConstant)
}

// OpenRecordLayerStoreCatalog is the standard entry point — opens the
// catalog at the canonical __SYS/CATALOG subspace.
func OpenRecordLayerStoreCatalog() (*RecordLayerStoreCatalog, error) {
	return NewRecordLayerStoreCatalog(DefaultCatalogSubspace())
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
		return nil, fmt.Errorf("open catalog store: %w", err)
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
		return fmt.Errorf("save schema: %w", err)
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
		return fmt.Errorf("save database: %w", err)
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

// DeleteDatabase removes dbURI and every schema within. Java returns
// false + no error when the transaction times out mid-deletion; we
// surface any FDB error instead, since the Go driver doesn't model
// TRANSACTION_INACTIVE the same way. throwIfDoesNotExist=true raises
// ErrCodeUnknownDatabase for an already-absent dbURI.
func (c *RecordLayerStoreCatalog) DeleteDatabase(txn api.Transaction, dbURI string, throwIfDoesNotExist bool) (bool, error) {
	if _, err := c.openStore(txn); err != nil {
		return false, err
	}
	_ = dbURI
	_ = throwIfDoesNotExist
	// TODO (next shift): scan Schemas with PK prefix [0, dbURI], delete
	// each, then delete the Databases row. Java uses cursor + delete per
	// record with a mid-scan timeout fallback; port the same shape here.
	return false, api.NewError(api.ErrCodeInternalError,
		"DeleteDatabase is not implemented yet — use ListSchemasInDatabase + DeleteSchema + (future) DeleteDatabaseRow until the range-delete path lands")
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

// ListDatabases returns an api.ResultSet over every database. Today
// it materialises the full list in memory — fine for the catalog's
// expected scale (hundreds, not millions). Continuation support is
// deferred.
func (c *RecordLayerStoreCatalog) ListDatabases(txn api.Transaction, _ api.Continuation) (api.ResultSet, error) {
	store, err := c.openStore(txn)
	if err != nil {
		return nil, err
	}
	cursor := store.ScanRecordsByType(DatabasesRecordName, nil, recordlayer.ForwardScan())
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
	cursor := store.ScanRecordsByType(SchemasRecordName, nil, recordlayer.ForwardScan())
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
		if databaseID != "" && s.GetDATABASE_ID() != databaseID {
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
