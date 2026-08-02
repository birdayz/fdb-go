package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// GROUP BY over a whole STRUCT is SUPPORTED — Java answers it by flattening
// the record grouping value into its primitive leaf accessors
// (Values.primitiveAccessorsForType, Values.java:99-121), driven from the
// grouping path at ImplementStreamingAggregationRule.java:111,
// GroupByExpression.java:434 and
// PushRequestedOrderingThroughGroupByRule.java:141. The pre-aggregate
// ordering requirement is therefore expressed over the primitive LEAVES in
// field order — never over the record itself — which is what makes the
// sort comparator's "no ordering defined between *dynamicpb.Message and
// *dynamicpb.Message" unreachable on this path.
//
// Before the Go port of that flattening, this exact query CRASHED with that
// raw internal comparator error (measured on two rows; a single-row table
// never compares and answered fine). Each subtest here would reproduce the
// crash if the flattening were disabled — that is the mutation contract.
func TestFDB_StructGrouping(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/structgroup")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /structgroup"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE sgr_tmpl "+
			"CREATE TYPE AS STRUCT GEO (lat BIGINT, lon BIGINT) "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT, pos GEO) "+
			"CREATE TABLE T_S (id BIGINT, home ADDR, cat STRING, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /structgroup/s WITH TEMPLATE sgr_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	dsn := fmt.Sprintf("fdbsql:///structgroup?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Two EQUAL struct values (rows 1,2 — same city/zip/pos), one differing
	// only in the NESTED leaf (row 3 — same city/zip, different pos.lat),
	// one differing in a top-level leaf (row 4), and a NULL struct (row 5).
	// The nested-leaf-only difference is load-bearing: a flattening that
	// stopped at the first record level would merge rows 1-3.
	for _, ins := range []string{
		"INSERT INTO T_S VALUES (1, ('sf', 94100, (37, -122)), 'a')",
		"INSERT INTO T_S VALUES (2, ('sf', 94100, (37, -122)), 'b')",
		"INSERT INTO T_S VALUES (3, ('sf', 94100, (38, -122)), 'a')",
		"INSERT INTO T_S VALUES (4, ('la', 90001, (34, -118)), 'a')",
		"INSERT INTO T_S VALUES (5, NULL, 'a')",
	} {
		if _, err := db.ExecContext(ctx, ins); err != nil {
			t.Fatalf("%s: %v", ins, err)
		}
	}

	// counts runs q (SELECT <cols>..., COUNT(*)) and returns each group as
	// "col|col|...=count", sorted, with a struct group column rendered by the
	// driver skipped from the key (the COUNT distribution is the assertion).
	counts := func(t *testing.T, q string, nCols int) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			vals := make([]any, nCols+1)
			ptrs := make([]any, nCols+1)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan: %v", err)
			}
			key := ""
			for i := 0; i < nCols; i++ {
				if i > 0 {
					key += "|"
				}
				key += fmt.Sprintf("%v", vals[i])
			}
			out = append(out, fmt.Sprintf("%s=%v", key, vals[nCols]))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Strings(out)
		return out
	}

	// countsOnly projects ONLY COUNT(*) per struct group, sidestepping how the
	// driver renders a struct column: the group COUNT DISTRIBUTION is what
	// proves grouping identity (equal structs merged, any leaf difference —
	// nested included — split, NULL its own group).
	countsOnly := func(t *testing.T, q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n int64
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, fmt.Sprintf("%d", n))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		sort.Strings(out)
		return out
	}

	t.Run("group_by_struct_counts", func(t *testing.T) {
		t.Parallel()
		// Groups: {1,2} (equal, incl. nested), {3} (nested leaf differs),
		// {4}, {5} (NULL struct). Distribution: 1,1,1,2.
		got := countsOnly(t, "SELECT COUNT(*) FROM T_S GROUP BY home")
		want := []string{"1", "1", "1", "2"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("GROUP BY <struct> distribution = %v, want %v", got, want)
		}
	})

	t.Run("group_by_nested_struct_counts", func(t *testing.T) {
		t.Parallel()
		// Grouping by the NESTED struct column directly: home.pos is not
		// reachable (Phase 3), but grouping the whole struct exercises the
		// recursion because ADDR contains GEO — covered above; here the
		// two-level flattening is probed via the distribution over a
		// projection of the struct itself.
		got := countsOnly(t, "SELECT COUNT(*) FROM T_S GROUP BY home, cat")
		// (sf,94100,(37,-122))×a, ×b, (38)×a, (la)×a, NULL×a → all size 1.
		want := []string{"1", "1", "1", "1", "1"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("GROUP BY <struct>, <scalar> distribution = %v, want %v", got, want)
		}
	})

	t.Run("group_by_struct_with_scalar_output", func(t *testing.T) {
		t.Parallel()
		// Mixed GROUP BY (scalar first) with the scalar projected: proves
		// the flattened ordering keeps scalar/struct keys composable and the
		// scalar group column still surfaces normally.
		got := counts(t, "SELECT cat, COUNT(*) FROM T_S GROUP BY cat, home", 1)
		want := []string{"a=1", "a=1", "a=1", "a=1", "b=1"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("GROUP BY cat, home = %v, want %v", got, want)
		}
	})
}
