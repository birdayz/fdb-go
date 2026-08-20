package sqldriver_test

// Tenth axis: REDUNDANT PARENTHESES.
//
// Wrapping an operand in parentheses groups it. In an argument position it can
// never change the answer, so for every generated predicate the bare spelling
// and its parenthesized twins must return exactly the same rows.
//
// This axis exists because two separate defects lived in it and neither was
// reachable from any other oracle. `( expr )` parses as a one-field RECORD in
// this grammar — the same production that builds `(x, y)` — so a parenthesis is
// not the no-op it looks like; it builds a value, and something downstream has
// to flatten it back. Where nothing did:
//
//   - a searched CASE walked its condition as a VALUE first, so a parenthesized
//     COMPOUND boolean became {_0: predicate}, compared unequal to TRUE, and
//     every row silently took the ELSE branch;
//   - CASE arms and IN-list items were never flattened, so `THEN (5)` failed to
//     plan and `THEN (5) ELSE (6)` returned a STRUCT column.
//
// Both were found by hand, one shape at a time. This sweep finds the whole
// class, because it does not need anyone to guess WHICH position is unflattened
// — it parenthesizes every position the generator can reach and compares.
//
// PROJECTION POSITION IS DELIBERATELY EXCLUDED, and that exclusion is the one
// thing here that must not be widened. `SELECT (val)` really IS a one-field
// struct in both engines — Java's visitRecordConstructor goes straight to
// RecordConstructorValue whatever the element count, and the ambiguity is
// resolved by POSITION rather than by parse. So a sweep that wrapped SELECT
// items would report a divergence where the engines agree, and the natural
// "fix" for it would be to unwrap in the constructor, which is precisely the
// divergence this codebase used to have. Every shape below puts its
// parentheses inside an ARGUMENT: a WHERE predicate, a CASE arm, an IN item.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
)

// mmpWrap returns s wrapped in depth layers of parentheses.
func mmpWrap(s string, depth int) string {
	for i := 0; i < depth; i++ {
		s = "(" + s + ")"
	}
	return s
}

func TestFDB_MetamorphicParenthesization(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_mhparen")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_mhparen")
	table := "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, c DOUBLE, s STRING, f BOOLEAN, PRIMARY KEY (id)) "
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE mhparen_idx "+table+
		"CREATE INDEX t_a ON t (a) CREATE INDEX t_ab ON t (a, b) CREATE INDEX t_s ON t (s)")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_mhparen/si WITH TEMPLATE mhparen_idx")

	dsn := fmt.Sprintf("fdbsql:///testdb_mhparen?cluster_file=%s&schema=si", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dataRand := rand.New(rand.NewSource(20260820))
	const nRows = 120
	var vals []string
	for i := 1; i <= nRows; i++ {
		vals = append(vals, mhRowLiteral(dataRand, i, false))
	}
	for start := 0; start < len(vals); start += 20 {
		end := start + 20
		if end > len(vals) {
			end = len(vals)
		}
		mwjoMustExec(t, db, ctx, "INSERT INTO t "+mhCols+" VALUES "+strings.Join(vals[start:end], ", "))
	}

	seed := int64(7)
	if s := os.Getenv("MHP_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	iters := 120
	if s := os.Getenv("MHP_ITERS"); s != "" {
		fmt.Sscan(s, &iters)
	}
	g := &mhGen{r: rand.New(rand.NewSource(seed))}

	rowsFor := func(q string) ([]string, error) { return mmRows(t, ctx, db, q) }

	// The population guards. Three distinct ways this sweep could be green
	// while proving nothing, so three counters:
	//
	//	compared  pairs actually compared. Zero if every bare query errored.
	//	nonEmpty  pairs whose expected row set was NOT empty. A predicate corpus
	//	          that happened to select nothing would compare empty against
	//	          empty forever and pass through any defect.
	//	skipped   bare queries the engine declined, kept only so the report can
	//	          say WHY compared is what it is.
	var compared, nonEmpty, skipped int

	// ---- axis 1: a parenthesized WHERE predicate ---------------------------
	//
	// The whole predicate, then each conjunct: `p` vs `(p)` vs `((p))`, and
	// `p AND q` vs `(p) AND (q)`.
	t.Run("where_predicate", func(t *testing.T) {
		for i := 0; i < iters; i++ {
			p := g.pred(2)
			bare := fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", p)
			want, err := rowsFor(bare)
			if err != nil {
				// An unsupported shape is not a finding here: what matters is
				// that the parenthesized twin behaves the SAME way, which the
				// error comparison below still checks.
				for _, d := range []int{1, 2} {
					variant := fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", mmpWrap(p, d))
					if _, verr := rowsFor(variant); (verr == nil) != (err == nil) {
						t.Errorf("parenthesizing changed whether the query is ACCEPTED\n"+
							"  bare : %s\n  err  : %v\n  paren: %s\n  err  : %v", bare, err, variant, verr)
					}
				}
				skipped++
				continue
			}
			for _, d := range []int{1, 2} {
				variant := fmt.Sprintf("SELECT id FROM t WHERE %s ORDER BY id", mmpWrap(p, d))
				got, verr := rowsFor(variant)
				if verr != nil {
					t.Errorf("the bare predicate ran but its parenthesized twin failed\n"+
						"  bare : %s\n  paren: %s\n  err  : %v", bare, variant, verr)
					continue
				}
				compared++
				if len(want) > 0 {
					nonEmpty++
				}
				if !mmEqRows(got, want) {
					t.Errorf("REDUNDANT PARENTHESES CHANGED THE ANSWER\n  bare : %s\n  paren: %s\n"+
						"  bare rows  %v\n  paren rows %v\n  %s",
						bare, variant, want, got, mmFirstDiff(got, want))
				}
			}
		}
	})

	// ---- axis 2: parenthesized CASE arms -----------------------------------
	//
	// The position where an unflattened record planned cleanly and returned a
	// column of the wrong TYPE, which is why the comparison here is over the
	// CASE's own output rather than over a filtered row set.
	t.Run("case_arms", func(t *testing.T) {
		lits := []string{"1", "0", "-3", "'z'", "NULL"}
		for i := 0; i < iters; i++ {
			p := g.pred(1)
			thenLit := g.pick(lits)
			elseLit := g.pick(lits)
			// Both arms the same literal KIND, so the bare form's own result
			// type is not what the variants are being compared against.
			if (thenLit == "'z'") != (elseLit == "'z'") {
				elseLit = thenLit
			}
			bare := fmt.Sprintf("SELECT CASE WHEN %s THEN %s ELSE %s END FROM t ORDER BY id",
				p, thenLit, elseLit)
			want, err := rowsFor(bare)
			if err != nil {
				skipped++
				continue
			}
			variants := []string{
				fmt.Sprintf("SELECT CASE WHEN %s THEN %s ELSE %s END FROM t ORDER BY id",
					p, mmpWrap(thenLit, 1), elseLit),
				fmt.Sprintf("SELECT CASE WHEN %s THEN %s ELSE %s END FROM t ORDER BY id",
					p, thenLit, mmpWrap(elseLit, 1)),
				fmt.Sprintf("SELECT CASE WHEN %s THEN %s ELSE %s END FROM t ORDER BY id",
					p, mmpWrap(thenLit, 1), mmpWrap(elseLit, 1)),
				fmt.Sprintf("SELECT CASE WHEN %s THEN %s ELSE %s END FROM t ORDER BY id",
					p, mmpWrap(thenLit, 2), mmpWrap(elseLit, 2)),
				// The CONDITION, which is the other defect this axis covers.
				fmt.Sprintf("SELECT CASE WHEN %s THEN %s ELSE %s END FROM t ORDER BY id",
					mmpWrap(p, 1), thenLit, elseLit),
			}
			for _, variant := range variants {
				got, verr := rowsFor(variant)
				if verr != nil {
					t.Errorf("the bare CASE ran but its parenthesized twin failed\n"+
						"  bare : %s\n  paren: %s\n  err  : %v", bare, variant, verr)
					continue
				}
				compared++
				if len(want) > 0 {
					nonEmpty++
				}
				if !mmEqRows(got, want) {
					t.Errorf("REDUNDANT PARENTHESES CHANGED A CASE RESULT\n  bare : %s\n  paren: %s\n"+
						"  bare rows  %v\n  paren rows %v\n  %s",
						bare, variant, want, got, mmFirstDiff(got, want))
				}
			}
		}
	})

	// ---- axis 2b: a parenthesized COMPARISON as a CASE arm -----------------
	//
	// A comparison is a legal consequent — `CASE WHEN id = 1 THEN a > 3 ELSE
	// FALSE END` is a boolean-valued column — and parenthesizing it puts a
	// predicate INSIDE a one-field record, which is the one shape where the
	// flatten and the predicate-first condition walk meet. The literal arms
	// above cannot reach it: their operands are never predicates.
	t.Run("case_arm_is_a_comparison", func(t *testing.T) {
		for i := 0; i < iters/2; i++ {
			thenPred := g.atom(1)
			bare := fmt.Sprintf("SELECT CASE WHEN id > 0 THEN %s ELSE FALSE END FROM t ORDER BY id",
				thenPred)
			want, err := rowsFor(bare)
			if err != nil {
				skipped++
				continue
			}
			for _, d := range []int{1, 2} {
				variant := fmt.Sprintf("SELECT CASE WHEN id > 0 THEN %s ELSE FALSE END FROM t ORDER BY id",
					mmpWrap(thenPred, d))
				got, verr := rowsFor(variant)
				if verr != nil {
					t.Errorf("a bare comparison CASE arm ran but its parenthesized twin failed\n"+
						"  bare : %s\n  paren: %s\n  err  : %v", bare, variant, verr)
					continue
				}
				compared++
				if len(want) > 0 {
					nonEmpty++
				}
				if !mmEqRows(got, want) {
					t.Errorf("REDUNDANT PARENTHESES CHANGED A COMPARISON-VALUED CASE ARM\n"+
						"  bare : %s\n  paren: %s\n  bare rows  %v\n  paren rows %v\n  %s",
						bare, variant, want, got, mmFirstDiff(got, want))
				}
			}
		}
	})

	// ---- axis 3: parenthesized IN-list items -------------------------------
	t.Run("in_list_items", func(t *testing.T) {
		for i := 0; i < iters; i++ {
			col := g.pick(g.ints())
			items := []string{g.pick(mhIntLits), g.pick(mhIntLits), g.pick(mhIntLits)}
			bare := fmt.Sprintf("SELECT id FROM t WHERE %s IN (%s) ORDER BY id",
				col, strings.Join(items, ", "))
			want, err := rowsFor(bare)
			if err != nil {
				skipped++
				continue
			}
			wrapped := make([]string, len(items))
			for j, it := range items {
				wrapped[j] = mmpWrap(it, 1)
			}
			variants := []string{
				fmt.Sprintf("SELECT id FROM t WHERE %s IN (%s) ORDER BY id",
					col, strings.Join(wrapped, ", ")),
				// Only the first item, which is the shape that first failed.
				fmt.Sprintf("SELECT id FROM t WHERE %s IN (%s, %s, %s) ORDER BY id",
					col, mmpWrap(items[0], 1), items[1], items[2]),
				// The LEFT operand too, which already flattened before the
				// repair — so this variant proves the two flattens compose.
				fmt.Sprintf("SELECT id FROM t WHERE %s IN (%s) ORDER BY id",
					mmpWrap(col, 1), strings.Join(wrapped, ", ")),
			}
			for _, variant := range variants {
				got, verr := rowsFor(variant)
				if verr != nil {
					t.Errorf("the bare IN list ran but its parenthesized twin failed\n"+
						"  bare : %s\n  paren: %s\n  err  : %v", bare, variant, verr)
					continue
				}
				compared++
				if len(want) > 0 {
					nonEmpty++
				}
				if !mmEqRows(got, want) {
					t.Errorf("REDUNDANT PARENTHESES CHANGED AN IN-LIST RESULT\n  bare : %s\n  paren: %s\n"+
						"  bare rows  %v\n  paren rows %v\n  %s",
						bare, variant, want, got, mmFirstDiff(got, want))
				}
			}
		}
	})

	// The vacuity floor. `compared` counts pairs actually COMPARED, not queries
	// generated: a sweep whose every bare query errored would reach here with
	// compared == 0, having asserted nothing, and would otherwise be green.
	// The floor is deliberately far below the ~1500 pairs three axes at 120
	// iterations produce, so ordinary generator drift does not trip it while a
	// collapse does.
	if compared < 500 {
		t.Fatalf("the sweep compared only %d parenthesization pairs (%d bare queries were "+
			"unsupported and skipped). Below this floor the axis is not exercising the engine, "+
			"so its green says nothing", compared, skipped)
	}
	// The second floor is the one that is easy to forget and easy to trip
	// accidentally: every comparison above could have run against an EMPTY
	// expected row set — an empty result equals an empty result whatever the
	// parentheses did — and the sweep would be green having compared nothing
	// but absences. Measured at 120 iterations on this fixture the non-empty
	// count sits far above 300; the floor is set well below that so generator
	// drift does not trip it while a collapse does.
	if nonEmpty < 300 {
		t.Fatalf("only %d of %d compared pairs had a NON-EMPTY expected row set. An empty result "+
			"matches an empty result whatever the parentheses did, so below this floor the sweep "+
			"is comparing absences and its green says nothing", nonEmpty, compared)
	}
	t.Logf("parenthesization sweep: %d pairs compared (%d with non-empty rows), "+
		"%d unsupported bare queries skipped", compared, nonEmpty, skipped)
}
