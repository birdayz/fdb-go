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
// ascending by key). Merges with overlapping or adjacent existing entries.
func (sc *snapshotCache) insert(begin, end []byte, kvs []KeyValue) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}

	n := len(sc.entries)
	if n == 0 {
		sc.entries = []cacheEntry{{
			begin: append([]byte(nil), begin...),
			end:   append([]byte(nil), end...),
			kvs:   copyKVs(kvs),
		}}
		return
	}

	// Find overlapping/adjacent entries. An entry e overlaps or is adjacent
	// to [begin, end) if e.end >= begin && e.begin <= end.
	loIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].end, begin) >= 0
	})
	hiIdx := sort.Search(n, func(i int) bool {
		return bytes.Compare(sc.entries[i].begin, end) > 0
	})

	// [loIdx, hiIdx) are entries that overlap or are adjacent.
	newBegin := append([]byte(nil), begin...)
	newEnd := append([]byte(nil), end...)

	// Collect all KVs from overlapping entries + new KVs, then merge.
	var allKVs []KeyValue
	for i := loIdx; i < hiIdx; i++ {
		e := &sc.entries[i]
		if bytes.Compare(e.begin, newBegin) < 0 {
			newBegin = e.begin // no copy needed, we're replacing
		}
		if bytes.Compare(e.end, newEnd) > 0 {
			newEnd = e.end
		}
		allKVs = append(allKVs, e.kvs...)
	}
	allKVs = append(allKVs, kvs...)

	// Sort and deduplicate. New KVs (from the latest fetch) win on
	// duplicates, but at a fixed read version they should be identical.
	sort.Slice(allKVs, func(i, j int) bool {
		return bytes.Compare(allKVs[i].Key, allKVs[j].Key) < 0
	})
	if len(allKVs) > 1 {
		j := 0
		for i := 1; i < len(allKVs); i++ {
			if !bytes.Equal(allKVs[i].Key, allKVs[j].Key) {
				j++
				allKVs[j] = allKVs[i]
			}
		}
		allKVs = allKVs[:j+1]
	}

	// Replace [loIdx, hiIdx) with the merged entry.
	merged := cacheEntry{begin: newBegin, end: newEnd, kvs: allKVs}
	overlapCount := hiIdx - loIdx
	if overlapCount == 0 {
		sc.entries = append(sc.entries, cacheEntry{})
		copy(sc.entries[loIdx+1:], sc.entries[loIdx:])
		sc.entries[loIdx] = merged
	} else if overlapCount == 1 {
		sc.entries[loIdx] = merged
	} else {
		sc.entries[loIdx] = merged
		sc.entries = append(sc.entries[:loIdx+1], sc.entries[hiIdx:]...)
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
