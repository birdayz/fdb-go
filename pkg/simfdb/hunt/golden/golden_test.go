package golden

import (
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
	return []Scenario{orders}
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
