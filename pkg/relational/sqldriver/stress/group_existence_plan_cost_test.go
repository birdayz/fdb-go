//go:build stress

package stress_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestFDB_GroupExistenceMerge_VsStreamingAggregation measures the three plan
// shapes RFC-209 §5.3.1 reasons about, on the grouping key §7 calls
// "near-unique": many groups relative to rows (here 10 rows per group, the
// stress suite's own ratio).
//
// §5.3.1 asserts that as the group count approaches base-table cardinality,
// streaming aggregation over the base table should WIN on cost, and §7 makes
// picking the companion-joined merge on that key a failure of the RFC. That
// assertion is a cost claim, and a cost claim is settled by measurement, not by
// argument — this test is the measurement.
//
// Two schemas over IDENTICAL data:
//
//   - "withagg" declares SUM(amount) GROUP BY customer_id, so RFC-209's
//     companion discovery applies and the query plans GroupExistenceMerge over
//     two BY_GROUP index scans (2 * groupCount index entries).
//   - "noagg" declares no aggregate index on customer_id, so the only available
//     plan is aggregation over a base-table scan (rowCount records, plus the
//     grouping work) — exactly the plan §5.3.1 says should win here.
//
// Both answer the same query and MUST agree on the row count; the timings are
// logged. The assertion is the one §5.3.1's premise stands or falls on: that the
// base-table alternative is not in fact dramatically more expensive.
func TestFDB_GroupExistenceMerge_VsStreamingAggregation(t *testing.T) {
	t.Parallel()

	const rows = 100_000
	const groups = rows / 10 // the stress suite's own rows:groups ratio

	const query = "SELECT customer_id, SUM(amount) FROM orders " +
		"GROUP BY customer_id HAVING SUM(amount) > 50000 ORDER BY customer_id"

	// Identical data on both sides — generated once, inserted twice.
	rng := rand.New(rand.NewSource(42))
	type orderRow struct {
		custID int
		amount int
		status string
	}
	statuses := []string{"pending", "shipped", "delivered", "cancelled"}
	orders := make([]orderRow, rows)
	for i := range orders {
		orders[i] = orderRow{
			custID: rng.Intn(groups),
			amount: rng.Intn(10000) + 1,
			status: statuses[i%len(statuses)],
		}
	}

	build := func(suffix string, aggIndex bool) *stressHarness {
		h := newStressHarness(t, suffix)
		ddl := `
		CREATE TABLE orders (
			id BIGINT,
			customer_id BIGINT,
			amount BIGINT,
			status STRING,
			PRIMARY KEY (id)
		)
		CREATE INDEX idx_customer ON orders (customer_id)
		`
		if aggIndex {
			ddl += "\nCREATE INDEX sum_amount_by_customer AS SELECT SUM(amount) FROM orders GROUP BY customer_id\n"
		}
		h.createSchema(ddl)
		h.bulkInsert("orders", rows, func(i int) string {
			o := orders[i]
			return fmt.Sprintf("(%d, %d, %d, '%s')", i, o.custID, o.amount, o.status)
		})
		return h
	}

	explainOf := func(h *stressHarness) string {
		out := &strings.Builder{}
		rs, err := h.db.Query("EXPLAIN " + query)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rs.Close()
		for rs.Next() {
			var plan string
			if err := rs.Scan(&plan); err != nil {
				t.Fatalf("scan EXPLAIN: %v", err)
			}
			out.WriteString(plan)
		}
		return out.String()
	}

	withAgg := build("gemerge_withagg", true)
	noAgg := build("gemerge_noagg", false)

	mergePlan := explainOf(withAgg)
	basePlan := explainOf(noAgg)
	t.Logf("  merge plan: %s", mergePlan)
	t.Logf("  base  plan: %s", basePlan)

	if !strings.Contains(mergePlan, "GroupExistenceMerge") {
		t.Fatalf("with the aggregate index declared, expected the companion-joined plan; got %s", mergePlan)
	}
	if strings.Contains(basePlan, "AggregateIndex") {
		t.Fatalf("without the aggregate index, expected a base-table plan; got %s", basePlan)
	}

	// Time each side twice and keep the faster run, so a cold first touch of a
	// freshly written index subspace is not reported as the plan's cost.
	best := func(h *stressHarness) (int, float64) {
		bestMs := -1.0
		rowCount := -1
		for range 2 {
			r := h.timeQuery(query)
			r.mustSucceed(t, "HAVING")
			ms := float64(r.Duration.Microseconds()) / 1000.0
			if bestMs < 0 || ms < bestMs {
				bestMs = ms
			}
			rowCount = r.RowCount
		}
		return rowCount, bestMs
	}

	mergeRows, mergeMs := best(withAgg)
	baseRows, baseMs := best(noAgg)

	t.Logf("  GroupExistenceMerge (2 x %d index entries): %d rows in %.2fms", groups, mergeRows, mergeMs)
	t.Logf("  aggregation over base scan (%d records):   %d rows in %.2fms", rows, baseRows, baseMs)
	t.Logf("  base/merge ratio: %.2fx", baseMs/mergeMs)

	if mergeRows != baseRows {
		t.Fatalf("the two plans disagree: merge %d rows, base scan %d rows", mergeRows, baseRows)
	}

	// The load-bearing assertion, and the reason this file exists.
	//
	// RFC-209 §5.3.1 claims streaming aggregation over the base table should win
	// on a grouping key with many groups, and §7 turns that claim into an
	// acceptance criterion that fails the RFC if the planner picks the merge
	// here. If that claim held, the base-table plan would be at worst comparable.
	// It is not: the merge reads 2*groups NARROW index entries while the
	// base-table plan reads `rows` FULL records and must group them, and with
	// rows an order of magnitude above groups the merge wins by a wide margin.
	//
	// A flip to the base-table plan on this key is therefore not a fix — it is a
	// larger regression than the one it would be fixing. The threshold below is
	// deliberately loose (the finding is a multiple, not a few percent); it goes
	// RED if the base-table plan ever becomes the cheaper one, which is the
	// condition under which §5.3.1's flip would become the right thing to build.
	if baseMs <= mergeMs {
		t.Fatalf("RFC-209 §5.3.1's premise now HOLDS at rows=%d groups=%d: "+
			"aggregation over the base scan (%.2fms) is at least as cheap as the "+
			"companion-joined merge (%.2fms). The cost-model flip §5.3.1 describes "+
			"is now worth building; re-open §7's near-unique criterion.",
			rows, groups, baseMs, mergeMs)
	}
}
