package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// A NESTED path in the ON clause of an OUTER third leg — `m.n.sk = r.n.sk` over
// `l JOIN m ... LEFT JOIN r ...` — used to fail loud with "no frontier row
// resolved" (the multi-leg positional row's correct-or-loud guard).
//
// The gated join's predicate bake identifies a leg reference by legRef, which
// excluded every MULTI-ACCESSOR path on the reading that only the bake's own
// machinery mints one. That stopped being true when a user-written nested
// descent started arriving as ONE unpinned FieldValue with the descent in its
// remaining accessors: the reference was declined, kept its leg correlation,
// nothing bound that correlation at execution, and the read fell through to the
// multi-leg row. Machinery-ownership is the FRONTIER PIN, not the arity.
//
// THE ARMS ARE A GRID, NOT A REPRODUCER, and it asserts VALUES rather than the
// absence of an error, because the two ways this can be wrong are opposite:
//   - admitting the reference without fusing its suffix bakes the address of the
//     enclosing STRUCT and drops the descent. That address is a real leg column,
//     so nothing rejects it — the predicate compares a BIGINT against a whole
//     struct and quietly matches nothing. WRONG ROWS, not an error.
//   - declining it again restores the loud failure.
//
// A–E are the arms that already worked and they pin the no-regression half:
// two-way (nested, inner and outer), three-way inner, three-way outer FLAT, and
// three-way outer nested on the PRESERVED side only. F–H are the three that
// failed — the defect needs three legs, an OUTER third leg, AND a nested path
// reading the multi-leg side.
func TestFDB_OuterMultilegNestedOnPredicate(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_omlnon")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_omlnon")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE omlnon_tmpl "+
		"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE nt(id BIGINT, sk BIGINT, n gst, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_omlnon/s WITH TEMPLATE omlnon_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_omlnon?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// n.sk DELIBERATELY collides across rows 1 and 2 while the flat sk does not:
	// the nested key must produce a DIFFERENT multiplicity from the flat one, so
	// a descent that silently reads the struct root (or the wrong column) cannot
	// coincide with the right answer.
	mustExec(t, db, ctx, "INSERT INTO nt VALUES (1, 10, (1, 7)), (2, 20, (1, 8)), (3, 30, (2, 9))")

	run := func(q string) ([]string, error) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v sql.NullInt64
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			if v.Valid {
				out = append(out, fmt.Sprint(v.Int64))
			} else {
				out = append(out, "NULL")
			}
		}
		return out, rows.Err()
	}

	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		// ---- A–E: already correct before the fix; they pin that it moved nothing.
		{
			"A_two-way-inner-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
		},
		{
			"B_two-way-outer-nested",
			"SELECT l.id FROM nt AS l LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
		},
		{
			"C_three-way-inner-nested",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
		},
		{
			// The flat twin of F. Same three legs, same outer third leg, ONE
			// accessor — this is the arm the old arity gate admitted, and its
			// row set differs from F's, which is what makes F's assertion a
			// statement about the descent rather than about the join shape.
			"D_three-way-outer-flat",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.sk = r.sk ORDER BY l.id",
			[]string{"1", "2", "3"},
		},
		{
			// Nested on the NULL-supplied side only: no reference reads the
			// multi-leg side, so the bake was never asked.
			"E_three-way-outer-nested-preserved-only",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "2", "3"},
		},

		// ---- F–H: the three arms that failed loud. m.n.sk is 1,1,2 over
		// m.id 1,2,3, so a correct descent multiplies rows 1 and 2 by two.
		{
			"F_three-way-outer-nested-both",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
		},
		{
			// Nested on the MULTI-LEG side only, against a flat r.sk (10/20/30)
			// no n.sk can equal — every row is NULL-supplied exactly once. A
			// dropped descent would compare the struct against r.sk, which also
			// matches nothing, so this arm alone cannot separate the two; it is
			// here because it is a distinct failing shape, and F/H carry the
			// discrimination.
			"G_three-way-outer-nested-multileg-only",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON m.n.sk = r.sk ORDER BY l.id",
			[]string{"1", "2", "3"},
		},
		{
			"H_three-way-outer-nested-first-leg",
			"SELECT l.id FROM nt AS l JOIN nt AS m ON l.id = m.id LEFT JOIN nt AS r ON l.n.sk = r.n.sk ORDER BY l.id",
			[]string{"1", "1", "2", "2", "3"},
		},
	} {
		got, err := run(tc.query)
		if err != nil {
			t.Errorf("%s: %s\n\tunexpected error: %v\n\twant rows %v", tc.name, tc.query, err, tc.want)
			continue
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: %s\n\t got %v\n\twant %v\n"+
				"A row set that is neither the error nor the expected rows is the SILENT half of "+
				"this defect: the nested descent was dropped and the predicate compared against "+
				"the enclosing struct.", tc.name, tc.query, got, tc.want)
		}
	}
}
