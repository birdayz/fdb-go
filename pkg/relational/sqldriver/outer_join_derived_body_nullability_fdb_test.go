package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// A derived table whose BODY is an OUTER join types its null-supplying leg's
// columns NULLABLE, even where the catalog declares them NOT NULL — and leaves
// the PRESERVED leg's alone.
//
// The derivation that types `FROM (SELECT … FROM a LEFT JOIN b …) AS d` reads
// each body leg's columns off the CATALOG and never looked at the join flavour.
// That is a STRUCTURAL answer to a question this engine answers ALGEBRAICALLY
// everywhere else: an outer join pads its null-supplying side, so that side's
// columns are nullable in the body's output no matter what the base table says.
// The physical side already knows this — ordinal_seed.go wraps a null-supplying
// leg's column types with WithNullability(true), citing Java's
// pullUpResultColumnsWithNullability — and the two sides must agree, because
// this derivation is what adjudicates the outer references against the row that
// side produces.
//
// THE REACHABLE SURFACE IS EXACTLY ONE COLUMN SHAPE, and that is a measured
// narrowing rather than a caveat. This DDL REFUSES `NOT NULL` on a scalar
// column outright:
//
//	CREATE TABLE dept (did BIGINT, dbudget BIGINT NOT NULL, …)
//	  -> 0A000: NOT NULL is only allowed for ARRAY column type
//
// and every scalar column — PRIMARY KEY columns included, measured — arrives
// from the catalog already nullable. So the shape usually reached for to
// describe this defect (`SELECT a.x, b.y FROM a LEFT JOIN b`, `d.y` typed NOT
// NULL for a scalar `y`) CANNOT be built here: `d.y` is nullable before the fix
// and after it. An ARRAY column declared NOT NULL is the only column this
// derivation can carry a false NOT NULL for, and it is what this test uses.
//
// WHY THE SUITE WAS BLIND. Every shape in the sibling derived-table tests —
// ambiguous_column_ref_rejected_fdb_test.go, join_ordinalization_boundary_test.go,
// explain_unplannable_fdb_test.go — uses a comma join or an INNER join. None can
// express an outer-join nullability defect, so the dimension was unprobed rather
// than under-probed.
func TestFDB_OuterJoinDerivedBodyNullability(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/outer_derived_null"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	// BOTH legs carry an ARRAY NOT NULL column, because the two directions of
	// this fix need separate witnesses: the null-supplying leg's must become
	// nullable, and the preserved leg's must NOT. A fix that marks every leg
	// nullable satisfies the first and destroys the second.
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE outer_derived_tmpl"+
		" CREATE TABLE emp (eid BIGINT, etags BIGINT ARRAY NOT NULL, PRIMARY KEY (eid))"+
		" CREATE TABLE dept (did BIGINT, dtags BIGINT ARRAY NOT NULL, PRIMARY KEY (did))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE outer_derived_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		// emp 1 and 2 have a matching dept; emp 3 does NOT. Row 3 is the whole
		// point — it is the only row where the padded NULL exists.
		"INSERT INTO emp VALUES (1, [11]), (2, [22]), (3, [33])",
		"INSERT INTO dept VALUES (1, [1]), (2, [2])",
	} {
		if _, e := db.ExecContext(ctx, s); e != nil {
			t.Fatalf("seed %q: %v", s, e)
		}
	}

	const body = "SELECT a.eid, a.etags, b.did, b.dtags FROM emp AS a LEFT JOIN dept AS b ON b.did = a.eid"

	// FIXTURE GUARD, and it carries the whole test. The claim is that a NOT NULL
	// column becomes nullable through an outer join, so the column has to be NOT
	// NULL somewhere first. Read it off the BASE table: if `dept.dtags` ever
	// stops reporting NOT NULL here, the derived assertion below is satisfied by
	// a column that was already nullable and proves nothing.
	t.Run("rows", func(t *testing.T) {
		t.Parallel()
		rows, qerr := db.QueryContext(ctx, "SELECT d.eid, d.did FROM ("+body+") AS d ORDER BY d.eid")
		if qerr != nil {
			t.Fatalf("query: %v", qerr)
		}
		defer rows.Close()
		type row struct {
			eid int64
			did sql.NullInt64
		}
		var got []row
		for rows.Next() {
			var r row
			if serr := rows.Scan(&r.eid, &r.did); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			got = append(got, r)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		want := []row{
			{1, sql.NullInt64{Int64: 1, Valid: true}},
			{2, sql.NullInt64{Int64: 2, Valid: true}},
			{3, sql.NullInt64{}},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d = %v, want %v — the LEFT JOIN's unmatched row must carry "+
					"NULL in the right leg's columns", i, got[i], want[i])
			}
		}
	})

	t.Run("IS NULL over the null-supplying leg", func(t *testing.T) {
		t.Parallel()
		// The consumer the declared type feeds: columnCascadesType carries it
		// into FieldValue.Typ, and the IS NULL probe answers from
		// ExpectedType.IsNullable(). The unmatched row is the only place the two
		// answers can differ.
		for _, tc := range []struct {
			name  string
			query string
			want  []int64
		}{
			{
				"IS NULL selects the unmatched row",
				"SELECT d.eid FROM (" + body + ") AS d WHERE d.dtags IS NULL ORDER BY d.eid",
				[]int64{3},
			},
			{
				"IS NOT NULL selects its complement",
				"SELECT d.eid FROM (" + body + ") AS d WHERE d.dtags IS NOT NULL ORDER BY d.eid",
				[]int64{1, 2},
			},
			{
				// The other direction, read through ROWS rather than metadata:
				// the preserved leg's NOT NULL column must still match nothing.
				"the preserved leg's NOT NULL column is never null",
				"SELECT d.eid FROM (" + body + ") AS d WHERE d.etags IS NULL",
				nil,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got := queryInt64Column(t, ctx, db, tc.query)
				if len(got) != len(tc.want) {
					t.Fatalf("%s\n  got %v, want %v", tc.query, got, tc.want)
				}
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("%s\n  got %v, want %v", tc.query, got, tc.want)
					}
				}
			})
		}
	})
}

// TestFDB_DerivedJoinBodyRowAgreesWithThePlanner cross-checks the TWO
// implementations of "what row does this subtree emit".
//
// The semantic derivation for a join-bodied derived table builds its own column
// list, and that list supplies BOTH the 42702 ambiguity verdict AND the ordinal
// each `d.<col>` reads. The planner independently builds the leg-concat the
// executor actually produces. Nothing checked that the two agree; the agreement
// was load-bearing on inspection alone.
//
// A disagreement in WIDTH, ORDER or NAMES is a wrong-column read: `d.x` would
// resolve to an ordinal holding a different column of the same type, which is
// the exact failure mode this line of work exists to remove.
//
// The check is observable from the driver without reaching into either
// implementation: `SELECT *` over the derived table renders the PLANNER's
// concat (names via rows.Columns(), values in its order), while `SELECT d.<name>`
// resolves through the SEMANTIC row. Reading the same body both ways and
// comparing position-for-position compares the two lists.
//
// The legs carry DISJOINT column names deliberately — with duplicates every
// qualified read is refused 42702 and the comparison could not be made at all.
func TestFDB_DerivedJoinBodyRowAgreesWithThePlanner(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/derived_body_agree"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE derived_agree_tmpl"+
		" CREATE TABLE lft (lid BIGINT, lv BIGINT, lw BIGINT, PRIMARY KEY (lid))"+
		" CREATE TABLE rgt (rid BIGINT, rv BIGINT, PRIMARY KEY (rid))"); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE derived_agree_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		// Every value distinct, so a column read off the wrong ordinal cannot
		// coincide with the right one.
		"INSERT INTO lft VALUES (1, 11, 111)",
		"INSERT INTO rgt VALUES (1, 1111)",
	} {
		if _, e := db.ExecContext(ctx, s); e != nil {
			t.Fatalf("seed %q: %v", s, e)
		}
	}

	for _, body := range []struct{ name, sql string }{
		{"comma body", "SELECT * FROM lft AS x, rgt AS y WHERE y.rid = x.lid"},
		{"explicit JOIN body", "SELECT * FROM lft AS x JOIN rgt AS y ON y.rid = x.lid"},
	} {
		t.Run(body.name, func(t *testing.T) {
			t.Parallel()
			// THE PLANNER'S LEG-CONCAT, as the executor emits it.
			rows, qerr := db.QueryContext(ctx, "SELECT * FROM ("+body.sql+") AS d")
			if qerr != nil {
				t.Fatalf("star query: %v", qerr)
			}
			cols, cerr := rows.Columns()
			if cerr != nil {
				rows.Close()
				t.Fatalf("columns: %v", cerr)
			}
			if !rows.Next() {
				rows.Close()
				t.Fatalf("the star query returned NO rows, so nothing is being compared — "+
					"an empty population reports this test green with both lists unread (%v)", cols)
			}
			starVals := make([]int64, len(cols))
			ptrs := make([]any, len(cols))
			for i := range starVals {
				ptrs[i] = &starVals[i]
			}
			if serr := rows.Scan(ptrs...); serr != nil {
				rows.Close()
				t.Fatalf("scan: %v", serr)
			}
			if rerr := rows.Err(); rerr != nil {
				rows.Close()
				t.Fatalf("rows: %v", rerr)
			}
			rows.Close()

			// WIDTH. The body is two whole tables concatenated; anything else
			// means one of the two derivations dropped or duplicated a leg.
			if len(cols) != 5 {
				t.Fatalf("the derived table emitted %d columns %v, want 5 (lft's 3 + rgt's 2) — "+
					"the two derivations disagree about the WIDTH of the row, and every "+
					"ordinal past the disagreement reads a different column", len(cols), cols)
			}
			// ORDER and NAMES. Star expansion is FROM order, leg by leg.
			want := []string{"LID", "LV", "LW", "RID", "RV"}
			for i := range want {
				if cols[i] != want[i] {
					t.Fatalf("star column %d is %q, want %q (full: %v) — star expansion is "+
						"leg-by-leg in FROM order and the semantic derivation builds its list "+
						"the same way; a disagreement here is a wrong-column read", i, cols[i], want[i], cols)
				}
			}

			// AGREEMENT. Each column read BY NAME through the semantic row must
			// return the value the planner's concat put at that position. If the
			// semantic list is a permutation of the planner's, or a different
			// width, some name lands on the wrong ordinal and the values diverge.
			for i, name := range cols {
				var got int64
				q := "SELECT d." + name + " FROM (" + body.sql + ") AS d"
				if serr := db.QueryRowContext(ctx, q).Scan(&got); serr != nil {
					t.Fatalf("%s: %v — the semantic derivation could not resolve a column "+
						"the planner's row emits, so the two lists already disagree", q, serr)
				}
				if got != starVals[i] {
					t.Fatalf("d.%s read %d, but the planner's row holds %d at position %d "+
						"(star row %v).\n  The semantic derivation and the planner's leg-concat "+
						"disagree about which slot this name names — a wrong-column read.",
						name, got, starVals[i], i, starVals)
				}
			}
		})
	}
}

// queryInt64Column runs a single-column BIGINT query and returns its rows.
func queryInt64Column(t *testing.T, ctx context.Context, db *sql.DB, query string) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if serr := rows.Scan(&v); serr != nil {
			t.Fatalf("%s: scan: %v", query, serr)
		}
		out = append(out, v)
	}
	if rerr := rows.Err(); rerr != nil {
		t.Fatalf("%s: rows: %v", query, rerr)
	}
	return out
}
