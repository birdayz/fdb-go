package simfdb

import (
	"bytes"
	"sort"
)

// A read that the transaction's OWN pending writes already answered did not read the database,
// so it must take no read conflict. Without that filter every `Set(K); Get(K)` conflicts on K
// and a concurrent writer of K aborts a transaction that never depended on K's stored value —
// systematic over-conflict, and a SimFDB that certifies spurious 1020s.
//
// This file ports the client's filter: conflictForKeyLocked and conflictRangesLocked
// (client/ryw.go, C++ ReadYourWrites.actor.cpp updateConflictMap). The rule is the same in both
// forms — a segment conflicts iff it is an UNMODIFIED gap or a DEPENDENT operation:
//
//   - UNMODIFIED gap (no pending write, not locally cleared) → CONFLICT. A database read
//     resolved it.
//   - INDEPENDENT operation (a plain Set, an atomic folded over a Set, an atomic over a
//     locally-cleared base, a versionstamped op) → NO conflict. The value came entirely from
//     the buffer.
//   - DEPENDENT operation (a standalone atomic, which had to read the stored base to resolve)
//     → CONFLICT, over exactly the single key [k, keyAfter(k)).
//   - CLEARED range with no operation key → NO conflict. The transaction knows it is empty.
//
// The independent/dependent split is what makes the subtraction SAFE. Dropping every written
// key would under-conflict: a standalone `Add(K, 1)` genuinely read K's stored value, so a
// concurrent writer of K must still abort it.

// writeEntry is one key's operation-level classification in the replayed write map. Only the
// DEPENDENT/INDEPENDENT distinction matters for conflicts; the resolved value is irrelevant.
type writeEntry struct {
	// dependent marks a C++ DEPENDENT_WRITE: the operation resolved against the STORED base,
	// so the transaction did read the database at this key. Set once, at the bottom of the
	// operation stack, and carried through later folds — a plain Set REPLACES the stack
	// (client/ryw.go set(): the entry is rebuilt with dependent=false), while an atomic
	// coalesces onto it and leaves the bottom alone.
	dependent bool
}

// writeMap is the RYW buffer replayed into the two structures the conflict filter consults:
// per-key operations and the locally-cleared ranges.
type writeMap struct {
	ops     map[string]writeEntry
	cleared []keyRange // sorted by begin, non-overlapping
}

// buildWriteMap replays tx.buffer in issue order into a writeMap.
//
// It is rebuilt per conflict decision rather than maintained incrementally. The sim's buffers
// are small and its reads are synchronous, so the cost is irrelevant here — unlike in the
// client, where routing the hot single-key Get path through the range walk made a write-heavy
// transaction quadratic and is why conflictForKeyLocked exists as a separate fast path.
func (tx *simTxn) buildWriteMap() writeMap {
	wm := writeMap{ops: make(map[string]writeEntry, len(tx.buffer))}
	for _, m := range tx.buffer {
		switch m.kind {
		case mutSet:
			// A plain Set replaces the operation stack: the key's value is now fully known
			// locally whatever came before, so the entry is INDEPENDENT.
			wm.ops[string(m.key)] = writeEntry{}
		case mutVersionstampedKey, mutVersionstampedValue:
			// C++ marks SetVersionstamped* INDEPENDENT (the operand is written as given; the
			// stamp comes from the commit proxy, not from the stored value).
			wm.ops[string(m.key)] = writeEntry{}
		case mutClear:
			delete(wm.ops, string(m.key))
			wm.addCleared(m.key, keyAfter(m.key))
		case mutClearRange:
			for k := range wm.ops {
				if bytes.Compare([]byte(k), m.key) >= 0 && bytes.Compare([]byte(k), m.end) < 0 {
					delete(wm.ops, k)
				}
			}
			wm.addCleared(m.key, m.end)
		case mutAtomic:
			if _, ok := wm.ops[string(m.key)]; ok {
				// Fold onto an existing entry: the stack bottom — and therefore the
				// dependence — is unchanged.
				continue
			}
			// A standalone atomic is DEPENDENT (it resolves against the stored base) UNLESS
			// the base is a range this transaction already cleared, in which case the base is
			// known-empty locally and C++ pushes a synthetic SetValue at the bottom
			// (WriteMap.cpp:102-111) — INDEPENDENT.
			wm.ops[string(m.key)] = writeEntry{dependent: !wm.isCleared(m.key)}
		}
	}
	return wm
}

// addCleared inserts [begin,end) into the cleared list, keeping it sorted and non-overlapping.
func (wm *writeMap) addCleared(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	merged := keyRange{begin: append([]byte(nil), begin...), end: append([]byte(nil), end...)}
	out := wm.cleared[:0:0]
	for _, r := range wm.cleared {
		switch {
		case bytes.Compare(r.end, merged.begin) < 0 || bytes.Compare(merged.end, r.begin) < 0:
			out = append(out, r) // disjoint
		default:
			if bytes.Compare(r.begin, merged.begin) < 0 {
				merged.begin = r.begin
			}
			if bytes.Compare(r.end, merged.end) > 0 {
				merged.end = r.end
			}
		}
	}
	out = append(out, merged)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].begin, out[j].begin) < 0 })
	wm.cleared = out
}

// isCleared reports whether key falls inside a locally-cleared range.
func (wm *writeMap) isCleared(key []byte) bool {
	i := sort.Search(len(wm.cleared), func(i int) bool {
		return bytes.Compare(wm.cleared[i].begin, key) > 0
	})
	if i == 0 {
		return false
	}
	r := wm.cleared[i-1]
	return bytes.Compare(key, r.end) < 0
}

// conflictForKey is the single-key form: conflict iff the key sits in an UNMODIFIED gap or a
// DEPENDENT operation. Port of client/ryw.go conflictForKeyLocked (C++ updateConflictMap(ryw,
// key, it)).
func (wm *writeMap) conflictForKey(key []byte) bool {
	if e, ok := wm.ops[string(key)]; ok {
		return e.dependent
	}
	if wm.isCleared(key) {
		return false
	}
	return true
}

// conflictRanges walks [begin,end) and returns the maximal sub-ranges that must be recorded as
// read conflicts. Port of client/ryw.go conflictRangesLocked (C++ updateConflictMap): the
// window is cut at every operation key, its keyAfter, and every clamped cleared bound; each
// resulting segment is classified and the qualifying ones are coalesced.
func (wm *writeMap) conflictRanges(begin, end []byte) []keyRange {
	if bytes.Compare(begin, end) >= 0 {
		return nil
	}
	bounds := [][]byte{append([]byte(nil), begin...), append([]byte(nil), end...)}
	add := func(b []byte) {
		if bytes.Compare(b, begin) > 0 && bytes.Compare(b, end) < 0 {
			bounds = append(bounds, append([]byte(nil), b...))
		}
	}
	for k := range wm.ops {
		kb := []byte(k)
		if bytes.Compare(kb, begin) < 0 || bytes.Compare(kb, end) >= 0 {
			continue
		}
		add(kb)
		add(keyAfter(kb))
	}
	for _, r := range wm.cleared {
		if bytes.Compare(r.begin, end) >= 0 {
			break
		}
		if bytes.Compare(r.end, begin) <= 0 {
			continue
		}
		add(r.begin)
		add(r.end)
	}
	sort.Slice(bounds, func(i, j int) bool { return bytes.Compare(bounds[i], bounds[j]) < 0 })

	var out []keyRange
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		if bytes.Equal(lo, hi) {
			continue
		}
		conflict := true
		if e, ok := wm.ops[string(lo)]; ok && bytes.Equal(hi, keyAfter(lo)) {
			// The segment IS the operation key's own single-key slot.
			conflict = e.dependent
		} else if wm.isCleared(lo) {
			conflict = false
		}
		if !conflict {
			continue
		}
		if n := len(out); n > 0 && bytes.Equal(out[n-1].end, lo) {
			out[n-1].end = append([]byte(nil), hi...)
			continue
		}
		out = append(out, keyRange{begin: append([]byte(nil), lo...), end: append([]byte(nil), hi...)})
	}
	return out
}

// addFilteredReadConflictKey records the read conflict for a single-key read, filtered through
// the write map.
//
// With read-your-writes DISABLED the read went straight to storage and the write map answered
// nothing, so the full conflict is recorded — the same rywDisabled/else split the client makes
// at every one of these sites (client/transaction.go addReadConflictForKeyRYW,
// addGetKeyConflictRange, AddReadConflictRange). Filtering a bypassed write map would subtract
// a segment the transaction really did read from the database and MISS a conflict.
func (tx *simTxn) addFilteredReadConflictKey(key []byte) {
	if tx.rywDisabled {
		tx.addReadConflict(key, keyAfter(key))
		return
	}
	wm := tx.buildWriteMap()
	if wm.conflictForKey(key) {
		tx.addReadConflict(key, keyAfter(key))
	}
}

// addFilteredReadConflict records a range read conflict, filtered through the write map.
func (tx *simTxn) addFilteredReadConflict(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	if tx.rywDisabled {
		tx.addReadConflict(begin, end)
		return
	}
	wm := tx.buildWriteMap()
	for _, r := range wm.conflictRanges(begin, end) {
		tx.addReadConflict(r.begin, r.end)
	}
}
