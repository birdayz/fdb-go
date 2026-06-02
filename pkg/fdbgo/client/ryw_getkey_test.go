package client

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// mockStorage is a sorted key→value map standing in for the server at the read
// version. rangeFn returns the keys in [begin, end) (never truncated — more=false).
type mockStorage struct {
	keys map[string][]byte
	// calls counts server reads so tests can assert "resolved purely from cache".
	calls int
}

func (m *mockStorage) rangeFn(_ context.Context, begin, end []byte, _ int, _ bool) ([]KeyValue, bool, error) {
	m.calls++
	var ks []string
	for k := range m.keys {
		if k >= string(begin) && k < string(end) {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	out := make([]KeyValue, len(ks))
	for i, k := range ks {
		out[i] = KeyValue{Key: []byte(k), Value: m.keys[k]}
	}
	return out, false, nil
}

// Selector constructors (FDB semantics).
func fgt(k string) (string, bool, int32) { return k, true, 1 }  // firstGreaterThan
func fge(k string) (string, bool, int32) { return k, false, 1 } // firstGreaterOrEqual
func llt(k string) (string, bool, int32) { return k, false, 0 } // lastLessThan
func lle(k string) (string, bool, int32) { return k, true, 0 }  // lastLessOrEqual

func TestGetKeyRYW_FullyCached(t *testing.T) {
	t.Parallel()
	maxKey := []byte("\xff")

	// Seed a fully-known cache range so resolution needs NO server read; a server
	// call fails the test (proves the answer came from the merged RYW view).
	newCache := func(seed map[string][]byte, lo, hi string) *rywCache {
		c := &rywCache{}
		var ks []string
		for k := range seed {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		kvs := make([]KeyValue, len(ks))
		for i, k := range ks {
			kvs[i] = KeyValue{Key: []byte(k), Value: seed[k]}
		}
		c.serverCache.insert([]byte(lo), []byte(hi), kvs)
		return c
	}
	noServer := func(_ context.Context, _, _ []byte, _ int, _ bool) ([]KeyValue, bool, error) {
		t.Fatalf("server read must not happen — resolution should be fully cached")
		return nil, false, nil
	}

	cases := []struct {
		name    string
		seed    map[string][]byte // committed storage (known cache range [lo,hi))
		lo, hi  string
		pending func(*rywCache)
		k       string
		orEqual bool
		offset  int32
		want    string
	}{
		// pending Set fills a gap: firstGreaterThan(a) over {a,c}+Set(b) → b, not c.
		{
			"set_fills_gap_fgt_a",
			map[string][]byte{"a": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) { c.set([]byte("b"), []byte("x")) }, "a", true, 1, "b",
		},
		// firstGreaterOrEqual(b) on the pending key.
		{
			"set_fge_b",
			map[string][]byte{"a": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) { c.set([]byte("b"), []byte("x")) }, "b", false, 1, "b",
		},
		// pending Clear shifts: firstGreaterThan(a) over {a,b,c}\{b} → c.
		{
			"clear_shifts_fgt_a",
			map[string][]byte{"a": []byte("x"), "b": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) { c.clear([]byte("b")) }, "a", true, 1, "c",
		},
		// lastLessThan(c) over {a,b,c} → b (backward, offset 0).
		{
			"llt_c",
			map[string][]byte{"a": []byte("x"), "b": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) {}, "c", false, 0, "b",
		},
		// lastLessThan(c) with pending Clear(b) → a.
		{
			"llt_c_clear_b",
			map[string][]byte{"a": []byte("x"), "b": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) { c.clear([]byte("b")) }, "c", false, 0, "a",
		},
		// offset 2 forward: 2nd key >= a over {a,b,c} → b (the {orEqual,offset>1} axis).
		{
			"fge_a_offset2",
			map[string][]byte{"a": []byte("x"), "b": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) {}, "a", false, 2, "b",
		},
		// offset 2 with orEqual + a pending Set in the gap: 2nd key > a over {a,c}+Set(b)
		// → c (b is 1st > a, c is 2nd). The exact shape the merged-GetRange shortcut got wrong.
		{
			"fgt_a_offset2_pending",
			map[string][]byte{"a": []byte("x"), "c": []byte("x")},
			"a", "c\x00",
			func(c *rywCache) { c.set([]byte("b"), []byte("x")) }, "a", true, 2, "c",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCache(tc.seed, tc.lo, tc.hi)
			tc.pending(c)
			got, err := c.getKeyRYW(context.Background(), []byte(tc.k), tc.orEqual, tc.offset, maxKey, true, noServer)
			if err != nil {
				t.Fatalf("getKeyRYW: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("getKeyRYW(%q,oe=%v,off=%d): got %q, want %q", tc.k, tc.orEqual, tc.offset, got, tc.want)
			}
		})
	}
}

// TestGetKeyRYW_ServerRemerge exercises the unknown-range server-read-then-remerge
// loop: an empty cache + a mock server, with pending writes merged on top.
func TestGetKeyRYW_ServerRemerge(t *testing.T) {
	t.Parallel()
	maxKey := []byte("\xff")

	// storage {a, c}; pending Set(b). firstGreaterThan(a) → b (pending), and the loop
	// must read the server for the unknown tail to confirm nothing precedes b.
	c := &rywCache{}
	c.set([]byte("b"), []byte("x"))
	srv := &mockStorage{keys: map[string][]byte{"a": []byte("x"), "c": []byte("x")}}

	k, oe, off := fgt("a")
	got, err := c.getKeyRYW(context.Background(), []byte(k), oe, off, maxKey, true, srv.rangeFn)
	if err != nil {
		t.Fatalf("getKeyRYW: %v", err)
	}
	if string(got) != "b" {
		t.Fatalf("fgt(a) over storage{a,c}+Set(b): got %q, want b", got)
	}

	// firstGreaterThan(c) over storage{a,c}+Set(b): nothing > c → readThroughEnd → maxKey.
	c2 := &rywCache{}
	c2.set([]byte("b"), []byte("x"))
	srv2 := &mockStorage{keys: map[string][]byte{"a": []byte("x"), "c": []byte("x")}}
	k, oe, off = fgt("c")
	got, err = c2.getKeyRYW(context.Background(), []byte(k), oe, off, maxKey, true, srv2.rangeFn)
	if err != nil {
		t.Fatalf("getKeyRYW: %v", err)
	}
	if string(got) != string(maxKey) {
		t.Fatalf("fgt(c) past last key: got %q, want maxKey %q", got, maxKey)
	}

	// lastLessThan(b) over storage{a,c} (no pending): → a.
	c3 := &rywCache{}
	srv3 := &mockStorage{keys: map[string][]byte{"a": []byte("x"), "c": []byte("x")}}
	k, oe, off = llt("b")
	got, err = c3.getKeyRYW(context.Background(), []byte(k), oe, off, maxKey, true, srv3.rangeFn)
	if err != nil {
		t.Fatalf("getKeyRYW: %v", err)
	}
	if string(got) != "a" {
		t.Fatalf("llt(b) over storage{a,c}: got %q, want a", got)
	}
}

// TestGetKeyRYW_SnapshotBypassesWrites: with includeWrites=false (snapshot RYW
// disabled), a pending Set must NOT shift the selector.
func TestGetKeyRYW_SnapshotBypassesWrites(t *testing.T) {
	t.Parallel()
	maxKey := []byte("\xff")
	c := &rywCache{}
	c.serverCache.insert([]byte("a"), []byte("c\x00"), []KeyValue{
		{Key: []byte("a"), Value: []byte("x")}, {Key: []byte("c"), Value: []byte("x")},
	})
	c.set([]byte("b"), []byte("x")) // pending — must be ignored when includeWrites=false

	noServer := func(_ context.Context, _, _ []byte, _ int, _ bool) ([]KeyValue, bool, error) {
		t.Fatalf("server read must not happen")
		return nil, false, nil
	}
	// firstGreaterThan(a): includeWrites=false → ignores pending Set(b) → c.
	got, err := c.getKeyRYW(context.Background(), []byte("a"), true, 1, maxKey, false, noServer)
	if err != nil {
		t.Fatalf("getKeyRYW: %v", err)
	}
	if string(got) != "c" {
		t.Fatalf("snapshot fgt(a) ignoring pending Set(b): got %q, want c", got)
	}
}

// BenchmarkGetKeyRYW_CacheSize is RFC-057 Step 0 — the go/no-go gate for the lazy
// iterator. It measures whether getKeyRYW's per-call cost grows with the snapshot-cache
// size (the predicted O(cacheKeys) materialization in buildSegmentsLocked). A fully
// populated cache makes resolution cache-only (no server read), so this isolates the
// segment-build cost. If ns/op grows materially with N, the lazy iterator is warranted;
// if flat/negligible, the materializer is adequate and the refactor is dropped.
func BenchmarkGetKeyRYW_CacheSize(b *testing.B) {
	maxKey := []byte("\xff")
	noServer := func(_ context.Context, _, _ []byte, _ int, _ bool) ([]KeyValue, bool, error) {
		return nil, false, nil
	}
	for _, n := range []int{1, 100, 1000, 10000, 100000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			c := &rywCache{}
			kvs := make([]KeyValue, n)
			for i := 0; i < n; i++ {
				kvs[i] = KeyValue{Key: []byte(fmt.Sprintf("k%08d", i)), Value: []byte("v")}
			}
			// One fully-known cache range covering all n keys → resolution is cache-only.
			c.serverCache.insert([]byte("k"), []byte("k\xff"), kvs)
			// Probe near the middle so the walk itself is short (offset 1) — what varies
			// is only the per-call segment-build cost vs N.
			mid := []byte(fmt.Sprintf("k%08d", n/2))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := c.getKeyRYW(context.Background(), mid, false, 1, maxKey, true, noServer)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
