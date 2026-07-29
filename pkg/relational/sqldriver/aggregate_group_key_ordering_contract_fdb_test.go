package sqldriver_test

// The group-key output NAME on the streaming aggregate's PROVIDED ordering is an
// identity decision, not a display label — pinned end to end, on rows AND on the
// plan.
//
// RFC-197 item 5 treats `expressions.AggregateKeyColumnName` as a naming
// authority whose consumers must all become ordinal before it can return a
// display-only carrier. Two of its consumers really are display-only, and were
// measured so: replacing every name the aggregate cursor stamps on its emitted
// PositionalRow with a positional probe, and separately replacing every name the
// plan reports for the UNION position-remap, each leaves the entire relational
// corpus green — this driver suite, yamsql, rowdiff, plandiff, explaindiff,
// memoinvariant. Only tests asserting the spelling itself notice.
//
// The consumer pinned HERE is not display-only. `RecordQueryStreamingAggregation
// Plan.HintOrdering` (plans/ordering.go) stamps the canonical group-key output
// name onto each advertised ordering key, and `properties.RichOrdering` addresses
// its ordering set by `values.ExplainValue` — a rendered string. The ORDER BY's
// requested key is an independently constructed FieldValue that meets the
// provided key only as that rendering, and the two agree solely because both
// spellings come from this one authority.
//
// Break that agreement and nothing errors: the ORDER BY simply stops being
// satisfied by the group-key order the aggregate already provides, and the
// planner stacks a SECOND InMemorySort above the aggregate. Rows stay correct,
// which is why only a plan assertion can see it. Measured, not predicted —
// probing the name at the HintOrdering site produced
//
//	want exactly 1 InMemorySort (group-key sort, reused by ORDER BY), got 2
//
// in TestFDB_GroupByHavingOverOrdinalJoin, plus corpus plan-shape golden
// movement. That existing red is incidental (it belongs to a join-ordinalization
// test); this file pins the contract deliberately, including the two-same-leaf-
// group-keys shape where a spelling-based match has the most to get wrong and
// where the `#ordinal` discriminator in the rendering is what keeps the keys
// apart.
//
// The unit-level twin, which asserts the two spellings agree rather than
// asserting the plan that follows from it, is
// plans/streaming_aggregation_ordering_key_name_test.go.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func agkoMustExec(t *testing.T, db *sql.DB, ctx context.Context, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func TestFDB_AggregateGroupKeyOrderingIsProvidedNotResorted(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	t.Parallel()
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_agg_ord_contract")
	agkoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_agg_ord_contract")
	agkoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE agg_ord_contract "+
			"CREATE TABLE ot (k BIGINT NOT NULL, v BIGINT, PRIMARY KEY (k)) "+
			"CREATE TABLE it (k BIGINT NOT NULL, o_k BIGINT, v BIGINT, PRIMARY KEY (k))")
	agkoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_agg_ord_contract/s WITH TEMPLATE agg_ord_contract")
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql:///testdb_agg_ord_contract?cluster_file=%s&schema=s", clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Inserted OUT of key order, so a plan that merely drops the sort without
	// really providing the order emits rows in the wrong sequence and the row
	// assertion catches it. Two inner rows per outer key give every aggregate a
	// distinct value per group.
	agkoMustExec(t, db, ctx, "INSERT INTO ot (k, v) VALUES (3, 30), (1, 10), (2, 20)")
	agkoMustExec(t, db, ctx,
		"INSERT INTO it (k, o_k, v) VALUES "+
			"(300, 3, 3000), (301, 3, 3001), "+
			"(100, 1, 1000), (101, 1, 1001), "+
			"(200, 2, 2000), (201, 2, 2001)")

	// countSorts is the whole point of the plan half: the group-key sort the
	// aggregate needs for its own correctness is ONE sort; a second one means
	// the ORDER BY stopped matching the ordering the aggregate advertises.
	countSorts := func(plan string) int { return strings.Count(plan, "InMemorySort(") }

	t.Run("single_group_key_over_a_pk_ordered_scan_sorts_not_at_all", func(t *testing.T) {
		// Grouping on the primary key over a PK-ordered scan: the aggregate needs
		// no sort of its own, so the ORDER BY has NOTHING to ride except the
		// ordering the aggregate advertises. Any sort at all is the regression.
		q := "SELECT k, COUNT(*) FROM ot GROUP BY k ORDER BY k"
		got := pinRows(t, db, ctx, q)
		if want := []string{"1|1", "2|1", "3|1"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v (ascending by group key)", got, want)
		}
		plan := pinExplain(t, db, ctx, q)
		if n := countSorts(plan); n != 0 {
			t.Errorf("want ZERO InMemorySort (the aggregate provides the group-key order "+
				"outright over a PK-ordered scan), got %d.\n"+
				"A sort here means the provided group-key ordering stopped matching the "+
				"requested one — the two are matched by RENDERED STRING, both spelled "+
				"through expressions.AggregateKeyColumnName.\n%s", n, plan)
		}
	})

	t.Run("group_key_order_by_over_a_join", func(t *testing.T) {
		// The shape the incidental red was found on: the aggregate sits over a
		// join, so the group-key sort is unavoidable and the ORDER BY must reuse
		// it rather than add its own.
		q := "SELECT ot.k, COUNT(it.k) FROM ot JOIN it ON it.o_k = ot.k GROUP BY ot.k ORDER BY ot.k"
		got := pinRows(t, db, ctx, q)
		if want := []string{"1|2", "2|2", "3|2"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		plan := pinExplain(t, db, ctx, q)
		if n := countSorts(plan); n != 1 {
			t.Errorf("want exactly 1 InMemorySort (the group-key sort, reused by ORDER BY), got %d.\n%s",
				n, plan)
		}
	})

	t.Run("two_same_leaf_group_keys_order_by_both", func(t *testing.T) {
		// Both group keys render their canonical output name as the bare leaf K,
		// from two different sources. The ordering match survives only because
		// the rendering carries the `#ordinal` discriminator alongside the name;
		// this is the shape where a name-only match would conflate the keys.
		q := "SELECT ot.k, it.k, COUNT(*) FROM ot JOIN it ON it.o_k = ot.k " +
			"GROUP BY ot.k, it.k ORDER BY ot.k, it.k"
		got := pinRows(t, db, ctx, q)
		want := []string{"1|100|1", "1|101|1", "2|200|1", "2|201|1", "3|300|1", "3|301|1"}
		if !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v — the two same-leaf group keys must stay "+
				"independently ordered, outer key major and inner key minor", got, want)
		}
		plan := pinExplain(t, db, ctx, q)
		if n := countSorts(plan); n != 1 {
			t.Errorf("want exactly 1 InMemorySort (the group-key sort, reused by ORDER BY), got %d.\n"+
				"Two same-leaf group keys are told apart in the ordering match only by the "+
				"`#ordinal` discriminator on the rendered key.\n%s", n, plan)
		}
	})

	t.Run("group_key_order_by_beside_an_aggregate_projection", func(t *testing.T) {
		// A post-aggregate projection sits between the aggregate and the sort.
		// The ORDER BY key still has to pull up onto the aggregate's output slot
		// and still has to render the same as the provided key.
		q := "SELECT ot.k, SUM(it.v) FROM ot JOIN it ON it.o_k = ot.k GROUP BY ot.k ORDER BY ot.k"
		got := pinRows(t, db, ctx, q)
		if want := []string{"1|2001", "2|4001", "3|6001"}; !eqStrSlices(got, want) {
			t.Errorf("rows = %v, want %v", got, want)
		}
		plan := pinExplain(t, db, ctx, q)
		if n := countSorts(plan); n != 1 {
			t.Errorf("want exactly 1 InMemorySort (the group-key sort, reused by ORDER BY), got %d.\n%s",
				n, plan)
		}
	})
}
