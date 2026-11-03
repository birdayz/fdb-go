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
//
// entries is a pseq (C++ keeps them in an IndexedSet, SnapshotCache.h:143), so a
// fetch inserts its fragment in O(log n) and the walk's iterator steps in O(1).
type snapshotCache struct {
	entries pseq[cacheEntry]
	// node0 holds the first entries' nodes, so the common 1-2 point read path
	// allocates none; nodesLent records that entries may hand them out.
	node0     [2]pnode[cacheEntry]
	nodesLent bool
	// snapshotted records that a walk took a snapshot (snapshot): reset then
	// leaves the inline storage to it.
	snapshotted bool
	kv0         [2]KeyValue
	keyBuf      [64]byte
	kvUsed      int
	keyBufUsed  int

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
	sc.entries = pseq[cacheEntry]{}
	if sc.snapshotted {
		// A walk may still read a snapshot holding the inline nodes, keys and
		// rows: they are never handed out again.
		sc.nodesLent, sc.kvUsed, sc.keyBufUsed = true, len(sc.kv0), len(sc.keyBuf)
		return
	}
	sc.node0 = [2]pnode[cacheEntry]{}
	sc.nodesLent = false
	sc.kv0 = [2]KeyValue{}
	sc.kvUsed = 0
	sc.keyBufUsed = 0
}

// insertPoint caches [key, key+\x00) in inline storage when capacity allows,
// avoiding per-Get slice/row allocations on the common 1-2 point read path.
func (sc *snapshotCache) insertPoint(key, value []byte) {
	need := len(key) + 1
	var buf []byte
	if sc.keyBufUsed+need <= len(sc.keyBuf) {
		buf = sc.keyBuf[sc.keyBufUsed : sc.keyBufUsed+need : sc.keyBufUsed+need]
		sc.keyBufUsed += need
	} else {
		buf = make([]byte, need)
	}
	copy(buf, key)
	buf[len(key)] = 0
	k := buf[:len(key):len(key)]
	end := buf

	var rows []KeyValue
	if value != nil {
		if sc.kvUsed < len(sc.kv0) {
			rows = sc.kv0[sc.kvUsed : sc.kvUsed+1 : sc.kvUsed+1]
			sc.kvUsed++
			rows[0] = KeyValue{Key: k, Value: value}
		} else {
			rows = []KeyValue{{Key: k, Value: value}}
		}
	}
	if sc.entries.Len() == 0 {
		sc.lendNodes()
		sc.entries.insert(0, cacheEntry{begin: k, end: end, kvs: rows})
		return
	}
	sc.insert(k, end, rows)
}

// lendNodes gives entries the inline nodes, once per reset.
func (sc *snapshotCache) lendNodes() {
	if !sc.nodesLent {
		sc.entries.free = sc.node0[:]
		sc.nodesLent = true
	}
}

// entryList returns the entries in order.
func (sc *snapshotCache) entryList() []cacheEntry { return sc.entries.appendTo(nil) }

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
	sc.insertKVs(begin, end, kvs, false)
}

// insertOwned is insert that keeps begin, end and kvs, which nothing writes,
// instead of copying them: the walk prepares them before it takes c.mu.
func (sc *snapshotCache) insertOwned(begin, end []byte, kvs []KeyValue) {
	sc.insertKVs(begin, end, kvs, true)
}

func (sc *snapshotCache) insertKVs(begin, end []byte, kvs []KeyValue, owned bool) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}

	newBegin := begin
	newEnd := end
	es := &sc.entries
	sc.lendNodes()

	// Trim the front past an existing entry that STRICTLY contains newBegin.
	// C++: `begin = itb.it->endKey`, guarded by `!itb.is_unknown_range() &&
	// itb.it->beginKey != keys.begin`. An entry starting exactly at newBegin is
	// deliberately not trimmed past — it is superseded below instead. An entry
	// ending exactly at newBegin does not contain it (end is exclusive), which
	// is the adjacent-page case and must not trim either.
	if _, e := es.find(func(e *cacheEntry) bool {
		return bytes.Compare(e.end, newBegin) > 0
	}); e != nil && bytes.Compare(e.begin, newBegin) < 0 {
		newBegin = e.end
	}

	// Trim the back to the start of an existing entry that STRICTLY contains
	// newEnd. C++: `ite._prevUnknown(); end = ite.endKey()`, reached only when
	// the segment holding keys.end is not an unknown range — an entry ending
	// exactly at newEnd does not hold it, so it must not trim.
	if j := es.search(func(e *cacheEntry) bool {
		return bytes.Compare(e.begin, newEnd) >= 0
	}); j > 0 && bytes.Compare(es.at(j-1).end, newEnd) > 0 {
		newEnd = es.at(j - 1).begin
	}

	if bytes.Compare(newBegin, newEnd) >= 0 {
		return // fully known already — C++'s `if (begin < end)` guard
	}

	// After trimming, no surviving entry partially overlaps [newBegin, newEnd):
	// the only entry that could straddle either end was what the trim moved that
	// end to. So [lo, hi) is exactly the set of entries strictly inside, which
	// this fetch supersedes wholesale (same read version, so same contents).
	lo := es.search(func(e *cacheEntry) bool {
		return bytes.Compare(e.begin, newBegin) >= 0
	})
	hi := es.search(func(e *cacheEntry) bool {
		return bytes.Compare(e.end, newEnd) > 0
	})

	// Keep only the values that fall inside the trimmed range. kvs is sorted, so
	// this is a slice of the caller's page, never a merge with cached values.
	lok := sort.Search(len(kvs), func(k int) bool {
		return bytes.Compare(kvs[k].Key, newBegin) >= 0
	})
	hik := sort.Search(len(kvs), func(k int) bool {
		return bytes.Compare(kvs[k].Key, newEnd) >= 0
	})

	fresh := cacheEntry{begin: newBegin, end: newEnd, kvs: kvs[lok:hik:hik]}
	if !owned {
		fresh = cacheEntry{
			begin: append([]byte(nil), newBegin...),
			end:   append([]byte(nil), newEnd...),
			kvs:   copyKVs(kvs[lok:hik]),
		}
	}

	switch hi - lo {
	case 0:
		es.insert(lo, fresh)
	case 1:
		es.set(lo, fresh)
	default:
		es.delete(lo, hi)
		es.insert(lo, fresh)
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
	if sc.entries.Len() == 0 || bytes.Compare(begin, end) >= 0 {
		return nil, false
	}
	if reverse {
		return sc.getRangeKVsBack(begin, end, budget)
	}

	// Find the first entry whose end > begin (could contain begin).
	i, first := sc.entries.find(func(e *cacheEntry) bool {
		return bytes.Compare(e.end, begin) > 0
	})

	if first == nil || bytes.Compare(first.begin, begin) > 0 {
		return nil, false // begin is not covered
	}

	// Walk entries ascending, collecting KVs, checking for gaps.
	var result []KeyValue
	cur := begin
	for it := sc.entries.cursor(i); it.valid(); it.next() {
		e := it.item()
		if bytes.Compare(e.begin, cur) > 0 {
			return nil, false // gap
		}
		// Seek to the first key at or after begin rather than scanning the whole
		// entry: with one fragment per fetch, a linear scan per entry is what made
		// the walk cost the tail rather than the page.
		lo := sort.Search(len(e.kvs), func(k int) bool {
			return bytes.Compare(e.kvs[k].Key, begin) >= 0
		})
		if result == nil && lo < len(e.kvs) {
			// Usually one fragment answers the walk: size the result for it.
			size := len(e.kvs) - lo
			if budget > 0 && budget < size {
				size = budget
			}
			result = make([]KeyValue, 0, size)
		}
		for k := lo; k < len(e.kvs); k++ {
			kv := e.kvs[k]
			if bytes.Compare(kv.Key, end) >= 0 {
				break
			}
			if sc.keyBufUsed > 0 && len(e.kvs) == 1 {
				kv.Key = bytes.Clone(kv.Key)
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
	// Last entry that can hold a key below end.
	j := sc.entries.search(func(e *cacheEntry) bool {
		return bytes.Compare(e.begin, end) >= 0
	}) - 1
	if j < 0 {
		return nil, false
	}

	var desc []KeyValue
	cur := end // everything at or above cur is accounted for
	for it := sc.entries.cursor(j); it.valid(); it.prev() {
		e := it.item()
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
			if sc.keyBufUsed > 0 && len(e.kvs) == 1 {
				kv.Key = bytes.Clone(kv.Key)
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

// snapshot returns the entries as they are now, for a walk that reads them
// without rywCache.mu: later inserts copy what they change.
func (sc *snapshotCache) snapshot() pseq[cacheEntry] {
	sc.snapshotted = true
	return sc.entries.share()
}

// getKey checks if a single key's server state is cached. Returns (value, true)
// if the key falls within a known range. value is nil if the key doesn't exist
// at the server. Returns (nil, false) if the key's range is unknown.
func (sc *snapshotCache) getKey(key []byte) ([]byte, bool) { return cacheGetKey(&sc.entries, key) }

// cacheGetKey is getKey over entries or a snapshot of them.
func cacheGetKey(entries *pseq[cacheEntry], key []byte) ([]byte, bool) {
	// Find the entry containing key: the first one ending after key, when it
	// begins at or before key (entries are sorted and non-overlapping).
	_, e := entries.find(func(e *cacheEntry) bool {
		return bytes.Compare(e.end, key) > 0
	})
	if e == nil || bytes.Compare(e.begin, key) > 0 {
		return nil, false
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

// skipKnownEmpty narrows [begin, end) past the known ranges holding no key at its scan
// origin: begin moves up forward, end moves down in reverse.
func (sc *snapshotCache) skipKnownEmpty(begin, end []byte, reverse bool) ([]byte, []byte) {
	origBegin, origEnd := begin, end
	es := &sc.entries
	for bytes.Compare(begin, end) < 0 {
		if reverse {
			// The entry holding the keys just below end.
			j := es.search(func(e *cacheEntry) bool { return bytes.Compare(e.begin, end) >= 0 }) - 1
			if j < 0 || bytes.Compare(es.at(j).end, end) < 0 {
				break
			}
			e := es.at(j)
			k := sort.Search(len(e.kvs), func(k int) bool { return bytes.Compare(e.kvs[k].Key, end) >= 0 })
			if k > 0 && bytes.Compare(e.kvs[k-1].Key, begin) >= 0 {
				break
			}
			end = e.begin
			continue
		}
		_, e := es.find(func(e *cacheEntry) bool { return bytes.Compare(e.end, begin) > 0 })
		if e == nil || bytes.Compare(e.begin, begin) > 0 {
			break
		}
		k := sort.Search(len(e.kvs), func(k int) bool { return bytes.Compare(e.kvs[k].Key, begin) >= 0 })
		if k < len(e.kvs) && bytes.Compare(e.kvs[k].Key, end) < 0 {
			break
		}
		begin = e.end
	}
	if sc.keyBufUsed > 0 {
		if len(begin) != len(origBegin) || (len(begin) > 0 && &begin[0] != &origBegin[0]) {
			begin = bytes.Clone(begin)
		}
		if len(end) != len(origEnd) || (len(end) > 0 && &end[0] != &origEnd[0]) {
			end = bytes.Clone(end)
		}
	}
	return begin, end
}

// knownPast reports whether the cache knows the position adjacent to key in scan
// direction: keyAfter(key) forward, the keys just below key in reverse.
func (sc *snapshotCache) knownPast(key []byte, reverse bool) bool {
	es := &sc.entries
	if reverse {
		i := es.search(func(e *cacheEntry) bool { return bytes.Compare(e.begin, key) >= 0 })
		return i > 0 && bytes.Compare(es.at(i-1).end, key) >= 0
	}
	i := es.search(func(e *cacheEntry) bool { return bytes.Compare(e.begin, key) > 0 })
	if i > 0 && bytes.Compare(es.at(i-1).end, key) > 0 && !isKeyAfter(es.at(i-1).end, key) {
		return true
	}
	return i < es.Len() && isKeyAfter(es.at(i).begin, key)
}

// isKeyAfter reports a == keyAfter(b) without allocating.
func isKeyAfter(a, b []byte) bool {
	return len(a) == len(b)+1 && a[len(b)] == 0 && bytes.Equal(a[:len(b)], b)
}

// cloneReadKVs separates public results from immutable RYW/cache storage.
func cloneReadKVs(kvs []KeyValue) []KeyValue {
	if len(kvs) == 0 {
		return kvs
	}
	return cloneReadKVsInto(make([]KeyValue, len(kvs)), kvs)
}

// cloneReadKVsInPlace is cloneReadKVs for a slice its caller alone holds (its
// rows may alias shared storage, the slice itself must not be shared): each
// row's bytes are replaced by the copy, sparing a second slice.
func cloneReadKVsInPlace(kvs []KeyValue) []KeyValue {
	if len(kvs) == 0 {
		return kvs
	}
	return cloneReadKVsInto(kvs, kvs)
}

// cloneReadKVsInto writes into out, len(kvs) long, the rows of kvs with their
// bytes copied into one arena; out may be kvs itself.
func cloneReadKVsInto(out, kvs []KeyValue) []KeyValue {
	size := 0
	for _, kv := range kvs {
		size += len(kv.Key) + len(kv.Value)
	}
	arena := make([]byte, size)
	clone := func(src []byte) []byte {
		if src == nil {
			return nil
		}
		dst := arena[:len(src):len(src)]
		copy(dst, src)
		arena = arena[len(src):]
		return dst
	}
	for i, kv := range kvs {
		out[i] = KeyValue{Key: clone(kv.Key), Value: clone(kv.Value)}
	}
	return out
}

// copyKVs retains immutable internal bytes. Public read boundaries copy them
// before handing results to callers, and FDB response buffers are not pooled.
func copyKVs(kvs []KeyValue) []KeyValue {
	if len(kvs) == 0 {
		return nil
	}
	out := make([]KeyValue, len(kvs))
	copy(out, kvs)
	return out
}
