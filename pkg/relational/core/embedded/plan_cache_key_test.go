package embedded

import "testing"

// TestPlanCacheKey_Injective pins the fix for the non-injective key
// (DIVERGENCES / item 3c bug 1): q.GetText() concatenated tokens with no
// separator, so structurally different queries collapsed to the same string
// and shared a cache entry — a wrong-plan bug. The schema-scoped canonical key
// (normalizeSQL(planCacheKeyInput(...))), the exact string PlanCache keys on,
// must keep them distinct.
func TestPlanCacheKey_Injective(t *testing.T) {
	t.Parallel()

	key := func(sql string) string {
		return normalizeSQL(planCacheKeyInput("S", 0, parseQuery(t, sql)))
	}

	// The canonical collision case: `SELECT AB FROM T` (one column AB) vs
	// `SELECT A B FROM T` (column A aliased B). GetText() gives "SELECTABFROMT"
	// for BOTH.
	ab := parseQuery(t, "SELECT AB FROM T")
	aSpaceB := parseQuery(t, "SELECT A B FROM T")
	if ab.GetText() != aSpaceB.GetText() {
		t.Fatalf("precondition: GetText() must collide for these — got %q vs %q (test no longer exercises the bug)",
			ab.GetText(), aSpaceB.GetText())
	}
	if k1, k2 := key("SELECT AB FROM T"), key("SELECT A B FROM T"); k1 == k2 {
		t.Fatalf("non-injective cache key: `SELECT AB` and `SELECT A B` share key %q", k1)
	}

	// Quoted-identifier case difference is a real difference (delimited ids are
	// case-sensitive); normalizeSQL only folds OUTSIDE string literals, but a
	// quoted id is not a single-quoted string, so this relies on the canonical
	// text preserving it. `"a"` vs `"A"` must not share a plan.
	if k1, k2 := key(`SELECT "a" FROM T`), key(`SELECT "A" FROM T`); k1 == k2 {
		t.Fatalf("quoted-identifier case collapsed: %q", k1)
	}

	// String-literal whitespace is significant: `'a b'` != `'ab'`.
	if k1, k2 := key(`SELECT * FROM T WHERE x = 'a b'`), key(`SELECT * FROM T WHERE x = 'ab'`); k1 == k2 {
		t.Fatalf("string-literal whitespace collapsed: %q", k1)
	}

	// Sanity: equivalent spellings (case, whitespace, comment) STILL share —
	// the fix must not over-partition.
	base := key("SELECT AB FROM T")
	for _, v := range []string{"select ab from t", "SELECT   AB   FROM   T", "SELECT AB FROM T -- c"} {
		if key(v) != base {
			t.Fatalf("equivalent spelling %q did not share the base key", v)
		}
	}
}

// TestPlanCacheKey_SchemaScoped pins the fix for the unscoped key (item 3c
// bug 2): SetSchema mutates only the session schema, never the cache, so the
// same SQL text resolving against a different schema/table set must key
// differently. Same for a metadata-version bump.
func TestPlanCacheKey_SchemaScoped(t *testing.T) {
	t.Parallel()

	sql := "SELECT id FROM orders"
	keyFor := func(schema string, version int) string {
		return normalizeSQL(planCacheKeyInput(schema, version, parseQuery(t, sql)))
	}

	if keyFor("SCHEMA_A", 0) == keyFor("SCHEMA_B", 0) {
		t.Fatal("same SQL under different schemas shares a cache key — a SET SCHEMA staleness bug")
	}
	if keyFor("SCHEMA_A", 1) == keyFor("SCHEMA_A", 2) {
		t.Fatal("same SQL under different metadata versions shares a cache key — a schema-evolution staleness bug")
	}
	// The scope delimiter must not let a schema name bleed into the query text:
	// schema "A" + query "B..." must not equal schema "" + query "AB...".
	if keyFor("A", 0) == normalizeSQL(planCacheKeyInput("", 0, parseQuery(t, sql))) {
		t.Fatal("schema scope bled into query text")
	}
}
