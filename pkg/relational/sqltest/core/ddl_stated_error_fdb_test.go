package sqltest

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// A schema template Java's PlanGenerator refuses renders the exception's own
// message once, with the typed cause: 42602 for a name, XXXXX for a UUID comparand.
func TestFDB_DDLRefusalRendersMessageOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.OpenDB(t, "/FRL/testdb_ddlstated")
	testkit.MustExec(t, db, ctx, "CREATE DATABASE /FRL/testdb_ddlstated")
	for _, tc := range []struct {
		name, body, want string
		cause            any
	}{
		{"column name", `CREATE TABLE t (id BIGINT, "A'B" BIGINT, PRIMARY KEY (id))`, "42602: A'B it not a valid protobuf identifier", new(*recordlayer.InvalidNameError)},
		{"enum value", `CREATE TYPE AS ENUM mood ('O''NEIL') CREATE TABLE t (id BIGINT, m mood, PRIMARY KEY (id))`, "42602: O'NEIL it not a valid protobuf identifier", new(*recordlayer.InvalidNameError)},
		{"index predicate", `CREATE TABLE t (id BIGINT, u UUID, PRIMARY KEY (id)) CREATE INDEX ix AS SELECT id FROM t WHERE u = '5ea7a4c2-54a4-4f7a-9d0e-2d1b0b3f8e10' ORDER BY id`, "XXXXX: Unsupported value type class java.util.UUID", new(*recordlayer.UnsupportedValueTypeError)},
	} {
		_, err := db.ExecContext(ctx, "CREATE SCHEMA TEMPLATE ddlstated_tpl "+tc.body)
		var ae *api.Error // an index's refusal carries an `index "IX": ` prefix (TODO.md)
		if !errors.As(err, &ae) || ae.Error() != tc.want || !errors.As(err, tc.cause) {
			t.Errorf("%s: err = %v, want %q with its typed cause", tc.name, err, tc.want)
		}
	}
}
