package sqldriver_test

// Probes the string-function set (comma forms): SUBSTR/SUBSTRING, LEFT, RIGHT,
// REVERSE, REPLACE, TRIM/LTRIM/RTRIM, CONCAT, POSITION(x,y), CHAR_LENGTH/LENGTH,
// + NULL propagation. The SQL-standard `POSITION(x IN y)` and
// `SUBSTRING(x FROM y FOR z)` syntaxes are rejected (0AF00) — conformant: Java's
// visitor stubs them (return visitChildren, no real resolution) and no Java test
// uses them; the comma forms are the supported path in both.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_StringFunctionsProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_strfns")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_strfns")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE strfns "+
			"CREATE TABLE t (id BIGINT NOT NULL, s STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_strfns/s WITH TEMPLATE strfns")
	dsn := fmt.Sprintf("fdbsql:///testdb_strfns?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id, s) VALUES (1, 'hello'), (2, '  pad  '), (3, 'héllo')")
	mwjoMustExec(t, db, ctx, "INSERT INTO t (id) VALUES (4)") // s NULL

	strAt := func(id int, expr string) sql.NullString {
		var v sql.NullString
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM t WHERE id = %d", expr, id)).Scan(&v); err != nil {
			t.Fatalf("%s (id=%d): %v", expr, id, err)
		}
		return v
	}
	ck := func(name, expr string, want string) {
		t.Run(name, func(t *testing.T) {
			got := strAt(1, expr)
			if !got.Valid || got.String != want {
				t.Errorf("%s = %q (valid=%v), want %q", expr, got.String, got.Valid, want)
			}
		})
	}

	ck("upper", "UPPER(s)", "HELLO")
	ck("lower", "LOWER(s)", "hello")
	ck("substr_3arg", "SUBSTR(s, 2, 3)", "ell")
	ck("substring_3arg", "SUBSTRING(s, 2, 3)", "ell")
	ck("left", "LEFT(s, 3)", "hel")
	ck("right", "RIGHT(s, 2)", "lo")
	ck("reverse", "REVERSE(s)", "olleh")
	ck("replace", "REPLACE(s, 'l', 'L')", "heLLo")
	ck("concat", "CONCAT(s, '!')", "hello!")

	t.Run("trim_variants", func(t *testing.T) {
		if got := strAt(2, "TRIM(s)"); got.String != "pad" {
			t.Errorf("TRIM('  pad  ') = %q, want 'pad'", got.String)
		}
		if got := strAt(2, "LTRIM(s)"); got.String != "pad  " {
			t.Errorf("LTRIM = %q, want 'pad  '", got.String)
		}
		if got := strAt(2, "RTRIM(s)"); got.String != "  pad" {
			t.Errorf("RTRIM = %q, want '  pad'", got.String)
		}
	})
	t.Run("char_length_unicode", func(t *testing.T) {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT CHAR_LENGTH(s) FROM t WHERE id = 3").Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 5 {
			t.Errorf("CHAR_LENGTH('héllo') = %d, want 5 (runes, not bytes)", n)
		}
	})
	t.Run("position_comma_form", func(t *testing.T) {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT POSITION('l', s) FROM t WHERE id = 1").Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 3 {
			t.Errorf("POSITION('l', 'hello') = %d, want 3 (1-based)", n)
		}
	})
	t.Run("upper_null_propagates", func(t *testing.T) {
		if got := strAt(4, "UPPER(s)"); got.Valid {
			t.Errorf("UPPER(NULL) = %q, want NULL", got.String)
		}
	})

	// Conformant rejections: SQL-standard IN / FROM-FOR syntaxes (Java stubs them).
	rejected := func(name, q string) {
		t.Run(name, func(t *testing.T) {
			_, err := db.QueryContext(ctx, q)
			if err == nil || !strings.Contains(err.Error(), "0AF00") {
				t.Errorf("%s error = %v, want 0AF00 (unsupported syntax form)", name, err)
			}
		})
	}
	rejected("position_in_form_rejected", "SELECT POSITION('l' IN s) FROM t WHERE id = 1")
	rejected("substring_from_for_rejected", "SELECT SUBSTRING(s FROM 2 FOR 3) FROM t WHERE id = 1")
}
