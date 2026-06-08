package recordlayer

import (
	"fmt"
	"sync"
	"testing"
)

func TestSharedNodeCache_GetPutInvalidate(t *testing.T) {
	t.Parallel()
	c := newSharedNodeCache(0) // unbounded
	n := &parsedNode{vecBytes: []byte{1, 2, 3}}

	if _, ok := c.get("k"); ok {
		t.Fatal("empty cache should miss")
	}
	c.put("k", n)
	got, ok := c.get("k")
	if !ok || got != n {
		t.Fatalf("get after put: ok=%v got=%v", ok, got)
	}
	c.invalidate("k")
	if _, ok := c.get("k"); ok {
		t.Fatal("get after invalidate should miss")
	}
}

func TestSharedNodeCache_Eviction(t *testing.T) {
	t.Parallel()
	const max = 1000
	c := newSharedNodeCache(max)
	for i := 0; i < max*3; i++ {
		c.put(fmt.Sprintf("k%d", i), &parsedNode{})
	}
	// Bounded: never exceeds the cap (eviction fires before insert when full).
	if c.len() > max {
		t.Errorf("cache len %d exceeds max %d", c.len(), max)
	}
	if c.len() == 0 {
		t.Error("cache should retain recent entries")
	}
}

func TestSharedNodeCache_Concurrent(t *testing.T) {
	t.Parallel()
	c := newSharedNodeCache(5000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				k := fmt.Sprintf("g%d-k%d", g, i%200)
				switch i % 3 {
				case 0:
					c.put(k, &parsedNode{})
				case 1:
					c.get(k)
				default:
					c.invalidate(k)
				}
			}
		}(g)
	}
	wg.Wait() // -race must not flag any data race; bounded throughout
	if c.len() > 5000 {
		t.Errorf("len %d exceeds cap", c.len())
	}
}

func TestSharedNodeCache_Registry(t *testing.T) {
	t.Parallel()
	a1 := getSharedNodeCache("idx-A", 100)
	a2 := getSharedNodeCache("idx-A", 100)
	b := getSharedNodeCache("idx-B", 100)
	if a1 != a2 {
		t.Error("same subspace key must return the same cache instance")
	}
	if a1 == b {
		t.Error("different subspace keys must return different caches")
	}
}
