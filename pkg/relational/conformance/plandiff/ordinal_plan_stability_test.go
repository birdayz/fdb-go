package plandiff

import (
	"context"
	"testing"
)

// ordinalSeedPlanCorpus is an N-way ordinal-seed plan corpus:
// catalog-aware (typed legs, so the translator's gated ordinal seed actually
// fires — a text-only join flows UnknownType legs and falls back to the name
// model, never exercising the ordinal path) maximal inner-join clusters of
// arity >= 3 — the shapes admitted by the join-cluster arity gate (`if a >= 2`).
// Chain, star, and self-join topologies.
func ordinalSeedPlanCorpus() []Query {
	return []Query{
		{
			Name: "ordinal_3way_chain",
			SchemaTemplate: "CREATE TABLE A (id BIGINT NOT NULL, b_id BIGINT, PRIMARY KEY (id)) " +
				"CREATE TABLE B (id BIGINT NOT NULL, c_id BIGINT, PRIMARY KEY (id)) " +
				"CREATE TABLE C (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))",
			SQL: "SELECT a.id, c.name FROM A a, B b, C c WHERE a.b_id = b.id AND b.c_id = c.id",
		},
		{
			Name: "ordinal_3way_star",
			SchemaTemplate: "CREATE TABLE H (id BIGINT NOT NULL, PRIMARY KEY (id)) " +
				"CREATE TABLE X (id BIGINT NOT NULL, hid BIGINT, v BIGINT, PRIMARY KEY (id)) " +
				"CREATE TABLE Y (id BIGINT NOT NULL, hid BIGINT, w BIGINT, PRIMARY KEY (id))",
			SQL: "SELECT h.id, x.v, y.w FROM H h, X x, Y y WHERE h.id = x.hid AND h.id = y.hid",
		},
		{
			Name:           "ordinal_self_3way",
			SchemaTemplate: "CREATE TABLE NODE (id BIGINT NOT NULL, parent BIGINT, name STRING, PRIMARY KEY (id))",
			SQL:            "SELECT g.id, p.name, gp.name FROM NODE g, NODE p, NODE gp WHERE g.parent = p.id AND p.parent = gp.id",
		},
	}
}

// ordinalSeedPlanGolden pins the exact Go Explain tree each N-way ordinal-seed
// shape plans to. These are REGENERATED, not hand-written: run
// TestOrdinalPlanShapeStable with -run and read the failure diff, or
// temporarily log eng.Plan(q).Tree. Regenerate ONLY on an intentional plan-shape
// change (and then the vs-Java plan-tree harness — plandiff.Run with a live
// NewJavaEngineHTTP — must confirm the new shape still matches Java).
// The renderings carry plan-time-baked ordinals: each WHERE-conjunct
// leg reference shows its LEG-RELATIVE ordinal (`A.B_ID#1` — column 1 of A's
// own row), and the self-join's duplicated NAME leaves carry their
// qualified-alias pins (`P.NAME AS P.NAME`). The TREE (operators, join order,
// column set) is byte-identical to the pre-bake golden — a representation
// change, not a plan change.
var ordinalSeedPlanGolden = map[string]string{
	"ordinal_3way_chain": `Project(A.ID, C.NAME)
  Filter((A.B_ID#1 = B.ID#0 AND B.C_ID#1 = C.ID#0))
    InnerJoin
      InnerJoin
        Scan(A)
        Scan(B)
      Scan(C)`,
	"ordinal_3way_star": `Project(H.ID, X.V, Y.W)
  Filter((H.ID#0 = X.HID#1 AND H.ID#0 = Y.HID#1))
    InnerJoin
      InnerJoin
        Scan(H)
        Scan(X)
      Scan(Y)`,
	"ordinal_self_3way": `Project(G.ID, P.NAME AS P.NAME, GP.NAME AS GP.NAME)
  Filter((G.PARENT#1 = P.ID#0 AND P.PARENT#1 = GP.ID#0))
    InnerJoin
      InnerJoin
        Scan(NODE AS G)
        Scan(NODE AS P)
      Scan(NODE AS GP)`,
}

// TestOrdinalPlanShapeStable pins the plan shape for N-way ordinal-seed
// joins — the Java-INDEPENDENT half of "plandiff byte-identical". The
// internal column model resolves by ordinal rather than by name, which is a
// REPRESENTATION choice, so the planned SHAPE (the Explain tree) must not
// move because of it. This pins that shape for the N-way ordinal-seed corpus
// (arity >= 3), and plans each shape repeatedly to catch non-determinism (a
// non-deterministic plan is ALWAYS an alias/interning bug, never noise).
//
// The CROSS-ENGINE half is wired separately: the N-way inner-join entries in
// SeedRunCorpus assert Go ROWS == Java rows (a wrong ordinal plan yields wrong
// rows), and select_inner_join in SeedCorpus pins the 2-way ordinal plan TREE
// byte-identical to Java. Together with the cascades-level dispatch
// (MergeArmHits==0) and interning (task-count ±2%) pins, these confirm that
// resolving by ordinal is a representation choice, not a plan-shape change.
func TestOrdinalPlanShapeStable(t *testing.T) {
	t.Parallel()
	eng := NewGoEngine()
	ctx := context.Background()
	for _, q := range ordinalSeedPlanCorpus() {
		q := q
		t.Run(q.Name, func(t *testing.T) {
			t.Parallel()
			want, ok := ordinalSeedPlanGolden[q.Name]
			if !ok {
				t.Fatalf("no golden for %s — add one after reviewing the plan", q.Name)
			}
			// Plan several times: an ordinal-seed plan-shape must be
			// deterministic (the alias-aware interning tier makes the
			// re-enumeration sub-products stable across explorations).
			var first string
			for i := 0; i < 5; i++ {
				r := eng.Plan(ctx, q)
				if r.Err != nil {
					t.Fatalf("%s: plan error: %v\n  sql: %s", q.Name, r.Err, q.SQL)
				}
				if i == 0 {
					first = r.Tree
					continue
				}
				if r.Tree != first {
					t.Fatalf("%s: NON-DETERMINISTIC plan across runs:\n--- run 0 ---\n%s\n--- run %d ---\n%s",
						q.Name, first, i, r.Tree)
				}
			}
			if first != want {
				t.Errorf("%s: ordinal-seed plan SHAPE changed:\n--- want (golden) ---\n%s\n--- got ---\n%s\n"+
					"If intentional, regenerate the golden AND confirm the new shape still matches Java via the plan-tree harness.",
					q.Name, want, first)
			}
		})
	}
}
