package sqldriver_test

// RFC-173 regression pin for the STREAMING-AGGREGATE INPUT edge over RESHAPING /
// multi-source inputs — the last name-keyed survivor family. A GROUP BY / aggregate
// whose input is NOT a plain base-table scan and NOT a raw 2-way ordinal JOIN merge,
// but a reshaping producer that emits a flat single-source positional row:
//
//   - a projection / derived table / CTE            (RecordQueryProjectionPlan)
//   - a projecting CTE with a RENAME (aliased key)  (RecordQueryProjectionPlan)
//   - a DISTINCT over a projection                  (RecordQueryDistinctPlan)
//   - a WHERE-EXISTS semi-join (identity-over-outer) (RecordQueryFlatMapPlan)
//   - a nested StreamingAgg (GROUP BY over an aggregate subquery)
//
// Before this slice the aggregate cursor gated its positional dispatch on a NARROW
// aggregateInputIsPlainScan (walks only Scan/Index under layout-preserving single-
// child wrappers), so every reshaping input fell through to the name-keyed Datum arm
// and read its group key / operand by NAME off the map. The fix makes aggregateEvalArg
// mirror executeProjection's frontier dispatch exactly: baked-ordinal → join-window →
// ANY flat positional frontier → name fallback. Because every reshaping producer above
// emits a flat single-source positional row whose GetByName is unambiguous, the group
// key / operand now resolves against the inner PositionalRow (frontierRowContext →
// evaluateOrdinal → GetByName), not the name-keyed Datum.
//
// The proof is the values.NameReadForbidden measurement gate: with it ARMED, EVERY
// name-keyed Datum read (present or absent) is a hard error. Each shape below was a
// SURVIVOR (hard-errored) before the fix and returns the correct rows after — the
// dimension the dual-window differential cannot see (both models agree when BOTH read
// by name). Not t.Parallel(): the gate is a process-global runtime flag; Go runs this
// in the serial phase and it resets the flag in defer, so no other test observes it
// armed. The schema is unique to this test, so no cached plan is shared.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFDB_RFC173_AggregateReshapingInputOrdinal(t *testing.T) {
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	db := setupPlanShapeDB(t, "aggreshape",
		"CREATE TABLE orders (oid BIGINT NOT NULL, cust BIGINT, status STRING, amt BIGINT, PRIMARY KEY (oid)) "+
			"CREATE INDEX status_idx ON orders (status) "+
			"CREATE TABLE cust (cid BIGINT NOT NULL, name STRING, region STRING, PRIMARY KEY (cid))")

	mwjoMustExec(t, db, ctx,
		"INSERT INTO cust (cid, name, region) VALUES (1,'alice','west'),(2,'bob','east'),(3,'carol','west')")
	mwjoMustExec(t, db, ctx,
		"INSERT INTO orders (oid, cust, status, amt) VALUES "+
			"(10,1,'shipped',100),(11,1,'pending',50),(12,2,'shipped',200),(13,3,'shipped',30),(14,2,'pending',70)")

	// Arm the measurement gate ONLY for this serial test: any name-keyed read on
	// the aggregate input path now hard-errors.
	values.NameReadForbidden = true
	defer func() { values.NameReadForbidden = false }()

	// rowsOf runs q and returns its rows sorted, joined "|" per column, for
	// order-independent comparison. Fatals on any error (a survivor would
	// surface here as the NameReadForbidden hard error).
	rowsOf := func(t *testing.T, q string) string {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q under NameReadForbidden: %v", q, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		n := len(cols)
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
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err %q: %v", q, err)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	// GROUP BY over a CTE (Project input): key CUST resolves off the projection's
	// flat positional row.
	t.Run("agg_over_cte", func(t *testing.T) {
		got := rowsOf(t, "WITH s AS (SELECT cust, amt FROM orders WHERE status = 'shipped') "+
			"SELECT cust, SUM(amt) FROM s GROUP BY cust")
		if want := "1|100 2|200 3|30"; got != want {
			t.Errorf("agg over CTE = %q, want %q", got, want)
		}
	})

	// GROUP BY over a derived table (subquery in FROM = Project input).
	t.Run("agg_over_derived_table", func(t *testing.T) {
		got := rowsOf(t, "SELECT cust, SUM(amt) FROM (SELECT cust, amt FROM orders) AS d GROUP BY cust")
		if want := "1|150 2|270 3|30"; got != want {
			t.Errorf("agg over derived table = %q, want %q", got, want)
		}
	})

	// GROUP BY over a projecting CTE with a RENAME: the group key CK is the
	// projection's OUTPUT (aliased) name, resolved off the positional row by that
	// alias — not the source column CUST.
	t.Run("agg_over_cte_rename", func(t *testing.T) {
		got := rowsOf(t, "WITH s AS (SELECT cust AS ck, amt AS a FROM orders) "+
			"SELECT ck, SUM(a) FROM s GROUP BY ck")
		if want := "1|150 2|270 3|30"; got != want {
			t.Errorf("agg over renamed CTE = %q, want %q", got, want)
		}
	})

	// GROUP BY over a DISTINCT input (dedup node above the projection).
	t.Run("agg_over_distinct", func(t *testing.T) {
		got := rowsOf(t, "SELECT cust, COUNT(*) FROM (SELECT DISTINCT cust, status FROM orders) AS d GROUP BY cust")
		if want := "1|2 2|2 3|1"; got != want {
			t.Errorf("agg over DISTINCT = %q, want %q", got, want)
		}
	})

	// GROUP BY over a WHERE-EXISTS semi-join (identity-over-outer FlatMap whose
	// outer is a plain scan): the group key REGION resolves off the passed-through
	// outer scan positional row. This shape unwraps to a non-join outer, so
	// joinWindowsOK is false — it exercises the flat-frontier arm, NOT leg windows.
	t.Run("agg_over_exists_semijoin", func(t *testing.T) {
		got := rowsOf(t, "SELECT c.region, COUNT(*) FROM cust c "+
			"WHERE EXISTS (SELECT 1 FROM orders o WHERE o.cust = c.cid) GROUP BY c.region")
		if want := "east|1 west|2"; got != want {
			t.Errorf("agg over EXISTS semi-join = %q, want %q", got, want)
		}
	})

	// GROUP BY over a nested StreamingAgg (aggregate subquery in FROM): the outer
	// group key G is the inner aggregate's aliased projection output.
	t.Run("agg_over_nested_agg", func(t *testing.T) {
		got := rowsOf(t, "SELECT g, COUNT(*) FROM "+
			"(SELECT cust AS g, COUNT(*) AS n FROM orders GROUP BY cust) AS d GROUP BY g")
		if want := "1|1 2|1 3|1"; got != want {
			t.Errorf("agg over nested agg = %q, want %q", got, want)
		}
	})
}
