package sqldriver_test

// ORDER BY over two GROUP BY keys that share a LEAF name.
//
// This is an UNREACHABILITY pin, and saying which fact it holds down matters
// more than the rows it checks.
//
// The sort arm of the translator resolves an ORDER BY key over an aggregate by
// asking `AggregateKeyColumnName` for the key's output name, and that authority
// renders a FieldValue as its BARE leaf, unconditionally — so both keys of
// `GROUP BY o.k, i.k` are asked for under "K" and the output-ordinal map is
// LAST-WINS on that name. The qualified spelling that saves the HAVING consumer
// (see groupby_same_leaf_key_binder_fdb_test.go) never even forms here.
//
// The queries below nonetheless sort correctly, and did before RFC-229 step 0
// gave that arm a structural decider. The reason is that they never reach it:
// the ORDER BY key arrives already bound to its own slot by the post-aggregate
// binder in core/embedded/logical_predicate.go, which matches the reference
// against the grouping keys STRUCTURALLY and pins the loop index. Measured with
// a probe at all three group-key name-map call sites, this test's queries reach
// them ZERO times.
//
// So what goes red here is that channel disappearing — a reference that stops
// arriving pinned falls through to a last-wins leaf, and this shape is what
// notices. The o.k/i.k pairing is ANTI-CORRELATED on purpose (o.k=1 with i.k=20,
// o.k=2 with i.k=10): with the natural pairing the two sort orders coincide and
// every assertion below passes with the wrong slot bound. Both GROUP BY orders
// are covered, because which key last-wins keeps is the GROUP BY order.
//
// The step-0 decider itself is pinned as a unit, in
// //pkg/relational/core/query:group_key_structural_ordinal_test.go — the corpus
// cannot drive it, so a green suite there is not evidence that it works.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestFDB_GroupBySameLeafKeys_OrderByBindsItsOwnSlot(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_gb_same_leaf_ob")
	gslkMustExec(t, setup, ctx, "CREATE DATABASE /testdb_gb_same_leaf_ob")
	gslkMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE gb_same_leaf_ob "+
			"CREATE TABLE outer_t (k BIGINT, PRIMARY KEY (k)) "+
			"CREATE TABLE inner_t (k BIGINT, o_k BIGINT, PRIMARY KEY (k))")
	gslkMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_gb_same_leaf_ob/s WITH TEMPLATE gb_same_leaf_ob")
	dsn := fmt.Sprintf("fdbsql:///testdb_gb_same_leaf_ob?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	gslkMustExec(t, db, ctx, "INSERT INTO outer_t (k) VALUES (1), (2)")
	gslkMustExec(t, db, ctx, "INSERT INTO inner_t (k, o_k) VALUES (20, 1), (10, 2)")

	// ORDERED, never sorted by the test: the row order IS the assertion.
	ordered := func(t *testing.T, q string) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b, c int64
			if err := rows.Scan(&a, &b, &c); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			out = append(out, fmt.Sprintf("%d/%d/%d", a, b, c))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		return strings.Join(out, " ")
	}

	const sel = "SELECT o.k, i.k, COUNT(*) FROM outer_t o, inner_t i WHERE i.o_k = o.k "

	// Ascending o.k is (1,20) then (2,10); ascending i.k is (2,10) then (1,20).
	const byOK = "1/20/1 2/10/1"
	const byIK = "2/10/1 1/20/1"

	cases := []struct {
		name  string
		query string
		want  string
	}{
		// The two that a last-wins leaf map gets wrong: ORDER BY the FIRST
		// group key, whose ordinal the second key overwrote.
		{"first_key_o_k", sel + "GROUP BY o.k, i.k ORDER BY o.k", byOK},
		{"first_key_i_k", sel + "GROUP BY i.k, o.k ORDER BY i.k", byIK},
		// Controls on the LAST key, which last-wins happens to keep. They pin
		// the rows so neither assertion above can be met by a plan that returns
		// nothing, or by one whose grouping is wrong in the same direction.
		{"last_key_i_k_control", sel + "GROUP BY o.k, i.k ORDER BY i.k", byIK},
		{"last_key_o_k_control", sel + "GROUP BY i.k, o.k ORDER BY o.k", byOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ordered(t, tc.query); got != tc.want {
				t.Errorf("%s\n = %q\nwant %q\n"+
					"The ORDER BY key bound to a group-key output slot that is not its own. "+
					"These keys are supposed to arrive from the post-aggregate binder "+
					"(core/embedded/logical_predicate.go) already pinned to the slot the "+
					"STRUCTURAL match recorded; if they now arrive lazy they are resolved by "+
					"the leaf \"K\", which AggregateKeyColumnName renders for BOTH keys and "+
					"the output-ordinal map holds last-wins.", tc.query, got, tc.want)
			}
		})
	}
}
