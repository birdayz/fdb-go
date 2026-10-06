package embedded

import (
	"testing"
	"time"

	"fdb.dev/pkg/relational/api"
)

// fakeClock is a settable clock for the cache's TTLs.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testPlanCache(limits planCacheLimits) (*RelationalPlanCache, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	return newRelationalPlanCache(limits, clock.now), clock
}

// TestRelationalPlanCache_Stages pins Java's MultiStageCache.reduce counting:
// a new template is a PRIMARY_MISS, a new query a SECONDARY_MISS, and the
// plan a TERTIARY_HIT or TERTIARY_MISS; a store creates no miss.
func TestRelationalPlanCache_Stages(t *testing.T) {
	t.Parallel()
	c, _ := testPlanCache(planCacheLimits{primarySize: 4, secondarySize: 4, tertiarySize: 4})
	q1 := cacheKey{scope: "s", sql: "SELECT 1"}
	q2 := cacheKey{scope: "s", sql: "SELECT 2"}
	plan := &planCacheEntry{plan: &stubPlan{label: "p"}, outputLabels: []string{"A"}}

	if _, ok := c.lookup("T", q1, ""); ok {
		t.Fatal("hit on an empty cache")
	}
	c.store("T", q1, "", plan)
	got, ok := c.lookup("T", q1, "")
	if !ok || got.plan != plan.plan || len(got.outputLabels) != 1 {
		t.Fatalf("lookup after store = %v, %v", got, ok)
	}
	got.outputLabels[0] = "mutated"
	if again, _ := c.lookup("T", q1, ""); again.outputLabels[0] != "A" {
		t.Error("a lookup's labels alias the cached entry")
	}
	if _, ok := c.lookup("T", q1, "param=1"); ok {
		t.Error("another equivalence hit")
	}
	if _, ok := c.lookup("T", q2, ""); ok {
		t.Error("another query hit")
	}
	if _, ok := c.lookup("U", q1, ""); ok {
		t.Error("another template hit")
	}
	want := PlanCacheCounts{PrimaryMiss: 2, SecondaryMiss: 3, TertiaryHit: 2, TertiaryMiss: 4}
	if got := c.Counts(); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
	if n := c.numEntries(); n != 2 {
		t.Errorf("primary entries = %d, want 2 (T and U)", n)
	}
}

// TestRelationalPlanCache_SizeBounds pins each stage's size bound and its
// LRU_EVICTION count: the tertiary stage holds 2 equivalences per query, the
// secondary 2 queries per template, the primary 2 templates.
func TestRelationalPlanCache_SizeBounds(t *testing.T) {
	t.Parallel()
	c, _ := testPlanCache(planCacheLimits{primarySize: 2, secondarySize: 2, tertiarySize: 2})
	entry := &planCacheEntry{plan: &stubPlan{}}
	q := func(s string) cacheKey { return cacheKey{scope: "s", sql: s} }

	for _, eq := range []string{"a", "b", "c"} {
		c.store("T", q("x"), eq, entry)
	}
	if _, ok := c.lookup("T", q("x"), "a"); ok {
		t.Error("the least recently used equivalence survived the tertiary bound")
	}
	for _, eq := range []string{"b", "c"} {
		if _, ok := c.lookup("T", q("x"), eq); !ok {
			t.Errorf("equivalence %s evicted", eq)
		}
	}
	c.store("T", q("y"), "", entry)
	c.store("T", q("z"), "", entry)
	if _, ok := c.lookup("T", q("x"), "b"); ok {
		t.Error("the least recently used query survived the secondary bound")
	}
	c.store("U", q("x"), "", entry)
	c.store("V", q("x"), "", entry)
	if n := c.numEntries(); n != 2 {
		t.Errorf("primary entries = %d, want 2", n)
	}
	counts := c.Counts()
	if counts.TertiaryLRUEviction == 0 || counts.SecondaryLRUEviction == 0 || counts.PrimaryLRUEviction == 0 {
		t.Errorf("evictions not counted: %+v", counts)
	}
}

// TestRelationalPlanCache_TTL pins the stages' expiry: the primary expires
// after its TTL without access, the secondary and tertiary their TTL after
// they were written, however often they are read.
func TestRelationalPlanCache_TTL(t *testing.T) {
	t.Parallel()
	c, clock := testPlanCache(planCacheLimits{
		primarySize: 4, secondarySize: 4, tertiarySize: 4,
		primaryTTL: 10 * time.Second, secondaryTTL: 30 * time.Second, tertiaryTTL: 30 * time.Second,
	})
	key := cacheKey{scope: "s", sql: "SELECT 1"}
	c.store("T", key, "", &planCacheEntry{plan: &stubPlan{}})
	for i := 0; i < 5; i++ {
		clock.t = clock.t.Add(5 * time.Second)
		if _, ok := c.lookup("T", key, ""); !ok {
			t.Fatalf("expired at %d s while read every 5 s", 5*(i+1))
		}
	}
	clock.t = clock.t.Add(5 * time.Second) // 30 s after the write
	if _, ok := c.lookup("T", key, ""); ok {
		t.Error("a plan outlived its write TTL by being read")
	}
	c.store("T", key, "", &planCacheEntry{plan: &stubPlan{}})
	clock.t = clock.t.Add(11 * time.Second)
	before := c.Counts().PrimaryMiss
	if _, ok := c.lookup("T", key, ""); ok {
		t.Error("a template survived its access TTL")
	}
	if c.Counts().PrimaryMiss != before+1 {
		t.Error("the expired template was not a primary miss")
	}
}

// TestNewRelationalPlanCache_Options pins the stage limits to the PLAN_CACHE_*
// options and their Java defaults.
func TestNewRelationalPlanCache_Options(t *testing.T) {
	t.Parallel()
	defaults := NewRelationalPlanCache(nil).limits
	want := planCacheLimits{
		primarySize: 1024, secondarySize: 256, tertiarySize: 8,
		primaryTTL: 10 * time.Second, secondaryTTL: 30 * time.Second, tertiaryTTL: 30 * time.Second,
	}
	if defaults != want {
		t.Errorf("defaults = %+v, want %+v", defaults, want)
	}
	set := NewRelationalPlanCache(api.NewOptionsBuilder().
		Set(api.OptPlanCacheTertiaryMaxEntries, 3).
		Set(api.OptPlanCacheSecondaryTimeToLiveMillis, int64(500)).Build()).limits
	if set.tertiarySize != 3 || set.secondaryTTL != 500*time.Millisecond || set.primarySize != 1024 {
		t.Errorf("options not applied: %+v", set)
	}
}
