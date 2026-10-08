package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A typed NULL branch (CAST(NULL AS BIGINT), Java's CastValue.inject) takes part
// in a CASE's result type: Java's Type.maximumType over BIGINT and INTEGER is
// BIGINT. Go skipped every NullValue and reported INTEGER (the oracle's
// case_typed_null_then_branch).
func TestFDB_CaseTypedNullBranchKeepsItsType(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_ctnb")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_ctnb")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE ctnb CREATE TABLE T_CTNB (id BIGINT, v BIGINT, PRIMARY KEY (id))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_ctnb/s WITH TEMPLATE ctnb")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_CTNB?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO T_CTNB VALUES (1, 5)")
	for q, want := range map[string]string{
		"SELECT CASE WHEN v < 10 THEN CAST(NULL AS BIGINT) ELSE 0 END FROM T_CTNB": "BIGINT",
		"SELECT CASE WHEN v < 10 THEN 0 ELSE CAST(NULL AS BIGINT) END FROM T_CTNB": "BIGINT",
		"SELECT CASE WHEN v < 10 THEN NULL ELSE 0 END FROM T_CTNB":                 "INTEGER",
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		cts, err := rows.ColumnTypes()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := cts[0].DatabaseTypeName(); got != want {
			t.Errorf("%s: column type %s, want %s", q, got, want)
		}
	}
}
