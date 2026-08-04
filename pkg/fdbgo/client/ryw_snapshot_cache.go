package client

import (
	"bytes"
	"sort"
)

// snapshotCache caches server-side key-value state at the transaction's read
// version. Entries are sorted by begin key, non-overlapping, and store all
// server KVs in forward (ascending key) order.
//
// This cache is NOT invalidated by local writes/clears — those are tracked
// separately in the rywCache WriteMap. The merge of server state + local
// mutations happens at read time, matching C++'s SnapshotCache + WriteMap
// architecture.
//
// Thread safety: callers must hold rywCache.mu when accessing this struct.
type snapshotCache struct {
	entries []cacheEntry

	// kvsTouched counts cached KeyValues examined by the range walk since the last
	// reset. It exists to make the walk's cost observable: the walk is supposed to do
	// work proportional to the PAGE it returns, not to the tail still cached behind it,
	// and that is a property no assertion on the returned rows can distinguish (a walk
	// that scans a million rows and truncates to 1024 returns exactly the same page as
	// one that stops at 1024). Read only by tests; an int increment on the walk is not
	// measurable against the byte comparisons it accompanies.
	kvsTouched uint64
}

// cacheEntry represents a contiguous range of known server state.
// All keys in [begin, end) are accounted for: if a key exists at the server,
// it appears in kvs. If it doesn't appear, the key doesn't exist.
//
// Entries are non-empty (begin < end) and pairwise NON-OVERLAPPING, in the weak
// sense entries[i].end <= entries[i+1].begin — adjacency is allowed and, since
// each fetch leaves its own fragment, is the normal case. Non-overlap is a
// contract, not a coincidence: getRangeKVs walks entries in order and emits
// every kv in [begin, end) it finds, so its no-duplicate guarantee is exactly
// the statement that no key is covered by two entries. getKey stops at the last
// entry whose begin <= key and would silently read past a shadowed one.
//
// insert is the sole writer and the sole thing keeping the invariant true; it
// has no self-healing property (see its comment), so the invariant is pinned
// directly by assertSnapshotCacheInvariants rather than through either reader.
type cacheEntry struct {
	begin []byte     // inclusive
	end   []byte     // exclusive
	kvs   []KeyValue // sorted ascending by key
}

// reset clears all cached state.
func (sc *snapshotCache) reset() {
	sc.entries = nil
}

// insert marks [begin, end) as known with the given KVs (must be sorted
// ascending by key).
//
// This is C++ `SnapshotCache::insert(KeyRangeRef, VectorRef<KeyValueRef>)`
// (fdbclient/include/fdbclient/SnapshotCache.h:345-382). The shape that matters
// is what it does NOT do: it never coalesces an already-known neighbour into the
// range being inserted, and it therefore never touches the values already cached.
// It trims the incoming range against whatever is already known at each end,
// erases only the entries STRICTLY INSIDE what remains, and inserts one new node
// holding just this fetch's values.
//
// Coalescing instead — concatenating the neighbours' values with the new ones and
// re-sorting — is what makes a sequential scan quadratic: the Nth page copies and
// sorts all N-1 pages before it, so a scan of G rows in pages of P does Θ(G²/P)
// work and allocates that many KeyValues, while retaining exactly the same rows.
// A single unbounded scan of a large subspace then burns the transaction's 5s
// budget inside the cache rather than at the server. Fragments cost O(log n) per
// page; the lookup side already walks adjacent entries and checks for gaps, so
// nothing downstream needs them merged.
//
// Retention itself is NOT the divergence: C++ keeps every row of every snapshot
// range read for the life of the transaction too (the SnapshotCache is
// arena-allocated and has no eviction), so bounding memory here would diverge.
// Only the per-insert cost is being brought back in line.
func (sc *snapshotCache) insert(begin, end []byte, kvs []KeyValue) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}

	newBegin := begin
	newEnd := end
	n := len(sc.entries)

	// Trim the front past an existing entry that STRICTLY contains newBegin.
	// C++: `begin = itb.it->endKey`, guarded by `!itb.is_unknown_range() &&
	// itb.it->beginKey != keys.begin`. An entry starting exactly at newBegin is
	// deliberately not trimmed past — it is superseded below instead. An entry
	// ending exactly at newBegin does not contain it (end is exclusive), which
	// is the adjacent-page case and must not trim either.
	if i := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].end, newBegin) > 0
	}); i < n && bytes.Compare(sc.entries[i].begin, newBegin) < 0 {
		newBegin = sc.entries[i].end
	}

	// Trim the back to the start of an existing entry that STRICTLY contains
	// newEnd. C++: `ite._prevUnknown(); end = ite.endKey()`, reached only when
	// the segment holding keys.end is not an unknown range — an entry ending
	// exactly at newEnd does not hold it, so it must not trim.
	if j := sort.Search(n, func(j int) bool {
		return bytes.Compare(sc.entries[j].begin, newEnd) >= 0
	}); j > 0 && bytes.Compare(sc.entries[j-1].end, newEnd) > 0 {
		newEnd = sc.entries[j-1].begin
	}

	if bytes.Compare(newBegin, newEnd) >= 0 {
		return // fully known already — C++'s `if (begin < end)` guard
	}

	// After trimming, no surviving entry partially overlaps [newBegin, newEnd):
	// the only entry that could straddle either end was what the trim moved that
	// end to. So [lo, hi) is exactly the set of entries strictly inside, which
	// this fetch supersedes wholesale (same read version, so same contents).
	lo := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].begin, newBegin) >= 0
	})
	hi := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].end, newEnd) > 0
	})

	// Keep only the values that fall inside the trimmed range. kvs is sorted, so
	// this is a slice of the caller's page, never a merge with cached values.
	lok := sort.Search(len(kvs), func(k int) bool {
		return bytes.Compare(kvs[k].Key, newBegin) >= 0
	})
	hik := sort.Search(len(kvs), func(k int) bool {
		return bytes.Compare(kvs[k].Key, newEnd) >= 0
	})

	fresh := cacheEntry{
		begin: append([]byte(nil), newBegin...),
		end:   append([]byte(nil), newEnd...),
		kvs:   copyKVs(kvs[lok:hik]),
	}

	switch hi - lo {
	case 0:
		sc.entries = append(sc.entries, cacheEntry{})
		copy(sc.entries[lo+1:], sc.entries[lo:])
		sc.entries[lo] = fresh
	case 1:
		sc.entries[lo] = fresh
	default:
		sc.entries[lo] = fresh
		sc.entries = append(sc.entries[:lo+1], sc.entries[hi:]...)
	}
}

// getRangeKVs returns all cached KVs in [begin, end) if the entire range is
// known. Returns (kvs, true) on full cache hit, (nil, false) on any miss.
// Returned KVs are in ascending key order.
func (sc *snapshotCache) getRangeKVs(begin, end []byte) ([]KeyValue, bool) {
	return sc.getRangeKVsLimited(begin, end, 0, false)
}

// getRangeKVsLimited is getRangeKVs bounded by demand: it walks the cache in the
// direction the read asked for and stops as soon as `budget` KVs are in hand.
// budget <= 0 means unbounded. Results are ALWAYS returned in ascending key order
// (the callers' applyLimitAndDirection does the reversing), so for a reverse read
// this is the HIGHEST `budget` keys in [begin, end).
//
// This is C++'s shape, not an optimization on top of it. The RYW range walk carries
// GetRangeLimits into the cache traversal: ReadYourWrites.actor.cpp:785-789 counts
// out of a contiguous cached run under `!limits.isReached()` and then appends only
// that counted prefix —
//
//	int maxCount = it.kv(ryw->arena) - start + 1;
//	int count = 0;
//	for (; count < maxCount && !limits.isReached(); count++)
//	        limits.decrement(start[count]);
//	...
//	if (count) result.append(result.arena(), start, count);
//
// — and :685 (`if (limits.isReached() && itemsPastEnd >= 1 - end.offset) break;`)
// ends the traversal outright. getRangeValueBack does the same at :1091. C++ never
// materializes the tail behind the page.
//
// Why the bound matters here and not just in theory: a satisfied budget makes the
// answer FINAL, so coverage of the rest of [begin, end) is irrelevant to it. Without
// that, a fully-cached range re-scanned page by page (GetSliceWithError, then an
// Iterator over the same range in the same transaction) copied every remaining KV on
// every page before truncating to the page size — N/page full-tail copies, and even
// the first "bounded" page allocated Theta(N).
//
// Correctness rests on the callers being unchanged: both sites feed this straight to
// applyLimitAndDirection(kvs, limit, reverse) and computeMore(kvs, limit), so passing
// budget = limit+1 makes this a pure FUSION of the walk with the truncation that
// already followed it — same page, same `more` (the +1 is exactly what computeMore's
// `len(kvs) > limit` needs to see). It is deliberately NOT a limit applied to a merged
// stream: the fast-path caller is only reached when no write or clear touches
// [begin, end), and the fetchOrCached caller passes the server-side fetch limit that
// already carries the slow path's clear-headroom, with the merge limiting itself
// afterwards.
func (sc *snapshotCache) getRangeKVsLimited(begin, end []byte, budget int, reverse bool) ([]KeyValue, bool) {
	n := len(sc.entries)
	if n == 0 || bytes.Compare(begin, end) >= 0 {
		return nil, false
	}
	if reverse {
		return sc.getRangeKVsBack(begin, end, budget)
	}

	// Find the first entry whose end > begin (could contain begin).
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].end, begin) > 0
	})

	if i >= n || bytes.Compare(sc.entries[i].begin, begin) > 0 {
		return nil, false // begin is not covered
	}

	// Walk entries ascending, collecting KVs, checking for gaps.
	var result []KeyValue
	cur := begin
	for j := i; j < n; j++ {
		e := &sc.entries[j]
		if bytes.Compare(e.begin, cur) > 0 {
			return nil, false // gap
		}
		// Seek to the first key at or after begin rather than scanning the whole
		// entry: with one fragment per fetch, a linear scan per entry is what made
		// the walk cost the tail rather than the page.
		lo := sort.Search(len(e.kvs), func(k int) bool {
			return bytes.Compare(e.kvs[k].Key, begin) >= 0
		})
		for k := lo; k < len(e.kvs); k++ {
			kv := e.kvs[k]
			if bytes.Compare(kv.Key, end) >= 0 {
				break
			}
			sc.kvsTouched++
			result = append(result, kv)
			if budget > 0 && len(result) >= budget {
				// The page is decided; whatever is cached past here cannot change it.
				return result, true
			}
		}
		cur = e.end
		if bytes.Compare(cur, end) >= 0 {
			return result, true // fully covered
		}
	}
	return nil, false // end not reached
}

// getRangeKVsBack is the reverse-direction walk: it descends from `end` so a reverse
// read pays for its page rather than for the range below it. Returns ascending KVs.
// C++'s counterpart is getRangeValueBack, whose cache walk bounds itself the same way
// (ReadYourWrites.actor.cpp:1091).
func (sc *snapshotCache) getRangeKVsBack(begin, end []byte, budget int) ([]KeyValue, bool) {
	n := len(sc.entries)

	// Last entry that can hold a key below end.
	j := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].begin, end) >= 0
	}) - 1
	if j < 0 {
		return nil, false
	}

	var desc []KeyValue
	cur := end // everything at or above cur is accounted for
	for ; j >= 0; j-- {
		e := &sc.entries[j]
		if bytes.Compare(e.end, cur) < 0 {
			return nil, false // gap between this entry and what we already covered
		}
		// Highest key strictly below end, walking down to begin.
		hi := sort.Search(len(e.kvs), func(k int) bool {
			return bytes.Compare(e.kvs[k].Key, end) >= 0
		})
		for k := hi - 1; k >= 0; k-- {
			kv := e.kvs[k]
			if bytes.Compare(kv.Key, begin) < 0 {
				break
			}
			sc.kvsTouched++
			desc = append(desc, kv)
			if budget > 0 && len(desc) >= budget {
				return reversedKVs(desc), true // top `budget` keys — page decided
			}
		}
		cur = e.begin
		if bytes.Compare(cur, begin) <= 0 {
			return reversedKVs(desc), true // covered down to begin
		}
	}
	return nil, false // begin not reached
}

// reversedKVs returns kvs in the opposite order, as a fresh slice.
func reversedKVs(kvs []KeyValue) []KeyValue {
	out := make([]KeyValue, len(kvs))
	for i, kv := range kvs {
		out[len(kvs)-1-i] = kv
	}
	return out
}

// getKey checks if a single key's server state is cached. Returns (value, true)
// if the key falls within a known range. value is nil if the key doesn't exist
// at the server. Returns (nil, false) if the key's range is unknown.
func (sc *snapshotCache) getKey(key []byte) ([]byte, bool) {
	n := len(sc.entries)
	if n == 0 {
		return nil, false
	}

	// Find entry containing key: last entry with begin <= key, check key < end.
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].begin, key) > 0
	})
	if i == 0 {
		return nil, false
	}
	e := &sc.entries[i-1]
	if bytes.Compare(key, e.end) >= 0 {
		return nil, false // key is past this entry's range
	}

	// Key is in [e.begin, e.end). Binary search the KVs.
	k := sort.Search(len(e.kvs), func(j int) bool {
		return bytes.Compare(e.kvs[j].Key, key) >= 0
	})
	if k < len(e.kvs) && bytes.Equal(e.kvs[k].Key, key) {
		return e.kvs[k].Value, true
	}
	return nil, true // key is in known range but doesn't exist at server
}

// copyKVs makes a shallow copy of the KV slice. The Key/Value byte slices
// alias the caller's backing arrays. This is safe because FDB response
// buffers are not pooled — once parsed, the byte slices are stable for the
// lifetime of the transaction.
func copyKVs(kvs []KeyValue) []KeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]KeyValue, len(kvs))
	copy(out, kvs)
	return out
}
