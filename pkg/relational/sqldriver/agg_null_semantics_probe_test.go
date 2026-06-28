package sqldriver_test

// Probes NULL-aware aggregate semantics: COUNT(*) counts rows, COUNT(col) and
// COUNT(DISTINCT col) skip NULLs, SUM/AVG/MAX/MIN ignore NULLs, AVG divides by
// the non-NULL count, and SELECT DISTINCT keeps exactly one NULL.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestFDB_AggNullSemanticsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_aggnull")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_aggnull")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE aggnull "+
			"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_aggnull/s WITH TEMPLATE aggnull")
	dsn := fmt.Sprintf("fdbsql:///testdb_aggnull?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// v = 10, 10, NULL, 20, NULL
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, v) VALUES (1, 10), (2, 10), (4, 20)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (3), (5)")

	scalarI := func(q string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}

	t.Run("count_star_all_rows", func(t *testing.T) {
		if got := scalarI("SELECT COUNT(*) FROM t"); got != 5 {
			t.Errorf("COUNT(*) = %d, want 5", got)
		}
	})
	t.Run("count_col_skips_null", func(t *testing.T) {
		if got := scalarI("SELECT COUNT(v) FROM t"); got != 3 {
			t.Errorf("COUNT(v) = %d, want 3 (non-NULL only)", got)
		}
	})
	t.Run("count_distinct_rejected_conformant", func(t *testing.T) {
		// DISTINCT aggregates are unsupported in BOTH engines (Java rejects in
		// ExpressionVisitor.visitAggregateWindowedFunction with UNSUPPORTED_QUERY).
		// Go must reject with the SAME SQLSTATE 0AF00, not 0A000.
		_, err := db.QueryContext(ctx, "SELECT COUNT(DISTINCT v) FROM t")
		if err == nil {
			t.Fatal("COUNT(DISTINCT v) unexpectedly succeeded; Java rejects it (UNSUPPORTED_QUERY)")
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("COUNT(DISTINCT v) error = %v, want SQLSTATE 0AF00 (Java UNSUPPORTED_QUERY)", err)
		}
	})
	t.Run("sum_skips_null", func(t *testing.T) {
		if got := scalarI("SELECT SUM(v) FROM t"); got != 40 {
			t.Errorf("SUM(v) = %d, want 40", got)
		}
	})
	t.Run("max_min_skip_null", func(t *testing.T) {
		if got := scalarI("SELECT MAX(v) FROM t"); got != 20 {
			t.Errorf("MAX(v) = %d, want 20", got)
		}
		if got := scalarI("SELECT MIN(v) FROM t"); got != 10 {
			t.Errorf("MIN(v) = %d, want 10", got)
		}
	})
	t.Run("avg_divides_by_nonnull_count", func(t *testing.T) {
		var avg float64
		if err := db.QueryRowContext(ctx, "SELECT AVG(v) FROM t").Scan(&avg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want := 40.0 / 3.0 // non-NULL count is 3, not 5
		if math.Abs(avg-want) > 1e-9 {
			t.Errorf("AVG(v) = %v, want %v (SUM/non-NULL count)", avg, want)
		}
	})
	t.Run("distinct_keeps_one_null", func(t *testing.T) {
		// SELECT DISTINCT v → {10, 20, NULL}: 3 distinct values, exactly one NULL.
		rows, err := db.QueryContext(ctx, "SELECT DISTINCT v FROM t")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		total, nulls := 0, 0
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			total++
			if !v.Valid {
				nulls++
			}
		}
		if total != 3 || nulls != 1 {
			t.Errorf("DISTINCT v = %d rows (%d null), want 3 rows with exactly 1 null", total, nulls)
		}
	})
}
