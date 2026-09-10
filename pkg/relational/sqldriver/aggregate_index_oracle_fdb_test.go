package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestFDB_AggregateIndexOracle is the aggregate-index correctness battery
// over real FDB: COUNT(*), COUNT(col), SUM, MIN, MAX indexes over one and two
// grouping keys (a STRING key among them), read through equality, range, IN,
// IS NULL and input-column predicates, HAVING, ORDER BY the key or the
// aggregate with LIMIT, after deletes and updates that vacate and move
// groups. Every query is answered by a Go oracle over the same rows and
// compared as row-strings (sorted unless the query orders). The generative
// rowdiff corpus has no aggregate indexes in its DDL, so this is the rows
// net for that access path; the plan pins live in
// embedded/aggregate_index_residual_test.go.
func TestFDB_AggregateIndexOracle(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggoracle")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggoracle")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggoracle "+
			"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, v BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX idx_a ON t (a) "+
			"CREATE INDEX idx_ab ON t (a, b) "+
			"CREATE INDEX idx_c ON t (c) "+
			"CREATE INDEX cnt_by_a AS SELECT COUNT(*) FROM t GROUP BY a "+
			"CREATE INDEX cnt_v_by_a AS SELECT COUNT(v) FROM t GROUP BY a "+
			"CREATE INDEX sum_v_by_ab AS SELECT SUM(v) FROM t GROUP BY a, b "+
			"CREATE INDEX max_v_by_a AS SELECT MAX(v) FROM t GROUP BY a "+
			"CREATE INDEX min_v_by_ab AS SELECT MIN(v) FROM t GROUP BY a, b "+
			"CREATE INDEX sum_v_by_s AS SELECT SUM(v) FROM t GROUP BY s")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggoracle/s WITH TEMPLATE aggoracle")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggoracle?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rows := oracleGenRows(300)
	for i := 0; i < len(rows); i += 50 {
		end := i + 50
		if end > len(rows) {
			end = len(rows)
		}
		var parts []string
		for _, r := range rows[i:end] {
			parts = append(parts, r.insertSQL())
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO t (id, a, b, c, s, v) VALUES "+strings.Join(parts, ","))
	}
	// A few deletes and updates so the aggregate indexes have seen maintenance.
	mwjoMustExec(t, db, ctx, "DELETE FROM t WHERE id IN (3, 17, 99, 150, 288)")
	mwjoMustExec(t, db, ctx, "UPDATE t SET v = 7 WHERE id IN (5, 6, 7)")
	mwjoMustExec(t, db, ctx, "UPDATE t SET a = 2 WHERE id IN (8, 9)")
	mwjoMustExec(t, db, ctx, "UPDATE t SET v = NULL WHERE id IN (10, 11)")
	live := make([]oracleRow, 0, len(rows))
	for _, r := range rows {
		switch r.id {
		case 3, 17, 99, 150, 288:
			continue
		case 5, 6, 7:
			r.v = oracleInt(7)
		case 8, 9:
			r.a = oracleInt(2)
		case 10, 11:
			r.v = nil
		}
		live = append(live, r)
	}
	explain := mwjoExplainer(t, db, ctx)

	str := func(p *int64) string {
		if p == nil {
			return "NULL"
		}
		return fmt.Sprint(*p)
	}
	sstr := func(p *string) string {
		if p == nil {
			return "NULL"
		}
		return *p
	}
	type agg struct {
		count, countV, sum int64
		min, max           *int64
		hasSum             bool
	}
	groupBy := func(pred func(oracleRow) bool, key func(oracleRow) string) map[string]*agg {
		out := map[string]*agg{}
		for _, r := range live {
			if !pred(r) {
				continue
			}
			k := key(r)
			g := out[k]
			if g == nil {
				g = &agg{}
				out[k] = g
			}
			g.count++
			if r.v != nil {
				g.countV++
				g.sum += *r.v
				g.hasSum = true
				if g.min == nil || *r.v < *g.min {
					g.min = oracleInt(*r.v)
				}
				if g.max == nil || *r.v > *g.max {
					g.max = oracleInt(*r.v)
				}
			}
		}
		return out
	}
	all := func(oracleRow) bool { return true }
	sumStr := func(g *agg) string {
		if !g.hasSum {
			return "NULL"
		}
		return fmt.Sprint(g.sum)
	}

	type tc struct {
		name    string
		sql     string
		exp     func() []string // expected rows rendered as "c1|c2|..."
		ordered bool
	}
	cases := []tc{
		{"count_by_a", "SELECT a, COUNT(*) FROM t GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+fmt.Sprint(g.count))
			}
			return out
		}, false},
		{"countv_by_a", "SELECT a, COUNT(v) FROM t GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+fmt.Sprint(g.countV))
			}
			return out
		}, false},
		{"count_by_a_where_a_eq", "SELECT a, COUNT(*) FROM t WHERE a = 2 GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleEq(r.a, 2) }, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+fmt.Sprint(g.count))
			}
			return out
		}, false},
		{"count_by_a_where_a_gt", "SELECT a, COUNT(*) FROM t WHERE a > 2 GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleGt(r.a, 2) }, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+fmt.Sprint(g.count))
			}
			return out
		}, false},
		{"count_by_a_where_a_in", "SELECT a, COUNT(*) FROM t WHERE a IN (1, 3) GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleIn(r.a, 1, 3) }, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+fmt.Sprint(g.count))
			}
			return out
		}, false},
		{"count_by_a_where_a_null", "SELECT a, COUNT(*) FROM t WHERE a IS NULL GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return r.a == nil }, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+fmt.Sprint(g.count))
			}
			return out
		}, false},
		{"count_by_a_order_desc_limit", "SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY a DESC LIMIT 2", func() []string {
			var keys []string
			m := groupBy(all, func(r oracleRow) string { return str(r.a) })
			for k := range m {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				// DESC: NULL last; numeric desc
				if keys[i] == "NULL" {
					return false
				}
				if keys[j] == "NULL" {
					return true
				}
				var x, y int64
				fmt.Sscan(keys[i], &x)
				fmt.Sscan(keys[j], &y)
				return x > y
			})
			var out []string
			for _, k := range keys[:2] {
				out = append(out, k+"|"+fmt.Sprint(m[k].count))
			}
			return out
		}, true},
		{"count_by_a_having", "SELECT a, COUNT(*) FROM t GROUP BY a HAVING COUNT(*) > 50", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) }) {
				if g.count > 50 {
					out = append(out, k+"|"+fmt.Sprint(g.count))
				}
			}
			return out
		}, false},
		{"sum_by_ab", "SELECT a, b, SUM(v) FROM t GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_ab_where_a", "SELECT a, b, SUM(v) FROM t WHERE a = 1 GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_ab_where_b", "SELECT a, b, SUM(v) FROM t WHERE b = 3 GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleEq(r.b, 3) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_ab_where_a_b_range", "SELECT a, b, SUM(v) FROM t WHERE a = 1 AND b >= 2 AND b <= 4 GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleEq(r.a, 1) && oracleGe(r.b, 2) && oracleLe(r.b, 4) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_ab_where_v", "SELECT a, b, SUM(v) FROM t WHERE v > 20 GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleGt(r.v, 20) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_ab_where_c", "SELECT a, b, SUM(v) FROM t WHERE c = 2 GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleEq(r.c, 2) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_ab_having", "SELECT a, b, SUM(v) FROM t GROUP BY a, b HAVING SUM(v) > 100", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				if g.hasSum && g.sum > 100 {
					out = append(out, k+"|"+sumStr(g))
				}
			}
			return out
		}, false},
		{"sum_by_ab_order_sum_desc_limit", "SELECT a, b, SUM(v) FROM t GROUP BY a, b ORDER BY SUM(v) DESC, a, b LIMIT 3", func() []string {
			type kv struct {
				k string
				g *agg
			}
			var kvs []kv
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				kvs = append(kvs, kv{k, g})
			}
			sort.Slice(kvs, func(i, j int) bool {
				gi, gj := kvs[i].g, kvs[j].g
				if gi.hasSum != gj.hasSum {
					return !gi.hasSum == false && gj.hasSum == false // NULL last under DESC
				}
				if gi.sum != gj.sum {
					return gi.sum > gj.sum
				}
				return kvs[i].k < kvs[j].k
			})
			var out []string
			for _, e := range kvs[:3] {
				out = append(out, e.k+"|"+sumStr(e.g))
			}
			return out
		}, true},
		{"max_by_a", "SELECT a, MAX(v) FROM t GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+str(g.max))
			}
			return out
		}, false},
		{"max_by_a_having", "SELECT a, MAX(v) FROM t GROUP BY a HAVING MAX(v) < 50", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) }) {
				if g.max != nil && *g.max < 50 {
					out = append(out, k+"|"+str(g.max))
				}
			}
			return out
		}, false},
		{"max_by_a_where_v", "SELECT a, MAX(v) FROM t WHERE v < 40 GROUP BY a", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleLt(r.v, 40) }, func(r oracleRow) string { return str(r.a) }) {
				out = append(out, k+"|"+str(g.max))
			}
			return out
		}, false},
		{"min_by_ab", "SELECT a, b, MIN(v) FROM t GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+str(g.min))
			}
			return out
		}, false},
		{"min_by_ab_where_a_in", "SELECT a, b, MIN(v) FROM t WHERE a IN (1, 2) GROUP BY a, b", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return oracleIn(r.a, 1, 2) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) }) {
				out = append(out, k+"|"+str(g.min))
			}
			return out
		}, false},
		{"sum_by_s", "SELECT s, SUM(v) FROM t GROUP BY s", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return sstr(r.s) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"sum_by_s_where_s_gt", "SELECT s, SUM(v) FROM t WHERE s > 'x' GROUP BY s", func() []string {
			var out []string
			for k, g := range groupBy(func(r oracleRow) bool { return r.s != nil && *r.s > "x" }, func(r oracleRow) string { return sstr(r.s) }) {
				out = append(out, k+"|"+sumStr(g))
			}
			return out
		}, false},
		{"count_star_total", "SELECT COUNT(*) FROM t", func() []string {
			return []string{fmt.Sprint(len(live))}
		}, false},
		{"count_where_a", "SELECT COUNT(*) FROM t WHERE a = 2", func() []string {
			n := 0
			for _, r := range live {
				if oracleEq(r.a, 2) {
					n++
				}
			}
			return []string{fmt.Sprint(n)}
		}, false},
		{"sum_where_ab", "SELECT SUM(v) FROM t WHERE a = 2 AND b = 3", func() []string {
			g := groupBy(func(r oracleRow) bool { return oracleEq(r.a, 2) && oracleEq(r.b, 3) }, func(oracleRow) string { return "k" })["k"]
			if g == nil {
				return []string{"NULL"}
			}
			return []string{sumStr(g)}
		}, false},
		{"max_total", "SELECT MAX(v), MIN(v), COUNT(v), COUNT(*) FROM t", func() []string {
			g := groupBy(all, func(oracleRow) string { return "k" })["k"]
			return []string{str(g.max) + "|" + str(g.min) + "|" + fmt.Sprint(g.countV) + "|" + fmt.Sprint(g.count)}
		}, false},
		{"count_by_a_b_grouped_by_b_only", "SELECT b, COUNT(*) FROM t GROUP BY b", func() []string {
			var out []string
			for k, g := range groupBy(all, func(r oracleRow) string { return str(r.b) }) {
				out = append(out, k+"|"+fmt.Sprint(g.count))
			}
			return out
		}, false},
		{"count_distinct_a_via_group", "SELECT COUNT(*) FROM (SELECT a FROM t GROUP BY a) AS g", func() []string {
			return []string{fmt.Sprint(len(groupBy(all, func(r oracleRow) string { return str(r.a) })))}
		}, false},
		{"group_by_a_where_a_gt_order_a", "SELECT a, COUNT(*) FROM t WHERE a > 1 GROUP BY a ORDER BY a", func() []string {
			var keys []int64
			m := groupBy(func(r oracleRow) bool { return oracleGt(r.a, 1) }, func(r oracleRow) string { return str(r.a) })
			for k := range m {
				var x int64
				fmt.Sscan(k, &x)
				keys = append(keys, x)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			var out []string
			for _, k := range keys {
				out = append(out, fmt.Sprint(k)+"|"+fmt.Sprint(m[fmt.Sprint(k)].count))
			}
			return out
		}, true},
		{"sum_by_ab_where_a_order_b_desc", "SELECT a, b, SUM(v) FROM t WHERE a = 1 GROUP BY a, b ORDER BY b DESC", func() []string {
			m := groupBy(func(r oracleRow) bool { return oracleEq(r.a, 1) }, func(r oracleRow) string { return str(r.a) + "|" + str(r.b) })
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				bi := strings.Split(keys[i], "|")[1]
				bj := strings.Split(keys[j], "|")[1]
				if bi == "NULL" {
					return false
				}
				if bj == "NULL" {
					return true
				}
				var x, y int64
				fmt.Sscan(bi, &x)
				fmt.Sscan(bj, &y)
				return x > y
			})
			var out []string
			for _, k := range keys {
				out = append(out, k+"|"+sumStr(m[k]))
			}
			return out
		}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(c.sql)
			rs, err := db.QueryContext(ctx, c.sql)
			if err != nil {
				t.Fatalf("query %q: %v", c.sql, err)
			}
			defer rs.Close()
			cols, _ := rs.Columns()
			var got []string
			for rs.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rs.Scan(ptrs...); err != nil {
					t.Fatalf("scan: %v", err)
				}
				parts := make([]string, len(vals))
				for i, v := range vals {
					switch x := v.(type) {
					case nil:
						parts[i] = "NULL"
					case []byte:
						parts[i] = string(x)
					default:
						parts[i] = fmt.Sprint(x)
					}
				}
				got = append(got, strings.Join(parts, "|"))
			}
			if err := rs.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			exp := c.exp()
			g2, e2 := append([]string(nil), got...), append([]string(nil), exp...)
			if !c.ordered {
				sort.Strings(g2)
				sort.Strings(e2)
			}
			if strings.Join(g2, "\n") != strings.Join(e2, "\n") {
				t.Errorf("mismatch\n sql: %s\n plan: %s\n got: %v\n exp: %v", c.sql, plan, g2, e2)
				return
			}
			t.Logf("ok (%d rows) plan: %s", len(got), plan)
		})
	}
}
