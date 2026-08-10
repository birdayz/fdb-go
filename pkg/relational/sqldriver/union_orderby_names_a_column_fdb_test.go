package sqldriver_test

// An ORDER BY over a UNION NAMES a result column. That is what makes the
// validation in validateUnionOrderByColumns name-keyed, and it is why the
// RFC-197 debt entry on that function cannot be closed by rewriting the check
// to compare POSITIONS instead.
//
// The name is the only thing the user wrote. The one positional form the
// grammar admits — ORDER BY <integer> — is already skipped before the check
// runs (it binds to an output slot by ordinal, and range is enforced upstream),
// so everything that reaches the name comparison arrived as a name and has
// nowhere else to be resolved.
//
// JAVA. This surface is a Go-side extension, so there is no Java behaviour to
// port, and that is worth stating because it is the opposite of the usual
// finding here. Java's grammar attaches orderByClause to `queryTerm` /
// `simpleTable` and NOT to the `setQuery` alternative
// (RelationalParser.g4:428-431, :521-534), and `parenthesisQuery` is
// `'(' query ')'` with nothing following — so `(A UNION B) ORDER BY x` does not
// parse, and in `A UNION ALL B ORDER BY x` the ORDER BY is parsed INTO B and
// resolved against B's own select list (QueryVisitor.java:313-315).
// QueryVisitor.visitSetQuery (:345-356) has no ORDER BY handling at all. Where
// Java does resolve such a key it resolves it BY NAME —
// SemanticAnalyzer.lookupAlias (:513-536) is Identifier.equals on the display
// name. So neither SQL nor Java offers a positional resolution to port.
//
// WHAT THIS PINS, in both directions, because one direction alone would go
// green under a positional rewrite:
//
//   - a name that IS a left-branch output column is ACCEPTED and orders rows;
//   - a name that is only the RIGHT branch's spelling of the same slot is
//     REJECTED with 42703.
//
// The second is the load-bearing one. A check rewritten to validate by position
// would accept `ORDER BY w` — slot 2 exists — and this test is what stops that
// from landing silently. The two branches deliberately spell their second
// column differently (v on the left, w on the right) so that name and position
// disagree; with matching names the whole distinction is inexpressible.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_UnionOrderByNamesAColumnNotAPosition(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	dbPath := "/testdb_union_ob_names"
	setup := openTestDB(t, dbPath)
	mustExec(t, setup, ctx, "CREATE DATABASE "+dbPath)
	mustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE uobn_tmpl "+
		"CREATE TABLE a (id BIGINT, v BIGINT, PRIMARY KEY (id)) "+
		"CREATE TABLE b (id BIGINT, w BIGINT, PRIMARY KEY (id))")
	mustExec(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE uobn_tmpl")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mustExec(t, db, ctx, "INSERT INTO a VALUES (1, 10)")
	mustExec(t, db, ctx, "INSERT INTO a VALUES (2, 20)")
	mustExec(t, db, ctx, "INSERT INTO b VALUES (1, 100)")
	mustExec(t, db, ctx, "INSERT INTO b VALUES (2, 200)")

	const union = "SELECT id, v FROM a UNION ALL SELECT id, w FROM b"

	// CONTROL. The union itself must work, otherwise both arms below could be
	// satisfied by a query that fails for an unrelated reason and the
	// name-versus-position distinction would never be reached.
	t.Run("control_union_without_order_by_returns_four_rows", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, union)
		if qerr != nil {
			t.Fatalf("control: the bare union failed (%v); neither arm below "+
				"can be attributed to ORDER BY validation", qerr)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if cerr := rows.Err(); cerr != nil {
			t.Fatalf("control: iterating the bare union failed: %v", cerr)
		}
		rows.Close()
		if n != 4 {
			t.Fatalf("control: bare union returned %d rows, want 4", n)
		}
	})

	// ACCEPTED: `v` is the LEFT branch's name for the second output column, and
	// the union's output columns take their names from the left branch.
	t.Run("left_branch_name_orders_the_rows", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, union+" ORDER BY v DESC")
		if qerr != nil {
			t.Fatalf("ORDER BY v was rejected (%v). `v` is the left branch's "+
				"name for output column 2 and names the union's result column; "+
				"rejecting it is a spurious 42703", qerr)
		}
		var got []int64
		for rows.Next() {
			var id, v int64
			if serr := rows.Scan(&id, &v); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			got = append(got, v)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		rows.Close()
		want := []int64{200, 100, 20, 10}
		if len(got) != len(want) {
			t.Fatalf("ORDER BY v DESC returned %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ORDER BY v DESC returned %v, want %v — the key was "+
					"accepted but did not order the rows", got, want)
			}
		}
	})

	// REJECTED: `w` is only the RIGHT branch's spelling. It is a perfectly good
	// column name in `b`, and it denotes the same OUTPUT SLOT (2) as `v` — which
	// is exactly why a positional check would wave it through.
	t.Run("right_branch_only_name_is_rejected_42703", func(t *testing.T) {
		rows, qerr := db.QueryContext(ctx, union+" ORDER BY w")
		if qerr == nil {
			for rows.Next() {
			}
			qerr = rows.Err()
			rows.Close()
		}
		if qerr == nil {
			t.Fatalf("ORDER BY w over %q was ACCEPTED. `w` is not a name of the "+
				"union's result columns (those come from the left branch: id, v), "+
				"so this must be 42703.\n\n"+
				"This is the arm that a positional rewrite of "+
				"validateUnionOrderByColumns breaks: output slot 2 exists, so a "+
				"check keyed on position accepts any spelling of it and the "+
				"undefined-column error disappears.", union)
		}
		requireSQLSTATE(t, qerr, api.ErrCodeUndefinedColumn)
	})
}
