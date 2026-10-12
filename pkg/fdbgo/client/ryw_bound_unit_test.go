package client

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
)

func materializedRYWBoundary(key []byte, after bool) []byte {
	out := bytes.Clone(key)
	if after {
		out = append(out, 0)
	}
	return out
}

func checkRYWBoundaryComparison(t *testing.T, a, b []byte, afterA, afterB bool) {
	t.Helper()
	got := (rywBoundary{key: a, after: afterA}).compare(rywBoundary{key: b, after: afterB})
	want := bytes.Compare(materializedRYWBoundary(a, afterA), materializedRYWBoundary(b, afterB))
	if got != want {
		t.Fatalf("compare(%x +zero=%t, %x +zero=%t)=%d, want %d", a, afterA, b, afterB, got, want)
	}
}

func TestRYWBoundaryComparison(t *testing.T) {
	t.Parallel()
	bases, level := [][]byte{nil}, [][]byte{nil}
	for range 3 {
		var next [][]byte
		for _, prefix := range level {
			for _, v := range []byte{0, 1, 0x7f, 0x80, 0xff} {
				next = append(next, append(bytes.Clone(prefix), v))
			}
		}
		bases, level = append(bases, next...), next
	}
	spare := make([]byte, 1, 16)
	copy(spare[:cap(spare)], bytes.Repeat([]byte{0xee}, cap(spare)))
	spare[0] = 'a'
	bases = append(bases, []byte{}, spare, bytes.Repeat([]byte{0}, 10_000), bytes.Repeat([]byte{0xff}, 10_000))
	type value struct {
		bound rywBoundary
		bytes []byte
	}
	var values []value
	for _, key := range bases {
		for _, after := range []bool{false, true} {
			values = append(values, value{rywBoundary{key: key, after: after}, materializedRYWBoundary(key, after)})
		}
	}
	for _, a := range values {
		for _, b := range values {
			if got, want := a.bound.compare(b.bound), bytes.Compare(a.bytes, b.bytes); got != want {
				t.Fatalf("compare(%x +zero=%t, %x +zero=%t)=%d, want %d", a.bound.key, a.bound.after, b.bound.key, b.bound.after, got, want)
			}
		}
	}
}

func TestRYWBoundaryComparisonAllocations(t *testing.T) {
	a, b := bytes.Repeat([]byte{0}, 10_000), bytes.Repeat([]byte{0}, 10_001)
	var got int
	n := testing.AllocsPerRun(100, func() {
		got = (rywBoundary{key: a, after: true}).compare(rywBoundary{key: b})
	})
	if n != 0 || got != 0 {
		t.Fatalf("virtual comparison: allocations=%v, result=%d; want both zero", n, got)
	}
}

func FuzzRYWBoundaryComparison(f *testing.F) {
	for _, pair := range [][2][]byte{
		{nil, {}},
		{{}, {0}},
		{[]byte("a"), []byte("a\x00")},
		{[]byte("a\x00"), []byte("a\x00\x00")},
		{{0xff}, {0xff, 0}},
		{{0, 0, 1}, {0, 0}},
	} {
		for _, afterA := range []bool{false, true} {
			for _, afterB := range []bool{false, true} {
				f.Add(pair[0], pair[1], afterA, afterB)
			}
		}
	}
	f.Fuzz(checkRYWBoundaryComparison)
}

func materializedRYWBounds(c *rywCache, hi []byte, includeWrites bool) [][]byte {
	bounds := [][]byte{allKeysBegin, hi}
	addKey := func(key []byte) {
		bounds = append(bounds, key, append(append([]byte{}, key...), 0))
	}
	if includeWrites {
		for key := range c.writes {
			addKey([]byte(key))
		}
		for _, ranges := range [][]rywRange{c.cleared, c.unreadableRanges} {
			for _, r := range ranges {
				bounds = append(bounds, r.begin, r.end)
			}
		}
	}
	for _, entry := range c.serverCache.entries {
		bounds = append(bounds, entry.begin, entry.end)
		for _, kv := range entry.kvs {
			addKey(kv.Key)
		}
	}
	bounds = slices.DeleteFunc(bounds, func(b []byte) bool { return bytes.Compare(b, hi) > 0 })
	slices.SortFunc(bounds, bytes.Compare)
	return bounds
}

func TestRYWBoundariesMatchMaterializedKeys(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"empty", "cache", "writes", "mixed", "unreadable-head"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			var c rywCache
			if shape == "cache" || shape == "mixed" {
				c.serverCache.insert([]byte("a"), []byte("b"), []KeyValue{
					{Key: []byte("a"), Value: []byte{}},
					{Key: []byte("a\x00"), Value: []byte("v")},
					{Key: []byte("a\x00\x00"), Value: []byte("v")},
				})
				for _, r := range [][2]string{{"b", "c"}, {"c", "d"}, {"d", "e"}, {"e", "f"}} {
					c.serverCache.insert([]byte(r[0]), []byte(r[1]), nil)
				}
				c.serverCache.insert([]byte("f"), []byte("h"), []KeyValue{{Key: []byte("g"), Value: []byte("v")}})
			}
			if shape == "writes" || shape == "mixed" {
				for _, key := range []string{"a", "a\x00", "b", "b\x00", "f", "\xff"} {
					c.set([]byte(key), []byte("v"))
				}
				c.clearRange([]byte("b"), []byte("c"))
				c.atomic(MutAddValue, []byte("d"), []byte{1})
				c.atomic(MutCompareAndClear, []byte("f"), []byte("v"))
				c.atomic(MutSetVersionstampedValue, []byte("h"), make([]byte, 14))
			}
			if shape == "unreadable-head" || shape == "mixed" {
				c.addUnreadableRange([]byte("m"), []byte("q"))
				c.set([]byte("m\x00\x00"), []byte("v"))
				c.addUnreadableRange([]byte("s"), []byte("w"))
				c.clearRange([]byte("s"), []byte("t"))
			}
			for _, includeWrites := range []bool{false, true} {
				for _, hi := range [][]byte{{}, []byte("b\x00"), {0xff}, {0xff, 0xff}} {
					bounds := materializedRYWBounds(&c, hi, includeWrites)
					probes := [][]byte{nil, {}, {0}, {0, 0}, []byte("a\x00\x01"), []byte("bb"), []byte("n"), []byte("u"), {0xff, 0}, {0xff, 0xff}}
					for _, b := range bounds {
						probes = append(probes, b, append(append([]byte{}, b...), 0))
					}
					for _, p := range probes {
						wantFloor, wantCeil, wantPrev := allKeysBegin, hi, allKeysBegin
						for _, b := range bounds {
							if bytes.Compare(b, p) <= 0 && bytes.Compare(b, wantFloor) > 0 {
								wantFloor = b
							}
							if bytes.Compare(b, p) > 0 && bytes.Compare(b, wantCeil) < 0 {
								wantCeil = b
							}
							if bytes.Compare(b, p) < 0 && bytes.Compare(b, wantPrev) > 0 {
								wantPrev = b
							}
						}
						floor, ceil := c.mergedBoundsLocked(p, hi, includeWrites)
						prev := c.prevBoundaryLocked(p, hi, includeWrites)
						if !bytes.Equal(floor, wantFloor) || !bytes.Equal(ceil, wantCeil) || !bytes.Equal(prev, wantPrev) {
							t.Fatalf("writes=%t p=%x hi=%x: bounds=(%x,%x), prev=%x; want (%x,%x), prev=%x", includeWrites, p, hi, floor, ceil, prev, wantFloor, wantCeil, wantPrev)
						}
					}
				}
			}
			if shape == "unreadable-head" {
				for _, p := range [][]byte{[]byte("m\x00"), []byte("u")} {
					cur := c.newSegCursor([]byte{0xff}, true)
					cur.seek(p)
					if !cur.valid() || cur.typ != segUnreadable {
						t.Fatalf("unreadable span without a write entry at %x classified as %v", p, cur.typ)
					}
				}
			}
		})
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestRYWBoundaryAllocations(t *testing.T) {
	var c rywCache
	c.serverCache.insert([]byte("a"), []byte("b"), []KeyValue{
		{Key: []byte("a"), Value: []byte("v")},
		{Key: []byte("a\x00"), Value: []byte("v")},
	})
	hi := []byte{0xff}
	for _, tc := range []struct {
		name, probe, floor, ceil string
		want                     float64
	}{
		{"raw", "record/" + string(bytes.Repeat([]byte{'p'}, 64)), "b", "\xff", 0},
		{"one-successor", "a", "a", "a\x00", 1},
		{"two-successors", "a\x00", "a\x00", "a\x00\x00", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := []byte(tc.probe)
			var floor, ceil []byte
			n := testing.AllocsPerRun(100, func() { floor, ceil = c.mergedBoundsLocked(p, hi, true) })
			if string(floor) != tc.floor || string(ceil) != tc.ceil {
				t.Fatalf("bounds=(%x,%x), want (%x,%x)", floor, ceil, tc.floor, tc.ceil)
			}
			if n > tc.want {
				t.Fatalf("RYW_BOUND_ALLOC: %v allocations, want <= %v", n, tc.want)
			}
			t.Logf("RYW_BOUND_ALLOC: %s = %v", tc.name, n)
		})
	}
	probe := []byte("record/" + string(bytes.Repeat([]byte{'p'}, 64)))
	var prev []byte
	if n := testing.AllocsPerRun(100, func() { prev = c.prevBoundaryLocked(probe, hi, true) }); n != 0 {
		t.Fatalf("RYW_BOUND_ALLOC: predecessor allocated %v times, want 0", n)
	}
	if string(prev) != "b" {
		t.Fatalf("predecessor=%x, want b", prev)
	}
}

func TestRYWBoundarySuccessorsOwnTheirStorage(t *testing.T) {
	t.Parallel()
	key := make([]byte, 1, 16)
	copy(key[:cap(key)], bytes.Repeat([]byte{0xee}, cap(key)))
	key[0] = 'a'
	c := &rywCache{serverCache: snapshotCache{entries: []cacheEntry{{
		begin: []byte("a"), end: []byte("b"), kvs: []KeyValue{{Key: key, Value: []byte("v")}},
	}}}}
	_, first := c.mergedBoundsLocked(key, []byte{0xff}, true)
	if string(first) != "a\x00" || cap(first) != len(first) || key[:cap(key)][1] != 0xee {
		t.Fatalf("successor did not use independent exact-capacity storage: %x, cap=%d", first, cap(first))
	}
	for i := range 20 {
		_, next := c.mergedBoundsLocked(key, []byte{0xff}, true)
		next[0] = byte(i)
		_ = c.prevBoundaryLocked([]byte(fmt.Sprintf("z%d", i)), []byte{0xff}, true)
	}
	if string(first) != "a\x00" || key[0] != 'a' {
		t.Fatal("later boundary selection reused an earlier successor or cache key")
	}
}
