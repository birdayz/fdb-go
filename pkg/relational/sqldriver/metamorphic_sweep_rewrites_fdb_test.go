package sqldriver_test

// Equivalence sweep: pairs of query spellings that MUST agree.
//
// The twin oracle compares two access paths and is blind to a defect both share.
// The NoREC oracle compares an optimized predicate against a per-row one and
// found a translator defect the twin could not see. This sweep generalizes that
// idea to a family of rewrites, each of which the engine is free to plan
// differently but not to answer differently:
//
//	IN vs a disjunction of equalities        BETWEEN vs two comparisons
//	De Morgan                                double negation
//	commutativity of AND / OR                idempotence (p AND p, p OR p)
//	DISTINCT vs GROUP BY                     HAVING vs filtering a derived table
//	a nested filter vs a conjunction         COUNT(*) vs COUNT(1)
//	parenthesized vs bare conditions         a condition in CASE vs in WHERE
//
// Each rule is a rewrite a planner might plausibly perform internally, so a
// disagreement is either the rewrite being applied wrongly or the two spellings
// taking different code paths that disagree. The parenthesization rules are
// there because that is exactly where the searched-CASE defect lived.
//
// Every pair is run on the INDEXED schema (where rewriting has the most freedom)
// and the unindexed one (which isolates a translator defect from an access-path
// one), and the counts are compared rather than the row sets where the rewrite
// does not fix an order.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

func TestFDB_MetamorphicRewriteEquivalenceSweep(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_rewrites", "rw",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) "+
			"CREATE INDEX t_c ON t (c) CREATE INDEX t_s ON t (s) ")

	dataRand := rand.New(rand.NewSource(5150))
	const nRows = 160
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, mhRowLiteral(dataRand, i, false))
	}
	for start := 0; start < len(vals); start += 20 {
		end := start + 20
		if end > len(vals) {
			end = len(vals)
		}
		w.Exec("INSERT INTO t " + mhCols + " VALUES " + strings.Join(vals[start:end], ", "))
	}

	seed := int64(1)
	if s := os.Getenv("RW_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 60
	if s := os.Getenv("RW_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	r := rand.New(rand.NewSource(seed))
	g := &mhGen{r: r}

	okByRule := map[string]int{}
	errByRule := map[string]int{}
	// equiv runs two spellings on BOTH schemas and requires all four answers to
	// agree. Comparing the two schemas as well as the two spellings costs
	// nothing and localizes a failure: same-schema disagreement is a rewrite or
	// translator defect, cross-schema disagreement is an access-path one.
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
			t.Errorf("REWRITE MISMATCH [%s] on the INDEXED schema (seed=%d)\n  A: %s\n  B: %s\n"+
				"  A gives %v\n  B gives %v\n  %s", rule, seed, qa, qb, mmHeadRows(ia), mmHeadRows(ib),
				mmFirstDiff(ia, ib))
		}
		if !mmEqRows(na, nb) {
			t.Errorf("REWRITE MISMATCH [%s] on the UNINDEXED schema (seed=%d)\n  A: %s\n  B: %s\n"+
				"  A gives %v\n  B gives %v\n"+
				"With no index in play this is a translator or evaluation defect rather than an "+
				"access-path one.", rule, seed, qa, qb, mmHeadRows(na), mmHeadRows(nb))
		}
		if !mmEqRows(ia, na) {
			t.Errorf("TWIN MISMATCH [%s] (seed=%d)\n  q: %s\n  indexed %v\n  unindexed %v",
				rule, seed, qa, mmHeadRows(ia), mmHeadRows(na))
		}
	}

	count := func(where string) string {
		return fmt.Sprintf("SELECT COUNT(*) FROM t WHERE %s", where)
	}
	ids := func(where string) string {
		return fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", where)
	}

	for i := 0; i < iters; i++ {
		p := g.pred(2)
		q := g.pred(1)
		lit1, lit2 := mhIntLits[r.Intn(len(mhIntLits))], mhIntLits[r.Intn(len(mhIntLits))]

		// Parenthesization must not change meaning — the family the searched-CASE
		// defect belonged to.
		equiv("paren-where", ids(p), ids("("+p+")"))
		equiv("paren-double", ids(p), ids("(("+p+"))"))
		equiv("paren-case",
			fmt.Sprintf("SELECT SUM(CASE WHEN %s THEN 1 ELSE 0 END) FROM t", p),
			fmt.Sprintf("SELECT SUM(CASE WHEN (%s) THEN 1 ELSE 0 END) FROM t", p))
		equiv("case-vs-where",
			fmt.Sprintf("SELECT SUM(CASE WHEN (%s) THEN 1 ELSE 0 END) FROM t", p),
			count(p))

		// Boolean algebra.
		equiv("de-morgan",
			ids(fmt.Sprintf("NOT ((%s) OR (%s))", p, q)),
			ids(fmt.Sprintf("(NOT (%s)) AND (NOT (%s))", p, q)))
		equiv("de-morgan-and",
			ids(fmt.Sprintf("NOT ((%s) AND (%s))", p, q)),
			ids(fmt.Sprintf("(NOT (%s)) OR (NOT (%s))", p, q)))
		equiv("double-negation", ids(p), ids(fmt.Sprintf("NOT (NOT (%s))", p)))
		equiv("and-commutes",
			ids(fmt.Sprintf("(%s) AND (%s)", p, q)),
			ids(fmt.Sprintf("(%s) AND (%s)", q, p)))
		equiv("or-commutes",
			ids(fmt.Sprintf("(%s) OR (%s)", p, q)),
			ids(fmt.Sprintf("(%s) OR (%s)", q, p)))
		equiv("and-idempotent", ids(p), ids(fmt.Sprintf("(%s) AND (%s)", p, p)))
		equiv("or-idempotent", ids(p), ids(fmt.Sprintf("(%s) OR (%s)", p, p)))

		// Sugar that has a longhand.
		equiv("in-vs-or",
			ids(fmt.Sprintf("a IN (%s, %s)", lit1, lit2)),
			ids(fmt.Sprintf("a = %s OR a = %s", lit1, lit2)))
		equiv("between-vs-comparisons",
			ids(fmt.Sprintf("b BETWEEN %s AND %s", lit1, lit2)),
			ids(fmt.Sprintf("b >= %s AND b <= %s", lit1, lit2)))
		equiv("count-star-vs-one", count(p), fmt.Sprintf("SELECT COUNT(1) FROM t WHERE %s", p))

		// Shape rewrites the planner may perform itself.
		equiv("nested-filter-vs-conjunction",
			fmt.Sprintf("SELECT id FROM (SELECT id, a, b FROM t WHERE %s) AS x WHERE x.a > 0 ORDER BY id", p),
			ids(fmt.Sprintf("(%s) AND a > 0", p)))
		equiv("distinct-vs-group-by",
			"SELECT DISTINCT a FROM t ORDER BY a",
			"SELECT a FROM t GROUP BY a ORDER BY a")
		equiv("having-vs-derived-filter",
			"SELECT a, COUNT(*) FROM t GROUP BY a HAVING COUNT(*) > 1 ORDER BY a",
			"SELECT * FROM (SELECT a, COUNT(*) AS n FROM t GROUP BY a) AS x WHERE x.n > 1 ORDER BY x.a")
	}

	rules := []string{
		"paren-where", "paren-double", "paren-case", "case-vs-where",
		"de-morgan", "de-morgan-and", "double-negation",
		"and-commutes", "or-commutes", "and-idempotent", "or-idempotent",
		"in-vs-or", "between-vs-comparisons", "count-star-vs-one",
		"nested-filter-vs-conjunction", "distinct-vs-group-by", "having-vs-derived-filter",
	}
	total := 0
	for _, rule := range rules {
		t.Logf("  %-30s compared=%d both-error=%d", rule, okByRule[rule], errByRule[rule])
		total += okByRule[rule]
		if okByRule[rule] == 0 {
			t.Errorf("instrument dead for %s: not one pair was comparable, so its green says nothing",
				rule)
		}
	}
	t.Logf("seed=%d iters=%d total-pairs=%d", seed, iters, total)
}

// mmHeadRows truncates a row list for a failure message.
func mmHeadRows(rows []string) []string {
	if len(rows) <= 20 {
		return rows
	}
	return append(append([]string{}, rows[:20]...), fmt.Sprintf("...(+%d more)", len(rows)-20))
}
