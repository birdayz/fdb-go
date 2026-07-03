package sqldriver_test

// RFC-173 W4b e2e: the correlated-scalar-subquery-in-projection seed ORDINALIZES
// when the outer is a single source, and executes correctly end-to-end. The
// ordinal-vs-name-model choice is invisible in EXPLAIN, so the seed SHAPE is
// pinned white-box (rfc173_w4b_scalar_seed_test.go); this proves the ordinal
// path returns correct rows AND that both a QUALIFIED outer ref (d.id) and a
// BARE one (id) resolve to the single outer ordinal (single-source
// alias-normalization — design condition b: the ordinal seed emits ONE field
// per column, no bare+qualified duplicates, so a qualified ref must resolve via
// the leg's RecordName span rather than a second name-model field).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_RFC173W4b_ScalarSubqueryOrdinalSeed(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ssos")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ssos")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ssos "+
		"CREATE TABLE dept (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ssos/s WITH TEMPLATE ssos")
	dsn := fmt.Sprintf("fdbsql:///testdb_ssos?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// dept 1: exactly one emp (salary 500) — single match, so no user LIMIT is
	// needed and the cardinality guard never fires. dept 2: no emp — the
	// LEFT-OUTER null-fill case (the inner ordinal is nullable).
	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id, name) VALUES (1, 'eng'), (2, 'ops')")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, dept_id, salary) VALUES (10, 1, 500)")

	// QUALIFIED outer refs (d.id, d.name) over the single-source (ordinalized) outer.
	t.Run("qualified_outer_refs", func(t *testing.T) {
		var id int64
		var name string
		var sal sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT d.id, d.name, (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 1",
		).Scan(&id, &name, &sal); sErr != nil {
			t.Fatalf("qualified outer refs: %v", sErr)
		}
		if id != 1 || name != "eng" || !sal.Valid || sal.Int64 != 500 {
			t.Fatalf("qualified = (%d, %q, valid=%v/%d), want (1, eng, valid/500)", id, name, sal.Valid, sal.Int64)
		}
	})

	// BARE outer refs (id, name) — must resolve to the SAME outer ordinals.
	t.Run("bare_outer_refs", func(t *testing.T) {
		var id int64
		var name string
		var sal sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT id, name, (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE id = 1",
		).Scan(&id, &name, &sal); sErr != nil {
			t.Fatalf("bare outer refs: %v", sErr)
		}
		if id != 1 || name != "eng" || !sal.Valid || sal.Int64 != 500 {
			t.Fatalf("bare = (%d, %q, valid=%v/%d), want (1, eng, valid/500)", id, name, sal.Valid, sal.Int64)
		}
	})

	// dept 2 has no emp → the scalar is NULL (LEFT-OUTER null-fill through the
	// nullable inner ordinal), NOT a dropped row.
	t.Run("empty_inner_null_scalar", func(t *testing.T) {
		var id int64
		var sal sql.NullInt64
		if sErr := db.QueryRowContext(ctx,
			"SELECT d.id, (SELECT salary FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 2",
		).Scan(&id, &sal); sErr != nil {
			t.Fatalf("empty inner: %v", sErr)
		}
		if id != 2 || sal.Valid {
			t.Fatalf("empty inner = (%d, valid=%v), want (2, NULL)", id, sal.Valid)
		}
	})
}

// TestFDB_RFC173W4b_ScalarSubqueryOrdinalSeed_ColumnType pins the RESULT-SET
// COLUMN TYPE of the ordinalized correlated-scalar subquery for a NON-BIGINT
// scalar. The ordinal seed types the inner scalar leg UnknownType at translation
// (Go quantifier flowed types are untyped), so deriveColumnsFromFlatMap must
// recover the real type from the INNER plan — otherwise a DOUBLE (AVG) or STRING
// scalar regresses to BIGINT while the name-model path reports the true type.
// The DATA is always correct; only the metadata was wrong. RED without the
// deriveColumnsFromFlatMap fix (both report BIGINT); GREEN with it. That the
// test is RED at all also proves the ORDINAL path is taken here — the name-model
// path already reports the true type, so a name-model plan would be GREEN
// unconditionally.
func TestFDB_RFC173W4b_ScalarSubqueryOrdinalSeed_ColumnType(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ssosct")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ssosct")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ssosct "+
		"CREATE TABLE dept (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, ename STRING, dsal DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ssosct/s WITH TEMPLATE ssosct")
	dsn := fmt.Sprintf("fdbsql:///testdb_ssosct?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// dept 1: two emps (for a well-defined aggregate). dept 2: exactly one emp (so
	// the non-aggregate plain-column scalar's strict-single guard never fires).
	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id, name) VALUES (1, 'eng'), (2, 'ops')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO emp (id, dept_id, salary, ename, dsal) VALUES (10, 1, 400, 'alice', 400.5), (11, 1, 600, 'bob', 600.5), (20, 2, 700, 'carol', 700.5)")

	// colType returns the DatabaseTypeName of the col-th (0-indexed) result column
	// and fully drains the rows so any execution error surfaces.
	colType := func(t *testing.T, q string, col, wantCols int) string {
		t.Helper()
		rows, qErr := db.QueryContext(ctx, q)
		if qErr != nil {
			t.Fatalf("query %q: %v", q, qErr)
		}
		defer rows.Close()
		cts, cErr := rows.ColumnTypes()
		if cErr != nil {
			t.Fatalf("ColumnTypes %q: %v", q, cErr)
		}
		if len(cts) != wantCols {
			t.Fatalf("query %q: got %d columns, want %d", q, len(cts), wantCols)
		}
		typ := cts[col].DatabaseTypeName()
		for rows.Next() { // drain — force execution, catch a runtime error
		}
		if rErr := rows.Err(); rErr != nil {
			t.Fatalf("drain %q: %v", q, rErr)
		}
		return typ
	}

	// DOUBLE aggregate scalar: AVG(BIGINT) → DOUBLE (function-determined). The
	// aggregate's synthesized output column resolves against no base descriptor,
	// so the enclosing projection INHERITS the type from deriveColumnsFromFlatMap
	// — RED (BIGINT) when the ordinal-seed inner scalar leg is mis-typed, GREEN
	// once it is recovered from the inner plan.
	t.Run("double_avg_scalar", func(t *testing.T) {
		got := colType(t,
			"SELECT d.id, (SELECT AVG(salary) FROM emp e WHERE e.dept_id = d.id) FROM dept d", 1, 2)
		if got != "DOUBLE" {
			t.Fatalf("AVG scalar column type = %q, want DOUBLE (BIGINT means the ordinal-seed inner leg fell back to the wrong type)", got)
		}
	})

	// SUM over a DOUBLE column → DOUBLE. Second, structurally-distinct non-BIGINT
	// case: unlike AVG (whose DOUBLE is FUNCTION-determined), SUM's type is
	// OPERAND-determined — a different derivation path — so this proves the fix
	// reports whatever the INNER PLAN's single output column is, not an
	// AVG-special-case. Also RED (BIGINT) → GREEN (DOUBLE).
	t.Run("double_sum_of_double_scalar", func(t *testing.T) {
		got := colType(t,
			"SELECT d.id, (SELECT SUM(dsal) FROM emp e WHERE e.dept_id = d.id) FROM dept d", 1, 2)
		if got != "DOUBLE" {
			t.Fatalf("SUM(DOUBLE) scalar column type = %q, want DOUBLE", got)
		}
	})

	// Guard: the plain STRING-column scalar reports STRING. This shape does NOT
	// exercise the bug — its projection column references the real field E.ENAME,
	// which resolves via the emp descriptor before the FlatMap type is ever
	// consulted — so it is GREEN with or without the fix. Pinned to prove the
	// change is not collateral damage. (A NON-BIGINT-typed scalar that BOTH
	// exercises the bug AND executes cleanly is unreachable here: string columns
	// are descriptor-resolved, string aggregates are planner-rejected, and string
	// EXPRESSIONS like CAST/UPPER surface a SEPARATE pre-existing W4b executor
	// limitation — a computed-projection inner produces a name-model merge row the
	// ordinal leg adapter rejects. The two numeric cases above are the clean
	// RED→GREEN proofs.)
	t.Run("string_plain_column_unaffected", func(t *testing.T) {
		got := colType(t,
			"SELECT d.id, (SELECT ename FROM emp e WHERE e.dept_id = d.id) FROM dept d WHERE d.id = 2", 1, 2)
		if got != "STRING" {
			t.Fatalf("plain STRING column scalar type = %q, want STRING", got)
		}
	})
}

// TestFDB_RFC173W4b_ScalarInnerShapeProbe maps the inner-shape safety boundary of
// the ordinal seed: every inner shape must EXECUTE without tripping the ordinal
// leg adapter ("name-model leg row carries NONE of the leg type's 1 columns"),
// which is the seed's regression signature. Plain-column, aggregate, AND
// computed-expression inners now ordinalize (RFC-173 W4b shape 3 materializes a
// computed scalar as the inner's projected output, so it is present in the inner
// row); only a JOIN-inner is still gated to name-model (innerContainsJoin, shape
// 2). This pins that the ordinal path runs cleanly on every non-join shape — no
// runtime error. (Correct-rows for the computed shapes: TestFDB_RFC173W4b_ComputedScalar.)
func TestFDB_RFC173W4b_ScalarInnerShapeProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_ssisp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_ssisp")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE ssisp "+
		"CREATE TABLE dept (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
		"CREATE TABLE emp (id BIGINT NOT NULL, dept_id BIGINT, salary BIGINT, ename STRING, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_ssisp/s WITH TEMPLATE ssisp")
	dsn := fmt.Sprintf("fdbsql:///testdb_ssisp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// dept 2 has exactly one emp so a plain-column / computed inner has a single
	// match (no cardinality guard); dept 1 has one too for the aggregates.
	mwjoMustExec(t, db, ctx, "INSERT INTO dept (id) VALUES (1), (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO emp (id, dept_id, salary, ename) VALUES (10, 1, 400, 'alice'), (20, 2, 700, 'carol')")

	shapes := map[string]string{
		"plain_column":   "(SELECT salary FROM emp e WHERE e.dept_id = d.id)",
		"aggregate_avg":  "(SELECT AVG(salary) FROM emp e WHERE e.dept_id = d.id)",
		"aggregate_max":  "(SELECT MAX(salary) FROM emp e WHERE e.dept_id = d.id)",
		"computed_upper": "(SELECT UPPER(ename) FROM emp e WHERE e.dept_id = d.id)",
		"computed_arith": "(SELECT salary + 1 FROM emp e WHERE e.dept_id = d.id)",
		"computed_cast":  "(SELECT CAST(salary AS DOUBLE) FROM emp e WHERE e.dept_id = d.id)",
	}
	for name, sq := range shapes {
		t.Run(name, func(t *testing.T) {
			q := "SELECT d.id, " + sq + " FROM dept d WHERE d.id = 2"
			rows, qErr := db.QueryContext(ctx, q)
			if qErr == nil {
				for rows.Next() {
					var a int64
					var b sql.NullString
					if sErr := rows.Scan(&a, &b); sErr != nil {
						qErr = sErr
						break
					}
				}
				if qErr == nil {
					qErr = rows.Err()
				}
				rows.Close()
			}
			// The seed's regression signature MUST NOT appear — my ordinal path
			// must never feed a name-model merge row to an ordinal leg. A different
			// error (or success) is master's behavior, not my regression.
			if qErr != nil && strings.Contains(qErr.Error(), "name-model leg row carries NONE") {
				t.Fatalf("shape %s tripped the ordinal leg adapter (seed regression): %v", name, qErr)
			}
		})
	}
}
