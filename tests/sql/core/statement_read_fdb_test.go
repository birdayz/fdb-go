package sqltest

// An auto-commit SELECT on a pooled *sql.DB reads its catalog, opens its
// store for planning and runs its first page in ONE transaction
// (embedded/statement_read.go), so a point SELECT takes one read version,
// where it took three (the catalog read, the planner's store open and the
// page). Measured with the client's own counters on a client this test alone
// uses, registered as the driver's backend.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"fdb.dev/internal/relational/connknob"
	"fdb.dev/internal/relational/sqltest/testkit"
	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
)

// srEngine builds an explicit *sqldriver.Engine over a record-layer database
// with a dedicated client handle, and returns both.
func srEngine(t *testing.T) (fdb.Database, *sqldriver.Engine) {
	t.Helper()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	recordDB := recordlayer.NewFDBDatabaseWithBackend(raw)
	recordDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	eng, err := sqldriver.NewEngine(sqldriver.EngineConfig{
		Domains: []string{"FRL"},
		Limits:  sqldriver.UnboundedJavaCompatLimits(),
	}, sqldriver.WithDatabase(recordDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return raw, eng
}

// srPooled is a *sql.DB of one pooled connection: every statement borrows
// the same connection, and database/sql runs its ResetSession at each
// checkout after the first. The connection's first pages join their
// statements' shared transactions however long planning takes.
func srPooled(t *testing.T, eng *sqldriver.Engine, dbPath string, opts ...sqldriver.ConnOption) *sql.DB {
	t.Helper()
	db, err := eng.OpenDB(dbPath, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(driverConn any) error {
		if dc, ok := driverConn.(driver.Conn); !ok || !connknob.SetStatementReadMaxAge(dc, time.Hour) {
			return fmt.Errorf("connknob refused the driver connection %T", driverConn)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

// srReadVersions runs q to the end and returns its rows rendered and the read
// versions the client completed meanwhile.
func srReadVersions(t *testing.T, raw fdb.Database, db *sql.DB, q string) (string, int64) {
	t.Helper()
	before := raw.Metrics().TransactionReadVersionsCompleted
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]int64, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " "), raw.Metrics().TransactionReadVersionsCompleted - before
}

// TestFDB_StatementRead_PooledSelects: on a pooled connection, every point
// SELECT after the first takes exactly one read version and returns its row;
// a SELECT the scan limit pages returns every row exactly once and takes a
// read version per page (the later pages run their own transactions, so
// more than one), which is also the control that the counter sees each
// transaction a statement takes.
func TestFDB_StatementRead_PooledSelects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw, eng := srEngine(t)
	const dbPath = "/FRL/SR_POOLED_SELECTS"
	setup := srPooled(t, eng, dbPath)
	for _, stmt := range []string{
		"CREATE DATABASE " + dbPath,
		"CREATE SCHEMA TEMPLATE sr_pooled_selects CREATE TABLE T (PK BIGINT, C BIGINT, V BIGINT, PRIMARY KEY (PK)) " +
			"CREATE INDEX T_BY_C ON T(C)",
		"CREATE SCHEMA " + dbPath + "/S WITH TEMPLATE sr_pooled_selects",
	} {
		testkit.MustExecCtx(t, setup, ctx, stmt)
	}
	db := srPooled(t, eng, dbPath, sqldriver.WithSchema("S"))
	var vals []string
	for i := 1; i <= 7; i++ {
		vals = append(vals, fmt.Sprintf("(%d, %d, %d)", i, i, 10*i))
	}
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO T (PK, C, V) VALUES "+strings.Join(vals, ", "))

	const point = "SELECT PK, V FROM T WHERE PK = 3"
	// The first statements of a connection also resolve its schema's
	// subspace and plan the query; the ones after are the steady state.
	srReadVersions(t, raw, db, point)
	for i := range 3 {
		got, grvs := srReadVersions(t, raw, db, point)
		if got != "[3 30]" {
			t.Fatalf("point SELECT %d: rows %q, want [3 30]", i, got)
		}
		if grvs != 1 {
			t.Fatalf("point SELECT %d took %d read versions, want 1: its catalog read, planner store open "+
				"and page did not share one transaction", i, grvs)
		}
	}

	paged := srPooled(t, eng, dbPath, sqldriver.WithSchema("S"), sqldriver.WithResourceLimits(&sqldriver.ResourceLimits{ScannedRowsLimit: 2}))
	srReadVersions(t, raw, paged, point)
	got, grvs := srReadVersions(t, raw, paged, "SELECT PK FROM T")
	pks := strings.Fields(strings.NewReplacer("[", "", "]", "").Replace(got))
	if want := []string{"1", "2", "3", "4", "5", "6", "7"}; !slices.Equal(pks, want) {
		t.Fatalf("paged SELECT rows %q, want every PK 1..7 exactly once", got)
	}
	if grvs < 2 {
		t.Fatalf("the paged SELECT took %d read versions, want one per page (at least 2): its later pages did not run "+
			"their own transactions", grvs)
	}
}
