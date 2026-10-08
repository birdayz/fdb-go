package embedded

// Planner-only cost of the 1M stress workload's query shapes, over its schema
// and with no FDB behind it. The stress comparison measures plan + execute
// against a live container, and its ~5-10 ms point reads are dominated by
// whatever the machine is doing in that second; when a stress ratio moves on
// those rows, this is the instrument that says whether the PLANNER moved.
// Measured across two stress orderings (base-first and branch-first), a 2x on
// the first five readings followed the run's POSITION in the sequence and not
// the tree, while these benchmarks agreed to within 2% — which is what let
// that reading be classified as noise rather than a regression.
//
//	go test ./pkg/relational/core/embedded -run '^$' -bench BenchmarkPlanStressShape_ -benchtime=200x -count=3

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

const planStressShapesSchema = `
CREATE TABLE customers (id BIGINT, name STRING, region STRING, PRIMARY KEY (id))
CREATE TABLE orders (id BIGINT, customer_id BIGINT, amount BIGINT, status STRING, PRIMARY KEY (id))
CREATE INDEX idx_customer ON orders (customer_id)
CREATE INDEX idx_amount ON orders (amount)
CREATE INDEX idx_status ON orders (status)
CREATE INDEX idx_status_count AS SELECT COUNT(*) FROM orders GROUP BY status
CREATE INDEX idx_status_sum AS SELECT SUM(amount) FROM orders GROUP BY status`

func benchPlanStressShape(b *testing.B, sql string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := PlanPhysicalForTest(sql, planStressShapesSchema, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPlanStressShape_PKLookup(b *testing.B) {
	benchPlanStressShape(b, "SELECT * FROM orders WHERE id = 0")
}

func BenchmarkPlanStressShape_IdxCustomerEq(b *testing.B) {
	benchPlanStressShape(b, "SELECT id, amount FROM orders WHERE customer_id = 0")
}

func BenchmarkPlanStressShape_IdxAmountRange(b *testing.B) {
	benchPlanStressShape(b, "SELECT id FROM orders WHERE amount > 9000")
}

func BenchmarkPlanStressShape_GroupByStatus(b *testing.B) {
	benchPlanStressShape(b, "SELECT status, COUNT(*) FROM orders GROUP BY status")
}

func BenchmarkPlanStressShape_SumByStatus(b *testing.B) {
	benchPlanStressShape(b, "SELECT status, SUM(amount) FROM orders GROUP BY status")
}

func BenchmarkPlanStressShape_InList(b *testing.B) {
	benchPlanStressShape(b, "SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id")
}

// No task observer: the convergence test's memo census is not planner latency.
func BenchmarkPlanFixedFactorUnionScalarSubquery(b *testing.B) {
	const schema = "CREATE TABLE T_RD (id BIGINT, a BIGINT, b BIGINT, c BIGINT, s STRING, f BOOLEAN, d DOUBLE, e FLOAT, PRIMARY KEY (id)) CREATE INDEX idx_c ON T_RD (c) CREATE INDEX idx_a ON T_RD (a) CREATE INDEX idx_d ON T_RD (d) CREATE INDEX idx_ab ON T_RD (a, b)"
	const sql = "SELECT * FROM t_rd WHERE (((NOT (c = 7)) AND (d = 4.0) AND (b = 2)) OR ((NOT (a BETWEEN 1 AND 4)) AND (NOT (e > 0.1)) AND (ABS(c) = 4))) AND NOT EXISTS (SELECT 1 FROM t_rd AS r WHERE r.a < t_rd.a AND r.a > 9) AND c <= (SELECT MIN(a) FROM t_rd) ORDER BY b, id"
	b.ReportAllocs()
	var plan plans.RecordQueryPlan
	var err error
	for i := 0; i < b.N; i++ {
		plan, err = PlanPhysicalForTest(sql, schema, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.Log(plan.Explain())
}
