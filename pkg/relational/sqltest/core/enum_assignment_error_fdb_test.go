package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A string that names no member of an enum column's type, assigned by UPDATE
// or INSERT ... SELECT, is Java's INVALID_ENUM_VALUE: XX000 with its message
// text, reported once, and the typed error as the cause.
func TestFDB_EnumAssignmentErrorMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_enumassign")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_enumassign")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE enumassign_tpl "+
		"CREATE TYPE AS ENUM color ('RED', 'BLUE') "+
		"CREATE TABLE t (id BIGINT, c color, s STRING, PRIMARY KEY (id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_enumassign/s WITH TEMPLATE enumassign_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ENUMASSIGN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 'RED', 'BOGUS')")

	const message = "Invalid enum value for the enum type BOGUS"
	for _, q := range []string{
		"UPDATE t SET c = 'BOGUS' WHERE id = 1",
		"UPDATE t SET c = s WHERE id = 1",
		"INSERT INTO t SELECT id + 10, s, s FROM t",
	} {
		_, err := db.ExecContext(ctx, q)
		var ae *api.Error
		var invalid *values.InvalidEnumValueError
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError || ae.Message != message ||
			err.Error() != "XX000: "+message || !errors.As(err, &invalid) || invalid.Value != "BOGUS" {
			t.Errorf("%s: err = %v, want XX000 %q caused by InvalidEnumValueError", q, err, message)
		}
	}
}
