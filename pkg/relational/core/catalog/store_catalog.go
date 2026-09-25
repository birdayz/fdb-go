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
	// beforeBind, when set, runs in SaveSchema and RepairSchema after their
	// checks and before the bind, with c.mu held: a test's interleaving hook.
	beforeBind func()
}

// NewInMemoryStoreCatalog returns a fresh, empty catalog.
func NewInMemoryStoreCatalog() *InMemoryStoreCatalog {
	c := &InMemoryStoreCatalog{
		databases: map[string]struct{}{},
		schemas:   map[string]map[string]api.Schema{},
		templates: NewInMemorySchemaTemplateCatalog(),
	}
	c.templates.bindings = c
	return c
}

// firstBindingHeld is the version guard's read (template_bindings.go): the
// first schema, in the order of Java's TEMPLATES_VALUE_INDEX (template version,
// database, schema), bound to templateName at a version from `from` through
// `through` (a negative bound is none). The caller holds c.mu, the only lock
// its data needs ("…Held").
func (c *InMemoryStoreCatalog) firstBindingHeld(templateName string, from, through int) *boundSchema {
	var first *boundSchema
	for dbID, byName := range c.schemas {
		for name, s := range byName {
			t := s.SchemaTemplate()
			if t == nil || t.MetadataName() != templateName {
				continue
			}
			v := t.Version()
			if (from >= 0 && v < from) || (through >= 0 && v > through) {
				continue
			}
			b := boundSchema{version: v, databaseID: dbID, schema: name}
			if first == nil || bindingLess(b, *first) {
				first = &b
			}
		}
	}
	return first
}

func bindingLess(a, b boundSchema) bool {
	if a.version != b.version {
		return a.version < b.version
	}
	if a.databaseID != b.databaseID {
		return a.databaseID < b.databaseID
	}
	return a.schema < b.schema
}

// boundTemplateTakingTC is Java's load of a schema row's template
// (parseSchemaTable): the row's (name, version) must be stored, and is
// refused with Java's text when it is gone. The caller holds c.mu and the
// template catalog's read takes tc.mu ("…TakingTC"; the lock order is c.mu,
// then tc.mu).
func (c *InMemoryStoreCatalog) boundTemplateTakingTC(txn api.Transaction, s api.Schema) error {
	t := s.SchemaTemplate()
	if t == nil {
		return nil
	}
	ok, err := c.templates.DoesSchemaTemplateExistAtVersion(txn, t.MetadataName(), t.Version())
	if err != nil {
		return err
	}
	if !ok {
		return errTemplateVersionNotInCatalog(t.MetadataName(), t.Version())
	}
	return nil
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
	if err := c.boundTemplateTakingTC(txn, s); err != nil {
		return nil, err
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

	// The mutex is held from the template check to the write, so the
	// version guard (which takes it before the template catalog's) cannot
	// delete the version between the two.
	c.mu.Lock()
	defer c.mu.Unlock()

	tmpl := dataToWrite.SchemaTemplate()
	if tmpl != nil {
		// Takes tc.mu under c.mu (the lock order).
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

	// Java's saveSchema loads the existing row with its template, which
	// fails when the bound version is gone.
	if existing, ok := c.schemas[dbID][name]; ok {
		if err := c.boundTemplateTakingTC(txn, existing); err != nil {
			return err
		}
	}

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
	if c.beforeBind != nil {
		c.beforeBind()
	}
	c.schemas[dbID][name] = dataToWrite
	return nil
}

// RepairSchema rebinds schemaName to the latest version of its
// owning template, as Java's repairSchema: it loads the schema with its
// template (refused when the bound version is gone) and saves it onto the
// latest version. The mutex is held throughout, so the template read and
// the write see one state.
func (c *InMemoryStoreCatalog) RepairSchema(txn api.Transaction, databaseID, schemaName string) error {
	if err := checkOpenTxn(txn); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.schemas[databaseID][schemaName]
	if !ok {
		return api.NewErrorf(api.ErrCodeUndefinedSchema, "Schema <%s/%s> does not exist in the catalog!", databaseID, schemaName)
	}
	if err := c.boundTemplateTakingTC(txn, existing); err != nil {
		return err
	}
	latest, err := c.templates.LoadSchemaTemplate(txn, existing.SchemaTemplate().MetadataName())
	if err != nil {
		return err
	}
	if c.beforeBind != nil {
		c.beforeBind()
	}
	c.schemas[databaseID][schemaName] = latest.GenerateSchema(databaseID, schemaName)
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
