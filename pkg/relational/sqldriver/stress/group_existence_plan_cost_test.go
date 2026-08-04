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
// §5.3.1 ONCE claimed that as the group count approaches base-table
// cardinality, streaming aggregation over the base table should win on cost,
// and §7 turned that claim into an acceptance criterion. This test is what
// refuted it, and the claim has since been struck: the merge is faster at every
// regime below, including the unique limit. §5.3.1 now records why the flip was
// not built — Java's Cascades will not construct the plan it prescribed, and
// the measurement went the other way.
//
// The test is kept, and kept assertion-free about which plan wins, because a
// struck claim is not a settled one: the numbers are what re-open it. A single
// measurement at one ratio could not settle the question in either direction —
// it is a statement about a LIMIT, so the measurement has to walk the axis.
// Four regimes at a fixed row count do exactly that:
//
//	unique       groups == rows        (1 row per group — the limit itself)
//	near-unique  groups == rows/1.25   (§7's "near-unique")
//	moderate     groups == rows/10     (the stress suite's own ratio)
//	low          groups == rows/10000  (10 groups — the aggregate index's
//	                                    home turf, for the other end of the axis)
//
// Each regime builds three schemas over IDENTICAL data:
//
//   - "withagg" declares SUM(amount) GROUP BY customer_id, so RFC-209's
//     companion discovery applies and the query plans GroupExistenceMerge over
//     two BY_GROUP index scans (2 * groupCount index entries).
//   - "noagg" declares no aggregate index on customer_id, so the only available
//     plan is aggregation over a base-table scan (rowCount records, plus the
//     grouping work) — the plan the struck §5.3.1 flip said should win at high
//     group counts.
//   - "refcount" declares ONLY a grouped COUNT(*) on customer_id. By §5.3(a) a
//     grouped COUNT(*) is its own group-existence oracle, so it needs no
//     companion and plans as a BARE SINGLE BY_GROUP aggregate-index scan over
//     groupCount entries. This is the REFERENCE: the plan shape §7's cost
//     criterion is written against.
//
// The reference exists because §7's real baseline — the pre-RFC
// aggregate-index-only plan, one scan and wrong — is UNCONSTRUCTIBLE by design.
// Once a SUM owner exists the companion is auto-emitted and the planner always
// builds the merge, so there is nothing to start a stopwatch on. A grouped
// COUNT(*) at the same group cardinality executes the identical physical shape
// (one BY_GROUP scan, one aggregate value per group, an ORDER BY the scan
// already satisfies, a HAVING over the aggregate) and IS constructible. §7's
// bound is therefore timed against something that runs, not derived from an
// argument about index I/O.
//
// There is deliberately NO assertion about which plan is faster: which one wins
// is the question being asked, so hardcoding an answer would make the
// measurement unable to report the interesting outcome. What is asserted is
// correctness — the two plans must return the same rows, both must match a
// ground truth computed from the generated data in Go, and the reference must
// match its own independently computed ground truth. The numbers are logged,
// two greppable lines per regime (prefixes GEMERGE_REGIME for merge-vs-base and
// GEMERGE_REFERENCE for merge-vs-single-scan), for §7's acceptance criterion to
// be written against.
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

// groupExistenceRefBound is §7's acceptance bound: the group-existence merge
// must stay within this multiple of a single-BY_GROUP-scan reference at matched
// cardinality. Measured merge-over-reference across the axis, over three runs:
// unique 2.34-2.40, near-unique 2.86-3.08, moderate 2.12-2.47, low 1.01-1.05.
//
// near-unique is the worst regime and the noisiest, moving ~7% between runs, so
// the bound is set at 3.5 — roughly 14% over the worst observed point. 3x was
// rejected outright: it sits BELOW a point already observed at 3.08, so it would
// fail the RFC on a clean run.
//
// The headroom is deliberately modest, which means this bound is a real
// constraint rather than a formality: a regression much past ~15% on the
// near-unique regime fails here. If it starts flapping without a code change,
// the honest fix is more samples per regime (median of N), not a looser number.
const groupExistenceRefBound = 3.5

// groupExistenceBoundMinGroups is the group count below which the reference
// query stops measuring scan work -- it reads too few index entries for its
// runtime to be anything but fixed per-query cost. The bound is a statement
// about scan cost, so it is not applied below this.
const groupExistenceBoundMinGroups = 1000

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
	groupCounts := make([]int64, groups)
	for i := range orders {
		orders[i] = orderRow{
			custID: custIDs[i],
			amount: rng.Intn(10000) + 1,
			status: statuses[i%len(statuses)],
		}
		groupSums[orders[i].custID] += int64(orders[i].amount)
		groupCounts[orders[i].custID]++
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

	// The REFERENCE query's threshold cannot be picked the same way, because
	// the median trick relies on a spread the group SIZES do not have. custID is
	// an exact partition (i % groups), so every group's row count is either
	// floor(rows/groups) or that plus one — at most two distinct values, and
	// exactly ONE whenever groups divides rows. Three of the four regimes
	// (unique, moderate, low) are that uniform case, and there no `COUNT(*) > T`
	// is non-degenerate: every threshold selects all groups or none.
	//
	// So the threshold is chosen by SEARCH over the count values actually
	// present, taking the non-degenerate one closest to 50% selectivity. Where
	// none exists — the uniform regimes — the fallback is min-1, which selects
	// EVERY group, and the log line says so (refUniform=true). Selecting every
	// group is the honest choice of the two available degeneracies: it makes the
	// reference do at least as much work as the merge does (same scan, more rows
	// out), so the merge/reference ratio it produces is not flattered by a
	// reference that skipped the output path. A threshold selecting nothing
	// would scan the same entries but return none, understating the reference
	// and inflating the ratio.
	distinct := append([]int64(nil), groupCounts...)
	slices.Sort(distinct)
	distinct = slices.Compact(distinct)
	refThreshold := distinct[0] - 1
	refUniform := true
	bestGap := 0
	for _, cand := range distinct {
		n := 0
		for _, c := range groupCounts {
			if c > cand {
				n++
			}
		}
		if n == 0 || n == groups {
			continue
		}
		gap := n - groups/2
		if gap < 0 {
			gap = -gap
		}
		if refUniform || gap < bestGap {
			refThreshold, bestGap, refUniform = cand, gap, false
		}
	}
	refExpectedRows := 0
	for _, c := range groupCounts {
		if c > refThreshold {
			refExpectedRows++
		}
	}
	if refExpectedRows == 0 {
		t.Fatalf("regime %s: reference threshold %d selects no group out of %d — the reference would measure an empty result, not a scan",
			name, refThreshold, groups)
	}
	if refUniform && refExpectedRows != groups {
		t.Fatalf("regime %s: uniform-count fallback threshold %d selected %d of %d groups, expected all",
			name, refThreshold, refExpectedRows, groups)
	}

	refQuery := fmt.Sprintf("SELECT customer_id, COUNT(*) FROM orders "+
		"GROUP BY customer_id HAVING COUNT(*) > %d ORDER BY customer_id", refThreshold)

	build := func(sfx string, extraIndexDDL string) *stressHarness {
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
		ddl += extraIndexDDL
		h.createSchema(ddl)
		h.bulkInsert("orders", rows, func(i int) string {
			o := orders[i]
			return fmt.Sprintf("(%d, %d, %d, '%s')", i, o.custID, o.amount, o.status)
		})
		return h
	}

	explainOf := func(h *stressHarness, q string) string {
		out := &strings.Builder{}
		rs, err := h.db.Query("EXPLAIN " + q)
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

	withAgg := build(suffix+"_withagg",
		"\nCREATE INDEX sum_amount_by_customer AS SELECT SUM(amount) FROM orders GROUP BY customer_id\n")
	noAgg := build(suffix+"_noagg", "")
	refCount := build(suffix+"_refcount",
		"\nCREATE INDEX count_by_customer AS SELECT COUNT(*) FROM orders GROUP BY customer_id\n")

	mergePlan := explainOf(withAgg, query)
	basePlan := explainOf(noAgg, query)
	refPlan := explainOf(refCount, refQuery)
	t.Logf("  merge plan: %s", mergePlan)
	t.Logf("  base  plan: %s", basePlan)
	t.Logf("  ref   plan: %s", refPlan)

	if !strings.Contains(mergePlan, "GroupExistenceMerge") {
		t.Fatalf("regime %s: with the aggregate index declared, expected the companion-joined plan; got %s", name, mergePlan)
	}
	if strings.Contains(basePlan, "AggregateIndex") {
		t.Fatalf("regime %s: without the aggregate index, expected a base-table plan; got %s", name, basePlan)
	}

	// The reference is only a reference if it is the SINGLE-SCAN shape. A
	// grouped COUNT(*) is its own group-existence oracle (§5.3(a)), so companion
	// discovery must find the index already sufficient and emit no second scan.
	// If either assertion fails, the reference is measuring some other plan and
	// §7's bound would be written against the wrong thing — that is a finding
	// about companion discovery, not a knob to loosen.
	if strings.Contains(refPlan, "GroupExistenceMerge") {
		t.Fatalf("regime %s: the grouped COUNT(*) reference is its own existence oracle (§5.3(a)) and must not be companion-joined; got %s", name, refPlan)
	}
	if n := strings.Count(refPlan, "AggregateIndex("); n != 1 {
		t.Fatalf("regime %s: the reference must be a single aggregate-index scan; found %d AggregateIndex scans in %s", name, n, refPlan)
	}

	// Time each side twice and keep the faster run, so a cold first touch of a
	// freshly written index subspace is not reported as the plan's cost.
	best := func(h *stressHarness, q string) (int, float64) {
		bestMs := -1.0
		rowCount := -1
		for range 2 {
			r := h.timeQuery(q)
			r.mustSucceed(t, "HAVING")
			ms := float64(r.Duration.Microseconds()) / 1000.0
			if bestMs < 0 || ms < bestMs {
				bestMs = ms
			}
			rowCount = r.RowCount
		}
		return rowCount, bestMs
	}

	mergeRows, mergeMs := best(withAgg, query)
	baseRows, baseMs := best(noAgg, query)
	refRows, refMs := best(refCount, refQuery)

	// Correctness first — the one invariant, and it holds regardless of which
	// plan is faster.
	if mergeRows != baseRows {
		t.Fatalf("regime %s: the two plans disagree: merge %d rows, base scan %d rows", name, mergeRows, baseRows)
	}
	if mergeRows != expectedRows {
		t.Fatalf("regime %s: both plans returned %d rows, but the data says %d groups have SUM(amount) > %d",
			name, mergeRows, expectedRows, threshold)
	}
	if refRows != refExpectedRows {
		t.Fatalf("regime %s: the reference returned %d rows, but the data says %d groups have COUNT(*) > %d",
			name, refRows, refExpectedRows, refThreshold)
	}

	winner := "merge"
	if baseMs < mergeMs {
		winner = "base"
	}
	t.Logf("GEMERGE_REGIME name=%s rows=%d groups=%d rowsPerGroup=%.2f having=%d resultRows=%d mergeMs=%.2f baseMs=%.2f ratio=%.2f winner=%s",
		name, rows, groups, float64(rows)/float64(groups), threshold, mergeRows,
		mergeMs, baseMs, baseMs/mergeMs, winner)
	// The single-BY_GROUP-scan reference, on its own line so the merge-vs-base
	// line keeps its existing field set. mergeOverRef is what §7's bound is read
	// from: how many times the correct two-stream merge costs over the
	// single-scan shape it stands in for.
	t.Logf("GEMERGE_REFERENCE name=%s rows=%d groups=%d refHaving=%d refUniform=%t refRows=%d refMs=%.2f mergeMs=%.2f mergeOverRef=%.2f",
		name, rows, groups, refThreshold, refUniform, refRows, refMs, mergeMs, mergeMs/refMs)

	// §7's acceptance criterion, ASSERTED. It was computed and logged and
	// nothing checked it, which makes it a number in a report rather than a
	// criterion: a regression to 10x would have shipped green, and the RFC's
	// justification for choosing 3.5 over 3 is noise-tolerance reasoning that
	// only means anything for a bound that can fail.
	//
	// Scoped to the regimes where the reference actually measures scan work.
	// Below groupExistenceBoundMinGroups the reference reads so few index
	// entries that its runtime is dominated by fixed per-query cost, so the
	// ratio measures harness overhead rather than the merge — the `low` regime
	// lands at ~0.95-1.01 and would only add noise to a bound about scan cost.
	// Excluding it is not excluding an inconvenient result: the merge is at its
	// CHEAPEST relative to the reference there.
	if groups >= groupExistenceBoundMinGroups {
		if ratio := mergeMs / refMs; ratio > groupExistenceRefBound {
			t.Errorf("regime %s: the merge costs %.2fx the single-BY_GROUP-scan reference, "+
				"over §7's bound of %.2fx (merge %.2fms, reference %.2fms).\n"+
				"§7 bounds the correct plan against the shape it stands in for — the "+
				"pre-RFC aggregate-index-only plan is unconstructible by design, so this "+
				"reference is what the criterion is measured against. Exceeding it means "+
				"group existence now costs materially more than reading one aggregate "+
				"index, which is the trade the RFC was accepted on.",
				name, ratio, groupExistenceRefBound, mergeMs, refMs)
		}
	}
	if winner == "base" {
		// Not a failure — this is the RE-OPENER, and the reason this test walks
		// the axis instead of asserting one point. §5.3.1 no longer predicts this
		// outcome; it records that the flip was struck because the measurement
		// went the other way at every regime. So reaching this branch contradicts
		// the amended text: it means the cost-model flip §5.3.1 describes and
		// rejects has become worth building after all, and §7.'s bound must be
		// rewritten against the regime boundary this run reports.
		t.Logf("GEMERGE_BASE_WINS name=%s groups=%d base=%.2fms merge=%.2fms — the struck §5.3.1 flip is re-armed at this regime",
			name, groups, baseMs, mergeMs)
	}
}
