package client

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestRYWSortedIndexRetainedAcrossMutations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*rywCache)
		want   []string
	}{
		{"insert first", func(c *rywCache) { c.set([]byte("a"), []byte("v")) }, []string{"a", "b", "d"}},
		{"insert middle", func(c *rywCache) { c.set([]byte("c"), []byte("v")) }, []string{"b", "c", "d"}},
		{"insert last", func(c *rywCache) { c.set([]byte("e"), []byte("v")) }, []string{"b", "d", "e"}},
		{"replace", func(c *rywCache) { c.set([]byte("b"), []byte("new")) }, []string{"b", "d"}},
		{"clear existing", func(c *rywCache) { c.clear([]byte("b")) }, []string{"d"}},
		{"clear missing", func(c *rywCache) { c.clear([]byte("c")) }, []string{"b", "d"}},
		{"clear range", func(c *rywCache) { c.clearRange([]byte("b"), []byte("d")) }, []string{"d"}},
		{"clear all", func(c *rywCache) { c.clearRange([]byte("a"), []byte("z")) }, []string{}},
		{"clear empty range", func(c *rywCache) { c.clearRange([]byte("b"), []byte("b")) }, []string{"b", "d"}},
		{"atomic new", func(c *rywCache) { c.atomic(MutAddValue, []byte("c"), []byte{1}) }, []string{"b", "c", "d"}},
		{"atomic existing", func(c *rywCache) { c.atomic(MutAddValue, []byte("b"), []byte{1}) }, []string{"b", "d"}},
		{"atomic cleared", func(c *rywCache) {
			c.clear([]byte("c"))
			c.atomic(MutAddValue, []byte("c"), []byte{1})
		}, []string{"b", "c", "d"}},
		{"atomic phantom", func(c *rywCache) { c.atomic(MutCompareAndClear, []byte("b"), []byte("v")) }, []string{"b", "d"}},
		{"unreadable new", func(c *rywCache) { c.atomic(MutSetVersionstampedValue, []byte("c"), []byte("stamp")) }, []string{"b", "c", "d"}},
		{"unreadable replacement", func(c *rywCache) {
			c.atomic(MutSetVersionstampedValue, []byte("b"), []byte("stamp"))
			c.set([]byte("b"), []byte("new"))
		}, []string{"b", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var c rywCache
			c.set([]byte("b"), []byte("v"))
			c.set([]byte("d"), []byte("v"))
			c.liveLocked().ensureSorted()
			tc.mutate(&c)
			// Check before any read can rebuild an invalidated index.
			if w := c.liveLocked(); !w.isSorted || w.writes != nil || !slices.Equal(sortedStoreKeys(w), tc.want) {
				t.Fatalf("retained index = %#v (sorted %t), want %#v", sortedStoreKeys(w), w.isSorted, tc.want)
			}
		})
	}
}

// sortedStoreKeys lists the sorted store's keys without building it.
func sortedStoreKeys(w *rywWrites) []string {
	keys := []string{}
	for _, x := range w.sorted.appendTo(nil) {
		keys = append(keys, x.key)
	}
	return keys
}

func TestRYWSortedIndexTracksWrites(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	key := func() []byte { return []byte(fmt.Sprintf("k%03d", rng.IntN(200))) }
	var c rywCache
	c.liveLocked().ensureSorted()
	model := map[string]bool{} // the written keys, kept independently
	for step := 0; step < 5000; step++ {
		switch rng.IntN(5) {
		case 0, 1:
			k := key()
			c.set(k, []byte("v"))
			model[string(k)] = true
		case 2:
			k := key()
			c.clear(k)
			delete(model, string(k))
		case 3:
			a, b := key(), key()
			if string(a) > string(b) {
				a, b = b, a
			}
			c.clearRange(a, b)
			for k := range model {
				if k >= string(a) && k < string(b) {
					delete(model, k)
				}
			}
		default:
			k := key()
			c.atomic(MutAddValue, k, []byte{1, 0, 0, 0, 0, 0, 0, 0})
			model[string(k)] = true
		}
		if w := c.liveLocked(); !w.isSorted || w.writes != nil {
			t.Fatalf("step %d: mutation left the sorted store", step)
		}
		want := make([]string, 0, len(model))
		for k := range model {
			want = append(want, k)
		}
		slices.Sort(want)
		if got := sortedStoreKeys(c.liveLocked()); !slices.Equal(got, want) {
			t.Fatalf("step %d: sorted index %v, write keys %v", step, got, want)
		}
	}
}

func TestRYWSortedIndexLazyAfterReset(t *testing.T) {
	t.Parallel()
	var c rywCache
	for range 2 {
		c.set([]byte("b"), []byte("v"))
		c.atomic(MutAddValue, []byte("a"), []byte{1})
		if c.liveLocked().isSorted {
			t.Fatal("write-only transaction built a sorted index")
		}
		c.liveLocked().ensureSorted()
		if got := sortedStoreKeys(c.liveLocked()); !slices.Equal(got, []string{"a", "b"}) {
			t.Fatalf("initial index = %v", got)
		}
		c.clearRange([]byte("a"), []byte("z"))
		c.set([]byte("c"), []byte("v"))
		if got := sortedStoreKeys(c.liveLocked()); !slices.Equal(got, []string{"c"}) {
			t.Fatalf("index after clearing all and inserting = %v", got)
		}
		c.reset()
		if w := c.liveLocked(); w.isSorted || w.sorted.Len() != 0 {
			t.Fatal("reset retained the old sorted index")
		}
	}
}
