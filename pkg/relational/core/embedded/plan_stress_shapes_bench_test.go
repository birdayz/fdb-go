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

import "testing"

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
