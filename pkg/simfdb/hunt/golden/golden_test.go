package golden

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpus is the curated set of characterization scenarios. Data is literal (not random) so the
// baseline is stable and human-reviewable. Queries span the read surface: point/range filters,
// aggregates, GROUP BY, ORDER BY, LIMIT, IN, NULL semantics, DISTINCT, index-eligible predicates,
// and boolean/string columns. Add scenarios/queries here, then GOLDEN_UPDATE=1 to record them.
func corpus() []Scenario {
	orders := Scenario{
		Name: "orders",
		Seed: 1,
		Tables: []string{
			"CREATE TABLE t (id BIGINT NOT NULL, cat BIGINT, val BIGINT, name STRING, flag BOOLEAN, PRIMARY KEY (id))",
			"CREATE INDEX idx_cat ON t(cat)",
			"CREATE INDEX idx_val ON t(val)",
		},
		Data: []string{
			"INSERT INTO t (id, cat, val, name, flag) VALUES (1, 1, 100, 'a', true)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (2, 1, 200, 'b', false)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (3, 2, 150, 'c', true)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (4, 2, 150, 'd', false)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (5, 3, NULL, 'e', true)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (6, 3, 300, NULL, false)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (7, 1, 50, 'g', true)",
			"INSERT INTO t (id, cat, val, name, flag) VALUES (8, 2, 250, 'h', NULL)",
		},
		Queries: []string{
			"SELECT id, cat, val, name, flag FROM t ORDER BY id",
			"SELECT COUNT(*) FROM t",
			"SELECT COUNT(val), COUNT(name), COUNT(flag) FROM t",
			"SELECT MIN(val), MAX(val), SUM(val) FROM t",
			"SELECT cat, COUNT(*), SUM(val) FROM t GROUP BY cat ORDER BY cat",
			"SELECT id FROM t WHERE cat = 2 ORDER BY id",
			"SELECT id, val FROM t WHERE val > 150 ORDER BY id",
			"SELECT id FROM t WHERE val IS NULL ORDER BY id",
			"SELECT id FROM t WHERE val IS NOT NULL AND val <= 150 ORDER BY id",
			"SELECT id FROM t WHERE cat IN (1, 3) ORDER BY id",
			"SELECT id, val FROM t ORDER BY val, id LIMIT 4",
			"SELECT DISTINCT cat FROM t ORDER BY cat",
			"SELECT id FROM t WHERE flag = true ORDER BY id",
			"SELECT id, name FROM t WHERE name IS NULL ORDER BY id",
		},
	}
	// joins: two tables + a secondary index on the FK, so the planner has a real join-order /
	// join-method choice, plus grouped aggregation over the join.
	joins := Scenario{
		Name: "joins",
		Seed: 2,
		Tables: []string{
			"CREATE TABLE cust (cid BIGINT NOT NULL, region BIGINT, PRIMARY KEY (cid))",
			"CREATE TABLE ord (oid BIGINT NOT NULL, cid BIGINT, amt BIGINT, PRIMARY KEY (oid))",
			"CREATE INDEX ord_cid ON ord(cid)",
		},
		Data: []string{
			"INSERT INTO cust (cid, region) VALUES (1, 10)",
			"INSERT INTO cust (cid, region) VALUES (2, 20)",
			"INSERT INTO cust (cid, region) VALUES (3, 10)",
			"INSERT INTO ord (oid, cid, amt) VALUES (100, 1, 50)",
			"INSERT INTO ord (oid, cid, amt) VALUES (101, 1, 70)",
			"INSERT INTO ord (oid, cid, amt) VALUES (102, 2, 30)",
			"INSERT INTO ord (oid, cid, amt) VALUES (103, 3, 90)",
		},
		Queries: []string{
			"SELECT c.cid, o.oid, o.amt FROM cust c JOIN ord o ON c.cid = o.cid ORDER BY c.cid, o.oid",
			"SELECT c.region, SUM(o.amt) FROM cust c JOIN ord o ON c.cid = o.cid GROUP BY c.region ORDER BY c.region",
			"SELECT o.oid FROM ord o JOIN cust c ON o.cid = c.cid WHERE c.region = 10 ORDER BY o.oid",
			"SELECT COUNT(*) FROM cust c JOIN ord o ON c.cid = o.cid",
		},
	}

	// multikey: a composite primary key (region, id) — locks composite key encoding and
	// prefix-range planning (WHERE region = ? [AND id > ?]).
	multikey := Scenario{
		Name: "multikey",
		Seed: 3,
		Tables: []string{
			"CREATE TABLE t (region BIGINT NOT NULL, id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (region, id))",
		},
		Data: []string{
			"INSERT INTO t (region, id, v) VALUES (1, 1, 100)",
			"INSERT INTO t (region, id, v) VALUES (1, 2, 200)",
			"INSERT INTO t (region, id, v) VALUES (1, 3, 300)",
			"INSERT INTO t (region, id, v) VALUES (2, 1, 400)",
			"INSERT INTO t (region, id, v) VALUES (2, 2, 500)",
			"INSERT INTO t (region, id, v) VALUES (3, 1, 600)",
		},
		Queries: []string{
			"SELECT region, id, v FROM t ORDER BY region, id",
			"SELECT id, v FROM t WHERE region = 1 ORDER BY id",
			"SELECT v FROM t WHERE region = 2 AND id = 2",
			"SELECT region, id FROM t WHERE region = 2 AND id > 1 ORDER BY id",
			"SELECT region, COUNT(*), SUM(v) FROM t GROUP BY region ORDER BY region",
		},
	}

	// aggidx: aggregate indexes (SUM/COUNT grouped) — the planner should answer the matching
	// grouped aggregate straight from the index (an AggregateIndex scan), a shape the golden
	// locks so a cost-model change that stops using the index shows up as a plan diff.
	aggidx := Scenario{
		Name: "aggidx",
		Seed: 4,
		Tables: []string{
			"CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))",
			"CREATE INDEX sum_by_g AS SELECT SUM(v) FROM t GROUP BY g",
			"CREATE INDEX cnt_by_g AS SELECT COUNT(*) FROM t GROUP BY g",
		},
		Data: []string{
			"INSERT INTO t (id, g, v) VALUES (1, 1, 10)",
			"INSERT INTO t (id, g, v) VALUES (2, 1, 20)",
			"INSERT INTO t (id, g, v) VALUES (3, 2, 30)",
			"INSERT INTO t (id, g, v) VALUES (4, 2, 40)",
			"INSERT INTO t (id, g, v) VALUES (5, 3, 50)",
			"INSERT INTO t (id, g, v) VALUES (6, 1, 60)",
			"INSERT INTO t (id, g, v) VALUES (7, 4, NULL)", // g=4 is an ALL-NULL group: the atomic SUM_LONG
			"INSERT INTO t (id, g, v) VALUES (8, 4, NULL)", // mutation skips NULL so SUM DROPS g=4 (Java parity),
			// but COUNT(*) counts rows regardless of NULL v so it KEEPS [4,2]. The golden locks that contrast
			// (SUM has no g=4 row; COUNT shows [4,2]) — the SUM side is the positive check that the agg-7
			// retraction reasoning holds, and it completes the null-group trio: SUM drops, COUNT keeps,
			// MIN/MAX keeps-with-NULL (setops).
		},
		Queries: []string{
			"SELECT g, SUM(v) FROM t GROUP BY g ORDER BY g",
			"SELECT g, COUNT(*) FROM t GROUP BY g ORDER BY g",
			"SELECT SUM(v) FROM t",
			"SELECT MIN(v), MAX(v) FROM t",
		},
	}

	// setops captures the query shapes the RFC-199 sqlpage/metamorphic findings touched — multi-value
	// IN (InJoin over a concat), UNION ALL (RecordQueryUnionPlan over the same concat), and grouped
	// MIN/MAX (permuted_min/max after the #474 fix). The MIN/MAX PLAN string is invariant across the
	// ever→permuted change (both render AggregateIndex(MAX|MIN,…) — the operator is AggMax/AggMin, not
	// the index type), so cat=4 is an ALL-NULL group: the permuted maintainer keeps it with a NULL
	// extremum ([4,NULL]), whereas the old ever-index dropped it entirely. That row in the golden ROWS
	// is the real regression sentinel — revert to the ever-index and this baseline loses [4,NULL].
	setops := Scenario{
		Name: "setops",
		Seed: 5,
		Tables: []string{
			"CREATE TABLE t (id BIGINT NOT NULL, cat BIGINT, v BIGINT, PRIMARY KEY (id))",
			"CREATE INDEX cat_idx ON t (cat)",
			"CREATE INDEX max_by_cat AS SELECT MAX(v) FROM t GROUP BY cat",
			"CREATE INDEX min_by_cat AS SELECT MIN(v) FROM t GROUP BY cat",
		},
		Data: []string{
			"INSERT INTO t (id, cat, v) VALUES (1, 1, 10)",
			"INSERT INTO t (id, cat, v) VALUES (2, 1, 20)",
			"INSERT INTO t (id, cat, v) VALUES (3, 2, 30)",
			"INSERT INTO t (id, cat, v) VALUES (4, 2, 40)",
			"INSERT INTO t (id, cat, v) VALUES (5, 3, 50)",
			"INSERT INTO t (id, cat, v) VALUES (6, 4, NULL)", // cat=4 is an ALL-NULL group: the permuted
			"INSERT INTO t (id, cat, v) VALUES (7, 4, NULL)", // MIN/MAX keeps it as [4,NULL]; ever dropped it
		},
		Queries: []string{
			"SELECT id FROM t WHERE cat IN (1, 2) ORDER BY id",
			"SELECT cat, MAX(v) FROM t GROUP BY cat ORDER BY cat",
			"SELECT cat, MIN(v) FROM t GROUP BY cat ORDER BY cat",
		},
	}

	// subquery: correlated + non-correlated scalar subqueries, EXISTS, and HAVING — the executor's
	// FlatMap / scalar-subquery-cursor / streaming-aggregate paths, pinned so a plan or result change
	// in any of them surfaces as a reviewable golden diff.
	subquery := Scenario{
		Name: "subquery",
		Seed: 6,
		Tables: []string{
			"CREATE TABLE cust (cid BIGINT NOT NULL, region BIGINT, PRIMARY KEY (cid))",
			"CREATE TABLE ord (oid BIGINT NOT NULL, cid BIGINT, amt BIGINT, PRIMARY KEY (oid))",
			"CREATE INDEX ord_cid ON ord(cid)",
		},
		Data: []string{
			"INSERT INTO cust (cid, region) VALUES (1, 10)",
			"INSERT INTO cust (cid, region) VALUES (2, 20)",
			"INSERT INTO cust (cid, region) VALUES (3, 10)",
			"INSERT INTO ord (oid, cid, amt) VALUES (100, 1, 50)",
			"INSERT INTO ord (oid, cid, amt) VALUES (101, 1, 70)",
			"INSERT INTO ord (oid, cid, amt) VALUES (102, 2, 30)",
			"INSERT INTO ord (oid, cid, amt) VALUES (103, 3, 90)",
		},
		Queries: []string{
			"SELECT c.cid, (SELECT COUNT(*) FROM ord o WHERE o.cid = c.cid) FROM cust c ORDER BY c.cid",
			"SELECT c.cid FROM cust c WHERE EXISTS (SELECT 1 FROM ord o WHERE o.cid = c.cid) ORDER BY c.cid",
			"SELECT oid FROM ord WHERE amt > (SELECT MIN(amt) FROM ord) ORDER BY oid",
			"SELECT c.region, COUNT(*) FROM cust c JOIN ord o ON c.cid = o.cid GROUP BY c.region HAVING COUNT(*) > 1 ORDER BY c.region",
		},
	}

	return []Scenario{orders, joins, multikey, aggidx, setops, subquery}
}

// TestGolden captures each scenario over SimFDB and diffs it against the committed baseline in
// testdata/. A mismatch fails with a reviewable diff. GOLDEN_UPDATE=1 regenerates the baselines
// (review the diff before committing). The capture is asserted deterministic first — a golden is
// only meaningful if the same scenario yields identical bytes every run.
func TestGolden(t *testing.T) {
	t.Parallel()
	update := os.Getenv("GOLDEN_UPDATE") != ""
	for _, s := range corpus() {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			got, err := Capture(s)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			// Determinism: the baseline is meaningless if capture varies run-to-run.
			if again, err := Capture(s); err != nil {
				t.Fatalf("recapture: %v", err)
			} else if again != got {
				t.Fatalf("NONDETERMINISTIC capture for %q:\n%s", s.Name, firstDiff(got, again))
			}

			path := filepath.Join("testdata", s.Name+".golden")
			if update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("updated %s (%d bytes)", path, len(got))
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run GOLDEN_UPDATE=1 to create): %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("GOLDEN MISMATCH for %q — behavior changed (result and/or plan). "+
					"If intended, GOLDEN_UPDATE=1 and review the diff:\n%s", s.Name, firstDiff(string(want), got))
			}
		})
	}
}

// firstDiff renders an LCS line diff (baseline "-" vs current "+"), so an inserted/removed line
// doesn't smear every following line into a spurious mismatch (a positional diff's flaw). The
// authoritative review artifact is still `git diff testdata/*.golden` after GOLDEN_UPDATE=1; this
// is the in-test convenience. Golden files are small, so the O(n·m) table is fine.
func firstDiff(want, got string) string {
	a := strings.Split(want, "\n")
	bb := strings.Split(got, "\n")
	la, lb := len(a), len(bb)
	lcs := make([][]int, la+1)
	for i := range lcs {
		lcs[i] = make([]int, lb+1)
	}
	for i := la - 1; i >= 0; i-- {
		for j := lb - 1; j >= 0; j-- {
			if a[i] == bb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var b strings.Builder
	const capLines = 60
	i, j, shown := 0, 0, 0
	for i < la && j < lb && shown < capLines {
		switch {
		case a[i] == bb[j]:
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&b, "  - %s\n", a[i])
			i, shown = i+1, shown+1
		default:
			fmt.Fprintf(&b, "  + %s\n", bb[j])
			j, shown = j+1, shown+1
		}
	}
	for ; i < la && shown < capLines; i, shown = i+1, shown+1 {
		fmt.Fprintf(&b, "  - %s\n", a[i])
	}
	for ; j < lb && shown < capLines; j, shown = j+1, shown+1 {
		fmt.Fprintf(&b, "  + %s\n", bb[j])
	}
	if shown == 0 {
		return "  (no line-level difference found)"
	}
	return b.String()
}

// TestCaptureRefusesToBakeAnErrorIntoABaseline pins that a scenario whose query does not run is
// a capture FAILURE, not baseline content.
//
// Capture used to write "PLAN-ERR: ..." / "ROWS-ERR: ..." into the text and return it as a
// successful capture. GOLDEN_UPDATE=1 then recorded the error message as the baseline, and the
// entry passed forever by reproducing its own breakage — a characterization harness certifying
// that a query still fails the same way, green on every CI summary.
//
// Both surfaces are checked separately: EXPLAIN can fail where the query itself would not (an
// unplannable shape), and a query can fail after EXPLAIN succeeded (an executor error). A guard
// on only one of them still lets the other bake in.
//
// So each case declares the surface it is supposed to exercise, and the test ASSERTS it. Without
// that, "both surfaces are checked" is a claim about inputs nobody re-checks: this table once held
// three cases described as covering both, and all three failed at EXPLAIN — 42703 (unknown
// column), 42F01 (unknown table) and 42804 (incompatible comparison operands, which the planner
// rejects rather than the executor). The rows guard had ZERO cases and deleting it would not have
// turned anything red. The surface assertion is what makes a case silently migrating across the
// boundary — a semantic check moving into the planner, say — show up as a red test instead of as
// lost coverage.
func TestCaptureRefusesToBakeAnErrorIntoABaseline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		query   string
		surface CaptureSurface
	}{
		// ---- plan surface: rejected before anything executes.
		// A column that does not exist.
		{"unknown column", "SELECT nosuchcolumn FROM t", SurfacePlan},
		// A table that does not exist.
		{"unknown table", "SELECT * FROM nosuchtable", SurfacePlan},
		// Comparing a BIGINT column against a string literal: the planner rejects the operand
		// types, so this never reaches the executor.
		{"bad literal", "SELECT id FROM t WHERE id = 'not-a-number'", SurfacePlan},

		// ---- rows surface: plans fine, fails while producing rows.
		// Division by zero on the first row. Written against a COLUMN (id - 1 with id = 1) rather
		// than the constant `id / 0`, so constant folding cannot move the failure into the planner
		// and quietly return this case to the plan surface.
		{"divide by zero", "SELECT id / (id - 1) FROM t", SurfaceRows},
		// A cast that is well-typed but fails on the actual data.
		{"failing cast", "SELECT CAST(name AS BIGINT) FROM t", SurfaceRows},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := Capture(Scenario{
				Name:   "err-" + tc.name,
				Seed:   99,
				Tables: []string{"CREATE TABLE t (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))"},
				Data:   []string{"INSERT INTO t (id, name) VALUES (1, 'x')"},
				// A good query first, so the failure is not the very first thing captured and
				// the guard cannot pass by accident of ordering.
				Queries: []string{"SELECT id FROM t ORDER BY id", tc.query},
			})
			if err == nil {
				t.Fatalf("Capture returned no error for a failing query; a baseline recorded "+
					"from this would pass forever by reproducing the breakage. Captured:\n%s", out)
			}
			if out != "" {
				t.Fatalf("Capture returned text alongside its error; GOLDEN_UPDATE=1 must have "+
					"nothing to write. Got:\n%s", out)
			}
			var ce *CaptureError
			if !errors.As(err, &ce) {
				t.Fatalf("Capture error is not a *CaptureError, so no test can tell which guard "+
					"produced it: %v", err)
			}
			if ce.Surface != tc.surface {
				t.Fatalf("failed on the %s surface, want %s. This case no longer exercises the "+
					"guard it was added for; the %s guard may now have no case at all. Error: %v",
					ce.Surface, tc.surface, tc.surface, err)
			}
			for _, marker := range []string{"PLAN-ERR", "ROWS-ERR"} {
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("the error still carries the %s baseline marker: %v", marker, err)
				}
			}
		})
	}
}
