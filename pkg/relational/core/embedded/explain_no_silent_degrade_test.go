package embedded

import (
	"context"
	"strings"
	"testing"
)

// EXPLAIN must never render a plan the engine cannot execute. computeExplainText
// splits on that rule: where Cascades is the plan, its failure is the answer;
// where planSelect itself never reaches Cascades, logical text IS the plan the
// statement runs and rendering it is accurate.
//
// These pin the second half — the two arms that legitimately stay quiet. The
// loud half needs a live Cascades attempt and is pinned over FDB in
// pkg/relational/sqldriver (TestFDB_ExplainUnplannableQueryFailsLoudly).

// explainOnlyText plans `EXPLAIN sql` through a generator with no FDB session
// and returns the rendered PLAN text.
func explainOnlyText(t *testing.T, sql string) string {
	t.Helper()
	gen := NewExplainOnlyGenerator()
	plan, err := gen.Plan(context.Background(), "EXPLAIN "+sql)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", sql, err)
	}
	return strings.TrimPrefix(plan.Explain(), "EXPLAIN: ")
}

// Explain-only mode has no FDB session, so planSelect routes to
// planSelectExplainOnly and the logical text IS the plan — EXPLAIN agreeing
// with it is accurate, not a degrade. Without this arm, every plandiff /
// explain-equivalence harness query would start erroring.
func TestExplainOnlyMode_StillRendersLogicalText(t *testing.T) {
	t.Parallel()
	got := explainOnlyText(t, "SELECT * FROM t WHERE id > 5")
	if want := "Filter(id > 5)\n  Scan(T)"; got != want {
		t.Fatalf("explain-only text = %q, want %q", got, want)
	}
}

// explainOnlyTextWithSchema plans `EXPLAIN sql` through a catalog-aware
// explain-only generator (session bound, still no DB) and returns the PLAN text.
func explainOnlyTextWithSchema(t *testing.T, schemaDDL, sql string) string {
	t.Helper()
	gen, err := NewExplainOnlyGeneratorWithSchema(schemaDDL)
	if err != nil {
		t.Fatalf("NewExplainOnlyGeneratorWithSchema: %v", err)
	}
	plan, err := gen.Plan(context.Background(), "EXPLAIN "+sql)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", sql, err)
	}
	return strings.TrimPrefix(plan.Explain(), "EXPLAIN: ")
}

// The catalog-aware explain-only generator binds a session but still no DB, so
// it takes the same arm and must render the catalog-resolved logical tree.
//
// The assertion is exact, on the one token that separates the two builders:
// buildLogicalPlanForQueryWithCatalog resolves the column to the ordinal
// `ID#0`, buildLogicalPlanForQuery echoes the raw source text `id > 5`. Both
// render `Scan(T)` and both start with `Filter(`, so a Contains check on those
// would stay green through exactly the silent drop back to the text builder
// that this test exists to catch.
func TestExplainOnlyModeWithSchema_StillRendersLogicalText(t *testing.T) {
	t.Parallel()
	got := explainOnlyTextWithSchema(t,
		"CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))",
		"SELECT * FROM t WHERE id > 5")
	if want := "Filter(ID#0 > 5)\n  Scan(T)"; got != want {
		t.Fatalf("explain-only-with-schema text = %q, want %q", got, want)
	}
}

// The cross-derived predicate in the flattening-evasion shape must survive
// TRANSLATION even though Cascades cannot plan the shape. This assertion used
// to ride on EXPLAIN over FDB, where it was pinning the silent-degrade defect
// (the query raised 0AF00 while EXPLAIN happily printed logical text). The
// property is real; explain-only mode is the layer where logical text is the
// honest answer, so it is asserted here instead.
//
// The generator is the catalog-aware one, carrying the same four tables: the
// deleted FDB subtest ran with metadata already cached by its sibling, so it
// exercised buildLogicalPlanForQueryWithCatalog. A bare NewExplainOnlyGenerator
// has nil metadata and would silently pin the weaker text-builder path instead.
func TestExplainOnlyMode_KeepsCrossDerivedPredicate(t *testing.T) {
	t.Parallel()
	const schema = "CREATE SCHEMA TEMPLATE evasion_tmpl " +
		"CREATE TABLE a (id BIGINT NOT NULL, av BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE b (id BIGINT NOT NULL, a_id BIGINT, bv BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE c (id BIGINT NOT NULL, cv BIGINT, PRIMARY KEY (id)) " +
		"CREATE TABLE d (id BIGINT NOT NULL, c_id BIGINT, dw BIGINT, PRIMARY KEY (id))"
	const evasion = "SELECT t1.aid, t1.bv, t2.cid, t2.dw " +
		"FROM (SELECT a.id AS aid, b.bv AS bv FROM a JOIN b ON b.a_id = a.id) t1, " +
		"(SELECT c.id AS cid, d.dw AS dw FROM c JOIN d ON d.c_id = c.id) t2 " +
		"WHERE t1.aid = t2.cid"
	got := explainOnlyTextWithSchema(t, schema, evasion)
	if !strings.Contains(got, "Filter(t1.aid = t2.cid)") {
		t.Fatalf("logical plan lost the cross-derived predicate:\n%s", got)
	}
}

// The whole point of the surviving arms is that EXPLAIN agrees with the plan
// the statement itself produces, so assert that directly rather than trusting
// the two renderings to stay in step by inspection: for every arm,
// `EXPLAIN q` must equal `Plan(q).Explain()`.
//
// The third case is the one that used to diverge. Both logical builders return
// nil for a non-ALL UNION (buildLogicalPlanForUnion bails when `ALL()` is nil),
// so explainLogicalQuery fell through to `("", nil)` and planExplain raised
// "EXPLAIN inner statement produced no plan text" — while the query's own plan
// happily rendered the statement echo.
func TestExplainMirrorsThePlansOwnExplain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"catalog_builder", "SELECT * FROM t WHERE id > 5"},
		{"union_all_text_builder", "SELECT id FROM t UNION ALL SELECT v FROM t"},
		// Non-ALL UNION: neither builder produces a tree, so both sides must
		// land on the echo-the-statement last resort.
		{"union_distinct_last_resort", "SELECT id FROM t UNION SELECT v FROM t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const schema = "CREATE SCHEMA TEMPLATE mirror_tmpl " +
				"CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))"
			gen, err := NewExplainOnlyGeneratorWithSchema(schema)
			if err != nil {
				t.Fatalf("NewExplainOnlyGeneratorWithSchema: %v", err)
			}
			bare, err := gen.Plan(context.Background(), tc.sql)
			if err != nil {
				t.Fatalf("plan %q: %v", tc.sql, err)
			}
			want := bare.Explain()
			explained, err := gen.Plan(context.Background(), "EXPLAIN "+tc.sql)
			if err != nil {
				t.Fatalf("EXPLAIN %q: %v", tc.sql, err)
			}
			got := strings.TrimPrefix(explained.Explain(), "EXPLAIN: ")
			if got != want {
				t.Fatalf("EXPLAIN text diverged from the plan's own Explain:\n EXPLAIN: %q\n    plan: %q", got, want)
			}
		})
	}
}

// DML EXPLAIN renders logical text in both directions; pinned so the SELECT
// split above cannot accidentally take the DML arms with it. (Java renders a
// physical plan here instead — a recorded gap, not this test's subject.)
func TestExplainDML_StillRendersLogicalText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{"delete", "DELETE FROM t WHERE id > 5", "Delete"},
		{"insert", "INSERT INTO t (id) VALUES (1)", "Insert"},
		{"update", "UPDATE t SET v = 1 WHERE id > 5", "Update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := explainOnlyText(t, tc.sql)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("EXPLAIN %s = %q, want it to contain %q", tc.sql, got, tc.want)
			}
		})
	}
}
