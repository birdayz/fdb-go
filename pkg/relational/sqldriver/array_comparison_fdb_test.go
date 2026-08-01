package sqldriver_test

// ARRAY comparison semantics — Java parity for {=, <>, IS [NOT] DISTINCT
// FROM} over ARRAY operands, plus the NULL/NONE (untyped `[]`) operand
// matrix and the plan-time rejections.
//
// The spec is Java's RelOpValue.encapsulate (RelOpValue.java:326-385):
// ARRAY operands are legal for the equality family only; an ARRAY compared
// against NULL or the untyped empty array `[]` (the NONE type) promotes the
// other side via PromoteValue (NULL_TO_ARRAY / NONE_TO_ARRAY,
// PromoteValue.java:88-90); after promotion the two ARRAY types must match
// modulo nullability — INTEGER ARRAY vs BIGINT ARRAY is DATATYPE_MISMATCH
// (42804), promotable in principle but deliberately unsupported
// (RelOpValue.java:365-370) — and there is no ordering operator over ARRAY
// or NONE at all (no LT_ARRAY_ARRAY row in the BinaryPhysicalOperator map),
// so `[1] < [2]` is 42804 too. Runtime equality is Java's
// Comparisons.compareListEquals (Comparisons.java:301-310): size mismatch
// is FALSE, elements compare pairwise with both-NULL equal and one-NULL
// unequal — element NULLs do NOT propagate UNKNOWN outward, so once both
// operands are non-NULL the result is two-valued.
//
// Before the fix Go evaluated `[1] = [1]` to NULL: the residual comparator
// (predicates.cmpAny) had no []any arm, so every array comparison degraded
// to UNKNOWN — the engine-gap:array-comparison entry this test closes.
//
// Expected results are the upstream corpus's own assertions
// (arrays-operators.yamsql, arrays-operators-binary-const block), which is
// the measured Java behaviour, not a prediction.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_ArrayComparison(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_arraycmp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_arraycmp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE arraycmp "+
			"CREATE TABLE dummy (pk BIGINT NOT NULL, PRIMARY KEY (pk)) "+
			"CREATE TABLE t1 (pk BIGINT NOT NULL, arr INTEGER ARRAY, arr_nn INTEGER ARRAY NOT NULL, PRIMARY KEY (pk))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_arraycmp/s WITH TEMPLATE arraycmp")
	dsn := fmt.Sprintf("fdbsql:///testdb_arraycmp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mwjoMustExec(t, db, ctx, "INSERT INTO dummy VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t1 (pk, arr, arr_nn) VALUES (-1, NULL, []), (0, [], []), (1, [1], [1])")

	// boolQuery runs a single-row single-column boolean projection and
	// returns (value, isNull).
	boolQuery := func(t *testing.T, q string) (bool, bool) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("query %q: no rows", q)
		}
		var v sql.NullBool
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", q, err)
		}
		if rows.Next() {
			t.Fatalf("query %q: more than one row", q)
		}
		return v.Bool, !v.Valid
	}

	t.Run("constant matrix", func(t *testing.T) {
		t.Parallel()
		// want: "t" TRUE, "f" FALSE, "n" NULL — the upstream corpus's
		// arrays-operators-binary-const expectations verbatim.
		cases := []struct {
			expr string
			want string
		}{
			{`[1] = [1]`, "t"},
			{`[1] = NULL`, "n"},
			{`[1] = []`, "f"},
			{`NULL = [1]`, "n"},
			{`NULL = CAST(NULL AS INTEGER ARRAY)`, "n"},
			{`NULL = []`, "n"},
			{`[] = [1]`, "f"},
			{`[] = NULL`, "n"},
			{`[] = CAST([] AS INTEGER ARRAY)`, "t"},
			{`[] = []`, "t"},
			{`[1] <> [1]`, "f"},
			{`[1] <> NULL`, "n"},
			{`[1] <> []`, "t"},
			{`[] <> [1]`, "t"},
			{`[] <> CAST([] AS INTEGER ARRAY)`, "f"},
			{`[1] IS DISTINCT FROM [1]`, "f"},
			{`[1] IS DISTINCT FROM NULL`, "t"},
			{`[1] IS DISTINCT FROM []`, "t"},
			{`NULL IS DISTINCT FROM [1]`, "t"},
			{`NULL IS DISTINCT FROM CAST(NULL AS INTEGER ARRAY)`, "f"},
			{`[] IS DISTINCT FROM NULL`, "t"},
			{`[] IS DISTINCT FROM CAST([] AS INTEGER ARRAY)`, "f"},
			{`[1] IS NOT DISTINCT FROM [1]`, "t"},
			{`[1] IS NOT DISTINCT FROM NULL`, "f"},
			{`[1] IS NOT DISTINCT FROM []`, "f"},
			{`NULL IS NOT DISTINCT FROM CAST(NULL AS INTEGER ARRAY)`, "t"},
			{`[] IS NOT DISTINCT FROM CAST([] AS INTEGER ARRAY)`, "t"},
			// Multi-element comparison is order-sensitive.
			{`[1, 2] <> [2, 1]`, "t"},
			{`[1, 2] = [1, 2]`, "t"},
			// Nested arrays compare deeply.
			{`[[1, 2], [3, 4]] IS NOT DISTINCT FROM [[1, 2], [3, 4]]`, "t"},
			{`[[1, 2], [3, 4]] = [[1, 2], [3, 4]]`, "t"},
			// Element NULLs do NOT propagate: once both operands are
			// non-NULL the result is two-valued (compareListEquals:
			// both-null elements are EQUAL, one-null is UNEQUAL).
			// MEASURED DIVERGENCE (live Java 4.12.11.0, pinned in
			// conformance's ArrayComparisonJavaProbe): Java throws a raw
			// NullPointerException on a NULL element inside a compared
			// array literal — an upstream bug, not designed semantics.
			// Go answers what Java's own compareListEquals specifies.
			{`[NULL] = [NULL]`, "t"},
			{`[1, NULL] = [1, NULL]`, "t"},
			// Size mismatch is FALSE, not UNKNOWN.
			{`[1] = [1, 2]`, "f"},
		}
		for _, c := range cases {
			got, isNull := boolQuery(t, "SELECT "+c.expr+" FROM dummy")
			var gotS string
			switch {
			case isNull:
				gotS = "n"
			case got:
				gotS = "t"
			default:
				gotS = "f"
			}
			if gotS != c.want {
				t.Errorf("%s: got %s, want %s", c.expr, gotS, c.want)
			}
		}
	})

	t.Run("field comparisons", func(t *testing.T) {
		t.Parallel()
		pks := func(t *testing.T, q string) []int64 {
			t.Helper()
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("query %q: %v", q, err)
			}
			defer rows.Close()
			var out []int64
			for rows.Next() {
				var pk int64
				if err := rows.Scan(&pk); err != nil {
					t.Fatalf("scan %q: %v", q, err)
				}
				out = append(out, pk)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows %q: %v", q, err)
			}
			return out
		}
		for _, c := range []struct {
			q    string
			want []int64
		}{
			{`SELECT pk FROM t1 WHERE arr = [1]`, []int64{1}},
			{`SELECT pk FROM t1 WHERE arr_nn = [1]`, []int64{1}},
			{`SELECT pk FROM t1 WHERE arr IS NOT DISTINCT FROM [1]`, []int64{1}},
			// MEASURED DIVERGENCE PIN (RFC-143 §3a): Go writes a nullable
			// array as a PLAIN repeated field — no `values` wrapper
			// message — so a STORED empty array is byte-identical to a
			// stored NULL and reads back as NULL. Java answers pk 0 for
			// each of these three (arrays-operators.yamsql); Go returns
			// NO rows because the empty array the setup inserted
			// evaluates to NULL, and `NULL = []` is UNKNOWN. When the
			// §3a wrapper write side lands, these three pins MUST flip
			// to []int64{0} — a failure here is that re-arm signal, not
			// a comparison-semantics regression.
			{`SELECT pk FROM t1 WHERE arr = CAST([] AS INTEGER ARRAY)`, nil},
			{`SELECT pk FROM t1 WHERE arr = []`, nil},
			{`SELECT pk FROM t1 WHERE arr_nn = [] AND pk != -1`, nil},
		} {
			got := pks(t, c.q)
			if len(got) != len(c.want) {
				t.Errorf("%s: got %v, want %v", c.q, got, c.want)
				continue
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("%s: got %v, want %v", c.q, got, c.want)
					break
				}
			}
		}
	})

	t.Run("rejections", func(t *testing.T) {
		t.Parallel()
		// Java: RelOpValue.encapsulate rejects promotable-but-unequal ARRAY
		// types, incompatible ARRAY types, and every ordering comparison
		// over ARRAY operands, all as COMPARISON_OF_INCOMPATIBLE_TYPES →
		// DATATYPE_MISMATCH (ExceptionUtil.java:98-99).
		for _, q := range []string{
			`SELECT * FROM t1 WHERE arr = CAST([] AS STRING ARRAY)`,
			`SELECT pk FROM t1 WHERE arr = CAST([1] AS BIGINT ARRAY)`,
			// Element NULLABILITY is part of the ARRAY type match (only the
			// OUTER nullability is stripped, RelOpValue.java:367-369): the
			// NULL element folds the left element type nullable, the right
			// stays NOT NULL — live Java rejects (ArrayComparisonJavaProbe).
			`SELECT [1, NULL] = [1, 2] FROM dummy`,
			`SELECT [1] < [2] FROM dummy`,
			`SELECT [1] <= [1] FROM dummy`,
			`SELECT [1] > [] FROM dummy`,
			`SELECT [1] >= NULL FROM dummy`,
		} {
			_, err := db.QueryContext(ctx, q)
			if err == nil {
				t.Errorf("%s: expected 42804, succeeded", q)
				continue
			}
			var relErr *api.Error
			if !errors.As(err, &relErr) || relErr.Code != api.ErrCodeDatatypeMismatch {
				t.Errorf("%s: expected 42804 DATATYPE_MISMATCH, got %v", q, err)
			}
		}
	})
}
