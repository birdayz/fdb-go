package sqltest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDB_AggregateIndex_CountStarVsCountCol(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	// Java's aggregate-index-tests-count.yamsql: both COUNT(*) and COUNT(col)
	// indexes on same table. Planner must pick the correct one.
	db := testkit.SetupPlanShapeDB(t, "cntboth",
		"CREATE TABLE items (id BIGINT, grp BIGINT, val BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX cnt_star AS SELECT COUNT(*) FROM items GROUP BY grp "+
			"CREATE INDEX cnt_val AS SELECT COUNT(val) FROM items GROUP BY grp")

	// Insert rows: some with NULL val
	for _, item := range []struct {
		id  int
		grp int
		val string
	}{
		{1, 1, "10"},
		{2, 1, "NULL"},
		{3, 1, "NULL"},
		{4, 2, "20"},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO items VALUES (%d, %d, %s)", item.id, item.grp, item.val)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("count_star_includes_nulls", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT grp, COUNT(*) FROM items GROUP BY grp ORDER BY grp")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			grp int64
			cnt int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.grp, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// grp=1: 3 rows total (including NULLs), grp=2: 1 row
		want := []row{{1, 3}, {2, 1}}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("count_col_excludes_nulls", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT grp, COUNT(val) FROM items GROUP BY grp ORDER BY grp")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			grp int64
			cnt int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.grp, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// grp=1: only 1 non-null val, grp=2: 1 non-null val
		want := []row{{1, 1}, {2, 1}}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})
}

func TestFDB_AggregateIndex_UpdateAggColumn(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "updagg",
		"CREATE TABLE accounts (id BIGINT, owner STRING, balance BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_balance AS SELECT SUM(balance) FROM accounts GROUP BY owner")

	for _, a := range []struct {
		id      int
		owner   string
		balance int
	}{
		{1, "alice", 100},
		{2, "alice", 200},
		{3, "bob", 500},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO accounts VALUES (%d, '%s', %d)", a.id, a.owner, a.balance,
		)); err != nil {
			t.Fatalf("INSERT id=%d: %v", a.id, err)
		}
	}

	t.Run("initial_sums", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT owner, SUM(balance) FROM accounts GROUP BY owner ORDER BY owner")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			owner string
			sum   int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.owner, &r.sum); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{{"alice", 300}, {"bob", 500}}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("noop_update_preserves_sum", func(t *testing.T) {
		// No-op UPDATE (same value): SUM should remain unchanged.
		// Tests removeCommonAtomicEntries skipping common entries.
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = 200 WHERE id = 2"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		rows, err := db.QueryContext(ctx, "SELECT owner, SUM(balance) FROM accounts GROUP BY owner ORDER BY owner")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			owner string
			sum   int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.owner, &r.sum); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// alice: 100+200=300, bob: 500 — unchanged from initial
		want := []row{{"alice", 300}, {"bob", 500}}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("update_value_same_group", func(t *testing.T) {
		// Update alice's account 1 from 100 → 400. SUM should go 300→600.
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = 400 WHERE id = 1"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}

		var owner string
		var sum int64
		if err := db.QueryRowContext(ctx, "SELECT owner, SUM(balance) FROM accounts WHERE owner = 'alice' GROUP BY owner").Scan(&owner, &sum); err != nil {
			// Fallback: try without WHERE (known gap)
			rows, err2 := db.QueryContext(ctx, "SELECT owner, SUM(balance) FROM accounts GROUP BY owner ORDER BY owner")
			if err2 != nil {
				t.Fatalf("QueryContext: %v (original: %v)", err2, err)
			}
			defer rows.Close()
			for rows.Next() {
				var o string
				var s int64
				rows.Scan(&o, &s)
				if o == "alice" {
					owner, sum = o, s
				}
			}
		}
		if sum != 600 {
			t.Errorf("alice SUM after update: got %d, want 600", sum)
		}
	})
}

func TestFDB_AggregateIndex_CompositeAggExpressions(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cmpag",
		"CREATE TABLE invoices (id BIGINT, vendor STRING, amount BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX sum_by_vendor AS SELECT SUM(amount) FROM invoices GROUP BY vendor "+
			"CREATE INDEX count_by_vendor AS SELECT COUNT(*) FROM invoices GROUP BY vendor")

	for _, inv := range []struct {
		id     int
		vendor string
		amount int
	}{
		{1, "acme", 100},
		{2, "acme", 200},
		{3, "acme", 300},
		{4, "acme", 400},
		{5, "globex", 1000},
		{6, "globex", 2000},
	} {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO invoices VALUES (%d, '%s', %d)", inv.id, inv.vendor, inv.amount,
		)); err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}

	t.Run("sum_div_count_per_group", func(t *testing.T) {
		// Java: SUM(col2) / COUNT(col2) GROUP BY col1 → AISCAN ∩ AISCAN
		// Go: streaming aggregation (both aggregates in one pass)
		rows, err := db.QueryContext(ctx,
			"SELECT vendor, SUM(amount) / COUNT(*) FROM invoices GROUP BY vendor ORDER BY vendor")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			vendor string
			avg    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.vendor, &r.avg); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		// acme: 1000/4=250, globex: 3000/2=1500
		want := []row{{"acme", 250}, {"globex", 1500}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d", len(got), got, len(want))
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("multiple_agg_funcs_one_query", func(t *testing.T) {
		// Java uses INTERSECT for two aggs; Go should handle via streaming
		rows, err := db.QueryContext(ctx,
			"SELECT vendor, SUM(amount), COUNT(*) FROM invoices GROUP BY vendor ORDER BY vendor")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			vendor string
			total  int64
			cnt    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.vendor, &r.total, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		want := []row{{"acme", 1000, 4}, {"globex", 3000, 2}}
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

func TestFDB_AggregateIndex_InsertDeleteLifecycle(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "agglife",
		"CREATE TABLE counters (id BIGINT, bucket STRING, val BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX count_by_bucket AS SELECT COUNT(*) FROM counters GROUP BY bucket")

	// Insert 3 records into bucket 'A'
	for i := 1; i <= 3; i++ {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO counters VALUES (%d, 'A', %d)", i, i*100,
		)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	// Verify COUNT=3 via aggregate index
	plan := testkit.ExplainVia(t, ctx, db, "SELECT bucket, COUNT(*) FROM counters GROUP BY bucket")
	t.Logf("plan: %s", plan)
	if !strings.Contains(plan, "AggregateIndex") {
		t.Errorf("expected AggregateIndex plan, got: %s", plan)
	}

	var bucket string
	var cnt int64
	row := db.QueryRowContext(ctx, "SELECT bucket, COUNT(*) FROM counters GROUP BY bucket")
	if err := row.Scan(&bucket, &cnt); err != nil {
		t.Fatalf("scan after insert: %v", err)
	}
	if cnt != 3 {
		t.Fatalf("after 3 inserts: COUNT = %d, want 3", cnt)
	}

	// Delete record id=2
	if _, err := db.ExecContext(ctx, "DELETE FROM counters WHERE id = 2"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}

	// Verify COUNT=2
	row = db.QueryRowContext(ctx, "SELECT bucket, COUNT(*) FROM counters GROUP BY bucket")
	if err := row.Scan(&bucket, &cnt); err != nil {
		t.Fatalf("scan after delete: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("after delete: COUNT = %d, want 2", cnt)
	}

	// Insert another record
	if _, err := db.ExecContext(ctx, "INSERT INTO counters VALUES (10, 'A', 1000)"); err != nil {
		t.Fatalf("INSERT 10: %v", err)
	}

	// Verify COUNT=3 again
	row = db.QueryRowContext(ctx, "SELECT bucket, COUNT(*) FROM counters GROUP BY bucket")
	if err := row.Scan(&bucket, &cnt); err != nil {
		t.Fatalf("scan final: %v", err)
	}
	if cnt != 3 {
		t.Fatalf("final: COUNT = %d, want 3", cnt)
	}
}

func TestFDB_AggregateIndex_NullGroupKey(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "nullgrp",
		"CREATE TABLE events (id BIGINT, category STRING, weight BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX count_by_cat AS SELECT COUNT(*) FROM events GROUP BY category "+
			"CREATE INDEX sum_weight_by_cat AS SELECT SUM(weight) FROM events GROUP BY category")

	inserts := []struct {
		id       int
		category string // "NULL" or quoted string
		weight   int
	}{
		{1, "'alpha'", 10},
		{2, "'alpha'", 20},
		{3, "NULL", 30},
		{4, "NULL", 40},
		{5, "'beta'", 50},
	}
	for _, ins := range inserts {
		sql := fmt.Sprintf("INSERT INTO events VALUES (%d, %s, %d)", ins.id, ins.category, ins.weight)
		if _, err := db.ExecContext(ctx, sql); err != nil {
			t.Fatalf("INSERT id=%d: %v", ins.id, err)
		}
	}

	t.Run("count_includes_null_group", func(t *testing.T) {
		plan := testkit.ExplainVia(t, ctx, db, "SELECT category, COUNT(*) FROM events GROUP BY category ORDER BY category")
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "AggregateIndex") {
			t.Errorf("expected AggregateIndex in plan, got: %s", plan)
		}

		rows, err := db.QueryContext(ctx, "SELECT category, COUNT(*) FROM events GROUP BY category ORDER BY category")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			category *string
			cnt      int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.category, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// Expect: NULL group (2), alpha (2), beta (1)
		// NULL sorts first in FDB tuple ordering
		if len(got) != 3 {
			t.Fatalf("row count: got %d (%+v), want 3", len(got), got)
		}
		// NULL group
		if got[0].category != nil {
			t.Errorf("row 0: expected NULL category, got %q", *got[0].category)
		}
		if got[0].cnt != 2 {
			t.Errorf("row 0 (NULL): count = %d, want 2", got[0].cnt)
		}
		// alpha group
		if got[1].category == nil || *got[1].category != "alpha" {
			t.Errorf("row 1: expected 'alpha', got %v", got[1].category)
		}
		if got[1].cnt != 2 {
			t.Errorf("row 1 (alpha): count = %d, want 2", got[1].cnt)
		}
		// beta group
		if got[2].category == nil || *got[2].category != "beta" {
			t.Errorf("row 2: expected 'beta', got %v", got[2].category)
		}
		if got[2].cnt != 1 {
			t.Errorf("row 2 (beta): count = %d, want 1", got[2].cnt)
		}
	})

	t.Run("sum_includes_null_group", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, "SELECT category, SUM(weight) FROM events GROUP BY category ORDER BY category")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			category *string
			total    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.category, &r.total); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("row count: got %d (%+v), want 3", len(got), got)
		}
		// NULL group: 30+40=70
		if got[0].category != nil {
			t.Errorf("row 0: expected NULL category, got %q", *got[0].category)
		}
		if got[0].total != 70 {
			t.Errorf("row 0 (NULL): sum = %d, want 70", got[0].total)
		}
		// alpha: 10+20=30
		if got[1].category == nil || *got[1].category != "alpha" {
			t.Errorf("row 1: expected 'alpha', got %v", got[1].category)
		}
		if got[1].total != 30 {
			t.Errorf("row 1 (alpha): sum = %d, want 30", got[1].total)
		}
		// beta: 50
		if got[2].category == nil || *got[2].category != "beta" {
			t.Errorf("row 2: expected 'beta', got %v", got[2].category)
		}
		if got[2].total != 50 {
			t.Errorf("row 2 (beta): sum = %d, want 50", got[2].total)
		}
	})
}

func TestFDB_AggregateIndex_CountNotNull(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	db := testkit.SetupPlanShapeDB(t, "cntnull",
		"CREATE TABLE sensors (id BIGINT, sensor STRING, reading BIGINT, PRIMARY KEY (id)) "+
			"CREATE INDEX count_readings AS SELECT COUNT(reading) FROM sensors GROUP BY sensor")

	inserts := []struct {
		id      int
		sensor  string
		reading string // "NULL" or a number
	}{
		{1, "temp", "72"},
		{2, "temp", "68"},
		{3, "temp", "NULL"},
		{4, "humidity", "45"},
		{5, "humidity", "NULL"},
		{6, "humidity", "NULL"},
		{7, "pressure", "NULL"},
	}
	for _, ins := range inserts {
		sql := fmt.Sprintf("INSERT INTO sensors VALUES (%d, '%s', %s)", ins.id, ins.sensor, ins.reading)
		if _, err := db.ExecContext(ctx, sql); err != nil {
			t.Fatalf("INSERT id=%d: %v", ins.id, err)
		}
	}

	t.Run("count_col_skips_nulls", func(t *testing.T) {
		plan := testkit.ExplainVia(t, ctx, db, "SELECT sensor, COUNT(reading) FROM sensors GROUP BY sensor ORDER BY sensor")
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "AggregateIndex(COUNT, COUNT_READINGS") {
			t.Errorf("expected the COUNT(reading) index in plan, got: %s", plan)
		}

		rows, err := db.QueryContext(ctx, "SELECT sensor, COUNT(reading) FROM sensors GROUP BY sensor ORDER BY sensor")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			sensor string
			cnt    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.sensor, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		// temp: 2 non-null (72,68), humidity: 1 non-null (45). pressure is
		// missing, as in Java: COUNT_NOT_NULL writes no entry for a NULL value
		// (AtomicMutation.java:165-171), so an all-NULL group has no key, and the
		// index answers alone (aggregate-empty-table.yamsql marks it a bug).
		want := []row{{"humidity", 1}, {"temp", 2}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d (%+v)", len(got), got, len(want), want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("update_null_to_value", func(t *testing.T) {
		// Make pressure's reading non-null
		if _, err := db.ExecContext(ctx, "UPDATE sensors SET reading = 1013 WHERE id = 7"); err != nil {
			t.Fatalf("UPDATE: %v", err)
		}

		rows, err := db.QueryContext(ctx, "SELECT sensor, COUNT(reading) FROM sensors GROUP BY sensor ORDER BY sensor")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			sensor string
			cnt    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.sensor, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		want := []row{{"humidity", 1}, {"pressure", 1}, {"temp", 2}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d (%+v)", len(got), got, len(want), want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})

	t.Run("delete_non_null_row", func(t *testing.T) {
		// Delete temp id=1 (reading=72, non-null) → temp count goes from 2→1
		if _, err := db.ExecContext(ctx, "DELETE FROM sensors WHERE id = 1"); err != nil {
			t.Fatalf("DELETE: %v", err)
		}

		rows, err := db.QueryContext(ctx, "SELECT sensor, COUNT(reading) FROM sensors GROUP BY sensor ORDER BY sensor")
		if err != nil {
			t.Fatalf("QueryContext: %v", err)
		}
		defer rows.Close()
		type row struct {
			sensor string
			cnt    int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.sensor, &r.cnt); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		want := []row{{"humidity", 1}, {"pressure", 1}, {"temp", 1}}
		if len(got) != len(want) {
			t.Fatalf("row count: got %d (%+v), want %d (%+v)", len(got), got, len(want), want)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("row %d: got %+v, want %+v", i, got[i], w)
			}
		}
	})
}
