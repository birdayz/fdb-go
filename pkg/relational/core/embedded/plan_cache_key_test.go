package embedded

import (
	"testing"

	"fdb.dev/pkg/relational/api"
)

// planCacheHitsSame reports whether two (scope, sql) pairs land on the same
// cache entry — the ground truth for injectivity/scoping, exercised through
// the REAL PlanCache key path (scope and key text kept verbatim).
func planCacheHitsSame(t *testing.T, scopeA, sqlA, scopeB, sqlB string) bool {
	t.Helper()
	c := NewPlanCache(16)
	p := &stubPlan{label: "p"}
	c.Put(scopeA, sqlA, p, nil)
	got, _, ok := c.Get(scopeB, sqlB)
	return ok && got == p
}

// TestPlanCacheKey_Injective: the key text is rendered from tokens, so
// structurally different queries never share an entry while spellings that
// differ only in keyword/identifier case, whitespace or comments do.
func TestPlanCacheKey_Injective(t *testing.T) {
	t.Parallel()

	sqlOf := func(sql string) string { return planCacheText(parseQuery(t, sql)) }
	for _, pair := range [][2]string{
		{"SELECT AB FROM T", "SELECT A B FROM T"},
		{`SELECT "a" FROM T`, `SELECT "A" FROM T`},
		{`SELECT * FROM T WHERE x = 'a b'`, `SELECT * FROM T WHERE x = 'ab'`},
		{`SELECT * FROM T WHERE x = 'a b'`, `SELECT * FROM T WHERE x = 'a' 'b'`},
		{"SELECT * FROM T WHERE b = B64'YWJj'", "SELECT * FROM T WHERE b = B64'ywjj'"},
		{"SELECT * FROM T WHERE b = X'0A'", "SELECT * FROM T WHERE b = X'0a'"},
	} {
		if planCacheHitsSame(t, "S", sqlOf(pair[0]), "S", sqlOf(pair[1])) {
			t.Fatalf("%q and %q share a cache entry", pair[0], pair[1])
		}
	}

	base := sqlOf("SELECT AB FROM T")
	for _, v := range []string{
		"select ab from t",
		"SELECT   AB   FROM   T",
		"SELECT AB FROM T -- trace",
		"SELECT /* trace /* nested */ */ AB\nFROM T",
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
// scope is kept verbatim).
func TestPlanCacheKey_SchemaScoped(t *testing.T) {
	t.Parallel()

	sql := planCacheText(parseQuery(t, "SELECT id FROM orders"))

	if planCacheHitsSame(t, planCacheScope("", "SCHEMA_A", 0, ""), sql, planCacheScope("", "SCHEMA_B", 0, ""), sql) {
		t.Fatal("same SQL under different schemas shares a cache entry — SET SCHEMA staleness")
	}
	if planCacheHitsSame(t, planCacheScope("", "SCHEMA_A", 1, ""), sql, planCacheScope("", "SCHEMA_A", 2, ""), sql) {
		t.Fatal("same SQL under different metadata versions shares a cache entry")
	}
	// case-distinct schemas are DISTINCT (the scope is not folded).
	if planCacheHitsSame(t, planCacheScope("", "s", 0, ""), sql, planCacheScope("", "S", 0, ""), sql) {
		t.Fatal("case-distinct schemas `s` and `S` collided — scope was normalized (wrong-schema plan)")
	}
	// Scope must not bleed into query text: schema "A" + query B... must not
	// equal schema "" + query AB...
	if planCacheHitsSame(t, planCacheScope("", "A", 0, ""), sql, planCacheScope("", "", 0, ""), "A"+sql) {
		t.Fatal("schema scope bled into query text")
	}
}

// TestPlanCacheKey_DBPathScoped pins that the plan-cache scope separates two
// DATABASES, not just two schemas within one. A schema name is unique only
// within its database, and the multi-tenant shape is exactly two databases
// each holding a schema of the same name — different table sets, different
// subspaces, different plans. With the database path outside the scope, the
// two tenants' identical SQL lands on one cache entry, and whichever tenant
// planned first has its compiled plan served to the other.
//
// This is the same wrong-plan family as TestPlanCacheKey_SchemaScoped above,
// one level up. It is unreachable today only because DBPath is written once
// per connection (connection.go New/Reset) and USE DATABASE is unimplemented,
// so a single cache never sees two paths — an accident of two other files,
// not a property of this key. Making the path a scope component is what stops
// implementing USE DATABASE, or sharing a cache across connections, from
// silently arming it.
func TestPlanCacheKey_DBPathScoped(t *testing.T) {
	t.Parallel()

	sql := planCacheText(parseQuery(t, "SELECT id FROM orders"))

	if planCacheHitsSame(t,
		planCacheScope("/tenant_a", "MAIN", 0, ""), sql,
		planCacheScope("/tenant_b", "MAIN", 0, ""), sql) {
		t.Fatal("same SQL, same schema NAME, different databases share a cache entry — " +
			"one tenant's compiled plan is served for another tenant's query")
	}
	// Database paths are case-sensitive for the same reason schema names are:
	// the scope is verbatim and must never be folded.
	if planCacheHitsSame(t,
		planCacheScope("/FRL/db", "MAIN", 0, ""), sql,
		planCacheScope("/FRL/DB", "MAIN", 0, ""), sql) {
		t.Fatal("case-distinct database paths `/FRL/db` and `/FRL/DB` collided — scope was normalized")
	}
	// Equal in every component still SHARES: the added component must not
	// over-partition the cache into a permanent 100% miss rate.
	if !planCacheHitsSame(t,
		planCacheScope("/tenant_a", "MAIN", 0, ""), sql,
		planCacheScope("/tenant_a", "MAIN", 0, ""), sql) {
		t.Fatal("identical (dbPath, schema, version, opts) did not share an entry — cache never hits")
	}
	// The path must not bleed into the neighbouring component: a path ending
	// in the delimiter must not be able to spell a different (path, schema)
	// split. This is the length-prefixing property, now over four components.
	if planCacheHitsSame(t,
		planCacheScope("/FRL/db"+planCacheScopeDelim+"4", "MAIN", 0, ""), sql,
		planCacheScope("/FRL/db", planCacheScopeDelim+"4MAIN", 0, ""), sql) {
		t.Fatal("database path bled into the schema component")
	}
}

// TestPlanCacheKey_PlannerOptionsScoped_Injective is the end-to-end proof that
// the cacheKeyPart injectivity fix actually protects the plan cache: two
// connections whose DISABLED_PLANNER_RULES option sets differ — one disabling
// two real rules as separate entries, the other disabling one inert,
// unrecognized name that happens to spell the same two names joined by a
// comma — must not serve one connection's cached plan to the other. Before
// cacheKeyPart length-prefixed its names, both rendered the identical
// ",PredicatePushDownRule,SelectMergeRule" component and shared one entry: a
// connection that asked for only the inert name disabled would have been
// served the plan built with both real rules disabled instead.
func TestPlanCacheKey_PlannerOptionsScoped_Injective(t *testing.T) {
	t.Parallel()

	sql := planCacheText(parseQuery(t, "SELECT id FROM orders"))

	twoRealRules := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"PredicatePushDownRule", "SelectMergeRule"}).Build())
	oneInertCommaName := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"PredicatePushDownRule,SelectMergeRule"}).Build())

	scopeA := planCacheScope("", "S", 0, twoRealRules.cacheKeyPart())
	scopeB := planCacheScope("", "S", 0, oneInertCommaName.cacheKeyPart())

	if scopeA == scopeB {
		t.Fatalf("plan-cache scope collides two different DISABLED_PLANNER_RULES option sets: %q", scopeA)
	}
	if planCacheHitsSame(t, scopeA, sql, scopeB, sql) {
		t.Fatal("a connection with two rules disabled and a connection with one inert comma-bearing " +
			"name disabled share a plan-cache entry — the plan built under one option set would be " +
			"served to a connection that asked for the other")
	}
}

// TestPlanCacheKey_PlannerOptionsScoped_Injective_Colon is the same
// end-to-end proof as TestPlanCacheKey_PlannerOptionsScoped_Injective, but
// for a ':' collision rather than a ','  one. It exists because a mutant that
// drops cacheKeyPart's length prefix but keeps a bare ':' delimiter passes
// the comma-based test above untouched (':' isn't ','), yet still lets two
// real rule names disabled as separate entries collide with one inert name
// that merely CONTAINS a colon — DISABLED_PLANNER_RULES names are
// user-controlled strings, never validated against the rule set (see
// optStringSet), so nothing stops a caller from choosing one.
func TestPlanCacheKey_PlannerOptionsScoped_Injective_Colon(t *testing.T) {
	t.Parallel()

	sql := planCacheText(parseQuery(t, "SELECT id FROM orders"))

	twoRealRules := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"A", "B"}).Build())
	oneInertColonName := plannerOptionsFrom(api.NewOptionsBuilder().
		Set(api.OptDisabledPlannerRules, []string{"A:B"}).Build())

	scopeA := planCacheScope("", "S", 0, twoRealRules.cacheKeyPart())
	scopeB := planCacheScope("", "S", 0, oneInertColonName.cacheKeyPart())

	if scopeA == scopeB {
		t.Fatalf("plan-cache scope collides two different DISABLED_PLANNER_RULES option sets: %q", scopeA)
	}
	if planCacheHitsSame(t, scopeA, sql, scopeB, sql) {
		t.Fatal("a connection with two rules disabled ({A, B}) and a connection with one inert " +
			"colon-bearing name disabled ({\"A:B\"}) share a plan-cache entry — the plan built under " +
			"one option set would be served to a connection that asked for the other")
	}
}
