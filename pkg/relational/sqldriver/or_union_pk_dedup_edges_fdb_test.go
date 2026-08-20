package sqldriver_test

// Edge cases of the OR-to-union cross-leg dedup, beyond the two- and three-way
// disjunctions the main suite pins.
//
// WHICH OF THESE ACTUALLY REACH THE UNION, MEASURED on this fixture rather than
// assumed — the distinction decides what each case is worth:
//
//	REACHES IT   a disjunction of EQUALITIES on indexed columns, correlated or
//	             constant, as the inner of a LEFT JOIN. Including two points on
//	             the SAME index, which plans as a union of that index with
//	             itself.
//	DOES NOT     a conjunction of disjunctions — (A OR B) AND (C OR D) — which
//	             plans as NestedLoopJoin(LEFT OUTER, Scan, Scan) even with 1200
//	             padding rows making the scan expensive; a disjunction of
//	             RANGES; a disjunct on the primary key; a disjunction reached
//	             through NOT; and every single-table OR, which plans as
//	             PredicatesFilter(Scan) whatever the indexes.
//
// The cases that do not reach it are kept deliberately, and they are the reason
// this file states the above instead of asserting a plan everywhere. The number
// of legs a record can arrive on is a product, not a sum, so if the cost model
// ever starts choosing the union for a conjunction of disjunctions, a record
// matching one probe from each pair arrives on several legs at once — and the
// row assertions here are already in place to catch a dedup that does not
// collapse them. Pinning the ROWS holds under whichever plan is chosen; pinning
// a plan these shapes do not currently get would only pin today's cost model.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mmOrEdgeFixture is a driver row plus a probed table with four indexed
// columns, so a conjunction of two disjunctions has four legs to enumerate.
func mmOrEdgeFixture(t *testing.T, ctx context.Context, dbPath, prefix string) *mmTwin {
	t.Helper()
	w := mmNewTwin(t, ctx, dbPath, prefix,
		"CREATE TABLE d (did BIGINT, da BIGINT, db2 BIGINT, dc BIGINT, dd BIGINT, PRIMARY KEY (did)) "+
			"CREATE TABLE u (uid BIGINT, ua BIGINT, ub BIGINT, uc BIGINT, ud BIGINT, PRIMARY KEY (uid)) ",
		"CREATE INDEX u_ua ON u (ua) "+
			"CREATE INDEX u_ub ON u (ub) "+
			"CREATE INDEX u_uc ON u (uc) "+
			"CREATE INDEX u_ud ON u (ud) ")
	w.Exec("INSERT INTO d (did, da, db2, dc, dd) VALUES (1, 100, 200, 300, 400)")
	// Each row is labelled by which of the four probes it satisfies. The rows
	// that matter are the ones satisfying at least one probe from BOTH pairs,
	// because those are the ones a cross-product enumeration produces more than
	// once: row 15 satisfies all four.
	w.Exec("INSERT INTO u (uid, ua, ub, uc, ud) VALUES " +
		"(10, 100,   1,   1,   1), " + // a only
		"(11,   1, 200,   1,   1), " + // b only
		"(12,   1,   1, 300,   1), " + // c only
		"(13, 100,   1, 300,   1), " + // a and c
		"(14, 100, 200, 300,   1), " + // a, b, c
		"(15, 100, 200, 300, 400), " + // all four
		"(16,   1,   1,   1, 400), " + // d only
		"(17,   1, 200,   1, 400), " + // b and d
		"(18,   1,   1,   1,   1)") // none
	return w
}

// TestFDB_OrUnionPrimaryKeyDedup_ConjunctionOfDisjunctions is the product case:
// (A OR B) AND (C OR D) has four DNF terms, and a record satisfying one probe
// from each pair belongs to several of them.
//
// This shape does NOT use the union today (see the file header) — it plans as a
// nested-loop join over full scans, where a residual predicate evaluates the
// whole condition per row and no dedup is involved. The rows are pinned anyway,
// because they are what must not change if that ever flips: a cost-model change
// that starts choosing the union here brings the multi-leg dedup with it, and
// this is the test that would notice it collapsing the wrong number of copies.
func TestFDB_OrUnionPrimaryKeyDedup_ConjunctionOfDisjunctions(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmOrEdgeFixture(t, ctx, "/testdb_oredge_prod", "oredgep")

	// (a OR b) AND (c OR d): 13 (a,c), 14 (a,b,c), 15 (all), 17 (b,d).
	q2x2 := "SELECT d.did, u.uid FROM d LEFT JOIN u ON " +
		"(u.ua = d.da OR u.ub = d.db2) AND (u.uc = d.dc OR u.ud = d.dd) " +
		"ORDER BY d.did, u.uid"
	w.Want("two-by-two product", q2x2, []string{"1|13", "1|14", "1|15", "1|17"})
	w.Want("two-by-two product, counted",
		"SELECT COUNT(*) FROM d LEFT JOIN u ON "+
			"(u.ua = d.da OR u.ub = d.db2) AND (u.uc = d.dc OR u.ud = d.dd)",
		[]string{"4"})

	// Three disjunctions multiply to eight terms.
	w.Want("three-way product",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON "+
			"(u.ua = d.da OR u.ub = d.db2) AND (u.uc = d.dc OR u.ud = d.dd) "+
			"AND (u.ua = d.da OR u.ud = d.dd) ORDER BY d.did, u.uid",
		[]string{"1|13", "1|14", "1|15", "1|17"})

	// A disjunction ANDed with a plain conjunct: the fixed predicate is repeated
	// into every DNF term, so the term count is unchanged and each is narrower.
	w.Want("product with a fixed conjunct",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON "+
			"(u.ua = d.da OR u.ub = d.db2) AND (u.uc = d.dc OR u.ud = d.dd) AND u.uid > 13 "+
			"ORDER BY d.did, u.uid",
		[]string{"1|14", "1|15", "1|17"})
}

// TestFDB_OrUnionPrimaryKeyDedup_ConjunctLimit takes DefaultMaxNumConjuncts (9)
// from both sides. Under the limit the rewrite is allowed to fire and over it
// the rule declines outright, and both must answer the same rows — the limit is
// a planning budget, never a licence to return something different.
//
// Nothing else in the tree exercises this boundary: DefaultMaxNumConjuncts
// appears in no test file. What it guards is a COMBINATORIAL one — n
// disjunctions enumerate 2^n terms — so the failure it prevents is a planner
// that becomes unusably slow rather than one that answers wrongly, and the
// assertion that matters either side of it is that the answer is stable.
func TestFDB_OrUnionPrimaryKeyDedup_ConjunctLimit(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmOrEdgeFixture(t, ctx, "/testdb_oredge_limit", "oredgel")

	// n disjunctions ANDed together, each satisfied by every row, so the answer
	// is the whole table and a dedup failure shows as a duplicated uid rather
	// than a missing one.
	conjuncts := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "(u.ua = d.da OR u.uid >= 0)"
		}
		return strings.Join(parts, " AND ")
	}
	all := []string{"1|10", "1|11", "1|12", "1|13", "1|14", "1|15", "1|16", "1|17", "1|18"}

	for _, n := range []int{1, 2, 3, 8, 9, 10, 12} {
		q := fmt.Sprintf("SELECT d.did, u.uid FROM d LEFT JOIN u ON %s ORDER BY d.did, u.uid",
			conjuncts(n))
		w.Want(fmt.Sprintf("%d conjunct(s)", n), q, all)
	}
}

// TestFDB_OrUnionPrimaryKeyDedup_LegShapes covers disjunctions whose legs are
// NOT two different secondary indexes. These are the shapes where a full-row
// dedup would have happened to work — identical partial rows, or no overlap at
// all — which makes them the ones a fix aimed at the broken case is most likely
// to leave untested.
func TestFDB_OrUnionPrimaryKeyDedup_LegShapes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmOrEdgeFixture(t, ctx, "/testdb_oredge_shapes", "oredges")

	// Two points on the SAME index. This one DOES reach the union, and it plans
	// as a union of one index with itself — so both legs emit rows of identical
	// shape, and the dedup is keyed on a primary key that both carry in the same
	// position. No record can satisfy both points here, so what this pins is
	// that the dedup does not collapse rows it should keep.
	sameIdx := "SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ua = 1 ORDER BY d.did, u.uid"
	w.WantPlanContains("same-index disjuncts reach the union", sameIdx, "UnorderedUnion")
	w.WantPlanContains("and dedup by primary key", sameIdx, "UnorderedPrimaryKeyDistinct")
	w.Want("same index, disjoint points", sameIdx,
		[]string{"1|10", "1|11", "1|12", "1|13", "1|14", "1|15", "1|16", "1|17", "1|18"})

	// The remaining shapes plan as a nested-loop join with a residual predicate
	// (measured; see the file header). Their rows are pinned because the ANSWER
	// is what must hold whichever access path is chosen.
	w.Want("same index, overlapping ranges",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua >= 1 OR u.ua <= 100 ORDER BY d.did, u.uid",
		[]string{"1|10", "1|11", "1|12", "1|13", "1|14", "1|15", "1|16", "1|17", "1|18"})
	w.Want("primary-key leg beside an index leg",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.uid = 15 OR u.ua = d.da ORDER BY d.did, u.uid",
		[]string{"1|10", "1|13", "1|14", "1|15"})
	// De Morgan turns a conjunction of inequalities into a disjunction of
	// equalities during normalization, so the union rule can see an OR the SQL
	// text never spelled.
	w.Want("disjunction reached through NOT",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON NOT (u.ua <> d.da AND u.ub <> d.db2) "+
			"ORDER BY d.did, u.uid",
		[]string{"1|10", "1|11", "1|13", "1|14", "1|15", "1|17"})
	w.Want("empty union still null-extends once",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = 77777 OR u.ub = 88888 "+
			"ORDER BY d.did, u.uid",
		[]string{"1|NULL"})

	// Aggregates directly over a disjunctive join, where a surviving duplicate
	// is invisible in the output but changes the number.
	w.Want("COUNT over a union with overlap",
		"SELECT COUNT(*) FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2",
		[]string{"6"})
	w.Want("MIN and MAX over a union with overlap",
		"SELECT MIN(u.uid), MAX(u.uid) FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2",
		[]string{"10|17"})
	w.Want("GROUP BY over a union with overlap",
		"SELECT u.ua, COUNT(*) FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 "+
			"GROUP BY u.ua ORDER BY u.ua",
		[]string{"1|2", "100|4"})

	// LIMIT over a union whose rows include a collapsed duplicate: if the dedup
	// ran after the limit, the page would be short by the number collapsed.
	w.Want("LIMIT 1 over a union",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 "+
			"ORDER BY d.did, u.uid LIMIT 1",
		[]string{"1|10"})
	w.Want("LIMIT 2 OFFSET 1 over a union",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 "+
			"ORDER BY d.did, u.uid LIMIT 2 OFFSET 1",
		[]string{"1|11", "1|13"})
}
