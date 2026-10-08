package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/relational/api"
)

// UUID strings parse as java.util.UUID.fromString does, in CAST, INSERT and
// bound parameters alike.
func TestFDB_UUIDJavaStringForms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_uuid_forms")
	testkit.MustExec(t, setup, ctx, "CREATE DATABASE /FRL/testdb_uuid_forms")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uuid_forms_tmpl "+
		"CREATE TABLE U (id BIGINT, u UUID, PRIMARY KEY (id))")
	testkit.MustExec(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_uuid_forms/s WITH TEMPLATE uuid_forms_tmpl")
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/TESTDB_UUID_FORMS?cluster_file=%s&schema=S", testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	testkit.MustExec(t, db, ctx, "INSERT INTO U VALUES (0, '123e4567-e89b-12d3-a456-426614174000')")

	id := int64(0)
	for in, want := range map[string]string{
		"1-2-3-4-5":                              "00000001-0002-0003-0004-000000000005",
		"+1-2-3-4-5":                             "00000001-0002-0003-0004-000000000005",
		"Ａ-2-3-4-5":                              "0000000a-0002-0003-0004-000000000005",
		"{123e4567-e89b-12d3-a456-426614174000}": "",
		"urn:uuid:123e4567-e89b-12d3-a456-426614174000": "",
		"123e4567e89b12d3a456426614174000":              "",
		" 123e4567-e89b-12d3-a456-426614174000":         "",
	} {
		check := func(label string, err error, got string) {
			t.Helper()
			var apiErr *api.Error
			switch {
			case want == "" && (!errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInternalError):
				t.Errorf("%s %q: want XX000, got %q %v", label, in, got, err)
			case want != "" && (err != nil || got != want):
				t.Errorf("%s %q: %q %v, want %s", label, in, got, err, want)
			}
		}
		var got string
		err := db.QueryRowContext(ctx, "SELECT CAST(? AS UUID) FROM U WHERE id = 0", in).Scan(&got)
		check("CAST", err, got)
		id++
		_, err = db.ExecContext(ctx, "INSERT INTO U VALUES (?, ?)", id, in)
		got = ""
		if err == nil {
			err = db.QueryRowContext(ctx, "SELECT u FROM U WHERE id = ?", id).Scan(&got)
		}
		check("INSERT", err, got)
	}
}
