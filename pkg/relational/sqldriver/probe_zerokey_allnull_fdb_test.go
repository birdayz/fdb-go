package sqldriver_test

// PROBE (investigation scratch): the PRESENT-zero all-NULL group.
//
// The existing vacated-group test covers the ABSENT case (a group whose values
// were ALWAYS NULL has no SUM key at all, so the merge takes its absent branch
// and answers NULL). This probe covers the sibling the merge's MATCHED branch
// owns: a group that once held non-NULL values, so the SUM index has a key,
// whose last non-NULL value is then removed while a NULL-valued row remains.
// With clearWhenZero=false the SUM key survives holding 0; COUNT(*) stays
// positive because rows still exist; so the merge matches and emits 0 where
// SQL requires NULL.
//
// Two removal routes are probed separately because they take different
// maintainer paths: UPDATE (old value subtracted, new NULL contributes nothing)
// and DELETE (old value subtracted, row gone).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_ProbeZeroKeyAllNullGroup(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_zkan")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_zkan")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE zkan "+
			"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE ao (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_min_g AS SELECT MIN(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_max_g AS SELECT MAX(v) FROM ai GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_zkan/s WITH TEMPLATE zkan")
	dsn := fmt.Sprintf("fdbsql:///testdb_zkan?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// g=10 : update-to-NULL route. pk=1 holds 7, pk=2 holds NULL.
	// g=11 : delete-leaving-NULL route. pk=3 holds 9, pk=4 holds NULL.
	// g=12 : control, never disturbed, all-NULL from the start (ABSENT case).
	// g=13 : control, live non-zero.
	for _, tbl := range []string{"ai", "ao"} {
		mwjoMustExec(t, db, ctx, "INSERT INTO "+tbl+" (pk,g,v) VALUES "+
			"(1,10,7),(2,10,NULL),"+
			"(3,11,9),(4,11,NULL),"+
			"(5,12,NULL),(6,12,NULL),"+
			"(7,13,3),(8,13,4)")
	}
	for _, tbl := range []string{"ai", "ao"} {
		mwjoMustExec(t, db, ctx, "UPDATE "+tbl+" SET v = NULL WHERE pk = 1")
		mwjoMustExec(t, db, ctx, "DELETE FROM "+tbl+" WHERE pk = 3")
	}

	rowsOf := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		var out []string
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(sql.NullString)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			var parts []string
			for _, c := range cells {
				ns := c.(*sql.NullString)
				if ns.Valid {
					parts = append(parts, ns.String)
				} else {
					parts = append(parts, "NULL")
				}
			}
			out = append(out, "["+strings.Join(parts, " ")+"]")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return out
	}
	explain := func(t *testing.T, q string) string {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN %q: %v", q, err)
		}
		return plan
	}

	// pin records the MEASURED behaviour. `wantIndexed` is what the
	// index-backed spelling answers today; `agreesWithOracle` says whether that
	// equals the base-scan answer. Where it does not, the difference is the
	// present-zero SUM defect, and it is BYTE-IDENTICAL to what Java's own
	// aggregate index answers for the same data (see
	// conformance/probe_zerokey_allnull_java_test.go) — so it is a shared
	// property of the SUM index's storage, not something the group-existence
	// merge introduced.
	pin := func(name, indexedQ, oracleQ, wantIndexed string, agreesWithOracle bool) {
		t.Run(name, func(t *testing.T) {
			plan := explain(t, indexedQ)
			got := strings.Join(rowsOf(t, indexedQ), ",")
			oracle := strings.Join(rowsOf(t, oracleQ), ",")
			t.Logf("PROBE %s\n  query : %s\n  plan  : %s\n  INDEX : %s\n  ORACLE: %s",
				name, indexedQ, plan, got, oracle)
			if got != wantIndexed {
				t.Errorf("%s: index-backed rows changed.\n  got : %s\n  want: %s\n"+
					"Either the present-zero SUM defect moved, or the merge's matched "+
					"branch changed what it passes through.", name, got, wantIndexed)
			}
			if (got == oracle) != agreesWithOracle {
				t.Errorf("%s: index-vs-scan agreement flipped (agree=%v, expected %v).\n"+
					"  index : %s\n  oracle: %s\nA group whose SUM key was decremented to "+
					"zero while only NULL-valued rows remain reads 0 from the index and "+
					"NULL from the scan. If this now AGREES, the defect was fixed and this "+
					"pin must be re-armed to the correct expectation.",
					name, got == oracle, agreesWithOracle, got, oracle)
			}
		})
	}

	// SUM: g=10 (update-to-NULL) and g=11 (delete-leaving-NULL) read 0 where SQL
	// says NULL. g=12 (all-NULL from the start, no key ever written) correctly
	// reads NULL — that is the merge's ABSENT branch, and it is where Go is
	// strictly better than Java, which drops g=12 entirely.
	pin("sum", "SELECT g, SUM(v) FROM ai GROUP BY g", "SELECT g, SUM(v) FROM ao GROUP BY g",
		"[10 0],[11 0],[12 NULL],[13 7]", false)
	// COUNT(col) is NOT exposed: its SQL answer for an all-NULL group is 0, which
	// is exactly what both a present-zero key and an absent key yield.
	pin("count-col", "SELECT g, COUNT(v) FROM ai GROUP BY g", "SELECT g, COUNT(v) FROM ao GROUP BY g",
		"[10 0],[11 0],[12 0],[13 2]", true)
	pin("count-star", "SELECT g, COUNT(*) FROM ai GROUP BY g", "SELECT g, COUNT(*) FROM ao GROUP BY g",
		"[10 2],[11 1],[12 2],[13 2]", true)
	// MIN/MAX have no analogue: SQL MIN/MAX map to PERMUTED_MIN/PERMUTED_MAX,
	// whose result comes from the entry KEY (a real per-record entry deleted with
	// the record), not from an ADD accumulator that can only be decremented. A
	// NULL v writes no entry at all, so an all-NULL group has none and reads NULL.
	pin("min", "SELECT g, MIN(v) FROM ai GROUP BY g", "SELECT g, MIN(v) FROM ao GROUP BY g",
		"[10 NULL],[11 NULL],[12 NULL],[13 3]", true)
	pin("max", "SELECT g, MAX(v) FROM ai GROUP BY g", "SELECT g, MAX(v) FROM ao GROUP BY g",
		"[10 NULL],[11 NULL],[12 NULL],[13 4]", true)
	pin("multi", "SELECT g, SUM(v), COUNT(v) FROM ai GROUP BY g",
		"SELECT g, SUM(v), COUNT(v) FROM ao GROUP BY g",
		"[10 0 0],[11 0 0],[12 NULL 0],[13 7 2]", false)
}
