package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// TestFDB_GroupByNestedPathGroupsPerPath is the RFC-229 tripwire, FLIPPED to
// the assertions its own failure message named once nested-path GROUP BY
// landed. It is the same fixture and the same data; only the verdict moved,
// because the thing it was watching for happened.
//
// WHAT IT WATCHES, and it is not "nested GROUP BY works" — the standalone
// acceptance file (groupby_nested_path_key_fdb_test.go) owns that. This file
// owns the COLLAPSE: two members of ONE struct root used to share one output
// name. The resolver FUSES a nested reference — `n.sk` becomes ONE
// FieldValue{Field:"N", Resolved:[N,SK]} — and AggregateKeyColumnName, the
// single naming authority for a grouping key, rendered the flat ROOT
// (strings.ToUpper(fv.Field)), so it answered "N" for `n.sk` AND for `n.co`.
// The translator keys grouping columns by that name and the later key
// overwrites the earlier, so `GROUP BY n.sk, n.co` would have collapsed to ONE
// grouping column and returned TWO groups where four exist.
//
// That is why RFC-229 §2.3 converted AggregateKeyColumnName, its mirror
// aggregateGroupKeyOutputName and the ColumnDef mirror to the RESOLVED PATH
// BEFORE the feature was implemented rather than alongside it: converting
// afterwards ships the collapse, and its symptom — missing groups — is silent.
// The conversion is pinned as a unit at
// expressions/group_by_naming_test.go:TestAggregateKeyColumnName_NestedKeyTakesTheResolvedPath;
// what THIS file adds is the end-to-end consequence, on data seeded so that a
// collapsed key returns a number the assertion can name.
//
// THE COUNT IS THE POINT. Four (sk, co) pairs over five rows, with sk alone
// taking two values and co alone taking two: 4 groups is correct, 2 is the
// collapse, and the single-key controls beside it say the 4 came from two
// independent keys rather than from one key with four values.
//
// The ORDER BY assertion is kept from the tripwire era. It used to state the
// asymmetry that made GROUP BY the odd clause out; it now guards the other
// direction — a nested path must still work in ORDER BY, and a regression that
// took the whole descent away would otherwise show up only as this file's
// GROUP BY arms failing, which reads as a group-key defect.
func TestFDB_GroupByNestedPathGroupsPerPath(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/gnk"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA TEMPLATE gnk_tmpl "+
			"CREATE TYPE AS STRUCT gst (sk BIGINT, co BIGINT) "+
			"CREATE TABLE t1 (id BIGINT, n gst, PRIMARY KEY (id)) "+
			// t2 differs from t1 in ONE way: it also declares a FLAT column
			// whose name equals the struct member's LEAF. That single
			// difference is what used to defeat the leaf-based refusal, and it
			// is now what makes a wrong-column read visible: the flat values
			// are disjoint from the struct's, so reading `sk` where `n.sk` was
			// asked for changes the group COUNT and the group VALUES.
			"CREATE TABLE t2 (id BIGINT, sk BIGINT, n gst, PRIMARY KEY (id))"); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := setup.ExecContext(ctx,
		"CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE gnk_tmpl"); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// (sk, co) pairs (1,1) (1,2) (2,1) (2,2) (1,1): four distinct pairs over
	// five rows, two distinct sk, two distinct co. The repeated (1,1) is what
	// makes "4 groups" a statement about grouping rather than about row count.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t1 VALUES (1, (1, 1)), (2, (1, 2)), (3, (2, 1)), (4, (2, 2)), (5, (1, 1))"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	// t2 MUST be non-empty. The defect this table controls for surfaced from
	// the EXECUTOR, so an empty table hides it completely: the plan is built,
	// no row is ever evaluated, and the query returns zero rows with no error —
	// a green that proves only that the table was empty.
	if _, err := db.ExecContext(ctx,
		"INSERT INTO t2 VALUES (1, 90, (1, 1)), (2, 91, (1, 2)), (3, 92, (2, 1))"); err != nil {
		t.Fatalf("INSERT t2: %v", err)
	}

	// A nested path in ORDER BY works — kept from the tripwire era so a
	// regression that removed nested descent WHOLESALE is distinguishable from
	// one that removed it from grouping alone.
	rows, err := db.QueryContext(ctx, "SELECT id FROM t1 ORDER BY n.sk, n.co")
	if err != nil {
		t.Fatalf("ORDER BY over a nested path stopped working: %v\n"+
			"  The GROUP BY arms below will fail too, and they would read as a "+
			"group-key defect. The descent itself is gone.", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("ORDER BY nested path iterate: %v", err)
	}
	if n != 5 {
		t.Fatalf("ORDER BY over a nested path returned %d rows, want 5", n)
	}

	groups := func(t *testing.T, q string, cols int) [][]int64 {
		t.Helper()
		rs, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rs.Close()
		var out [][]int64
		for rs.Next() {
			row := make([]int64, cols)
			ptrs := make([]any, cols)
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, row)
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("rows %q: %v", q, err)
		}
		return out
	}

	t.Run("two_members_of_one_struct_root_are_two_keys", func(t *testing.T) {
		q := "SELECT n.sk, n.co, COUNT(*) FROM t1 GROUP BY n.sk, n.co"
		got := groups(t, q, 3)
		if len(got) != 4 {
			t.Fatalf("`%s` returned %d groups, want 4.\n"+
				"  rows: %v\n"+
				"  TWO groups is the COLLAPSE this file exists for: both keys "+
				"named by their shared struct ROOT \"N\", the name map last-wins, "+
				"and the second key overwrites the first. Check that no authority "+
				"has RE-DERIVED a group-key name from fv.Field — the unit pin at "+
				"expressions/group_by_naming_test.go covers three authorities, not "+
				"a fourth that a new feature might add.", q, len(got), got)
		}
		if fmt.Sprint(got) != "[[1 1 2] [1 2 1] [2 1 1] [2 2 1]]" {
			t.Fatalf("`%s` grouped into the wrong four groups.\n  got:  %v\n"+
				"  want: [[1 1 2] [1 2 1] [2 1 1] [2 2 1]]", q, got)
		}
	})

	t.Run("each_member_alone_groups_by_itself", func(t *testing.T) {
		// The controls. Four groups above must come from TWO independent keys,
		// not from one key that happens to take four values.
		q := "SELECT n.sk, COUNT(*) FROM t1 GROUP BY n.sk"
		if got := groups(t, q, 2); fmt.Sprint(got) != "[[1 3] [2 2]]" {
			t.Fatalf("`%s` = %v, want [[1 3] [2 2]]", q, got)
		}
		q = "SELECT n.co, COUNT(*) FROM t1 GROUP BY n.co"
		if got := groups(t, q, 2); fmt.Sprint(got) != "[[1 3] [2 2]]" {
			t.Fatalf("`%s` = %v, want [[1 3] [2 2]]", q, got)
		}
	})

	t.Run("a_flat_column_sharing_the_leaf_name_is_not_the_key", func(t *testing.T) {
		// t2 declares BOTH a flat `sk` and `n.sk`. This query used to escape
		// the leaf-based refusal on the strength of the unrelated flat column
		// and die in the executor; the hazard now is the opposite one — the
		// nested key silently reading the flat column that shares its leaf.
		// The flat values (90, 91, 92) are disjoint from the struct's (1, 2),
		// so the two readings differ in COUNT and in VALUE, not by a coincidence.
		q := "SELECT n.sk, COUNT(*) FROM t2 GROUP BY n.sk"
		got := groups(t, q, 2)
		if fmt.Sprint(got) != "[[1 2] [2 1]]" {
			t.Fatalf("`%s` = %v, want [[1 2] [2 1]].\n"+
				"  THREE groups of 90/91/92 means the key read the FLAT column "+
				"`sk` that shares the path's leaf name instead of the struct "+
				"member — a silent wrong-column read, which is why this table "+
				"declares both.", q, got)
		}
		// The control: the flat column still groups as itself.
		q = "SELECT sk, COUNT(*) FROM t2 GROUP BY sk"
		if g := groups(t, q, 2); fmt.Sprint(g) != "[[90 1] [91 1] [92 1]]" {
			t.Fatalf("`%s` = %v, want [[90 1] [91 1] [92 1]] — the nested key's "+
				"arrival must not have captured the flat column's slot", q, g)
		}
		// Both in one query: two keys whose leaves collide, which is the shape
		// that would re-create the collision in the name map.
		q = "SELECT sk, n.sk, COUNT(*) FROM t2 GROUP BY sk, n.sk"
		if g := groups(t, q, 3); fmt.Sprint(g) != "[[90 1 1] [91 1 1] [92 2 1]]" {
			t.Fatalf("`%s` = %v, want [[90 1 1] [91 1 1] [92 2 1]]", q, g)
		}
	})
}
