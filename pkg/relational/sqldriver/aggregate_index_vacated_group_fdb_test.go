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

	"fdb.dev/pkg/recordlayer"
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

	// HAVING over the identity rows (RFC-209 §5.3, "Compensation under outer
	// semantics"; §7). These two predicates change their answers as a DIRECT
	// result of the merge, and they are the ones that most look like a
	// regression in review, so they are pinned explicitly rather than left to
	// the generic oracle comparison.
	//
	// A compensating filter now sees rows that did not previously exist: a group
	// present in the companion but absent from the aggregate index arrives as
	// SUM(v) = NULL or COUNT(v) = 0 instead of not arriving at all. g=5 has two
	// live rows whose every v is NULL, so the SUM index and the COUNT(v) index
	// hold NO entry for it — before the merge there was simply no row for the
	// HAVING to test, and both queries returned nothing.
	//
	// That is the under-approximation repair reaching HAVING, not a filter
	// regression: the oracle returns those groups, so a HAVING over the oracle
	// already returned them, and the two answers converge. Both halves are
	// asserted — equality with the oracle AND the exact expected row — because
	// two empty results also compare equal, and an empty answer here would mean
	// the identity row never reached the filter at all.
	havingOverIdentity := func(name, indexedQ, oracleQ, want string) {
		t.Run(name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+indexedQ).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "GroupExistenceMerge(") {
				t.Fatalf("HAVING over identity rows must sit above the companion-joined merge, "+
					"or this test is not exercising compensation under outer semantics at all.\n"+
					"  query: %s\n  plan : %s", indexedQ, plan)
			}
			got := rowsOf(t, indexedQ)
			oracle := rowsOf(t, oracleQ)
			if strings.Join(got, ",") != strings.Join(oracle, ",") {
				t.Fatalf("index-backed HAVING disagrees with the oracle\n  query: %s\n"+
					"  indexed: %v\n  oracle : %v", indexedQ, got, oracle)
			}
			if strings.Join(got, ",") != want {
				t.Fatalf("HAVING did not return the all-NULL group.\n  query: %s\n"+
					"  got : %v\n  want: %s\n"+
					"g=5 has two live rows whose every v is NULL, so neither the SUM index nor "+
					"the COUNT(v) index holds an entry for it. It can only reach this filter as "+
					"the merge's empty-group identity; an empty answer means the identity row "+
					"was never produced and the under-approximation is back.",
					indexedQ, got, want)
			}
		})
	}
	havingOverIdentity("having-sum-is-null-returns-the-all-null-group",
		"SELECT g, SUM(v) FROM ai GROUP BY g HAVING SUM(v) IS NULL",
		"SELECT g, SUM(v) FROM ao GROUP BY g HAVING SUM(v) IS NULL",
		"[5 NULL]")
	havingOverIdentity("having-count-col-zero-returns-the-all-null-group",
		"SELECT g, COUNT(v) FROM ai GROUP BY g HAVING COUNT(v) = 0",
		"SELECT g, COUNT(v) FROM ao GROUP BY g HAVING COUNT(v) = 0",
		"[5 0]")

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

// TestFDB_AggregateIndexVacatedGroup_SumAndCountColOracle asserts that an
// index-backed grouped SUM and COUNT(col) return exactly what the same query
// returns with no aggregate index at all — the streaming-aggregation oracle.
//
// THE SHAPE THAT WAS WRONG. A SUM index and a COUNT(col) index derive the set
// of groups from their own key set, and that key set is not the set of groups:
//
//   - it OVER-approximates: an atomic ADD that decrements a group to zero
//     leaves the key behind, so a group vacated by DELETE or by an UPDATE that
//     moves its last row elsewhere still answers, with 0.
//   - it UNDER-approximates: no entry is ever written for a NULL value
//     (AtomicMutation.Standard.getMutationParam returns null,
//     fdb-record-layer-core/.../indexes/AtomicMutation.java:181-189), so a group
//     whose every value is NULL has no key at all and vanishes.
//
// Group existence is not derivable from a SUM or COUNT(col) accumulator at all:
// the stored bytes for "vacated" and for "cancels to zero" are identical, so no
// read-side filter over that one stream and no maintainer tweak can tell them
// apart. It comes instead from a companion COUNT(*) index on the same grouping
// key, whose non-zero keys ARE the live groups because it never looks at the
// aggregated value. RFC-209 §5.3(b) drives an outer merge from it:
// present-in-companion but absent-from-SUM yields NULL (0 for COUNT(col)),
// which repairs the all-NULL group, and present-in-SUM but absent-from-companion
// is dropped, which repairs the phantom.
//
// TWO FIXES THAT DO NOT WORK, recorded so they are not re-attempted blind.
//
// 1. clearWhenZero on the index. For COUNT(*) it is semantically exact — a live
// group's COUNT(*) is never zero — and it was built and measured green. It is
// still not shippable: index options live in the STORED metadata, and
// conformance/index_ddl_metadata_conformance_test.go compares those bytes
// against the real Java engine for the same DDL. Java's relational DDL never
// sets the option (MaterializedViewIndexGenerator.java:235-243), so setting it
// fails with "I_CNT options diverge (UNIQUE lives here -- a dropped option is a
// wire divergence)". Wire compatibility outranks the wrong rows.
//
// For SUM the same option is additionally wrong on its own terms:
// IndexOptions.CLEAR_WHEN_ZERO documents that "a SUM index will not have
// entries for groups all of whose indexed values are zero". Groups 4 (+5 and
// -5) and 6 (two rows of 0) below are exactly those groups, both with live
// rows, and both must keep answering 0. Forcing the option on for SUM and
// re-running this test yielded exactly "[2 60]": the phantoms went, and groups
// 4 and 6 went with them — a wrong answer in the other direction.
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
// atomicMutationIndexMaintainer.clearIfZero.
//
// 2. A read-side filter on the aggregate's own stream. Exact for COUNT(*),
// where the index being scanned IS the existence oracle (§5.3(a), asserted
// below). Impossible for SUM and COUNT(col) for the reason above.
//
// This was never a Go slip. Java's SQL layer has the identical defect and its
// own corpus records it as a bug rather than as behaviour: relational DDL never
// sets clearWhenZero (MaterializedViewIndexGenerator.java:235-243) and nothing
// filters on the read side (AggregateIndexMatchCandidate.java:414-430,
// RecordQueryAggregateIndexPlan.java:135-143), while yaml-tests'
// aggregate-empty-table.yamsql has the exposing queries COMMENTED OUT with TODO
// bug references ("count index returns 0 instead of nothing when running on a
// table that was cleared"; "enhance SUM aggregate index to disambiguate null
// results and 0 results") and its unindexed control table asserts the correct
// answer. Answering correctly here is a sanctioned read-side extension, not a
// divergence on semantics Java stands behind.
func TestFDB_AggregateIndexVacatedGroup_SumAndCountColOracle(t *testing.T) {
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

	// The oracle assertion, with the plan pinned alongside it. Both halves are
	// load-bearing: the rows prove the answer is right, and the EXPLAIN proves
	// it is right BECAUSE the companion-joined plan ran — not because the
	// planner quietly stopped using the aggregate index and a streaming
	// aggregation got it right instead. A "fix" that silently drops the index
	// acceleration must fail here.
	oracle := func(name, q, want string) {
		t.Run(name, func(t *testing.T) {
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "GroupExistenceMerge(") {
				t.Fatalf("the query is no longer answered by the companion-joined plan, so "+
					"this test no longer covers the group-existence dimension — re-arm it, "+
					"do not relax it.\n  query: %s\n  plan : %s", q, plan)
			}
			if !strings.Contains(plan, "live_groups_only") {
				t.Fatalf("the companion COUNT(*) leg does not carry the vacated-group drop.\n"+
					"  query: %s\n  plan : %s\nThe companion is itself subject to the "+
					"over-approximation defect: without the drop its own zero-valued keys "+
					"put every vacated group straight back into the merged group set.", q, plan)
			}
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
			if got := strings.Join(out, " "); got != want {
				t.Fatalf("index-backed answer diverges from the oracle.\n  query: %s\n"+
					"  got : %s\n  want: %s\n"+
					"g=1 (vacated by UPDATE) and g=3 (vacated by DELETE) must not appear. "+
					"g=5 (two live rows, every v NULL) must, with SUM NULL and COUNT(v) 0 — "+
					"the aggregate index holds no entry for it at all, so it can only come "+
					"from the companion. g=4 (+5 and -5) and g=6 (two zeros) are LIVE groups "+
					"answering 0 and must survive: a fix that drops them passes every other "+
					"test in this suite.", q, got, want)
			}
		})
	}

	// These two asserted the WRONG answers until RFC-209 §5.3(b) landed:
	// "[1 0] [2 60] [3 0] [4 0] [6 0]" for SUM and "[1 0] [2 3] [3 0] [4 2] [6 2]"
	// for COUNT(v) — both phantoms present, the all-NULL group missing.
	oracle("sum-agrees-with-the-oracle",
		"SELECT g, SUM(v) FROM ai GROUP BY g",
		"[2 60] [4 0] [5 NULL] [6 0]")
	oracle("count-col-agrees-with-the-oracle",
		"SELECT g, COUNT(v) FROM ai GROUP BY g",
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

// TestFDB_AggregateIndexGroupExistence_FailsClosedWithoutCompanion pins
// RFC-209 §5.3(c) as a PROPERTY rather than an assertion.
//
// A grouped SUM or COUNT(col) index whose group existence cannot be decided
// must not be used at all: the answers must still equal the oracle, and the
// plan must NOT contain the aggregate index. Correct rows via a slower
// streaming aggregation is the only acceptable outcome, because the
// alternative is the index-backed plan that is fast and wrong.
//
// HOW THE COMPANION IS TAKEN AWAY, and why it is not simply left undeclared.
// This binary's DDL emits a create-if-absent companion for every grouped SUM
// and COUNT(col), so a SQL-created schema ALWAYS has one and there is no DROP
// INDEX to remove it. A fixture that just omits the COUNT(*) omits the SUM
// index too and asserts "no aggregate index in the plan" about a query that
// never had one available — vacuous, and it passes with fail-closed deleted.
// So the companion is declared and then WITHDRAWN by moving it to WRITE_ONLY,
// which is what the planner's readable-index filter acts on. From the rule's
// side that is indistinguishable from absent: findGroupCountCompanion searches
// the candidate list, and a non-readable index never becomes a candidate.
//
// That also makes this RFC-209 §6's "a companion that is not readable must not
// be used" — the stronger of the two cases, because such an index has a
// PARTIAL key set and driving the merge from it would drop LIVE groups, a
// brand-new wrong answer strictly worse than today's phantom.
//
// The first subtest asserts the companion-joined plan WITH the companion
// readable. It is not decoration: it is what proves the fixture really does
// have an index-backed path to lose, and therefore what stops the fail-closed
// assertions below from going vacuous again.
func TestFDB_AggregateIndexGroupExistence_FailsClosedWithoutCompanion(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggnocomp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggnocomp")
	// ai_cnt_g is declared FIRST so create-if-absent finds it already
	// qualifying and emits no reserved-suffix companion of its own: the
	// group-existence source for both ai_sum_g and ai_cntv_g is then this one
	// named index, which is what makes withdrawing it withdraw the companion.
	// ai_cnt_h is grouped on a DIFFERENT column and stays readable throughout,
	// so fail-closed can be shown to be scoped rather than a blanket ban.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggnocomp "+
			"CREATE TABLE ai (pk BIGINT, g BIGINT, h BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE TABLE ao (pk BIGINT, g BIGINT, h BIGINT, v BIGINT, PRIMARY KEY (pk)) "+
			"CREATE INDEX ai_cnt_g AS SELECT COUNT(*) FROM ai GROUP BY g "+
			"CREATE INDEX ai_sum_g AS SELECT SUM(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cntv_g AS SELECT COUNT(v) FROM ai GROUP BY g "+
			"CREATE INDEX ai_cnt_h AS SELECT COUNT(*) FROM ai GROUP BY h")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggnocomp/s WITH TEMPLATE aggnocomp")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggnocomp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, tbl := range []string{"ai", "ao"} {
		mwjoMustExec(t, db, ctx, "INSERT INTO "+tbl+" (pk,g,h,v) VALUES "+
			"(1,1,1,10),(2,1,1,20),"+ // g=1 vacated by UPDATE
			"(3,2,1,30),"+
			"(4,3,1,40),(5,3,1,50),"+ // g=3 vacated by DELETE
			"(6,4,1,5),(7,4,1,-5),"+ // g=4 cancels to zero, LIVE
			"(8,5,1,NULL),(9,5,1,NULL)") // g=5 all-NULL, LIVE
		mwjoMustExec(t, db, ctx, "UPDATE "+tbl+" SET g = 2 WHERE g = 1")
		mwjoMustExec(t, db, ctx, "DELETE FROM "+tbl+" WHERE g = 3")
	}

	rowsOf := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var g int64
			var a sql.NullInt64
			if err := rows.Scan(&g, &a); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			if a.Valid {
				out = append(out, fmt.Sprintf("[%d %d]", g, a.Int64))
			} else {
				out = append(out, fmt.Sprintf("[%d NULL]", g))
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return out
	}

	// ARM 1 — companion READABLE. Both shapes must be companion-joined. This is
	// the assertion that gives the fail-closed arm below its teeth: it proves
	// the schema really does offer an index-backed path for exactly these two
	// queries, so "no AggregateIndex in the plan" afterwards means the planner
	// DECLINED one rather than never having had one.
	for _, tc := range []struct{ name, agg string }{
		{"sum", "SUM(v)"},
		{"count-col", "COUNT(v)"},
	} {
		tc := tc
		t.Run("companion-readable-joins/"+tc.name, func(t *testing.T) {
			q := "SELECT g, " + tc.agg + " FROM ai GROUP BY g"
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+q).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if !strings.Contains(plan, "GroupExistenceMerge(") {
				t.Fatalf("with a readable companion this query must be companion-joined, "+
					"or the fail-closed arm below is asserting the absence of something that "+
					"was never available and would pass with fail-closed deleted.\n"+
					"  query: %s\n  plan : %s", q, plan)
			}
			if !strings.Contains(plan, "AI_CNT_G") {
				t.Fatalf("the group-existence source is not the declared COUNT(*) index.\n"+
					"  query: %s\n  plan : %s\nai_cnt_g is declared before the SUM and "+
					"COUNT(col) indexes precisely so create-if-absent finds it qualifying and "+
					"emits no reserved-suffix companion; if a __GROUP_COUNT index appears here "+
					"instead, withdrawing AI_CNT_G no longer withdraws the companion and the "+
					"fail-closed arm goes vacuous.", q, plan)
			}
		})
	}

	// Withdraw the companion. WRITE_ONLY is what the planner's readable-index
	// filter acts on, and a non-readable index never becomes a match candidate
	// — so from findGroupCountCompanion's side this is exactly "absent".
	setIndexStateRaw(t, "/testdb_aggnocomp", "s", "AI_CNT_G", recordlayer.IndexStateWriteOnly)
	t.Cleanup(func() {
		setIndexStateRaw(t, "/testdb_aggnocomp", "s", "AI_CNT_G", recordlayer.IndexStateReadable)
	})

	// ARM 2 — companion withdrawn. Fail closed: no aggregate index, oracle rows.
	for _, tc := range []struct{ name, agg string }{
		{"sum", "SUM(v)"},
		{"count-col", "COUNT(v)"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			indexedQ := "SELECT g, " + tc.agg + " FROM ai GROUP BY g"
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+indexedQ).Scan(&plan); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			if strings.Contains(plan, "AggregateIndex(") {
				t.Fatalf("the aggregate index was used with NO group-existence companion.\n"+
					"  query: %s\n  plan : %s\n"+
					"Its key set can neither prove nor disprove that a group exists: a stored "+
					"zero is byte-identical whether the group cancelled to zero or was vacated, "+
					"and an all-NULL group has no entry at all. Using it here returns phantom "+
					"groups and drops live ones. RFC-209 §5.3(c) requires declining the "+
					"candidate so planning falls back to streaming aggregation.", indexedQ, plan)
			}
			got := rowsOf(t, indexedQ)
			want := rowsOf(t, "SELECT g, "+tc.agg+" FROM ao GROUP BY g")
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("fallback answer disagrees with the oracle\n  query: %s\n"+
					"  got   : %v\n  oracle: %v", indexedQ, got, want)
			}
		})
	}

	// The COUNT(*) index that exists is grouped on h, and a query grouped on h
	// is still index-backed: fail-closed must be scoped to the shape that cannot
	// be decided, not applied to every aggregate index in the schema.
	t.Run("count-star-on-its-own-grouping-still-uses-the-index", func(t *testing.T) {
		var plan string
		if err := db.QueryRowContext(ctx,
			"EXPLAIN SELECT h, COUNT(*) FROM ai GROUP BY h").Scan(&plan); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		if !strings.Contains(plan, "AggregateIndex(") {
			t.Fatalf("a COUNT(*) index over its OWN grouping key lost its acceleration.\n"+
				"  plan: %s\nIt is its own group-existence oracle and needs no companion; "+
				"declining it would make fail-closed a blanket ban rather than a scoped one.", plan)
		}
	})
}
