package sqldriver_test

// Probes date/time part functions (Go extensions; Java 4.12 has no DATE/TIMESTAMP
// types): YEAR/MONTH/DAY/HOUR/MINUTE/SECOND extract the right parts, a TIMESTAMP
// column compares/filters against an ISO literal, and EXTRACT(part FROM ts) is
// rejected (0AF00) — conformant: Java stubs visitExtractFunctionCall.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFDB_DateTimeFunctionsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_dtfns")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_dtfns")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE dtfns "+
			"CREATE TABLE t (id BIGINT NOT NULL, ts TIMESTAMP, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_dtfns/s WITH TEMPLATE dtfns")
	dsn := fmt.Sprintf("fdbsql:///testdb_dtfns?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, e := db.ExecContext(ctx, "INSERT INTO t (id, ts) VALUES (1, ?)",
		time.Date(2026, 6, 28, 13, 45, 30, 0, time.UTC)); e != nil {
		t.Fatalf("insert: %v", e)
	}

	part := func(expr string) int64 {
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT "+expr+" FROM t WHERE id = 1").Scan(&v); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return v
	}
	for _, c := range []struct {
		expr string
		want int64
	}{
		{"YEAR(ts)", 2026},
		{"MONTH(ts)", 6},
		{"DAY(ts)", 28},
		{"HOUR(ts)", 13},
		{"MINUTE(ts)", 45},
		{"SECOND(ts)", 30},
	} {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			if got := part(c.expr); got != c.want {
				t.Errorf("%s = %d, want %d", c.expr, got, c.want)
			}
		})
	}

	t.Run("timestamp_compare_filter", func(t *testing.T) {
		var n int
		rows, err := db.QueryContext(ctx, "SELECT id FROM t WHERE ts > '2026-01-01 00:00:00'")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			n++
		}
		if n != 1 {
			t.Errorf("ts > '2026-01-01' count = %d, want 1", n)
		}
	})
	t.Run("extract_from_rejected", func(t *testing.T) {
		_, err := db.QueryContext(ctx, "SELECT EXTRACT(YEAR FROM ts) FROM t WHERE id = 1")
		if err == nil || !strings.Contains(err.Error(), "0AF00") {
			t.Errorf("EXTRACT(YEAR FROM ts) error = %v, want 0AF00 (Java stubs EXTRACT; use YEAR())", err)
		}
	})
}
