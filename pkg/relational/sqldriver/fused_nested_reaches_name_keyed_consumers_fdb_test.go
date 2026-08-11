package sqldriver_test

// The CONSUMER-side pin for a fused nested reference.
//
// The mint is pinned as an invariant elsewhere: the display Field of a fused
// value is its LEAF (Java's getLastFieldName). Nothing asserted the other half —
// that no consumer may key on that single name — and that is the dimension whose
// absence let a real regression ship. Every pre-existing nested test used names
// that COULD NOT COLLIDE, so a consumer reading the path and a consumer reading
// the leaf name rendered identically and the suite could not tell them apart.
//
// So the fixture is built entirely around the collision: every struct member here
// is spelled exactly like a TOP-LEVEL column of a different type. Remove that and
// this file asserts nothing. `n.sk` is BIGINT and `sk` is DOUBLE; `n.co` is
// STRING and `co` is BIGINT. The row values are chosen so the two columns induce
// DIFFERENT orderings and different aggregates, which is what makes a wrong read
// visible as wrong ROWS rather than as a coincidence.
//
// WHAT THIS PINS, stated as the property rather than as the list: a nested
// reference's identity is its resolved PATH, and every row-producing consumer
// must take it from there. Measured: this whole battery answers identically
// whether the mint names the fused value after its ROOT or after its LEAF, which
// is the empirical form of the structural claim that these channels never read
// Field at all. That makes the battery a sentinel for the CLASS — it goes red the
// day a consumer starts recovering a column from the single name — rather than a
// discriminator for any one mint.
//
// The metadata channel is the one that DID read Field, and it is pinned
// separately and discriminatingly in
// fused_nested_operand_types_from_its_own_leaf_fdb_test.go.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestFDB_FusedNestedReferenceSurvivesNameKeyedConsumers(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fnkc")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fnkc")
	// Every member collides with a top-level column of a DIFFERENT type. See the
	// header before touching either side of a pair.
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fnkc "+
			"CREATE TYPE AS STRUCT nn (sk BIGINT, co STRING) "+
			"CREATE TABLE t (id BIGINT, n nn, sk DOUBLE, co BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fnkc/s WITH TEMPLATE fnkc")
	dsn := fmt.Sprintf("fdbsql:///testdb_fnkc?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// n.sk ascends 30,10,20 while the flat sk ascends 1.5,2.5,3.5 — the two
	// columns order the table DIFFERENTLY, which is what lets an ordering
	// assertion below name the column that was actually read.
	for _, ins := range []string{
		"INSERT INTO t (id, n, sk, co) VALUES (1, (30, 'a'), 1.5, 7)",
		"INSERT INTO t (id, n, sk, co) VALUES (2, (10, 'b'), 2.5, 8)",
		"INSERT INTO t (id, n, sk, co) VALUES (3, (20, 'c'), 3.5, 9)",
	} {
		if _, e := db.ExecContext(ctx, ins); e != nil {
			t.Fatalf("%s: %v", ins, e)
		}
	}

	idsOf := func(q string) []int64 {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if e := rows.Scan(&id); e != nil {
				t.Fatalf("scan %q: %v", q, e)
			}
			out = append(out, id)
		}
		if e := rows.Err(); e != nil {
			t.Fatalf("rows %q: %v", q, e)
		}
		return out
	}
	eq := func(a []int64, b ...int64) bool {
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

	// ANTI-VACUITY, asserted first: the member and its flat namesake must induce
	// different orders. If they ever agree, every ordering assertion below is
	// satisfied by reading either column and this test has quietly stopped
	// testing anything.
	byMember := idsOf("SELECT id FROM t ORDER BY n.sk")
	byFlat := idsOf("SELECT id FROM t ORDER BY sk")
	if eq(byMember, byFlat...) {
		t.Fatalf("ORDER BY n.sk and ORDER BY sk both return %v, so no assertion in this "+
			"test can tell which column was read — restore the fixture's divergent orders",
			byMember)
	}
	if !eq(byFlat, 1, 2, 3) {
		t.Fatalf("ORDER BY sk (the FLAT column) = %v, want [1 2 3]; the control itself is "+
			"wrong, so nothing below can be trusted", byFlat)
	}

	// THE SORT KEY. A nested key is named for the hidden column it is appended
	// as, and the naming used to run through a FLAT rendering of the reference —
	// one segment standing in for a path. That segment is the leaf, and the leaf
	// collides with `sk`.
	if !eq(byMember, 2, 3, 1) {
		t.Errorf("ORDER BY n.sk = %v, want [2 3 1] — the MEMBER's ascending order "+
			"(10,20,30). [1 2 3] is the FLAT column `sk`'s order (1.5,2.5,3.5), which is "+
			"what a sort that recovered its column from the reference's single display "+
			"name would produce.", byMember)
	}
	if got := idsOf("SELECT id FROM t ORDER BY n.sk DESC"); !eq(got, 1, 3, 2) {
		t.Errorf("ORDER BY n.sk DESC = %v, want [1 3 2]", got)
	}
	// The member sorted while ALSO projected: the projection and the hidden sort
	// column are two renderings of one reference and must agree.
	if got := idsOf("SELECT id FROM t ORDER BY n.co"); !eq(got, 1, 2, 3) {
		t.Errorf("ORDER BY n.co = %v, want [1 2 3] — the STRING member's order. The flat "+
			"namesake `co` is a BIGINT.", got)
	}

	// THE PREDICATE. `n.sk = 10` selects row 2; the flat `sk` has no value 10 at
	// all, so a read of the wrong column returns nothing rather than the wrong
	// row — an empty result is the failure signature here.
	if got := idsOf("SELECT id FROM t WHERE n.sk = 10"); !eq(got, 2) {
		t.Errorf("WHERE n.sk = 10 = %v, want [2]. An empty result means the predicate was "+
			"evaluated against the flat DOUBLE column `sk`, which holds no 10.", got)
	}
	if got := idsOf("SELECT id FROM t WHERE sk = 1.5"); !eq(got, 1) {
		t.Errorf("WHERE sk = 1.5 = %v, want [1] — the flat control", got)
	}

	// THE AGGREGATE OPERAND. SUM/MIN/MAX inherit their operand's column, and the
	// inheritance is name-keyed against the record descriptor for the flat case.
	// SUM over the member is 60; over the flat namesake it is 7.5, so the two are
	// not merely different numbers but different TYPES.
	var sumMember int64
	if e := db.QueryRowContext(ctx, "SELECT SUM(n.sk) FROM t").Scan(&sumMember); e != nil {
		t.Fatalf("SUM(n.sk): %v", e)
	}
	if sumMember != 60 {
		t.Errorf("SUM(n.sk) = %d, want 60 (10+20+30, the MEMBER). The flat `sk` sums to 7.5.",
			sumMember)
	}
	var sumFlat float64
	if e := db.QueryRowContext(ctx, "SELECT SUM(sk) FROM t").Scan(&sumFlat); e != nil {
		t.Fatalf("SUM(sk): %v", e)
	}
	if sumFlat != 7.5 {
		t.Errorf("SUM(sk) = %v, want 7.5 — the flat control", sumFlat)
	}
	var maxMember int64
	if e := db.QueryRowContext(ctx, "SELECT MAX(n.sk) FROM t").Scan(&maxMember); e != nil {
		t.Fatalf("MAX(n.sk): %v", e)
	}
	if maxMember != 30 {
		t.Errorf("MAX(n.sk) = %d, want 30", maxMember)
	}
}

// TestFDB_NestedMemberSpelledLikeItsUnnestAliasIsRefused pins a NEGATIVE result,
// and pins it because a decision rests on it.
//
// The unnest element-substitution arm rewrites a reference that IS the bound
// element into the element's quantifier object, and it selects that arm by
// comparing the reference's single display name against the binding's AS/AT
// alias. A fused nested reference has only one name to offer for a whole path,
// so the comparison can be satisfied by a reference that is NOT the element:
// `orders.items AS sku` binding a member also spelled `sku`. The arm is gated on
// the accessor count for exactly that reason.
//
// That gate cannot be reached from SQL today, and this test is why: the resolver
// refuses the colliding spelling with 42702 before any consumer sees it. That
// refusal is the ONLY thing standing between the arm and a wrong read — it is
// load-bearing, and nothing else asserted it. Relax it (make `sku.sku` resolve to
// the member, which is what it unambiguously names) and the arity gate becomes
// live machinery rather than defence in depth.
//
// The measured history is what makes this worth pinning rather than shrugging at.
// Under the mint that named a fused value after its struct ROOT the comparison
// matched for EVERY member reference, because the root of a descent through an
// unnest binding IS the alias: `WHERE i.sku = 'x'` over `orders.items AS i`
// reached the arm with Field="I" and was replaced by the whole element. Naming
// the value after its leaf narrowed that to the collision this test shows is
// unreachable. The gate closes the remainder.
func TestFDB_NestedMemberSpelledLikeItsUnnestAliasIsRefused(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fnua")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fnua")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fnua "+
			"CREATE TYPE AS STRUCT item (sku STRING, qty BIGINT) "+
			"CREATE TABLE orders (order_id BIGINT, items item ARRAY, PRIMARY KEY (order_id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fnua/s WITH TEMPLATE fnua")
	dsn := fmt.Sprintf("fdbsql:///testdb_fnua?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, e := db.ExecContext(ctx,
		"INSERT INTO orders (order_id, items) VALUES (1, [('x', 10), ('y', 20)])"); e != nil {
		t.Fatalf("insert: %v", e)
	}

	// The CONTROL: a distinctly-spelled alias descends into the element and
	// serves the MEMBER. Without this the refusal below could be a general
	// inability to descend through an unnest binding rather than a property of
	// the collision.
	rows, err := db.QueryContext(ctx, "SELECT i.sku FROM orders, orders.items AS i")
	if err != nil {
		t.Fatalf("distinct alias `AS i`: %v", err)
	}
	var got []string
	for rows.Next() {
		var s string
		if e := rows.Scan(&s); e != nil {
			rows.Close()
			t.Fatalf("scan i.sku: %v — a struct was served where the STRING member was asked for", e)
		}
		got = append(got, s)
	}
	rows.Close()
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("SELECT i.sku = %v, want [x y] — the members of the element, not the element", got)
	}

	// The SUBJECT: the colliding spelling is refused, and refused as AMBIGUOUS
	// rather than as undefined — the engine sees two readings and declines to
	// pick, which is the behaviour the arity gate assumes it can rely on.
	for _, q := range []string{
		"SELECT sku.sku FROM orders, orders.items AS sku",
		"SELECT order_id FROM orders, orders.items AS sku WHERE sku.sku = 'x'",
		"SELECT qty.qty FROM orders, orders.items AS qty",
	} {
		r, e := db.QueryContext(ctx, q)
		if e == nil {
			r.Close()
			t.Errorf("%s: planned and ran. This spelling used to be refused 42702, and that "+
				"refusal is what kept the unnest element-substitution arm away from a "+
				"reference whose LEAF is spelled like the binding's alias. If the refusal "+
				"was relaxed deliberately, the arity gate in that arm "+
				"(cascades_translator.go, the unnest alias switch) is now LIVE machinery "+
				"and needs a row-level test proving the member is served rather than the "+
				"whole element.", q)
			continue
		}
		if !contains(e.Error(), "42702") {
			t.Errorf("%s: error = %v, want 42702 (ambiguous). A different refusal is fine "+
				"for the user but changes what this test is pinning.", q, e)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
