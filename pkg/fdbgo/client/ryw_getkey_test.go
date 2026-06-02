package client

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
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

// --- RFC-057: equivalence oracle (retained materializer) + property test --------------

type rywSegment struct {
	begin []byte
	end   []byte
	typ   rywSegType
}

// buildSegmentsLocked is the RETAINED materializer — kept ONLY as the equivalence-test
// ORACLE for the lazy rywSegCursor (RFC-057). It produced byte-identical resolution in
// production; the cursor must yield the identical segment partition. (Moved out of prod
// to the test binary so the lazy cursor is the sole production path.)
func (c *rywCache) buildSegmentsLocked(hi []byte, includeWrites bool) []rywSegment {
	var bounds [][]byte
	add := func(b []byte) {
		if bytes.Compare(b, allKeysBegin) >= 0 && bytes.Compare(b, hi) <= 0 {
			bounds = append(bounds, b)
		}
	}
	add(allKeysBegin)
	add(hi)
	if includeWrites {
		c.ensureSortedLocked()
		for _, k := range c.sortedKeys {
			kb := []byte(k)
			add(kb)
			add(keyAfterBytes(kb))
		}
		for _, r := range c.cleared {
			add(r.begin)
			add(r.end)
		}
	}
	for i := range c.serverCache.entries {
		e := &c.serverCache.entries[i]
		add(e.begin)
		add(e.end)
		for _, kv := range e.kvs {
			add(kv.Key)
			add(keyAfterBytes(kv.Key))
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bytes.Compare(bounds[i], bounds[j]) < 0 })
	uniq := bounds[:0]
	var last []byte
	for _, b := range bounds {
		if len(uniq) == 0 || !bytes.Equal(b, last) {
			uniq = append(uniq, b)
			last = b
		}
	}
	segs := make([]rywSegment, 0, len(uniq))
	for i := 0; i+1 < len(uniq); i++ {
		segs = append(segs, rywSegment{begin: uniq[i], end: uniq[i+1], typ: c.segTypeAtLocked(uniq[i], includeWrites)})
	}
	return segs
}

// walkCursor collects the full segment sequence the lazy cursor yields walking forward
// from allKeysBegin via seek+next, then verifies prev() reverses it.
func walkCursorForward(c *rywCache, hi []byte, includeWrites bool) []rywSegment {
	cur := c.newSegCursor(hi, includeWrites)
	var got []rywSegment
	for cur.seek(allKeysBegin); cur.valid(); cur.next() {
		got = append(got, rywSegment{begin: append([]byte(nil), cur.begin...), end: append([]byte(nil), cur.end...), typ: cur.typ})
	}
	return got
}

func segsEqual(t *testing.T, label string, want, got []rywSegment) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: segment COUNT differs: oracle=%d cursor=%d\noracle=%v\ncursor=%v", label, len(want), len(got), segDump(want), segDump(got))
	}
	for i := range want {
		if !bytes.Equal(want[i].begin, got[i].begin) || !bytes.Equal(want[i].end, got[i].end) || want[i].typ != got[i].typ {
			t.Fatalf("%s: segment %d differs: oracle=(%q,%q,%d) cursor=(%q,%q,%d)", label, i,
				want[i].begin, want[i].end, want[i].typ, got[i].begin, got[i].end, got[i].typ)
		}
	}
}

func segDump(s []rywSegment) string {
	out := ""
	for _, x := range s {
		out += fmt.Sprintf("[%q,%q):%d ", x.begin, x.end, x.typ)
	}
	return out
}

// TestRYWSegCursor_EquivalentToMaterializer is the RFC-057 load-bearing faithfulness
// check: over many random (writes, cleared, cache) states the lazy cursor must yield the
// IDENTICAL segment partition the materializer does — forward (seek+next), per-probe
// (seek to a key → containing segment), and backward (prev reverses). Seeded with the
// reviewer-flagged successor-collision + shared-boundary cases.
func TestRYWSegCursor_EquivalentToMaterializer(t *testing.T) {
	t.Parallel()
	hi := []byte("\xff")
	// Alphabet kept tiny + including adjacent/successor bytes so boundaries collide.
	alpha := []string{"\x00", "a", "a\x00", "b", "c", "c\x00", "d", "m", "\xee", "\xef"}
	pick := func(rnd *rand.Rand) []byte { return []byte(alpha[rnd.Intn(len(alpha))]) }

	build := func(rnd *rand.Rand) *rywCache {
		c := &rywCache{}
		// random pending writes
		for n := rnd.Intn(6); n > 0; n-- {
			k := pick(rnd)
			switch rnd.Intn(5) {
			case 0:
				c.set(k, []byte("v"))
			case 1:
				c.clear(k)
			case 2:
				c.atomic(MutAddValue, k, []byte("\x01"))
			case 3:
				c.atomic(MutSetVersionstampedValue, k, make([]byte, 14))
			case 4:
				c.atomic(MutCompareAndClear, k, []byte("v"))
			}
		}
		// random committed snapshot-cache ranges with random present keys
		for n := rnd.Intn(3); n > 0; n-- {
			b, e := pick(rnd), pick(rnd)
			if bytes.Compare(b, e) >= 0 {
				continue
			}
			var kvs []KeyValue
			for _, a := range alpha {
				ak := []byte(a)
				if bytes.Compare(ak, b) >= 0 && bytes.Compare(ak, e) < 0 && rnd.Intn(2) == 0 {
					kvs = append(kvs, KeyValue{Key: ak, Value: []byte("x")})
				}
			}
			c.serverCache.insert(b, e, kvs)
		}
		return c
	}

	rnd := rand.New(rand.NewSource(1))
	for iter := 0; iter < 4000; iter++ {
		c := build(rnd)
		for _, includeWrites := range []bool{true, false} {
			want := c.buildSegmentsLocked(hi, includeWrites)
			// Forward walk equivalence.
			got := walkCursorForward(c, hi, includeWrites)
			segsEqual(t, fmt.Sprintf("iter=%d iw=%v forward", iter, includeWrites), want, got)
			// Per-probe seek: cursor.seek(probe) must equal the materialized segment containing probe.
			for _, a := range alpha {
				probe := []byte(a)
				cur := c.newSegCursor(hi, includeWrites)
				cur.seek(probe)
				idx := segIdxOracle(want, probe)
				if idx < 0 {
					if !cur.offEnd() {
						t.Fatalf("iter=%d iw=%v seek(%q): cursor valid but oracle off-end", iter, includeWrites, probe)
					}
					continue
				}
				if !cur.valid() || !bytes.Equal(cur.begin, want[idx].begin) || !bytes.Equal(cur.end, want[idx].end) || cur.typ != want[idx].typ {
					t.Fatalf("iter=%d iw=%v seek(%q): cursor=(%q,%q,%d,valid=%v) oracle=(%q,%q,%d)", iter, includeWrites, probe,
						cur.begin, cur.end, cur.typ, cur.valid(), want[idx].begin, want[idx].end, want[idx].typ)
				}
			}
			// Backward walk: from the last segment, prev() must reverse the sequence.
			if len(want) > 0 {
				cur := c.newSegCursor(hi, includeWrites)
				cur.seek(want[len(want)-1].begin)
				var rev []rywSegment
				for cur.valid() {
					rev = append(rev, rywSegment{begin: append([]byte(nil), cur.begin...), end: append([]byte(nil), cur.end...), typ: cur.typ})
					cur.prev()
				}
				// reverse rev and compare to want
				for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
					rev[l], rev[r] = rev[r], rev[l]
				}
				segsEqual(t, fmt.Sprintf("iter=%d iw=%v backward", iter, includeWrites), want, rev)
			}
		}
	}
}

// segIdxOracle returns the index of the materialized segment containing key, or -1 if key >= hi.
func segIdxOracle(segs []rywSegment, key []byte) int {
	for i := range segs {
		if bytes.Compare(segs[i].begin, key) <= 0 && bytes.Compare(key, segs[i].end) < 0 {
			return i
		}
	}
	return -1
}
