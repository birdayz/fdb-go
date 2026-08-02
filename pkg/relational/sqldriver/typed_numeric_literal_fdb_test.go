package sqldriver_test

// WIDTH-SUFFIXED numeric literals — Java parity for the `1I`/`2L`/`1.0f`/
// `2.0d` literal forms (Java ParseHelpers.parseDecimal,
// ParseHelpers.java:68-104):
//
//   - with a '.' in the token: f/F parses the binary32 FLOAT
//     (Float.parseFloat), d/D the DOUBLE — the suffix is only honoured
//     when a '.' is present (Java's contains(".") gate);
//   - without one: l/L parses LONG (staying LONG even when the value fits
//     int32 — Long.parseLong, no re-narrowing), i/I parses INT with
//     Integer.parseInt's range check; unsuffixed keeps the
//     fits-int32-then-INT-else-LONG rule (intLiteralType).
//
// Before the fix the constant folder handed the WHOLE token to
// strconv.ParseInt / ParseFloat — `insert … (1I, …)` failed `integer parse
// "1I"` (literal-tests.yamsql's engine-gap:typed-integer-literal) and
// `1.0f` failed `numeric literal out of range` (union.yamsql's
// engine-gap:typed-float-literal, armed on PR #577's branch).
//
// The suffix WIDTH is observable, not cosmetic: INT arithmetic runs the
// int32-bounded lane (ADD_II — overflow errors 22003 where the LONG lane
// returns the wide value), which is what the overflow subtests pin.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_TypedNumericLiterals(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_typedlit")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_typedlit")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE typedlit "+
			"CREATE TABLE b (b1 INTEGER NOT NULL, b2 STRING, b3 BIGINT, PRIMARY KEY (b1))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_typedlit/s WITH TEMPLATE typedlit")
	dsn := fmt.Sprintf("fdbsql:///testdb_typedlit?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// literal-tests.yamsql's own setup shape: suffixed literals in INSERT.
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (1I, 'a', 2L), (3i, 'b', 4l), (5, 'c', 6)")

	row := func(t *testing.T, q string, dest ...any) {
		t.Helper()
		if err := db.QueryRowContext(ctx, q).Scan(dest...); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}

	t.Run("suffixed inserts round-trip", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx, "SELECT b1, b2, b3 FROM b")
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var b1, b3 int64
			var b2 string
			if err := rows.Scan(&b1, &b2, &b3); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, fmt.Sprintf("%d|%s|%d", b1, b2, b3))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := "1|a|2,3|b|4,5|c|6"
		if strings.Join(got, ",") != want {
			t.Fatalf("rows = %v, want %s", got, want)
		}
	})

	t.Run("suffixed comparisons plan and answer", func(t *testing.T) {
		t.Parallel()
		// literal-tests.yamsql's own probes (results, not plans).
		var n int64
		row(t, "SELECT COUNT(*) FROM b WHERE 6L = coalesce(5L, 6L)", &n)
		if n != 0 {
			t.Fatalf("6L = coalesce(5L, 6L): count %d, want 0", n)
		}
		row(t, "SELECT COUNT(*) FROM b WHERE 6i = coalesce(5l, 6i)", &n)
		if n != 0 {
			t.Fatalf("6i = coalesce(5l, 6i): count %d, want 0", n)
		}
		row(t, "SELECT COUNT(*) FROM b WHERE 6L = coalesce(6L, 5L)", &n)
		if n != 3 {
			t.Fatalf("6L = coalesce(6L, 5L): count %d, want 3", n)
		}
	})

	t.Run("float suffix is binary32", func(t *testing.T) {
		t.Parallel()
		// 0.1 is not representable in binary32: the f-suffix literal is the
		// ROUNDED float32 value, which widened back to float64 is NOT 0.1.
		var eq bool
		row(t, "SELECT CAST(0.1f AS DOUBLE) = 0.1 FROM b WHERE b1 = 5", &eq)
		if eq {
			t.Fatalf("0.1f widened to DOUBLE compared equal to 0.1 — literal was not rounded to binary32")
		}
		row(t, "SELECT 0.5f = 0.5 FROM b WHERE b1 = 5", &eq)
		if !eq {
			t.Fatalf("0.5f (exactly representable) != 0.5")
		}
		row(t, "SELECT 1.0d = 1.0 FROM b WHERE b1 = 5", &eq)
		if !eq {
			t.Fatalf("1.0d != 1.0")
		}
	})

	t.Run("L keeps LONG width, I keeps INT width", func(t *testing.T) {
		t.Parallel()
		// INT arithmetic runs the int32 lane: 2147483647 + 1I overflows
		// 22003. The same addition with 1L promotes to the LONG lane and
		// returns the wide value.
		var v int64
		err := db.QueryRowContext(ctx, "SELECT 2147483647 + 1I FROM b WHERE b1 = 5").Scan(&v)
		if err == nil {
			t.Fatalf("2147483647 + 1I: expected 22003 overflow, got %d", v)
		}
		if !strings.Contains(err.Error(), "22003") {
			t.Fatalf("2147483647 + 1I: expected 22003, got %v", err)
		}
		row(t, "SELECT 2147483647 + 1L FROM b WHERE b1 = 5", &v)
		if v != 2147483648 {
			t.Fatalf("2147483647 + 1L = %d, want 2147483648", v)
		}
	})

	t.Run("out-of-range I suffix errors", func(t *testing.T) {
		t.Parallel()
		// Java: Integer.parseInt("99999999999") throws — the suffix pins
		// the width, it does not clamp.
		_, err := db.QueryContext(ctx, "SELECT 99999999999I FROM b")
		if err == nil {
			t.Fatalf("99999999999I: expected a parse error, succeeded")
		}
	})
}
