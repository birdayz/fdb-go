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

// A string an enum or UUID column cannot hold is Java's INVALID_ENUM_VALUE or
// INVALID_UUID_VALUE: XX000 with that message text, once, and the typed cause.
func TestFDB_EnumAndUUIDAssignmentErrorMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_enumassign")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_enumassign")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE enumassign_tpl "+
		"CREATE TYPE AS ENUM color ('RED', 'BLUE') "+
		"CREATE TABLE t (id BIGINT, c color, s STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE u (id BIGINT, v UUID, s STRING, PRIMARY KEY (id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_enumassign/s WITH TEMPLATE enumassign_tpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_ENUMASSIGN?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO t VALUES (1, 'RED', 'BOGUS')")
	testkit.MustExec(t, db, ctx, "INSERT INTO u VALUES (1, '123e4567-e89b-12d3-a456-426614174000', 'BOGUS')")

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

	const uuidMessage = "Invalid UUID value for the UUID type BOGUS"
	for _, q := range []string{
		"INSERT INTO u VALUES (2, 'BOGUS', 'x')",
		"UPDATE u SET v = 'BOGUS' WHERE id = 1",
		"UPDATE u SET v = s WHERE id = 1",
		"INSERT INTO u SELECT id + 10, s, s FROM u",
	} {
		_, err := db.ExecContext(ctx, q)
		var ae *api.Error
		var invalid *values.InvalidUUIDValueError
		if !errors.As(err, &ae) || ae.Code != api.ErrCodeInternalError || ae.Message != uuidMessage ||
			err.Error() != "XX000: "+uuidMessage || !errors.As(err, &invalid) {
			t.Errorf("%s: err = %v, want XX000 %q caused by InvalidUUIDValueError", q, err, uuidMessage)
		}
	}
}
