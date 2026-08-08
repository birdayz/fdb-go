package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin locates a live
// execution failure by DIFFERENTIAL, and its point is as much what it EXCLUDES
// as what it reproduces.
//
// The shape was escalated as a struct-typed-column defect: "the positional
// merge does not model a struct-typed column", to be fixed by carrying a real
// record type where Go erases one. THE STRUCT IS NOT INVOLVED. Case
// unqualified_scalar_join_projectedExists below fails identically on a plain
// BIGINT, and case unqualified_structRoot_join_projectedExists's qualified twin
// passes on the very same struct column. A fix aimed at the type would have
// left the defect standing while looking principled.
//
// What actually discriminates is the QUALIFIER. Every case here holds three of
// the four factors fixed and moves one:
//
//	qualified   + projected EXISTS + join -> OK      (both types)
//	UNQUALIFIED + projected EXISTS + join -> FAILS   (both types)
//	UNQUALIFIED + projected EXISTS + NO join -> OK
//	UNQUALIFIED + NO projected EXISTS + join -> OK
//	UNQUALIFIED multi-accessor + projected EXISTS + join -> OK
//
// So the failure needs all of: an unqualified reference, a SINGLE accessor, a
// PROJECTED EXISTS, and a JOIN. Drop any one and it goes away. An EXISTS in
// WHERE rather than in the SELECT list is not enough either.
//
// The passing cases are CONTROLS, not decoration: a decline whose control also
// fails is uninterpretable, and each failing case here has a control that
// differs from it in exactly one factor.
//
// Every case asserts the COLUMN VALUES, never a row count — the escalation that
// produced this investigation reported a wrong first column, and a count or an
// order reads identically whether the column is right or wrong.
func TestFDB_UnqualifiedRefBesideAProjectedExistsOverAJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_unqual_pe")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_unqual_pe")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE unqual_pe_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_unqual_pe/s WITH TEMPLATE unqual_pe_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_unqual_pe?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50, 1)), (2, (40, 2)), (3, (30, 3))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (900, 1), (901, 2), (902, 3)")

	const exists = "EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h"
	const join = " FROM t1 JOIN t3 ON t3.t1_id = t1.id"

	// t1_id is a plain BIGINT and belongs only to t3, so it can be written
	// unqualified. n is the struct root. The pairs below differ ONLY in whether
	// the second projected column carries a table qualifier.
	cases := []struct {
		name string
		q    string
		// want is the full rendered result; wantErrField, when non-empty, says
		// the query must instead fail with that field unresolvable.
		want         string
		wantErrField string
	}{
		// --- the two failures, on DIFFERENT column types ---
		{
			name:         "unqualified_scalar_join_projectedExists",
			q:            "SELECT t1.id, t1_id, " + exists + join,
			wantErrField: "T1_ID",
		},
		{
			name:         "unqualified_structRoot_join_projectedExists",
			q:            "SELECT t1.id, n, " + exists + join,
			wantErrField: "N",
		},
		// --- their qualified twins: ONE factor moved, both pass ---
		{
			name: "CONTROL_qualified_scalar_join_projectedExists",
			q:    "SELECT t1.id, t3.t1_id, " + exists + join,
			want: "[[1 1 true] [2 2 false] [3 3 true]]",
		},
		{
			name: "CONTROL_qualified_structRoot_join_projectedExists",
			q:    "SELECT t1.id, t1.n, " + exists + join,
			want: "[[1 struct[50 1] true] [2 struct[40 2] false] [3 struct[30 3] true]]",
		},
		// --- drop the projected EXISTS: both pass unqualified ---
		{
			name: "CONTROL_unqualified_scalar_join_noExists",
			q:    "SELECT t1.id, t1_id" + join,
			want: "[[1 1] [2 2] [3 3]]",
		},
		{
			name: "CONTROL_unqualified_structRoot_join_noExists",
			q:    "SELECT t1.id, n" + join,
			want: "[[1 struct[50 1]] [2 struct[40 2]] [3 struct[30 3]]]",
		},
		// A third projected column that is NOT an EXISTS also passes, so it is
		// the EXISTS and not the arity of the SELECT list.
		{
			name: "CONTROL_unqualified_structRoot_join_thirdColumnNotExists",
			q:    "SELECT t1.id, n, 1 AS h" + join,
			want: "[[1 struct[50 1] 1] [2 struct[40 2] 1] [3 struct[30 3] 1]]",
		},
		// An EXISTS in WHERE rather than projected also passes, so it is the
		// PROJECTION of the EXISTS that matters, not its presence.
		{
			name: "CONTROL_unqualified_structRoot_join_existsInWhereNotProjected",
			q: "SELECT t1.id, n" + join +
				" WHERE EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id)",
			want: "[[1 struct[50 1]] [3 struct[30 3]]]",
		},
		// --- drop the join: passes unqualified ---
		{
			name: "CONTROL_unqualified_structRoot_noJoin_projectedExists",
			q:    "SELECT t1.id, n, " + exists + " FROM t1",
			want: "[[1 struct[50 1] true] [2 struct[40 2] false] [3 struct[30 3] true]]",
		},
		// --- a MULTI-ACCESSOR unqualified reference passes, which is why the
		// failure is specific to a SINGLE accessor and not to unqualified
		// references in general ---
		{
			name: "CONTROL_unqualified_structMember_join_projectedExists",
			q:    "SELECT t1.id, n.sk, " + exists + join,
			want: "[[1 50 true] [2 40 false] [3 30 true]]",
		},
		// Position in the SELECT list is not the discriminator.
		{
			name:         "unqualified_structRoot_join_projectedExists_structLast",
			q:            "SELECT t1.id, " + exists + ", n" + join,
			wantErrField: "N",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := db.QueryContext(ctx, tc.q)
			if tc.wantErrField != "" {
				if err == nil {
					rows.Close()
					t.Fatalf("%q now executes. If the ordinal for an unqualified "+
						"reference is now baked over a join's merged row, replace "+
						"this arm with the values its qualified twin already "+
						"returns", tc.q)
				}
				wantMsg := fmt.Sprintf("field %q not resolvable in the runtime row (ordinal -1", tc.wantErrField)
				if !strings.Contains(err.Error(), wantMsg) {
					t.Fatalf("expected %s, got: %v — a DIFFERENT failure means this "+
						"shape changed for another reason and the differential "+
						"recorded here no longer describes it", wantMsg, err)
				}
				// The merged runtime row CARRIES the column the reference names.
				// This is not a missing column; the reference arrived unbaked and
				// evaluateOrdinal has no name fallback. Pinning the row listing
				// keeps the diagnosis attached to the failure it describes.
				if !strings.Contains(err.Error(), "row columns [ID T1_ID ID N]") {
					t.Fatalf("expected the merged row [ID T1_ID ID N], which CONTAINS "+
						"the named column — that containment is what makes this an "+
						"ordinal-baking failure rather than a resolution one. Got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CONTROL %q failed: %v — a failing control makes the "+
					"paired failure above uninterpretable", tc.q, err)
			}
			defer rows.Close()
			cols, err := rows.Columns()
			if err != nil {
				t.Fatalf("columns %q: %v", tc.q, err)
			}
			var out []string
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatalf("scan %q: %v", tc.q, err)
				}
				r := make([]string, len(vals))
				for i, v := range vals {
					if s, ok := v.(interface{ Attributes() []any }); ok {
						r[i] = fmt.Sprintf("struct%v", s.Attributes())
						continue
					}
					r[i] = fmt.Sprintf("%v", v)
				}
				out = append(out, fmt.Sprint(r))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows %q: %v", tc.q, err)
			}
			if len(out) == 0 {
				t.Fatalf("CONTROL %q returned ZERO rows; the value assertion "+
					"below would hold vacuously", tc.q)
			}
			if fmt.Sprint(out) != tc.want {
				t.Fatalf("query %q =\n  %v\nwant\n  %v", tc.q, fmt.Sprint(out), tc.want)
			}
		})
	}
}
