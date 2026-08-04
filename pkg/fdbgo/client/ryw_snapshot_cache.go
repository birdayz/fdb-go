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
}

// cacheEntry represents a contiguous range of known server state.
// All keys in [begin, end) are accounted for: if a key exists at the server,
// it appears in kvs. If it doesn't appear, the key doesn't exist.
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
	n := len(sc.entries)
	if n == 0 {
		return nil, false
	}

	// Find the first entry whose end > begin (could contain begin).
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].end, begin) > 0
	})

	if i >= n || bytes.Compare(sc.entries[i].begin, begin) > 0 {
		return nil, false // begin is not covered
	}

	// Walk entries, collecting KVs, checking for gaps.
	var result []KeyValue
	cur := begin
	for j := i; j < n; j++ {
		e := &sc.entries[j]
		if bytes.Compare(e.begin, cur) > 0 {
			return nil, false // gap
		}
		// Collect KVs in [max(begin, e.begin), min(end, e.end))
		for _, kv := range e.kvs {
			if bytes.Compare(kv.Key, begin) >= 0 && bytes.Compare(kv.Key, end) < 0 {
				result = append(result, kv)
			}
		}
		cur = e.end
		if bytes.Compare(cur, end) >= 0 {
			return result, true // fully covered
		}
	}
	return nil, false // end not reached
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
