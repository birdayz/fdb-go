package catalog

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// CatalogDatabaseMetaData implements api.DatabaseMetaData by running
// queries against a StoreCatalog. Suitable for JDBC-style
// introspection tools and for backing sql.DB.Stats-like consumers.
//
// Each query runs in a fresh api.Transaction (new InMemoryTransaction)
// and closes it before returning — the ResultSet does not outlive the
// transaction. Callers iterating the ResultSet see a point-in-time
// snapshot; concurrent catalog changes after the method returns do
// not surface.
//
// The Go DatabaseMetaData interface is a lean subset of Java's
// RelationalDatabaseMetaData — see api/database_metadata.go for
// what's included and what's deferred.
type CatalogDatabaseMetaData struct {
	storeCatalog       api.StoreCatalog
	url                string
	userName           string
	readOnly           bool
	productName        string
	productVersion     string
	driverName         string
	driverVersion      string
	newTransactionFunc func() api.Transaction
}

// CatalogDatabaseMetaDataOptions configures identification fields.
// The storeCatalog is required; everything else is optional and
// defaults to sensible values.
type CatalogDatabaseMetaDataOptions struct {
	// StoreCatalog: required. Backs every discovery query.
	StoreCatalog api.StoreCatalog
	// NewTransaction: factory for a fresh transaction per query. If
	// nil, a new *InMemoryTransaction is used (valid only when the
	// catalog is an InMemoryStoreCatalog).
	NewTransaction func() api.Transaction
	// URL, UserName, ReadOnly, ProductName, ProductVersion, DriverName,
	// DriverVersion: returned by the corresponding accessors.
	URL            string
	UserName       string
	ReadOnly       bool
	ProductName    string
	ProductVersion string
	DriverName     string
	DriverVersion  string
}

// NewCatalogDatabaseMetaData constructs a CatalogDatabaseMetaData.
// Panics if opts.StoreCatalog is nil — DatabaseMetaData without a
// catalog is meaningless.
func NewCatalogDatabaseMetaData(opts CatalogDatabaseMetaDataOptions) *CatalogDatabaseMetaData {
	if opts.StoreCatalog == nil {
		panic("CatalogDatabaseMetaData: StoreCatalog is required")
	}
	newTx := opts.NewTransaction
	if newTx == nil {
		newTx = func() api.Transaction { return NewInMemoryTransaction() }
	}
	if opts.ProductName == "" {
		opts.ProductName = "FoundationDB Relational"
	}
	if opts.DriverName == "" {
		opts.DriverName = "fdbsql"
	}
	return &CatalogDatabaseMetaData{
		storeCatalog:       opts.StoreCatalog,
		url:                opts.URL,
		userName:           opts.UserName,
		readOnly:           opts.ReadOnly,
		productName:        opts.ProductName,
		productVersion:     opts.ProductVersion,
		driverName:         opts.DriverName,
		driverVersion:      opts.DriverVersion,
		newTransactionFunc: newTx,
	}
}

// Schemas returns every schema in every catalog. Columns match
// JDBC DatabaseMetaData.getSchemas(): (TABLE_SCHEM, TABLE_CATALOG).
// TABLE_CATALOG is the database URI path; Java surfaces it as the
// "catalog" value in JDBC terminology.
func (m *CatalogDatabaseMetaData) Schemas(ctx context.Context) (api.ResultSet, error) {
	return m.SchemasFiltered(ctx, "", "")
}

// SchemasFiltered narrows Schemas by catalog + schema LIKE patterns.
// Empty patterns match anything (SQL standard: no filter).
func (m *CatalogDatabaseMetaData) SchemasFiltered(_ context.Context, catalog, schemaPattern string) (api.ResultSet, error) {
	tx := m.newTransactionFunc()
	defer tx.Close()

	rs, err := m.storeCatalog.ListSchemas(tx, nil)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	catalogRE := compileLikePattern(catalog)
	schemaRE := compileLikePattern(schemaPattern)

	var rows [][]any
	for rs.Next() {
		db, err := rs.String(1)
		if err != nil {
			return nil, err
		}
		sch, err := rs.String(2)
		if err != nil {
			return nil, err
		}
		if catalogRE != nil && !catalogRE.MatchString(db) {
			continue
		}
		if schemaRE != nil && !schemaRE.MatchString(sch) {
			continue
		}
		rows = append(rows, []any{sch, db})
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i][1].(string) != rows[j][1].(string) {
			return rows[i][1].(string) < rows[j][1].(string)
		}
		return rows[i][0].(string) < rows[j][0].(string)
	})
	return newStringResultSet([]string{"TABLE_SCHEM", "TABLE_CATALOG"}, rows), nil
}

// Tables enumerates every table in every schema matching the
// patterns. types optionally restricts to a subset of table types
// ("TABLE", "VIEW", "SYSTEM TABLE"); nil or empty returns all types.
//
// Since our catalog today has only regular tables (no views, no
// system tables), every returned row has TABLE_TYPE == "TABLE" and
// all type columns (TYPE_CAT / TYPE_SCHEM / TYPE_NAME) are empty.
func (m *CatalogDatabaseMetaData) Tables(_ context.Context, catalog, schemaPattern, tableNamePattern string, types []string) (api.ResultSet, error) {
	if len(types) > 0 {
		allowed := false
		for _, t := range types {
			if strings.EqualFold(t, "TABLE") {
				allowed = true
				break
			}
		}
		if !allowed {
			return newStringResultSet(tablesColumns(), nil), nil
		}
	}

	tx := m.newTransactionFunc()
	defer tx.Close()

	// Snapshot (db, schema) pairs first — we can't keep the ListSchemas
	// ResultSet open while calling LoadSchema because LoadSchema itself
	// advances the catalog mutex.
	listRS, err := m.storeCatalog.ListSchemas(tx, nil)
	if err != nil {
		return nil, err
	}
	type dbSchema struct{ db, schema string }
	var pairs []dbSchema
	for listRS.Next() {
		db, err := listRS.String(1)
		if err != nil {
			listRS.Close()
			return nil, err
		}
		sch, err := listRS.String(2)
		if err != nil {
			listRS.Close()
			return nil, err
		}
		pairs = append(pairs, dbSchema{db, sch})
	}
	if err := listRS.Err(); err != nil {
		listRS.Close()
		return nil, err
	}
	listRS.Close()

	catalogRE := compileLikePattern(catalog)
	schemaRE := compileLikePattern(schemaPattern)
	tableRE := compileLikePattern(tableNamePattern)

	var rows [][]any
	for _, p := range pairs {
		if catalogRE != nil && !catalogRE.MatchString(p.db) {
			continue
		}
		if schemaRE != nil && !schemaRE.MatchString(p.schema) {
			continue
		}
		schema, err := m.storeCatalog.LoadSchema(tx, p.db, p.schema)
		if err != nil {
			return nil, err
		}
		tables, err := schema.Tables()
		if err != nil {
			return nil, err
		}
		for _, tbl := range tables {
			if tableRE != nil && !tableRE.MatchString(tbl.MetadataName()) {
				continue
			}
			rows = append(rows, []any{
				p.db, p.schema, tbl.MetadataName(),
				"TABLE", // TABLE_TYPE
				"",      // REMARKS
				"",      // TYPE_CAT
				"",      // TYPE_SCHEM
				"",      // TYPE_NAME
				"",      // SELF_REFERENCING_COL_NAME
				"",      // REF_GENERATION
			})
		}
	}

	// JDBC's getTables orders by (TABLE_TYPE, TABLE_CAT, TABLE_SCHEM,
	// TABLE_NAME). We only have TABLE type, so the TABLE_TYPE sort
	// is a no-op; keep (cat, schem, name).
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := rows[i], rows[j]
		for _, k := range []int{0, 1, 2} {
			si, sj := ri[k].(string), rj[k].(string)
			if si != sj {
				return si < sj
			}
		}
		return false
	})
	return newStringResultSet(tablesColumns(), rows), nil
}

// PrimaryKeys returns the primary-key columns of a single table. Each
// row is one PK column (key_seq 1..N). Today we surface the
// recordlayer-level primary key tuple as a single "compound key" row —
// the Java side splits into individual column rows when the PK is a
// composite expression, which we'll match once our key-expression
// introspection grows.
func (m *CatalogDatabaseMetaData) PrimaryKeys(_ context.Context, catalog, schema, table string) (api.ResultSet, error) {
	tx := m.newTransactionFunc()
	defer tx.Close()

	sch, err := m.storeCatalog.LoadSchema(tx, catalog, schema)
	if err != nil {
		return nil, err
	}
	tables, err := sch.Tables()
	if err != nil {
		return nil, err
	}
	var target api.Table
	for _, t := range tables {
		if t.MetadataName() == table {
			target = t
			break
		}
	}
	if target == nil {
		return nil, api.NewErrorf(api.ErrCodeUndefinedTable,
			"table %s/%s/%s does not exist", catalog, schema, table)
	}
	// For now, expose a single "PRIMARY_KEY" row — our api.Table
	// doesn't yet surface the PK column list. When it does, widen to
	// one row per column with key_seq 1..N.
	pkName := "PK_" + table
	rows := [][]any{
		{catalog, schema, table, "PRIMARY_KEY", int64(1), pkName},
	}
	return newStringResultSet([]string{
		"TABLE_CAT", "TABLE_SCHEM", "TABLE_NAME",
		"COLUMN_NAME", "KEY_SEQ", "PK_NAME",
	}, rows), nil
}

// Columns is deferred — see database_metadata.go for the 24-column
// JDBC schema. Returning an empty ResultSet keeps the interface
// satisfied; callers that actually need column metadata should
// iterate api.Schema.Tables() directly for now.
func (m *CatalogDatabaseMetaData) Columns(_ context.Context, _, _, _, _ string) (api.ResultSet, error) {
	return newStringResultSet(columnsColumns(), nil), nil
}

// IndexInfo is deferred — same shape decision as Columns.
func (m *CatalogDatabaseMetaData) IndexInfo(_ context.Context, _, _, _ string, _, _ bool) (api.ResultSet, error) {
	return newStringResultSet(indexInfoColumns(), nil), nil
}

// ---- Product / driver identification ----

func (m *CatalogDatabaseMetaData) URL() string                    { return m.url }
func (m *CatalogDatabaseMetaData) UserName() string               { return m.userName }
func (m *CatalogDatabaseMetaData) IsReadOnly() bool               { return m.readOnly }
func (m *CatalogDatabaseMetaData) DatabaseProductName() string    { return m.productName }
func (m *CatalogDatabaseMetaData) DatabaseProductVersion() string { return m.productVersion }
func (m *CatalogDatabaseMetaData) DriverName() string             { return m.driverName }
func (m *CatalogDatabaseMetaData) DriverVersion() string          { return m.driverVersion }

// Compile-time interface check.
var _ api.DatabaseMetaData = (*CatalogDatabaseMetaData)(nil)

// ---- helpers ----

func tablesColumns() []string {
	return []string{
		"TABLE_CAT", "TABLE_SCHEM", "TABLE_NAME",
		"TABLE_TYPE", "REMARKS", "TYPE_CAT", "TYPE_SCHEM", "TYPE_NAME",
		"SELF_REFERENCING_COL_NAME", "REF_GENERATION",
	}
}

func columnsColumns() []string {
	return []string{
		"TABLE_CAT", "TABLE_SCHEM", "TABLE_NAME", "COLUMN_NAME",
		"DATA_TYPE", "TYPE_NAME", "COLUMN_SIZE", "BUFFER_LENGTH",
		"DECIMAL_DIGITS", "NUM_PREC_RADIX", "NULLABLE", "REMARKS",
		"COLUMN_DEF", "SQL_DATA_TYPE", "SQL_DATETIME_SUB",
		"CHAR_OCTET_LENGTH", "ORDINAL_POSITION", "IS_NULLABLE",
		"SCOPE_CATALOG", "SCOPE_SCHEMA", "SCOPE_TABLE",
		"SOURCE_DATA_TYPE", "IS_AUTOINCREMENT", "IS_GENERATEDCOLUMN",
	}
}

func indexInfoColumns() []string {
	return []string{
		"TABLE_CAT", "TABLE_SCHEM", "TABLE_NAME", "NON_UNIQUE",
		"INDEX_QUALIFIER", "INDEX_NAME", "TYPE", "ORDINAL_POSITION",
		"COLUMN_NAME", "ASC_OR_DESC", "CARDINALITY", "PAGES",
		"FILTER_CONDITION",
	}
}

// compileLikePattern turns a SQL LIKE pattern into an anchored regex.
// Empty input → nil (no filter). `%` maps to `.*`, `_` to `.`, all
// other regex metacharacters are escaped. Matching is case-sensitive
// to match JDBC's default behaviour.
//
// Escape sequences (e.g. the SQL ESCAPE clause) are not supported:
// `%` and `_` are always wildcards, with no way for the caller to
// express a literal percent sign or underscore. JDBC's
// DatabaseMetaData.getSearchStringEscape() would advertise a `\`
// escape when this is ported.
func compileLikePattern(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	var b strings.Builder
	b.WriteByte('^')
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	return regexp.MustCompile(b.String())
}
