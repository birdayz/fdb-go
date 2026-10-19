package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"slices"
	"sort"
	"sync"

	"fdb.dev/pkg/fdbgo/wire"
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

	// onPin and onClone are test seams (S4): every pin/unpin and every clone.
	onPin   func(w *rywWrites, pin bool)
	onClone func(from, to *rywWrites)

	// serverCache caches server-side state at the read version, avoiding
	// redundant server round-trips for repeated reads of the same range.
	// Matches C++'s SnapshotCache. Not invalidated by local writes/clears.
	serverCache snapshotCache

	// byteBuf batch-allocates small byte copies (e.g. atomic mutation params).
	// Reduces per-op allocs for small values.
	byteBuf []byte
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
	atomics    []rywMutation
	// unreadable: a versionstamped op landed on this key, making it UNREADABLE for
	// the transaction's lifetime — reads reaching it throw accessed_unreadable (1036)
	// unless bypassed. STICKY, mirroring C++ WriteMap.cpp:97
	// (`is_unreadable = it.is_unreadable() || op == SetVersionstampedValue/Key`):
	// set when a versionstamped op lands, PRESERVED by later plain Sets/atomics (the
	// exact-key SetValue stack-replace fast path is gated `!it.is_unreadable()` at
	// :125; on an unreadable entry the Set is pushed and the flag stays at :141), and
	// removed only by clear() (cleared entries are readable — you know they're empty).
	unreadable bool
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
	typ   MutationType
	param []byte
}

// rywRange represents a cleared range [begin, end).
type rywRange struct {
	begin []byte
	end   []byte
}

// allocBytes returns a slice of n bytes from the cache's shared buffer.
// Must be called with c.mu held. Reduces per-op allocs for small byte copies.
func (c *rywCache) allocBytes(n int) []byte {
	if cap(c.byteBuf)-len(c.byteBuf) < n {
		newCap := max(2*cap(c.byteBuf), len(c.byteBuf)+n)
		if newCap < 2048 {
			newCap = 2048
		}
		newBuf := make([]byte, len(c.byteBuf), newCap)
		copy(newBuf, c.byteBuf)
		c.byteBuf = newBuf
	}
	start := len(c.byteBuf)
	c.byteBuf = c.byteBuf[:start+n]
	return c.byteBuf[start : start+n]
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
	w := c.liveLocked()
	return len(w.writes) == 0 && len(w.cleared) == 0 && len(c.serverCache.entries) == 0
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
	c.byteBuf = nil // issued reads may still hold rows in the old buffer
	c.serverCache.reset()
}

// set records a Set operation.
func (c *rywCache) set(key, value []byte) { c.setWith(key, value, true) }

// setWith is RYW set: WriteMap::mutate(key, SetValue, value, addConflict)
// (ReadYourWrites.actor.cpp:2353).
func (c *rywCache) setWith(key, value []byte, addConflict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	if w.writes == nil {
		w.writes = make(map[string]rywEntry)
	}
	// Defensive copy — value must have its own backing array.
	// Cannot share byteBuf: the READABLE_UNIQUE_PENDING test proves
	// that read-back of cached Set values fails when values alias
	// the shared buffer (root cause: byteBuf growth copies data,
	// but sub-slices of the old buffer become stale if the buffer
	// pointer advances during the same transaction).
	copied := make([]byte, len(value))
	copy(copied, value)
	// Preserve the previous entry's unreadable flag: a plain Set over a key with a
	// pending versionstamped op keeps it unreadable (sticky — C++ WriteMap.cpp:125
	// gates the stack-replacing SetValue fast path on !it.is_unreadable(); on an
	// unreadable entry the Set is pushed and :141 keeps the flag).
	prev, exists := w.writes[string(key)]
	k := string(key) // one copy serves the map, the sorted index and the nodes
	if nodeKey(key) {
		w.nodes.mutate(stringBytes(k), addConflict, false)
	}
	w.writes[k] = rywEntry{value: copied, unreadable: prev.unreadable}
	if !exists {
		w.insertSortedKey(k)
	}
	// A Set after ClearRange wins — no need to remove from cleared, because
	// get() checks writes before cleared.
}

// clear records a Clear operation (single key).
func (c *rywCache) clear(key []byte) { c.clearWith(key, true) }

// clearWith is RYW clear(key): WriteMap::clear([key, keyAfter(key)), addConflict)
// (ReadYourWrites.actor.cpp:2435).
func (c *rywCache) clearWith(key []byte, addConflict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	if b, e, ok := nodeRange(key, keyAfterBytes(key)); ok {
		w.nodes.clear(b, e, addConflict)
	}
	// Remove from writes.
	if e, existed := w.writes[string(key)]; existed {
		delete(w.writes, string(key))
		w.removeSortedKey(string(key))
		if e.unreadable {
			w.removeUnreadableKey(string(key))
		}
	}
	// Add [key, key+\x00) to cleared.
	end := make([]byte, len(key)+1)
	copy(end, key)
	end[len(key)] = 0
	w.addClearedRange(key, end)
	// Cleared = readable (you know the key is empty): subtract the span from the
	// SVK unreadable ranges. C++ gets this free from the shared PTree — clear()
	// inserts readable cleared entries over the span (WriteMap.cpp:195).
	w.unreadableRanges = subtractRangeList(w.unreadableRanges, key, end)
}

// clearRange records a ClearRange [begin, end).
func (c *rywCache) clearRange(begin, end []byte) { c.clearRangeWith(begin, end, true) }

// clearRangeWith is RYW clear(range): WriteMap::clear(range, addConflict)
// (ReadYourWrites.actor.cpp:2402).
func (c *rywCache) clearRangeWith(begin, end []byte, addConflict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	if b, e, ok := nodeRange(begin, end); ok {
		w.nodes.clear(b, e, addConflict)
	}
	// Search the sorted index rather than scanning unaffected map entries.
	if len(w.writes) > 0 {
		w.ensureSorted()
		wStart := sort.SearchStrings(w.sortedKeys, string(begin))
		wEnd := sort.SearchStrings(w.sortedKeys, string(end))
		if wStart < wEnd {
			for i := wStart; i < wEnd; i++ {
				k := w.sortedKeys[i]
				if w.writes[k].unreadable {
					w.removeUnreadableKey(k)
				}
				delete(w.writes, k)
			}
			w.sortedKeys = slices.Delete(w.sortedKeys, wStart, wEnd)
		}
	}
	w.addClearedRange(begin, end)
	// Cleared = readable: subtract the span from the SVK unreadable ranges (see clear).
	w.unreadableRanges = subtractRangeList(w.unreadableRanges, begin, end)
}

// materializeCommit builds the coalesced commit mutation vector from the RYW write map — a port of C++
// ReadYourWritesTransaction::writeRangeToNativeTransaction over the whole key space
// (StringRef()..allKeys.end, ReadYourWrites.actor.cpp:1392 → :1997-2071). Two passes, matching C++:
//   - pass 1: emit the cleared stretches as ClearRange mutations, first ("because of keys that are both
//     cleared and set to a new value"). A key with an operation (a value, an absent value or an atomic
//     chain) is not a cleared segment of the C++ write map, so it splits a cleared range around itself
//     (:2001-2018); conflict-only boundaries do not.
//   - pass 2: per operation key in sorted order emit the coalesced op — a present value → Set; an absent
//     value (a matched CompareAndClear) → single-key Clear (C++ SetValue-absent → tr.clear(key)); a
//     pending atomic chain → one atomicOp per folded stack entry.
//
// The result is [all clears in range order] ++ [all ops in key order]. The fold already happened at insert
// time (coalesceOverAtomics + the site-B/C value-fold). coalesceCommitVectors does not use it for a
// versionstamped or RYW-disabled transaction, which ships its op-log as is.
// Caller must NOT hold c.mu.
func (c *rywCache) materializeCommit() []Mutation {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.liveLocked()
	out := make([]Mutation, 0, len(w.cleared)+len(w.writes))
	w.ensureSorted()
	// Pass 1 — clears (w.cleared is kept sorted + non-overlapping by addClearedRangeLocked).
	// A key with an operation is not a cleared segment of the C++ write map, so
	// it ends the cleared stretch: the clear resumes after it
	// (writeRangeToNativeTransaction, ReadYourWrites.actor.cpp:2001-2018).
	for _, r := range w.cleared {
		begin := r.begin
		i, _ := slices.BinarySearch(w.sortedKeys, string(r.begin))
		for ; i < len(w.sortedKeys) && w.sortedKeys[i] < string(r.end); i++ {
			key := w.sortedKeys[i]
			if string(begin) < key {
				out = append(out, Mutation{Type: MutClearRange, Key: begin, Value: []byte(key)})
			}
			begin = keyAfterBytes([]byte(key))
		}
		if bytes.Compare(begin, r.end) < 0 {
			out = append(out, Mutation{Type: MutClearRange, Key: begin, Value: r.end})
		}
	}
	// Pass 2 — per-key operations in sorted key order.
	for _, k := range w.sortedKeys {
		e := w.writes[k]
		key := []byte(k)
		switch {
		case e.hasAtomics:
			for _, m := range e.atomics {
				out = append(out, Mutation{Type: m.typ, Key: key, Value: m.param})
			}
		case e.absent:
			// A matched CompareAndClear resolved to "no value" → C++ SetValue(absent) → tr.clear(key),
			// i.e. a single-key ClearRange [key, key+\x00). (An absent entry is an operation key in the
			// write map, emitted in pass 2, NOT a cleared range from pass 1.)
			end := make([]byte, len(key)+1)
			copy(end, key)
			out = append(out, Mutation{Type: MutClearRange, Key: key, Value: end})
		default:
			out = append(out, Mutation{Type: MutSetValue, Key: key, Value: e.value})
		}
	}
	return out
}

// coalesceCommitMutations coalesces a validated mutation snapshot, using an ordered-Set specialization
// or a throwaway RYW write map, yielding the vector libfdb_c ships (RFC-172 / #28). Working from the
// SNAPSHOT rather than the live tx.ryw keeps the shipped set byte-identical to the set Commit just
// validated: the write map is a pure function of the op-log — the site-B/C value-fold and the
// coalesceOverAtomics chain-fold use only LOCAL write state, never DB reads — so the replay reproduces
// tx.ryw exactly, while a Set racing this Commit on another goroutine (appended to tx.mutations beyond the
// snapshot) is simply absent and can never ship unvalidated. Single-key clears are stored in the op-log as
// MutClearRange(k, k+\x00), so clearRange reproduces the original clear().
func coalesceCommitMutations(muts []Mutation) []Mutation {
	if out, ok := coalescePlainSetMutations(muts); ok {
		return out
	}
	var wm rywCache
	for _, m := range muts {
		switch m.Type {
		case MutSetValue:
			wm.set(m.Key, m.Value)
		case MutClearRange:
			wm.clearRange(m.Key, m.Value)
		default:
			wm.atomic(m.Type, m.Key, m.Value)
		}
	}
	return wm.materializeCommit()
}

// Ordered Sets need only last-write-wins, not replay (C++ WriteMap.cpp:125-137).
// Operands stay in the transaction's slab, which captureCommit borrows.
func coalescePlainSetMutations(muts []Mutation) ([]Mutation, bool) {
	unique := 0
	for i, m := range muts {
		if m.Type != MutSetValue {
			return nil, false
		}
		if i == 0 {
			unique++
			continue
		}
		switch bytes.Compare(muts[i-1].Key, m.Key) {
		case 1:
			return nil, false
		case -1:
			unique++
		}
	}
	out := make([]Mutation, 0, unique)
	for _, m := range muts {
		n := len(out)
		if n > 0 && bytes.Equal(out[n-1].Key, m.Key) {
			out[n-1] = m
		} else {
			out = append(out, m)
		}
	}
	return out, true
}

// mutationsHaveVersionstamp reports whether the op-log carries any SetVersionstamped{Key,Value}. Those ops
// can't be safely coalesced through the keyed write map — the 10-byte stamp is assigned at commit, so the
// final key/value is unknown client-side, and a later Set on the same TEMPLATE key must not drop the
// versionstamped op (C++ WriteMap::mutate pushes it onto the unreadable entry, WriteMap.cpp:139-146). A txn
// carrying one ships its raw op-log 1:1 instead of the materialized write map (#28 / RFC-172).
func mutationsHaveVersionstamp(muts []Mutation) bool {
	for _, m := range muts {
		if m.Type == MutSetVersionstampedKey || m.Type == MutSetVersionstampedValue {
			return true
		}
	}
	return false
}

// coalesceWriteConflicts sorts and merges write-conflict ranges (overlapping AND adjacent), matching C++'s
// contiguous is_conflict_range segment emission in writeRangeToNativeTransaction pass 2
// (ReadYourWrites.actor.cpp:2022-2033, 2069-2071). The per-op tx.writeConflicts list is uncoalesced, so a
// key hammered N times carries N identical [k,k+\x00) ranges; without this they ship N-fold and trip 2101
// on size just like the unfolded mutations. Adjacent merge ([a,b)+[b,c)→[a,c)) mirrors C++ treating
// touching conflict segments as one range. Returns a fresh slice; input is not mutated.
func coalesceWriteConflicts(in []KeyRange) []KeyRange {
	if len(in) <= 1 {
		return in
	}
	return coalesceOwnedWriteConflicts(slices.Clone(in))
}

// The caller owns rs; sorting and merging may overwrite its range headers.
func coalesceOwnedWriteConflicts(rs []KeyRange) []KeyRange {
	if len(rs) <= 1 {
		return rs
	}
	slices.SortFunc(rs, func(a, b KeyRange) int {
		if cmp := bytes.Compare(a.Begin, b.Begin); cmp != 0 {
			return cmp
		}
		return bytes.Compare(a.End, b.End)
	})
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if bytes.Compare(r.Begin, last.End) <= 0 { // overlapping OR adjacent
			if bytes.Compare(r.End, last.End) > 0 {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// coalesceCommitVectors decides the mutation vector + pre-self-conflict write-conflict vector to size and
// ship for a commit (#28 / RFC-172). rawMuts is the already-validated op-log snapshot; rawConflicts is the
// pre-SC write-conflict snapshot. Returns (shipMuts, sizeConflicts, coalesced). The op-log is coalesced
// (write-map materialize + conflict merge) UNLESS RYW is disabled OR the txn carries a versionstamp op —
// both keep the raw op-log 1:1: the 10-byte stamp is assigned at commit so it can't be keyed in the write
// map (a later Set on the same template key must not drop the versionstamped op — C++ WriteMap::mutate
// pushes it, WriteMap.cpp:139-148), and such txns are tiny (never near 2101). A versionstamp txn that also
// hammers an unrelated key ships uncoalesced — safe (no wrong bytes, no dropped op), identical to pre-#28
// and the rywDisabled path (FDB-C-dev delta ACK: non-blocking). Extracted from Commit as a PURE function so
// the ship-decision is unit-testable / revert-provable outside Commit's inline flow.
// A coalesced shipMuts is always a fresh vector: captureCommitVectors keeps it uncopied.
func coalesceCommitVectors(rawMuts []Mutation, rawConflicts []KeyRange, rywDisabled bool) (shipMuts []Mutation, sizeConflicts []KeyRange, coalesced bool) {
	if rywDisabled || mutationsHaveVersionstamp(rawMuts) {
		return rawMuts, rawConflicts, false
	}
	return coalesceCommitMutations(rawMuts), coalesceWriteConflicts(rawConflicts), true
}

// finalizeShipConflicts appends the self-conflict range (added by maybeMakeSelfConflicting AFTER the 2101
// gate, so it is unsized — matching C++, which adds it after the transaction_too_large check,
// NativeAPI.actor.cpp:6858) to the SIZED write-conflict snapshot, re-coalescing when the txn coalesces.
// Works from the snapshot (rawConflicts) — NOT a live re-read of tx.writeConflicts — so a Set racing this
// Commit cannot ship an unvalidated/unsized conflict range whose mutation was excluded from the frozen
// op-log. Pure + unit-testable.
func finalizeShipConflicts(rawConflicts []KeyRange, sc KeyRange, scAdded, coalesced bool) []KeyRange {
	base := rawConflicts
	if scAdded {
		base = append(append(make([]KeyRange, 0, len(rawConflicts)+1), rawConflicts...), sc)
		if coalesced {
			return coalesceOwnedWriteConflicts(base)
		}
	} else if coalesced {
		return coalesceWriteConflicts(base)
	}
	return base
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
	if w.writes == nil {
		w.writes = make(map[string]rywEntry)
	}
	k := string(key)
	if nodeKey(key) {
		w.nodes.mutate(stringBytes(k), addConflict, isUnresolvedVersionstamp(op)) // WriteMap.cpp:87-158
	}
	entry, exists := w.writes[k]
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
			val, clr := applyAtomic(op, entry.value, param)
			if !clr && val == nil {
				// A non-clear op that resolved to EMPTY (e.g. doMax(_, "")) is present-empty,
				// NOT absent. Normalize nil→[]byte{} (C++ Optional present) so a LATER fold
				// over this entry reads it as present — matching resolveAtomics. Without this,
				// a subsequent CompareAndClear/V2 op misreads the nil base as absent.
				val = []byte{}
			}
			entry.value = val
			entry.absent = clr
			w.writes[k] = entry
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
			w.writes[k] = rywEntry{value: val, absent: clr}
			w.insertSortedKey(k)
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
	paramCopy := c.allocBytes(len(param))
	copy(paramCopy, param)
	// Coalesce onto the pending chain (C++ WriteMap::coalesceOver at insert time, WriteMap.cpp:106/144):
	// repeated same-type resolvable atomics fold to one op, so a 150k-`ADD 1` chain on an unread key ships
	// one mutation, not 150k (the RFC-172 / #28 fix). Read-transparent; versionstamps/CompareAndClear push.
	entry.atomics = coalesceOverAtomics(entry.atomics, rywMutation{typ: op, param: paramCopy})
	w.writes[k] = entry
	if !exists {
		w.insertSortedKey(k)
	}
}

// localPointLocked evaluates the merged point view without changing the write
// map's dependence or unreadability (C++ RYWIterator::kv).
func (c *rywCache) localPointLocked(w *rywWrites, key []byte) ([]byte, bool, error) {
	entry, written := w.writes[string(key)]
	if !w.bypassUnreadable && ((written && entry.unreadable) || w.isUnreadable(key)) {
		return nil, true, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	if written {
		if !entry.hasAtomics {
			if entry.absent {
				return nil, true, nil
			}
			return entry.value, true, nil
		}
		base, known := c.serverCache.getKey(key)
		if !known && !(len(entry.atomics) > 0 && isUnresolvedVersionstamp(entry.atomics[0].typ)) {
			return nil, false, nil
		}
		value, err := resolvePointAtomics(base, entry.atomics, w.bypassUnreadable)
		return value, true, err
	}
	if w.isCleared(key) {
		return nil, true, nil
	}
	value, known := c.serverCache.getKey(key)
	return value, known, nil
}

// resolvePointAtomics is RYWIterator::kv for a pending atomic stack over a known
// storage base (nil = absent).
func resolvePointAtomics(base []byte, atomics []rywMutation, bypassUnreadable bool) ([]byte, error) {
	if bypassUnreadable {
		value, cleared := resolveAtomicsBypass(base, atomics)
		if cleared {
			return nil, nil
		}
		return value, nil
	}
	value, cleared, unreadable := resolveAtomics(base, atomics)
	if unreadable {
		return nil, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	if cleared {
		return nil, nil
	}
	return value, nil
}

// get intercepts a single-key read and merges with pending writes.
//
// A dependent read resolves the atomic stack it was issued against: C++ re-skips
// the read's own RYWIterator after the storage wait (ReadYourWrites.actor.cpp:127),
// and that iterator's write-map version is unaffected by later writes
// (WriteMap.h:158-160). Operands are copied because the stack outlives the lock.
func (c *rywCache) get(ctx context.Context, key []byte, serverGet func(ctx context.Context, key []byte) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	w := c.liveLocked()
	value, known, err := c.localPointLocked(w, key)
	var atomics []rywMutation
	if entry, written := w.writes[string(key)]; !known && written && entry.hasAtomics {
		atomics = make([]rywMutation, len(entry.atomics))
		for i, m := range entry.atomics {
			atomics[i] = rywMutation{typ: m.typ, param: bytes.Clone(m.param)}
		}
	}
	bypassUnreadable := w.bypassUnreadable
	c.mu.Unlock()
	if known {
		return value, err
	}
	base, err := c.getSnapshot(ctx, key, serverGet)
	if err != nil || atomics == nil {
		return base, err
	}
	return resolvePointAtomics(base, atomics, bypassUnreadable)
}

func (c *rywCache) getSnapshot(ctx context.Context, key []byte, serverGet func(context.Context, []byte) ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	value, known := c.serverCache.getKey(key)
	c.mu.Unlock()
	if known {
		return value, nil
	}
	value, err := serverGet(ctx, key)
	if err == nil {
		c.cachePointResult(key, value)
	}
	return value, err
}

func (c *rywCache) cachePointResult(key, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var rows []KeyValue
	if value != nil {
		rows = []KeyValue{{Key: bytes.Clone(key), Value: value}}
	}
	c.serverCache.insert(key, append(bytes.Clone(key), 0), rows)
}

// rywRangeRead is a range read past its RYW issue; its merge and conflict filter use the
// write state captured then (ReadYourWrites.actor.cpp:382, WriteMap.h:158).
type rywRangeRead struct {
	c                 *rywCache
	begin, end        []byte
	limit, byteTarget int
	reverse           bool
	known             bool // answered from local state at issue
	rows              []KeyValue
	more              bool
	err               error
	unreadableCap     []byte
	writesAtIssue     bool        // local writes or clears in [begin, end) at issue
	view              *rywWrites  // their state, a private copy for a read that must fetch
	localConflicts    [][2][]byte // a local answer's filtered read conflict, computed at issue
}

// issueRangeInto answers the read from local state when it can, deciding its read conflict in the
// same critical section; only a read that must fetch copies the write span it will merge against.
func (c *rywCache) issueRangeInto(r *rywRangeRead, begin, end []byte, limit, byteTarget int, reverse, wantConflicts bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*r = rywRangeRead{c: c, begin: begin, end: end, limit: limit, byteTarget: byteTarget, reverse: reverse}
	w := c.liveLocked()
	r.writesAtIssue = w.hasWritesInRange(begin, end) || w.hasClearsInRange(begin, end)
	if rows, more, known, err := c.localRangeLocked(w, begin, end, limit, byteTarget, reverse); known {
		r.known, r.rows, r.more, r.err = true, rows, more, err
		if wantConflicts && err == nil && r.writesAtIssue {
			if cb, ce := rangeConflictExtent(begin, end, rows, more, reverse); bytes.Compare(cb, ce) < 0 {
				r.localConflicts = w.conflictRanges(cb, ce)
			}
		}
		return
	}
	if r.writesAtIssue {
		r.view = c.captureWriteSpanLocked(begin, end)
	}
	// Unreadable reach cap (RFC-098): truncate the scan window at the first
	// (forward) / last (reverse) unreadable position; iteration that would
	// CROSS the cap throws accessed_unreadable, results before it emit
	// normally (C++ reach semantics, ReadYourWrites.actor.cpp:685 vs :692).
	// Computed BEFORE the fast-path branch: an SVK candidate range can cover
	// spans with no local write entries at all.
	r.unreadableCap = w.unreadableScanCap(begin, end, reverse)
	if r.view == nil {
		r.known, r.rows, r.more, r.err = c.cachedRangeLocked(begin, end, limit, byteTarget, reverse, r.unreadableCap)
	}
}

// cachedRangeLocked answers a range with no local writes from the snapshot cache
// when it holds all of the range (capped at an unreadable position).
func (c *rywCache) cachedRangeLocked(begin, end []byte, limit, byteTarget int, reverse bool, unreadableCap []byte) (bool, []KeyValue, bool, error) {
	if unreadableCap != nil {
		if reverse {
			begin = unreadableCap
		} else {
			end = unreadableCap
		}
	}
	cachedKVs, fullyKnown := c.serverCache.getRangeKVsLimited(begin, end, cacheWalkBudget(limit), reverse)
	if !fullyKnown {
		return false, nil, false, nil
	}
	kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
	kvs, remainingBytes := applyRangeByteLimit(kvs, byteTarget)
	bytesReached := byteTarget > 0 && remainingBytes <= 0
	// Reaching the unreadable cap without filling a limit throws (RFC-098).
	if !bytesReached && unreadableCap != nil && (limit <= 0 || len(kvs) < limit) {
		return true, nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	return true, kvs, bytesReached || computeMore(cachedKVs, limit) || limitReached(limit, len(kvs)) || unreadableCap != nil, nil
}

// conflictRanges filters [begin, end) through the issue-time write state (view must be set):
// C++ addConflictRange walks the read's own iterator (ReadYourWrites.actor.cpp:388).
func (r *rywRangeRead) conflictRanges(begin, end []byte) [][2][]byte {
	if bytes.Compare(begin, end) >= 0 {
		return nil
	}
	c := r.c
	c.mu.Lock()
	defer c.mu.Unlock()
	return r.view.conflictRanges(begin, end)
}

// rangeServerFunc is the storage range read under the RYW layer. readThrough, when set, is
// where the read finished its last shard (NativeAPI.actor.cpp:4795-4799).
type rangeServerFunc func(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error)

// run fetches what local state did not answer and merges it, re-fetching past locally cleared
// rows rather than truncating, as the C++ RYWIterator does (ReadYourWrites.actor.cpp:685-686).
func (r *rywRangeRead) run(ctx context.Context, serverGetRange rangeServerFunc) ([]KeyValue, bool, error) {
	if r.known {
		return r.rows, r.more, r.err
	}
	c, begin, end, limit, byteTarget, reverse := r.c, r.begin, r.end, r.limit, r.byteTarget, r.reverse
	unreadableCap, view := r.unreadableCap, r.view
	requestedBegin, requestedEnd := begin, end
	if unreadableCap != nil {
		if reverse {
			begin = unreadableCap
		} else {
			end = unreadableCap
		}
	}
	// reached reports whether iteration of the capped window would continue
	// INTO the unreadable position: the limit was not filled inside the
	// window (an unlimited scan always reaches).
	reached := func(got int) bool {
		return unreadableCap != nil && (limit <= 0 || got < limit)
	}

	if view == nil {
		// No local mutations, and the snapshot cache did not answer at issue.
		kvs, more, err := c.fetchServer(ctx, begin, end, limit, rywFirstRequestBytes(byteTarget), reverse, serverGetRange)
		if err != nil {
			return nil, false, err
		}
		cut, remainingBytes := applyRangeByteLimit(kvs, byteTarget)
		bytesReached := byteTarget > 0 && remainingBytes <= 0
		if byteTarget > 0 && more && !bytesReached && c.replyEdgeKnown(begin, end, kvs, reverse) {
			// Re-walk storage only, within the issue-time cap: the read was issued
			// with no writes in range, and later writes must not join it.
			if rows, rowsMore, known, err := c.localRange(nil, begin, end, limit, byteTarget, reverse); known {
				if err != nil {
					return nil, false, err
				}
				if !rowsMore && reached(len(rows)) {
					return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
				}
				return rows, rowsMore || unreadableCap != nil, nil
			}
		}
		if !more && !bytesReached && reached(len(cut)) {
			return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
		}
		// A byte-limited page ends like the cache re-walk, which reports a filled row limit.
		limitStop := byteTarget > 0 && limitReached(limit, len(cut))
		return cut, more || bytesReached || limitStop || unreadableCap != nil, nil
	}

	// Slow path: iterative fetch + merge. Loop until we either fill
	// the limit or the server is exhausted for the remaining range.
	var result []KeyValue
	remainingBytes := byteTarget
	remaining := limit
	if remaining <= 0 {
		remaining = math.MaxInt // C++ ROW_LIMIT_UNLIMITED: 0 or negative = no limit
	}
	curBegin := begin
	curEnd := end
	requestCount := 0
	countedGetRange := func(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
		requestCount++
		return serverGetRange(ctx, begin, end, limit, byteTarget, reverse)
	}

	for remaining > 0 && bytes.Compare(curBegin, curEnd) < 0 {
		// Fetch from server with headroom to compensate for clears.
		// Cap at 10000 before doubling to avoid overflow when remaining=math.MaxInt.
		fetchLimit := 10000
		if remaining <= 5000 {
			fetchLimit = remaining * 2
			if fetchLimit < 256 {
				fetchLimit = 256
			}
		}

		serverKVs, serverMore, err := c.fetchOrCached(ctx, curBegin, curEnd, fetchLimit, rywRequestBytes(remainingBytes, requestCount), reverse, countedGetRange)
		if err != nil {
			return nil, false, err
		}
		if byteTarget > 0 {
			if rows, more, known, err := c.localRange(view, requestedBegin, requestedEnd, limit, byteTarget, reverse); known {
				return rows, more, err
			}
		}

		// Knowledge boundary: when serverMore=true, we only know the DB
		// state up to the last returned key. Writes beyond this boundary
		// MUST NOT be included — un-fetched server keys may interleave.
		// When serverMore=false, boundary is nil → all writes in range
		// are safe to include.
		var boundary []byte
		if serverMore && len(serverKVs) > 0 {
			boundary = serverKVs[len(serverKVs)-1].Key
		}

		batch := c.mergeBatch(view, serverKVs, curBegin, curEnd, boundary, reverse)

		take := len(batch)
		if take > remaining {
			take = remaining
		}
		batch, remainingBytes = applyRangeByteLimit(batch[:take], remainingBytes)
		result = append(result, batch...)
		remaining -= len(batch)

		if remaining <= 0 || (byteTarget > 0 && remainingBytes <= 0) {
			// C++ reports more whenever a limit stopped iteration, including an
			// exactly-filled final page; do not reach the unreadable cap below.
			return result, true, nil
		}

		if !serverMore {
			// Server exhausted the (possibly capped) window without filling
			// the limit: iteration would continue INTO the unreadable
			// position — that is the REACH (RFC-098).
			if reached(len(result)) {
				return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
			}
			// All writes included. Done.
			return result, false, nil
		}

		if byteTarget > 0 && len(result) > 0 {
			return result, true, nil // C++ soft byte stop before the next unknown span.
		}
		// Server had more data, but we still need results.
		// Advance the scan range past the last fetched server key.
		if len(serverKVs) == 0 {
			break // Shouldn't happen: serverMore=true with 0 results.
		}

		if reverse {
			curEnd = serverKVs[len(serverKVs)-1].Key // [curBegin, lastKey)
		} else {
			// keyAfter(lastKey): append \x00 to step past the last fetched key.
			lastKey := serverKVs[len(serverKVs)-1].Key
			curBegin = append(append([]byte{}, lastKey...), 0)
		}
	}

	if reached(len(result)) {
		// The capped window was exhausted without filling the limit (RFC-098).
		return nil, false, &wire.FDBError{Code: ErrAccessedUnreadable}
	}
	return result, false, nil
}

func (c *rywCache) getSnapshotRange(
	ctx context.Context,
	begin, end []byte,
	limit, byteTarget int,
	reverse bool,
	serverGetRange rangeServerFunc,
) ([]KeyValue, bool, error) {
	rows, more, known, err := c.localRange(nil, begin, end, limit, byteTarget, reverse)
	if known {
		return rows, more, err
	}
	rows, more, err = c.fetchServer(ctx, begin, end, limit, rywFirstRequestBytes(byteTarget), reverse, serverGetRange)
	if err == nil {
		cut, remainingBytes := applyRangeByteLimit(rows, byteTarget)
		bytesReached := byteTarget > 0 && remainingBytes <= 0
		if byteTarget > 0 && more && !bytesReached && c.replyEdgeKnown(begin, end, rows, reverse) {
			if rows, more, known, err := c.localRange(nil, begin, end, limit, byteTarget, reverse); known {
				return rows, more, err
			}
		}
		return cut, more || bytesReached || (byteTarget > 0 && limitReached(limit, len(cut))), nil
	}
	return rows, more, err
}

// replyEdgeKnown reports whether a more=true storage reply is followed by cached
// state, so only the snapshot-cache walk can tell where the page ends.
func (c *rywCache) replyEdgeKnown(begin, end []byte, kvs []KeyValue, reverse bool) bool {
	if len(kvs) == 0 {
		return true
	}
	last := kvs[len(kvs)-1].Key
	if (!reverse && isKeyAfter(end, last)) || (reverse && bytes.Equal(last, begin)) {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverCache.knownPast(last, reverse)
}

// captureWriteSpanLocked copies the write-map state intersecting [begin, end) into a
// private version the old range loop merges against: C++ iterates a WriteMap version
// later writes do not change (WriteMap.h:158-160). The copy is never c.w and never
// pinned, so mergeBatchLocked may fold resolved atomics into it. It has no nodes; its
// cursor keeps the legacy boundary sources until phase 3 deletes the loop (RFC-262 D2).
// Atomic stacks are cloned because coalesceOverAtomics folds into the top in place.
func (c *rywCache) captureWriteSpanLocked(begin, end []byte) *rywWrites {
	w := c.liveLocked()
	w.ensureSorted()
	i := sort.SearchStrings(w.sortedKeys, string(begin))
	j := sort.SearchStrings(w.sortedKeys, string(end))
	s := &rywWrites{
		writes:           make(map[string]rywEntry, j-i),
		sortedKeys:       append(make([]string, 0, j-i), w.sortedKeys[i:j]...),
		cleared:          overlappingRanges(w.cleared, begin, end),
		unreadableRanges: overlappingRanges(w.unreadableRanges, begin, end),
		bypassUnreadable: w.bypassUnreadable,
	}
	for _, k := range s.sortedKeys {
		e := w.writes[k]
		if e.hasAtomics {
			e.atomics = append([]rywMutation(nil), e.atomics...)
		}
		s.writes[k] = e
	}
	ui := sort.SearchStrings(w.unreadableKeys, string(begin))
	uj := sort.SearchStrings(w.unreadableKeys, string(end))
	s.unreadableKeys = append([]string(nil), w.unreadableKeys[ui:uj]...)
	return s
}

// overlappingRanges returns the sorted, non-overlapping ranges intersecting [begin, end), unclipped.
func overlappingRanges(ranges []rywRange, begin, end []byte) []rywRange {
	lo := sort.Search(len(ranges), func(i int) bool { return bytes.Compare(ranges[i].end, begin) > 0 })
	hi := sort.Search(len(ranges), func(i int) bool { return bytes.Compare(ranges[i].begin, end) >= 0 })
	if lo >= hi {
		return nil
	}
	return append([]rywRange(nil), ranges[lo:hi]...)
}

// rywRequestBytes is C++ setRequestLimits' byte limit for a storage read of a
// RYW range call: min(byteTarget << min(requestCount, 20), REPLY_BYTE_LIMIT)
// (ReadYourWrites.actor.cpp:569-590, 748-753).
func rywRequestBytes(byteTarget, requestCount int) int {
	if byteTarget <= 0 {
		return byteTarget
	}
	return int(min(int64(min(byteTarget, replyByteLimit))<<min(requestCount, 20), int64(replyByteLimit)))
}

// rywFirstRequestBytes is C++ setRequestLimits' byte limit for the first storage read of a
// RYW range call (requestCount 0): never above REPLY_BYTE_LIMIT, while the rows returned are
// still cut against the caller's own target (ReadYourWrites.actor.cpp:569-590, 748-753).
func rywFirstRequestBytes(byteTarget int) int {
	return rywRequestBytes(byteTarget, 0)
}

// applyRangeByteLimit includes the row that exhausts the budget, guaranteeing
// progress even when one row is larger than the requested page.
func applyRangeByteLimit(rows []KeyValue, remaining int) ([]KeyValue, int) {
	if remaining > 0 {
		for i, row := range rows {
			remaining -= 8 + len(row.Key) + len(row.Value)
			if remaining <= 0 {
				return rows[:i+1], remaining
			}
		}
	}
	return rows, remaining
}

// A storage reply's more flag can be true even when its known span covers the
// requested endpoint. Re-walk the cached iterator to determine RYW completion.
// w is the write-map version to merge (nil: the snapshot cache alone).
func (c *rywCache) localRange(w *rywWrites, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localRangeLocked(w, begin, end, limit, byteTarget, reverse)
}

// localRangeLocked follows C++ getRangeValue's merged iterator before any
// NativeAPI read. A byte-limited page can end at unknown state after any row.
func (c *rywCache) localRangeLocked(w *rywWrites, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, bool, error) {
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, true, nil
	}
	cur := c.newSegCursor(end, w)
	if reverse {
		cur.seek(c.prevBoundaryLocked(end, end, w))
	} else {
		cur.seek(begin)
	}
	var rows []KeyValue
	remainingBytes := byteTarget
	for cur.valid() && bytes.Compare(cur.end, begin) > 0 {
		switch cur.typ {
		case segUnknown:
			if byteTarget > 0 && len(rows) > 0 {
				return rows, true, true, nil
			}
			return nil, false, false, nil
		case segUnreadable:
			return nil, false, true, &wire.FDBError{Code: ErrAccessedUnreadable}
		case segKV:
			value := c.segmentValueLocked(w, cur.begin)
			rows = append(rows, KeyValue{Key: bytes.Clone(cur.begin), Value: value})
			if byteTarget > 0 {
				// NativeAPI GetRangeLimits::decrement charges eight bytes per row
				// in addition to the key and value, including present-empty values.
				remainingBytes -= 8 + len(cur.begin) + len(value)
			}
			if (limit > 0 && len(rows) >= limit) || (byteTarget > 0 && remainingBytes <= 0) {
				return rows, true, true, nil
			}
		}
		if reverse {
			cur.prev()
		} else {
			cur.next()
		}
	}
	return rows, false, true, nil
}

// segmentValueLocked is RYWIterator::kv for a segment already classified segKV.
// Resolve against cached storage without rewriting dependent/unreadable flags.
func (c *rywCache) segmentValueLocked(w *rywWrites, key []byte) []byte {
	base, _ := c.serverCache.getKey(key)
	if w == nil {
		return base
	}
	if entry, ok := w.writes[string(key)]; ok {
		if !entry.hasAtomics {
			return entry.value
		}
		value, _, _ := resolveAtomics(base, entry.atomics)
		return value
	}
	return base
}

// fetchOrCached checks the snapshot cache before making a server call.
// If the range is fully cached, returns cached KVs (in scan direction).
// Otherwise fetches from server and caches the result.
func (c *rywCache) fetchOrCached(
	ctx context.Context,
	begin, end []byte,
	limit int,
	byteTarget int,
	reverse bool,
	serverGetRange rangeServerFunc,
) ([]KeyValue, bool, error) {
	c.mu.Lock()
	cachedKVs, fullyKnown := c.serverCache.getRangeKVsLimited(begin, end, cacheWalkBudget(limit), reverse)
	c.mu.Unlock()

	if fullyKnown {
		kvs := applyLimitAndDirection(cachedKVs, limit, reverse)
		more := computeMore(cachedKVs, limit)
		return kvs, more, nil
	}
	return c.fetchServer(ctx, begin, end, limit, byteTarget, reverse, serverGetRange)
}

// fetchServer reads storage past any range the snapshot cache knows is empty at the scan
// origin, where C++'s RYW iterator starts its storage read (ReadYourWrites.actor.cpp:487-508,
// 760-764), and caches the reply up to its readThrough.
func (c *rywCache) fetchServer(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool, serverGetRange rangeServerFunc) ([]KeyValue, bool, error) {
	c.mu.Lock()
	begin, end = c.serverCache.skipKnownEmpty(begin, end, reverse)
	c.mu.Unlock()
	if bytes.Compare(begin, end) >= 0 {
		return nil, false, nil
	}
	kvs, more, readThrough, err := serverGetRange(ctx, begin, end, limit, byteTarget, reverse)
	if err != nil {
		return nil, false, err
	}
	c.cacheServerReply(begin, end, kvs, more, reverse, readThrough)
	return kvs, more, nil
}

// cacheServerResult inserts a server getRange result into the snapshot cache.
//
// serverMore with ZERO rows would over-claim: the narrowing below is what keeps
// a truncated fetch from asserting knowledge past its last returned key, and
// with no rows there is nothing to narrow to, so [fetchBegin, fetchEnd) would be
// marked entirely known-empty. insert then erases every fragment strictly inside
// it and real cached rows are lost — silently, since the invariant still holds
// and later reads just see a known-empty range.
//
// The read path cannot produce that pair. rangeScanImpl returns more=true in
// four places: the two soft byte stops (readpath.go:794 and :916), which
// require len(allKVs) > 0; isReached after a reply's decrement (:891); and the
// final `limits.rows == 0` (:957). The last two need a limit that was not
// reached on entry to reach zero, and decrement lowers rows and minRows only by
// rows appended — the RYW range read's limits come from newRangeLimits
// (readpath.go:642), which gives rows >= 1 (0 becomes unlimited; callers reject
// rows below -1) and minRows = 1, and a byte limit alone is not reached until
// minRows is. So more=true implies rows were returned. The zero-row break at
// readpath.go:908 is a different guard: it stops a misbehaving storage server
// that reports more=true with an empty batch from reaching the narrowing at
// :921, which would index kvs[len(kvs)-1] on an empty slice. It bounds the
// inner re-query loop; it is not what establishes the invariant relied on here.
func (c *rywCache) cacheServerResult(fetchBegin, fetchEnd []byte, serverKVs []KeyValue, serverMore bool, reverse bool) {
	c.cacheServerReply(fetchBegin, fetchEnd, serverKVs, serverMore, reverse, nil)
}

// cacheServerReply is cacheServerResult knowing also through readThrough, as C++
// getKnownKeyRange extends a reply's known range (ReadYourWrites.actor.cpp:496-499).
func (c *rywCache) cacheServerReply(fetchBegin, fetchEnd []byte, serverKVs []KeyValue, serverMore bool, reverse bool, readThrough []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheBegin := fetchBegin
	cacheEnd := fetchEnd
	var cacheKVs []KeyValue

	if reverse {
		if serverMore && len(serverKVs) > 0 {
			// Reverse scan: last element is the smallest key returned.
			// Known range is [lastKey, fetchEnd).
			cacheBegin = serverKVs[len(serverKVs)-1].Key
		}
		if readThrough != nil && bytes.Compare(readThrough, cacheBegin) < 0 {
			cacheBegin = readThrough
		}
		// Store in forward order.
		cacheKVs = make([]KeyValue, len(serverKVs))
		for i, kv := range serverKVs {
			cacheKVs[len(serverKVs)-1-i] = kv
		}
	} else {
		if serverMore && len(serverKVs) > 0 {
			// Forward scan: known range is [fetchBegin, keyAfter(lastKey)).
			lastKey := serverKVs[len(serverKVs)-1].Key
			cacheEnd = append(append([]byte(nil), lastKey...), 0)
		}
		if readThrough != nil && bytes.Compare(readThrough, cacheEnd) > 0 {
			cacheEnd = readThrough
		}
		// insert retains its own descriptors; reply bytes remain immutable internally.
		cacheKVs = serverKVs
	}

	c.serverCache.insert(cacheBegin, cacheEnd, cacheKVs)
}

// applyLimitAndDirection returns KVs with limit and direction applied.
// Input KVs must be in ascending order.
func applyLimitAndDirection(kvs []KeyValue, limit int, reverse bool) []KeyValue {
	if reverse {
		// Reverse the order.
		out := make([]KeyValue, len(kvs))
		for i, kv := range kvs {
			out[len(kvs)-1-i] = kv
		}
		kvs = out
	}
	if limit > 0 && len(kvs) > limit {
		kvs = kvs[:limit]
	}
	return kvs
}

// computeMore returns true if applying the limit would leave remaining KVs.
func computeMore(kvs []KeyValue, limit int) bool {
	return limit > 0 && len(kvs) > limit
}

// cacheWalkBudget is how many cached KVs a snapshot-cache walk must produce before the
// answer to a `limit`-row read is final: one more than the limit. The limit itself
// supplies the page; the extra row is what computeMore's `len(kvs) > limit` reads to
// decide `more`, so stopping at exactly `limit` would report more=false on a range that
// has further rows. limit <= 0 is unlimited and cannot be bounded.
func cacheWalkBudget(limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit + 1
}

// limitReached ports C++ GetRangeLimits::isReached() for a row-only limit: the
// requested row limit was fully consumed (rows==0). FDB forces more=true in this
// case — ReadYourWrites.actor.cpp:799 `result.more = result.more || limits.isReached()` —
// the canonical "the limit was the stop reason" contract that continuation-based
// iteration relies on, INDEPENDENT of whether further data actually exists. `returned`
// is the count after the limit was applied, so returned==limit ⟺ the limit was reached.
// (Go's merge already clamps results to [begin,end), so C++'s subsequent
// resize-clears-more branch — which only fires for items BEYOND end — never applies here.)
func limitReached(limit, returned int) bool {
	return limit > 0 && returned >= limit
}

// mergeBatch merges a batch of server results with local writes and clears.
// boundary is the knowledge boundary (last fetched key); nil means the entire
// range is known. Returns sorted key-value pairs.
//
// Uses sorted write keys + two-pointer merge for O(k + S) instead of the
// previous O(W + S log S) where W = total writes, k = writes in range, S = server results.
func (c *rywCache) mergeBatch(
	w *rywWrites,
	serverKVs []KeyValue,
	rangeBegin, rangeEnd []byte,
	boundary []byte,
	reverse bool,
) []KeyValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mergeBatchLocked(w, serverKVs, rangeBegin, rangeEnd, boundary, reverse)
}

// It stores resolved atomics into w, so w must be a private span copy
// (captureWriteSpanLocked): never the live version and never a pinned one (RFC-262 D1).
func (c *rywCache) mergeBatchLocked(
	w *rywWrites,
	serverKVs []KeyValue,
	rangeBegin, rangeEnd []byte,
	boundary []byte,
	reverse bool,
) []KeyValue {
	if w == c.w || w.pins != 0 {
		panic("rywCache.mergeBatchLocked: the write-map version is shared (live or pinned)")
	}
	w.ensureSorted()

	// Phase 1: Filter server results — remove cleared keys.
	// Server results are already sorted in scan direction.
	filteredServer := make([]KeyValue, 0, len(serverKVs))
	// Build server key lookup only if needed for atomic resolution.
	// Most Record Layer transactions have no atomics — skip the map allocation.
	var serverValues map[string][]byte
	for _, kv := range serverKVs {
		if !w.isCleared(kv.Key) {
			filteredServer = append(filteredServer, kv)
		}
	}

	// atomicCleared tracks keys where atomic resolution resulted in deletion.
	// These must be excluded from filteredServer during the merge phase,
	// because the atomic was resolved AFTER building filteredServer.
	var atomicCleared map[string]bool

	// Phase 2: Find write keys in the effective range using binary search.
	// For forward scans: include writes in [rangeBegin, boundary] (inclusive).
	// For reverse scans: include writes in [boundary, rangeEnd) (inclusive begin).
	effectiveBegin := string(rangeBegin)
	effectiveEnd := string(rangeEnd)

	if boundary != nil {
		if reverse {
			// Include writes >= boundary.
			if string(boundary) > effectiveBegin {
				effectiveBegin = string(boundary)
			}
		} else {
			// Include writes <= boundary. Use boundary+"\x00" as exclusive end
			// so sort.SearchStrings returns an index that includes boundary itself
			// (the boundary key is the last fetched server key — safe to include).
			boundaryAfter := string(append(append([]byte(nil), boundary...), 0))
			if boundaryAfter < effectiveEnd {
				effectiveEnd = boundaryAfter
			}
		}
	}

	wStart := sort.SearchStrings(w.sortedKeys, effectiveBegin)
	wEnd := sort.SearchStrings(w.sortedKeys, effectiveEnd)

	// Process writes in range: resolve atomics, collect into sorted slice.
	writeKVs := make([]KeyValue, 0, wEnd-wStart)
	for i := wStart; i < wEnd; i++ {
		k := w.sortedKeys[i]
		entry, exists := w.writes[k]
		if !exists {
			continue // phantom key (deleted by prior atomic caching)
		}
		if entry.absent {
			// Phantom slot (a matched CompareAndClear, RFC-058): an is_kv slot for getKey
			// but ABSENT in a range read. Skip it from writeKVs AND shadow any server-present
			// value at this key (the local clear wins) via atomicCleared — else this
			// pre-existing absent entry would fall through to the plain-entry branch below
			// and be emitted as a phantom KV.
			if atomicCleared == nil {
				atomicCleared = make(map[string]bool)
			}
			atomicCleared[k] = true
			continue
		}
		if entry.hasAtomics {
			// Lazily build server values map on first atomic encounter.
			if serverValues == nil {
				serverValues = make(map[string][]byte, len(serverKVs))
				for _, kv := range serverKVs {
					serverValues[string(kv.Key)] = kv.Value
				}
			}
			// Resolve atomics against server base. nil = absent, non-nil = present
			// (incl. present-empty) — mirroring C++ Optional<ValueRef> so V2 ops
			// (MinV2/AndV2) distinguish "missing → operand" from "present-empty → op".
			base, basePresent := serverValues[k]
			if basePresent && base == nil {
				base = []byte{} // present-empty storage value — keep non-nil
			}
			base, cleared, unresolved := resolveAtomics(base, entry.atomics)
			// Cache resolved value.
			if unresolved {
				// Unreadable entries are outside the capped scan window. Never
				// expose their operands: BYPASS_UNREADABLE applies only to points.
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
			} else if cleared {
				// Site E: a standalone CompareAndClear matched. Cache as a DEPENDENT phantom
				// (absent+dependent) — an is_kv slot for getKey that reads ABSENT here — NOT
				// a cleared range (RFC-058). Still exclude the server entry from this merge
				// (filteredServer was built before atomic resolution) via atomicCleared.
				w.writes[k] = rywEntry{absent: true, dependent: true}
				if atomicCleared == nil {
					atomicCleared = make(map[string]bool)
				}
				atomicCleared[k] = true
				// sortedKeys stays valid: the key remains in w.writes (now a phantom),
				// it is not removed.
			} else if base == nil {
				// Defensive: resolveAtomics normalizes a resolved non-clear chain to
				// non-nil (present-empty). Unreachable today; treat as absent.
			} else {
				// Site E: a resolved standalone atomic is a DEPENDENT_WRITE — preserve the
				// op-type for conflict filtering.
				w.writes[k] = rywEntry{value: base, dependent: true}
				writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: base})
			}
		} else {
			// A plain (non-atomic) entry is always a PRESENT key: cleared keys are
			// removed from w.writes (never tombstoned), so entry.value == nil means
			// "Set to empty bytes" / an atomic resolved to empty — NOT absent. The
			// previous `entry.value != nil` guard dropped such empty-value keys from
			// the merged range (e.g. after a Get resolved a pending Xor(k,"") into a
			// nil-value entry), so getRange disagreed with libfdb_c. Found by the
			// RFC-055 RYW-read differential.
			writeKVs = append(writeKVs, KeyValue{Key: []byte(k), Value: entry.value})
		}
	}
	// writeKVs is sorted ascending (from sortedKeys iteration).

	// Phase 3: Two-pointer merge.
	// filteredServer: sorted in scan direction (forward=ascending, reverse=descending).
	// writeKVs: sorted ascending. Reverse it for reverse scans.
	if reverse {
		for i, j := 0, len(writeKVs)-1; i < j; i, j = i+1, j-1 {
			writeKVs[i], writeKVs[j] = writeKVs[j], writeKVs[i]
		}
	}

	result := make([]KeyValue, 0, len(filteredServer)+len(writeKVs))
	si, wi := 0, 0

	for si < len(filteredServer) || wi < len(writeKVs) {
		if si >= len(filteredServer) {
			result = append(result, writeKVs[wi:]...)
			break
		}
		if wi >= len(writeKVs) {
			// Append remaining server entries, skipping any cleared by atomics.
			for ; si < len(filteredServer); si++ {
				if !atomicCleared[string(filteredServer[si].Key)] {
					result = append(result, filteredServer[si])
				}
			}
			break
		}

		// Skip server entries cleared by atomic resolution.
		if atomicCleared[string(filteredServer[si].Key)] {
			si++
			continue
		}

		cmp := bytes.Compare(filteredServer[si].Key, writeKVs[wi].Key)
		if cmp == 0 {
			// Write shadows server — take write value, skip server.
			result = append(result, writeKVs[wi])
			si++
			wi++
		} else if (reverse && cmp > 0) || (!reverse && cmp < 0) {
			result = append(result, filteredServer[si])
			si++
		} else {
			result = append(result, writeKVs[wi])
			wi++
		}
	}

	return result
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
	c.mutableWritesLocked().addClearedRange(begin, end)
}

// isUnresolvedVersionstamp reports whether op is a versionstamped mutation, which
// cannot be resolved client-side: the 10-byte stamp is assigned at commit, so the
// resolved key (SetVersionstampedKey) or value (SetVersionstampedValue) is unknown
// pre-commit. C++ marks such a key accessed_unreadable; the RYW read paths must read
// it as ABSENT (our approximation) rather than surface a phantom value.
func isUnresolvedVersionstamp(op MutationType) bool {
	return op == MutSetVersionstampedKey || op == MutSetVersionstampedValue
}

// isNonAssociativeOp mirrors C++ isNonAssociativeOp / NON_ASSOCIATIVE_MASK (CommitTransaction.h:576-578):
// ops that obey the associative law ONLY when all operands have equal length. coalesceOverAtomics folds
// two same-type non-associative ops solely when their operand lengths match; the associative ops
// (And, AndV2, ByteMin, ByteMax, AppendIfFits) always fold on same type.
func isNonAssociativeOp(op MutationType) bool {
	switch op {
	case MutAddValue, MutOr, MutXor, MutMax, MutMin, MutMinV2,
		MutSetVersionstampedKey, MutSetVersionstampedValue, MutCompareAndClear:
		return true
	default:
		return false
	}
}

// coalesceOverAtomics folds newMut into the pending atomic chain, a port of C++ WriteMap::coalesceOver
// (WriteMap.cpp:480-494) + coalesce (:357) restricted to Go's representation: the SetValue base is stored
// separately in rywEntry.value and resolved via the eager value-fold at atomic() sites B/C, so this stack
// is a PURE atomic chain (its top is always an atomic op, never a SetValue). Within that restriction the
// two C++ branches reduce to:
//   - same op type, associative OR equal operand length → FOLD (combine operands via applyAtomic, keep the
//     atomic type) — 150k `ADD 1` (all 8-byte) → one `ADD 150000`;
//   - same op type, non-associative, DIFFERENT operand length → PUSH (keep both);
//   - a DIFFERENT op type → PUSH (two atomics of different type never collapse — C++ else-branch
//     `isAtomicOp(new) && isAtomicOp(existing)` → push).
//
// Versionstamped ops and CompareAndClear are always PUSHED (never folded): CompareAndClear is excluded from
// C++'s same-type fold branch (`newEntry.type != CompareAndClear`) and over another atomic pushes; a
// versionstamped op is kept intact so the entry's unreadable/resolve semantics are byte-for-byte unchanged
// (its 10-byte stamp is assigned at commit, so folding cannot combine operands anyway). The fold is
// read-transparent: resolveAtomics over the folded chain yields the identical value.
func coalesceOverAtomics(stack []rywMutation, newMut rywMutation) []rywMutation {
	if len(stack) == 0 || isUnresolvedVersionstamp(newMut.typ) || newMut.typ == MutCompareAndClear {
		return append(stack, newMut)
	}
	top := &stack[len(stack)-1]
	if top.typ != newMut.typ || isUnresolvedVersionstamp(top.typ) {
		return append(stack, newMut) // different type (or the top is an unfoldable versionstamp) → keep both
	}
	if isNonAssociativeOp(top.typ) && len(top.param) != len(newMut.param) {
		return append(stack, newMut) // non-associative + operand-size mismatch → keep both
	}
	// Fold: combine the two operands with the op's own primitive, keeping the atomic type
	// (C++ coalesce same-type branch, e.g. AddValue → doAdd(existing, new) → AddValue).
	//
	// An OPERAND is always PRESENT, including a zero-length one: C++ RYWMutation.value for an
	// atomic op is a present Optional, and the only not-present RYWMutation is the synthetic
	// SetValue bottom pushed over a cleared range (WriteMap.cpp:105/143). Go spells not-present
	// as a nil []byte and applyAtomic reads its BASE argument with that convention, so the
	// operand handed to the base slot must be normalized — otherwise an empty operand is read
	// as ABSENT and takes the absent→operand branch of doAndV2 (Atomic.h:71), doByteMin (:229)
	// or doMinV2 (:222), returning the second operand verbatim instead of combining the two.
	// Two sources produce such a nil: allocBytes(0) on a still-unallocated byteBuf (a
	// zero-length operand arriving first in the transaction), and a fold that itself resolved
	// to empty (ByteMin over ""), whose nil result is the next fold's base. Normalizing here
	// covers both; downstream readers of param use it only in operand position, where the do*
	// primitives test len() and nil is indistinguishable from empty.
	combined, _ := applyAtomic(top.typ, existing(top.param), newMut.param)
	top.param = combined
	return stack
}

// resolveAtomics folds an atomic-mutation chain onto a storage base for the RYW read
// paths (single-key get + range merge). It returns the resolved value and two
// terminal flags:
//
//   - cleared: a CompareAndClear matched → the key is removed from the merged view.
//   - unresolved: the chain contains a versionstamped op → the key is UNREADABLE
//     client-side (C++ accessed_unreadable) and reads as absent. This is terminal and
//     DOMINANT: once a versionstamp appears, a later resolvable op layered on top is
//     still unreadable (an op over an unknown value/key stays unknown), so the rest of
//     the chain is skipped and unresolved wins over cleared.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present);
// base==[]byte{} means "present with empty value". A resolved non-clear op normalizes
// a nil result to []byte{} so a later V2 op (MinV2/AndV2) sees "present" and does the
// op rather than the absent→operand path — matching C++ Optional.present() (Atomic.h).
func resolveAtomics(base []byte, atomics []rywMutation) (result []byte, cleared, unresolved bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			return nil, false, true
		}
		base, cleared = applyAtomic(m.typ, base, m.param)
		if cleared {
			base = nil
		} else if base == nil {
			base = []byte{}
		}
	}
	return base, cleared, false
}

// applyAtomic applies an atomic mutation to a base value, mirroring the C++
// implementations in fdbclient/include/fdbclient/Atomic.h exactly.
//
// Convention: base==nil means "key absent" (C++ Optional<ValueRef> not present).
// base==[]byte{} means "key present with empty value".
// Returns (result, cleared). cleared=true means the key should be removed
// (only happens for CompareAndClear).
func applyAtomic(op MutationType, base, param []byte) (result []byte, cleared bool) {
	switch op {
	case MutSetValue:
		return append([]byte(nil), param...), false
	case MutAddValue:
		return doAdd(base, param), false
	case MutAnd:
		return doAnd(base, param), false
	case MutAndV2:
		return doAndV2(base, param), false
	case MutOr:
		return doOr(base, param), false
	case MutXor:
		return doXor(base, param), false
	case MutMax:
		return doMax(base, param), false
	case MutMin:
		return doMin(base, param), false
	case MutMinV2:
		return doMinV2(base, param), false
	case MutByteMax:
		return doByteMax(base, param), false
	case MutByteMin:
		return doByteMin(base, param), false
	case MutAppendIfFits:
		return doAppendIfFits(base, param), false
	case MutCompareAndClear:
		return doCompareAndClear(base, param)
	default:
		// Versionstamped mutations can't be resolved client-side.
		if base != nil {
			return append([]byte(nil), base...), false
		}
		return nil, false
	}
}

// existing returns the "present" value for C++ Optional<ValueRef> semantics.
// nil → empty StringRef (for operations that treat absent as empty).
func existing(base []byte) []byte {
	if base == nil {
		return []byte{}
	}
	return base
}

// doAdd — C++ doAdd in Atomic.h. Little-endian addition.
// Result length = len(param), matching C++ which allocates otherOperand.size().
// Base bytes beyond len(param) are silently dropped (carry discarded).
func doAdd(base, param []byte) []byte {
	e := existing(base)
	size := len(param)
	if size == 0 {
		return []byte{}
	}
	a := make([]byte, size)
	copy(a, e) // zero-pads if len(e) < size; truncates if len(e) > size
	b := make([]byte, size)
	copy(b, param)

	if size == 8 {
		result := make([]byte, 8)
		binary.LittleEndian.PutUint64(result, binary.LittleEndian.Uint64(a)+binary.LittleEndian.Uint64(b))
		return result
	}
	result := make([]byte, size)
	var carry uint16
	for i := 0; i < size; i++ {
		sum := carry + uint16(a[i]) + uint16(b[i])
		result[i] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// doAnd — C++ doAnd in Atomic.h. Result length = param length. Missing base → 0x00.
func doAnd(base, param []byte) []byte {
	e := existing(base)
	if len(param) == 0 {
		return []byte{}
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] & param[i]
	}
	// Remaining positions: 0x00 (base bytes beyond existing are 0, 0 & anything = 0)
	return result
}

// doAndV2 — C++ doAndV2. If absent → return param. Otherwise → doAnd.
func doAndV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doAnd(base, param)
}

// doOr — C++ doOr. Result length = param length. Missing base → 0x00.
func doOr(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] | param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doXor — C++ doXor. Result length = param length. Missing base → 0x00.
func doXor(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	result := make([]byte, len(param))
	minLen := len(e)
	if minLen > len(param) {
		minLen = len(param)
	}
	for i := 0; i < minLen; i++ {
		result[i] = e[i] ^ param[i]
	}
	for i := minLen; i < len(param); i++ {
		result[i] = param[i]
	}
	return result
}

// doMax — C++ doMax. Little-endian unsigned compare. Result length = param length.
func doMax(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	// Compare from MSB of param down. Extra param bytes beyond existing treated as > 0.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			return append([]byte(nil), param...)
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			return append([]byte(nil), param...)
		} else if param[i] < e[i] {
			// Return existing truncated/zero-padded to param length.
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	return append([]byte(nil), param...)
}

// doMin — C++ doMin. Little-endian unsigned compare. Result length = param length.
func doMin(base, param []byte) []byte {
	if len(param) == 0 {
		return append([]byte(nil), param...)
	}
	e := existing(base)
	// Compare from MSB of param down.
	for i := len(param) - 1; i >= len(e); i-- {
		if param[i] != 0 {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		}
	}
	for i := min(len(e), len(param)) - 1; i >= 0; i-- {
		if param[i] > e[i] {
			result := make([]byte, len(param))
			copy(result, e)
			return result
		} else if param[i] < e[i] {
			return append([]byte(nil), param...)
		}
	}
	return append([]byte(nil), param...)
}

// doMinV2 — C++ doMinV2. If absent → return param. Otherwise → doMin.
func doMinV2(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	return doMin(base, param)
}

// doByteMax — C++ doByteMax. Lexicographic (big-endian byte) comparison.
func doByteMax(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) > 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doByteMin — C++ doByteMin. Lexicographic comparison.
func doByteMin(base, param []byte) []byte {
	if base == nil {
		return append([]byte(nil), param...)
	}
	if bytes.Compare(base, param) < 0 {
		return append([]byte(nil), base...)
	}
	return append([]byte(nil), param...)
}

// doAppendIfFits — C++ doAppendIfFits. Concatenates if within 100KB limit.
func doAppendIfFits(base, param []byte) []byte {
	e := existing(base)
	if len(e) == 0 {
		return append([]byte(nil), param...)
	}
	if len(param) == 0 {
		return append([]byte(nil), e...)
	}
	if len(e)+len(param) > valueSizeLimit { // CLIENT_KNOBS->VALUE_SIZE_LIMIT (pkg const)
		return append([]byte(nil), e...)
	}
	result := make([]byte, len(e)+len(param))
	copy(result, e)
	copy(result[len(e):], param)
	return result
}

// doCompareAndClear — C++ doCompareAndClear. If absent or equal → clear.
func doCompareAndClear(base, param []byte) ([]byte, bool) {
	if base == nil || bytes.Equal(base, param) {
		return nil, true // Clear the value.
	}
	return append([]byte(nil), base...), false // No change.
}

// subtractRangeList removes [begin, end) from a sorted, non-overlapping range
// list, splitting ranges that straddle the span. Used to make cleared spans
// readable again (C++ gets this free from the shared PTree: clear() inserts
// readable entries over the span, WriteMap.cpp:195).
func subtractRangeList(ranges []rywRange, begin, end []byte) []rywRange {
	if len(ranges) == 0 || bytes.Compare(begin, end) >= 0 {
		return ranges
	}
	out := make([]rywRange, 0, len(ranges)+1)
	for _, r := range ranges {
		// No overlap: keep as-is.
		if bytes.Compare(r.end, begin) <= 0 || bytes.Compare(r.begin, end) >= 0 {
			out = append(out, r)
			continue
		}
		// Left remainder.
		if bytes.Compare(r.begin, begin) < 0 {
			out = append(out, rywRange{begin: r.begin, end: append([]byte(nil), begin...)})
		}
		// Right remainder.
		if bytes.Compare(r.end, end) > 0 {
			out = append(out, rywRange{begin: append([]byte(nil), end...), end: r.end})
		}
	}
	return out
}

// resolveAtomicsBypass resolves a chain CONTAINING versionstamped ops for a
// BYPASS_UNREADABLE read: each versionstamped op applies as a plain Set of its
// operand exactly as written — placeholder bytes unfilled, trailing 4-byte
// offset suffix INCLUDED (C++ kv() under bypass returns the write-map value;
// the RYWIterator.cpp:433-449 unit pins kv->value == metadataVersionRequiredValue,
// all 14 bytes). Mirrors resolveAtomics for everything else. Never cached.
func resolveAtomicsBypass(base []byte, atomics []rywMutation) (result []byte, cleared bool) {
	for _, m := range atomics {
		if isUnresolvedVersionstamp(m.typ) {
			base = m.param
			cleared = false
			continue
		}
		val, clr := applyAtomic(m.typ, base, m.param)
		if !clr && val == nil {
			val = []byte{} // present-empty, matching resolveAtomics normalization
		}
		base = val
		cleared = clr
	}
	return base, cleared
}

// addUnreadableRange marks [begin, end) unreadable (SVK candidate stamp
// range, RFC-098).
func (c *rywCache) addUnreadableRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.mutableWritesLocked()
	if b, e, ok := nodeRange(begin, end); ok {
		w.nodes.addUnmodifiedAndUnreadableRange(b, e) // RYW:2272
	}
	w.addUnreadableRange(begin, end)
}

// nodeKey reports a key inside the keyspace the write map's nodes cover. C++
// rejects every write and conflict range past getMaxWriteKey before it reaches
// the write map (RYW:2353, 2466-2468); Go buffers an illegal Set or Clear until
// commit, so the nodes skip what lies at or past the \xff\xff sentinel.
func nodeKey(key []byte) bool { return bytes.Compare(key, allKeysEnd) < 0 }

// nodeRange is [begin, end) clipped to the nodes' keyspace, copied for the nodes
// (which keep their keys); ok is false when nothing remains.
func nodeRange(begin, end []byte) (b, e []byte, ok bool) {
	if bytes.Compare(end, allKeysEnd) > 0 {
		end = allKeysEnd
	}
	if bytes.Compare(begin, end) >= 0 {
		return nil, nil, false
	}
	return bytes.Clone(begin), bytes.Clone(end), true
}

// addConflictRange is RYW addWriteConflictRange with RYW enabled: conflict-only
// nodes in the write map, no operation (ReadYourWrites.actor.cpp:2493-2499,
// WriteMap.cpp:244-302).
func (c *rywCache) addConflictRange(begin, end []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, e, ok := nodeRange(begin, end); ok {
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
