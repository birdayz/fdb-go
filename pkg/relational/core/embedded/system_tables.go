package embedded

import (
	"context"
	"database/sql/driver"
	"strings"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// INFORMATION_SCHEMA.* query handlers + SHOW DATABASES / SHOW SCHEMA
// TEMPLATES. Each handler assembles a static result set from the
// catalog (no FDB data path) and hands it back as a staticRows.
//
// filterSysRows re-applies the WHERE clause against the in-memory
// rows via evalPredicateOnMap, so standard SQL predicates work over
// INFORMATION_SCHEMA / SHOW rows the same as over table rows.

// execSystemTableQuery is the executor-free entry point for an
// INFORMATION_SCHEMA.* SELECT. It serves the simple shape
// `SELECT [*|cols] FROM INFORMATION_SCHEMA.X [WHERE …] [ORDER BY …]
// [LIMIT … OFFSET …]` directly off the catalog-synthesized rows. RFC-145
// detached this from the legacy embedded interpreter (Phase 1) before that
// interpreter island was deleted (Phase 2); it never routes through an
// executor.
//
// INFORMATION_SCHEMA is a Go-only extension Java rejects entirely, so no
// cross-engine reference exists for any shape here. The simple-SELECT subset
// is the only shape ever used; any join / aggregate / GROUP BY / HAVING /
// DISTINCT / QUALIFY / CTE / derived-table / set-query (UNION) against a
// system table is rejected with a clean error (verified none are used today).
// Subqueries / EXISTS embedded in the WHERE filter surface the severed-arm
// error from filterSysRows → evalPredicateOnMapExpr (RFC-145 Phase 1).
func (c *EmbeddedConnection) execSystemTableQuery(ctx context.Context, sel antlrgen.ISelectStatementContext, q antlrgen.IQueryContext) (driver.Rows, error) {
	// WITH / WITH RECURSIVE against a system table is not supported.
	if q.Ctes() != nil {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"WITH clause is not supported against INFORMATION_SCHEMA")
	}
	// UNION / set-query and any non-simple query term are rejected by
	// extractSelectParts (it only accepts a QueryTermDefaultContext over a
	// SimpleTableContext), with a clean ErrCodeUnsupportedOperation error.
	sq, err := extractSelectParts(sel)
	if err != nil {
		return nil, err
	}

	// Reject every shape the simple system-table handler can't serve. These
	// are mutually exclusive with the plain `SELECT … FROM INFORMATION_SCHEMA.X`
	// projection that projectSystemRows applies.
	switch {
	case len(sq.joins) > 0:
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"JOIN is not supported against INFORMATION_SCHEMA")
	case sq.derivedQuery != nil:
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"derived table is not supported against INFORMATION_SCHEMA")
	case sq.countStar || len(sq.aggCols) > 0 || len(sq.groupBy) > 0 || sq.havingExpr != nil:
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"aggregate / GROUP BY / HAVING is not supported against INFORMATION_SCHEMA")
	case sq.distinct:
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"SELECT DISTINCT is not supported against INFORMATION_SCHEMA")
	case sq.qualifyExpr != nil:
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"QUALIFY is not supported against INFORMATION_SCHEMA")
	}

	upper := strings.ToUpper(sq.tableName)
	const prefix = "INFORMATION_SCHEMA."
	if !strings.HasPrefix(upper, prefix) {
		// referencesInformationSchema matched somewhere in the parse tree, but
		// the FROM target is not a plain INFORMATION_SCHEMA.X reference (e.g.
		// the only mention was inside a subquery the handler doesn't support).
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
			"unsupported INFORMATION_SCHEMA query shape: FROM %q", sq.tableName)
	}
	sysTable := upper[len(prefix):]
	sysRows, sysErr := c.execSystemTable(ctx, sysTable, sq.whereExpr)
	if sysErr != nil {
		return nil, sysErr
	}
	return projectSystemRows(sysRows, sq)
}

// execSystemTable dispatches INFORMATION_SCHEMA.* queries.
func (c *EmbeddedConnection) execSystemTable(ctx context.Context, name string, whereExpr antlrgen.IWhereExprContext) (driver.Rows, error) {
	switch name {
	case "SCHEMATA":
		return c.execSysSchemata(ctx, whereExpr)
	case "TABLES":
		return c.execSysTables(ctx, whereExpr)
	case "COLUMNS":
		return c.execSysColumns(ctx, whereExpr)
	case "INDEXES":
		return c.execSysIndexes(ctx, whereExpr)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unknown INFORMATION_SCHEMA table: %q", name)
	}
}

// listSessionSchemas lists the schemas of the connection's OWN database and
// nothing else. Every INFORMATION_SCHEMA view goes through here.
//
// Java scopes the equivalent catalog reads the same way:
// CatalogMetaData.getSchemas() calls getSchemas(conn.getPath().getPath(), null),
// which reaches the per-database catalog.listSchemas(txn, URI, BEGIN) — never the
// cluster-wide overload. getTables/getColumns/getIndexInfo refuse outright when
// the database argument is absent ("Cannot scan across Databases yet"). A catalog
// listing that spans databases is therefore not something Java ever produces, and
// in a deployment where one database path is one tenant it is a disclosure of
// every other tenant's schema, table, column and index names.
//
// The user's WHERE clause is no substitute: filterSysRows runs AFTER the rows are
// materialised, so an unscoped listing has already read the whole cluster by the
// time any predicate is applied.
//
// An empty DBPath fails closed rather than falling back to the cluster-wide scan —
// the fallback is precisely the leak this exists to prevent.
func (c *EmbeddedConnection) listSessionSchemas(txn api.Transaction) (api.ResultSet, error) {
	if c.sess.DBPath == "" {
		return nil, api.NewError(api.ErrCodeInvalidPath,
			"connection has no database path; INFORMATION_SCHEMA cannot be scoped")
	}
	return c.sess.Catalog.ListSchemasInDatabase(txn, c.sess.DBPath, nil)
}

// filterSysRows applies WHERE filtering to system-table rows (map-based, no proto).
// Column names are matched case-insensitively against the cols slice.
func filterSysRows(ctx context.Context, conn *EmbeddedConnection, rows [][]driver.Value, cols []string, where antlrgen.IWhereExprContext) ([][]driver.Value, error) {
	if where == nil {
		return rows, nil
	}
	expr := where.Expression()
	var out [][]driver.Value
	for _, row := range rows {
		m := make(map[string]driver.Value, len(cols))
		for i, c := range cols {
			m[strings.ToUpper(c)] = row[i]
		}
		ok, err := evalPredicateOnMapExpr(ctx, conn, m, expr)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// execSysSchemata implements SELECT * FROM INFORMATION_SCHEMA.SCHEMATA.
func (c *EmbeddedConnection) execSysSchemata(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.listSessionSchemas(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			data = append(data, row{dbID, schemaName, "", "", ""})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	cols := []string{"CATALOG_NAME", "SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME", "SQL_PATH"}
	filtered, ferr := filterSysRows(ctx, c, data, cols, where)
	if ferr != nil {
		return nil, ferr
	}
	return &staticRows{cols: cols, rows: filtered}, nil
}

// execSysTables implements SELECT * FROM INFORMATION_SCHEMA.TABLES.
func (c *EmbeddedConnection) execSysTables(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.listSessionSchemas(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		// Snapshot (db, schema) pairs first; LoadSchema below opens another txn context.
		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				data = append(data, row{ref.db, ref.schema, tbl.MetadataName(), "TABLE", "", "", "", "", "", ""})
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE",
		"REMARKS", "TYPE_CAT", "TYPE_SCHEM", "TYPE_NAME",
		"SELF_REFERENCING_COL_NAME", "REF_GENERATION",
	}
	filtered, ferr := filterSysRows(ctx, c, data, cols, where)
	if ferr != nil {
		return nil, ferr
	}
	return &staticRows{cols: cols, rows: filtered}, nil
}

// execSysColumns implements SELECT * FROM INFORMATION_SCHEMA.COLUMNS.
func (c *EmbeddedConnection) execSysColumns(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.listSessionSchemas(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				cols := tbl.Columns()
				for i, col := range cols {
					nullable := "NO"
					if col.DataType().IsNullable() {
						nullable = "YES"
					}
					data = append(data, row{
						ref.db,
						ref.schema,
						tbl.MetadataName(),
						col.MetadataName(),
						int64(i + 1),
						"",
						nullable,
						col.DataType().Code().String(),
						"",
						"",
						"",
					})
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME", "COLUMN_NAME",
		"ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
		"CHARACTER_MAXIMUM_LENGTH", "NUMERIC_PRECISION", "NUMERIC_SCALE",
	}
	filtered, ferr := filterSysRows(ctx, c, data, cols, where)
	if ferr != nil {
		return nil, ferr
	}
	return &staticRows{cols: cols, rows: filtered}, nil
}

// execSysIndexes implements SELECT * FROM INFORMATION_SCHEMA.INDEXES.
// Returns one row per index across all (database, schema, table) tuples.
func (c *EmbeddedConnection) execSysIndexes(ctx context.Context, where antlrgen.IWhereExprContext) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.listSessionSchemas(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck

		type schemaRef struct{ db, schema string }
		var refs []schemaRef
		for rs.Next() {
			dbID, e := rs.StringByName("DATABASE_ID")
			if e != nil {
				return nil, e
			}
			schemaName, e := rs.StringByName("SCHEMA_NAME")
			if e != nil {
				return nil, e
			}
			refs = append(refs, schemaRef{dbID, schemaName})
		}
		if e := rs.Err(); e != nil {
			return nil, e
		}

		for _, ref := range refs {
			schema, loadErr := c.cachedLoadSchema(txn, ref.db, ref.schema)
			if loadErr != nil {
				return nil, loadErr
			}
			tables, tablesErr := schema.Tables()
			if tablesErr != nil {
				return nil, tablesErr
			}
			for _, tbl := range tables {
				for _, idx := range tbl.Indexes() {
					isUnique := "NO"
					if idx.IsUnique() {
						isUnique = "YES"
					}
					isSparse := "NO"
					if idx.IsSparse() {
						isSparse = "YES"
					}
					data = append(data, row{
						ref.db,
						ref.schema,
						tbl.MetadataName(),
						idx.MetadataName(),
						idx.IndexType(),
						isUnique,
						isSparse,
					})
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	cols := []string{
		"TABLE_CATALOG", "TABLE_SCHEMA", "TABLE_NAME",
		"INDEX_NAME", "INDEX_TYPE", "IS_UNIQUE", "IS_SPARSE",
	}
	data, err = filterSysRows(ctx, c, data, cols, where)
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: cols, rows: data}, nil
}

func (c *EmbeddedConnection) execShowStatement(ctx context.Context, show antlrgen.IShowStatementContext) (driver.Rows, error) {
	switch t := show.(type) {
	case *antlrgen.ShowDatabasesStatementContext:
		// `SHOW DATABASES WITH PREFIX <path>` — the prefix is read off the
		// typed parse-tree node, never off statement text.
		prefix := c.sess.DBPath
		if p := t.Path(); p != nil {
			prefix = p.GetText()
			if err := validateDatabasePath(prefix); err != nil {
				return nil, err
			}
		}
		if prefix == "" {
			// Same fail-closed rule as listSessionSchemas: with no database to
			// scope to, the only alternative is the cluster-wide listing, which
			// is the disclosure this scoping exists to prevent.
			return nil, api.NewError(api.ErrCodeInvalidPath,
				"connection has no database path; SHOW DATABASES cannot be scoped")
		}
		return c.execShowDatabases(ctx, prefix)
	case *antlrgen.ShowSchemaTemplatesStatementContext:
		return c.execShowSchemaTemplates(ctx)
	default:
		return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported SHOW statement: %s", show.GetText())
	}
}

// databaseInPrefix reports whether dbID lies at or under prefix, compared at
// path-segment granularity so /tenant-a does not match /tenant-abc.
//
// An empty prefix matches NOTHING. Without that arm the trailing-slash trim
// would leave "/", and every database path starts with "/" — so an absent
// session database would widen the scope to the whole cluster instead of
// narrowing it. Every caller rejects an empty scope before reaching here; this
// is the second layer, so a future caller that forgets fails closed.
func databaseInPrefix(dbID, prefix string) bool {
	if prefix == "" {
		return false
	}
	return dbID == prefix || strings.HasPrefix(dbID, strings.TrimSuffix(prefix, "/")+"/")
}

// execShowDatabases lists databases at or under prefix.
//
// Java's MetadataPlanVisitor.visitShowDatabasesStatement already computes
// exactly this scope — an explicit `WITH PREFIX <path>` wins, and an absent
// prefix falls back to getDbUri(), the connection's own database. Go previously
// switched on the statement's context type and never read ctx.path(), so both
// halves of that decision were discarded and every caller got the whole cluster.
//
// Java's own CatalogQueryFactory.getListDatabasesQueryAction then drops the
// prefix it was handed ("TODO(bfines) make use of this prefix") and calls the
// unscoped listDatabases, so upstream leaks here too. We do not copy that: the
// visitor states the intended scope unambiguously, the filter is applied at the
// boundary where the rows are read, and honouring it is what stops one tenant's
// connection from enumerating every other tenant's database path.
func (c *EmbeddedConnection) execShowDatabases(ctx context.Context, prefix string) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.sess.Catalog.ListDatabases(txn, nil)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			id, strErr := rs.StringByName("DATABASE_ID")
			if strErr != nil {
				return nil, strErr
			}
			if !databaseInPrefix(id, prefix) {
				continue
			}
			data = append(data, row{id})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: []string{"DATABASE_ID"}, rows: data}, nil
}

// execShowSchemaTemplates lists every schema template on the cluster.
//
// This is deliberately NOT scoped to the session's database, and it cannot be.
// A schema template is a cluster-global object in the catalog wire format: the
// Templates record carries only TEMPLATE_NAME, TEMPLATE_VERSION and META_DATA,
// with no owning database. Java agrees on both counts — its
// getListSchemaTemplatesQueryAction takes no database argument at all and calls
// getSchemaTemplateCatalog().listTemplates(txn) unscoped, and its Templates
// record has the same three fields.
//
// Giving templates a tenant would mean adding an owner field to a record Java
// also reads and writes, i.e. a catalog wire-format change — the one line the
// port does not cross. Callers that need per-tenant templates must namespace the
// template NAME; the listing itself stays global.
func (c *EmbeddedConnection) execShowSchemaTemplates(ctx context.Context) (driver.Rows, error) {
	type row = []driver.Value
	var data []row
	_, err := c.runInTx(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		data = nil // reset on retry
		txn := catalog.NewFDBTransaction(rctx)
		rs, rsErr := c.sess.Catalog.SchemaTemplateCatalog().ListTemplates(txn)
		if rsErr != nil {
			return nil, rsErr
		}
		defer rs.Close() //nolint:errcheck
		for rs.Next() {
			name, strErr := rs.StringByName("TEMPLATE_NAME")
			if strErr != nil {
				return nil, strErr
			}
			ver, intErr := rs.LongByName("TEMPLATE_VERSION")
			if intErr != nil {
				return nil, intErr
			}
			data = append(data, row{name, ver})
		}
		return nil, rs.Err()
	})
	if err != nil {
		return nil, err
	}
	return &staticRows{cols: []string{"TEMPLATE_NAME", "TEMPLATE_VERSION"}, rows: data}, nil
}

// staticRows is a driver.Rows backed by a pre-materialised slice.
