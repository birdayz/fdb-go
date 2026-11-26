package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/internal/relational/sqltest/testkit"
)

func TestFDB_AggregateIndex_UngroupedAndEmpty(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "ungrp",
		"CREATE TABLE counters (id BIGINT, val BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX total_count AS SELECT COUNT(*) FROM counters "+
			"CREATE INDEX total_sum AS SELECT SUM(val) FROM counters "+
			"CREATE INDEX count_val AS SELECT COUNT(val) FROM counters")

	t.Run("empty_table_count_star", func(t *testing.T) {
		// Java >= 4.0.561: ungrouped COUNT(*) on empty table returns [{0}]
		var cnt int64
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM counters").Scan(&cnt)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if cnt != 0 {
			t.Errorf("COUNT(*) on empty table: got %d, want 0", cnt)
		}
	})

	t.Run("empty_table_sum", func(t *testing.T) {
		// SUM on empty table: SQL standard says NULL
		var sum *int64
		err := db.QueryRowContext(ctx, "SELECT SUM(val) FROM counters").Scan(&sum)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if sum != nil {
			t.Errorf("SUM on empty table: got %d, want NULL", *sum)
		}
	})

	// Insert data
	for _, ins := range []struct {
		id  int
		val string
	}{
		{1, "10"},
		{2, "20"},
		{3, "30"},
		{4, "NULL"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO counters VALUES (%d, %s)", ins.id, ins.val)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("ungrouped_count_star", func(t *testing.T) {
		var cnt int64
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM counters").Scan(&cnt)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if cnt != 4 {
			t.Errorf("COUNT(*): got %d, want 4", cnt)
		}
	})

	t.Run("ungrouped_sum", func(t *testing.T) {
		var total int64
		err := db.QueryRowContext(ctx, "SELECT SUM(val) FROM counters").Scan(&total)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		if total != 60 {
			t.Errorf("SUM(val): got %d, want 60", total)
		}
	})

	t.Run("ungrouped_count_col", func(t *testing.T) {
		var cnt int64
		err := db.QueryRowContext(ctx, "SELECT COUNT(val) FROM counters").Scan(&cnt)
		if err != nil {
			t.Fatalf("QueryRow: %v", err)
		}
		// 3 non-null values
		if cnt != 3 {
			t.Errorf("COUNT(val): got %d, want 3", cnt)
		}
	})
}

func TestFDB_AggregateIndex_Having(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "aghav",
		"CREATE TABLE sales (id BIGINT, region STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_by_region AS SELECT SUM(amount) FROM sales GROUP BY region "+
			"CREATE INDEX count_by_region AS SELECT COUNT(*) FROM sales GROUP BY region")

	for _, s := range []struct {
		id     int
		region string
		amount int
	}{
		{1, "US", 100},
		{2, "US", 200},
		{3, "US", 300},
		{4, "EU", 50},
		{5, "EU", 75},
		{6, "APAC", 1000},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO sales VALUES (%d, '%s', %d)", s.id, s.region, s.amount,
		)); err != nil {
			t.Fatalf("INSERT id=%d: %v", s.id, err)
		}
	}

	t.Run("having_count_gt", func(t *testing.T) {
		rows, err := db.QueryContext(ctx,
			"SELECT region, COUNT(*) FROM sales GROUP BY region HAVING COUNT(*) > 2 ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			cnt    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// Only US has count > 2
		want := []row{{"US", 3}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("having_sum_gt", func(t *testing.T) {
		plan := testkit.ExplainVia(t, ctx, db, "SELECT region, SUM(amount) FROM sales GROUP BY region HAVING SUM(amount) > 200 ORDER BY region")
		t.Logf("plan: %s", plan)

		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales GROUP BY region HAVING SUM(amount) > 200 ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// APAC=1000, US=600 both > 200. EU=125 excluded.
		want := []row{{"APAC", 1000}, {"US", 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("select_only_aggregate_no_group_col", func(t *testing.T) {
		// Java: SELECT sum(col2) FROM t1 GROUP BY col1 → [{15}, {76}]
		// Tests that the aggregate value is returned without the group key
		rows, err := db.QueryContext(ctx,
			"SELECT SUM(amount) FROM sales GROUP BY region ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var total int64
			if err := rows.Scan(&total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, total)
		}
		// ORDER BY region: APAC=1000, EU=125, US=600
		want := []int64{1000, 125, 600}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %d, want %d", i, got[i], w)
			}
		}
	})

	t.Run("arithmetic_on_aggregate", func(t *testing.T) {
		// Java tests: sum(col2) + 1 as post-aggregate arithmetic
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) + 1 FROM sales GROUP BY region ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// APAC=1000+1=1001, EU=125+1=126, US=600+1=601
		want := []row{{"APAC", 1001}, {"EU", 126}, {"US", 601}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("order_by_aggregate_desc", func(t *testing.T) {
		// ORDER BY SUM(amount) DESC — sorts aggregate output
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales GROUP BY region ORDER BY SUM(amount) DESC")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// APAC=1000, US=600, EU=125. DESC order: APAC, US, EU
		want := []row{{"APAC", 1000}, {"US", 600}, {"EU", 125}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("group_by_with_limit", func(t *testing.T) {
		// Go extension: LIMIT with GROUP BY
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales GROUP BY region ORDER BY region LIMIT 2")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// ORDER BY region: APAC, EU, US. LIMIT 2 → only APAC, EU.
		want := []row{{"APAC", 1000}, {"EU", 125}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("having_multiple_conditions", func(t *testing.T) {
		// Multiple HAVING conditions + multiple aggregates
		rows, err := db.QueryContext(ctx,
			"SELECT region, COUNT(*), SUM(amount) FROM sales GROUP BY region HAVING COUNT(*) >= 2 AND SUM(amount) > 200 ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			cnt    int64
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.cnt, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// EU: cnt=2, sum=125. Count>=2 but sum<=200 → excluded.
		// US: cnt=3, sum=600. Both conditions met.
		// APAC: cnt=1, sum=1000. Count<2 → excluded.
		want := []row{{"US", 3, 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("mismatched_agg_func_fallback", func(t *testing.T) {
		// Schema has SUM+COUNT indexes for region, but query asks for MIN.
		// Must NOT use aggregate index — falls back to streaming agg.
		rows, err := db.QueryContext(ctx,
			"SELECT region, MIN(amount) FROM sales GROUP BY region ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			min    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.min); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// APAC min=1000, EU min=50, US min=100
		want := []row{{"APAC", 1000}, {"EU", 50}, {"US", 100}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("group_by_order_group_col_desc", func(t *testing.T) {
		// Java: ORDER BY col1 DESC uses AISCAN in reverse
		// Go: falls back to full scan + streaming agg (optimization gap: aggregate
		// index doesn't satisfy DESC ordering constraint). Result is correct.
		rows, err := db.QueryContext(ctx,
			"SELECT region, COUNT(*) FROM sales GROUP BY region ORDER BY region DESC")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			cnt    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// DESC: US(3), EU(2), APAC(1)
		want := []row{{"US", 3}, {"EU", 2}, {"APAC", 1}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("arithmetic_across_aggregates", func(t *testing.T) {
		// Java pattern: SELECT COUNT(*) + SUM(amount) — arithmetic over two aggregates
		rows, err := db.QueryContext(ctx,
			"SELECT region, COUNT(*) + SUM(amount) FROM sales GROUP BY region ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			val    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.val); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// APAC: 1+1000=1001, EU: 2+125=127, US: 3+600=603
		want := []row{{"APAC", 1001}, {"EU", 127}, {"US", 603}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("having_agg_and_group_key_combined", func(t *testing.T) {
		// Java: HAVING min(id) > 0 AND col1 = 20 — aggregate AND group key
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales GROUP BY region HAVING SUM(amount) > 100 AND region = 'US'")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// Only US matches both: SUM=600>100 AND region='US'
		want := []row{{"US", 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("having_on_different_agg_than_select", func(t *testing.T) {
		// SELECT SUM but HAVING on COUNT — different agg in filter vs output
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales GROUP BY region HAVING COUNT(*) >= 2 ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// EU cnt=2, US cnt=3 → both pass HAVING. APAC cnt=1 excluded.
		want := []row{{"EU", 125}, {"US", 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("having_or_condition", func(t *testing.T) {
		// HAVING with OR: either high count OR high sum
		rows, err := db.QueryContext(ctx,
			"SELECT region, COUNT(*), SUM(amount) FROM sales GROUP BY region HAVING COUNT(*) >= 3 OR SUM(amount) >= 1000 ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			cnt    int64
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.cnt, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// APAC: cnt=1, sum=1000 → sum>=1000 matches
		// EU: cnt=2, sum=125 → neither matches
		// US: cnt=3, sum=600 → cnt>=3 matches
		want := []row{{"APAC", 1, 1000}, {"US", 3, 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("where_on_group_key", func(t *testing.T) {
		// Correctness test: WHERE on group key works via full scan + filter + streaming agg.
		// Bounded aggregate scan (AISCAN [EQUALS]) is a future optimization (see TODO.md).
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales WHERE region = 'US' GROUP BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{{"US", 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("having_with_where", func(t *testing.T) {
		// WHERE + HAVING combined: filter rows first, then groups
		rows, err := db.QueryContext(ctx,
			"SELECT region, SUM(amount) FROM sales WHERE amount > 50 GROUP BY region HAVING SUM(amount) >= 500 ORDER BY region")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			region string
			total  int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.region, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// After WHERE amount>50: US(100,200,300)=600, EU(75)=75, APAC(1000)=1000
		// HAVING >=500: APAC=1000, US=600
		want := []row{{"APAC", 1000}, {"US", 600}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})
}

func TestFDB_AggregateIndex_MultiColumnGroupBy(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "mcgrp",
		"CREATE TABLE events (id BIGINT, cat STRING, sev STRING, dur BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX count_cat_sev AS SELECT COUNT(*) FROM events GROUP BY cat, sev")

	for _, e := range []struct {
		id  int
		cat string
		sev string
		dur int
	}{
		{1, "error", "high", 10},
		{2, "error", "high", 20},
		{3, "error", "low", 5},
		{4, "warn", "high", 3},
		{5, "warn", "low", 1},
		{6, "warn", "low", 2},
		{7, "info", "low", 1},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO events VALUES (%d, '%s', '%s', %d)", e.id, e.cat, e.sev, e.dur,
		)); err != nil {
			t.Fatalf("INSERT id=%d: %v", e.id, err)
		}
	}

	t.Run("multi_group_count", func(t *testing.T) {
		plan := testkit.ExplainVia(t, ctx, db, "SELECT cat, sev, COUNT(*) FROM events GROUP BY cat, sev ORDER BY cat, sev")
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "AggregateIndex") {
			t.Errorf("expected AggregateIndex in plan, got: %s", plan)
		}

		rows, err := db.QueryContext(ctx,
			"SELECT cat, sev, COUNT(*) FROM events GROUP BY cat, sev ORDER BY cat, sev")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			cat string
			sev string
			cnt int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.cat, &r.sev, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{
			{"error", "high", 2},
			{"error", "low", 1},
			{"info", "low", 1},
			{"warn", "high", 1},
			{"warn", "low", 2},
		}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})
}
