package embedded

import "testing"

// planCacheHitsSame reports whether two (scope, sql) pairs land on the same
// cache entry — the ground truth for injectivity/scoping, exercised through
// the REAL PlanCache key path (scope kept verbatim, sql normalized).
func planCacheHitsSame(t *testing.T, scopeA, sqlA, scopeB, sqlB string) bool {
	t.Helper()
	c := NewPlanCache(16)
	p := &stubPlan{label: "p"}
	c.Put(scopeA, sqlA, p, nil)
	got, _, ok := c.Get(scopeB, sqlB)
	return ok && got == p
}

// TestPlanCacheKey_Injective pins the fix for the non-injective key (item 3c):
// q.GetText() concatenated tokens with no separator, so structurally different
// queries collapsed to the same string and shared a cache entry — a wrong-plan
// bug. The canonical query text (normalized inside PlanCache) must keep them
// distinct while still sharing equivalent spellings.
func TestPlanCacheKey_Injective(t *testing.T) {
	t.Parallel()

	sqlOf := func(sql string) string { return canonicalTextOf(parseQuery(t, sql)) }

	// The canonical collision case: `SELECT AB FROM T` (one column AB) vs
	// `SELECT A B FROM T` (column A aliased B). GetText() gives "SELECTABFROMT"
	// for BOTH.
	if parseQuery(t, "SELECT AB FROM T").GetText() != parseQuery(t, "SELECT A B FROM T").GetText() {
		t.Fatal("precondition: GetText() must collide for these (test no longer exercises the bug)")
	}
	if planCacheHitsSame(t, "S", sqlOf("SELECT AB FROM T"), "S", sqlOf("SELECT A B FROM T")) {
		t.Fatal("non-injective cache key: `SELECT AB` and `SELECT A B` share an entry")
	}
	// Delimited identifiers are case-sensitive: `"a"` vs `"A"` are distinct.
	if planCacheHitsSame(t, "S", sqlOf(`SELECT "a" FROM T`), "S", sqlOf(`SELECT "A" FROM T`)) {
		t.Fatal("quoted-identifier case collapsed")
	}
	// String-literal whitespace is significant.
	if planCacheHitsSame(t, "S", sqlOf(`SELECT * FROM T WHERE x = 'a b'`), "S", sqlOf(`SELECT * FROM T WHERE x = 'ab'`)) {
		t.Fatal("string-literal whitespace collapsed")
	}

	// Equivalent spellings (case, whitespace, comments — both -- and #) STILL
	// share; the fix must not over-partition, and must not churn on trace
	// comments (item 3c — trace-comment churn).
	base := sqlOf("SELECT AB FROM T")
	for _, v := range []string{
		"select ab from t",
		"SELECT   AB   FROM   T",
		"SELECT AB FROM T -- trace",
		"SELECT # trace\n AB FROM T",
	} {
		if !planCacheHitsSame(t, "S", sqlOf(v), "S", base) {
			t.Fatalf("equivalent spelling %q did not share the base entry (cache churn)", v)
		}
	}
}

// TestPlanCacheKey_SchemaScoped pins the fix for the unscoped key (item 3c):
// SetSchema mutates only the session schema, never the cache, so the same SQL
// resolving against a different schema/version must key differently. Schema
// names are CASE-SENSITIVE, so `s` and `S` must not collide (the
// scope must bypass normalizeSQL).
func TestPlanCacheKey_SchemaScoped(t *testing.T) {
	t.Parallel()

	sql := canonicalTextOf(parseQuery(t, "SELECT id FROM orders"))

	if planCacheHitsSame(t, planCacheScope("SCHEMA_A", 0), sql, planCacheScope("SCHEMA_B", 0), sql) {
		t.Fatal("same SQL under different schemas shares a cache entry — SET SCHEMA staleness")
	}
	if planCacheHitsSame(t, planCacheScope("SCHEMA_A", 1), sql, planCacheScope("SCHEMA_A", 2), sql) {
		t.Fatal("same SQL under different metadata versions shares a cache entry")
	}
	// case-distinct schemas are DISTINCT (the scope is not folded).
	if planCacheHitsSame(t, planCacheScope("s", 0), sql, planCacheScope("S", 0), sql) {
		t.Fatal("case-distinct schemas `s` and `S` collided — scope was normalized (wrong-schema plan)")
	}
	// Scope must not bleed into query text: schema "A" + query B... must not
	// equal schema "" + query AB...
	if planCacheHitsSame(t, planCacheScope("A", 0), sql, planCacheScope("", 0), "A"+sql) {
		t.Fatal("schema scope bled into query text")
	}
}
