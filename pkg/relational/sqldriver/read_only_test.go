package sqldriver

// NewConnector and the read_only DSN option, end to end over SimFDB: a
// read-only connection runs only SELECT and EXPLAIN of a SELECT, and nothing
// it does persists.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/internal/connknob"
	"fdb.dev/pkg/simfdb"
)

// roTemplate has an index, so planning a SELECT opens the store (the index
// states), and EXPLAIN reaches the open too.
const roTemplate = "CREATE SCHEMA TEMPLATE tmpl " +
	"CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t(a)"

// roStaleFormatVersion is a store format version below the current one: an
// open of a store whose header carries it rewrites the header.
const roStaleFormatVersion = 13

// newROBackend returns an empty SimFDB-backed database.
func newROBackend(seed uint64) *recordlayer.FDBDatabase {
	// This target has no testkit.Main to register the domain.
	RegisterDomainIfNotExists("FRL")
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	rdb := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	rdb.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	return rdb
}

// openROConnector opens a database/sql pool over NewConnector(rdb, dsn).
func openROConnector(t *testing.T, rdb *recordlayer.FDBDatabase, dsn string) *sql.DB {
	t.Helper()
	c, err := NewConnector(rdb, dsn)
	if err != nil {
		t.Fatalf("NewConnector(%q): %v", dsn, err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { db.Close() })
	return db
}

// newROFixture creates, through writable connectors, /FRL/SIMDB with schemas
// T1 (ids 1-3) and T2 (ids 101-103) and /FRL/OTHER with schema S (ids
// 201-203), every one of template tmpl with a = id.
func newROFixture(t *testing.T, seed uint64) *recordlayer.FDBDatabase {
	t.Helper()
	ctx := context.Background()
	rdb := newROBackend(seed)
	setup := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB")
	for _, stmt := range []string{
		"CREATE DATABASE /FRL/simdb",
		roTemplate,
		"CREATE SCHEMA /FRL/simdb/t1 WITH TEMPLATE tmpl",
		"CREATE SCHEMA /FRL/simdb/t2 WITH TEMPLATE tmpl",
		"CREATE DATABASE /FRL/other",
		"CREATE SCHEMA /FRL/other/s WITH TEMPLATE tmpl",
	} {
		roExec(t, ctx, setup, stmt)
	}
	for _, s := range []struct {
		dsn   string
		first int
	}{
		{"fdbsql:///FRL/SIMDB?schema=T1", 1},
		{"fdbsql:///FRL/SIMDB?schema=T2", 101},
		{"fdbsql:///FRL/OTHER?schema=S", 201},
	} {
		db := openROConnector(t, rdb, s.dsn)
		for id := s.first; id < s.first+3; id++ {
			roExec(t, ctx, db, fmt.Sprintf("INSERT INTO t (id, a) VALUES (%d, %d)", id, id))
		}
	}
	return rdb
}

func roExec(t *testing.T, ctx context.Context, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// roQueryer is a *sql.DB, *sql.Conn or *sql.Tx.
type roQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// roIDs runs query, whose first column is an integer, and returns that column.
func roIDs(ctx context.Context, q roQueryer, query string) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []int64
	for rows.Next() {
		dest := make([]any, len(cols))
		var id int64
		dest[0] = &id
		for i := 1; i < len(cols); i++ {
			dest[i] = new(any)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func mustROIDs(t *testing.T, ctx context.Context, q roQueryer, query string) []int64 {
	t.Helper()
	ids, err := roIDs(ctx, q, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return ids
}

// roStrings runs query and returns its first column as text.
func roStrings(t *testing.T, ctx context.Context, q roQueryer, query string) []string {
	t.Helper()
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		dest := make([]any, len(cols))
		var s sql.NullString
		dest[0] = &s
		for i := 1; i < len(cols); i++ {
			dest[i] = new(any)
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// NewConnector drives the driver over the caller's database: it opens and
// caches none, and it checks the DSN up front.
func TestNewConnector_UsesTheCallersDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := newROFixture(t, 7107)
	c, err := NewConnector(rdb, "fdbsql:///FRL/SIMDB?schema=T1&read_only=true")
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	if got := mustROIDs(t, ctx, db, "SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("SELECT = %v; want [1 2 3]", got)
	}
	if got := c.(*Connector).fdbDB; got != rdb {
		t.Errorf("connector database = %p; want the caller's %p", got, rdb)
	}
	fdbDBCache.Range(func(key, value any) bool {
		if value == rdb {
			t.Errorf("the caller's database is in the driver's cache under %q", key)
		}
		return true
	})

	if _, err := NewConnector(nil, "fdbsql:///FRL/SIMDB"); err == nil {
		t.Error("NewConnector(nil) accepted")
	}
	for _, dsn := range []string{
		"fdbsql:///FRL/SIMDB?cluster_file=/etc/foundationdb/fdb.cluster",
		"fdbsql://localhost:1234/FRL/SIMDB",
		"fdbsql:///FRL/SIMDB?read_only=yes",
		"fdbsql:///FRL/SIMDB?readonly=true",
	} {
		var ae *api.Error
		if _, err := NewConnector(rdb, dsn); !errors.As(err, &ae) || ae.Code != api.ErrCodeInvalidParameter {
			t.Errorf("NewConnector(%q) = %v; want 22023", dsn, err)
		}
	}
}

// roErrorIs reports whether err is the api.Error code with message msg.
func roErrorIs(err error, code api.ErrorCode, msg string) bool {
	var ae *api.Error
	return errors.As(err, &ae) && ae.Code == code && ae.Message == msg
}

const readOnlyRefusal = "read-only connection: only SELECT and EXPLAIN of a SELECT are allowed"

// A read-only connection refuses, at planning, every statement but a SELECT and
// EXPLAIN of one, with its own 0A000, whether the statement comes through
// ExecContext or QueryContext and whatever a multi-statement batch holds.
func TestReadOnly_RefusesAllButSelectAndExplain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := newROFixture(t, 7101)
	ro := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1&read_only=true")

	for _, stmt := range []string{
		"INSERT INTO t (id, a) VALUES (9, 9)",
		"UPDATE t SET a = 0 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"EXPLAIN DELETE FROM t WHERE id = 1",
		"CREATE SCHEMA /FRL/simdb/t3 WITH TEMPLATE tmpl",
		"DROP SCHEMA /FRL/simdb/t2",
		"CREATE DATABASE /FRL/third",
		"DROP DATABASE /FRL/other",
		"SHOW DATABASES",
		"SHOW SCHEMA TEMPLATES",
		"START TRANSACTION",
		"SELECT id FROM t; SELECT id FROM t",
		"INSERT INTO t (id, a) VALUES (9, 9); DELETE FROM t WHERE id = 1",
	} {
		if _, err := ro.ExecContext(ctx, stmt); !roErrorIs(err, api.ErrCodeUnsupportedOperation, readOnlyRefusal) {
			t.Errorf("ExecContext(%q) = %v; want 0A000 %q", stmt, err, readOnlyRefusal)
		}
		rows, err := ro.QueryContext(ctx, stmt)
		if err == nil {
			rows.Close()
		}
		if !roErrorIs(err, api.ErrCodeUnsupportedOperation, readOnlyRefusal) {
			t.Errorf("QueryContext(%q) = %v; want 0A000 %q", stmt, err, readOnlyRefusal)
		}
	}

	if got := mustROIDs(t, ctx, ro, "SELECT id, a FROM t WHERE a > 1 ORDER BY id"); !slices.Equal(got, []int64{2, 3}) {
		t.Errorf("read-only SELECT = %v; want [2 3]", got)
	}
	if plan := roStrings(t, ctx, ro, "EXPLAIN SELECT id FROM t WHERE a > 1"); len(plan) != 1 || plan[0] == "" {
		t.Errorf("read-only EXPLAIN = %q; want one plan row", plan)
	}

	// Nothing the refused statements name changed.
	if got := mustROIDs(t, ctx, openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1"),
		"SELECT a FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("T1 after the refusals = %v; want a = [1 2 3]", got)
	}
	if got := mustROIDs(t, ctx, openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T2"),
		"SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{101, 102, 103}) {
		t.Errorf("T2 after the refusals = %v; want [101 102 103]", got)
	}
	if got := mustROIDs(t, ctx, openROConnector(t, rdb, "fdbsql:///FRL/OTHER?schema=S"),
		"SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{201, 202, 203}) {
		t.Errorf("/FRL/OTHER after the refusals = %v; want [201 202 203]", got)
	}
	var ae *api.Error
	if _, err := roIDs(ctx, openROConnector(t, rdb, "fdbsql:///FRL/THIRD?schema=S"), "SELECT id FROM t"); !errors.As(err, &ae) ||
		ae.Code != api.ErrCodeUndefinedDatabase {
		t.Errorf("SELECT on /FRL/THIRD after the refusals = %v; want 42F00 (undefined database)", err)
	}
	if _, err := roIDs(ctx, openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T3"), "SELECT id FROM t"); err == nil {
		t.Error("schema T3 exists after the refusals")
	}
}

// roSchemaSubspace is where the store of /FRL/SIMDB's schema T1 lives.
func roSchemaSubspace(t *testing.T, ctx context.Context, rdb *recordlayer.FDBDatabase) subspace.Subspace {
	t.Helper()
	ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(ctx, rdb, "/FRL/SIMDB", "T1")
	if err != nil {
		t.Fatalf("schema subspace: %v", err)
	}
	return ss
}

// roStoreHeader rewrites the store header's format version to v when v is
// non-zero, and returns the version it read.
func roStoreHeader(t *testing.T, ctx context.Context, rdb *recordlayer.FDBDatabase, ss subspace.Subspace, v int32) int32 {
	t.Helper()
	key := ss.Pack(tuple.Tuple{recordlayer.StoreInfoKey})
	got, err := rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
		raw, err := rctx.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("no store header at %x", []byte(key))
		}
		hdr := &gen.DataStoreInfo{}
		if err := hdr.UnmarshalVT(raw); err != nil {
			return nil, err
		}
		read := hdr.GetFormatVersion()
		if v != 0 {
			hdr.FormatVersion = &v
			out, err := hdr.MarshalVT()
			if err != nil {
				return nil, err
			}
			rctx.Transaction().Set(key, out)
		}
		return read, nil
	})
	if err != nil {
		t.Fatalf("store header: %v", err)
	}
	return got.(int32)
}

// roPin borrows one connection of db and configures its embedded connection.
func roPin(t *testing.T, ctx context.Context, db *sql.DB, configure func(*embedded.EmbeddedConnection)) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Raw(func(dc any) error {
		configure(dc.(*embedded.EmbeddedConnection))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return conn
}

// roSetMaxAge sets how long after its first read a statement's first page may
// still join the statement's shared transaction.
func roSetMaxAge(t *testing.T, ec *embedded.EmbeddedConnection, maxAge time.Duration) {
	t.Helper()
	if !connknob.SetStatementReadMaxAge(ec, maxAge) {
		t.Fatal("connknob refused the embedded connection")
	}
}

// A read over a store an older version wrote does not persist the header
// upgrade its open makes, on any route a read's transaction ends by: the
// statement's shared transaction ending at its first page, a page in a
// transaction of its own, EXPLAIN ending the shared transaction it planned in,
// and an explicit transaction's Commit. Each route, run on a writable
// connection, does persist the upgrade, so each read-only run is a real test.
func TestReadOnly_ReadsDoNotPersistStoreHeaderUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := newROFixture(t, 7102)
	ss := roSchemaSubspace(t, ctx, rdb)
	current := roStoreHeader(t, ctx, rdb, ss, 0)
	if current == roStaleFormatVersion {
		t.Fatalf("a new store already has format version %d, the one this test writes as stale", current)
	}

	pages := func(maxAge time.Duration, scanLimit int) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			conn := roPin(t, ctx, db, func(ec *embedded.EmbeddedConnection) {
				if scanLimit > 0 {
					if err := ec.SetOption(api.OptExecutionScannedRowsLimit, scanLimit); err != nil {
						t.Fatal(err)
					}
				}
				roSetMaxAge(t, ec, maxAge)
			})
			if got := mustROIDs(t, ctx, conn, "SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
				t.Fatalf("SELECT = %v; want [1 2 3]", got)
			}
		}
	}
	for _, route := range []struct {
		name string
		run  func(*testing.T, *sql.DB)
	}{
		{"shared first page", pages(time.Hour, 0)},
		{"pages of their own", pages(time.Nanosecond, 1)},
		{"explain", func(t *testing.T, db *sql.DB) {
			roStrings(t, ctx, db, "EXPLAIN SELECT id FROM t WHERE a > 1")
		}},
		{"explicit transaction", func(t *testing.T, db *sql.DB) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := mustROIDs(t, ctx, tx, "SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
				t.Fatalf("SELECT = %v; want [1 2 3]", got)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, mode := range []struct {
				params string
				want   int32
			}{
				{"", current},
				{"&read_only=true", roStaleFormatVersion},
			} {
				roStoreHeader(t, ctx, rdb, ss, roStaleFormatVersion)
				db := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1"+mode.params)
				route.run(t, db)
				if got := roStoreHeader(t, ctx, rdb, ss, 0); got != mode.want {
					t.Errorf("DSN params %q: store format version after the read = %d; want %d (%d is stale)",
						mode.params, got, mode.want, roStaleFormatVersion)
				}
			}
		})
	}
}

// A read-only connector over an empty cluster writes nothing, not even the
// catalog bootstrap every statement starts with; a writable one bootstraps it.
func TestReadOnly_EmptyClusterStaysEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []struct {
		params    string
		wantEmpty bool
	}{
		{"", false},
		{"&read_only=true", true},
	} {
		rdb := newROBackend(7103)
		db := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=S"+mode.params)
		if _, err := roIDs(ctx, db, "SELECT id FROM t"); err == nil {
			t.Fatalf("DSN params %q: SELECT on an empty cluster succeeded", mode.params)
		}
		kvs, err := rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			return rctx.Transaction().GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")},
				fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatal(err)
		}
		if empty := len(kvs.([]fdb.KeyValue)) == 0; empty != mode.wantEmpty {
			t.Errorf("DSN params %q: cluster empty after a failed SELECT = %v; want %v",
				mode.params, empty, mode.wantEmpty)
		}
	}
}

// TxOptions.ReadOnly is accepted on a read-only connection, whose transaction
// then runs SELECTs and refuses writes, and refused (0A000) on any other.
func TestReadOnly_BeginTxReadOnlyOnlyOnReadOnlyConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := newROFixture(t, 7104)

	ro := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1&read_only=true")
	tx, err := ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(ReadOnly) on a read_only connection: %v", err)
	}
	if got := mustROIDs(t, ctx, tx, "SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("SELECT in the read-only transaction = %v; want [1 2 3]", got)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (9, 9)"); !roErrorIs(err, api.ErrCodeUnsupportedOperation, readOnlyRefusal) {
		t.Errorf("INSERT in the read-only transaction = %v; want 0A000 %q", err, readOnlyRefusal)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("Commit of the read-only transaction: %v", err)
	}

	rw := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1")
	if tx, err := rw.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); !roErrorIs(err, api.ErrCodeUnsupportedOperation,
		"read-only transactions are not supported") {
		if err == nil {
			tx.Rollback()
		}
		t.Errorf("BeginTx(ReadOnly) on a writable connection = %v; want 0A000", err)
	}
}

// isolation_level_snapshot=true in the DSN refuses every statement but a
// SELECT and EXPLAIN of one, end to end, as Java's ISOLATION_LEVEL_SNAPSHOT
// connection option does.
func TestIsolationLevelSnapshotDSN_RefusesAllButSelect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := newROFixture(t, 7105)
	snap := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1&isolation_level_snapshot=true")
	const refusal = "OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on SELECT queries"
	for _, stmt := range []string{
		"INSERT INTO t (id, a) VALUES (9, 9)",
		"UPDATE t SET a = 0 WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"CREATE SCHEMA /FRL/simdb/t3 WITH TEMPLATE tmpl",
		"SHOW DATABASES",
	} {
		if _, err := snap.ExecContext(ctx, stmt); !roErrorIs(err, api.ErrCodeUnsupportedOperation, refusal) {
			t.Errorf("ExecContext(%q) = %v; want 0A000 %q", stmt, err, refusal)
		}
	}
	if got := mustROIDs(t, ctx, snap, "SELECT a FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("snapshot SELECT = %v; want a = [1 2 3]", got)
	}
	if plan := roStrings(t, ctx, snap, "EXPLAIN SELECT id FROM t WHERE a > 1"); len(plan) != 1 {
		t.Errorf("snapshot EXPLAIN = %q; want one plan row", plan)
	}
	if got := mustROIDs(t, ctx, snap, "SELECT id FROM t ORDER BY id"); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("T1 after the refusals = %v; want [1 2 3]", got)
	}
}

// A SELECT on a connection bound to schema T1 reads T1's rows and no other
// schema's: a qualifier on a table names the schema TEMPLATE, which resolves
// in the connection's own schema, so neither another schema of the same
// database nor another database is reachable by name. What does cross the
// schema boundary is metadata: INFORMATION_SCHEMA lists every schema of the
// connection's database, with its tables and columns, but nothing of another
// database.
func TestReadOnly_SelectReadsOnlyItsOwnSchemasRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := newROFixture(t, 7106)
	ro := openROConnector(t, rdb, "fdbsql:///FRL/SIMDB?schema=T1&read_only=true")

	for _, q := range []string{"SELECT id FROM t ORDER BY id", "SELECT id FROM tmpl.t ORDER BY id"} {
		if got := mustROIDs(t, ctx, ro, q); !slices.Equal(got, []int64{1, 2, 3}) {
			t.Errorf("%s = %v; want T1's [1 2 3]", q, got)
		}
	}
	for _, q := range []string{
		"SELECT id FROM T2.t",
		`SELECT id FROM "T2"."T"`,
		"SELECT id FROM SIMDB.T2.t",
		"SELECT id FROM OTHER.S.t",
		"SELECT t.id FROM t, T2.t AS u WHERE t.id = u.id",
	} {
		ids, err := roIDs(ctx, ro, q)
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeUndefinedColumn {
			t.Errorf("%s = %v, %v; want 42703 (unknown reference)", q, ids, err)
		}
	}

	if got := roStrings(t, ctx, ro,
		"SELECT SCHEMA_NAME, CATALOG_NAME FROM INFORMATION_SCHEMA.SCHEMATA ORDER BY SCHEMA_NAME"); !slices.Equal(got, []string{"T1", "T2"}) {
		t.Errorf("INFORMATION_SCHEMA.SCHEMATA = %q; want [T1 T2]: the database's schemas, not /FRL/OTHER's", got)
	}
	if got := roStrings(t, ctx, ro,
		`SELECT TABLE_SCHEMA, TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES" ORDER BY TABLE_SCHEMA`); !slices.Equal(got, []string{"T1", "T2"}) {
		t.Errorf("INFORMATION_SCHEMA.TABLES schemas = %q; want [T1 T2]", got)
	}
	if got := roStrings(t, ctx, ro,
		"SELECT TABLE_SCHEMA, COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS ORDER BY TABLE_SCHEMA, ORDINAL_POSITION"); !slices.Equal(got, []string{"T1", "T1", "T2", "T2"}) {
		t.Errorf("INFORMATION_SCHEMA.COLUMNS schemas = %q; want [T1 T1 T2 T2]", got)
	}
}
