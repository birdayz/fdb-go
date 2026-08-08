package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestFDB_ProjectedStructColumnThroughAJoin pins what a projected struct-typed
// column does when it flows through an inner join's merged row.
//
// It exists because a report claimed TWO defects here and only ONE of them is
// real. The refuted half is pinned as hard as the real one, because a negative
// result is what classifies the other half as a contained capability gap rather
// than a live wrong-answer bug, and nothing else in the tree states it.
//
// REFUTED — `SELECT t1.id, n FROM t1 JOIN t3 ON ...` was reported to return
// "rows with the FIRST column 0 instead of the ids". It does not. It returns
// ids 1,2,3 with the struct intact, identical to the unjoined control. The
// assertions below pin the ID COLUMN'S VALUES and the STRUCT'S MEMBER VALUES,
// in order. The alleged defect is a wrong COLUMN, and a row COUNT reads
// identically whether the column is right or wrong. Order is asserted too, since
// these fixtures are deterministic; the claim is that a count ALONE is
// insufficient, not that order goes unchecked.
//
// CONFIRMED — adding a projected EXISTS to that same SELECT list makes the
// query fail to execute. That half is a tripwire on the current loud failure.
//
// t3 holds exactly one row per t1 id, so the join is row-preserving: every
// projected value must equal the unjoined form, and the unjoined queries are
// run as controls rather than assumed.
func TestFDB_ProjectedStructColumnThroughAJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()

	setup := openTestDB(t, "/testdb_structcol_join")
	mustExec(t, setup, ctx, "CREATE DATABASE /testdb_structcol_join")
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE structcol_join_tmpl "+
		"CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT) "+
		"CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id)) "+
		"CREATE TABLE t2(id BIGINT, t1_id BIGINT, PRIMARY KEY(id)) "+
		"CREATE TABLE t3(id BIGINT, t1_id BIGINT, PRIMARY KEY(id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA /testdb_structcol_join/s WITH TEMPLATE structcol_join_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///testdb_structcol_join?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// n.sk runs OPPOSITE to id and n.co runs WITH it, so reading the wrong
	// struct member, or the wrong column entirely, is visible rather than tied.
	mustExec(t, db, ctx, "INSERT INTO t1 VALUES (1, (50, 1)), (2, (40, 2)), (3, (30, 3))")
	mustExec(t, db, ctx, "INSERT INTO t2 VALUES (100, 1), (200, 3)")
	mustExec(t, db, ctx, "INSERT INTO t3 VALUES (900, 1), (901, 2), (902, 3)")

	// idAndStruct reads a two-column (BIGINT, struct) result into the id and the
	// struct's flattened members, so an assertion can name a MEMBER value.
	idAndStruct := func(t *testing.T, q string) ([]int64, [][]any) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var ids []int64
		var structs [][]any
		for rows.Next() {
			var id int64
			var raw any
			if err := rows.Scan(&id, &raw); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			ids = append(ids, id)
			s, ok := raw.(interface{ Attributes() []any })
			if !ok {
				t.Fatalf("query %q: second column is %T, not a struct — the struct "+
					"column did not survive the projection", q, raw)
			}
			structs = append(structs, s.Attributes())
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		if len(ids) == 0 {
			t.Fatalf("query %q returned ZERO rows; every assertion below would "+
				"hold vacuously", q)
		}
		return ids, structs
	}

	wantIDs := []int64{1, 2, 3}
	wantMembers := [][]any{{int64(50), int64(1)}, {int64(40), int64(2)}, {int64(30), int64(3)}}

	check := func(t *testing.T, q string, ids []int64, structs [][]any) {
		t.Helper()
		if fmt.Sprint(ids) != fmt.Sprint(wantIDs) {
			t.Fatalf("query %q: ID column = %v, want %v. The ID column is the one "+
				"the report claimed comes back 0 through a join", q, ids, wantIDs)
		}
		if fmt.Sprint(structs) != fmt.Sprint(wantMembers) {
			t.Fatalf("query %q: struct members = %v, want %v. A correct ID column "+
				"with a wrong struct payload is the same defect one column over",
				q, structs, wantMembers)
		}
	}

	t.Run("control_no_join", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, n FROM t1"
		ids, structs := idAndStruct(t, q)
		check(t, q, ids, structs)
	})

	// THE REFUTED HALF. This is the exact query reported to return a first
	// column of 0. If it ever does, this reddens and the report becomes true.
	t.Run("struct_root_projected_through_a_join_is_CORRECT", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, n FROM t1 JOIN t3 ON t3.t1_id = t1.id"
		ids, structs := idAndStruct(t, q)
		check(t, q, ids, structs)
	})

	t.Run("struct_root_projected_through_a_join_is_CORRECT_when_qualified", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, t1.n FROM t1 JOIN t3 ON t3.t1_id = t1.id"
		ids, structs := idAndStruct(t, q)
		check(t, q, ids, structs)
	})

	// A struct MEMBER through the same join also works, and its values are the
	// ones the struct-root arms above must agree with.
	t.Run("struct_member_through_a_join_is_CORRECT", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, n.sk FROM t1 JOIN t3 ON t3.t1_id = t1.id"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var got [][2]int64
		for rows.Next() {
			var id, sk int64
			if err := rows.Scan(&id, &sk); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			got = append(got, [2]int64{id, sk})
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		want := [][2]int64{{1, 50}, {2, 40}, {3, 30}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("query %q: (id, n.sk) = %v, want %v", q, got, want)
		}
	})

	// THE CONFIRMED HALF, as a tripwire on the CURRENT loud failure. The
	// control immediately below is not decoration: it is the same SELECT list
	// without the join, and it passes, so a failure here is attributable to the
	// join's merged row rather than to projecting a struct beside an EXISTS.
	t.Run("control_struct_and_projected_exists_without_a_join", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1"
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("CONTROL query %q failed: %v — the tripwire below is "+
				"uninterpretable while its control is red", q, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id int64
			var raw any
			var h bool
			if err := rows.Scan(&id, &raw, &h); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			s, ok := raw.(interface{ Attributes() []any })
			if !ok {
				t.Fatalf("query %q: struct column is %T", q, raw)
			}
			got = append(got, fmt.Sprint(id, s.Attributes(), h))
		}
		want := "[1 [50 1] true 2 [40 2] false 3 [30 3] true]"
		if fmt.Sprint(got) != want {
			t.Fatalf("CONTROL %q = %v, want %v", q, got, want)
		}
	})

	t.Run("struct_root_beside_a_projected_exists_over_a_join_still_fails", func(t *testing.T) {
		t.Parallel()
		q := "SELECT t1.id, n, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h " +
			"FROM t1 JOIN t3 ON t3.t1_id = t1.id"
		rows, err := db.QueryContext(ctx, q)
		if err == nil {
			rows.Close()
			t.Fatalf("%q now executes. Replace this tripwire with the row "+
				"assertion: ids [1 2 3], struct members [[50 1] [40 2] [30 3]], "+
				"h [true false true] — the values its no-join control already "+
				"produces", q)
		}
		if !strings.Contains(err.Error(), "not resolvable in the runtime row") {
			t.Fatalf("expected the unresolvable-struct-column failure, got: %v — a "+
				"DIFFERENT failure means this shape changed for another reason and "+
				"the diagnosis recorded here no longer describes it", err)
		}
		// The runtime row DOES carry a column named N. The failure is therefore
		// not "a name the row does not answer to"; the reference arrived unbaked
		// (ordinal -1) and the name path did not recover it.
		if !strings.Contains(err.Error(), "row columns [ID T1_ID ID N]") {
			t.Fatalf("expected the merged row to be reported as [ID T1_ID ID N] — "+
				"the struct column IS present in it, which is what makes this an "+
				"ordinal-resolution failure rather than a missing column. Got: %v", err)
		}
	})
}
