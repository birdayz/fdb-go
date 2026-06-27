package catalog

import (
	"sort"
	"sync"

	"fdb.dev/pkg/relational/api"
)

// InMemoryStoreCatalog keeps the entire catalog (databases + schemas)
// in a mutex-protected map. Intended for unit tests and development;
// the FDB-backed implementation lives in a separate type (later shift).
//
// A single InMemoryStoreCatalog instance is safe for concurrent use
// across transactions — the mutex serialises the full state — but
// does NOT implement MVCC. Readers see whatever the latest writer
// committed; aborts are no-ops.
type InMemoryStoreCatalog struct {
	mu        sync.Mutex
	databases map[string]struct{}
	// schemas maps dbURI → schemaName → Schema. Nested map so listing
	// a single database is cheap.
	schemas map[string]map[string]api.Schema
	// Embedded template catalog so we can implement the
	// SchemaTemplateCatalog() accessor trivially.
	templates *InMemorySchemaTemplateCatalog
}

// NewInMemoryStoreCatalog returns a fresh, empty catalog.
func NewInMemoryStoreCatalog() *InMemoryStoreCatalog {
	return &InMemoryStoreCatalog{
		databases: map[string]struct{}{},
		schemas:   map[string]map[string]api.Schema{},
		templates: NewInMemorySchemaTemplateCatalog(),
	}
}

// SchemaTemplateCatalog returns the nested template catalog.
func (c *InMemoryStoreCatalog) SchemaTemplateCatalog() api.SchemaTemplateCatalog {
	return c.templates
}

// LoadSchema looks up a Schema by (databaseID, schemaName).
//
// Java compliance: RecordLayerStoreCatalog.loadSchema always returns
// UNDEFINED_SCHEMA regardless of whether the database exists or only
// the schema is missing. The underlying primary key Tuple is
// (RECORD_TYPE_KEY, dbPath, schemaName) so "no record found" is the
// only observable state — we collapse both misses to ErrCodeUndefinedSchema
// to match.
func (c *InMemoryStoreCatalog) LoadSchema(txn api.Transaction, databaseID, schemaName string) (api.Schema, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.schemas[databaseID][schemaName]
	if !ok {
		return nil, api.NewErrorf(api.ErrCodeUndefinedSchema, "Schema <%s/%s> does not exist in the catalog!", databaseID, schemaName)
	}
	return s, nil
}

// SaveSchema persists or updates schema. Creates the owning database
// first when createDatabaseIfNecessary is true and it doesn't exist.
//
// Java compliance (RecordLayerStoreCatalog.saveSchema):
//  1. If database missing and !createDatabaseIfNecessary → UNDEFINED_DATABASE.
//  2. The schema template referenced by the Schema (name, version) MUST
//     exist in the SchemaTemplateCatalog, else UNKNOWN_SCHEMA_TEMPLATE.
//  3. Otherwise upsert the (db, schema) entry.
//
// Note the error codes: UNDEFINED_DATABASE here, not UNKNOWN_DATABASE —
// those are two distinct SQLSTATEs in Java.
func (c *InMemoryStoreCatalog) SaveSchema(txn api.Transaction, dataToWrite api.Schema, createDatabaseIfNecessary bool) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	if err := validateSchema(dataToWrite); err != nil {
		return err
	}
	dbID := dataToWrite.DatabaseName()
	name := dataToWrite.MetadataName()

	// Template-existence check first — uses the SchemaTemplateCatalog
	// without holding our own mutex (the template catalog has its
	// own). Matches Java's order-of-checks.
	tmpl := dataToWrite.SchemaTemplate()
	if tmpl != nil {
		exists, err := c.templates.DoesSchemaTemplateExistAtVersion(txn, tmpl.MetadataName(), tmpl.Version())
		if err != nil {
			return err
		}
		if !exists {
			return api.NewErrorf(api.ErrCodeUnknownSchemaTemplate,
				"Cannot create schema %s because schema template %s version %d does not exist.",
				name, tmpl.MetadataName(), tmpl.Version())
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.databases[dbID]; !ok {
		if !createDatabaseIfNecessary {
			return api.NewErrorf(api.ErrCodeUndefinedDatabase,
				"Cannot create schema %s because database %s does not exist.", name, dbID)
		}
		c.databases[dbID] = struct{}{}
	}
	if c.schemas[dbID] == nil {
		c.schemas[dbID] = map[string]api.Schema{}
	}
	c.schemas[dbID][name] = dataToWrite
	return nil
}

// RepairSchema rebinds schemaName to the latest version of its
// owning template. For the in-memory impl that means re-running
// templates.LoadSchemaTemplate and re-generating the schema — no
// storage rewrite is required.
//
// Looking up the template is a separate-mutex call, so we copy the
// template name out under our lock, release, call
// templates.LoadSchemaTemplate, and then re-acquire our lock AND
// re-check the database/schema still exist before writing. A
// concurrent DeleteDatabase/DeleteSchema between the two lock
// sections would otherwise panic on a nil-map write.
//
// TOCTOU note: between the two lock sections a concurrent SaveSchema
// could replace the schema with one pointing at a different template.
// We'd then rebind to the OLD template name, overwriting the newer
// entry. This matches Java's FDB-backed impl, which uses row-level
// locking and has the same window. Not worth a fix for the in-memory
// bridge; the FDB-backed catalog will inherit Java's semantics.
func (c *InMemoryStoreCatalog) RepairSchema(txn api.Transaction, databaseID, schemaName string) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	c.mu.Lock()
	existing, ok := c.schemas[databaseID][schemaName]
	c.mu.Unlock()
	if !ok {
		return api.NewErrorf(api.ErrCodeUndefinedSchema, "Schema <%s/%s> does not exist in the catalog!", databaseID, schemaName)
	}
	tmplName := existing.SchemaTemplate().MetadataName()
	latest, err := c.templates.LoadSchemaTemplate(txn, tmplName)
	if err != nil {
		return err
	}
	refreshed := latest.GenerateSchema(databaseID, schemaName)

	c.mu.Lock()
	defer c.mu.Unlock()
	byDB, ok := c.schemas[databaseID]
	if !ok {
		// Database was deleted between our read and our write.
		return api.NewErrorf(api.ErrCodeUndefinedSchema,
			"Schema <%s/%s> was deleted during repair", databaseID, schemaName)
	}
	if _, ok := byDB[schemaName]; !ok {
		return api.NewErrorf(api.ErrCodeUndefinedSchema,
			"Schema <%s/%s> was deleted during repair", databaseID, schemaName)
	}
	byDB[schemaName] = refreshed
	return nil
}

// CreateDatabase records a new database. Returns
// ErrCodeDatabaseAlreadyExists when dbURI is already present.
func (c *InMemoryStoreCatalog) CreateDatabase(txn api.Transaction, dbURI string) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.databases[dbURI]; ok {
		return api.NewErrorf(api.ErrCodeDatabaseAlreadyExists, "database %q already exists", dbURI)
	}
	c.databases[dbURI] = struct{}{}
	if c.schemas[dbURI] == nil {
		c.schemas[dbURI] = make(map[string]api.Schema)
	}
	return nil
}

// ListDatabases returns a sorted ResultSet of (database_name) rows.
// Continuations are ignored for the in-memory impl.
func (c *InMemoryStoreCatalog) ListDatabases(txn api.Transaction, _ api.Continuation) (api.ResultSet, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	names := make([]string, 0, len(c.databases))
	for name := range c.databases {
		names = append(names, name)
	}
	c.mu.Unlock()
	sort.Strings(names)

	rows := make([][]any, len(names))
	for i, n := range names {
		rows[i] = []any{n}
	}
	return newStringResultSet([]string{ColDatabaseID}, rows), nil
}

// ListSchemas returns a ResultSet of (database_name, schema_name)
// rows across every database.
func (c *InMemoryStoreCatalog) ListSchemas(txn api.Transaction, _ api.Continuation) (api.ResultSet, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	dbNames := make([]string, 0, len(c.schemas))
	for dbName := range c.schemas {
		dbNames = append(dbNames, dbName)
	}
	sort.Strings(dbNames)
	var rows [][]any
	for _, dbName := range dbNames {
		byDB := c.schemas[dbName]
		schemaNames := make([]string, 0, len(byDB))
		for schemaName := range byDB {
			schemaNames = append(schemaNames, schemaName)
		}
		sort.Strings(schemaNames)
		for _, schemaName := range schemaNames {
			s := byDB[schemaName]
			tmpl := s.SchemaTemplate()
			rows = append(rows, []any{dbName, schemaName, tmpl.MetadataName(), tmpl.Version()})
		}
	}
	c.mu.Unlock()
	return newStringResultSet([]string{ColDatabaseID, ColSchemaName, ColTemplateName, ColTemplateVersion}, rows), nil
}

// ListSchemasInDatabase narrows ListSchemas to a single database.
func (c *InMemoryStoreCatalog) ListSchemasInDatabase(txn api.Transaction, databaseID string, _ api.Continuation) (api.ResultSet, error) {
	if err := checkOpenTxn(txn); err != nil {
		return nil, err
	}
	c.mu.Lock()
	byDB, ok := c.schemas[databaseID]
	if !ok {
		c.mu.Unlock()
		return nil, api.NewErrorf(api.ErrCodeUnknownDatabase, "database %q does not exist", databaseID)
	}
	names := make([]string, 0, len(byDB))
	for name := range byDB {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][]any, len(names))
	for i, n := range names {
		s := byDB[n]
		tmpl := s.SchemaTemplate()
		rows[i] = []any{databaseID, n, tmpl.MetadataName(), tmpl.Version()}
	}
	c.mu.Unlock()
	return newStringResultSet([]string{ColDatabaseID, ColSchemaName, ColTemplateName, ColTemplateVersion}, rows), nil
}

// DeleteSchema removes (dbURI, schemaName).
func (c *InMemoryStoreCatalog) DeleteSchema(txn api.Transaction, dbURI, schemaName string) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byDB, ok := c.schemas[dbURI]
	if !ok {
		return api.NewErrorf(api.ErrCodeUnknownDatabase, "database %q does not exist", dbURI)
	}
	if _, ok := byDB[schemaName]; !ok {
		return api.NewErrorf(api.ErrCodeUndefinedSchema, "schema %q not found in database %q", schemaName, dbURI)
	}
	delete(byDB, schemaName)
	return nil
}

// DoesDatabaseExist returns true iff dbURI is present.
func (c *InMemoryStoreCatalog) DoesDatabaseExist(txn api.Transaction, dbURI string) (bool, error) {
	if err := checkOpenTxn(txn); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.databases[dbURI]
	return ok, nil
}

// DoesSchemaExist returns true iff (dbURI, schemaName) resolves.
func (c *InMemoryStoreCatalog) DoesSchemaExist(txn api.Transaction, dbURI, schemaName string) (bool, error) {
	if err := checkOpenTxn(txn); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.schemas[dbURI][schemaName]
	return ok, nil
}

// DeleteDatabase removes a database and all its schemas. Always
// returns true on success — the in-memory path never partially
// completes (no time limit).
func (c *InMemoryStoreCatalog) DeleteDatabase(txn api.Transaction, dbURI string, throwIfDoesNotExist bool) (bool, error) {
	if err := checkOpenTxn(txn); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.databases[dbURI]; !ok {
		if throwIfDoesNotExist {
			return false, api.NewErrorf(api.ErrCodeUnknownDatabase, "database %q does not exist", dbURI)
		}
		return true, nil
	}
	delete(c.databases, dbURI)
	delete(c.schemas, dbURI)
	return true, nil
}

// validateSchema mirrors Java's
// com.apple.foundationdb.relational.recordlayer.catalog.CatalogValidator.validateSchema:
// the four fields SaveSchema writes as primary key / FK components
// (schema_name, database_id, schema_template_name, schema_version)
// must all be set and the version must be non-negative.
func validateSchema(s api.Schema) error {
	if s == nil {
		return api.NewError(api.ErrCodeInvalidParameter, "schema is nil")
	}
	if s.MetadataName() == "" {
		return api.NewError(api.ErrCodeInvalidParameter, "Field schema_name in Schema must be set!")
	}
	if s.DatabaseName() == "" {
		return api.NewError(api.ErrCodeInvalidParameter, "Field database_id in Schema must be set!")
	}
	tmpl := s.SchemaTemplate()
	if tmpl == nil || tmpl.MetadataName() == "" {
		return api.NewError(api.ErrCodeInvalidParameter, "Field schema_template_name in Schema must be set!")
	}
	if tmpl.Version() < 0 {
		return api.NewError(api.ErrCodeInvalidParameter, "Field schema_version cannot be < 0!")
	}
	return nil
}

// checkOpenTxn confirms txn is an open InMemoryTransaction. Returns
// ErrCodeTransactionInactive on closed transactions and an internal
// error on any other Transaction impl (catch misuse early). Uses
// Unwrap() so a decorator that forwards Unwrap still passes.
func checkOpenTxn(txn api.Transaction) error {
	if txn == nil {
		return api.NewError(api.ErrCodeTransactionInactive, "transaction is nil")
	}
	raw := txn.Unwrap()
	imt, ok := raw.(*InMemoryTransaction)
	if !ok {
		return api.NewErrorf(api.ErrCodeInternalError,
			"in-memory catalog requires a transaction whose Unwrap() returns *InMemoryTransaction, got %T from %T",
			raw, txn)
	}
	return imt.checkOpen()
}

// Compile-time interface-conformance check.
var _ api.StoreCatalog = (*InMemoryStoreCatalog)(nil)
