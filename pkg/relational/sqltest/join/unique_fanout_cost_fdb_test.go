package sqltest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// This is companion SQL cost/plan coverage, not proof that SQL reaches the
// leaf's createsDuplicates guard: the EXISTS distinct wrapper stops that proof
// earlier. The paired plans-package test directly pins the leaf guard itself.
func TestFDB_UniqueFanOutJoinCostCompanion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const ddl = `
CREATE TYPE AS STRUCT attr (root_key BIGINT, payload STRING)
CREATE TABLE outer_rows (id BIGINT, k BIGINT, PRIMARY KEY (id))
CREATE TABLE scalar_rows (id BIGINT, root_key BIGINT, PRIMARY KEY (id))
CREATE UNIQUE INDEX scalar_key ON scalar_rows (root_key)
CREATE TABLE fan_rows (id BIGINT, root_key BIGINT, attrs attr ARRAY, PRIMARY KEY (id))
CREATE UNIQUE INDEX fan_key AS SELECT a.root_key FROM fan_rows AS f,
  (SELECT root_key FROM f.attrs) AS a ORDER BY a.root_key`
	db := testkit.SetupPlanShapeDB(t, "unique_fanout_cost", ddl)
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO outer_rows VALUES (1,5),(2,6),(3,9)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO scalar_rows VALUES (10,5),(20,6)")
	testkit.MustExecCtx(t, db, ctx, "INSERT INTO fan_rows VALUES "+
		"(10,5,[(10,'x'),(11,'x')]),(11,5,[(12,'x')]),(20,6,[(13,'x')])")
	const scalar = "SELECT o.id, s.id FROM outer_rows AS o LEFT JOIN scalar_rows AS s ON s.root_key = o.k ORDER BY o.id, s.id"
	const fanOut = "SELECT o.id, f.id FROM outer_rows AS o LEFT JOIN (SELECT f.id, f.root_key FROM fan_rows AS f WHERE EXISTS (SELECT 1 FROM f.attrs AS a WHERE a.root_key >= 0)) AS f ON f.root_key = o.k ORDER BY o.id, f.id"
	explain := testkit.Explainer(t, db, ctx)
	scalarPlan, fanPlan := explain(scalar), explain(fanOut)
	if !strings.Contains(scalarPlan, "FlatMap(") || !strings.Contains(scalarPlan, "IndexScan(SCALAR_KEY, [=]") {
		t.Fatalf("scalar UNIQUE key should supply a correlated probe: %s", scalarPlan)
	}
	if !strings.Contains(fanPlan, "NestedLoopJoin(") || !strings.Contains(fanPlan, "IndexScan(FAN_KEY") || !strings.Contains(fanPlan, "UnorderedPrimaryKeyDistinct") {
		t.Fatalf("fan-out uniqueness is not root_key uniqueness; expected materialized join over distinct fan-out access: %s", fanPlan)
	}
	for _, tc := range []struct{ query, want string }{
		{scalar, "[1 10] [2 20] [3 <nil>]"},
		{fanOut, "[1 10] [1 11] [2 20] [3 <nil>]"},
	} {
		rows, err := db.QueryContext(ctx, tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var outer int64
			var inner sql.NullInt64
			if err := rows.Scan(&outer, &inner); err != nil {
				t.Fatal(err)
			}
			var value any
			if inner.Valid {
				value = inner.Int64
			}
			got = append(got, fmt.Sprint([]any{outer, value}))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s: rows = %v, want %s", tc.query, got, tc.want)
		}
	}

	// Inspect the SQL-produced operator, rather than constructing another join.
	// Its cost must retain the non-point-probe cardinality for its inner leg.
	physical, err := embedded.PlanPhysicalForTest(fanOut, ddl, nil)
	if err != nil {
		t.Fatal(err)
	}
	joins := 0
	plans.Walk(physical, func(plan plans.RecordQueryPlan) bool {
		join, ok := plan.(*plans.RecordQueryNestedLoopJoinPlan)
		if !ok {
			return true
		}
		joins++
		if n, bound := plans.NestedLoopJoinUniqueKeyConjuncts(join); n != 0 || bound {
			t.Errorf("SQL fan-out join unexpectedly binds a unique root key: (%d,%t)", n, bound)
		}
		cost := join.HintCost([]properties.Cost{{Cardinality: 10, CPU: 1}, {Cardinality: 1000, CPU: 5}}, nil)
		if cost.Cardinality != 5000 {
			t.Errorf("SQL materialized join cardinality = %g, want 5000 rather than outer-only 10", cost.Cardinality)
		}
		return true
	})
	if joins != 1 {
		t.Fatalf("expected one SQL-produced materialized join, got %d: %s", joins, physical.Explain())
	}
}
