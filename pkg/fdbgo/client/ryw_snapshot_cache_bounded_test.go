package client

import (
	"bytes"
	"fmt"
	"testing"
)

// fillCache populates sc with n keys ("k%06d") spread across fragments of `page` keys each,
// the layout a paged scan leaves behind: one fragment per fetch, adjacent, never coalesced.
func fillCache(sc *snapshotCache, n, page int) (begin, end []byte) {
	for start := 0; start < n; start += page {
		var kvs []KeyValue
		for i := start; i < start+page && i < n; i++ {
			kvs = append(kvs, KeyValue{
				Key:   []byte(fmt.Sprintf("k%06d", i)),
				Value: []byte("v"),
			})
		}
		// The first fragment starts at "k" and the last ends at "l" so that [k,l) is
		// covered with NO unknown prefix or suffix — otherwise the walk is entitled to
		// report a miss and the range is not the fully-cached one these tests mean.
		b := []byte(fmt.Sprintf("k%06d", start))
		if start == 0 {
			b = []byte("k")
		}
		var e []byte
		if start+page < n {
			e = []byte(fmt.Sprintf("k%06d", start+page))
		} else {
			e = []byte("l")
		}
		sc.insert(b, e, kvs)
	}
	return []byte("k"), []byte("l")
}

// TestSnapshotCacheWalkCostsThePageNotTheTail is the cost pin for the bounded cache walk.
//
// A fully-cached range re-scanned page by page — GetSliceWithError and then an Iterator over
// the same range in the same transaction — reaches the cache-hit arm on every Advance. The
// walk used to append EVERY remaining KV in [begin, end) and let applyLimitAndDirection
// truncate to the page afterwards, so a drain did N/page full-tail copies and even the first
// "bounded" page allocated Theta(N).
//
// No assertion on the returned rows can catch that: a walk that scans a million rows and
// truncates to 1024 returns exactly the page a walk that stops at 1024 returns. So the pin is
// on the work itself, via the kvsTouched counter — the honest instrument. C++ bounds the same
// walk with GetRangeLimits (ReadYourWrites.actor.cpp:785-789 counts under !limits.isReached()
// and appends only the counted prefix; :685 breaks the traversal).
func TestSnapshotCacheWalkCostsThePageNotTheTail(t *testing.T) {
	t.Parallel()

	const n = 50000
	const page = 1024
	const budget = page + 1 // what cacheWalkBudget(1024) asks for

	for _, reverse := range []bool{false, true} {
		reverse := reverse
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sc := &snapshotCache{}
			begin, end := fillCache(sc, n, page)

			sc.kvsTouched = 0
			kvs, ok := sc.getRangeKVsLimited(begin, end, budget, reverse)
			if !ok {
				t.Fatalf("expected a full cache hit over a fully-cached range")
			}
			// The whole point: work is proportional to the PAGE, not to the tail behind it.
			// The unbounded walk touches all n.
			if sc.kvsTouched > budget {
				t.Fatalf("walk touched %d KVs to return a %d-row page over a %d-row cached "+
					"range — a page must cost the page, not the tail. The walk has to stop "+
					"once the budget is met (C++ bounds the same traversal with "+
					"GetRangeLimits, ReadYourWrites.actor.cpp:785-789)",
					sc.kvsTouched, budget, n)
			}

			if len(kvs) != budget {
				t.Fatalf("returned %d KVs, want %d (the page plus the one row computeMore reads)",
					len(kvs), budget)
			}
		})
	}
}

// TestSnapshotCacheBoundedWalkMatchesUnbounded is the correctness control for the bound: the
// bounded walk must return exactly what the unbounded walk would have, after the caller's
// applyLimitAndDirection — same rows, same order, same `more` verdict — across directions,
// budgets, and sub-ranges that start and end inside a fragment.
//
// This is the regression risk of the change, so it is checked against the unbounded walk
// itself rather than against hand-written expectations.
func TestSnapshotCacheBoundedWalkMatchesUnbounded(t *testing.T) {
	t.Parallel()

	const n = 4000
	const page = 512
	sc := &snapshotCache{}
	fillCache(sc, n, page)

	ranges := [][2]string{
		{"k", "l"},             // whole range
		{"k000000", "k000001"}, // single key
		{"k000700", "k002300"}, // starts and ends mid-fragment
		{"k000512", "k001024"}, // exactly one fragment
		{"k003999", "l"},       // tail
		{"k002047", "k002049"}, // straddles a fragment boundary
	}
	limits := []int{0, 1, 2, 511, 512, 513, 1000, n * 2}

	for _, r := range ranges {
		for _, limit := range limits {
			for _, reverse := range []bool{false, true} {
				begin, end := []byte(r[0]), []byte(r[1])

				full, okFull := sc.getRangeKVsLimited(begin, end, 0, false)
				bounded, okBounded := sc.getRangeKVsLimited(begin, end, cacheWalkBudget(limit), reverse)
				if okFull != okBounded {
					t.Fatalf("range [%s,%s) limit=%d reverse=%v: coverage verdict differs "+
						"(unbounded=%v bounded=%v)", r[0], r[1], limit, reverse, okFull, okBounded)
				}
				if !okFull {
					continue
				}

				wantKVs := applyLimitAndDirection(full, limit, reverse)
				wantMore := computeMore(full, limit)
				gotKVs := applyLimitAndDirection(bounded, limit, reverse)
				gotMore := computeMore(bounded, limit)

				if len(gotKVs) != len(wantKVs) {
					t.Fatalf("range [%s,%s) limit=%d reverse=%v: got %d rows, want %d",
						r[0], r[1], limit, reverse, len(gotKVs), len(wantKVs))
				}
				for i := range wantKVs {
					if !bytes.Equal(gotKVs[i].Key, wantKVs[i].Key) {
						t.Fatalf("range [%s,%s) limit=%d reverse=%v: row %d key = %q, want %q",
							r[0], r[1], limit, reverse, i, gotKVs[i].Key, wantKVs[i].Key)
					}
					if !bytes.Equal(gotKVs[i].Value, wantKVs[i].Value) {
						t.Fatalf("range [%s,%s) limit=%d reverse=%v: row %d value mismatch",
							r[0], r[1], limit, reverse, i)
					}
				}
				if gotMore != wantMore {
					t.Fatalf("range [%s,%s) limit=%d reverse=%v: more = %v, want %v — the "+
						"budget must leave one row past the limit for computeMore to read",
						r[0], r[1], limit, reverse, gotMore, wantMore)
				}
			}
		}
	}
}

// TestSnapshotCacheBoundedWalkStillDetectsGaps pins that the bound did not buy its speed by
// weakening the coverage contract. A satisfied budget makes the answer final and coverage of
// the rest of the range irrelevant — but a budget that is NOT satisfied before a gap must
// still report a miss, or the caller would serve a short page from a range it has not fully
// cached and call it complete.
func TestSnapshotCacheBoundedWalkStillDetectsGaps(t *testing.T) {
	t.Parallel()

	sc := &snapshotCache{}
	// Two fragments with a hole between them: [a,b) known, [b,c) unknown, [c,d) known.
	sc.insert([]byte("a"), []byte("b"), []KeyValue{
		{Key: []byte("a1"), Value: []byte("v")},
		{Key: []byte("a2"), Value: []byte("v")},
	})
	sc.insert([]byte("c"), []byte("d"), []KeyValue{
		{Key: []byte("c1"), Value: []byte("v")},
		{Key: []byte("c2"), Value: []byte("v")},
	})

	// A budget satisfied entirely inside the first fragment: the gap is past the decided
	// page, so this is a legitimate hit.
	if kvs, ok := sc.getRangeKVsLimited([]byte("a"), []byte("d"), 2, false); !ok || len(kvs) != 2 {
		t.Fatalf("budget satisfiable before the gap: got (%d rows, ok=%v), want (2, true) — "+
			"a page decided before the gap is final", len(kvs), ok)
	}
	// A budget that cannot be satisfied before the gap must miss, not return a short page.
	if _, ok := sc.getRangeKVsLimited([]byte("a"), []byte("d"), 3, false); ok {
		t.Fatal("budget spanning the gap reported a full cache hit — the walk must still " +
			"detect the unknown range and force a server fetch")
	}
	// Same both ways: descending from d, the gap sits between c1/c2 and the a-fragment.
	if kvs, ok := sc.getRangeKVsLimited([]byte("a"), []byte("d"), 2, true); !ok || len(kvs) != 2 {
		t.Fatalf("reverse budget satisfiable before the gap: got (%d rows, ok=%v), want (2, true)",
			len(kvs), ok)
	}
	if _, ok := sc.getRangeKVsLimited([]byte("a"), []byte("d"), 3, true); ok {
		t.Fatal("reverse budget spanning the gap reported a full cache hit")
	}
	// Unbounded is unchanged: the whole range is not known.
	if _, ok := sc.getRangeKVs([]byte("a"), []byte("d")); ok {
		t.Fatal("unbounded walk over a gapped range reported a hit")
	}
}
