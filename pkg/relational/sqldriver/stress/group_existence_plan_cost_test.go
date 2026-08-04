//go:build stress

package stress_test

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// TestFDB_GroupExistenceMerge_VsStreamingAggregation measures the two plan
// shapes RFC-209 §5.3.1 reasons about, ACROSS THE CARDINALITY AXIS §5.3.1 is
// actually about.
//
// §5.3.1 claims that as the group count approaches base-table cardinality,
// streaming aggregation over the base table should WIN on cost, and §7 turns
// that claim into an acceptance criterion. A single measurement at one ratio
// cannot settle that claim in either direction: the claim is a statement about
// a LIMIT, so the measurement has to walk the axis. Four regimes at a fixed
// row count do exactly that:
//
//	unique       groups == rows        (1 row per group — the limit itself)
//	near-unique  groups == rows/1.25   (§7's "near-unique")
//	moderate     groups == rows/10     (the stress suite's own ratio)
//	low          groups == rows/10000  (10 groups — the aggregate index's
//	                                    home turf, for the other end of the axis)
//
// Each regime builds two schemas over IDENTICAL data:
//
//   - "withagg" declares SUM(amount) GROUP BY customer_id, so RFC-209's
//     companion discovery applies and the query plans GroupExistenceMerge over
//     two BY_GROUP index scans (2 * groupCount index entries).
//   - "noagg" declares no aggregate index on customer_id, so the only available
//     plan is aggregation over a base-table scan (rowCount records, plus the
//     grouping work) — the plan §5.3.1 says should win at high group counts.
//
// There is deliberately NO assertion about which plan is faster: which one wins
// is the question being asked, so hardcoding an answer would make the
// measurement unable to report the interesting outcome. What is asserted is
// correctness — both plans must return the same rows, and both must match a
// ground truth computed from the generated data in Go. The numbers are logged,
// one greppable line per regime (prefix GEMERGE_REGIME), for §7's acceptance
// criterion to be written against.
func TestFDB_GroupExistenceMerge_VsStreamingAggregation(t *testing.T) {
	t.Parallel()

	// Fixed across all regimes so the regimes differ in exactly one variable.
	const rows = 100_000

	regimes := []struct {
		name   string
		suffix string
		groups int
	}{
		{"unique", "gemerge_uniq", rows},              // 1.00 rows/group
		{"near-unique", "gemerge_near", rows * 4 / 5}, // 1.25 rows/group
		{"moderate", "gemerge_mod", rows / 10},        // 10 rows/group
		{"low", "gemerge_low", rows / 10_000},         // 10000 rows/group
	}

	for _, reg := range regimes {
		t.Run(reg.name, func(t *testing.T) {
			// Deliberately NOT parallel: these subtests are timing
			// measurements, and running them concurrently would have them
			// measure contention for one single-node cluster rather than the
			// plans.
			runGroupExistenceRegime(t, reg.name, reg.suffix, rows, reg.groups)
		})
	}
}

// runGroupExistenceRegime measures one (rows, groups) point of the axis.
// It deliberately does NOT call t.Helper(): every line it logs is a
// measurement, and attributing them to the caller would collapse four regimes'
// worth of numbers onto one line number in the output.
func runGroupExistenceRegime(t *testing.T, name, suffix string, rows, groups int) {
	// custID is assigned by an exact partition (i % groups) rather than a
	// random draw so the DISTINCT GROUP COUNT IS EXACTLY `groups` in every
	// regime. A random draw of 100k values over 100k customers yields only
	// ~63k distinct groups, which would make the "groups == rows" regime
	// measure something other than what it is named for and would leave the
	// four points incomparable to one another. The assignment is then shuffled
	// so group identity carries no correlation with primary-key order.
	rng := rand.New(rand.NewSource(42))
	custIDs := make([]int, rows)
	for i := range custIDs {
		custIDs[i] = i % groups
	}
	rng.Shuffle(rows, func(a, b int) { custIDs[a], custIDs[b] = custIDs[b], custIDs[a] })

	type orderRow struct {
		custID int
		amount int
		status string
	}
	statuses := []string{"pending", "shipped", "delivered", "cancelled"}
	orders := make([]orderRow, rows)
	groupSums := make([]int64, groups)
	for i := range orders {
		orders[i] = orderRow{
			custID: custIDs[i],
			amount: rng.Intn(10000) + 1,
			status: statuses[i%len(statuses)],
		}
		groupSums[orders[i].custID] += int64(orders[i].amount)
	}

	// The HAVING threshold is the MEDIAN group sum, computed from the data
	// just generated. A constant threshold cannot work across the axis: 50000
	// selects almost nothing at 1 row per group (mean sum ~5000) and
	// everything at 10000 rows per group (mean sum ~5e7), and either way the
	// regime collapses into "aggregate, then discard", which measures the
	// HAVING filter rather than the aggregation. The median holds every regime
	// at ~50% selectivity by construction, so all four ask the same shape of
	// question.
	sorted := append([]int64(nil), groupSums...)
	slices.Sort(sorted)
	threshold := sorted[len(sorted)/2]

	// Ground truth: how many groups the query MUST return. Computed here, in
	// Go, from the same data — so the two plans are checked against a third,
	// independent answer and not merely against each other.
	expectedRows := 0
	for _, s := range groupSums {
		if s > threshold {
			expectedRows++
		}
	}
	if expectedRows == 0 || expectedRows == groups {
		t.Fatalf("regime %s: degenerate HAVING threshold %d — %d of %d groups match",
			name, threshold, expectedRows, groups)
	}

	query := fmt.Sprintf("SELECT customer_id, SUM(amount) FROM orders "+
		"GROUP BY customer_id HAVING SUM(amount) > %d ORDER BY customer_id", threshold)

	build := func(sfx string, aggIndex bool) *stressHarness {
		h := newStressHarness(t, sfx)
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

	withAgg := build(suffix+"_withagg", true)
	noAgg := build(suffix+"_noagg", false)

	mergePlan := explainOf(withAgg)
	basePlan := explainOf(noAgg)
	t.Logf("  merge plan: %s", mergePlan)
	t.Logf("  base  plan: %s", basePlan)

	if !strings.Contains(mergePlan, "GroupExistenceMerge") {
		t.Fatalf("regime %s: with the aggregate index declared, expected the companion-joined plan; got %s", name, mergePlan)
	}
	if strings.Contains(basePlan, "AggregateIndex") {
		t.Fatalf("regime %s: without the aggregate index, expected a base-table plan; got %s", name, basePlan)
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

	// Correctness first — the one invariant, and it holds regardless of which
	// plan is faster.
	if mergeRows != baseRows {
		t.Fatalf("regime %s: the two plans disagree: merge %d rows, base scan %d rows", name, mergeRows, baseRows)
	}
	if mergeRows != expectedRows {
		t.Fatalf("regime %s: both plans returned %d rows, but the data says %d groups have SUM(amount) > %d",
			name, mergeRows, expectedRows, threshold)
	}

	winner := "merge"
	if baseMs < mergeMs {
		winner = "base"
	}
	t.Logf("GEMERGE_REGIME name=%s rows=%d groups=%d rowsPerGroup=%.2f having=%d resultRows=%d mergeMs=%.2f baseMs=%.2f ratio=%.2f winner=%s",
		name, rows, groups, float64(rows)/float64(groups), threshold, mergeRows,
		mergeMs, baseMs, baseMs/mergeMs, winner)
	if winner == "base" {
		// Not a failure. It is the outcome §5.3.1 predicts, and the whole
		// reason this test walks the axis instead of asserting one point:
		// wherever the base-table plan wins, the cost-model flip §5.3.1
		// describes is worth building, and §7's criterion should be written
		// against the regime boundary this run reports rather than against an
		// argument.
		t.Logf("GEMERGE_BASE_WINS name=%s groups=%d base=%.2fms merge=%.2fms — §5.3.1's premise holds at this regime",
			name, groups, baseMs, mergeMs)
	}
}
