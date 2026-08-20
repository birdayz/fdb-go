package sqldriver_test

// Expression-level equivalences, the layer where the parenthesized-CASE defect
// lived.
//
// The rewrite sweep beside this one works on BOOLEAN structure — De Morgan,
// commutativity, parenthesization. This one works on the VALUE expressions
// inside and around those booleans, because that is the walker that mistook a
// grouping paren for a record constructor, and a defect there shows up as a
// wrong value rather than as a wrong row set.
//
// Every pair is exact under three-valued logic, and the ones that are NOT are
// deliberately absent — `CASE WHEN p THEN x ELSE y END` and
// `CASE WHEN NOT p THEN y ELSE x END` differ when p is UNKNOWN (the first yields
// y, the second x), so it is not a rule here. A sweep that asserts a false
// equivalence reports the engine as broken and teaches the reader to ignore it.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

func TestFDB_MetamorphicExpressionEquivalenceSweep(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_exprequiv", "xeq",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) CREATE INDEX t_c ON t (c) ")

	dataRand := rand.New(rand.NewSource(31337))
	const nRows = 150
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, mhRowLiteral(dataRand, i, false))
	}
	for start := 0; start < len(vals); start += 25 {
		end := start + 25
		if end > len(vals) {
			end = len(vals)
		}
		w.Exec("INSERT INTO t " + mhCols + " VALUES " + strings.Join(vals[start:end], ", "))
	}

	seed := int64(1)
	if s := os.Getenv("XEQ_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 40
	if s := os.Getenv("XEQ_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	r := rand.New(rand.NewSource(seed))
	g := &mhGen{r: r}

	okByRule := map[string]int{}
	errByRule := map[string]int{}
	equiv := func(rule, qa, qb string) {
		t.Helper()
		ia, ea := mmRows(t, ctx, w.idx, qa)
		ib, eb := mmRows(t, ctx, w.idx, qb)
		na, ena := mmRows(t, ctx, w.plain, qa)
		nb, enb := mmRows(t, ctx, w.plain, qb)
		if ea != nil || eb != nil || ena != nil || enb != nil {
			errByRule[rule]++
			if errByRule[rule] <= 1 {
				t.Logf("%s both-error sample: %v / %v\n  A: %s\n  B: %s", rule, ea, eb, qa, qb)
			}
			return
		}
		okByRule[rule]++
		if !mmEqRows(ia, ib) {
			t.Errorf("EXPRESSION MISMATCH [%s] on the INDEXED schema (seed=%d)\n  A: %s\n  B: %s\n"+
				"  A gives %v\n  B gives %v\n  %s",
				rule, seed, qa, qb, mmHeadRows(ia), mmHeadRows(ib), mmFirstDiff(ia, ib))
		}
		if !mmEqRows(na, nb) {
			t.Errorf("EXPRESSION MISMATCH [%s] on the UNINDEXED schema (seed=%d)\n  A: %s\n  B: %s\n"+
				"  A gives %v\n  B gives %v\n"+
				"With no index in play this is an expression-evaluation defect.",
				rule, seed, qa, qb, mmHeadRows(na), mmHeadRows(nb))
		}
	}

	for i := 0; i < iters; i++ {
		p := g.pred(1)
		lit := mhIntLits[r.Intn(len(mhIntLits))]

		// NULL-substituting sugar against its CASE longhand.
		equiv("coalesce-vs-case",
			"SELECT id, COALESCE(a, -777) FROM t ORDER BY id",
			"SELECT id, CASE WHEN a IS NULL THEN -777 ELSE a END FROM t ORDER BY id")
		// NULLIF is not supported (0AF00) and `-(-a)` does not parse (42601), so
		// neither can be swept; both are pinned as rejections below, where a
		// change in either shows up as a test telling you to move the rule back
		// into this loop.

		// Arithmetic identities. NULL propagates through both sides alike.
		equiv("plus-zero", "SELECT id, a FROM t ORDER BY id", "SELECT id, a + 0 FROM t ORDER BY id")
		equiv("times-one", "SELECT id, a FROM t ORDER BY id", "SELECT id, a * 1 FROM t ORDER BY id")
		// Double negation, spelled through subtraction because `-(-a)` is a parse
		// error here. Same identity, syntax the dialect accepts.
		equiv("double-negate", "SELECT id, a FROM t ORDER BY id",
			"SELECT id, 0 - (0 - a) FROM t ORDER BY id")
		equiv("minus-zero", "SELECT id, a FROM t ORDER BY id", "SELECT id, a - 0 FROM t ORDER BY id")

		// Comparison spellings.
		equiv("not-equals-vs-ne",
			fmt.Sprintf("SELECT id FROM t WHERE NOT (a = %s) ORDER BY id", lit),
			fmt.Sprintf("SELECT id FROM t WHERE a <> %s ORDER BY id", lit))
		equiv("flipped-comparison",
			fmt.Sprintf("SELECT id FROM t WHERE a < %s ORDER BY id", lit),
			fmt.Sprintf("SELECT id FROM t WHERE %s > a ORDER BY id", lit))
		equiv("is-not-null-vs-not-is-null",
			"SELECT id FROM t WHERE a IS NOT NULL ORDER BY id",
			"SELECT id FROM t WHERE NOT (a IS NULL) ORDER BY id")
		equiv("in-one-vs-equals",
			fmt.Sprintf("SELECT id FROM t WHERE a IN (%s) ORDER BY id", lit),
			fmt.Sprintf("SELECT id FROM t WHERE a = %s ORDER BY id", lit))

		// Aggregate spellings. MIN over the non-NULL values is the first row of
		// the ascending order, which is a completely different execution path to
		// the same answer.
		equiv("min-vs-order-limit",
			"SELECT MIN(a) FROM t",
			"SELECT a FROM t WHERE a IS NOT NULL ORDER BY a LIMIT 1")
		equiv("max-vs-order-limit",
			"SELECT MAX(a) FROM t",
			"SELECT a FROM t WHERE a IS NOT NULL ORDER BY a DESC LIMIT 1")
		equiv("count-col-vs-count-star-filtered",
			"SELECT COUNT(a) FROM t",
			"SELECT COUNT(*) FROM t WHERE a IS NOT NULL")
		equiv("sum-ignores-nulls",
			"SELECT SUM(a) FROM t",
			"SELECT SUM(a) FROM t WHERE a IS NOT NULL")
		equiv("grouped-min-vs-ordered-first",
			"SELECT MIN(a) FROM t WHERE b = 1",
			"SELECT a FROM t WHERE b = 1 AND a IS NOT NULL ORDER BY a LIMIT 1")

		// The predicate under test, expressed as a projected CASE and as a
		// filter — the pairing that caught the parenthesized-condition defect,
		// kept here over randomly generated predicates.
		equiv("case-projection-vs-filter",
			fmt.Sprintf("SELECT COUNT(*) FROM (SELECT id FROM t WHERE %s) AS x", p),
			fmt.Sprintf("SELECT SUM(CASE WHEN (%s) THEN 1 ELSE 0 END) FROM t", p))
	}

	// The two shapes that cannot be swept, pinned as rejections. Each names the
	// rule it would restore, so the day support lands the failure says what to
	// do rather than merely that something changed.
	if _, err := mmRows(t, ctx, w.plain, "SELECT id, NULLIF(a, 1) FROM t ORDER BY id"); err == nil {
		t.Errorf("NULLIF is now supported. Restore the nullif-vs-case rule to the sweep above: " +
			"NULLIF(a, v) must equal CASE WHEN a = v THEN NULL ELSE a END for every row.")
	}
	if _, err := mmRows(t, ctx, w.plain, "SELECT id, -(-a) FROM t ORDER BY id"); err == nil {
		t.Errorf("`-(-a)` now parses. Restore it as the double-negate rule's spelling — it is the " +
			"direct form of the identity that is currently expressed as 0 - (0 - a).")
	}

	rules := []string{
		"coalesce-vs-case",
		"plus-zero", "times-one", "double-negate", "minus-zero",
		"not-equals-vs-ne", "flipped-comparison", "is-not-null-vs-not-is-null", "in-one-vs-equals",
		"min-vs-order-limit", "max-vs-order-limit", "count-col-vs-count-star-filtered",
		"sum-ignores-nulls", "grouped-min-vs-ordered-first", "case-projection-vs-filter",
	}
	total := 0
	for _, rule := range rules {
		t.Logf("  %-34s compared=%d both-error=%d", rule, okByRule[rule], errByRule[rule])
		total += okByRule[rule]
		if okByRule[rule] == 0 {
			t.Errorf("instrument dead for %s: not one pair was comparable", rule)
		}
	}
	t.Logf("seed=%d iters=%d total-pairs=%d", seed, iters, total)
}
