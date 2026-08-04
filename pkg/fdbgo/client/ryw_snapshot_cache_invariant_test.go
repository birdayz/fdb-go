package client

import (
	"bytes"
	"fmt"
	"testing"
)

// assertSnapshotCacheInvariants checks the structural contract every reader of
// snapshotCache binary-searches on: entries strictly ascending, pairwise
// non-overlapping, each entry non-empty, and every cached KV inside its own
// entry's [begin, end).
//
// Why this exists as a separate check rather than being implied by the
// behavioural tests: the old coalescing insert could not violate non-overlap
// even in principle — merging every touching neighbour into the new range is
// self-healing, so the invariant held no matter what the comparisons did. The
// C++ fragment insert has no such safety net; non-overlap rests entirely on the
// four boundary comparisons in insert (two trims, two erase bounds), and a
// single one of them flipped between > and >= leaves two entries covering the
// same key.
//
// Consumer-level assertions cannot substitute. getRangeKVs and getKey both read
// the same structure, so a corrupt structure makes them agree with each other
// while both being wrong: getRangeKVs walks entries in order and would emit a
// key twice, getKey stops at the first entry whose begin <= key and never sees
// the shadowed one. A cursor-vs-oracle comparison built on top of the cache
// inherits exactly the same blind spot. Only looking at the entries themselves
// catches it.
func assertSnapshotCacheInvariants(t *testing.T, sc *snapshotCache, tag string) {
	t.Helper()
	for i, e := range sc.entries {
		if bytes.Compare(e.begin, e.end) >= 0 {
			t.Fatalf("%s: entry %d is empty or inverted: %q..%q", tag, i, e.begin, e.end)
		}
		if i > 0 {
			prev := sc.entries[i-1]
			// Sorted AND non-overlapping in one comparison: the previous
			// entry must end at or before this one begins.
			if bytes.Compare(prev.end, e.begin) > 0 {
				t.Fatalf("%s: entries %d,%d overlap: %q..%q vs %q..%q", tag, i-1, i,
					prev.begin, prev.end, e.begin, e.end)
			}
			if bytes.Compare(prev.begin, e.begin) >= 0 {
				t.Fatalf("%s: entries %d,%d out of order: %q..%q vs %q..%q", tag, i-1, i,
					prev.begin, prev.end, e.begin, e.end)
			}
		}
		for k, kv := range e.kvs {
			if bytes.Compare(kv.Key, e.begin) < 0 || bytes.Compare(kv.Key, e.end) >= 0 {
				t.Fatalf("%s: entry %d kv %d key %q outside %q..%q", tag, i, k, kv.Key, e.begin, e.end)
			}
			if k > 0 && bytes.Compare(e.kvs[k-1].Key, kv.Key) >= 0 {
				t.Fatalf("%s: entry %d kvs unsorted at %d: %q then %q", tag, i, k,
					e.kvs[k-1].Key, kv.Key)
			}
		}
	}
}

// TestSnapshotCache_InsertSequencesHoldInvariants drives insert-order shapes
// that put each of insert's four boundary comparisons on the boundary itself —
// where > and >= differ. Every one of them is a real fetch pattern: a scan
// re-reading a page it already has, a bounded read whose end lands exactly on a
// cached fragment's edge, a widening retry after a limit was raised.
//
// The abut-end case (insert a range, then insert a wider range with the SAME
// end) is the one no behavioural assertion reaches: it produces two entries
// covering the same keys, and both consumers of the cache read past it happily.
func TestSnapshotCache_InsertSequencesHoldInvariants(t *testing.T) {
	t.Parallel()

	type ins struct {
		begin, end string
		keys       []string
	}
	cases := []struct {
		name  string
		steps []ins
		// want is the expected [begin,end) layout after all steps, so the
		// invariant check is backed by an exact-layout assertion rather than
		// only by "nothing overlaps".
		want []string
	}{
		{
			name:  "widen left, same end",
			steps: []ins{{"b", "c", []string{"b"}}, {"a", "c", []string{"b"}}},
			want:  []string{"a..c"},
		},
		{
			name:  "widen right, same begin",
			steps: []ins{{"a", "b", []string{"a"}}, {"a", "c", []string{"a"}}},
			want:  []string{"a..c"},
		},
		{
			name:  "identical re-insert",
			steps: []ins{{"a", "c", []string{"b"}}, {"a", "c", []string{"b"}}},
			want:  []string{"a..c"},
		},
		{
			name:  "adjacent pages stay separate",
			steps: []ins{{"a", "c", []string{"a"}}, {"c", "e", []string{"c"}}},
			want:  []string{"a..c", "c..e"},
		},
		{
			name:  "re-fetch spanning two adjacent fragments supersedes both",
			steps: []ins{{"a", "c", []string{"b"}}, {"c", "e", []string{"d"}}, {"a", "e", []string{"b", "d"}}},
			want:  []string{"a..e"},
		},
		{
			name:  "span erases interior, both ends trimmed",
			steps: []ins{{"a", "c", []string{"a"}}, {"d", "e", []string{"d"}}, {"g", "i", []string{"h"}}, {"b", "h", []string{"f"}}},
			want:  []string{"a..c", "c..g", "g..i"},
		},
		{
			// A zero-row fetch leaves an EMPTY fragment of its own rather than
			// being absorbed by a neighbour, so an empty entry can be the thing
			// a later insert trims against.
			name:  "later insert trims against an empty fragment",
			steps: []ins{{"c", "e", nil}, {"a", "d", []string{"b"}}},
			want:  []string{"a..c", "c..e"},
		},
		{
			name:  "zero-row fetch adjacent to a populated fragment",
			steps: []ins{{"a", "c", []string{"b"}}, {"c", "e", nil}, {"e", "g", nil}},
			want:  []string{"a..c", "c..e", "e..g"},
		},
		{
			name:  "insert wholly inside a known range",
			steps: []ins{{"a", "z", []string{"m"}}, {"c", "f", []string{"d"}}},
			want:  []string{"a..z"},
		},
		{
			name:  "front trim to exactly the existing end",
			steps: []ins{{"a", "c", []string{"b"}}, {"b", "c", []string{"b"}}},
			want:  []string{"a..c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sc snapshotCache
			for i, s := range tc.steps {
				kvs := make([]KeyValue, 0, len(s.keys))
				for _, k := range s.keys {
					kvs = append(kvs, KeyValue{Key: []byte(k), Value: []byte("v")})
				}
				sc.insert([]byte(s.begin), []byte(s.end), kvs)
				assertSnapshotCacheInvariants(t, &sc, fmt.Sprintf("%s after step %d", tc.name, i))
			}
			var got []string
			for _, e := range sc.entries {
				got = append(got, string(e.begin)+".."+string(e.end))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s: layout %v, want %v", tc.name, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: layout %v, want %v", tc.name, got, tc.want)
				}
			}
		})
	}
}

// TestCacheServerResult_TruncatedFetchKeepsFragmentsBeyondIt pins the retention
// property cacheServerResult's narrowing exists for. A fetch that reported more
// data available knows the server's state only up to its last returned key;
// claiming the whole requested range instead would mark the tail known-empty,
// and insert would erase every fragment inside it. That is silent data loss —
// the structure stays perfectly valid, later reads just see rows that are gone.
//
// Both directions are checked because the narrowing is asymmetric: a forward
// fetch trims the END down to keyAfter(lastKey), a reverse fetch trims the
// BEGIN up to the smallest key returned, and each protects the opposite side.
func TestCacheServerResult_TruncatedFetchKeepsFragmentsBeyondIt(t *testing.T) {
	t.Parallel()

	kv := func(k string) KeyValue { return KeyValue{Key: []byte(k), Value: []byte("v")} }

	t.Run("forward keeps the fragment past its last key", func(t *testing.T) {
		t.Parallel()
		var c rywCache
		c.serverCache.insert([]byte("m"), []byte("q"), []KeyValue{kv("n")})

		// Fetch [a,z) that stopped after "b": knowledge ends at "b\x00".
		c.cacheServerResult([]byte("a"), []byte("z"), []KeyValue{kv("a"), kv("b")}, true, false)

		assertSnapshotCacheInvariants(t, &c.serverCache, "forward truncated fetch")
		if v, known := c.serverCache.getKey([]byte("n")); !known || v == nil {
			t.Fatalf("truncated forward fetch erased the fragment at [m,q): known=%v value=%v", known, v)
		}
		if _, known := c.serverCache.getKey([]byte("c")); known {
			t.Fatal("truncated forward fetch claimed knowledge past its last returned key")
		}
	})

	t.Run("reverse keeps the fragment before its smallest key", func(t *testing.T) {
		t.Parallel()
		var c rywCache
		c.serverCache.insert([]byte("b"), []byte("e"), []KeyValue{kv("c")})

		// Reverse fetch of [a,z) that stopped at "w": knowledge begins at "w".
		// serverKVs arrive in descending order on the reverse path.
		c.cacheServerResult([]byte("a"), []byte("z"), []KeyValue{kv("y"), kv("w")}, true, true)

		assertSnapshotCacheInvariants(t, &c.serverCache, "reverse truncated fetch")
		if v, known := c.serverCache.getKey([]byte("c")); !known || v == nil {
			t.Fatalf("truncated reverse fetch erased the fragment at [b,e): known=%v value=%v", known, v)
		}
		if _, known := c.serverCache.getKey([]byte("v")); known {
			t.Fatal("truncated reverse fetch claimed knowledge below its smallest returned key")
		}
	})
}
