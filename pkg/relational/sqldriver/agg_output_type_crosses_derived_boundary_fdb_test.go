package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_AggregateOutputTypeCrossesTheDerivedBoundary pins that an aggregate's
// output column carries a REAL type across a derived-table boundary.
//
// THE DEFECT. aggOutputCols typed every aggregate output "UNKNOWN" — only a
// SELECT-list COUNT(*) got BIGINT — so `MIN(col2) AS G` arrived at the enclosing
// query untyped and `G + 4` demoted to the narrow arithmetic lane. Measured
// against Java on the vendored corpus (groupby-tests.yamsql:178,181,
// `select G + 4 from (select MIN(x.col2) as G from … group by x.col1) as Y
// where G > 5` → `[{!l 10}]`): expected 10 as a Long, got 10 as an Integer. The
// VALUE was right, which is why this hid — the type is what moved.
//
// JAVA IS THE SPEC AND HAS NO TABLE OF SQL-LEVEL AGGREGATE RESULT TYPES. The
// type falls out of which PhysicalOperator `encapsulate` selects for the
// (function, argument TypeCode) pair (NumericAggregationValue.java:194-213), and
// that lookup is EXACT — no widening. Read off the enum:
//
//   - COUNT / COUNT(*) → LONG always (CountValue.java:241-243, getResultType
//     :140-141). The argument's type is irrelevant.
//   - AVG → DOUBLE always: AVG_I, AVG_L, AVG_F, AVG_D every one declares
//     TypeCode.DOUBLE (NumericAggregationValue.java:634-676).
//   - SUM / MIN / MAX → the ARGUMENT's type, exactly (:629-632, :679-687).
//     SUM over an INTEGER column is INTEGER, not BIGINT.
//
// The arms below drive each of those rules SEPARATELY, because they are
// independent: a fix that typed everything BIGINT satisfies the MIN(BIGINT) arm
// and fails AVG and SUM-over-INTEGER; one that always took the argument's type
// satisfies MIN, MAX and SUM and fails AVG and COUNT.
func TestFDB_AggregateOutputTypeCrossesTheDerivedBoundary(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/aggtype"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	// col2 is BIGINT and n32 is INTEGER, so "the argument's type" and "some
	// integer type" are distinguishable — with one integer width the
	// type-preserving rule and a blanket BIGINT rule agree everywhere.
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE aggtype_tmpl "+
			"CREATE TABLE t1 (id BIGINT, col1 BIGINT, col2 BIGINT, n32 INTEGER, d DOUBLE, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE aggtype_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// The corpus's own shape: two groups on col1 (10 and 20), col2 ascending, so
	// MIN over group 10 is 1 and over group 20 is 6 — the `> 5` filter below
	// leaves exactly one row and `+ 4` makes it 10, the corpus's expected value.
	for i := 1; i <= 13; i++ {
		g := 10
		if i > 5 {
			g = 20
		}
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO t1 VALUES (%d, %d, %d, %d, %d.5)", i, g, i, i, i)); err != nil {
			t.Fatalf("INSERT %d: %v", i, err)
		}
	}

	// typeAndRows returns the declared type database/sql reports for the single
	// output column plus the rows. The TYPE is the assertion this file exists
	// for; the rows are the control that stops a type fix from passing while the
	// query stopped answering.
	typeAndRows := func(t *testing.T, q string) (string, []string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatalf("%s: ColumnTypes: %v", q, err)
		}
		if len(cts) != 1 {
			t.Fatalf("%s: %d columns, want 1", q, len(cts))
		}
		var out []string
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s: scan: %v", q, err)
			}
			out = append(out, fmt.Sprint(v))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: iterate: %v", q, err)
		}
		return cts[0].DatabaseTypeName(), out
	}

	for _, tc := range []struct {
		name     string
		sql      string
		wantType string
		wantRows string
		why      string
	}{{
		name:     "the corpus row, verbatim",
		sql:      "SELECT G + 4 FROM (SELECT MIN(x.col2) AS G FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1) AS Y WHERE G > 5",
		wantType: "BIGINT",
		wantRows: "[10]",
		why: "THE DEFECT THIS FILE EXISTS FOR — groupby-tests.yamsql:178. Java " +
			"answers 10 as a Long; with G untyped the addition demoted to Integer.",
	}, {
		name:     "the aliased-grouping-key spelling of the same row",
		sql:      "SELECT G + 4 FROM (SELECT MIN(x.col2) AS G FROM (SELECT col1, col2 FROM t1) AS x GROUP BY x.col1 AS K) AS Y WHERE G > 5",
		wantType: "BIGINT",
		wantRows: "[10]",
		why:      "groupby-tests.yamsql:181. The grouping alias must not change the aggregate's type.",
	}, {
		name:     "MIN over a BIGINT keeps BIGINT",
		sql:      "SELECT G + 1 FROM (SELECT MIN(col2) AS G FROM t1) AS Y",
		wantType: "BIGINT",
		wantRows: "[2]",
		why:      "MIN takes the argument's type (NumericAggregationValue.java:679-682).",
	}, {
		name:     "MAX over a BIGINT keeps BIGINT",
		sql:      "SELECT G + 1 FROM (SELECT MAX(col2) AS G FROM t1) AS Y",
		wantType: "BIGINT",
		wantRows: "[14]",
		why:      "MAX takes the argument's type (:684-687).",
	}, {
		name:     "MIN over an INTEGER keeps INTEGER",
		sql:      "SELECT G + 1 FROM (SELECT MIN(n32) AS G FROM t1) AS Y",
		wantType: "INTEGER",
		wantRows: "[2]",
		why: "THE ARM THAT SEPARATES 'the argument's type' FROM 'BIGINT'. Java's " +
			"MIN_I result TypeCode is INT (:679) and encapsulate's lookup is exact " +
			"— there is no widening step. A fix that typed every integer aggregate " +
			"BIGINT passes every arm above and fails here.",
	}, {
		name:     "SUM over an INTEGER keeps INTEGER",
		sql:      "SELECT G + 1 FROM (SELECT SUM(n32) AS G FROM t1) AS Y",
		wantType: "INTEGER",
		wantRows: "[92]",
		why: "SUM_I's result TypeCode is INT (:629). This is also what keeps the " +
			"int32 overflow lane reachable through a derived table — the same " +
			"property buildDerivedTableSourceFromUnion exists to preserve.",
	}, {
		name:     "MIN over a DOUBLE keeps DOUBLE",
		sql:      "SELECT G + 1 FROM (SELECT MIN(d) AS G FROM t1) AS Y",
		wantType: "DOUBLE",
		wantRows: "[2.5]",
		why:      "MIN_D (:682) — the rule is the argument's type, not 'some integer'.",
	}, {
		name:     "AVG widens to DOUBLE",
		sql:      "SELECT G + 1 FROM (SELECT AVG(col2) AS G FROM t1) AS Y",
		wantType: "DOUBLE",
		wantRows: "[8]",
		why: "AVG IS THE ONE THAT DOES NOT TAKE ITS ARGUMENT'S TYPE — AVG_I, " +
			"AVG_L, AVG_F and AVG_D all declare DOUBLE (:634-676). A fix that " +
			"took the argument's type everywhere would answer BIGINT here.",
	}, {
		name:     "COUNT of a column is BIGINT whatever it counts",
		sql:      "SELECT G + 1 FROM (SELECT COUNT(n32) AS G FROM t1) AS Y",
		wantType: "BIGINT",
		wantRows: "[14]",
		why: "COUNT's two operators both carry TypeCode.LONG and getResultType " +
			"returns it unconditionally (CountValue.java:140-141,241-243). " +
			"Counting an INTEGER column must NOT produce INTEGER, which is what " +
			"an argument-typed rule would do.",
	}, {
		name:     "COUNT(*) is BIGINT across the boundary",
		sql:      "SELECT C + 1 FROM (SELECT COUNT(*) AS C FROM t1) AS Y",
		wantType: "BIGINT",
		wantRows: "[14]",
		why: "COUNT(*) is the one aggregate output that was ALREADY typed before " +
			"this change, in its own arm of aggOutputCols. It is asserted here " +
			"beside COUNT(x) so the two arms cannot drift — they are the same " +
			"Java rule (CountValue's two operators both carry TypeCode.LONG) and " +
			"they were written twenty lines apart.",
	}, {
		name:     "a GROUPING KEY carries its source column's type",
		sql:      "SELECT K + 4 FROM (SELECT col1 AS K, COUNT(*) AS C FROM t1 GROUP BY col1) AS Y WHERE K > 15",
		wantType: "BIGINT",
		wantRows: "[24]",
		why: "Grouping does not change a value's type, and the key was UNKNOWN " +
			"for the same reason the aggregates were. Same demotion, different " +
			"column.",
	}, {
		name:     "an INTEGER grouping key stays INTEGER",
		sql:      "SELECT K + 1 FROM (SELECT n32 AS K, COUNT(*) AS C FROM t1 GROUP BY n32) AS Y WHERE K = 7",
		wantType: "INTEGER",
		wantRows: "[8]",
		why:      "the key's type is the SOURCE column's, not a uniform integer width.",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typ, rows := typeAndRows(t, tc.sql)
			if typ != tc.wantType {
				t.Errorf("%s\n  column type = %q, want %q\n  %s", tc.sql, typ, tc.wantType, tc.why)
			}
			if fmt.Sprint(rows) != tc.wantRows {
				t.Errorf("%s\n  rows = %v, want %s\n  the rows are the control: a type "+
					"fix that stopped the query answering, or that renamed the slot it "+
					"types, changes these", tc.sql, rows, tc.wantRows)
			}
		})
	}

	// A COMPUTED AGGREGATE ARGUMENT IS STILL UNTYPED, and this is a MEASURED
	// NEGATIVE pinned so nobody reads the arms above as covering it.
	//
	// `MIN(col2 + 1)` over a BIGINT column is LONG in Java — the argument
	// expression types LONG and MIN_L returns LONG — so `G + 1` should be
	// BIGINT. It is INTEGER: the SAME demotion the arms above fix, one level
	// deeper. aggregateOutputColumn answers UNKNOWN whenever ac.aggExpr is set,
	// because typing an arbitrary argument expression needs the body's semantic
	// scope, and aggOutputCols runs before that scope exists — the same
	// ordering wall buildDerivedTableSourceFromTerm names where it declines a
	// computed projection outright.
	//
	// It is NOT a missing capability: the innermost projection already reports
	// BIGINT for the same expression (asserted below), so the type is derivable
	// at plan time; only the SCOPE-level derivation is absent. Closing it means
	// building the aggregate body's scope before its output columns are typed,
	// which is a change to when the scope is built and belongs to its own
	// review.
	//
	// OWNER: CQ-102, which carries the ordering change and states why it did not
	// ride along with the typing fix. A residual pin with no owner is an orphan —
	// it records that something is wrong and hands the work to nobody.
	//
	// IF THIS SUBTEST GOES GREEN because the outer arithmetic starts reporting
	// BIGINT, the deeper gap has been closed and this pin should become an
	// ordinary arm of the table above.
	t.Run("a computed aggregate argument is still untyped, and demotes", func(t *testing.T) {
		t.Parallel()
		innerType, innerRows := typeAndRows(t, "SELECT G FROM (SELECT MIN(col2 + 1) AS G FROM t1) AS Y")
		if innerType != "BIGINT" || fmt.Sprint(innerRows) != "[2]" {
			t.Errorf("the INNER read of a computed-argument aggregate = %q %v, want BIGINT [2]\n"+
				"  This half is what says the type is derivable at all. If it moved, the "+
				"claim that only the scope-level derivation is missing needs re-measuring.",
				innerType, innerRows)
		}
		outerType, outerRows := typeAndRows(t, "SELECT G + 1 FROM (SELECT MIN(col2 + 1) AS G FROM t1) AS Y")
		if fmt.Sprint(outerRows) != "[3]" {
			t.Errorf("computed-argument aggregate + 1 = %v, want [3] — the VALUE must be "+
				"right whatever the declared type says", outerRows)
		}
		if outerType != "INTEGER" {
			t.Errorf("computed-argument aggregate + 1 now reports %q, not INTEGER.\n"+
				"  If it reports BIGINT, the scope-level typing of a computed aggregate "+
				"argument has landed — fold this shape into the table above as an "+
				"ordinary arm and delete this pin. Any OTHER value is a third state and "+
				"needs measuring before it is described.", outerType)
		}
	})
}
