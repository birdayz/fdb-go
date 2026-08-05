package embedded

import (
	"strconv"
	"strings"
	"testing"
)

// TestPlanCacheScope_ArbitraryBytesInjective pins that planCacheScope is
// injective over ARBITRARY component bytes, not merely over the components a
// reader believes are reachable today.
//
// The scope was built by joining (schema, version, plannerOpts) with a single
// delimiter byte. That join is ambiguous the moment any component can contain
// the delimiter, and the readable-index section of cacheKeyPart carries index
// names verbatim — index names are quotable in DDL and are not restricted to
// identifier characters. The concrete collision:
//
//	planCacheScope("", "S", 0, "i2:\x010") == planCacheScope("", "S\x010\x01i2:", 0, "")
//
// A scope collision serves one schema/readable-index-set's compiled plan for a
// DIFFERENT one. The readable-index view is in this key precisely so a plan
// built while an index was readable is not served after that index goes
// WRITE_ONLY, so a collision reinstates exactly that wrong-plan bug.
func TestPlanCacheScope_ArbitraryBytesInjective(t *testing.T) {
	t.Parallel()

	type comps struct {
		dbPath  string
		schema  string
		version int
		opts    string
	}
	// The exact reported pair first, then a spread of tuples whose components
	// contain the delimiter, other control bytes, and the digits/markers the
	// encoding itself emits — the shapes that defeat a delimiter join. The
	// dbPath column carries the same shapes: it is a user-supplied URI, so it
	// is no more restricted than an index name.
	cases := []comps{
		{"", "S", 0, "i2:\x010"},
		{"", "S\x010\x01i2:", 0, ""},
		{"", "S", 0, ""},
		{"", "S", 0, "rd"},
		{"", "", 0, ""},
		{"", "\x01", 0, ""},
		{"", "A\x011", 2, ""},
		{"", "A", 1, "2"},
		{"", "A\x011\x012", 0, ""},
		{"", "S\x01", 0, "0"},
		{"", "S", 10, "i3:a\x01b"},
		{"", "S", 10, "i3:a\x00b"},
		{"", "S\x00", 10, "i3:ab"},
		{"", "i2:", 0, "S"},
		{"", "S", 0, "i2:"},
		{"", "S", 100, ""},
		{"", "S", 1, "00"},
		{"", "S1", 0, "0"},
		// Two tenants whose schemas share a name — the multi-tenant shape.
		{"/tenant_a", "MAIN", 0, ""},
		{"/tenant_b", "MAIN", 0, ""},
		// A dbPath that could absorb the following component under a
		// delimiter join, in both directions.
		{"/db\x014", "MAIN", 0, ""},
		{"/db", "\x014MAIN", 0, ""},
		{"\x01", "", 0, ""},
		// An EMPTY component must still be emitted, as a zero-length one.
		// Dropping it on emptiness would make the component COUNT depend on
		// component CONTENT, and what that collides is a pair differing ONLY
		// in which of two components holds the value. The {"", "\x01", ...} /
		// {"\x01", "", ...} pair above already reaches it, but it reads as a
		// probe of delimiter bytes inside components; these spell the same
		// property with an ordinary string, so the emptiness rule keeps a
		// sentinel that names it. Partners {"", "S", 0, ""} and
		// {"", "S", 0, "rd"} are already above — the table rejects any repeat
		// scope, so neither may be restated here.
		{"S", "", 0, ""},
		{"", "", 0, "S"},
	}

	seen := map[string]comps{}
	for _, c := range cases {
		got := planCacheScope(c.dbPath, c.schema, c.version, c.opts)
		if prev, dup := seen[got]; dup {
			t.Fatalf("plan-cache scope collision: %+v and %+v both render %q\n"+
				"two distinct (database path, schema, metadata version, planner options) tuples share a cache entry",
				prev, c, got)
		}
		seen[got] = c
	}
}

// TestPlanCacheScope_Deterministic pins the other direction of the encoding
// change: EQUAL component tuples must still render EQUAL scopes. An encoding
// that is injective but not deterministic would never hit the plan cache — a
// silent 100% miss rate, which no correctness test would notice.
func TestPlanCacheScope_Deterministic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		schema  string
		version int
		opts    string
	}{
		{"S", 0, ""},
		{"S", 7, "rd"},
		{"S\x010", 0, "i2:\x010"},
		{"", 0, ""},
		{"schema with spaces", 42, "i5:INDEX"},
	}
	for _, c := range cases {
		a := planCacheScope("", c.schema, c.version, c.opts)
		for i := 0; i < 4; i++ {
			b := planCacheScope("", c.schema, c.version, c.opts)
			if a != b {
				t.Fatalf("planCacheScope is not deterministic for %+v: %q vs %q", c, a, b)
			}
		}
		// Same components rebuilt from independent copies must also agree —
		// identity of the input strings must not leak into the key.
		schemaCopy := string([]byte(c.schema))
		optsCopy := string([]byte(c.opts))
		if b := planCacheScope("", schemaCopy, c.version, optsCopy); a != b {
			t.Fatalf("planCacheScope differs for equal-valued copies of %+v: %q vs %q", c, a, b)
		}
	}
}

// TestPlanCacheScope_SizeEstimateExact pins the cost claim in planCacheScope's
// doc. The scope is built on every plan-cache lookup, so the builder is
// pre-sized to the exact output length and allocates once; an estimate that
// undershoots makes the builder grow and allocate twice, silently doubling the
// cost of every lookup, and one that overshoots wastes the difference.
//
// The assertion is on the size FUNCTION rather than on a measured allocation
// count: every test in this package is parallel, and both testing.AllocsPerRun
// (which panics under t.Parallel) and testing.Benchmark (which reads
// process-wide MemStats, so concurrent tests inflate it) would be wrong or
// flaky here. The exactness of the estimate is the property that produces the
// single allocation, and it is deterministic.
func TestPlanCacheScope_SizeEstimateExact(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"",
		"S",
		"SCHEMA_A",
		"\x01\x00",
		// Boundaries where the decimal length prefix gains a digit.
		strings.Repeat("x", 9),
		strings.Repeat("x", 10),
		strings.Repeat("x", 99),
		strings.Repeat("x", 100),
		strings.Repeat("x", 1000),
	} {
		var b strings.Builder
		writeLengthPrefixed(&b, s)
		if got, want := lengthPrefixedSize(s), b.Len(); got != want {
			t.Errorf("lengthPrefixedSize(%d-byte component) = %d, but writeLengthPrefixed appended %d "+
				"bytes — the scope builder would grow (or over-allocate) on every plan-cache lookup",
				len(s), got, want)
		}
	}

	// And end to end: the pre-sized total must equal the rendered scope.
	for _, tc := range []struct {
		dbPath  string
		schema  string
		version int
		opts    string
	}{
		{"", "SCHEMA_A", 17, ""},
		{"/DB", "SCHEMA_A", 17, ""},
		{"", "", 0, ""},
		{strings.Repeat("d", 40), strings.Repeat("x", 250), 1234, strings.Repeat("y", 12)},
	} {
		version := strconv.Itoa(tc.version)
		want := lengthPrefixedSize(tc.dbPath) + lengthPrefixedSize(tc.schema) +
			lengthPrefixedSize(version) + lengthPrefixedSize(tc.opts)
		if got := len(planCacheScope(tc.dbPath, tc.schema, tc.version, tc.opts)); got != want {
			t.Errorf("planCacheScope(%d-byte dbPath, %d-byte schema, %d, %d-byte opts) rendered %d bytes, pre-sized for %d",
				len(tc.dbPath), len(tc.schema), tc.version, len(tc.opts), got, want)
		}
	}
}

// scopeSink keeps the allocation probe's result live.
var scopeSink string

// BenchmarkPlanCacheScope measures the encoding on the plan-cache lookup path.
// Kept because it is what established that length-prefixing costs one
// allocation of the same size the ambiguous delimiter join used.
func BenchmarkPlanCacheScope(b *testing.B) {
	for i := 0; i < b.N; i++ {
		scopeSink = planCacheScope("", "SCHEMA_A", 17, "")
	}
}

// FuzzPlanCacheScope_Injective is the property form of the table above:
// distinct component triples must never render the same scope, and equal
// triples must always render the same scope, for arbitrary component bytes.
//
// The fuzzer explores both directions at once by keeping a map from rendered
// scope to the triple that produced it: a repeat scope from a different triple
// is a collision, and a differing scope from the same triple is
// non-determinism.
func FuzzPlanCacheScope_Injective(f *testing.F) {
	f.Add("", "S", 0, "i2:\x010")
	f.Add("", "S\x010\x01i2:", 0, "")
	f.Add("", "", 0, "")
	f.Add("", "A\x011", 2, "")
	f.Add("", "\x00\x01", 10, "rd\x01")
	f.Add("/tenant_a", "MAIN", 0, "")
	f.Add("/db\x014", "MAIN", 0, "")

	type tuple4 struct {
		dbPath  string
		schema  string
		version int
		opts    string
	}
	seen := map[string]tuple4{}
	f.Fuzz(func(t *testing.T, dbPath string, schema string, version int, opts string) {
		// The version component is an int rendered by the encoder; keep it in a
		// range the fuzzer can revisit so collisions are actually explored.
		version %= 1000
		if version < 0 {
			version = -version
		}
		cur := tuple4{dbPath, schema, version, opts}
		got := planCacheScope(dbPath, schema, version, opts)
		if prev, ok := seen[got]; ok && prev != cur {
			t.Fatalf("plan-cache scope collision: %+v and %+v both render %q", prev, cur, got)
		}
		seen[got] = cur
		if again := planCacheScope(dbPath, schema, version, opts); again != got {
			t.Fatalf("planCacheScope not deterministic for %+v: %q vs %q", cur, got, again)
		}
		// The rendered scope must carry every component byte-for-byte: an
		// encoding that dropped or truncated a component would be neither
		// injective nor useful, and a length-check here catches a lossy
		// encoding the collision map might not reach.
		if len(got) < len(dbPath)+len(schema)+len(opts)+len(strconv.Itoa(version)) {
			t.Fatalf("scope %q is shorter than its components %+v (lossy encoding)", got, cur)
		}
	})
}
