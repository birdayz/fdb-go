package embedded

import (
	"sync"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

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
	h := QueryHash(sql)

	c.Put(h, sql, plan, nil)

	got, subs, ok := c.Get(h)
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

	got, subs, ok := c.Get(12345)
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

	// Fill cache to capacity.
	for i := uint64(1); i <= 3; i++ {
		c.Put(i, "sql", &stubPlan{label: "plan"}, nil)
	}

	// All three should be present.
	for i := uint64(1); i <= 3; i++ {
		if _, _, ok := c.Get(i); !ok {
			t.Fatalf("expected hash %d to be cached", i)
		}
	}

	// Access hash 1 to promote it — makes hash 2 the LRU.
	c.Get(1)

	// Insert a 4th entry — hash 2 (LRU) should be evicted.
	c.Put(4, "sql", &stubPlan{label: "plan4"}, nil)

	// Hash 2 should be evicted (it was LRU after the Get(1) promotion).
	if _, _, ok := c.Get(2); ok {
		t.Fatal("expected hash 2 to be evicted")
	}

	// Hash 1 should still be present (was promoted by Get).
	if _, _, ok := c.Get(1); !ok {
		t.Fatal("expected hash 1 to still be cached")
	}

	// Hash 3 and 4 should be present.
	if _, _, ok := c.Get(3); !ok {
		t.Fatal("expected hash 3 to still be cached")
	}
	if _, _, ok := c.Get(4); !ok {
		t.Fatal("expected hash 4 to still be cached")
	}
}

func TestPlanCache_LRUEviction_Simple(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(2)

	c.Put(1, "a", &stubPlan{label: "a"}, nil)
	c.Put(2, "b", &stubPlan{label: "b"}, nil)

	// Cache is full. Insert 3 → evicts 1.
	c.Put(3, "c", &stubPlan{label: "c"}, nil)

	if _, _, ok := c.Get(1); ok {
		t.Fatal("expected hash 1 to be evicted")
	}
	if _, _, ok := c.Get(2); !ok {
		t.Fatal("expected hash 2 to still be cached")
	}
	if _, _, ok := c.Get(3); !ok {
		t.Fatal("expected hash 3 to still be cached")
	}
}

func TestPlanCache_Invalidate(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	c.Put(1, "a", &stubPlan{label: "a"}, nil)
	c.Put(2, "b", &stubPlan{label: "b"}, nil)

	c.Invalidate()

	if _, _, ok := c.Get(1); ok {
		t.Fatal("expected hash 1 to be gone after invalidate")
	}
	if _, _, ok := c.Get(2); ok {
		t.Fatal("expected hash 2 to be gone after invalidate")
	}

	// Cache should be usable after invalidation.
	c.Put(3, "c", &stubPlan{label: "c"}, nil)
	if _, _, ok := c.Get(3); !ok {
		t.Fatal("expected hash 3 to be cached after re-use")
	}
}

func TestPlanCache_Stats(t *testing.T) {
	t.Parallel()
	c := NewPlanCache(16)

	c.Put(1, "a", &stubPlan{label: "a"}, nil)

	// 2 hits
	c.Get(1)
	c.Get(1)

	// 3 misses
	c.Get(2)
	c.Get(3)
	c.Get(4)

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

	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				h := uint64(g*opsPerGoroutine + i)
				c.Put(h, "sql", &stubPlan{label: "p"}, nil)
				c.Get(h)
			}
		}()
	}

	wg.Wait()

	// Verify stats are consistent (no panics, no data races).
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

	c.Put(1, "sql", plan1, nil)
	c.Put(1, "sql", plan2, nil)

	got, _, ok := c.Get(1)
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

	c.Put(1, "sql", plan, subs)

	gotPlan, gotSubs, ok := c.Get(1)
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

func BenchmarkPlanCache_Hit(b *testing.B) {
	c := NewPlanCache(256)
	plan := &stubPlan{label: "select-all"}
	sql := "SELECT * FROM Item WHERE item_id = 42"
	h := QueryHash(sql)
	c.Put(h, sql, plan, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(h)
	}
}

func BenchmarkPlanCache_Miss(b *testing.B) {
	c := NewPlanCache(256)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(99999)
	}
}
