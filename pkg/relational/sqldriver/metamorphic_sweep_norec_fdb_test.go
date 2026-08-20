package sqldriver_test

// A second oracle, complementary to the indexed/unindexed twin.
//
// The twin compares two ACCESS PATHS and its oracle is the unindexed side, so it
// is blind to a defect both sides share — a predicate that is evaluated wrongly
// in the same way whether it came from an index range or a residual filter. This
// sweep compares two SPELLINGS of the same question instead:
//
//	SELECT COUNT(*) FROM t WHERE p                            -- p may be optimized
//	SELECT SUM(CASE WHEN p THEN 1 ELSE 0 END) FROM t          -- p per row, as an expression
//
// The first lets the planner turn p into scan bounds, index selection and
// residual filters; the second denies it that, because p sits inside a
// projection over every row and the aggregate has nothing to push down. The two
// must count the same rows. Where they differ, the optimized form derived
// something from p that evaluating p does not support — a wrong range endpoint,
// a dropped conjunct, an inverted comparison.
//
// The NULL handling of the two spellings agrees by construction, which is what
// makes them comparable: `WHERE p` keeps only rows where p is TRUE, and CASE's
// ELSE catches FALSE and UNKNOWN alike, so both count exactly the TRUE rows.
//
// This is the NoREC oracle (non-optimizing reference engine construction). Both
// schemas are swept: the indexed one is where an optimization can go wrong, and
// the unindexed one is the control that the two spellings agree even with
// nothing to optimize.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

func TestFDB_MetamorphicNoRecPredicateSweep(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_norec", "norec",
		"CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) ",
		"CREATE INDEX t_a ON t (a) "+
			"CREATE INDEX t_ab ON t (a, b) "+
			"CREATE INDEX t_c ON t (c) "+
			"CREATE INDEX t_s ON t (s) "+
			"CREATE INDEX t_ba ON t (b, a) ")

	dataRand := rand.New(rand.NewSource(90210))
	const nRows = 200
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
	if s := os.Getenv("NOREC_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 150
	if s := os.Getenv("NOREC_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	g := &mhGen{r: rand.New(rand.NewSource(seed))}

	compared, bothErr := 0, 0
	nonTrivial := 0
	for i := 0; i < iters; i++ {
		p := g.pred(2)
		optimized := fmt.Sprintf("SELECT COUNT(*) FROM t WHERE %s", p)
		unoptimized := fmt.Sprintf("SELECT SUM(CASE WHEN %s THEN 1 ELSE 0 END) FROM t", p)

		// Indexed side: where an optimization exists to go wrong.
		gotOpt, e1 := mmRows(t, ctx, w.idx, optimized)
		gotUnopt, e2 := mmRows(t, ctx, w.idx, unoptimized)
		// Unindexed side: the control.
		gotOptN, e3 := mmRows(t, ctx, w.plain, optimized)
		gotUnoptN, e4 := mmRows(t, ctx, w.plain, unoptimized)

		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			bothErr++
			if bothErr <= 2 {
				t.Logf("both-error sample (%v / %v / %v / %v): %s", e1, e2, e3, e4, p)
			}
			continue
		}
		compared++

		// SUM over zero rows is NULL rather than 0, so a predicate matching
		// nothing renders as "NULL" on the unoptimized side and "0" on the
		// optimized one. Normalize that ONE case rather than skipping it: a
		// predicate that matches nothing is worth comparing, and it is where an
		// over-narrow range endpoint would show.
		norm := func(rows []string) string {
			if len(rows) != 1 {
				return fmt.Sprintf("<%d rows>", len(rows))
			}
			if rows[0] == "NULL" {
				return "0"
			}
			return rows[0]
		}
		optV, unoptV := norm(gotOpt), norm(gotUnopt)
		optNV, unoptNV := norm(gotOptN), norm(gotUnoptN)
		if optV != "0" {
			nonTrivial++
		}

		if optV != unoptV {
			t.Errorf("NOREC MISMATCH on the INDEXED schema (seed=%d i=%d)\n"+
				"  predicate: %s\n"+
				"  COUNT(*) WHERE p          = %s\n"+
				"  SUM(CASE WHEN p THEN 1..) = %s\n"+
				"  plan: %s\n"+
				"The optimized spelling derived something from this predicate that evaluating it "+
				"per row does not support.", seed, i, p, optV, unoptV, w.Explain(optimized))
		}
		if optNV != unoptNV {
			t.Errorf("NOREC MISMATCH on the UNINDEXED schema (seed=%d i=%d)\n"+
				"  predicate: %s\n  COUNT(*) WHERE p = %s\n  SUM(CASE ...) = %s\n"+
				"With no index to match, the two spellings differ only in WHERE the predicate is "+
				"evaluated, so this is a predicate-evaluation defect rather than an access-path one.",
				seed, i, p, optNV, unoptNV)
		}
		// And the two schemas must agree with each other, which is the twin
		// oracle riding along for free.
		if optV != optNV {
			t.Errorf("TWIN MISMATCH (seed=%d i=%d)\n  predicate: %s\n  indexed = %s, unindexed = %s",
				seed, i, p, optV, optNV)
		}
	}

	t.Logf("seed=%d iters=%d compared=%d both-error=%d predicates-matching-rows=%d",
		seed, iters, compared, bothErr, nonTrivial)
	if compared < iters/2 {
		t.Fatalf("instrument dead: only %d of %d predicates were comparable", compared, iters)
	}
	// A sweep whose every predicate matched nothing compares 0 with 0 and proves
	// nothing about ranges: the counts have to be non-trivial to be evidence.
	if nonTrivial < iters/10 {
		t.Errorf("only %d predicates matched any row, so almost every comparison was 0 == 0 and "+
			"this run is close to vacuous", nonTrivial)
	}
}
