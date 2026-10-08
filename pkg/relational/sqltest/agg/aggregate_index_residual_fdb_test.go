package sqltest

// Rows behind TestAggregateIndexResidualOverGroupingColumns (embedded), RFC-248:
// a predicate on grouping columns the aggregate index's scan cannot bind is
// applied as a residual filter above the scan. The indexed schema's answer is
// compared with the unindexed schema's through DML that empties, revives and
// merges groups, for COUNT(*) (a plain scan) and SUM (the COUNT(*)-companion
// merge), over residual shapes that exercise the partition: a non-leading
// equality, a gap, a leading range (bound, no residual), a range then an
// equality, IS NULL, IN, a column-to-column residual, an OR, contradictory
// equalities, and ORDER BY through the residual. Every read is asserted, via
// the typed plan, to be served by the aggregate index — a read that fell back
// to a base scan would agree with the twin for a reason unrelated to this
// change.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDB_AggregateIndexResidual(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const table = "CREATE TABLE t (id BIGINT, a STRING, b STRING, c STRING, v BIGINT, d DOUBLE, PRIMARY KEY (id)) " +
		"CREATE TABLE cust (id BIGINT, b STRING, region STRING, PRIMARY KEY (id)) "
	const indexes = "CREATE INDEX t_sum_abc AS SELECT SUM(v) FROM t GROUP BY a, b, c " +
		"CREATE INDEX t_cntv_abc AS SELECT COUNT(v) FROM t GROUP BY a, b, c " +
		"CREATE INDEX t_cnt_abc AS SELECT COUNT(*) FROM t GROUP BY a, b, c " +
		"CREATE INDEX t_cnt_d_a AS SELECT COUNT(*) FROM t GROUP BY d, a "
	w := testkit.NewTwin(t, ctx, "/FRL/testdb_aggresidual", "aggresidual", table, indexes)

	as := []string{"'x'", "'y'", "'z'", "NULL"}
	bs := []string{"'p'", "'q'", "NULL"}
	cs := []string{"'x'", "'m'", "'z'"}
	ds := []string{"0.5", "1.5", "2.5", "NULL"}
	var rows []string
	for id := int64(0); id < 60; id++ {
		rows = append(rows, fmt.Sprintf("(%d, %s, %s, %s, %d, %s)", id,
			as[id%4], bs[(id/2)%3], cs[(id/3)%3], (id*7)%11-3, ds[(id/5)%4]))
	}
	w.Exec("INSERT INTO t (id, a, b, c, v, d) VALUES " + strings.Join(rows, ", "))
	w.Exec("INSERT INTO cust (id, b, region) VALUES (1, 'p', 'eu'), (2, 'q', 'us'), (3, NULL, 'us'), (4, 'zz', 'eu')")

	// wantResidual says which reads carry a residual filter above the aggregate
	// scan; the others bind everything they filter (a leading range, a leading
	// IS NULL). Asserted on the typed plan, never by matching SQL text.
	reads := []struct {
		sql          string
		wantResidual bool
	}{
		{"SELECT a, b, c, COUNT(*) FROM t WHERE b = 'p' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE b = 'p' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a > 'x' GROUP BY a, b, c ORDER BY a, b, c", false},
		{"SELECT a, b, c, SUM(v) FROM t WHERE a > 'x' GROUP BY a, b, c ORDER BY a, b, c", false},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a > 'x' AND b = 'q' GROUP BY a, b, c ORDER BY a, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a >= 'x' AND a < 'z' GROUP BY a, b, c ORDER BY a, b, c", false},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a IS NULL GROUP BY a, b, c ORDER BY b, c", false},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a IS NULL AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE b IS NULL GROUP BY a, b, c ORDER BY a, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE c IN ('m', 'z') GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = c GROUP BY a, b, c ORDER BY a, b", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE b = 'q' OR c = 'm' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND a = 'y' GROUP BY a, b, c", true},
		// An equality beside an inequality on the leading key: the equality
		// binds, the inequality is a residual over the group key.
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND a > 'm' GROUP BY a, b, c ORDER BY b, c", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE a > 'm' AND a = 'y' GROUP BY a, b, c ORDER BY b, c", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY a DESC, b", true},
		{"SELECT d, a, COUNT(*) FROM t WHERE d > 1.0 GROUP BY d, a ORDER BY d, a", true},
		{"SELECT a, b, c, COUNT(*) FROM t WHERE b = 'p' GROUP BY a, b, c HAVING COUNT(*) > 1 ORDER BY a, c", true},
		// A grouping-column leaf under a function wrapper stays wrapped after
		// the rewrite.
		{"SELECT a, b, c, COUNT(*) FROM t WHERE UPPER(b) = 'P' GROUP BY a, b, c ORDER BY a, c", true},
		{"SELECT a, b, c, SUM(v) FROM t WHERE LENGTH(c) = 1 AND a = 'y' GROUP BY a, b, c ORDER BY b, c", true},
		// Correlated: the outer row's b shares the grouping column's name and
		// must stay the outer read — rewritten by name it would compare the
		// group with itself and count every group for every customer.
		{"SELECT c.id, (SELECT COUNT(*) FROM t WHERE t.b = c.b AND t.a = 'x' AND t.c = 'z' GROUP BY t.a, t.b, t.c) FROM cust c ORDER BY c.id", true},
		{"SELECT c.id, (SELECT SUM(v) FROM t WHERE t.b = c.b AND t.a = 'y' AND t.c = 'm' GROUP BY t.a, t.b, t.c) FROM cust c ORDER BY c.id", true},
		// Two aggregates over two indexes: the residual sits above the
		// intersection (the third scan site).
		{"SELECT a, b, c, SUM(v), COUNT(*) FROM t WHERE b = 'p' GROUP BY a, b, c ORDER BY a, b, c", true},
		{"SELECT a, b, c, SUM(v), COUNT(*) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c ORDER BY b", true},
	}
	for _, r := range reads {
		plan, err := embedded.PlanPhysicalForTest(r.sql, table+indexes, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", r.sql, err)
		}
		if reached, _ := aggregateIndexAndSortIn(plan); !reached {
			t.Fatalf("read is not served by the aggregate index, so its rows prove nothing here\n  q: %s\n  plan: %s", r.sql, plan.Explain())
		}
		if residualFilterIn(plan) != r.wantResidual {
			t.Fatalf("residual filter above the aggregate scan = %v, want %v\n  q: %s\n  plan: %s", !r.wantResidual, r.wantResidual, r.sql, plan.Explain())
		}
	}
	sweep := func(stage string) {
		t.Helper()
		for _, r := range reads {
			q := r.sql
			gi, ei := testkit.QueryRowStrings(t, ctx, w.Idx, q)
			gn, en := testkit.QueryRowStrings(t, ctx, w.Plain, q)
			if ei != nil || en != nil {
				t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", stage, q, ei, en)
				continue
			}
			if !testkit.MmAggregateIndexRowsAgree(gi, gn, testkit.MmTrailingAggregates(q)) {
				t.Errorf("%s: the residual-filtered aggregate index disagrees with the unindexed twin\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
					stage, q, gi, gn, w.Explain(q))
			}
		}
	}
	sweep("initial")
	for i, stmt := range []string{
		"UPDATE t SET b = 'p' WHERE a = 'z'",                                    // moves groups into the residual's selection
		"DELETE FROM t WHERE a = 'x' AND c = 'z'",                               // empties the gap arm's groups
		"INSERT INTO t (id, a, b, c, v, d) VALUES (100, 'x', 'q', 'z', 5, 1.5)", // revives one
		"UPDATE t SET c = 'x' WHERE b IS NULL",                                  // makes a = c true for more groups
		"UPDATE t SET v = 0 WHERE a > 'x'",                                      // SUM to zero under the range arm
		"DELETE FROM t WHERE d > 1.0",                                           // empties the double-range arm entirely
	} {
		w.Exec(stmt)
		sweep(fmt.Sprintf("after dml %d (%s)", i, stmt))
	}
}

// residualFilterIn reports whether a PredicatesFilter sits directly above an
// aggregate index plan or the merge/intersection it feeds.
func residualFilterIn(plan plans.RecordQueryPlan) bool {
	found := false
	var walk func(p plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if f, ok := p.(*plans.RecordQueryPredicatesFilterPlan); ok && len(f.GetChildren()) == 1 {
			switch f.GetChildren()[0].(type) {
			case *plans.RecordQueryAggregateIndexPlan, *plans.RecordQueryMultiIntersectionOnValuesPlan:
				found = true
			}
		}
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
	return found
}

func TestFDB_BitmapAggregateIndex(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const table = `CREATE TABLE t (id BIGINT, category STRING, PRIMARY KEY(id)) `
	const indexes = `CREATE INDEX bm AS SELECT bitmap_construct_agg(bitmap_bit_position(id)), category, bitmap_bucket_offset(id) FROM t GROUP BY category, bitmap_bucket_offset(id)`
	w := testkit.NewTwin(t, ctx, "/FRL/testdb_bitmapagg", "bitmapagg", table, indexes)
	w.Exec("INSERT INTO t VALUES (1, 'a'), (2, 'a'), (10001, 'a'), (3, 'b')")
	queries := []string{
		`SELECT category, bitmap_bucket_offset(id), bitmap_construct_agg(bitmap_bit_position(id)) FROM t GROUP BY category, bitmap_bucket_offset(id) ORDER BY category, bitmap_bucket_offset(id)`,
		`SELECT category, bitmap_bucket_offset(id), bitmap_construct_agg(bitmap_bit_position(id)) FROM t WHERE category = 'a' GROUP BY category, bitmap_bucket_offset(id) ORDER BY category, bitmap_bucket_offset(id)`,
	}
	sweep := func() {
		t.Helper()
		for _, q := range queries {
			if plan := w.Explain(q); !strings.Contains(plan, "AggregateIndex") || strings.Contains(plan, "StreamingAgg") {
				t.Fatalf("not index-backed: %s", plan)
			}
			gi, ei := testkit.QueryRowStrings(t, ctx, w.Idx, q)
			gn, en := testkit.QueryRowStrings(t, ctx, w.Plain, q)
			if ei != nil || en != nil || !testkit.EqualRows(gi, gn) {
				t.Fatalf("bitmap index differs from streaming: %v/%v, errors %v/%v", gi, gn, ei, en)
			}
		}
	}
	sweep()
	for _, stmt := range []string{"DELETE FROM t WHERE id = 10001", "UPDATE t SET category = 'b' WHERE id = 2", "DELETE FROM t WHERE category = 'a'", "INSERT INTO t VALUES (1, 'a')"} {
		w.Exec(stmt)
		sweep()
	}
}

// TestFDB_AggregateIndexNestedLeafGrouping is the rows behind
// TestAggregateIndexNestedLeafGroupingColumns (embedded, RFC-257 WS-J 3.3b): a
// GROUP BY over nested LEAF fields served by the aggregate index grouping by
// them answers what the unindexed twin answers, through DML that rewrites
// structs, nulls them, empties and revives groups. A RECORD-typed grouping key
// over the same leaves stays off the index and agrees too.
func TestFDB_AggregateIndexNestedLeafGrouping(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	const table = "CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT) " +
		"CREATE TABLE t_s (id BIGINT, home ADDR, office ADDR, city STRING, cat STRING, v BIGINT, PRIMARY KEY (id)) "
	const indexes = "CREATE INDEX cnt_home_cat AS SELECT COUNT(*) FROM t_s GROUP BY home.city, home.zip, cat " +
		"CREATE INDEX sum_home_cat AS SELECT SUM(v) FROM t_s GROUP BY home.city, home.zip, cat " +
		"CREATE INDEX sum_home_cat_nn AS SELECT COUNT(v) FROM t_s GROUP BY home.city, home.zip, cat " +
		"CREATE INDEX cnt_home_office AS SELECT COUNT(*) FROM t_s GROUP BY home.city, office.city "
	w := testkit.NewTwin(t, ctx, "/FRL/testdb_aggnested", "aggnested", table, indexes)

	cities := []string{"'a'", "'b'", "NULL"}
	cats := []string{"'x'", "'y'"}
	var rows []string
	for id := int64(0); id < 36; id++ {
		home := fmt.Sprintf("(%s, %d)", cities[id%3], id%2+1)
		if id%11 == 5 {
			home = "NULL"
		}
		rows = append(rows, fmt.Sprintf("(%d, %s, (%s, %d), %s, %s, %d)", id, home,
			cities[(id/3)%3], id%3, cities[(id/2)%3], cats[(id/4)%2], (id*5)%7-2))
	}
	w.Exec("INSERT INTO t_s (id, home, office, city, cat, v) VALUES " + strings.Join(rows, ", "))

	served := []string{
		"SELECT home.city, home.zip, cat, COUNT(*) FROM t_s GROUP BY home.city, home.zip, cat ORDER BY home.city, home.zip, cat",
		"SELECT home.city, home.zip, cat, COUNT(*) FROM t_s WHERE cat = 'x' GROUP BY home.city, home.zip, cat ORDER BY home.city, home.zip",
		"SELECT home.city, home.zip, cat, SUM(v) FROM t_s WHERE home.city = 'a' GROUP BY home.city, home.zip, cat ORDER BY home.city, home.zip, cat",
		"SELECT home.city, home.zip, cat, COUNT(*) FROM t_s WHERE home.city = 'b' AND home.zip > 1 GROUP BY home.city, home.zip, cat",
		"SELECT home.city, home.zip, cat, SUM(v), COUNT(*) FROM t_s WHERE home.zip = 2 GROUP BY home.city, home.zip, cat ORDER BY home.city, home.zip, cat",
		"SELECT home.city, home.zip, cat, COUNT(*) FROM t_s WHERE home.city IS NULL GROUP BY home.city, home.zip, cat ORDER BY home.zip, cat",
		"SELECT home.city, office.city, COUNT(*) FROM t_s WHERE office.city = 'b' GROUP BY home.city, office.city ORDER BY home.city",
	}
	unserved := []string{
		// RECORD-typed key: Java plans nothing; Go aggregates the records.
		// (projected without the struct, which the row scanner cannot hold).
		"SELECT cat, COUNT(*) FROM t_s GROUP BY home, cat",
		// The top-level CITY is not HOME.CITY.
		"SELECT city, home.zip, cat, COUNT(*) FROM t_s GROUP BY city, home.zip, cat ORDER BY city, home.zip, cat",
	}
	for _, q := range served {
		plan, err := embedded.PlanPhysicalForTest(q, table+indexes, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", q, err)
		}
		if reached, sorted := aggregateIndexAndSortIn(plan); !reached || sorted {
			t.Fatalf("read is not served by the aggregate index without a sort (reached=%v sorted=%v)\n  q: %s\n  plan: %s",
				reached, sorted, q, plan.Explain())
		}
	}
	for _, q := range unserved {
		plan, err := embedded.PlanPhysicalForTest(q, table+indexes, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", q, err)
		}
		if reached, _ := aggregateIndexAndSortIn(plan); reached {
			t.Fatalf("read is served by an aggregate index over other columns\n  q: %s\n  plan: %s", q, plan.Explain())
		}
	}
	sweep := func(stage string) {
		t.Helper()
		for _, q := range append(append([]string(nil), served...), unserved...) {
			gi, ei := testkit.QueryRowStrings(t, ctx, w.Idx, q)
			gn, en := testkit.QueryRowStrings(t, ctx, w.Plain, q)
			if ei != nil || en != nil {
				t.Errorf("%s: query failed\n  q: %s\n  indexed:   %v\n  unindexed: %v", stage, q, ei, en)
				continue
			}
			if len(gn) == 0 {
				t.Errorf("%s: the unindexed twin answers no rows, so agreement proves nothing\n  q: %s", stage, q)
			}
			if !testkit.MmAggregateIndexRowsAgree(gi, gn, testkit.MmTrailingAggregates(q)) {
				t.Errorf("%s: the nested-leaf aggregate index disagrees with the unindexed twin\n  q: %s\n  indexed  : %v\n  unindexed: %v\n  plan: %s",
					stage, q, gi, gn, w.Explain(q))
			}
		}
	}
	sweep("initial")
	for i, stmt := range []string{
		"UPDATE t_s SET home = ('b', 2) WHERE id < 6",           // moves rows between nested groups
		"DELETE FROM t_s WHERE cat = 'y' AND id > 30",           // empties groups
		"UPDATE t_s SET office = ('b', 9) WHERE office IS NULL", // the second nested parent
		"INSERT INTO t_s (id, home, office, city, cat, v) VALUES (100, ('a', 2), ('b', 1), 'a', 'x', 4), (101, NULL, NULL, NULL, 'y', 1)",
		"UPDATE t_s SET v = 0 WHERE home.city = 'a'", // SUM to zero in live groups
	} {
		w.Exec(stmt)
		sweep(fmt.Sprintf("after dml %d (%s)", i, stmt))
	}
}
