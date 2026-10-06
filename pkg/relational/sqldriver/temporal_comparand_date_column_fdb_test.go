package sqldriver_test

// Pins how a DATE-spelled column compares with DATE and TIMESTAMP values,
// through an index and through a residual filter over the SAME rows.
//
// Every stored column is STRING whatever its DDL spelling (the type is not
// recoverable from the proto descriptor, and persisting it would change
// catalog bytes Java reads), so a DATE or TIMESTAMP value compared with one
// takes the common type STRING and compares TEXT (RFC-257 WS-E,
// ws-e-design.md section 4.3). `D = CAST('2024-01-01 00:00:00' AS TIMESTAMP)`
// therefore misses the row stored as '2024-01-01', and `>=` it drops that row.
//
// A bound time.Time is a TIMESTAMP value (its canonical UTC text), so it
// answers exactly what its CAST constant answers, at midnight too. The old
// text channel bound a value at midnight in its own zone as date text, which
// is why `D = ?` with a midnight once found row 1; `D = CAST(? AS DATE)` is the
// statement that finds a day, and it takes the instant's UTC day.
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
	const dbName = "FRL/testdb_temporal_comparand_date"
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
	// Midnight at UTC+2 is 2023-12-31 22:00 UTC. The old channel bound it as
	// the day '2023-12-31' by its own zone's midnight, 22 hours from the day
	// it was written in and the day of its instant in neither zone's sense.
	eastMidnight := time.Date(2024, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC+2", 2*3600))
	for _, tc := range []struct {
		name, cond string
		args       []any
		want, scan string
	}{
		{"cast_eq_midnight", "= CAST('2024-01-01 00:00:00' AS TIMESTAMP)", nil, "[]", "IndexScan(X_D, [=] COVERING)"},
		{"cast_ge_midnight", ">= CAST('2024-01-01 00:00:00' AS TIMESTAMP)", nil, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"cast_lt_morning", "< CAST('2024-01-01 10:00:00' AS TIMESTAMP)", nil, "[1]", "IndexScan(X_D, [<>] COVERING)"},
		{"cast_gt_morning", "> CAST('2024-01-01 10:00:00' AS TIMESTAMP)", nil, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"bound_eq_midnight", "= ?", []any{midnight}, "[]", "IndexScan(X_D, [=] COVERING)"},
		{"bound_ge_midnight", ">= ?", []any{midnight}, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"bound_eq_morning", "= ?", []any{morning}, "[]", "IndexScan(X_D, [=] COVERING)"},
		{"bound_ge_morning", ">= ?", []any{morning}, "[2]", "IndexScan(X_D, [<>] COVERING)"},
		{"bound_le_morning", "<= ?", []any{morning}, "[1]", "IndexScan(X_D, [<>] COVERING)"},
		{"bound_ge_east_midnight", ">= ?", []any{eastMidnight}, "[1 2]", "IndexScan(X_D, [<>] COVERING)"},
		{"cast_date_eq_midnight", "= CAST(? AS DATE)", []any{midnight}, "[1]", "IndexScan(X_D, [=] COVERING)"},
		{"cast_date_eq_morning", "= CAST(? AS DATE)", []any{morning}, "[1]", "IndexScan(X_D, [=] COVERING)"},
		{"cast_date_eq_east_midnight", "= CAST(? AS DATE)", []any{eastMidnight}, "[]", "IndexScan(X_D, [=] COVERING)"},
	} {
		idx := answer("SELECT id FROM X WHERE D "+tc.cond+" ORDER BY id", tc.args...)
		res := answer("SELECT id FROM X WHERE R "+tc.cond+" ORDER BY id", tc.args...)
		plan := explain("SELECT id FROM X WHERE D "+tc.cond, tc.args...)
		resPlan := explain("SELECT id FROM X WHERE R "+tc.cond, tc.args...)
		if idx != tc.want || res != tc.want || !strings.Contains(plan, tc.scan) {
			t.Errorf("%s: index=%s residual=%s plan=%s; want both %s over %s",
				tc.name, idx, res, plan, tc.want, tc.scan)
		}
		// The copy R is not indexed, so its comparison must be a residual
		// filter; an index BOUND by it would make the pair compare two index
		// reads. PREFER_INDEX may read X_D whole under the residual (F-7c):
		// that read binds nothing.
		boundIndex := strings.Contains(resPlan, "IndexScan") && !strings.Contains(resPlan, "IndexScan(X_D, [*])")
		if !strings.Contains(resPlan, "PredicatesFilter(") || boundIndex {
			t.Errorf("%s: the residual copy's plan is %s, want a PredicatesFilter over an unbounded read", tc.name, resPlan)
		}
	}
}
