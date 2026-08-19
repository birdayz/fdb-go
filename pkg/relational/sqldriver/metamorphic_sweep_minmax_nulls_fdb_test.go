package sqldriver_test

// A randomized sweep over MIN/MAX aggregate indexes whose aggregated column is
// NULLABLE, under continuous mutation.
//
// This is the regression net for the permuted-MIN null repair. The repair reads
// the ordinary subspace whenever the stored extremum is NULL, so what has to
// hold is a relationship between two structures that are maintained separately
// — the permuted extremum and the ordinary entries — across every write that
// can move a row between groups, into the NULL run, or out of it. Enumerated
// cases cover the transitions someone thought of; this covers the ones nobody
// did, by driving random mutations and comparing against the unindexed twin
// after every single one.
//
// The state is compared after EVERY mutation rather than at the end, because a
// divergence that appears at operation 40 and is then written over by operation
// 41 is invisible to a final comparison — and the operation that caused it is
// what makes it reproducible.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

func TestFDB_MetamorphicMinMaxNullSweep(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_mm_minmax", "mmmm",
		"CREATE TABLE t (id BIGINT, g BIGINT, h BIGINT, v BIGINT, s STRING, PRIMARY KEY (id)) ",
		"CREATE INDEX t_min_v_g AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max_v_g AS SELECT MAX(v) FROM t GROUP BY g "+
			"CREATE INDEX t_cnt_g AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_min_v_gh AS SELECT MIN(v) FROM t GROUP BY g, h "+
			"CREATE INDEX t_g ON t (g) ")

	seed := int64(1)
	if s := os.Getenv("MMNULL_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	ops := 120
	if s := os.Getenv("MMNULL_OPS"); s != "" {
		fmt.Sscan(s, &ops)
	}
	r := rand.New(rand.NewSource(seed))

	// A small group domain and a small value domain, so groups fill up, empty
	// out and refill — the churn the repair has to survive. NULLs are frequent
	// on purpose: they are the arm under test, and a rate that made them rare
	// would turn this into a sweep of the path that already worked.
	nullableInt := func(nullPct int) string {
		if r.Intn(100) < nullPct {
			return "NULL"
		}
		return fmt.Sprintf("%d", r.Intn(11)-5)
	}
	nullableStr := func(nullPct int) string {
		if r.Intn(100) < nullPct {
			return "NULL"
		}
		return []string{"''", "'a'", "'b'", "'zz'"}[r.Intn(4)]
	}
	group := func() string {
		// A NULL group key is in the domain: it groups like any other value and
		// the repair's seek prefix then contains a NULL of its own.
		if r.Intn(10) == 0 {
			return "NULL"
		}
		return fmt.Sprintf("%d", r.Intn(6))
	}

	nextID := 1
	insert := func(n int) {
		var rows []string
		for i := 0; i < n; i++ {
			rows = append(rows, fmt.Sprintf("(%d, %s, %d, %s, %s)",
				nextID, group(), r.Intn(3), nullableInt(40), nullableStr(40)))
			nextID++
		}
		w.Exec("INSERT INTO t (id, g, h, v, s) VALUES " + strings.Join(rows, ", "))
	}
	insert(40)

	// The aggregate queries compared after every mutation. Each is a different
	// read path: single aggregate, both extrema together (an intersection of two
	// aggregate indexes), the composite grouping, and the string-typed extremum.
	probes := []string{
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g",
		"SELECT g, MAX(v) FROM t GROUP BY g ORDER BY g",
		"SELECT g, MIN(v), MAX(v) FROM t GROUP BY g ORDER BY g",
		"SELECT g, MIN(v), COUNT(*) FROM t GROUP BY g ORDER BY g",
		"SELECT g, h, MIN(v) FROM t GROUP BY g, h ORDER BY g, h",
		"SELECT g FROM t GROUP BY g HAVING MIN(v) IS NULL ORDER BY g",
		"SELECT g FROM t GROUP BY g HAVING MIN(v) IS NOT NULL ORDER BY g",
		// DESCENDING, which may reverse the scan over the permuted subspace.
		// The repair's own seek is always FORWARD — the smallest non-NULL value
		// is at the low end of the group whichever way the groups are being
		// enumerated — so a reversed outer scan is exactly the case where a
		// repair that inherited the outer direction would answer with the
		// group's MAXIMUM instead.
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g DESC",
		"SELECT g, MIN(v), MAX(v) FROM t GROUP BY g ORDER BY g DESC",
		// A bounded page off each end.
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g LIMIT 3",
		"SELECT g, MIN(v) FROM t GROUP BY g ORDER BY g DESC LIMIT 3",
		// A range over the grouping key, so the scan starts mid-index.
		"SELECT g, MIN(v) FROM t GROUP BY g HAVING g >= 2 ORDER BY g",
	}

	compared := 0
	nonNullMinSeen := 0
	nullMinSeen := 0
	check := func(after string) {
		t.Helper()
		for _, q := range probes {
			gi, ei := mmRows(t, ctx, w.idx, q)
			gn, en := mmRows(t, ctx, w.plain, q)
			if ei != nil || en != nil {
				t.Fatalf("probe failed after %q\n  q: %s\n  indexed: %v\n  unindexed: %v", after, q, ei, en)
			}
			compared++
			if !mmEqRows(gi, gn) {
				t.Fatalf("AGGREGATE DIVERGENCE after %q\n  q: %s\n  %s\n  indexed  : %v\n  unindexed: %v\n"+
					"  reproduce with MMNULL_SEED=%d", after, q, mmFirstDiff(gi, gn), gi, gn, seed)
			}
		}
		// Track whether the sweep is actually reaching both arms of the repair.
		// A run in which no group ever has a NULL minimum exercises none of it
		// and its green means nothing; likewise a run where every minimum is
		// NULL never reaches the resolved-value arm.
		rows, err := mmRows(t, ctx, w.idx, "SELECT g, MIN(v) FROM t GROUP BY g")
		if err != nil {
			t.Fatalf("population probe: %v", err)
		}
		for _, row := range rows {
			if strings.HasSuffix(row, "|NULL") {
				nullMinSeen++
			} else {
				nonNullMinSeen++
			}
		}
	}
	check("initial load")

	for i := 0; i < ops; i++ {
		var stmt string
		switch r.Intn(9) {
		case 0, 1:
			stmt = fmt.Sprintf("INSERT INTO t (id, g, h, v, s) VALUES (%d, %s, %d, %s, %s)",
				nextID, group(), r.Intn(3), nullableInt(40), nullableStr(40))
			nextID++
		case 2, 3:
			// Re-value a row, often to or from NULL — the transition that moves
			// a group into or out of the repaired arm.
			stmt = fmt.Sprintf("UPDATE t SET v = %s WHERE id = %d", nullableInt(45), 1+r.Intn(nextID-1))
		case 4:
			// Move a row between groups, carrying its value with it.
			stmt = fmt.Sprintf("UPDATE t SET g = %s WHERE id = %d", group(), 1+r.Intn(nextID-1))
		case 5:
			stmt = fmt.Sprintf("UPDATE t SET s = %s WHERE id = %d", nullableStr(45), 1+r.Intn(nextID-1))
		case 6:
			stmt = fmt.Sprintf("DELETE FROM t WHERE id = %d", 1+r.Intn(nextID-1))
		case 7:
			// Predicate-driven delete: removes a whole group's worth at once,
			// including the row holding its extremum.
			stmt = fmt.Sprintf("DELETE FROM t WHERE g = %d", r.Intn(6))
		case 8:
			// Null out an entire group in one statement, so its minimum goes
			// from a value to NULL without any row leaving.
			stmt = fmt.Sprintf("UPDATE t SET v = NULL WHERE g = %d", r.Intn(6))
		}
		w.Exec(stmt)
		check(stmt)
	}

	t.Logf("seed=%d ops=%d comparisons=%d groups-with-a-value=%d groups-with-NULL-min=%d",
		seed, ops, compared, nonNullMinSeen, nullMinSeen)
	// Both arms of the repair must have been reached, or the run proves only
	// that the arm it happened to take is fine.
	if nonNullMinSeen == 0 {
		t.Errorf("no group ever had a non-NULL minimum: the resolve-the-value arm of the repair " +
			"was never exercised, so this run's green says nothing about it")
	}
	if nullMinSeen == 0 {
		t.Errorf("no group ever had a NULL minimum: the repair was never entered at all, so this " +
			"run's green is a statement about the unrepaired path only")
	}
}
