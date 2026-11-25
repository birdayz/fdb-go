package client

import (
	"bytes"
	"slices"
	"sort"
)

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
	return c.liveLocked().materializeCommit()
}

// materializeCommit is rywCache.materializeCommit on a version locked or pinned; keys and values
// alias the write map's bytes, which are never written.
func (w *rywWrites) materializeCommit() []Mutation {
	w.ensureSorted()
	var a boundaryArena
	out := make([]Mutation, 0, w.cleared.Len()+w.numWrites())
	// Pass 1 — clears (w.cleared is kept sorted + non-overlapping by addClearedRangeLocked).
	// A key with an operation is not a cleared segment of the C++ write map, so
	// it ends the cleared stretch: the clear resumes after it
	// (writeRangeToNativeTransaction, ReadYourWrites.actor.cpp:2001-2018).
	var wc pfinger[rywWrite]
	for rc := w.cleared.finger(0); rc.valid(); rc.next() {
		r := rc.item()
		begin := r.begin
		for wc.seekFirst(&w.sorted, func(x *rywWrite) bool { return x.key >= string(r.begin) }); wc.valid() && wc.item().key < string(r.end); wc.next() {
			key := wc.item().key
			if string(begin) < key {
				out = append(out, Mutation{Type: MutClearRange, Key: begin, Value: stringBytes(key)})
			}
			begin = a.bytes(rywBoundary{key: stringBytes(key), after: true})
		}
		if bytes.Compare(begin, r.end) < 0 {
			out = append(out, Mutation{Type: MutClearRange, Key: begin, Value: r.end})
		}
	}
	// Pass 2 — per-key operations in sorted key order.
	for wc := w.sorted.finger(0); wc.valid(); wc.next() {
		e := wc.item().e
		key := stringBytes(wc.item().key)
		switch {
		case e.hasAtomics:
			for _, m := range e.atomics {
				out = append(out, Mutation{Type: m.typ, Key: key, Value: m.param})
			}
		case e.absent:
			// A matched CompareAndClear resolved to "no value" → C++ SetValue(absent) → tr.clear(key),
			// i.e. a single-key ClearRange [key, key+\x00). (An absent entry is an operation key in the
			// write map, emitted in pass 2, NOT a cleared range from pass 1.)
			out = append(out, Mutation{Type: MutClearRange, Key: key, Value: a.bytes(rywBoundary{key: key, after: true})})
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
// Works from the snapshot (sized) — NOT a live re-read of tx.writeConflicts — so a Set racing this
// Commit cannot ship an unvalidated/unsized conflict range whose mutation was excluded from the frozen
// op-log. Pure + unit-testable. A coalesced sized takes the range in one merge pass, not a re-sort.
func finalizeShipConflicts(sized []KeyRange, sc KeyRange, scAdded, coalesced bool) []KeyRange {
	if !scAdded {
		return sized
	}
	if coalesced {
		return insertCoalesced(sized, sc)
	}
	return append(append(make([]KeyRange, 0, len(sized)+1), sized...), sc)
}

// insertCoalesced returns coalesceOwnedWriteConflicts(rs + r) as a new vector for an rs it produced:
// r absorbs the ranges it overlaps or touches.
func insertCoalesced(rs []KeyRange, r KeyRange) []KeyRange {
	i := sort.Search(len(rs), func(i int) bool { return bytes.Compare(rs[i].End, r.Begin) >= 0 })
	j := i
	for j < len(rs) && bytes.Compare(rs[j].Begin, r.End) <= 0 {
		j++
	}
	merged := r
	if i < j {
		if bytes.Compare(rs[i].Begin, merged.Begin) < 0 {
			merged.Begin = rs[i].Begin
		}
		if bytes.Compare(rs[j-1].End, merged.End) > 0 {
			merged.End = rs[j-1].End
		}
	}
	out := make([]KeyRange, 0, len(rs)-(j-i)+1)
	out = append(out, rs[:i]...)
	out = append(out, merged)
	return append(out, rs[j:]...)
}
