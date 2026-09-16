package yamsql_test

import (
	"testing"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

func TestSystemTableTypedCastFDB(t *testing.T) {
	t.Parallel()
	s := &yamsql.Scenario{Name: "system-typed-casts", SchemaTemplate: "CREATE TABLE t (id BIGINT, d DOUBLE, PRIMARY KEY(id))"}
	predicates := []string{
		"CAST(0.49999999999999994 AS BIGINT) = 0",
		"CAST(4503599627370497.0 AS BIGINT) = 4503599627370497",
		"CAST(1.0E20 AS BIGINT) = 9223372036854775807",
		"CAST(-1.0E20 AS BIGINT) = -9223372036854775808",
		"CAST(1.0E20 AS INTEGER) = -1",
		"CAST(-1.0E20 AS INTEGER) = 0",
		"CAST(CAST(1.0E20 AS FLOAT) AS INTEGER) = 2147483647",
		"CAST(CAST(1.0E20 AS FLOAT) AS BIGINT) = 2147483647",
		"CAST(CAST(0.49999999999999994 AS FLOAT) AS BIGINT) = 1",
		"CAST(CAST(0.49999999999999994 AS FLOAT) AS DOUBLE) = 0.5",
		"CAST(CAST(1.0E20 AS DOUBLE) AS INTEGER) = -1",
		"CAST(COALESCE(CAST(NULL AS FLOAT), CAST(1.0E20 AS FLOAT)) AS BIGINT) = 2147483647",
		"CAST(CASE WHEN ORDINAL_POSITION = 1 THEN CAST(1.0E20 AS FLOAT) ELSE CAST(0.0 AS FLOAT) END AS BIGINT) = 2147483647",
		"CAST(ORDINAL_POSITION AS DOUBLE) = 1.0",
		"CAST(CAST(COLUMN_NAME AS STRING) AS STRING) = 'ID'",
		"CAST(NULL AS INTEGER) IS NULL",
		"CAST(NULL AS BIGINT) IS NULL OR 1 = 0",
		"NOT (CAST(NULL AS INTEGER) = 1) OR CAST(NULL AS INTEGER) IS NULL",
		"CURRENT_TIMESTAMP = CURRENT_TIMESTAMP",
		"CAST(-0.0 AS INTEGER) = 0",
	}
	for _, pred := range predicates {
		query := "SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE COLUMN_NAME = 'ID' AND (" + pred + ")"
		s.Tests = append(s.Tests, yamsql.Test{Query: query, Rows: [][]any{{"ID"}}})
	}
	for _, tc := range []struct{ query, want string }{
		{"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS AS c WHERE c.COLUMN_NAME = 'ID' AND CAST(c.ORDINAL_POSITION AS DOUBLE) = 1.0", "ID"},
		{"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE COLUMNS.COLUMN_NAME = 'ID'", "ID"},
		{`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS AS "c" WHERE "c".COLUMN_NAME = 'ID'`, "ID"},
		{`SELECT TABLE_NAME FROM INFORMATION_SCHEMA."TABLES" AS q WHERE q.TABLE_NAME = 'T' AND CAST(1.0E20 AS INTEGER) = -1`, "T"},
	} {
		s.Tests = append(s.Tests, yamsql.Test{Query: tc.query, Rows: [][]any{{tc.want}}})
	}
	empty := [][]yamsql.Scalar{}
	for _, pred := range []string{"CAST(NULL AS INTEGER) = 1", "NOT (CAST(NULL AS INTEGER) = 1)", "CAST(CAST(1.0E20 AS FLOAT) AS BIGINT) = -1"} {
		s.Tests = append(s.Tests, yamsql.Test{Query: "SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE " + pred, ExactRows: &empty})
	}
	for _, tc := range []struct{ table, pred, code string }{
		{"COLUMNS", "CAST(CAST('NaN' AS FLOAT) AS BIGINT) = 0", "22F3H"},
		{"COLUMNS", "CAST(CAST('Infinity' AS DOUBLE) AS INTEGER) = 0", "22F3H"},
		{"COLUMNS", "CAST(9223372036854775807 AS INTEGER) = 0", "22F3H"},
		{"COLUMNS", "missing_column = 1", "42703"},
		{"COLUMNS", "bad_alias.COLUMN_NAME = 'ID'", "42703"},
		{"INDEXES", "missing_column = 1", "42703"},
		{"COLUMNS", "EXISTS (SELECT id FROM t)", "0A000"},
	} {
		s.Tests = append(s.Tests, yamsql.Test{Query: "SELECT * FROM INFORMATION_SCHEMA." + tc.table + " WHERE " + tc.pred, ErrorCode: tc.code})
	}
	r := runSemanticScenario(t, s)
	if len(s.Tests) != 34 || r.TestsRun != 34 || r.TestsPass != 34 || r.TestsFail != 0 {
		t.Fatalf("system CAST: run=%d pass=%d fail=%d; %+v", r.TestsRun, r.TestsPass, r.TestsFail, r.Failures)
	}
	t.Logf("typed INFORMATION_SCHEMA: %d validated statements", r.TestsPass)
}
