package sqldriver_test

// Pins how a DATE column compares with a TIMESTAMP-typed constant, through an
// index and through a residual filter over the SAME rows. RFC-257 WS-E
// (ws-e-design.md section 4.3) binds a time.Time as its canonical TIMESTAMP
// text, so this is the comparison every bound time.Time against a DATE column
// reaches.
//
// The expectations are the behaviour BEFORE that change, and two of them are
// wrong by instants: both sides are compared as TEXT, so `D = CAST('2024-01-01
// 00:00:00' AS TIMESTAMP)` misses the row whose day starts at that instant, and
// `>=` the same instant drops it. Today's bound time.Time dodges that only
// because the text channel renders a midnight value as date text. Index and
// residual agree on every row (both compare text), which is the property the
// change must keep while it moves every row to the instant comparison.
//
// Table X has a DATE column twice: D is indexed, R is not, and every row stores
// the same day in both. A statement over D can take the index; the same
// statement over R can only be a residual filter. The two must answer alike.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFDB_TemporalComparandDateColumn(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	const dbName = "testdb_temporal_comparand_date"
	setup := openTestDB(t, "/"+dbName)
	mustExec := func(db *sql.DB, stmt string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	mustExec(setup, "CREATE DATABASE /"+dbName)
	mustExec(setup, "CREATE SCHEMA TEMPLATE temporal_comparand_date "+
		"CREATE TABLE X (id BIGINT, D DATE, R DATE, PRIMARY KEY (id)) "+
		"CREATE INDEX X_D ON X(D)")
	mustExec(setup, "CREATE SCHEMA /"+dbName+"/s WITH TEMPLATE temporal_comparand_date")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///%s?cluster_file=%s&schema=S", strings.ToUpper(dbName), clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(db, "INSERT INTO X VALUES (1, CAST('2024-01-01' AS DATE), CAST('2024-01-01' AS DATE)), "+
		"(2, CAST('2024-01-02' AS DATE), CAST('2024-01-02' AS DATE))")

	answer := func(q string, args ...any) string {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return "ERROR " + err.Error()
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return "ERROR scan " + err.Error()
			}
			ids = append(ids, fmt.Sprint(id))
		}
		if err := rows.Err(); err != nil {
			return "ERROR " + err.Error()
		}
		return "[" + strings.Join(ids, " ") + "]"
	}
	explain := func(q string, args ...any) string {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q, args...).Scan(&plan); err != nil {
			return "ERROR " + err.Error()
		}
		return plan
	}

	midnight := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	morning := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, cond string
		args       []any
		want, scan string
	}{
		{"cast_eq_midnight", "= CAST('2024-01-01 00:00:00' AS TIMESTAMP)", nil, "[]", "IndexScan(X_D, [=] COVERING)"},
		{"cast_ge_midnight", ">= CAST('2024-01-01 00:00:00' AS TIMESTAMP)", nil, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"cast_lt_morning", "< CAST('2024-01-01 10:00:00' AS TIMESTAMP)", nil, "[1]", "IndexScan(X_D, [<>] COVERING)"},
		{"cast_gt_morning", "> CAST('2024-01-01 10:00:00' AS TIMESTAMP)", nil, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"bound_eq_midnight", "= ?", []any{midnight}, "[1]", "IndexScan(X_D, [=] COVERING)"},
		{"bound_eq_morning", "= ?", []any{morning}, "[]", "IndexScan(X_D, [=] COVERING)"},
		{"bound_ge_morning", ">= ?", []any{morning}, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"bound_le_morning", "<= ?", []any{morning}, "[1]", "IndexScan(X_D, [<>] COVERING)"},
	} {
		idx := answer("SELECT id FROM X WHERE D "+tc.cond+" ORDER BY id", tc.args...)
		res := answer("SELECT id FROM X WHERE R "+tc.cond+" ORDER BY id", tc.args...)
		plan := explain("SELECT id FROM X WHERE D "+tc.cond, tc.args...)
		resPlan := explain("SELECT id FROM X WHERE R "+tc.cond, tc.args...)
		if idx != tc.want || res != tc.want || !strings.Contains(plan, tc.scan) {
			t.Errorf("%s: index=%s residual=%s plan=%s; want both %s over %s",
				tc.name, idx, res, plan, tc.want, tc.scan)
		}
		// The copy R is not indexed, so its read must be a residual filter over
		// the record scan; an index plan here would make the pair compare two
		// index reads.
		if !strings.Contains(resPlan, "PredicatesFilter(Scan(X)") || strings.Contains(resPlan, "IndexScan") {
			t.Errorf("%s: the residual copy's plan is %s, want a PredicatesFilter over Scan(X)", tc.name, resPlan)
		}
	}
}
