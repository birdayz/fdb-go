package sqldriver_test

// A post-aggregate reference to an AGGREGATE binds to the slot the composition
// recorded, not to one recovered from the aggregate's rendered output name.
//
// The producer is `rewriteAggregateValue` (logical_predicate.go): it takes an
// AggregateValue whose identity is fully structural — function plus resolved
// operand — and used to emit a bare `FieldValue{Field: canonicalAggName(...)}`,
// throwing the slot away. `groupByOutputBaker` then recovered it from `aggOrds`,
// a map keyed by `AggregateResultColumnName`'s rendering of the PARSE TEXT.
//
// Two renderings therefore had to agree that are produced by different code from
// different inputs, and neither the compiler nor any test checked that they did.
// The failure modes are a MISS (renderings diverge → the reference falls through
// to a name-model read) and a COLLISION (two renderings coincide → the map is
// last-wins). `aggregateCallOutputSlot` records the slot instead, so neither is
// reachable from this producer.
//
// These scenarios span the shape space the conversion touches: same-leaf
// operands over different legs, aliased aggregates re-read from ORDER BY,
// expression-nested re-reads (the shape the ninth bug of this class needed),
// expression OPERANDS whose two renderings are most likely to diverge, the same
// column under two different functions, and COUNT/SUM/MIN/MAX/AVG variety.
//
// WHAT THIS FILE PINS, stated exactly, because the honest answer is a NEGATIVE
// result and negative results are the ones that get quietly dropped. Every row
// below is IDENTICAL with the conversion and without it — measured by reverting
// the change and re-running, byte for byte, across this file and a wider probe
// set (qualified-versus-bare spellings, quoted operands, CAST operands, nested
// arithmetic operands, an alias deliberately spelled as another aggregate's
// canonical name). The name channel happened to recover the right slot on every
// shape that could be constructed for it. So these are NOT a detector for the
// conversion; they are the pin that says the conversion moved no rows, and the
// coverage that will notice if a future one does.
//
// The detector lives beside the decision, in
// core/embedded/aggregate_output_slot_recorded_test.go, which asserts that the
// reference CARRIES the slot rather than that the answer happens to come out
// right. It goes red on revert; this file does not, and claiming otherwise is
// the failure mode that would make it worthless.
//
// The discriminating data matters as much as the queries: `ot.v` is an order of
// magnitude below `it.v`, and each group has TWO inner rows with different
// values, so SUM, MIN, MAX and AVG of the same column are all distinct. A
// predicate threshold can therefore only be satisfied by the intended
// (aggregate, operand) pair — reading a neighbouring slot changes the answer.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func aosrMustExec(t *testing.T, db *sql.DB, ctx context.Context, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func TestFDB_AggregateOutputSlotIsRecordedAtComposition(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_agg_slot_recorded")
	aosrMustExec(t, setup, ctx, "CREATE DATABASE /testdb_agg_slot_recorded")
	aosrMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE agg_slot_recorded "+
			"CREATE TABLE ot (k BIGINT NOT NULL, v BIGINT, PRIMARY KEY (k)) "+
			"CREATE TABLE it (k BIGINT NOT NULL, o_k BIGINT, v BIGINT, PRIMARY KEY (k))")
	aosrMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_agg_slot_recorded/s WITH TEMPLATE agg_slot_recorded")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_agg_slot_recorded?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// ot.v ∈ {1,2}; it.v ∈ {100,3} for group 1 and {200,7} for group 2.
	// Per group: SUM(o.v) = 2 or 4 (two joined rows), SUM(i.v) = 103 or 207,
	// MIN(i.v) = 3 or 7, MAX(i.v) = 100 or 200, COUNT(*) = 2.
	aosrMustExec(t, db, ctx, "INSERT INTO ot (k, v) VALUES (1, 1), (2, 2)")
	aosrMustExec(t, db, ctx, "INSERT INTO it (k, o_k, v) VALUES (10, 1, 100), (11, 1, 3), (20, 2, 200), (21, 2, 7)")

	// ORDER BY results must keep their order; everything else is compared as a
	// set so a plan's row order cannot make an assertion pass or fail.
	rowsOf := func(t *testing.T, q string, n int) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			cells := make([]any, n)
			ptrs := make([]any, n)
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %q: %v", q, err)
			}
			parts := make([]string, n)
			for i, c := range cells {
				parts[i] = fmt.Sprintf("%v", c)
			}
			out = append(out, strings.Join(parts, "/"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		if !strings.Contains(strings.ToUpper(q), "ORDER BY") {
			sort.Strings(out)
		}
		return strings.Join(out, " ")
	}

	const from = "FROM ot o, it i WHERE i.o_k = o.k "

	cases := []struct {
		name string
		q    string
		n    int
		want string
		why  string
	}{
		{
			name: "same_leaf_operands_having_reads_the_inner_sum",
			q:    "SELECT o.k, SUM(o.v), SUM(i.v) " + from + "GROUP BY o.k HAVING SUM(i.v) > 150",
			n:    3, want: "2/4/207",
			why: "SUM(o.v) and SUM(i.v) share the leaf V. Bound to SUM(o.v)'s slot the " +
				"threshold 150 admits no group at all.",
		},
		{
			name: "same_leaf_operands_having_reads_the_outer_sum",
			q:    "SELECT o.k, SUM(o.v), SUM(i.v) " + from + "GROUP BY o.k HAVING SUM(o.v) > 2",
			n:    3, want: "2/4/207",
			why: "The mirror: bound to SUM(i.v)'s slot the threshold 2 admits BOTH groups. " +
				"One direction alone passes for a binder that always picks the same neighbour.",
		},
		{
			name: "expression_nested_reread",
			q:    "SELECT o.k, SUM(o.v), SUM(i.v) " + from + "GROUP BY o.k HAVING SUM(i.v) + 0 > 150",
			n:    3, want: "2/4/207",
			why: "The ninth bug of this class needed the re-read NESTED in an expression; a " +
				"top-level comparison can be rescued by a second name-based recovery.",
		},
		{
			name: "expression_nested_reread_mirror",
			q:    "SELECT o.k, SUM(o.v), SUM(i.v) " + from + "GROUP BY o.k HAVING SUM(o.v) * 1 > 2",
			n:    3, want: "2/4/207",
			why: "Expression-nested, other direction.",
		},
		{
			name: "expression_operand_renderings",
			q: "SELECT o.k, SUM(o.v * 2), SUM(i.v) " + from +
				"GROUP BY o.k HAVING SUM(o.v * 2) > 4",
			n: 3, want: "2/8/207",
			why: "A COMPUTED operand is where the two renderings that used to have to agree " +
				"are most likely to diverge: one walks the resolved Value, the other the parse text.",
		},
		{
			name: "two_leg_expression_operand",
			q: "SELECT o.k, SUM(o.v + i.v), SUM(i.v) " + from +
				"GROUP BY o.k HAVING SUM(o.v + i.v) > 150",
			n: 3, want: "2/211/207",
			why: "An operand spanning BOTH legs: no single leg's column order can address it.",
		},
		{
			name: "same_column_two_functions_min",
			q:    "SELECT o.k, SUM(i.v), MIN(i.v), MAX(i.v) " + from + "GROUP BY o.k HAVING MIN(i.v) > 5",
			n:    4, want: "2/207/7/200",
			why: "Three aggregates over the SAME column: only the FUNCTION separates them, so a " +
				"binder that matched on the operand alone would take the first.",
		},
		{
			name: "same_column_two_functions_max",
			q:    "SELECT o.k, SUM(i.v), MIN(i.v), MAX(i.v) " + from + "GROUP BY o.k HAVING MAX(i.v) > 150",
			n:    4, want: "2/207/7/200",
			why: "Same three aggregates, the predicate on the LAST of them.",
		},
		{
			name: "count_star_beside_sums",
			q: "SELECT o.k, COUNT(*), SUM(o.v), SUM(i.v) " + from +
				"GROUP BY o.k HAVING COUNT(*) > 1 AND SUM(i.v) > 150",
			n: 4, want: "2/2/4/207",
			why: "COUNT(*) has no operand at all; it binds by its star-ness. Conjoined with an " +
				"operand aggregate so both binds must land.",
		},
		{
			name: "count_column_and_sum",
			q: "SELECT o.k, COUNT(i.v), SUM(i.v) " + from +
				"GROUP BY o.k HAVING COUNT(i.v) > 1 AND SUM(i.v) > 150",
			n: 3, want: "2/2/207",
			why: "COUNT over a column, beside SUM over the SAME column.",
		},
		{
			name: "avg_beside_sum_same_column",
			q:    "SELECT o.k, AVG(i.v), SUM(i.v) " + from + "GROUP BY o.k HAVING AVG(i.v) > 100",
			n:    3, want: "2/103.5/207",
			why: "AVG's result type differs from SUM's over the same column, so a mis-bind here " +
				"also mis-types the reference.",
		},
		{
			name: "aliased_aggregate_reread_from_order_by",
			q: "SELECT o.k, SUM(o.v) AS a, SUM(i.v) AS b " + from +
				"GROUP BY o.k ORDER BY b ASC",
			n: 3, want: "1/2/103 2/4/207",
			why: "An ALIAS is a second name for the same slot, and aliases are exactly what made " +
				"the name map collide. Ascending, so reading the other slot cannot coincide.",
		},
		{
			name: "aliased_aggregate_reread_from_order_by_desc",
			q: "SELECT o.k, SUM(o.v) AS a, SUM(i.v) AS b " + from +
				"GROUP BY o.k ORDER BY b DESC",
			n: 3, want: "2/4/207 1/2/103",
			why: "The descending mirror.",
		},
		{
			name: "order_by_canonical_aggregate_text",
			q:    "SELECT o.k, SUM(o.v), SUM(i.v) " + from + "GROUP BY o.k ORDER BY SUM(i.v) DESC",
			n:    3, want: "2/4/207 1/2/103",
			why: "ORDER BY spelling the aggregate out rather than naming its alias.",
		},
		{
			name: "having_and_order_by_together",
			q: "SELECT o.k, SUM(o.v), SUM(i.v) " + from +
				"GROUP BY o.k HAVING SUM(o.v) > 1 ORDER BY SUM(i.v) DESC",
			n: 3, want: "2/4/207 1/2/103",
			why: "Both post-aggregate consumers on one query; each binds through its own path.",
		},
		{
			name: "duplicate_aggregate_select_and_having",
			q:    "SELECT o.k, SUM(i.v) " + from + "GROUP BY o.k HAVING SUM(i.v) > 150",
			n:    2, want: "2/207",
			why: "The SELECT copy and the HAVING copy of one aggregate are harvested as TWO " +
				"value-identical calls. The recorded slot picks the first; the retired name map's " +
				"last-wins picked the second. Both compute the same value, which is why this " +
				"case pins ROWS rather than a slot.",
		},
		{
			name: "same_leaf_group_keys_beside_an_aggregate_reread",
			q: "SELECT o.k, i.k, SUM(i.v) FROM ot o, it i WHERE i.o_k = o.k " +
				"GROUP BY o.k, i.k HAVING SUM(i.v) + o.k > 102",
			n: 3, want: "2/20/200",
			why: "The two-same-leaf-group-keys collision re-verified THROUGH the converted " +
				"aggregate reader: one predicate, one group-key re-read and one aggregate " +
				"re-read, each binding by its own recorded slot. Groups are " +
				"(1,10,100) (1,11,3) (2,20,200) (2,21,7); reading O.K off I.K's slot admits " +
				"(1,10) as well.",
		},
		{
			name: "same_leaf_group_keys_beside_an_aggregate_reread_mirror",
			q: "SELECT o.k, i.k, SUM(i.v) FROM ot o, it i WHERE i.o_k = o.k " +
				"GROUP BY i.k, o.k HAVING SUM(i.v) + i.k > 105",
			n: 3, want: "1/10/100 2/20/200",
			why: "The mirror swaps which key the output-name map's last-wins would keep, so a " +
				"binder that merely reorders the map cannot satisfy both. Reading I.K off " +
				"O.K's slot drops (1,10).",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := rowsOf(t, c.q, c.n); got != c.want {
				t.Errorf("%s\n  query: %s\n  got:  %q\n  want: %q\n  %s\n"+
					"A post-aggregate reference bound to a slot other than the one its "+
					"composition recorded (RFC-197 item 5: aggregateCallOutputSlot).",
					c.name, c.q, got, c.want, c.why)
			}
		})
	}
}
