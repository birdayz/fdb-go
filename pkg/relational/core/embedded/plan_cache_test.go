package embedded

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// checkInvariants asserts the internal consistency of the cache: the
// doubly-linked list and the lookup map must hold exactly the same keys, the
// map must point at the matching list element, no key may appear twice in the
// list, and the size must never exceed maxSize. White-box (same package).
func checkInvariants(t *testing.T, c *PlanCache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ll.Len() != len(c.items) {
		t.Fatalf("invariant: list len %d != map len %d", c.ll.Len(), len(c.items))
	}
	if c.ll.Len() > c.maxSize {
		t.Fatalf("invariant: list len %d exceeds maxSize %d", c.ll.Len(), c.maxSize)
	}
	seen := make(map[string]bool, c.ll.Len())
	for e := c.ll.Front(); e != nil; e = e.Next() {
		it := e.Value.(*lruItem)
		if seen[it.key] {
			t.Fatalf("invariant: key %q appears twice in list", it.key)
		}
		seen[it.key] = true
		mapped, ok := c.items[it.key]
		if !ok {
			t.Fatalf("invariant: list key %q missing from map", it.key)
		}
		if mapped != e {
			t.Fatalf("invariant: map[%q] points at a different element than the list", it.key)
		}
	}
	for k := range c.items {
		if !seen[k] {
			t.Fatalf("invariant: map key %q not present in list", k)
		}
	}
}

// cacheKeys returns the current set of keys held by the cache. White-box.
func cacheKeys(c *PlanCache) map[string]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := make(map[string]struct{}, len(c.items))
	for k := range c.items {
		m[k] = struct{}{}
	}
	return m
}

// lruOracle is an independent, deliberately naive reference LRU used as the
// differential-test ground truth. order[0] is the LRU, the tail is the MRU —
// the same recency discipline the production cache must implement.
type lruOracle struct {
	order []string
	set   map[string]struct{}
	max   int
}

func newLRUOracle(max int) *lruOracle {
	return &lruOracle{set: make(map[string]struct{}), max: max}
}

func (o *lruOracle) touch(k string) {
	for i, x := range o.order {
		if x == k {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
	o.order = append(o.order, k)
}

// get reports whether k is present, promoting it on a hit (a miss never
// changes recency, matching PlanCache.Get).
func (o *lruOracle) get(k string) bool {
	if _, ok := o.set[k]; ok {
		o.touch(k)
		return true
	}
	return false
}

func (o *lruOracle) put(k string) {
	if _, ok := o.set[k]; ok {
		o.touch(k)
		return
	}
	o.set[k] = struct{}{}
	o.order = append(o.order, k)
	for len(o.order) > o.max {
		victim := o.order[0]
		o.order = o.order[1:]
		delete(o.set, victim)
	}
}

func (o *lruOracle) keys() map[string]struct{} {
	m := make(map[string]struct{}, len(o.set))
	for k := range o.set {
		m[k] = struct{}{}
	}
	return m
}

func sameKeySet(t *testing.T, ctx string, got, want map[string]struct{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: key-set size mismatch: got %d want %d (got=%v want=%v)", ctx, len(got), len(want), got, want)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s: cache missing key %q present in oracle (got=%v want=%v)", ctx, k, got, want)
		}
	}
}

// stubPlan is a minimal RecordQueryPlan for cache testing.
type stubPlan struct {
	label string
}

func (s *stubPlan) Explain() string                                        { return "stub:" + s.label }
func (s *stubPlan) GetResultType() values.Type                             { return nil }
func (s *stubPlan) GetChildren() []plans.RecordQueryPlan                   { return nil }
func (s *stubPlan) EqualsWithoutChildren(other plans.RecordQueryPlan) bool { return false }
func (s *stubPlan) HashCodeWithoutChildren() uint64                        { return 0 }

var _ plans.RecordQueryPlan = (*stubPlan)(nil)

func TestPlanCache_HitReturnsSamePlan(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	plan := &stubPlan{label: "select-all"}
	sql := "SELECT * FROM t"

	c.Put(sql, plan, nil)

	got, subs, ok := c.Get(sql)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != plan {
		t.Fatalf("got different plan object: %p vs %p", got, plan)
	}
	if subs != nil {
		t.Fatalf("expected nil subs, got %v", subs)
	}
}

func TestPlanCache_MissReturnsNil(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	got, subs, ok := c.Get("SELECT nonexistent")
	if ok {
		t.Fatal("expected cache miss")
	}
	if got != nil {
		t.Fatal("expected nil plan on miss")
	}
	if subs != nil {
		t.Fatal("expected nil subs on miss")
	}
}

func TestPlanCache_LRUEviction(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(3)

	c.Put("SQL_1", &stubPlan{label: "plan"}, nil)
	c.Put("SQL_2", &stubPlan{label: "plan"}, nil)
	c.Put("SQL_3", &stubPlan{label: "plan"}, nil)

	for _, sql := range []string{"SQL_1", "SQL_2", "SQL_3"} {
		if _, _, ok := c.Get(sql); !ok {
			t.Fatalf("expected %s to be cached", sql)
		}
	}

	// Access SQL_1 to promote it — makes SQL_2 the LRU.
	c.Get("SQL_1")

	// Insert a 4th entry — SQL_2 (LRU) should be evicted.
	c.Put("SQL_4", &stubPlan{label: "plan4"}, nil)

	if _, _, ok := c.Get("SQL_2"); ok {
		t.Fatal("expected SQL_2 to be evicted")
	}
	if _, _, ok := c.Get("SQL_1"); !ok {
		t.Fatal("expected SQL_1 to still be cached")
	}
	if _, _, ok := c.Get("SQL_3"); !ok {
		t.Fatal("expected SQL_3 to still be cached")
	}
	if _, _, ok := c.Get("SQL_4"); !ok {
		t.Fatal("expected SQL_4 to still be cached")
	}
}

func TestPlanCache_LRUEviction_Simple(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(2)

	c.Put("a", &stubPlan{label: "a"}, nil)
	c.Put("b", &stubPlan{label: "b"}, nil)

	// Cache is full. Insert c → evicts a.
	c.Put("c", &stubPlan{label: "c"}, nil)

	if _, _, ok := c.Get("a"); ok {
		t.Fatal("expected 'a' to be evicted")
	}
	if _, _, ok := c.Get("b"); !ok {
		t.Fatal("expected 'b' to still be cached")
	}
	if _, _, ok := c.Get("c"); !ok {
		t.Fatal("expected 'c' to still be cached")
	}
}

func TestPlanCache_Invalidate(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	c.Put("a", &stubPlan{label: "a"}, nil)
	c.Put("b", &stubPlan{label: "b"}, nil)

	c.Invalidate()

	if _, _, ok := c.Get("a"); ok {
		t.Fatal("expected 'a' to be gone after invalidate")
	}
	if _, _, ok := c.Get("b"); ok {
		t.Fatal("expected 'b' to be gone after invalidate")
	}

	c.Put("c", &stubPlan{label: "c"}, nil)
	if _, _, ok := c.Get("c"); !ok {
		t.Fatal("expected 'c' to be cached after re-use")
	}
}

func TestPlanCache_Stats(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	c.Put("a", &stubPlan{label: "a"}, nil)

	// 2 hits
	c.Get("a")
	c.Get("a")

	// 3 misses
	c.Get("b")
	c.Get("c")
	c.Get("d")

	hits, misses := c.Stats()
	if hits != 2 {
		t.Fatalf("expected 2 hits, got %d", hits)
	}
	if misses != 3 {
		t.Fatalf("expected 3 misses, got %d", misses)
	}
}

func TestPlanCache_ConcurrentGetPut(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(64)

	const goroutines = 16
	const opsPerGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)

	sqls := make([]string, goroutines*opsPerGoroutine)
	for i := range sqls {
		sqls[i] = "SELECT " + string(rune('A'+(i%26))) + " FROM t WHERE id = " + string(rune('0'+(i%10)))
	}

	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				sql := sqls[g*opsPerGoroutine+i]
				c.Put(sql, &stubPlan{label: "p"}, nil)
				c.Get(sql)
			}
		}()
	}

	wg.Wait()

	hits, misses := c.Stats()
	total := hits + misses
	if total != goroutines*opsPerGoroutine {
		t.Fatalf("total lookups = %d, expected %d", total, goroutines*opsPerGoroutine)
	}
}

func TestPlanCache_PutUpdatesExisting(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	plan1 := &stubPlan{label: "v1"}
	plan2 := &stubPlan{label: "v2"}

	c.Put("sql", plan1, nil)
	c.Put("sql", plan2, nil)

	got, _, ok := c.Get("sql")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != plan2 {
		t.Fatal("expected updated plan to be returned")
	}
}

func TestPlanCache_DefaultMaxSize(t *testing.T) {
	t.Parallel()

	c := NewPlanCache(0)
	if c.maxSize != 256 {
		t.Fatalf("expected default maxSize 256, got %d", c.maxSize)
	}

	c2 := NewPlanCache(-1)
	if c2.maxSize != 256 {
		t.Fatalf("expected default maxSize 256, got %d", c2.maxSize)
	}
}

func TestPlanCache_ScalarSubqueryBindings(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	plan := &stubPlan{label: "main"}
	alias := values.NamedCorrelationIdentifier("sq1")
	subs := []scalarSubqueryBinding{
		{alias: alias, plan: &stubPlan{label: "sub1"}},
	}

	c.Put("sql", plan, subs)

	gotPlan, gotSubs, ok := c.Get("sql")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if gotPlan != plan {
		t.Fatal("wrong plan returned")
	}
	if len(gotSubs) != 1 || gotSubs[0].alias.Name() != "sq1" {
		t.Fatalf("wrong subs returned: %v", gotSubs)
	}
}

// TestPlanCache_NormalizationHit verifies that SQL strings differing only
// in case, whitespace, or comments hit the same cache entry.
func TestPlanCache_NormalizationHit(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	plan := &stubPlan{label: "normalized"}
	c.Put("SELECT * FROM foo", plan, nil)

	variants := []string{
		"select * from foo",
		"SELECT  *  FROM  foo",
		"  SELECT * FROM foo  ",
		"SELECT * FROM foo -- comment",
	}
	for _, v := range variants {
		got, _, ok := c.Get(v)
		if !ok {
			t.Fatalf("expected cache hit for %q", v)
		}
		if got != plan {
			t.Fatalf("wrong plan for %q", v)
		}
	}
}

// TestPlanCache_NoHashCollision verifies that distinct SQL strings
// always return their own plans, even if they would have collided
// under the old uint64 hash scheme.
func TestPlanCache_NoHashCollision(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(256)

	planA := &stubPlan{label: "plan_A"}
	planB := &stubPlan{label: "plan_B"}

	sqlA := "SELECT a FROM tableA WHERE id = 1"
	sqlB := "SELECT b FROM tableB WHERE id = 2"

	c.Put(sqlA, planA, nil)
	c.Put(sqlB, planB, nil)

	gotA, _, okA := c.Get(sqlA)
	gotB, _, okB := c.Get(sqlB)

	if !okA || gotA != planA {
		t.Fatal("sqlA returned wrong plan")
	}
	if !okB || gotB != planB {
		t.Fatal("sqlB returned wrong plan")
	}
}

// TestPlanCache_InterleavedEvictionOrder exercises promote-on-update
// interacting with eviction. With capacity 3, an update to an existing key
// must promote it (not evict, since size is unchanged), so a subsequent
// insert evicts the genuine LRU victim rather than the just-updated key.
func TestPlanCache_InterleavedEvictionOrder(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(3)

	c.Put("a", &stubPlan{label: "a"}, nil) // order: a
	c.Put("b", &stubPlan{label: "b"}, nil) // order: a, b
	c.Put("c", &stubPlan{label: "c"}, nil) // order: a, b, c

	// Update "a" — promotes it to MRU. Size stays 3, nothing evicted.
	c.Put("a", &stubPlan{label: "a2"}, nil) // order: b, c, a

	// Now "b" is the LRU. Inserting "d" must evict "b".
	c.Put("d", &stubPlan{label: "d"}, nil) // order: c, a, d

	if _, _, ok := c.Get("b"); ok {
		t.Fatal("expected 'b' (LRU) to be evicted")
	}
	got, _, ok := c.Get("a")
	if !ok {
		t.Fatal("expected updated 'a' to still be cached")
	}
	if got.(*stubPlan).label != "a2" {
		t.Fatalf("expected promoted+updated plan 'a2', got %q", got.(*stubPlan).label)
	}
	if _, _, ok := c.Get("c"); !ok {
		t.Fatal("expected 'c' to still be cached")
	}
	if _, _, ok := c.Get("d"); !ok {
		t.Fatal("expected 'd' to still be cached")
	}
}

// TestPlanCache_DifferentialModel runs a long randomized sequence of
// Get/Put/Invalidate operations against both the production cache and an
// independent reference LRU (lruOracle), asserting after every single
// operation that (a) the two agree on membership, (b) Get hit/miss verdicts
// agree, (c) a hit returns the plan that was stored under that key, and
// (d) the cache's internal list/map invariants hold. If recency tracking ever
// diverges from a textbook LRU — a promote that doesn't promote, an eviction
// of the wrong victim — the membership sets drift and this fails immediately.
func TestPlanCache_DifferentialModel(t *testing.T) {
	t.Parallel()

	for _, maxSize := range []int{1, 2, 5, 16, 64} {
		for _, seed := range []int64{1, 7, 42, 1337} {
			maxSize, seed := maxSize, seed
			t.Run(fmt.Sprintf("max%d_seed%d", maxSize, seed), func(t *testing.T) {
				t.Parallel()
				c := NewPlanCache(maxSize)
				o := newLRUOracle(maxSize)
				rng := rand.New(rand.NewSource(seed))

				// Key space deliberately larger than maxSize so eviction churns.
				keyspace := maxSize*3 + 2

				for step := 0; step < 4000; step++ {
					k := "q" + strconv.Itoa(rng.Intn(keyspace))
					// The cache keys on normalizeSQL(k) internally; the oracle
					// must do the same. normalizeSQL is 1:1 over this key space
					// (distinct integers → distinct normalized keys), so the
					// stored stubPlan.label (raw k) is unambiguous on a hit.
					nk := normalizeSQL(k)

					switch r := rng.Intn(10); {
					case r < 5: // Put (50%)
						c.Put(k, &stubPlan{label: k}, nil)
						o.put(nk)
					case r < 9: // Get (40%)
						plan, _, gotOK := c.Get(k)
						wantOK := o.get(nk)
						if gotOK != wantOK {
							t.Fatalf("step %d key %q: cache hit=%v but oracle hit=%v", step, k, gotOK, wantOK)
						}
						if gotOK && plan.(*stubPlan).label != k {
							t.Fatalf("step %d key %q: hit returned plan for %q", step, k, plan.(*stubPlan).label)
						}
					default: // Invalidate (10%)
						c.Invalidate()
						o = newLRUOracle(maxSize)
					}

					sameKeySet(t, fmt.Sprintf("step %d", step), cacheKeys(c), o.keys())
					checkInvariants(t, c)
				}
			})
		}
	}
}

// TestPlanCache_MaxSizeOne exercises the degenerate single-slot cache: every
// distinct Put must evict the previous entry, and the invariants must hold.
func TestPlanCache_MaxSizeOne(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(1)

	c.Put("a", &stubPlan{label: "a"}, nil)
	checkInvariants(t, c)
	c.Put("b", &stubPlan{label: "b"}, nil)
	checkInvariants(t, c)

	if _, _, ok := c.Get("a"); ok {
		t.Fatal("expected 'a' evicted in a size-1 cache")
	}
	if got, _, ok := c.Get("b"); !ok || got.(*stubPlan).label != "b" {
		t.Fatal("expected 'b' resident in a size-1 cache")
	}

	// Re-Put of the resident key must not evict it (size unchanged).
	c.Put("b", &stubPlan{label: "b2"}, nil)
	checkInvariants(t, c)
	if got, _, ok := c.Get("b"); !ok || got.(*stubPlan).label != "b2" {
		t.Fatal("expected updated 'b2' resident")
	}
}

// TestPlanCache_EvictionExactBoundary verifies no eviction happens at exactly
// maxSize and exactly one happens at maxSize+1.
func TestPlanCache_EvictionExactBoundary(t *testing.T) {
	t.Parallel()
	const max = 4
	c := NewPlanCache(max)

	for i := 0; i < max; i++ {
		c.Put("k"+strconv.Itoa(i), &stubPlan{label: "p"}, nil)
	}
	checkInvariants(t, c)
	if got := len(cacheKeys(c)); got != max {
		t.Fatalf("at capacity: expected %d entries, got %d", max, got)
	}
	// All still present — nothing evicted at exactly maxSize.
	for i := 0; i < max; i++ {
		if _, _, ok := c.Get("k" + strconv.Itoa(i)); !ok {
			t.Fatalf("k%d evicted before exceeding capacity", i)
		}
	}

	// k0 is now LRU (the Get loop above promoted them in order k0..k3, so k0
	// is oldest). One more insert evicts exactly k0.
	c.Put("k4", &stubPlan{label: "p"}, nil)
	checkInvariants(t, c)
	if got := len(cacheKeys(c)); got != max {
		t.Fatalf("after overflow: expected %d entries, got %d", max, got)
	}
	if _, _, ok := c.Get("k0"); ok {
		t.Fatal("expected k0 (LRU) evicted")
	}
}

// TestPlanCache_NilPlanStored documents that a hit on a key whose stored plan
// is nil still reports ok=true (the cache distinguishes presence from value;
// it never inspects the plan).
func TestPlanCache_NilPlanStored(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(4)
	c.Put("k", nil, nil)
	got, _, ok := c.Get("k")
	if !ok {
		t.Fatal("expected hit for a present key with a nil plan")
	}
	if got != nil {
		t.Fatal("expected the stored nil plan back")
	}
	checkInvariants(t, c)
}

// TestPlanCache_UpdateReplacesSubs verifies that re-Putting a key swaps both
// the plan and the scalar-subquery bindings.
func TestPlanCache_UpdateReplacesSubs(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(4)

	subs1 := []scalarSubqueryBinding{{alias: values.NamedCorrelationIdentifier("a")}}
	subs2 := []scalarSubqueryBinding{
		{alias: values.NamedCorrelationIdentifier("b")},
		{alias: values.NamedCorrelationIdentifier("c")},
	}
	c.Put("sql", &stubPlan{label: "v1"}, subs1)
	c.Put("sql", &stubPlan{label: "v2"}, subs2)

	plan, gotSubs, ok := c.Get("sql")
	if !ok || plan.(*stubPlan).label != "v2" {
		t.Fatal("expected updated plan v2")
	}
	if len(gotSubs) != 2 || gotSubs[0].alias.Name() != "b" || gotSubs[1].alias.Name() != "c" {
		t.Fatalf("expected updated subs [b c], got %v", gotSubs)
	}
	checkInvariants(t, c)
}

// TestPlanCache_RaceSameKey hammers a small set of keys with concurrent Put and
// Get from many goroutines, and — critically — READS the values returned by Get
// (plan.Explain(), each binding's alias) AFTER the cache lock is released, while
// other goroutines concurrently update those same keys. This is the data-race
// test for the central correctness claim: PlanCache.Put never mutates a stored
// *planCacheEntry in place (it swaps in a fresh one), so a pointer captured
// under the lock and read after release is immutable and race-free. Run under
// `go test -race`; in-place mutation would trip the detector here.
func TestPlanCache_RaceSameKey(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(8) // smaller than the key set → eviction churn under contention

	keys := make([]string, 12)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}

	const goroutines = 32
	const ops = 4000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g) + 1))
			for i := 0; i < ops; i++ {
				k := keys[rng.Intn(len(keys))]
				if rng.Intn(2) == 0 {
					c.Put(k, &stubPlan{label: k}, []scalarSubqueryBinding{
						{alias: values.NamedCorrelationIdentifier(k)},
					})
				} else {
					plan, subs, ok := c.Get(k)
					if ok {
						// Force reads of the returned, post-unlock values.
						_ = plan.Explain()
						for _, s := range subs {
							_ = s.alias.Name()
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	checkInvariants(t, c)
}

// TestPlanCache_RaceInvalidate runs concurrent Get/Put/Stats workers against a
// stream of Invalidate calls. Invalidate replaces both the list and the map; a
// reader that captured an entry pointer before the Invalidate must still be
// able to read it safely. Run under `go test -race`.
func TestPlanCache_RaceInvalidate(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	const workers = 24
	const invalidators = 4
	const ops = 6000

	var wg sync.WaitGroup
	wg.Add(workers + invalidators)

	for g := 0; g < workers; g++ {
		g := g
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g) + 100))
			for i := 0; i < ops; i++ {
				k := "k" + strconv.Itoa(rng.Intn(40))
				switch rng.Intn(3) {
				case 0:
					c.Put(k, &stubPlan{label: k}, nil)
				case 1:
					if plan, _, ok := c.Get(k); ok {
						_ = plan.Explain()
					}
				case 2:
					c.Stats()
				}
			}
		}()
	}
	for g := 0; g < invalidators; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < ops/20; i++ {
				c.Invalidate()
			}
		}()
	}
	wg.Wait()
	checkInvariants(t, c)
}

func BenchmarkPlanCache_Hit(b *testing.B) {
	c := NewPlanCache(256)
	plan := &stubPlan{label: "select-all"}
	sql := "SELECT * FROM Item WHERE item_id = 42"
	c.Put(sql, plan, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(sql)
	}
}

// BenchmarkPlanCache_HitLargeCache measures repeated hits against a single key
// in a large (1024-entry) cache. The map lookup + MoveToBack is O(1) regardless
// of cache size, so this should match BenchmarkPlanCache_Hit (256 entries, one
// key). The old slice-based promote() linear-scanned to find the key, so its
// per-hit cost scaled with the number of entries — this benchmark would have
// been markedly slower under it. (Note: after the first iteration the key is
// MRU, so this measures the steady-state hot-key path, not a per-iteration
// worst-case scan position.)
func BenchmarkPlanCache_HitLargeCache(b *testing.B) {
	const size = 1024
	c := NewPlanCache(size)
	for i := 0; i < size; i++ {
		c.Put("SELECT * FROM t WHERE id = "+strconv.Itoa(i), &stubPlan{label: "p"}, nil)
	}
	sql := "SELECT * FROM t WHERE id = " + strconv.Itoa(size/2)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(sql)
	}
}

func BenchmarkPlanCache_Miss(b *testing.B) {
	c := NewPlanCache(256)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get("SELECT nonexistent_query_99999")
	}
}
