package sqldriver_test

// Cross-leg dedup for the OR-to-union access path.
//
// A disjunctive predicate is planned as a UNION of one access path per DNF
// term. A record satisfying more than one term is produced by more than one
// leg, so the union needs a dedup that collapses those copies. The dedup key
// must be the PRIMARY KEY, because the legs are covering scans of DIFFERENT
// indexes and therefore emit DIFFERENT rows for the same record — `(ua, uid)`
// from one index and `(ub, ua, uid)` from another. A full-ROW dedup cannot
// collapse them and the record is returned twice.
//
// Java expresses this with LogicalDistinctExpression, whose implementation
// (ImplementDistinctRule) is RecordQueryUnorderedPrimaryKeyDistinctPlan — a
// primary-key dedup. Go's LogicalDistinctExpression means FULL-ROW dedup (it
// carries SELECT DISTINCT, which Java's Cascades has no node for); Go's
// primary-key dedup node is LogicalUniqueExpression. The two node sets share
// their names but not their meanings, so a port that matched the names got the
// semantics backwards.
//
// Every case here pins ROWS. A shape assertion cannot see this defect: the
// wrong plan and the right plan differ only in which dedup key a node uses.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mmOrUnionFixture builds the two-table fixture every case in this file shares.
//
//	d = drivers, one row per case, carrying the two correlated values
//	u = the probed table, whose rows are rigged by which disjuncts they satisfy
func mmOrUnionFixture(t *testing.T, ctx context.Context, dbPath, prefix string) *mmTwin {
	t.Helper()
	w := mmNewTwin(t, ctx, dbPath, prefix,
		"CREATE TABLE d (did BIGINT, da BIGINT, db2 BIGINT, dc BIGINT, PRIMARY KEY (did)) "+
			"CREATE TABLE u (uid BIGINT, ua BIGINT, ub BIGINT, uc BIGINT, pad STRING, PRIMARY KEY (uid)) ",
		"CREATE INDEX u_ua ON u (ua) "+
			"CREATE INDEX u_ub ON u (ub) "+
			"CREATE INDEX u_uc ON u (uc) ")
	w.Exec("INSERT INTO d (did, da, db2, dc) VALUES (1, 100, 200, 300)")
	// u rows, by which of (ua=100, ub=200, uc=300) they satisfy:
	//   10 : ua only          20 : ub only          30 : uc only
	//   40 : ua AND ub        50 : ua AND uc        60 : ub AND uc
	//   70 : all three        80 : none
	//   90 : ua only, NULL in the other probed columns
	w.Exec("INSERT INTO u (uid, ua, ub, uc, pad) VALUES " +
		"(10, 100,   1,   1, 'p'), " +
		"(20,   1, 200,   1, 'p'), " +
		"(30,   1,   1, 300, 'p'), " +
		"(40, 100, 200,   1, 'p'), " +
		"(50, 100,   1, 300, 'p'), " +
		"(60,   1, 200, 300, 'p'), " +
		"(70, 100, 200, 300, 'p'), " +
		"(80,   1,   1,   1, 'p'), " +
		"(90, 100, NULL, NULL, 'p')")
	return w
}

// TestFDB_OrUnionPrimaryKeyDedup_LeftJoin is the shape that reaches the union
// access path today: the correlated inner of a LEFT JOIN, where a per-outer-row
// full scan is expensive enough that the cost model prefers a union of index
// probes.
func TestFDB_OrUnionPrimaryKeyDedup_LeftJoin(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmOrUnionFixture(t, ctx, "/testdb_orunion_lj", "orunlj")

	twoWay := "SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 ORDER BY d.did, u.uid"
	// Every case in this file is worthless if the plan stopped using the union,
	// so the operator is pinned before the rows are.
	w.WantPlanContains("two-way OR reaches the union", twoWay, "UnorderedUnion")
	// …and the dedup above it is the PRIMARY-KEY one, sitting BELOW the fetch —
	// Java's shape. Pinned separately from the rows because the correct rows are
	// also produced by the slower Distinct-above-Fetch arrangement, so a row
	// assertion alone cannot tell the repaired plan from a merely-correct one.
	w.WantPlanContains("dedup is by primary key", twoWay, "UnorderedPrimaryKeyDistinct")
	if plan := w.Explain(twoWay); !strings.Contains(plan, "Fetch(UnorderedPrimaryKeyDistinct(") {
		t.Errorf("the primary-key dedup is not below the fetch, so duplicates are fetched before "+
			"being discarded — Java pushes it through (PushDistinctThroughFetchRule).\n  plan: %s", plan)
	}
	w.Want("two-way OR", twoWay,
		[]string{"1|10", "1|20", "1|40", "1|50", "1|60", "1|70", "1|90"})

	threeWay := "SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 OR u.uc = d.dc " +
		"ORDER BY d.did, u.uid"
	w.WantPlanContains("three-way OR reaches the union", threeWay, "UnorderedUnion")
	// uid 70 satisfies all three legs and must still appear exactly once.
	w.Want("three-way OR", threeWay,
		[]string{"1|10", "1|20", "1|30", "1|40", "1|50", "1|60", "1|70", "1|90"})

	// A COUNT over the join is the sharpest form of the defect: duplicates are
	// invisible in a row list a reader skims, and arithmetic on them is silently
	// wrong.
	w.Want("count over two-way OR",
		"SELECT COUNT(*) FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2",
		[]string{"7"})
	w.Want("count over three-way OR",
		"SELECT COUNT(*) FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 OR u.uc = d.dc",
		[]string{"8"})

	// SUM over the joined ids: a duplicated row moves the total, so this catches
	// the defect even where a dedup-by-row happens to look right.
	w.Want("sum over two-way OR",
		"SELECT SUM(u.uid) FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2",
		[]string{"340"})

	// An outer row matching NOTHING must be null-extended exactly once — the
	// dedup must not disturb the LEFT JOIN's no-match branch.
	w.Exec("INSERT INTO d (did, da, db2, dc) VALUES (2, 999, 999, 999)")
	w.Want("unmatched outer row null-extends once",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 WHERE d.did = 2 "+
			"ORDER BY d.did, u.uid",
		[]string{"2|NULL"})

	// A disjunct that matches NOTHING on one leg must not change the other leg's
	// rows: the union degenerates to one contributing leg.
	w.Want("one empty leg",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = 88888 WHERE d.did = 1 "+
			"ORDER BY d.did, u.uid",
		[]string{"1|10", "1|40", "1|50", "1|70", "1|90"})

	// Both disjuncts selecting the SAME rows is the maximal-overlap case: every
	// row is produced twice and every one must be collapsed.
	w.Want("fully overlapping legs",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ua = d.da WHERE d.did = 1 "+
			"ORDER BY d.did, u.uid",
		[]string{"1|10", "1|40", "1|50", "1|70", "1|90"})

	// NULL on a probed column must not match, and must not produce a phantom row.
	w.Want("NULL probe column does not match",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ub = d.db2 OR u.uc = d.dc WHERE d.did = 1 "+
			"ORDER BY d.did, u.uid",
		[]string{"1|20", "1|30", "1|40", "1|50", "1|60", "1|70"})

	// A conjunct alongside the disjunction: the fixed predicate is repeated into
	// every leg, so a dedup defect survives it.
	w.Want("OR with an AND conjunct",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON (u.ua = d.da OR u.ub = d.db2) AND u.pad = 'p' "+
			"WHERE d.did = 1 ORDER BY d.did, u.uid",
		[]string{"1|10", "1|20", "1|40", "1|50", "1|60", "1|70", "1|90"})

	// DISTINCT above the join must not be what makes the answer right: it would
	// mask the defect here, so the case exists to keep the two dedups separable.
	w.Want("DISTINCT above the join",
		"SELECT DISTINCT u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 "+
			"WHERE d.did = 1 ORDER BY u.uid",
		[]string{"10", "20", "40", "50", "60", "70", "90"})

	// LIMIT interacts with the dedup: a duplicate consumes a slot, so the page
	// is short by exactly the number of collapsed copies.
	w.Want("LIMIT over the union",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 WHERE d.did = 1 "+
			"ORDER BY d.did, u.uid LIMIT 3",
		[]string{"1|10", "1|20", "1|40"})
	w.Want("LIMIT with OFFSET over the union",
		"SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 WHERE d.did = 1 "+
			"ORDER BY d.did, u.uid LIMIT 3 OFFSET 3",
		[]string{"1|50", "1|60", "1|70"})
}

// TestFDB_OrUnionPrimaryKeyDedup_OtherShapes pins the same invariant for the
// join and subquery spellings that do NOT choose the union plan today. They are
// here because which plan the cost model picks is not part of the contract: a
// cost-model change that starts choosing the union must not start returning
// duplicates, and without these cases that regression would land silently.
func TestFDB_OrUnionPrimaryKeyDedup_OtherShapes(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmOrUnionFixture(t, ctx, "/testdb_orunion_other", "orunot")

	w.Want("inner join with OR ON",
		"SELECT d.did, u.uid FROM d JOIN u ON u.ua = d.da OR u.ub = d.db2 ORDER BY d.did, u.uid",
		[]string{"1|10", "1|20", "1|40", "1|50", "1|60", "1|70", "1|90"})
	w.Want("comma join with OR WHERE",
		"SELECT d.did, u.uid FROM d, u WHERE u.ua = d.da OR u.ub = d.db2 ORDER BY d.did, u.uid",
		[]string{"1|10", "1|20", "1|40", "1|50", "1|60", "1|70", "1|90"})
	w.Want("single-table OR",
		"SELECT uid FROM u WHERE ua = 100 OR ub = 200 ORDER BY uid",
		[]string{"10", "20", "40", "50", "60", "70", "90"})
	w.Want("single-table three-way OR",
		"SELECT uid FROM u WHERE ua = 100 OR ub = 200 OR uc = 300 ORDER BY uid",
		[]string{"10", "20", "30", "40", "50", "60", "70", "90"})
	w.Want("single-table OR count",
		"SELECT COUNT(*) FROM u WHERE ua = 100 OR ub = 200",
		[]string{"7"})
	w.Want("correlated EXISTS with OR",
		"SELECT d.did FROM d WHERE EXISTS (SELECT 1 FROM u WHERE u.ua = d.da OR u.ub = d.db2) ORDER BY d.did",
		[]string{"1"})
	w.Want("IN-list is not a union defect",
		"SELECT uid FROM u WHERE ua IN (100, 1) ORDER BY uid",
		[]string{"10", "20", "30", "40", "50", "60", "70", "80", "90"})
}

// TestFDB_OrUnionPrimaryKeyDedup_ScaleAndMaintenance runs the union path over a
// table large enough that a full scan is not the cheap alternative, and keeps
// checking it while rows are mutated underneath. Index maintenance changes which
// leg produces a record, so a record can move between legs and into and out of
// the overlap.
func TestFDB_OrUnionPrimaryKeyDedup_ScaleAndMaintenance(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	w := mmNewTwin(t, ctx, "/testdb_orunion_scale", "orunsc",
		"CREATE TABLE d (did BIGINT, da BIGINT, db2 BIGINT, PRIMARY KEY (did)) "+
			"CREATE TABLE u (uid BIGINT, ua BIGINT, ub BIGINT, pad STRING, PRIMARY KEY (uid)) ",
		"CREATE INDEX u_ua ON u (ua) CREATE INDEX u_ub ON u (ub) ")
	w.Exec("INSERT INTO d (did, da, db2) VALUES (1, 7, 9)")

	const n = 1500
	var vals []string
	for i := 1; i <= n; i++ {
		ua, ub := 1000+i, 5000+i
		switch i {
		case 1:
			ua, ub = 7, 9 // both legs
		case 2:
			ua, ub = 7, 100000 // first leg only
		case 3:
			ua, ub = 200000, 9 // second leg only
		}
		vals = append(vals, fmt.Sprintf("(%d, %d, %d, 'padpadpad')", i, ua, ub))
	}
	for start := 0; start < len(vals); start += 150 {
		end := start + 150
		if end > len(vals) {
			end = len(vals)
		}
		w.Exec("INSERT INTO u (uid, ua, ub, pad) VALUES " + strings.Join(vals[start:end], ", "))
	}

	q := "SELECT d.did, u.uid FROM d LEFT JOIN u ON u.ua = d.da OR u.ub = d.db2 ORDER BY d.did, u.uid"
	w.Want("at scale", q, []string{"1|1", "1|2", "1|3"})

	// Move row 2 into the overlap: it now satisfies both legs.
	w.Exec("UPDATE u SET ub = 9 WHERE uid = 2")
	w.Want("after a row moves into the overlap", q, []string{"1|1", "1|2", "1|3"})

	// Move row 1 out of the first leg: it is now second-leg only.
	w.Exec("UPDATE u SET ua = 424242 WHERE uid = 1")
	w.Want("after a row leaves one leg", q, []string{"1|1", "1|2", "1|3"})

	// Delete an overlapping row.
	w.Exec("DELETE FROM u WHERE uid = 2")
	w.Want("after an overlapping row is deleted", q, []string{"1|1", "1|3"})

	// Insert a fresh overlapping row.
	w.Exec("INSERT INTO u (uid, ua, ub, pad) VALUES (9999, 7, 9, 'padpadpad')")
	w.Want("after a fresh overlapping row is inserted", q, []string{"1|1", "1|3", "1|9999"})

	// Removing the last row of one leg leaves the other leg intact.
	w.Exec("DELETE FROM u WHERE uid = 3")
	w.Want("after one leg is emptied", q, []string{"1|1", "1|9999"})
}
