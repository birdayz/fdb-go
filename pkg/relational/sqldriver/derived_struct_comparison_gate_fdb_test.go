package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// The whole-struct comparison gate must survive a DERIVED TABLE and a CTE.
//
// The gate (expr.comparisonOperandSupported, Java's
// RelOpValue.isSupportedOperandType at RelOpValue.java:320-322, asserted at
// :333/:345/:350) reads the operand's values.Type. A base-table struct column
// types as RECORD via structColumnType, which reads semantic.Column
// .StructFields. Derived-table and CTE virtual columns used to be minted as a
// bare `semantic.Column{Id, Type, Nullable}`, DROPPING StructFields and
// IsArray — so the operand typed UNKNOWN, and UNKNOWN is DELIBERATELY admitted
// by the gate (the carve-out bound parameters need, since `? = ?` is a shape
// Java never presents to this check).
//
// The result was silent wrong rows: `home IS DISTINCT FROM other` rejected
// 0AF00 on the base table and answered [1 2] — where [2] is right — through a
// derived table, because the row-time comparator has no record arm and every
// row evaluated UNKNOWN. Simple-CASE answered wrong non-NULL values the same
// way.
//
// THE DIMENSION THIS TEST EXISTS ON: BOTH operands must come through the
// derived table. A MIXED pair (one derived, one base) CANNOT express the
// defect — the base operand still types RECORD and the gate fires on it — so a
// mixed-pair test passes with the bug fully present. mixed_pair_cannot_detect
// below pins that fact so the reason this test is shaped the way it is cannot
// be optimised away.
func TestFDB_DerivedStructComparisonGate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/derivedstructgate")
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE /derivedstructgate"); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE dsg_tmpl "+
			"CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT) "+
			"CREATE TABLE T_S (id BIGINT, home ADDR, other ADDR, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA /derivedstructgate/s WITH TEMPLATE dsg_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	dsn := fmt.Sprintf("fdbsql:///derivedstructgate?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// t.Cleanup, NOT defer: the subtests below call t.Parallel(), so they run
	// AFTER this function body returns. A deferred Close fires first and every
	// subtest fails with "sql: database is closed".
	t.Cleanup(func() { db.Close() })

	// Row 1: the two structs are EQUAL. Row 2: they DIFFER. Both rows are
	// needed — with only equal rows or only differing rows, one of the two
	// wrong answers (all-rows / no-rows) coincides with the right one.
	for _, ins := range []string{
		"INSERT INTO T_S VALUES (1, ('sf', 94100), ('sf', 94100))",
		"INSERT INTO T_S VALUES (2, ('la', 90001), ('ny', 10001))",
	} {
		if _, err := db.ExecContext(ctx, ins); err != nil {
			t.Fatalf("%s: %v", ins, err)
		}
	}

	// mustReject runs q and requires the 0AF00 whole-struct rejection. A query
	// that ANSWERS is the bug: it means the operand typed UNKNOWN and the
	// comparison planned onto a comparator with no record arm.
	mustReject := func(t *testing.T, q string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			var got []string
			for rows.Next() {
				var v any
				if scanErr := rows.Scan(&v); scanErr != nil {
					break
				}
				got = append(got, fmt.Sprintf("%v", v))
			}
			rows.Close()
			t.Fatalf("query ANSWERED rows=%v instead of rejecting the whole-struct comparison.\n"+
				"query: %s\n"+
				"The virtual column's semantic.Column lost StructFields, so the operand typed "+
				"UNKNOWN and comparisonOperandSupported admitted it (the bound-parameter "+
				"carve-out). Carry the whole source column across (renameCarriedColumn in "+
				"logical_predicate.go) instead of minting {Id, Type, Nullable}.", got, q)
		}
		if !strings.Contains(err.Error(), "0AF00") {
			t.Fatalf("query rejected with the WRONG error.\nquery: %s\ngot:   %v\n"+
				"want a 0AF00 whole-struct rejection", q, err)
		}
	}

	// The base-table controls. If these ever stop rejecting, the gate itself
	// is gone and every case below would pass vacuously.
	t.Run("base_controls_reject", func(t *testing.T) {
		t.Parallel()
		mustReject(t, "SELECT id FROM T_S WHERE home = other")
		mustReject(t, "SELECT id FROM T_S WHERE home IS DISTINCT FROM other")
	})

	// BOTH operands through a DERIVED TABLE, across every operator family that
	// reaches the gate by a different door: equality and IS DISTINCT FROM go
	// through ResolveComparison, BETWEEN expands to a conjunction of ordering
	// comparisons, simple-CASE compares the operand against each WHEN arm, and
	// `>` is the ordering arm.
	t.Run("derived_table_both_operands", func(t *testing.T) {
		t.Parallel()
		const from = " FROM (SELECT id, home AS h, other AS o FROM T_S) x"
		mustReject(t, "SELECT x.id"+from+" WHERE x.h = x.o")
		mustReject(t, "SELECT x.id"+from+" WHERE x.h IS DISTINCT FROM x.o")
		mustReject(t, "SELECT x.id"+from+" WHERE x.h > x.o")
		mustReject(t, "SELECT x.id"+from+" WHERE x.h BETWEEN x.o AND x.o")
		mustReject(t, "SELECT CASE x.h WHEN x.o THEN 1 ELSE 0 END"+from)
	})

	// The CTE spelling reaches a DIFFERENT mint site than the derived-table
	// one, so it is not a restatement of the case above.
	t.Run("cte_both_operands", func(t *testing.T) {
		t.Parallel()
		const with = "WITH x AS (SELECT id, home AS h, other AS o FROM T_S) "
		mustReject(t, with+"SELECT x.id FROM x WHERE x.h = x.o")
		mustReject(t, with+"SELECT x.id FROM x WHERE x.h IS DISTINCT FROM x.o")
		mustReject(t, with+"SELECT CASE x.h WHEN x.o THEN 1 ELSE 0 END FROM x")
	})

	// DERIVED-OF-DERIVED is a third mint site: the inner scope is rebuilt
	// recursively and the projection aliases re-applied, and that rebuild had
	// its own bare-Column mint.
	t.Run("derived_of_derived", func(t *testing.T) {
		t.Parallel()
		mustReject(t,
			"SELECT y.id FROM (SELECT id, h, o FROM "+
				"(SELECT id, home AS h, other AS o FROM T_S) x) y WHERE y.h = y.o")
	})

	// A CTE referenced ONLY from an ON clause takes the wrap-rebuild path,
	// which resolves the body's legs separately from the two arms above. Both
	// a JOIN body and a DERIVED body reach it.
	t.Run("on_only_cte", func(t *testing.T) {
		t.Parallel()
		mustReject(t,
			"WITH c AS (SELECT a.id AS cid, a.home AS h, b.other AS o FROM T_S a JOIN T_S b ON a.id = b.id) "+
				"SELECT t.id FROM T_S t JOIN c ON c.h = c.o")
		mustReject(t,
			"WITH c AS (SELECT d.id AS cid, d.h AS h, d.o AS o FROM "+
				"(SELECT id, home AS h, other AS o FROM T_S) d) "+
				"SELECT t.id FROM T_S t JOIN c ON c.h = c.o")
	})

	// A RECURSIVE CTE is a separate scope-construction path, and it is the one
	// that looked safe. It is NOT safe by construction — it was silent-wrong
	// exactly like the others: with the StructFields carry reverted this query
	// ANSWERS `rows=[]` on a table whose row 1 satisfies the predicate.
	//
	// Pinning it here is the point. "Recursive CTEs don't leak" was a claim
	// about a path nobody had measured; what actually makes it not leak is the
	// carry, so this is the test that re-arms if the carry regresses on the
	// recursive path specifically.
	t.Run("recursive_cte_both_operands", func(t *testing.T) {
		t.Parallel()
		mustReject(t,
			"WITH RECURSIVE r AS ("+
				"SELECT id, home AS h, other AS o FROM T_S "+
				"UNION ALL SELECT id, h, o FROM r WHERE id < 0) "+
				"SELECT r.id FROM r WHERE r.h = r.o")
	})

	// NEGATIVE RESULT, pinned deliberately: a MIXED pair cannot express the
	// defect. This is the dimension the original coverage missed, and it is
	// why every case above puts BOTH operands behind the derived table.
	//
	// It rejects whether or not the fix is present — the BASE operand still
	// types RECORD and the gate fires on it — so if someone ever "simplifies"
	// the cases above into this shape, the suite goes green with the bug fully
	// restored. Keeping it here, labelled, makes that trade visible.
	t.Run("mixed_pair_cannot_detect_the_defect", func(t *testing.T) {
		t.Parallel()
		mustReject(t,
			"SELECT x.id FROM (SELECT id, home AS h FROM T_S) x, T_S b WHERE x.h = b.other")
	})

	// A struct column carried through a derived table must still be SELECTABLE
	// as a whole value. The fix carries the source column wholesale, and a
	// carry that broke plain projection would be a worse bug than the one it
	// closes.
	t.Run("whole_struct_still_projects_through_derived", func(t *testing.T) {
		t.Parallel()
		rows, err := db.QueryContext(ctx,
			"SELECT x.h FROM (SELECT id, home AS h FROM T_S) x ORDER BY x.id")
		if err != nil {
			t.Fatalf("projecting a whole struct through a derived table must work: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if v == nil {
				t.Fatal("struct projected through a derived table came back NULL")
			}
			n++
		}
		if n != 2 {
			t.Fatalf("got %d rows, want 2", n)
		}
	})
}
