package client

import (
	"bytes"
	"sync"
	"unsafe"
)

// rywCache implements a read-your-writes cache that intercepts reads and
// merges them with pending writes from the same transaction. It sits between
// the public Transaction API and the wire-level read functions.
//
// Key invariant: writes entries take precedence over cleared ranges. If a key
// was ClearRange'd then Set, the Set wins.
type rywCache struct {
	mu sync.Mutex

	// w is the live version of the write map (RFC-262 D1). Reads that outlive one
	// critical section pin the version they were issued against; a writer clones a
	// pinned version before mutating (mutableWritesLocked). Nothing ever installs
	// another version here except reset.
	w *rywWrites
	// base is the first version, so a transaction that never pins one allocates none.
	base rywWrites

	// gen counts resets. A walk records it at issue and drops a storage reply that
	// arrives after a reset, which C++ prevents by killing in-flight reads
	// (resetPromise, ReadYourWrites.actor.cpp:358-364).
	gen uint64

	// nextVer stamps mutable write-map lineages so coalesceOverAtomics (inPlace)
	// can update an unshared atomic stack in place.
	nextVer uint64

	hooks *rywCacheTestHooks

	// serverCache caches server-side state at the read version, avoiding
	// redundant server round-trips for repeated reads of the same range.
	// Matches C++'s SnapshotCache. Not invalidated by local writes/clears.
	serverCache snapshotCache

	// arena holds what the write map keeps (keepLocked).
	arena []byte
}

// onPin and onClone are test seams (S4): every pin/unpin and every clone.
type rywCacheTestHooks struct {
	onPin   func(w *rywWrites, pin bool)
	onClone func(from, to *rywWrites)
}

func (c *rywCache) testHooks() *rywCacheTestHooks {
	if c.hooks == nil {
		c.hooks = &rywCacheTestHooks{}
	}
	return c.hooks
}

// rywEntry represents a pending write for a single key.
//
// Op-type preservation (RFC-058) — mirrors the C++ WriteMap segment a key would occupy.
// The eager value-fold (a resolved atomic stored as a plain entry) is correct for VALUES
// but C++ classifies a key by its OPERATION, which two consumers need:
//   - getKey offset walk counts by SEGMENT TYPE (is_kv), not by resolved value.
//   - updateConflictMap records a read-conflict only for DEPENDENT writes + UNMODIFIED
//     ranges, skipping INDEPENDENT writes + cleared ranges.
//
// Two flags capture exactly the C++ predicates without abandoning the value-fold:
type rywEntry struct {
	value []byte
	// absent: the resolved value is "no value" (a matched CompareAndClear) — C++
	// RYWMutation(Optional<ValueRef>(), SetValue). The key is STILL an is_kv slot for
	// getKey (counted in the offset walk, a "phantom" slot), but Get/GetRange skip it.
	// Distinct from value==nil ("Set to empty bytes", a present key).
	absent bool
	// dependent: a DEPENDENT_WRITE — a standalone atomic resolved against a DB base
	// (C++ stack bottom is an atomic, isDependent()==true). Set ONCE at the entry's build
	// and carried UNCHANGED through later folds (C++ isDependent() reads singletonOperation,
	// the stack bottom, which coalesceOver never mutates — WriteMap.cpp:480). Drives
	// conflict filtering. A plain Set, a fold over a Set, and an atomic over a cleared base
	// are all INDEPENDENT (dependent==false). Unused while hasAtomics (independence of an
	// unresolved chain is derived from whether it contains a versionstamp — see
	// isDependentLocked).
	dependent bool
	// If true, this entry has pending atomic mutations instead of a plain Set.
	hasAtomics bool
	// valueOwned is the owned length of value's backing array (appendIfFitsOwned).
	valueOwned int32
	atomics    []rywMutation
	atomicsVer uint64
	// unreadable: a versionstamped op landed on this key, making it UNREADABLE for
	// the transaction's lifetime — reads reaching it throw accessed_unreadable (1036)
	// unless bypassed. STICKY, mirroring C++ WriteMap.cpp:97
	// (`is_unreadable = it.is_unreadable() || op == SetVersionstampedValue/Key`):
	// set when a versionstamped op lands, PRESERVED by later plain Sets/atomics (the
	// exact-key SetValue stack-replace fast path is gated `!it.is_unreadable()` at
	// :125; on an unreadable entry the Set is pushed and the flag stays at :141), and
	// removed only by clear() (cleared entries are readable — you know they're empty).
	unreadable bool
	// nodeConflict: an atomic set the key's node is_conflict (WriteMap.cpp:95); false is unknown.
	// Clearing the key or an SVK range over it undoes it; atomicWith skips mutates it makes no-ops.
	nodeConflict bool
}

// isDependentLocked reports whether this entry is a C++ DEPENDENT_WRITE (is_independent()
// false) for conflict-map purposes. A resolved entry uses its build-time dependent flag; an
// unresolved chain is INDEPENDENT iff it contains a versionstamp (unreadable — C++ marks
// SetVersionstamped* independent), else DEPENDENT. Caller holds c.mu.
//
// Invariant (RFC-058 "Consistency"): a dependent==true entry is never cleared-based, because
// an atomic over a cleared range is folded at the independent site (atomic() :!exists &&
// isClearedLocked) with dependent=false. So !isDependentLocked() subsumes C++ is_independent's
// `following_keys_cleared` disjunct for operation keys; pure cleared ranges (no operation key)
// are classified separately via the cleared list.
func (e *rywEntry) isDependentLocked() bool {
	if e.hasAtomics {
		for _, m := range e.atomics {
			if isUnresolvedVersionstamp(m.typ) {
				return false
			}
		}
		return true
	}
	return e.dependent
}

// rywMutation represents a single atomic mutation.
type rywMutation struct {
	typ MutationType
	// owned is the owned length of param's backing array (appendIfFitsOwned).
	owned int32
	param []byte
}

// rywRange represents a cleared range [begin, end).
type rywRange struct {
	begin []byte
	end   []byte
}

// keepLocked returns n zero bytes the write map keeps, as RYW copies into its arena (RYW:2400, 2498):
// bytes are handed out once and never rewritten, so holders keep them past a reset. Never nil; c.mu held.
func (c *rywCache) keepLocked(n int) []byte {
	switch {
	case n == 0:
		return []byte{}
	case n > keepChunkMax/4:
		return make([]byte, n)
	case cap(c.arena)-len(c.arena) < n:
		c.arena = make([]byte, 0, max(min(2*cap(c.arena), keepChunkMax), 512, n))
	}
	s := c.arena[len(c.arena) : len(c.arena)+n : len(c.arena)+n]
	c.arena = c.arena[:len(c.arena)+n]
	return s
}

// keepKeyAfter returns keyAfter(key) kept in the arena; its prefix is key.
func (c *rywCache) keepKeyAfter(key []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keepKeyAfterLocked(key)
}

// keepKeyAfterLocked is keepKeyAfter. Caller holds c.mu.
func (c *rywCache) keepKeyAfterLocked(key []byte) []byte {
	s := c.keepLocked(len(key) + 1)
	copy(s, key)
	return s
}

// keepPairLocked copies begin and end into one arena block; begin is capped. Caller holds c.mu.
func (c *rywCache) keepPairLocked(begin, end []byte) (b, e []byte) {
	buf := c.keepLocked(len(begin) + len(end))
	copy(buf, begin)
	copy(buf[len(begin):], end)
	return buf[:len(begin):len(begin)], buf[len(begin):]
}

// keepChunkMax caps the write map's arena chunks.
const keepChunkMax = 32 << 10

// keepCopyLocked is keepLocked holding a copy of b. Caller holds c.mu.
func (c *rywCache) keepCopyLocked(b []byte) []byte {
	s := c.keepLocked(len(b))
	copy(s, b)
	return s
}

// keepRangeLocked is nodeClip with both bounds copied into one arena block. Caller holds c.mu.
func (c *rywCache) keepRangeLocked(begin, end []byte) (b, e []byte, ok bool) {
	if begin, end, ok = nodeClip(begin, end); !ok {
		return nil, nil, false
	}
	b, e = c.keepPairLocked(begin, end)
	return b, e, true
}

// isEmpty reports whether the RYW layer holds no PENDING WRITE state (writes/cleared) and no
// cached reads (serverCache). It is the write-side half of the C++ `writes.empty() &&
// cache.empty()` poison predicate; the READ side is tracked separately by Transaction.hadRead
// (set at every read choke: getValue/getRange/getKey/GetPipelined), because the facade's Get
// uses GetPipelined which does not populate serverCache. Together (`hadRead || !isEmpty()`)
// they are the Go analog of `reading.getFutureCount()>0 || !cache.empty() || !writes.empty()`.
// Used by SetReadYourWritesDisable to decide whether to poison the transaction (RFC-059).
func (c *rywCache) isEmpty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writesEmptyLocked() && c.serverCache.entries.Len() == 0
}

// writesEmptyLocked is WriteMap::empty(): false once anything, even a conflict or unreadable range,
// reached the write map (WriteMap.cpp:88, 161, 206, 245). Caller holds c.mu.
func (c *rywCache) writesEmptyLocked() bool {
	w := c.liveLocked()
	if w.numWrites() != 0 || w.cleared.Len() != 0 || len(w.nodes.log) != 0 {
		return false
	}
	if !w.nodes.built {
		return true
	}
	if w.nodes.seq.Len() != 3 {
		return false
	}
	// A full-keyspace conflict can flag the existing sentinels without adding
	// a boundary (WriteMap.cpp:245-294); their count alone is not emptiness.
	for i := 0; i < 3; i++ {
		n := w.nodes.seq.at(i)
		if n.op || n.followingCleared || n.followingConflict || n.isConflict || n.followingUnreadable || n.isUnreadable {
			return false
		}
	}
	return true
}

// reset clears all cached state.
func (c *rywCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A fresh version: reads still holding the old one keep it until they unpin.
	// BYPASS_UNREADABLE is not a persistent option in C++ (fdb.options has no
	// persistent attribute for it), so resetRyow's options.reset clears it on every
	// retry/reset — the fresh version starts with it false.
	c.resetWritesLocked()
	c.gen++
	c.serverCache.reset()
}

// set records a Set operation.
func (c *rywCache) set(key, value []byte) { c.setWith(key, value, true) }

// setWith is RYW set: WriteMap::mutate(key, SetValue, value, addConflict)
// (ReadYourWrites.actor.cpp:2353). It returns the bytes it keeps, which the op-log shares.
func (c *rywCache) setWith(key, value []byte, addConflict bool) (keptKey, keptValue []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	// The arena never rewrites or regrows what it handed out (keepLocked).
	copied := c.keepCopyLocked(value)
	// Preserve the previous entry's unreadable flag: a plain Set over a key with a
	// pending versionstamped op keeps it unreadable (sticky — C++ WriteMap.cpp:125
	// gates the stack-replacing SetValue fast path on !it.is_unreadable(); on an
	// unreadable entry the Set is pushed and :141 keeps the flag).
	kb := c.keepCopyLocked(key) // one copy serves the write map and the nodes
	k := unsafe.String(unsafe.SliceData(kb), len(kb))
	idx, node, prev, exists := w.lookupForWrite(k)
	if exists && node != nil {
		k = node.item.key
	}
	mutate := nodeKey(key) && !(exists && (!addConflict || prev.nodeConflict) && !w.isUnreadable(key))
	if mutate {
		w.nodes.mutate(stringBytes(k), addConflict, false)
	}
	w.putEntryAt(idx, node, k, rywEntry{
		value:        copied,
		unreadable:   prev.unreadable,
		nodeConflict: prev.nodeConflict || (addConflict && nodeKey(key)),
	})
	keptKey, keptValue = kb, copied
	// A Set after ClearRange wins — no need to remove from cleared, because
	// get() checks writes before cleared.
	return keptKey, keptValue
}

// clear records a Clear operation (single key).
func (c *rywCache) clear(key []byte) { c.clearWith(key, true) }

// clearWith is RYW clear(key): WriteMap::clear([key, keyAfter(key)), addConflict)
// (ReadYourWrites.actor.cpp:2435). It returns the kept keyAfter(key), which the op-log shares.
func (c *rywCache) clearWith(key []byte, addConflict bool) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	// One kept copy of key\x00 serves the nodes and the cleared range.
	end := c.keepKeyAfterLocked(key)
	key = end[:len(key):len(key)]
	if b, e, ok := nodeClip(key, end); ok {
		w.nodes.clear(b, e, addConflict)
	}
	// Remove from writes.
	if e, existed := w.deleteEntry(unsafe.String(unsafe.SliceData(key), len(key))); existed && e.unreadable {
		w.removeUnreadableKey(string(key))
	}
	// Add [key, key+\x00) to cleared.
	w.addClearedRangeOwned(key, end)
	// Cleared = readable (you know the key is empty): subtract the span from the
	// SVK unreadable ranges. C++ gets this free from the shared PTree — clear()
	// inserts readable cleared entries over the span (WriteMap.cpp:195).
	subtractRange(&w.unreadableRanges, key, end)
	return end
}

// clearRange records a ClearRange [begin, end).
func (c *rywCache) clearRange(begin, end []byte) { c.clearRangeWith(begin, end, true) }

// clearRangeWith is RYW clear(range): WriteMap::clear(range, addConflict)
// (ReadYourWrites.actor.cpp:2402). It returns the kept bounds, which the op-log shares.
func (c *rywCache) clearRangeWith(begin, end []byte, addConflict bool) (keptBegin, keptEnd []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	begin, end = c.keepPairLocked(begin, end) // one kept copy for the nodes and the cleared range
	if b, e, ok := nodeClip(begin, end); ok {
		w.nodes.clear(b, e, addConflict)
	}
	// Search the sorted store rather than scanning unaffected entries.
	if w.numWrites() > 0 {
		w.ensureSorted()
		wStart := w.writeIndexOf(begin)
		wEnd := w.writeIndexOf(end)
		if wStart < wEnd {
			for cur := w.sorted.finger(wStart); cur.idx < wEnd; cur.next() {
				if x := cur.item(); x.e.unreadable {
					w.removeUnreadableKey(x.key)
				}
			}
			w.sorted.delete(wStart, wEnd)
		}
	}
	w.addClearedRangeOwned(begin, end)
	// Cleared = readable: subtract the span from the SVK unreadable ranges (see clear).
	subtractRange(&w.unreadableRanges, begin, end)
	return begin, end
}

// atomic records an atomic mutation.
func (c *rywCache) atomic(op MutationType, key, param []byte) {
	c.atomicWith(op, key, param, op != MutSetVersionstampedKey)
}

// atomicWith is RYW atomicOp: WriteMap::mutate(key, op, param, addConflict)
// (ReadYourWrites.actor.cpp:2295); a versionstamped op makes the node unreadable
// (WriteMap.cpp:97).
func (c *rywCache) atomicWith(op MutationType, key, param []byte, addConflict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	// A view of the caller's key for lookups; copied only where the map retains it.
	k := unsafe.String(unsafe.SliceData(key), len(key))
	idx, node, entry, exists := w.lookupForWrite(k)
	if exists && node != nil {
		k = node.item.key
	}
	versionstamp := isUnresolvedVersionstamp(op)
	// On an operation key mutate only ORs both flags in (WriteMap.cpp:95-98, 139-156); an SVK
	// unreadable range drops the key's node but keeps its entry, so there it always applies.
	mutate := nodeKey(key) && !(exists && (!addConflict || entry.nodeConflict) && (!versionstamp || entry.unreadable) &&
		!w.isUnreadable(key))
	if !(exists && node != nil) && (mutate || !exists || !w.isSorted || (versionstamp && !entry.unreadable)) {
		kb := c.keepCopyLocked(key)
		k = unsafe.String(unsafe.SliceData(kb), len(kb))
	}
	if mutate {
		w.nodes.mutate(stringBytes(k), addConflict, versionstamp) // WriteMap.cpp:87-158
	}
	entry.nodeConflict = entry.nodeConflict || (addConflict && nodeKey(key))
	// Versionstamped mutations can NEVER be folded into a plain entry: the 10-byte
	// stamp is assigned at commit, so the resolved key/value is unknown client-side.
	// Eagerly resolving one (the two branches below) would store a plain rywEntry
	// whose value is the unstamped operand — surfacing a phantom present key in
	// pre-commit Get/GetRange, whatever the base state (storage-absent, locally
	// cleared, or a pending plain Set). Always record it as an UNRESOLVED atomic so
	// every read path hits the unreadable gate and throws accessed_unreadable
	// (1036), matching C++ is_unreadable (RFC-098) — unless BYPASS_UNREADABLE
	// resolves the operand as written. The mutation still commits via tx.mutations.
	if !isUnresolvedVersionstamp(op) {
		if exists && !entry.hasAtomics {
			// Site B: fold over an existing resolved entry (plain Set or a prior fold). C++
			// coalesces the op onto the stack TOP; the bottom (dependence) is unchanged, so
			// keep entry.dependent. A matched CompareAndClear → a phantom is_kv slot
			// (absent), NOT a cleared range (the key stays an operation in the write-map).
			if op == MutAppendIfFits {
				// C++ coalesce SetValue+AppendIfFits → SetValue(doAppendIfFits) (WriteMap.cpp:388-391).
				entry.value, entry.valueOwned = appendIfFitsOwned(entry.value, entry.valueOwned, param)
				if entry.value == nil {
					entry.value = []byte{} // present-empty, as below
				}
				entry.absent = false
				w.putEntryAt(idx, node, k, entry)
				return
			}
			val, clr := applyAtomic(op, entry.value, param)
			if !clr && val == nil {
				// A non-clear op that resolved to EMPTY (e.g. doMax(_, "")) is present-empty,
				// NOT absent. Normalize nil→[]byte{} (C++ Optional present) so a LATER fold
				// over this entry reads it as present — matching resolveAtomics. Without this,
				// a subsequent CompareAndClear/V2 op misreads the nil base as absent.
				val = []byte{}
			}
			entry.value = val
			entry.valueOwned = 0
			entry.absent = clr
			w.putEntryAt(idx, node, k, entry)
			return
		}
		if !exists && w.isCleared(key) {
			// Site C: atomic over a locally-cleared range. C++ pushes a synthetic
			// SetValue(empty) at the stack bottom (WriteMap.cpp:102-111) → INDEPENDENT
			// (dependent=false). A CompareAndClear over the empty base → phantom (absent);
			// other atomics → a present resolved value.
			val, clr := applyAtomic(op, nil, param)
			if !clr && val == nil {
				val = []byte{} // present-empty, not absent (see site B).
			}
			w.putEntryAt(idx, node, k, rywEntry{value: val, absent: clr, nodeConflict: entry.nodeConflict})
			return
		}
	}
	entry.hasAtomics = true
	// C++ WriteMap.cpp:97: is_unreadable = it.is_unreadable() || op is a
	// versionstamped mutation. Sticky: once set it survives every later mutation on
	// the key (only clear() removes it, by deleting the entry).
	if !entry.unreadable && isUnresolvedVersionstamp(op) {
		entry.unreadable = true
		w.insertUnreadableKey(k)
	}
	// An entry with atomics carries no plain base — the base comes from storage at
	// read time. Drop any prior plain value (e.g. a pending Set superseded by a
	// versionstamped op) so a reader never mistakes it for a resolved value.
	entry.value = nil
	entry.valueOwned = 0
	newMut := rywMutation{typ: op, param: param}
	if !foldsOnto(entry.atomics, newMut) {
		// Kept as C++ RYW copies it (ReadYourWrites.actor.cpp:2260); a fold copies anyway.
		newMut.param = c.keepCopyLocked(param)
	}
	// Coalesce onto the pending chain (C++ WriteMap::coalesceOver at insert time, WriteMap.cpp:106/144):
	// repeated same-type resolvable atomics fold to one op, so a 150k-`ADD 1` chain on an unread key ships
	// one mutation, not 150k (the RFC-172 / #28 fix). Read-transparent; versionstamps/CompareAndClear push.
	inPlace := w.ver != 0 && entry.atomicsVer == w.ver
	entry.atomics = coalesceOverAtomics(entry.atomics, newMut, inPlace)
	entry.atomicsVer = w.ver
	w.putEntryAt(idx, node, k, entry)
}

// isCleared returns true if key falls within any cleared range (acquires lock).
func (c *rywCache) isCleared(key []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked().isCleared(key)
}

// addClearedRange adds [begin, end) to the cleared list, merging overlapping
// and adjacent ranges to keep the list sorted and non-overlapping.
func (c *rywCache) addClearedRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	addRange(&c.mutableWritesLocked().cleared, begin, end)
}

// addUnreadableRange marks [begin, end) unreadable (SVK candidate stamp
// range, RFC-098).
func (c *rywCache) addUnreadableRange(begin, end []byte) { c.addUnreadableRangeKeeping(begin, end) }

// addUnreadableRangeKeeping returns the conflict runs it removes from the nodes: unlike C++
// (WriteMap.cpp:205-242) Go keeps the span's writes (RFC-098), so the caller keeps their conflicts.
func (c *rywCache) addUnreadableRangeKeeping(begin, end []byte) (kept []KeyRange) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	if b, e, ok := c.keepRangeLocked(begin, end); ok {
		w.nodes.build()
		var a boundaryArena
		kept = w.nodes.appendWriteConflictsIn(nil, &a, b, e)
		w.nodes.addUnmodifiedAndUnreadableRange(b, e) // RYW:2272
	}
	w.addUnreadableRange(begin, end)
	return kept
}

// nodeKey reports a key inside the keyspace the write map's nodes cover. C++
// rejects every write and conflict range past getMaxWriteKey before it reaches
// the write map (RYW:2353, 2466-2468); Go still buffers an illegal Set or Clear (its
// error deferred at the call), so the nodes skip what lies at or past the \xff\xff sentinel.
func nodeKey(key []byte) bool { return bytes.Compare(key, allKeysEnd) < 0 }

// nodeClip is [begin, end) clipped to the nodes' keyspace; ok is false when
// nothing remains. The bounds are the caller's, or allKeysEnd.
func nodeClip(begin, end []byte) (b, e []byte, ok bool) {
	if bytes.Compare(end, allKeysEnd) > 0 {
		end = allKeysEnd
	}
	return begin, end, bytes.Compare(begin, end) < 0
}

// addConflictRange is RYW addWriteConflictRange with RYW enabled: conflict-only
// nodes in the write map, no operation (ReadYourWrites.actor.cpp:2493-2499,
// WriteMap.cpp:244-302).
func (c *rywCache) addConflictRange(begin, end []byte) { c.addConflictRangeOwned(begin, end, false) }

// addConflictRangeOwned is addConflictRange; owned bounds are never written and
// the nodes keep them without a copy.
func (c *rywCache) addConflictRangeOwned(begin, end []byte, owned bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owned {
		if b, e, ok := nodeClip(begin, end); ok {
			c.mutableWritesLocked().nodes.addConflictRange(b, e)
		}
		return
	}
	if b, e, ok := c.keepRangeLocked(begin, end); ok {
		c.mutableWritesLocked().nodes.addConflictRange(b, e)
	}
}

// setBypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE.
func (c *rywCache) setBypassUnreadable(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.liveLocked().bypassUnreadable != v {
		c.mutableWritesLocked().bypassUnreadable = v
	}
}

// getBypassUnreadable reads the option under mu: a range read swaps it with its write span.
func (c *rywCache) getBypassUnreadable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked().bypassUnreadable
}
