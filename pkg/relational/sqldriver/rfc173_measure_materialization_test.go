package sqldriver_test

// RFC-173 MATERIALIZATION ORDINALIZATION SENTINEL.
//
// The name-keyed Datum is GONE from the materialization read path:
// RecordLayerResultSet.columnValue reads SOLELY from the aligned positional output
// row and returns a loud (XX000) error if a row carries no aligned positional row.
// So driving each battery shape end-to-end — Scan-ing every column of every row —
// proves that shape's final output row is fully ordinal: any column that lacked an
// aligned positional slot would surface the correct-or-loud error here.
//
// This is the FINAL-OUTPUT companion of the FieldValue-level engine-read sweep. The
// battery spans the shapes whose two output-naming authorities historically
// disagreed and therefore stressed positionalAligned's ordinal-alignment path:
// scans, INNER/LEFT/3-way joins, CTE joins, unions, DISTINCT, ORDER BY, LIMIT,
// correlated scalars, EXISTS, ANONYMOUS computed projections (`SELECT UPPER(x)` —
// column "_i" vs slot "UPPER(X)"), aggregate columns (space-stripped ColumnDef vs
// spaced/aliased/qualified ExplainValue slot), multi-aggregate scalars, and the
// EMPTY-table scalar aggregate (emptyScalarResult births a positional row).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestFDB_RFC173_Measure_Materialization(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "matbattery",
		"CREATE TABLE orders (oid BIGINT NOT NULL, cust BIGINT, status STRING, amt BIGINT, PRIMARY KEY (oid)) "+
			"CREATE INDEX status_idx ON orders (status) "+
			"CREATE INDEX cust_idx ON orders (cust) "+
			"CREATE TABLE cust (cid BIGINT NOT NULL, name STRING, region STRING, PRIMARY KEY (cid)) "+
			"CREATE TABLE line (lid BIGINT NOT NULL, oid BIGINT, sku STRING, qty BIGINT, PRIMARY KEY (lid))")

	mwjoMustExec(t, db, ctx,
		"INSERT INTO cust (cid, name, region) VALUES (1,'alice','west'),(2,'bob','east'),(3,'carol','west')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO orders (oid, cust, status, amt) VALUES "+
			"(10,1,'shipped',100),(11,1,'pending',50),(12,2,'shipped',200),(13,3,'shipped',30),(14,2,'pending',70)")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO line (lid, oid, sku, qty) VALUES (100,10,'x',2),(101,10,'y',1),(102,12,'x',5),(103,14,'z',3)")

	// runQ drives the query and Scans EVERY column of EVERY row — the act of
	// reading each column is what exercises columnValue's positional read, so a
	// missing aligned positional row surfaces as a per-column XX000 error.
	runQ := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		n := len(cols)
		var out []string
		for rows.Next() {
			cells := make([]any, n)
			ptrs := make([]any, n)
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			parts := make([]string, n)
			for i, c := range cells {
				parts[i] = fmt.Sprintf("%v", c)
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.Strings(out)
		return out, nil
	}

	shapes := []struct{ name, sql string }{
		{"scan_filter_project", `SELECT oid, amt FROM orders WHERE amt > 60`},
		{"scan_star", `SELECT * FROM orders WHERE amt > 60`},
		{"index_equality", `SELECT oid FROM orders WHERE status = 'shipped'`},
		{"covering_index", `SELECT status FROM orders WHERE status = 'shipped'`},
		{"agg_count_star", `SELECT COUNT(*) FROM orders`},
		{"agg_group_by", `SELECT status, COUNT(*) FROM orders GROUP BY status`},
		{"agg_sum_group", `SELECT cust, SUM(amt) FROM orders GROUP BY cust`},
		{"agg_min_max", `SELECT status, MIN(amt), MAX(amt) FROM orders GROUP BY status`},
		{"agg_having", `SELECT cust, COUNT(*) FROM orders GROUP BY cust HAVING COUNT(*) > 1`},
		{"agg_no_group", `SELECT SUM(amt), AVG(amt) FROM orders`},
		{"inner_join", `SELECT c.name, o.amt FROM cust c, orders o WHERE c.cid = o.cust`},
		{"inner_join_star", `SELECT * FROM cust c, orders o WHERE c.cid = o.cust`},
		{"inner_join_3way", `SELECT c.name, l.sku FROM cust c, orders o, line l WHERE c.cid = o.cust AND o.oid = l.oid`},
		{"left_join", `SELECT c.name, o.amt FROM cust c LEFT JOIN orders o ON c.cid = o.cust`},
		{"agg_over_join", `SELECT c.region, SUM(o.amt) FROM cust c, orders o WHERE c.cid = o.cust GROUP BY c.region`},
		{"exists_corr", `SELECT name FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid)`},
		{"not_exists_corr", `SELECT name FROM cust c WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid)`},
		{"union_all", `SELECT status FROM orders UNION ALL SELECT region FROM cust`},
		{"distinct", `SELECT DISTINCT status FROM orders`},
		{"order_by", `SELECT oid FROM orders ORDER BY amt DESC`},
		{"limit", `SELECT oid FROM orders ORDER BY oid LIMIT 2`},
		{"cte", `WITH shipped AS (SELECT cust, amt FROM orders WHERE status = 'shipped') SELECT cust, SUM(amt) FROM shipped GROUP BY cust`},
		{"cte_join", `WITH w AS (SELECT cid, name FROM cust WHERE region = 'west') SELECT w.name, o.amt FROM w, orders o WHERE w.cid = o.cust`},
		{"scalar_subquery", `SELECT name FROM cust c WHERE c.cid = (SELECT MIN(cust) FROM orders)`},
		{"correlated_scalar", `SELECT c.name, (SELECT COUNT(*) FROM orders o WHERE o.cust = c.cid) FROM cust c`},
		{"exists_in_agg", `SELECT c.region, COUNT(*) FROM cust c WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid) GROUP BY c.region`},
		{"scalar_subq_in_select_bare_col", `SELECT oid, (SELECT COUNT(*) FROM line l WHERE l.oid = orders.oid) FROM orders`},
		// ANONYMOUS computed projections: result-set column "_i" vs positional slot
		// named by the expression rendering — aligned by ordinal (not a plain ref).
		{"anon_scalar_fn_upper", `SELECT UPPER(status) FROM orders`},
		{"anon_arith", `SELECT amt + 1 FROM orders`},
		{"anon_abs", `SELECT ABS(amt) FROM orders`},
		// AGGREGATE output columns whose ColumnDef spelling (space-stripped) and
		// positional-slot spelling (ExplainValue: spaced / aliased / qualified) differ.
		{"agg_computed_operand", `SELECT SUM(amt * 2) FROM orders`},
		{"agg_aliased", `SELECT SUM(amt) AS total FROM orders`},
		{"agg_multi_scalar", `SELECT COUNT(*), SUM(amt), MIN(amt), MAX(amt), AVG(amt) FROM orders`},
		{"agg_grouped_multi", `SELECT status, SUM(amt), COUNT(*) FROM orders GROUP BY status`},
		// EMPTY-table scalar aggregate: emptyScalarResult now births a positional row.
		{"agg_empty_scalar", `SELECT COUNT(*), SUM(amt) FROM orders WHERE amt > 100000`},
	}
	for _, sh := range shapes {
		rows, err := runQ(sh.sql)
		if err != nil {
			t.Errorf("shape %q errored — a final-output column had no aligned positional slot (materialization not ordinal): %v", sh.name, err)
			continue
		}
		t.Logf("[MATER %-24s] rows=%d", sh.name, len(rows))
	}
}
