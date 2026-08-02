package sqldriver_test

// BITMAP_BUCKET_OFFSET / BITMAP_BIT_POSITION take EXACTLY one user argument.
// Java resolves them through
//
//	argumentsCount -> BuiltInFunctionCatalog.resolve(name, 1 + argumentsCount)
//
// (SqlFunctionCatalogImpl.java:126-127) and the only registered built-in is the
// binary one, so zero user arguments resolve arity 1 and two resolve arity 3 —
// neither exists, and the query is rejected. The injected second argument is the
// default entry size 10000, appended by the analyzer, never supplied by the
// caller (SemanticAnalyzer.java:987-990).
//
// Go's catalogue entry carries a fixed result type and no arity, and the
// injection was conditional on len(args)==1, so every OTHER arity fell straight
// through admission: `BITMAP_BUCKET_OFFSET()` evaluated to NULL, and
// `BITMAP_BUCKET_OFFSET(id, 5)` silently used the caller's 5 as the bucket size
// — a value unrelated to the 10000 any bitmap index was built with, so the
// result does not address the same buckets the index stores.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_BitmapScalarFunctions_ArityIsExactlyOne(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_bmarity")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_bmarity")
	mwjoMustExec(t, setup, ctx, `CREATE SCHEMA TEMPLATE bmarity_tpl
		CREATE TABLE t(id BIGINT, g BIGINT, PRIMARY KEY(id))`)
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_bmarity/s1 WITH TEMPLATE bmarity_tpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_bmarity?cluster_file=%s&schema=s1", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (45678, 1)")

	t.Run("unary_is_accepted_and_uses_the_injected_10000", func(t *testing.T) {
		t.Parallel()
		var offset, position int64
		row := db.QueryRowContext(ctx,
			`SELECT BITMAP_BUCKET_OFFSET(id), BITMAP_BIT_POSITION(id) FROM t WHERE id = 45678`)
		if err := row.Scan(&offset, &position); err != nil {
			t.Fatalf("the unary form is the ONLY supported arity and must work: %v", err)
		}
		// 45678 with the analyzer-injected entry size 10000.
		if offset != 40000 || position != 5678 {
			t.Fatalf("got (offset=%d, position=%d), want (40000, 5678) — the second "+
				"argument must be the injected default 10000", offset, position)
		}
	})

	for _, tc := range []struct{ name, q string }{
		{"zero_args_bucket_offset", `SELECT BITMAP_BUCKET_OFFSET() FROM t`},
		{"zero_args_bit_position", `SELECT BITMAP_BIT_POSITION() FROM t`},
		{"two_args_bucket_offset", `SELECT BITMAP_BUCKET_OFFSET(id, 5) FROM t`},
		{"two_args_bit_position", `SELECT BITMAP_BIT_POSITION(id, 5) FROM t`},
		{"three_args_bucket_offset", `SELECT BITMAP_BUCKET_OFFSET(id, 5, 7) FROM t`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.q)
			if err == nil {
				cols, _ := rows.Columns()
				var got []any
				if rows.Next() {
					got = make([]any, len(cols))
					for i := range got {
						got[i] = new(any)
					}
					_ = rows.Scan(got...)
				}
				rows.Close()
				t.Fatalf("%s was ACCEPTED (returned %d columns) — Java resolves arity "+
					"1+n and has only the binary built-in, so every arity but one must "+
					"be rejected; a caller-supplied entry size does not address the "+
					"buckets any bitmap index actually stores", tc.q, len(cols))
			}
			// The refusal is 0AF00, Java's ErrorCode.UNSUPPORTED_QUERY. The
			// message is the planner's generic one because the walker DECLINES
			// the shape and the caller falls through to the default — the same
			// architectural path the adjacent distance-operator arity check
			// takes, and the shape the conformance principle requires. What
			// matters is that the query never runs and never yields a NULL or a
			// caller-chosen entry size.
			if !strings.Contains(err.Error(), "0AF00") {
				t.Fatalf("%s: want a 0AF00 unsupported-query refusal, got %v", tc.q, err)
			}
		})
	}
}
