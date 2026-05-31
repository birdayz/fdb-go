package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// RFC-048 W3.5: the plan-diversity oracle (Cascades-specific, self-oracling).
//
// A Cascades optimizer's defining hazard is that one logical query has many
// legal physical plans, and an unsound transformation rule returns wrong rows
// only on the plan it fires on. Differential-vs-Java (W3) compares Go's chosen
// plan against Java's chosen plan — it does NOT compare Go's chosen plan
// against Go's *other* legal plans, which is exactly where a bad implementation
// rule hides. This oracle does: it runs the SAME query against two schemas that
// differ only in available access paths (one with secondary indexes, one
// without), so the optimizer is forced to pick a materially different physical
// plan (index scan vs full scan + filter) for the same logical query. All
// plans must return byte-identical row sets. If an index-matching rule is
// unsound (wrong range, wrong bounds, dropped rows), the two diverge.
//
// Non-vacuity is asserted explicitly: EXPLAIN must show the indexed schema
// actually using an index where the unindexed one does not — otherwise the two
// "plans" are the same and the oracle proves nothing.

// pdDB builds a t(id,a,b,c) schema, optionally with secondary indexes on a and
// b, seeded identically from `seed`.
func pdDB(t *testing.T, withIndexes bool, seed int64) *sql.DB {
	t.Helper()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	tag := "noidx"
	if withIndexes {
		tag = "idx"
	}
	dbPath := fmt.Sprintf("/pd_%s_%d_%s", tag, seed, t.Name())
	db := openTestDB(t, dbPath)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbPath)); err != nil {
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	tmpl := fmt.Sprintf("PD_TMPL_%s_%d_%s", tag, seed, t.Name())
	ddl := fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, b BIGINT, c STRING, PRIMARY KEY (id))`, tmpl)
	if withIndexes {
		ddl += "\n\t\tCREATE INDEX idx_a ON t (a)\n\t\tCREATE INDEX idx_b ON t (b)"
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("CREATE SCHEMA TEMPLATE: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/s WITH TEMPLATE %s", dbPath, tmpl)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	sdb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbPath, clusterFilePath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { sdb.Close() })

	rng := rand.New(rand.NewSource(seed))
	strs := []string{"x", "y", "z"}
	for i := int64(1); i <= 60; i++ {
		var a, b, c string
		if rng.Float64() > 0.2 {
			a = fmt.Sprintf("%d", rng.Intn(15))
		} else {
			a = "null"
		}
		if rng.Float64() > 0.2 {
			b = fmt.Sprintf("%d", rng.Intn(15)-7)
		} else {
			b = "null"
		}
		if rng.Float64() > 0.2 {
			c = fmt.Sprintf("'%s'", strs[rng.Intn(len(strs))])
		} else {
			c = "null"
		}
		ins := fmt.Sprintf("INSERT INTO t VALUES (%d, %s, %s, %s)", i, a, b, c)
		if _, err := sdb.ExecContext(ctx, ins); err != nil {
			t.Fatalf("INSERT: %v (%s)", err, ins)
		}
	}
	return sdb
}

// canonRows renders a result set as a sorted, comparable multiset of rows so
// two plans can be compared regardless of emission order.
func canonRows(rows [][]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = fmt.Sprintf("%v", c)
		}
		out = append(out, strings.Join(cells, "|"))
	}
	sort.Strings(out)
	return out
}

func sameRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFDB_W3_5_PlanDiversity_IndexedVsFullScan(t *testing.T) {
	t.Parallel()
	const seed = 0xd1303

	idx := pdDB(t, true, seed)
	noidx := pdDB(t, false, seed)

	// A battery of queries whose physical plan depends on the available access
	// paths. Each must return the same rows whether served by an index scan or a
	// full scan + residual filter.
	queries := []string{
		"SELECT id, a FROM t WHERE a = 7",
		"SELECT id, a FROM t WHERE a > 5",
		"SELECT id, a FROM t WHERE a >= 3 AND a <= 9",
		// Upper-only range: the index plan must EXCLUDE NULL entries (NULL sorts
		// first). This line is the regression sentinel for the NULL-range fix in
		// scanComparisonsToTupleRange — before it, the index scan returned NULLs
		// that `a < 4` (UNKNOWN on NULL) must drop.
		"SELECT id FROM t WHERE a < 4",
		"SELECT id FROM t WHERE a <= 4",
		"SELECT id, b FROM t WHERE b = 0",
		"SELECT id, b FROM t WHERE b > -3",
		"SELECT id FROM t WHERE a = 7 AND b > 0",
		"SELECT id FROM t WHERE a IS NULL",
		"SELECT id FROM t WHERE a IS NOT NULL",
		"SELECT COUNT(*) FROM t WHERE a > 5",
		"SELECT a, COUNT(*) FROM t WHERE a IS NOT NULL GROUP BY a",
		"SELECT id, a FROM t WHERE a > 5 ORDER BY a",
		// NOTE: `... WHERE a IN (...)` is intentionally NOT in this battery.
		// W3.5 found that an IN over an indexed column plans as
		// InJoin(IndexScan) and drops the outer projection — `SELECT id`
		// returns [id, a] instead of [id] — a real, pre-existing bug in a
		// separate subsystem (Project-over-InJoin planning), tracked in TODO.md
		// with a deterministic repro. It is out of scope for the index-range
		// correctness fix this oracle's other lines pin; re-add this shape once
		// the projection bug is fixed.
	}
	for _, q := range queries {
		gotIdx := canonRows(collectRows(t, idx, q))
		gotNo := canonRows(collectRows(t, noidx, q))
		if !sameRows(gotIdx, gotNo) {
			t.Fatalf("plan-diversity mismatch for %q:\n indexed: %v\n  noidx: %v", q, gotIdx, gotNo)
		}
	}
}

func TestFDB_W3_5_PlanDiversity_PlansActuallyDiffer(t *testing.T) {
	t.Parallel()
	const seed = 0xd1304
	ctx := context.Background()

	idx := pdDB(t, true, seed)
	noidx := pdDB(t, false, seed)

	// Non-vacuity guard: at least one battery query must be planned with an
	// index on the indexed schema and WITHOUT one on the unindexed schema. If
	// the EXPLAINs were identical, the result-equality test above would be
	// comparing a plan against itself and proving nothing.
	probes := []string{
		"SELECT id, a FROM t WHERE a = 7",
		"SELECT id, a FROM t WHERE a > 5",
		"SELECT id, b FROM t WHERE b = 0",
	}
	diverged := false
	for _, q := range probes {
		pIdx := planExplainVia(t, ctx, idx, q)
		pNo := planExplainVia(t, ctx, noidx, q)
		idxUsesIndex := strings.Contains(strings.ToUpper(pIdx), "INDEX")
		noUsesIndex := strings.Contains(strings.ToUpper(pNo), "INDEX")
		if pIdx != pNo && idxUsesIndex && !noUsesIndex {
			t.Logf("plan diversity confirmed for %q:\n  indexed: %s\n  noidx:   %s", q, pIdx, pNo)
			diverged = true
		}
	}
	if !diverged {
		t.Fatalf("no query produced a divergent plan (index scan vs full scan) — the oracle would be vacuous; check index DDL / EXPLAIN output")
	}
}
