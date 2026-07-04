package sqldriver_test

// Regression for the DST metamorphic finding: SQL-standard MIN/MAX GROUP BY must
// map to a PERMUTED_MIN/MAX index (matching Java's NumericAggregationValue.Min/Max
// → PERMUTED_MIN/MAX), NOT the atomic MAX_EVER_LONG. Two facets the atomic
// max-ever index got wrong and permuted gets right:
//
//	(a) an all-NULL group has a real index entry (NULL extremum), so a GROUP BY
//	    served by the index emits [g, NULL] — max_ever_long skipped NULLs and
//	    dropped the whole group;
//	(b) MIN/MAX track the CURRENT extremum: after the prior max row is deleted or
//	    updated downward, the index returns the new (lower) max — max_ever_long
//	    returned the stale-high value forever.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_AggregateIndexMinMax_NullGroupAndCurrentExtremum(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggidx_ng")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggidx_ng")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggidxng "+
			"CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX min_by_g AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX max_by_g AS SELECT MAX(v) FROM t GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggidx_ng/s WITH TEMPLATE aggidxng")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggidx_ng?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// g=1 has a value; g=2 is all-NULL.
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id,g,v) VALUES (1,1,10),(2,2,NULL),(3,2,NULL)")

	// queryGroups runs q (asserting it uses the aggregate index) and returns
	// group -> (aggregate, isNull).
	type agg struct {
		val    int64
		isNull bool
	}
	queryGroups := func(t *testing.T, q string) map[int64]agg {
		t.Helper()
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "AggregateIndex") {
			t.Fatalf("query must use an AggregateIndex scan, got: %s", plan)
		}
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		out := map[int64]agg{}
		for rows.Next() {
			var g int64
			var a sql.NullInt64
			if err := rows.Scan(&g, &a); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[g] = agg{val: a.Int64, isNull: !a.Valid}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	// Facet (a): the all-NULL group g=2 MUST appear as [2, NULL] from the index.
	t.Run("all_null_group_kept_max", func(t *testing.T) {
		got := queryGroups(t, "SELECT g, MAX(v) FROM t GROUP BY g")
		if len(got) != 2 {
			t.Fatalf("MAX: got %d groups %v, want 2 (incl. all-NULL g=2)", len(got), got)
		}
		if a := got[1]; a.isNull || a.val != 10 {
			t.Errorf("MAX g=1 => %+v, want 10", a)
		}
		if a, ok := got[2]; !ok || !a.isNull {
			t.Errorf("MAX g=2 => %+v (present=%v), want NULL", a, ok)
		}
	})
	t.Run("all_null_group_kept_min", func(t *testing.T) {
		got := queryGroups(t, "SELECT g, MIN(v) FROM t GROUP BY g")
		if len(got) != 2 {
			t.Fatalf("MIN: got %d groups %v, want 2 (incl. all-NULL g=2)", len(got), got)
		}
		if a, ok := got[2]; !ok || !a.isNull {
			t.Errorf("MIN g=2 => %+v (present=%v), want NULL", a, ok)
		}
	})

	// Facet (b): current-max semantics. Add a higher value to g=1, then delete it;
	// the index must return the remaining (lower) max, not the stale-high one.
	t.Run("current_max_after_delete", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id,g,v) VALUES (4,1,99)")
		got := queryGroups(t, "SELECT g, MAX(v) FROM t GROUP BY g")
		if a := got[1]; a.isNull || a.val != 99 {
			t.Fatalf("MAX g=1 after insert 99 => %+v, want 99", a)
		}
		mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE id = 4")
		got = queryGroups(t, "SELECT g, MAX(v) FROM t GROUP BY g")
		if a := got[1]; a.isNull || a.val != 10 {
			t.Errorf("MAX g=1 after deleting the max row => %+v, want 10 (max_ever_long would wrongly return 99)", a)
		}
	})
}
