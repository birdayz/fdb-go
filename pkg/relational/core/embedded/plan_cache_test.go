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

func BenchmarkPlanCache_Miss(b *testing.B) {
	c := NewPlanCache(256)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get("SELECT nonexistent_query_99999")
	}
}
