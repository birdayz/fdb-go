package sqldriver_test

// Equivalences between whole QUERY SHAPES: grouped aggregates against per-group
// scalar ones, joins against their comma spelling and against their anti-join
// form, and a descending order against a reversed ascending one.
//
// These pit entire execution strategies against each other rather than two
// spellings of one expression. The grouped/per-group pair is the sharpest: a
// grouped aggregate is answered from an AGGREGATE INDEX, while the same
// aggregate under an equality on the grouping key is answered by a filtered
// scan — so the pair compares the index's accumulated answer with one computed
// from the records, group by group. That is exactly the comparison that exposed
// the permuted-MIN NULL defect when it was made by hand.
//
// SUM aggregates a NULL-FREE column throughout. SUM over a group whose last
// non-NULL value was removed is a KNOWN divergence (sumResidualZero), and a
// sweep that kept rediscovering a pinned defect would report it on every run and
// bury anything new.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

func TestFDB_MetamorphicShapeEquivalenceSweep(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_shapeequiv", "sheq",
		"CREATE TABLE t (id BIGINT, g BIGINT, v BIGINT, n BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE u (uid BIGINT, ug BIGINT, uv BIGINT, PRIMARY KEY (uid)) ",
		"CREATE INDEX t_g ON t (g) "+
			"CREATE INDEX t_cnt AS SELECT COUNT(*) FROM t GROUP BY g "+
			"CREATE INDEX t_sum AS SELECT SUM(n) FROM t GROUP BY g "+
			"CREATE INDEX t_min AS SELECT MIN(v) FROM t GROUP BY g "+
			"CREATE INDEX t_max AS SELECT MAX(v) FROM t GROUP BY g "+
			"CREATE INDEX u_ug ON u (ug) ")

	r := rand.New(rand.NewSource(777))
	// v is nullable (the extremum column); n is never NULL (the summed column).
	var trows []string
	for i := 1; i <= 240; i++ {
		v := "NULL"
		if r.Intn(100) >= 35 {
			v = fmt.Sprintf("%d", r.Intn(21)-10)
		}
		trows = append(trows, fmt.Sprintf("(%d, %d, %s, %d)", i, r.Intn(8), v, r.Intn(15)-7))
	}
	for start := 0; start < len(trows); start += 40 {
		end := start + 40
		if end > len(trows) {
			end = len(trows)
		}
		w.Exec("INSERT INTO t (id, g, v, n) VALUES " + strings.Join(trows[start:end], ", "))
	}
	var urows []string
	for i := 1; i <= 120; i++ {
		urows = append(urows, fmt.Sprintf("(%d, %d, %d)", 500+i, r.Intn(10), r.Intn(20)))
	}
	for start := 0; start < len(urows); start += 40 {
		end := start + 40
		if end > len(urows) {
			end = len(urows)
		}
		w.Exec("INSERT INTO u (uid, ug, uv) VALUES " + strings.Join(urows[start:end], ", "))
	}

	seed := int64(1)
	if s := os.Getenv("SHEQ_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	_ = seed

	okByRule := map[string]int{}
	equiv := func(rule, qa, qb string) {
		t.Helper()
		ia, ea := mmRows(t, ctx, w.idx, qa)
		ib, eb := mmRows(t, ctx, w.idx, qb)
		if ea != nil || eb != nil {
			t.Errorf("[%s] a query failed\n  A: %s -> %v\n  B: %s -> %v", rule, qa, ea, qb, eb)
			return
		}
		okByRule[rule]++
		if !mmEqRows(ia, ib) {
			t.Errorf("SHAPE MISMATCH [%s]\n  A: %s\n  B: %s\n  A gives %v\n  B gives %v\n  %s",
				rule, qa, qb, mmHeadRows(ia), mmHeadRows(ib), mmFirstDiff(ia, ib))
		}
	}

	// ---- grouped aggregate vs per-group scalar aggregate ----------------
	//
	// The grouped form reads an aggregate index; the per-group form filters and
	// recomputes. Every group is checked, including the ones whose extremum is
	// NULL, which is where the two paths most recently disagreed.
	groups, err := mmRows(t, ctx, w.plain, "SELECT g FROM t GROUP BY g ORDER BY g")
	if err != nil {
		t.Fatalf("group probe: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("no groups in the fixture, so every per-group comparison below is vacuous")
	}
	for _, agg := range []struct{ name, expr string }{
		{"count", "COUNT(*)"},
		{"sum", "SUM(n)"},
		{"min", "MIN(v)"},
		{"max", "MAX(v)"},
		{"countcol", "COUNT(v)"},
	} {
		for _, gv := range groups {
			equiv("grouped-vs-pergroup-"+agg.name,
				fmt.Sprintf("SELECT %s FROM t WHERE g = %s", agg.expr, gv),
				fmt.Sprintf("SELECT x FROM (SELECT g, %s AS x FROM t GROUP BY g) AS s WHERE s.g = %s",
					agg.expr, gv))
		}
	}

	// ---- descending order vs a reversed ascending one -------------------
	//
	// A descending scan of an index is a different traversal, not a different
	// answer. Compared by taking a bounded page off each end, so a mismatch is a
	// short list rather than a whole-table diff.
	equiv("desc-head-vs-asc-tail",
		"SELECT id FROM t ORDER BY g DESC, id DESC LIMIT 5",
		"SELECT id FROM (SELECT id, g FROM t ORDER BY g, id) AS s ORDER BY s.g DESC, s.id DESC LIMIT 5")
	equiv("desc-full-vs-asc-full",
		"SELECT COUNT(*) FROM (SELECT id FROM t ORDER BY g DESC, id DESC) AS s",
		"SELECT COUNT(*) FROM (SELECT id FROM t ORDER BY g, id) AS s")

	// ---- join spellings --------------------------------------------------
	equiv("join-on-vs-comma",
		"SELECT t.id, u.uid FROM t JOIN u ON t.g = u.ug ORDER BY t.id, u.uid LIMIT 25",
		"SELECT t.id, u.uid FROM t, u WHERE t.g = u.ug ORDER BY t.id, u.uid LIMIT 25")
	equiv("join-commutes",
		"SELECT t.id, u.uid FROM t JOIN u ON t.g = u.ug ORDER BY t.id, u.uid LIMIT 25",
		"SELECT t.id, u.uid FROM u JOIN t ON t.g = u.ug ORDER BY t.id, u.uid LIMIT 25")
	equiv("inner-vs-left-join-filtered",
		"SELECT t.id, u.uid FROM t JOIN u ON t.g = u.ug ORDER BY t.id, u.uid LIMIT 25",
		"SELECT t.id, u.uid FROM t LEFT JOIN u ON t.g = u.ug WHERE u.uid IS NOT NULL "+
			"ORDER BY t.id, u.uid LIMIT 25")

	// The anti-join, in both of its standard spellings. A LEFT JOIN whose right
	// side stayed NULL is exactly the set NOT EXISTS keeps.
	equiv("not-exists-vs-left-join-null",
		"SELECT id FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE u.ug = t.g) ORDER BY id",
		"SELECT t.id FROM t LEFT JOIN u ON t.g = u.ug WHERE u.uid IS NULL ORDER BY t.id")
	equiv("not-exists-count",
		"SELECT COUNT(*) FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE u.ug = t.g)",
		"SELECT COUNT(*) FROM (SELECT t.id FROM t LEFT JOIN u ON t.g = u.ug WHERE u.uid IS NULL) AS s")

	// EXISTS keeps each outer row once however many inner rows match, which a
	// join does not — so the join spelling needs a DISTINCT to agree.
	equiv("exists-vs-distinct-join",
		"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE u.ug = t.g) ORDER BY id",
		"SELECT DISTINCT t.id FROM t JOIN u ON t.g = u.ug ORDER BY t.id")

	rules := []string{
		"grouped-vs-pergroup-count", "grouped-vs-pergroup-sum", "grouped-vs-pergroup-min",
		"grouped-vs-pergroup-max", "grouped-vs-pergroup-countcol",
		"desc-head-vs-asc-tail", "desc-full-vs-asc-full",
		"join-on-vs-comma", "join-commutes", "inner-vs-left-join-filtered",
		"not-exists-vs-left-join-null", "not-exists-count", "exists-vs-distinct-join",
	}
	total := 0
	for _, rule := range rules {
		t.Logf("  %-32s compared=%d", rule, okByRule[rule])
		total += okByRule[rule]
		if okByRule[rule] == 0 {
			t.Errorf("instrument dead for %s: nothing was compared", rule)
		}
	}
	t.Logf("groups=%d total-comparisons=%d", len(groups), total)
}
