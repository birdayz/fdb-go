package sqltest

// What a SUM / COUNT aggregate index answers for groups its key set does not
// describe exactly, pinned as Java answers it.
//
// The aggregate index's key set is not the set of groups:
//
//   - an atomic ADD that decrements a group to zero leaves the key behind
//     (relational DDL never sets clearWhenZero), so a group vacated by DELETE
//     or by an UPDATE that moves its last row away still answers, with 0;
//   - no entry is ever written for a NULL value
//     (AtomicMutation.Standard.getMutationParam returns null), so a group whose
//     every value is NULL has no SUM or COUNT(col) key and no row.
//
// Java reads the index alone (AggregateIndexMatchCandidate.java has no notion
// of group existence, RecordQueryAggregateIndexPlan.java emits one row per
// entry), and its own corpus records the difference from the records' answer
// as an open bug: aggregate-empty-table.yamsql has the exposing queries
// commented out with TODO references, beside an unindexed control asserting the
// records' answer. Go reads the index alone too. Each read below pins both
// answers, from twin tables `ai` (indexed) and `ao` (no index), and that the
// indexed one really is index-backed.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

// aggVacRows runs q and returns its rows rendered "[a b ...]", sorted.
func aggVacRows(t *testing.T, ctx context.Context, db *sql.DB, q string) string {
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
			if ns := c.(*sql.NullString); ns.Valid {
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
	return strings.Join(out, ",")
}

// aggVacPin asserts the indexed read (over ai) answers wantIndex through an
// aggregate index, and the same read over ao answers wantRecords.
func aggVacPin(t *testing.T, ctx context.Context, db *sql.DB, name, indexedQ, wantIndex, wantRecords string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+indexedQ).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "AggregateIndex(") {
			t.Fatalf("%s is meant to read the aggregate index but planned as: %s", indexedQ, plan)
		}
		if got := aggVacRows(t, ctx, db, indexedQ); got != wantIndex {
			t.Errorf("index-backed answer is not Java's\n  query: %s\n  plan: %s\n  got : %s\n  want: %s",
				indexedQ, plan, got, wantIndex)
		}
		recordsQ := strings.ReplaceAll(indexedQ, " FROM ai ", " FROM ao ")
		if got := aggVacRows(t, ctx, db, recordsQ); got != wantRecords {
			t.Errorf("records' answer moved\n  query: %s\n  got : %s\n  want: %s", recordsQ, got, wantRecords)
		}
	})
}

func TestFDB_AggregateIndexVacatedGroup(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_aggvac")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_aggvac")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggvac "+
			"CREATE TABLE ai (pk BIGINT, d DOUBLE, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE ao (pk BIGINT, d DOUBLE, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_d AS SELECT SUM(v) FROM ai GROUP BY d "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_min_g AS SELECT MIN(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_max_g AS SELECT MAX(v) FROM ai GROUP BY g")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_aggvac/s WITH TEMPLATE aggvac")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_AGGVAC?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Same rows in both tables.
	//   g=1 / d=7.5 : vacated by UPDATE below (its rows move to g=2 / d=1.5)
	//   g=2 / d=1.5 : survives
	//   g=3 / d=2.5 : vacated by DELETE below
	//   g=4 / d=3.5 : sums to zero (+5, -5), live
	//   g=5 / d=4.5 : every v is NULL, live
	for _, tbl := range []string{"ai", "ao"} {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,d,g,v) VALUES "+
			"(1,7.5,1,10),(2,7.5,1,20),"+
			"(3,1.5,2,30),"+
			"(4,2.5,3,40),(5,2.5,3,50),"+
			"(6,3.5,4,5),(7,3.5,4,-5),"+
			"(8,4.5,5,NULL),(9,4.5,5,NULL)")
		testkit.MustExecCtx(t, db, ctx, "UPDATE "+tbl+" SET d = 1.5, g = 2 WHERE g = 1")
		testkit.MustExecCtx(t, db, ctx, "DELETE FROM "+tbl+" WHERE g = 3")
	}

	aggVacPin(t, ctx, db, "sum", "SELECT g, SUM(v) FROM ai GROUP BY g",
		"[1 0],[2 60],[3 0],[4 0]", "[2 60],[4 0],[5 NULL]")
	aggVacPin(t, ctx, db, "sum-by-double", "SELECT d, SUM(v) FROM ai GROUP BY d",
		"[1.5 60],[2.5 0],[3.5 0],[7.5 0]", "[1.5 60],[3.5 0],[4.5 NULL]")
	aggVacPin(t, ctx, db, "count-star", "SELECT g, COUNT(*) FROM ai GROUP BY g",
		"[1 0],[2 3],[3 0],[4 2],[5 2]", "[2 3],[4 2],[5 2]")
	aggVacPin(t, ctx, db, "count-col", "SELECT g, COUNT(v) FROM ai GROUP BY g",
		"[1 0],[2 3],[3 0],[4 2]", "[2 3],[4 2],[5 0]")
	// A multi-aggregate read is Java's inner intersection of the two indexes:
	// the vacated groups have a zero key in both, the all-NULL group in neither.
	aggVacPin(t, ctx, db, "multi-aggregate", "SELECT g, SUM(v), COUNT(v) FROM ai GROUP BY g",
		"[1 0 0],[2 60 3],[3 0 0],[4 0 2]", "[2 60 3],[4 0 2],[5 NULL 0]")
	aggVacPin(t, ctx, db, "having-sum-is-null",
		"SELECT g, SUM(v) FROM ai GROUP BY g HAVING SUM(v) IS NULL", "", "[5 NULL]")
	aggVacPin(t, ctx, db, "having-count-col-zero",
		"SELECT g, COUNT(v) FROM ai GROUP BY g HAVING COUNT(v) = 0", "[1 0],[3 0]", "[5 0]")
	// MIN/MAX are exact: SQL MIN/MAX map to PERMUTED_MIN/PERMUTED_MAX, which keep
	// a real per-record entry and delete it with the record.
	aggVacPin(t, ctx, db, "min", "SELECT g, MIN(v) FROM ai GROUP BY g",
		"[2 10],[4 -5],[5 NULL]", "[2 10],[4 -5],[5 NULL]")
	aggVacPin(t, ctx, db, "max", "SELECT g, MAX(v) FROM ai GROUP BY g",
		"[2 30],[4 5],[5 NULL]", "[2 30],[4 5],[5 NULL]")

	// The ungrouped COUNT(*) spelling over an emptied table reads the stored 0,
	// which is also SQL's answer.
	t.Run("count-star-ungrouped-empty-table", func(t *testing.T) {
		testkit.MustExecCtx(t, setup, ctx,
			"CREATE SCHEMA TEMPLATE aggvacu "+
				"CREATE TABLE e (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
				"CREATE INDEX e_cnt AS SELECT COUNT(*) FROM e")
		testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_aggvac/su WITH TEMPLATE aggvacu")
		udsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_AGGVAC?cluster_file=%s&schema=SU", testkit.ClusterFile())
		udb, err := sql.Open("fdbsql", udsn)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer udb.Close()
		count := func(what string) int64 {
			var n int64
			if err := udb.QueryRowContext(ctx, "SELECT COUNT(*) FROM e").Scan(&n); err != nil {
				t.Fatalf("count %s: %v", what, err)
			}
			return n
		}
		if n := count("empty"); n != 0 {
			t.Fatalf("COUNT(*) on a never-populated table = %d, want 0", n)
		}
		testkit.MustExecCtx(t, udb, ctx, "INSERT INTO e (pk,v) VALUES (1,1),(2,2)")
		if n := count("populated"); n != 2 {
			t.Fatalf("COUNT(*) after 2 inserts = %d, want 2", n)
		}
		testkit.MustExecCtx(t, udb, ctx, "DELETE FROM e")
		if n := count("emptied"); n != 0 {
			t.Fatalf("COUNT(*) after emptying the table = %d, want 0", n)
		}
	})
}

// TestFDB_AggregateIndexVacatedGroup_ZeroGroups adds the two live zero groups to
// the vacated ones: g=4 cancels to zero across two separate statements, and g=6
// holds two zeros. Both are live and answer 0 from the index as from the records,
// beside the vacated groups' residue 0 that only the index answers. Java reads
// these exact rows (RFC-209 §2 measured them before Go diverged).
func TestFDB_AggregateIndexVacatedGroup_ZeroGroups(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_aggvacpin")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_aggvacpin")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggvacpin "+
			"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE ao (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_aggvacpin/s WITH TEMPLATE aggvacpin")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_AGGVACPIN?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, tbl := range []string{"ai", "ao"} {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,g,v) VALUES "+
			"(1,1,10),(2,1,20),"+ // g=1 vacated by UPDATE below
			"(3,2,30),"+ // g=2 survives
			"(4,3,40),(5,3,50),"+ // g=3 vacated by DELETE below
			"(8,5,NULL),(9,5,NULL),"+ // g=5 all-NULL
			"(10,6,0),(11,6,0)") // g=6 all values zero
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,g,v) VALUES (6,4,5)")
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,g,v) VALUES (7,4,-5)")
		testkit.MustExecCtx(t, db, ctx, "UPDATE "+tbl+" SET g = 2 WHERE g = 1")
		testkit.MustExecCtx(t, db, ctx, "DELETE FROM "+tbl+" WHERE g = 3")
	}
	aggVacPin(t, ctx, db, "sum", "SELECT g, SUM(v) FROM ai GROUP BY g",
		"[1 0],[2 60],[3 0],[4 0],[6 0]", "[2 60],[4 0],[5 NULL],[6 0]")
	aggVacPin(t, ctx, db, "count-col", "SELECT g, COUNT(v) FROM ai GROUP BY g",
		"[1 0],[2 3],[3 0],[4 2],[6 2]", "[2 3],[4 2],[5 0],[6 2]")
	aggVacPin(t, ctx, db, "count-star", "SELECT g, COUNT(*) FROM ai GROUP BY g",
		"[1 0],[2 3],[3 0],[4 2],[5 2],[6 2]", "[2 3],[4 2],[5 2],[6 2]")
}

// TestFDB_AggregateIndexVacatedGroup_UngroupedSumEmptyTable pins Java's
// ungrouped SUM index: aggregate-empty-table.yamsql :577-580 plans
// `select sum(col1) from T2` as `AISCAN(T2_I5 <,> BY_GROUP ...)` over an
// emptied table and reads the stored 0, where the unindexed control (:547-550)
// answers NULL. Two live rows that cancel answer 0 both ways.
func TestFDB_AggregateIndexVacatedGroup_UngroupedSumEmptyTable(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := testkit.OpenDB(t, "/FRL/testdb_aggvacsum")
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE /FRL/testdb_aggvacsum")
	testkit.MustExecCtx(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggvacsum "+
			"CREATE TABLE ai (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE ao (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE bi (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE bo (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum AS SELECT SUM(v) FROM ai "+
			"CREATE INDEX bi_sum AS SELECT SUM(v) FROM bi")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA /FRL/testdb_aggvacsum/s WITH TEMPLATE aggvacsum")
	dsn := fmt.Sprintf("fdbsql:///FRL/TESTDB_AGGVACSUM?cluster_file=%s&schema=S", testkit.ClusterFile())
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// A never-populated table has no entry: the ungrouped answer is the one NULL
	// row either way.
	aggVacPin(t, ctx, db, "never-populated", "SELECT SUM(v) FROM ai ", "[NULL]", "[NULL]")

	for _, tbl := range []string{"ai", "ao"} {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,v) VALUES (1,10),(2,20)")
		testkit.MustExecCtx(t, db, ctx, "DELETE FROM "+tbl)
	}
	aggVacPin(t, ctx, db, "emptied-by-delete", "SELECT SUM(v) FROM ai ", "[0]", "[NULL]")

	for _, tbl := range []string{"bi", "bo"} {
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,v) VALUES (1,5)")
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO "+tbl+" (pk,v) VALUES (2,-5)")
	}
	t.Run("cancels-to-zero-live-rows", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT SUM(v) FROM bi").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "AggregateIndex(") {
			t.Fatalf("the ungrouped SUM is not read from its index: %s", plan)
		}
		if got := aggVacRows(t, ctx, db, "SELECT SUM(v) FROM bi"); got != "[0]" {
			t.Errorf("SUM over two live rows that cancel = %s, want [0]", got)
		}
		if got := aggVacRows(t, ctx, db, "SELECT SUM(v) FROM bo"); got != "[0]" {
			t.Errorf("records' SUM = %s, want [0]", got)
		}
	})
}
