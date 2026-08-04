package sqldriver_test

// Differential probe: an aggregate index must never invent or drop a group
// relative to the same query answered without an index.
//
// Two tables carry identical data. `ai` has the aggregate (materialized) indexes,
// `ao` has none, so the same GROUP BY is answered by a streaming aggregation over
// the base rows -- the oracle. Every shape below is run against both and the
// answers must agree.
//
// The hard case both directions have to survive at once: a group whose values
// legitimately sum to zero MUST appear with SUM 0, while a group all of whose rows
// moved away MUST NOT appear at all. Clearing on zero fixes the second and breaks
// the first; keeping zero fixes the first and breaks the second.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_AggregateIndexVacatedGroup(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggvac")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggvac")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggvac "+
			"CREATE TABLE ai (pk BIGINT, d DOUBLE, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE ao (pk BIGINT, d DOUBLE, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_d AS SELECT SUM(v) FROM ai GROUP BY d "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_min_g AS SELECT MIN(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_max_g AS SELECT MAX(v) FROM ai GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggvac/s WITH TEMPLATE aggvac")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggvac?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Same rows in both tables.
	//   g=1 / d=7.5 : vacated by UPDATE below
	//   g=2 / d=1.5 : survives
	//   g=3         : vacated by DELETE below
	//   g=4         : legitimately sums to zero (+5, -5) and must SURVIVE
	//   g=5         : every v is NULL -- group exists, COUNT(v)=0, SUM(v)=NULL
	for _, tbl := range []string{"ai", "ao"} {
		mwjoMustExec(t, db, ctx, "INSERT INTO "+tbl+" (pk,d,g,v) VALUES "+
			"(1,7.5,1,10),(2,7.5,1,20),"+
			"(3,1.5,2,30),"+
			"(4,2.5,3,40),(5,2.5,3,50),"+
			"(6,3.5,4,5),(7,3.5,4,-5),"+
			"(8,4.5,5,NULL),(9,4.5,5,NULL)")
	}
	// Vacate g=1/d=7.5 by UPDATE, g=3 by DELETE.
	for _, tbl := range []string{"ai", "ao"} {
		mwjoMustExec(t, db, ctx, "UPDATE "+tbl+" SET d = 1.5, g = 2 WHERE g = 1")
		mwjoMustExec(t, db, ctx, "DELETE FROM "+tbl+" WHERE g = 3")
	}

	// rowsOf runs q and returns its rows rendered as strings, sorted.
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

	// differ asserts that the indexed spelling agrees with the oracle, and that the
	// indexed spelling really is index-backed (otherwise the test proves nothing).
	differ := func(name, indexedQ, oracleQ string) {
		t.Run(name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+indexedQ).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "AggregateIndex") {
				t.Fatalf("%s is meant to exercise the aggregate-index path but planned as: %s\n"+
					"If the planner legitimately stopped using the index here, this test no longer "+
					"covers the vacated-group dimension -- re-arm it, do not relax it.", name, plan)
			}
			got := rowsOf(t, indexedQ)
			want := rowsOf(t, oracleQ)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("index-backed answer disagrees with the unindexed oracle\n"+
					"  query (indexed): %s\n  plan: %s\n  indexed: %v\n  oracle : %v",
					indexedQ, plan, got, want)
			}
		})
	}

	// MIN/MAX are exact because SQL MIN/MAX map to PERMUTED_MIN/PERMUTED_MAX,
	// which keep a real per-record entry and delete it with the record, rather
	// than an atomic accumulator that can only be decremented.
	differ("min-vacated",
		"SELECT g, MIN(v) FROM ai GROUP BY g",
		"SELECT g, MIN(v) FROM ao GROUP BY g")
	differ("max-vacated",
		"SELECT g, MAX(v) FROM ai GROUP BY g",
		"SELECT g, MAX(v) FROM ao GROUP BY g")

	// A guard for whatever fix lands, not a detector of today's bug: the
	// ungrouped COUNT(*) spelling must keep answering 0 for an emptied table.
	// Any fix that makes a vacated group disappear has to leave this alone,
	// because here "no entry" means the empty-table case and has to coalesce
	// to 0 rather than to no rows at all.
	t.Run("count-star-ungrouped-empty-table", func(t *testing.T) {
		mwjoMustExec(t, setup, ctx,
			"CREATE SCHEMA TEMPLATE aggvacu "+
				"CREATE TABLE e (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
				"CREATE INDEX e_cnt AS SELECT COUNT(*) FROM e")
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggvac/su WITH TEMPLATE aggvacu")
		udsn := fmt.Sprintf("fdbsql:///testdb_aggvac?cluster_file=%s&schema=su", clusterFilePath)
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
		mwjoMustExec(t, udb, ctx, "INSERT INTO e (pk,v) VALUES (1,1),(2,2)")
		if n := count("populated"); n != 2 {
			t.Fatalf("COUNT(*) after 2 inserts = %d, want 2", n)
		}
		mwjoMustExec(t, udb, ctx, "DELETE FROM e")
		if n := count("emptied"); n != 0 {
			t.Fatalf("COUNT(*) after emptying the table = %d, want 0. An ungrouped aggregate has "+
				"exactly one group, which exists whether or not the table has rows, so any fix "+
				"that makes vacated GROUPS disappear must not make this row disappear -- the "+
				"empty scan has to keep coalescing to 0 rather than returning no row.", n)
		}
	})
}

// TestFDB_AggregateIndexVacatedGroup_SumAndCountColPins pins a LIVE divergence.
// Read it as a problem statement, not as a bug under test: it asserts what the
// engine does TODAY and goes RED the moment that changes, in either direction.
//
// THE SHAPE. A SUM index and a COUNT(col) index derive the set of groups from
// their own key set, and that key set is not the set of groups:
//
//   - it OVER-approximates: an atomic ADD that decrements a group to zero leaves
//     the key behind, so a group vacated by DELETE or by an UPDATE that moves its
//     last row elsewhere still answers, with 0.
//   - it UNDER-approximates: no entry is ever written for a NULL value
//     (AtomicMutation.Standard.getMutationParam returns null,
//     fdb-record-layer-core/.../indexes/AtomicMutation.java:181-189), so a group
//     whose every value is NULL has no key at all and vanishes.
//
// Both directions are visible in the assertions below: group 1 and group 3 are
// vacated yet answer 0, and group 5 is all-NULL yet is missing.
//
// WHY IT IS NOT FIXED HERE. Two candidate fixes were built and measured, and
// both are blocked for reasons worth recording so they are not re-attempted
// blind.
//
// 1. clearWhenZero on the index, set by the DDL generator. For COUNT(*) this is
// semantically exact -- an existing group's COUNT(*) is never zero -- and it
// does make count-star agree with the oracle. It is still not shippable: the
// index options live in the stored metadata, and
// conformance/index_ddl_metadata_conformance_test.go compares those bytes
// against the real Java engine for the same DDL. Java's relational DDL never
// sets the option (MaterializedViewIndexGenerator.java:235-243), so setting it
// fails that test with "I_CNT options diverge (UNIQUE lives here -- a dropped
// option is a wire divergence)". Wire compatibility outranks the wrong rows, so
// the option cannot be the fix and the conformance golden must not be edited to
// admit it.
//
// For SUM the same option is additionally wrong on its own terms:
// IndexOptions.CLEAR_WHEN_ZERO documents that "a SUM index will not have
// entries for groups all of whose indexed values are zero". Groups 4 (+5 and
// -5) and 6 (two rows of 0) below are exactly those groups, both with live
// rows, and both must keep answering 0.
//
// 2. A read-side filter, which is the design this should get: an entry whose
// COUNT(*) is zero denotes a group that does not exist, so dropping it at scan
// time is exact, changes nothing on the wire, and leaves the ungrouped
// spelling correct because an empty scan already coalesces to 0. It is a
// Cascades planner/executor change and therefore needs an RFC and a
// query-engine review lap before implementation, so it is owner-gated rather
// than folded in here. SUM and COUNT(col) need the same read side driven by a
// companion COUNT(*) group set, since their own key set can neither prove nor
// disprove that a group exists.
//
// The SUM hazard is not theoretical -- it was measured. Forcing clearWhenZero
// on for SUM and re-running this test yields exactly "[2 60]": the phantoms go,
// and groups 4 and 6 go with them, which is the wrong answer in the other
// direction.
//
// That measurement was masked at first by a second, unrelated defect, and the
// masking is worth stating because the evidence it produced looked
// self-contradictory. updateInsertOnlyGrouped served grouped SUM inserts from
// two inline AddBytes fast paths that wrote the ADD directly instead of calling
// sumMutation.applyMutation, and so never issued the COMPARE_AND_CLEAR. Three
// legs, three different routes:
//
//   - groups 4 and 6 are grouped SUM INSERTs -> inline fast path -> no clear,
//     so they survived and the option looked harmless;
//   - groups 1 and 3 are vacated by UPDATE/DELETE -> the general Update path ->
//     applyMutation -> the clear fired, so the phantoms did go;
//   - the record-layer sibling test is UNGROUPED -> updateInsertOnly ->
//     applyMutation -> the clear fired, so the adding path looked correct.
//
// Hence "the adding path clears in one test and not in the other" was never
// about the planner or about which index was chosen; it was about which of the
// three maintenance routes each case took. The fast paths now go through
// atomicMutationIndexMaintainer.clearIfZero, and with that fixed the
// CLEAR_WHEN_ZERO contract reproduces exactly as documented.
//
// This is not a Go slip. Java's SQL layer has the identical defect: relational
// DDL never sets clearWhenZero (MaterializedViewIndexGenerator.java:235-243) and
// nothing filters on the read side (AggregateIndexMatchCandidate.java:414-430,
// RecordQueryAggregateIndexPlan.java:135-143). Java's own yaml-tests file
// aggregate-empty-table.yamsql has the exposing queries COMMENTED OUT with TODO
// bug references ("count index returns 0 instead of nothing when running on a
// table that was cleared"; "enhance SUM aggregate index to disambiguate null
// results and 0 results"), while its unindexed control table asserts the correct
// empty answer. So Java knows and has not fixed it.
//
// WHAT THE FIX HAS TO BE. Group existence is not derivable from a SUM or a
// COUNT(col) accumulator -- the stored bytes for "vacated" and for "cancels to
// zero" are identical, so no read-side filter and no maintainer tweak can tell
// them apart. It has to come from a companion COUNT(*) index on the same
// grouping key (which this change already makes exact), with the aggregate scan
// driven by the companion's group set: present-in-companion but absent-from-SUM
// then correctly yields NULL, which also repairs the all-NULL group. That is the
// standard materialized-view rule -- SQL Server requires COUNT_BIG(*) in any
// indexed view carrying an aggregate for precisely this reason. It needs a
// planner and executor change (a second BY_GROUP scan merged on the grouping
// key) and therefore an RFC and a query-engine review lap, so it is owner-gated
// rather than folded in here.
func TestFDB_AggregateIndexVacatedGroup_SumAndCountColPins(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggvacpin")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggvacpin")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggvacpin "+
			"CREATE TABLE ai (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggvacpin/s WITH TEMPLATE aggvacpin")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggvacpin?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO ai (pk,g,v) VALUES "+
		"(1,1,10),(2,1,20),"+ // g=1 vacated by UPDATE below
		"(3,2,30),"+ // g=2 survives
		"(4,3,40),(5,3,50),"+ // g=3 vacated by DELETE below
		"(8,5,NULL),(9,5,NULL),"+ // g=5 all-NULL: group exists, SUM NULL, COUNT(v) 0
		"(10,6,0),(11,6,0)") // g=6 all values ZERO: group exists and SUM is 0
	// g=4 cancels to zero, and its two rows go in as SEPARATE statements so the
	// running total actually lands on zero between two mutations rather than
	// only within one.
	mwjoMustExec(t, db, ctx, "INSERT INTO ai (pk,g,v) VALUES (6,4,5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO ai (pk,g,v) VALUES (7,4,-5)")
	mwjoMustExec(t, db, ctx, "UPDATE ai SET g = 2 WHERE g = 1")
	mwjoMustExec(t, db, ctx, "DELETE FROM ai WHERE g = 3")

	pin := func(name, q, current, correct string) {
		t.Run(name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var g int64
				var a sql.NullInt64
				if err := rows.Scan(&g, &a); err != nil {
					t.Fatalf("scan: %v", err)
				}
				if a.Valid {
					out = append(out, fmt.Sprintf("[%d %d]", g, a.Int64))
				} else {
					out = append(out, fmt.Sprintf("[%d NULL]", g))
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			sort.Strings(out)
			got := strings.Join(out, " ")
			if got != current {
				t.Fatalf("the pinned divergence moved.\n  query  : %s\n  was    : %s\n"+
					"  now    : %s\n  correct: %s\n"+
					"If it now matches the correct answer, the companion-COUNT(*) fix landed: "+
					"delete this pin and assert oracle equality in "+
					"TestFDB_AggregateIndexVacatedGroup instead. If it moved anywhere else, "+
					"something regressed.", q, current, got, correct)
			}
		})
	}

	// g=1 and g=3 are vacated yet answer 0; g=5 is all-NULL yet is absent.
	// g=4 answers 0 correctly and must keep doing so under any fix.
	pin("sum-keeps-vacated-groups-and-drops-the-all-null-group",
		"SELECT g, SUM(v) FROM ai GROUP BY g",
		"[1 0] [2 60] [3 0] [4 0] [6 0]",
		"[2 60] [4 0] [5 NULL] [6 0]")
	pin("count-col-keeps-vacated-groups-and-drops-the-all-null-group",
		"SELECT g, COUNT(v) FROM ai GROUP BY g",
		"[1 0] [2 3] [3 0] [4 2] [6 2]",
		"[2 3] [4 2] [5 0] [6 2]")

	// COUNT(*) is no longer a pin: RFC-209 §5.3(a) repairs it, exactly and
	// alone, because the index being scanned IS the group-existence oracle. The
	// stored value is the group's row count, so a zero can only be residue and
	// the scan drops it. Both phantoms go; g=5 (all-NULL values, two live rows)
	// stays, because COUNT(*) never looks at the value.
	//
	// The EXPLAIN assertion is what makes this a proof rather than a
	// coincidence: the answer must be right BECAUSE the aggregate index was
	// scanned with the drop in force, not because the planner quietly stopped
	// using the index and a streaming aggregation got it right instead.
	t.Run("count-star-drops-vacated-groups", func(t *testing.T) {
		const q = "SELECT g, COUNT(*) FROM ai GROUP BY g"
		var plan string
		if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "AggregateIndex(") {
			t.Fatalf("grouped COUNT(*) is no longer index-backed, so this test no "+
				"longer covers the vacated-group dimension -- re-arm it, do not relax it.\n  plan: %s", plan)
		}
		if !strings.Contains(plan, "live_groups_only") {
			t.Fatalf("the grouped COUNT(*) aggregate scan does not carry the "+
				"vacated-group drop (RFC-209 5.3(a)).\n  plan: %s\n"+
				"Correct rows without it would mean something else is filtering, and the "+
				"drop would be dead code the moment that something else changed.", plan)
		}
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var g, n int64
			if err := rows.Scan(&g, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, fmt.Sprintf("[%d %d]", g, n))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		sort.Strings(out)
		const want = "[2 3] [4 2] [5 2] [6 2]"
		if got := strings.Join(out, " "); got != want {
			t.Fatalf("grouped COUNT(*) over vacated groups\n  got : %s\n  want: %s\n"+
				"g=1 (vacated by UPDATE) and g=3 (vacated by DELETE) must not appear; "+
				"g=5 (two live rows, every v NULL) must, because COUNT(*) never reads the value.",
				got, want)
		}
	})
}

// TestFDB_AggregateIndexVacatedGroup_UngroupedSumEmptyTable pins a NEGATIVE
// result: the ungrouped `SELECT SUM(v) FROM t` divergence that Java has does
// NOT reproduce in Go, and it is important to pin exactly WHY, because the
// reason is an accident of planning rather than a property of the design.
//
// Java's corpus has the divergence in one file: aggregate-empty-table.yamsql
// :547-550 runs `select sum(col1) from T1` with no aggregate index over an
// emptied table and expects NULL, while :577-580 runs the same query over T2,
// which carries SUM index t2_i5, plans it as `AISCAN(T2_I5 <,> BY_GROUP ...)`
// and expects 0. Oracle and index-backed answer, same block, disagreeing.
//
// Go agrees with the oracle on both axes below -- but ONLY because Go's planner
// does not route an ungrouped SUM through the aggregate index at all; the plan
// is a StreamingAgg over a full scan even on the indexed table. There is no
// index-backed path here, so there is nothing to diverge. That is what the plan
// assertions below pin. The moment an ungrouped SUM becomes index-backed, this
// test goes red and the fact it was protecting is gone: the companion-existence
// rule (RFC-209) has to cover the ungrouped case too, and an emptied table must
// then yield NULL rather than the stored 0.
//
// Note this is the opposite conclusion from the grouped COUNT(*) case, where an
// empty scan legitimately coalesces to 0. Do not generalise one to the other.
func TestFDB_AggregateIndexVacatedGroup_UngroupedSumEmptyTable(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggvacsum")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggvacsum")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggvacsum "+
			"CREATE TABLE si (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE so (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX si_sum AS SELECT SUM(v) FROM si")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggvacsum/s WITH TEMPLATE aggvacsum")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggvacsum?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	sumOf := func(t *testing.T, tbl string) (val sql.NullInt64, plan string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, "EXPLAIN SELECT SUM(v) FROM "+tbl).Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN SUM(v) FROM %s: %v", tbl, err)
		}
		if err := db.QueryRowContext(ctx, "SELECT SUM(v) FROM "+tbl).Scan(&val); err != nil {
			t.Fatalf("SELECT SUM(v) FROM %s: %v", tbl, err)
		}
		return val, plan
	}

	show := func(v sql.NullInt64) string {
		if v.Valid {
			return fmt.Sprintf("%d", v.Int64)
		}
		return "NULL"
	}

	// assertAgrees pins both halves of the negative result: the answers match the
	// oracle, AND the reason they match is that the indexed table is not planned
	// through the aggregate index.
	assertAgrees := func(t *testing.T, label string, iv, ov sql.NullInt64, iplan, oplan string) {
		t.Helper()
		if iv.Valid != ov.Valid || iv.Int64 != ov.Int64 {
			t.Fatalf("%s: ungrouped SUM disagrees with the unindexed oracle\n"+
				"  indexed(si)   = %s  plan: %s\n  unindexed(so) = %s  plan: %s\n"+
				"This is Java's aggregate-empty-table.yamsql :547-550 vs :577-580 divergence "+
				"reproducing in Go. It must be fixed by RFC-209's companion-existence rule, "+
				"not by relaxing this test.",
				label, show(iv), iplan, show(ov), oplan)
		}
		if strings.Contains(iplan, "AggregateIndex") || strings.Contains(iplan, "AISCAN") {
			t.Fatalf("%s: the ungrouped SUM over the INDEXED table is now served by the "+
				"aggregate index:\n  plan: %s\n"+
				"That is a real capability change, and it re-arms a bug this test was pinning "+
				"as unreachable. An index-backed ungrouped SUM reads a stored 0 for an emptied "+
				"table, where the correct answer is NULL. Extend the companion-existence rule "+
				"(RFC-209) to the ungrouped case, then update this test -- do not delete it.",
				label, iplan)
		}
	}

	// Axis 1: emptied table (INSERT then DELETE all rows).
	t.Run("emptied-by-delete", func(t *testing.T) {
		mwjoMustExec(t, db, ctx, "INSERT INTO si (pk,v) VALUES (1,10),(2,20)")
		mwjoMustExec(t, db, ctx, "INSERT INTO so (pk,v) VALUES (1,10),(2,20)")
		mwjoMustExec(t, db, ctx, "DELETE FROM si")
		mwjoMustExec(t, db, ctx, "DELETE FROM so")
		iv, iplan := sumOf(t, "si")
		ov, oplan := sumOf(t, "so")
		assertAgrees(t, "emptied-by-delete", iv, ov, iplan, oplan)
		if iv.Valid {
			t.Fatalf("emptied-by-delete: SUM(v) over an emptied table = %d, want NULL. "+
				"SUM over zero rows is NULL; a 0 here is the stored accumulator leaking out.",
				iv.Int64)
		}
	})

	// Axis 2: live rows whose values legitimately cancel to zero (+5, -5),
	// never deleted.
	t.Run("cancels-to-zero-live-rows", func(t *testing.T) {
		mwjoMustExec(t, setup, ctx,
			"CREATE SCHEMA TEMPLATE aggvacsum2 "+
				"CREATE TABLE si (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
				"CREATE TABLE so (pk BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
				"CREATE INDEX si_sum AS SELECT SUM(v) FROM si")
		mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggvacsum/s2 WITH TEMPLATE aggvacsum2")
		dsn2 := fmt.Sprintf("fdbsql:///testdb_aggvacsum?cluster_file=%s&schema=s2", clusterFilePath)
		db2, err := sql.Open("fdbsql", dsn2)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer db2.Close()
		mwjoMustExec(t, db2, ctx, "INSERT INTO si (pk,v) VALUES (1,5)")
		mwjoMustExec(t, db2, ctx, "INSERT INTO si (pk,v) VALUES (2,-5)")
		mwjoMustExec(t, db2, ctx, "INSERT INTO so (pk,v) VALUES (1,5)")
		mwjoMustExec(t, db2, ctx, "INSERT INTO so (pk,v) VALUES (2,-5)")

		var iv, ov sql.NullInt64
		var iplan, oplan string
		if err := db2.QueryRowContext(ctx, "EXPLAIN SELECT SUM(v) FROM si").Scan(&iplan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if err := db2.QueryRowContext(ctx, "SELECT SUM(v) FROM si").Scan(&iv); err != nil {
			t.Fatalf("query si: %v", err)
		}
		if err := db2.QueryRowContext(ctx, "EXPLAIN SELECT SUM(v) FROM so").Scan(&oplan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if err := db2.QueryRowContext(ctx, "SELECT SUM(v) FROM so").Scan(&ov); err != nil {
			t.Fatalf("query so: %v", err)
		}
		assertAgrees(t, "cancels-to-zero-live-rows", iv, ov, iplan, oplan)
		// The mirror of the emptied case: here 0 is the CORRECT answer, because
		// two live rows sum to zero. Any fix that turns an emptied table's 0 into
		// NULL must leave this one alone.
		if !iv.Valid || iv.Int64 != 0 {
			t.Fatalf("cancels-to-zero-live-rows: SUM(v) = %s, want 0. Two live rows sum to "+
				"zero, so 0 is correct here and NULL would be the opposite wrong answer.",
				show(iv))
		}
	})
}
