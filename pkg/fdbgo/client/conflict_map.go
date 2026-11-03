package client

import (
	"bytes"
	"slices"
)

// conflict_map.go keeps a RYW transaction's conflict ranges where C++ keeps
// them. Write conflicts are flags on the write map's nodes (WriteMap entries'
// is_conflict / following_keys_conflict, WriteMap.h:72-76), set by the write
// that adds them under the same lock as its mutation; read conflicts are a
// coalescing range map (ReadYourWritesTransaction::readConflicts, a
// CoalescedKeyRefRangeMap<bool>, ReadYourWrites.h:237) whose bounds live in the
// transaction's conflict arena. Commit builds both vectors in one ordered pass
// (writeRangeToNativeTransaction, ReadYourWrites.actor.cpp:1392-1399, 2020-2071).
// Without RYW the native transaction's vectors are kept as added, in
// tx.readConflicts and tx.writeConflicts, which also hold the self-conflicting
// ranges and whatever a caller placed there directly.

// boundaryArena materializes the keyAfter bounds of vectors built from the
// write map; plain bounds alias write-map keys, which are never written.
type boundaryArena struct{ buf []byte }

func (a *boundaryArena) bytes(b rywBoundary) []byte {
	if !b.after {
		return b.key
	}
	n := b.size()
	if cap(a.buf)-len(a.buf) < n {
		a.buf = make([]byte, 0, max(4096, n))
	}
	s := a.buf[len(a.buf) : len(a.buf)+n : len(a.buf)+n]
	copy(s, b.key)
	clear(s[len(b.key):])
	a.buf = a.buf[:len(a.buf)+n]
	return s
}

// appendWriteConflicts appends the write-conflict ranges of a built node
// sequence: writeRangeToNativeTransaction's conflict pass over
// [StringRef(), allKeys.end) (ReadYourWrites.actor.cpp:2020-2033, 2069-2071),
// a range per maximal run of conflict segments, in key order. Segments follow
// WriteMap::iterator (WriteMap.cpp:312-322): an operation node is its key's
// segment [k, keyAfter(k)) with is_conflict, then, unless the next node is
// keyAfter(k), the range to the next node with following_keys_conflict; any
// other node starts the range to the next node.
//
// Nodes not yet built hold their operations in the log; the conflict runs are
// then the union of the logged conflict ranges, coalesced, which is what
// replaying them would flag (WriteMap only ever sets conflict flags), without
// building the nodes.
func (n *rywNodes) appendWriteConflicts(dst []KeyRange, a *boundaryArena) []KeyRange {
	return n.appendWriteConflictsIn(dst, a, allKeysBegin, allKeysEnd)
}

// appendWriteConflictsIn is appendWriteConflicts over [lo, hi): the runs
// clipped to it, as writeRangeToNativeTransaction(keys) emits them.
func (n *rywNodes) appendWriteConflictsIn(dst []KeyRange, a *boundaryArena, lo, hi []byte) []KeyRange {
	if !n.built {
		start := len(dst)
		add := func(b, e rywBoundary) {
			if b.compare(rywBoundary{key: lo}) < 0 {
				b = rywBoundary{key: lo}
			}
			if e.compare(rywBoundary{key: hi}) > 0 {
				e = rywBoundary{key: hi}
			}
			if b.compare(e) < 0 {
				dst = append(dst, KeyRange{Begin: a.bytes(b), End: a.bytes(e)})
			}
		}
		for _, op := range n.log {
			switch {
			case op.kind == rywNodeOpMutate && op.addConflict:
				add(rywBoundary{key: op.begin}, rywBoundary{key: op.begin, after: true})
			case op.kind == rywNodeOpClear && op.addConflict, op.kind == rywNodeOpConflict:
				add(rywBoundary{key: op.begin}, rywBoundary{key: op.end})
			}
		}
		return append(dst[:start], coalesceOwnedWriteConflicts(dst[start:])...)
	}
	low, end := rywBoundary{key: lo}, rywBoundary{key: hi}
	in := false
	var from rywBoundary
	segment := func(begin rywBoundary, conflict bool) {
		if begin.compare(low) < 0 {
			begin = low // max(keys.begin, it.beginKey()) (ReadYourWrites.actor.cpp:2026)
		}
		switch {
		case conflict && !in:
			from, in = begin, true
		case !conflict && in:
			if from.compare(begin) < 0 {
				dst = append(dst, KeyRange{Begin: a.bytes(from), End: a.bytes(begin)})
			}
			in = false
		}
	}
	// From the node whose segments contain lo.
	first := max(0, n.seq.search(func(nd *rywNode) bool { return bytes.Compare(nd.key, lo) > 0 })-1)
	for c := n.seq.cursor(first); c.valid(); c.next() {
		e := c.item()
		begin := rywBoundary{key: e.key}
		if begin.compare(end) >= 0 {
			break
		}
		if e.op {
			segment(begin, e.isConflict)
			next := c.peekNext()
			if next == nil || equalsKeyAfter(e.key, next.key) {
				continue
			}
			begin = begin.keyAfter()
			if begin.compare(end) >= 0 {
				break
			}
		}
		segment(begin, e.followingConflict)
	}
	if in && from.compare(end) < 0 {
		dst = append(dst, KeyRange{Begin: a.bytes(from), End: hi})
	}
	return dst
}

// insertReadConflictLocked merges [b, e) into the read-conflict map (readConflicts.insert, RYW:328,
// 345): a covered range copies nothing; new outer bounds go to the conflict arena. Caller holds conflictMu.
func (tx *Transaction) insertReadConflictLocked(b, e rywBoundary) {
	if b.compare(e) >= 0 {
		return
	}
	s := &tx.readMap
	// lo is the first range ending at or after b: the first it overlaps or touches.
	var c pfinger[rywRange]
	c.seekFirst(s, func(r *rywRange) bool { return (rywBoundary{key: r.end}).compare(b) >= 0 })
	lo := c.idx
	hi := lo
	var merged rywRange
	keepBegin, keepEnd := false, false
	if c.valid() {
		first := c.item()
		if (rywBoundary{key: first.begin}).compare(e) <= 0 {
			keepBegin = (rywBoundary{key: first.begin}).compare(b) <= 0
			if keepBegin && (rywBoundary{key: first.end}).compare(e) >= 0 {
				return // covered
			}
			// The ranges beginning at or before e join it; ranges never touch, so
			// they are consecutive from lo.
			last := first
			for c.next(); c.valid() && (rywBoundary{key: c.item().begin}).compare(e) <= 0; c.next() {
				last = c.item()
				hi++
			}
			hi++ // first
			keepEnd = (rywBoundary{key: last.end}).compare(e) >= 0
			merged.begin, merged.end = first.begin, last.end
		}
	}
	if !keepBegin {
		merged.begin = tx.ownBoundaryLocked(b)
	}
	if !keepEnd {
		merged.end = tx.ownBoundaryLocked(e)
	}
	switch hi - lo {
	case 0:
		s.insert(lo, merged)
	case 1:
		s.set(lo, merged)
	default:
		s.delete(lo, hi)
		s.insert(lo, merged)
	}
}

// ownBoundaryLocked copies a bound into the conflict arena. Caller holds conflictMu.
func (tx *Transaction) ownBoundaryLocked(b rywBoundary) []byte {
	buf := tx.conflictBufAlloc(b.size())
	copy(buf, b.key)
	clear(buf[len(b.key):]) // keyAfter's zero bytes; a pooled buffer may hold stale data
	return buf
}

// addReadConflictSpansLocked adds a read's filtered conflict ranges, charging
// GetApproximateSize per range as the list did. Caller holds conflictMu.
func (tx *Transaction) addReadConflictSpansLocked(spans []conflictSpan) {
	for _, s := range spans {
		if tx.rywDisabled {
			tx.addReadConflictLocked(s.begin.materialize(), s.end.materialize())
			continue
		}
		tx.rywConflictSize += int64(s.begin.size()+s.end.size()) + sizeofKeyRangeRef
		tx.insertReadConflictLocked(s.begin, s.end)
	}
}

// addReadConflictLocked adds [begin, end): to the read-conflict map with RYW,
// else to the native list as added. Caller holds conflictMu.
func (tx *Transaction) addReadConflictLocked(begin, end []byte) {
	if !tx.rywDisabled {
		tx.rywConflictSize += int64(len(begin)+len(end)) + sizeofKeyRangeRef
		tx.insertReadConflictLocked(rywBoundary{key: begin}, rywBoundary{key: end})
		return
	}
	buf := tx.conflictBufAlloc(len(begin) + len(end))
	nb := len(begin)
	copy(buf, begin)
	copy(buf[nb:], end)
	tx.readConflicts = append(tx.readConflicts, KeyRange{Begin: buf[:nb:nb], End: buf[nb:]})
}

// conflictSnapshot is what Commit freezes with the mutations, under conflictMu.
type conflictSnapshot struct {
	ryw bool // the conflicts are the write map's and the read-conflict map's
	// view is the pinned write-map version with RYW, nil without or when the
	// transaction wrote nothing. Its nodes may be built by a later read; nodes is
	// their state when frozen.
	view  *rywWrites
	nodes rywNodes
	// readMap is the read-conflict map with RYW.
	readMap pseq[rywRange]
	// reads and writes are the conflict lists (headers frozen; the lists are
	// append-only): everything without RYW, else only what was placed there.
	reads, writes []KeyRange
}

// snapshotConflictsLocked freezes the conflicts with the mutations. The write map is
// updated under conflictMu too, so the pinned version holds exactly the frozen
// operations. Caller holds conflictMu; the snapshot must be released.
func (tx *Transaction) snapshotConflictsLocked() conflictSnapshot {
	s := conflictSnapshot{
		reads:  tx.readConflicts[:len(tx.readConflicts):len(tx.readConflicts)],
		writes: tx.writeConflicts[:len(tx.writeConflicts):len(tx.writeConflicts)],
	}
	if tx.rywDisabled {
		return s
	}
	s.ryw = true
	s.readMap = tx.readMap.share()
	tx.ryw.mu.Lock()
	if len(tx.mutations) == 0 && tx.ryw.writesEmptyLocked() {
		tx.ryw.mu.Unlock()
		return s // nothing to pin: a read-only commit ships no write-map state
	}
	s.view = tx.ryw.pinWritesForCommitLocked()
	if n := &s.view.nodes; n.built {
		s.nodes = rywNodes{seq: n.seq.share(), built: true}
	} else {
		s.nodes = rywNodes{log: n.log[:len(n.log):len(n.log)]} // logged ops are never written
	}
	tx.ryw.mu.Unlock()
	return s
}

// release unpins the snapshot's write-map version.
func (s *conflictSnapshot) release(tx *Transaction) {
	if s.view != nil {
		tx.ryw.mu.Lock()
		defer tx.ryw.mu.Unlock()
		tx.ryw.unpinWritesLocked(s.view)
		s.view = nil
	}
}

// writeConflicts returns the snapshot's write-conflict ranges: the list as added
// without RYW; with RYW the nodes' conflict runs, coalesced with anything in the
// list. fresh reports a vector the caller owns.
func (s *conflictSnapshot) writeConflicts() (ranges []KeyRange, fresh bool) {
	if !s.ryw {
		return s.writes, false
	}
	if s.view == nil && len(s.writes) == 0 {
		return nil, false
	}
	var a boundaryArena
	out := s.nodes.appendWriteConflicts(nil, &a)
	if len(s.writes) > 0 {
		out = coalesceOwnedWriteConflicts(append(out, s.writes...))
	}
	return out, true
}

// readConflicts returns the read-conflict ranges a commit ships: the list as
// added without RYW (shipReadConflicts); with RYW the map's ranges in order,
// coalesced with anything in the list. Always a new vector.
func (s *conflictSnapshot) readConflicts(rywDisabled bool) []KeyRange {
	if !s.ryw {
		return shipReadConflicts(s.reads, rywDisabled)
	}
	out := make([]KeyRange, 0, s.readMap.Len()+len(s.reads))
	for c := s.readMap.cursor(0); c.valid(); c.next() {
		r := c.item()
		out = append(out, KeyRange{Begin: r.begin, End: r.end})
	}
	if len(s.reads) > 0 {
		out = readConflictMap(append(out, s.reads...))
	}
	return out
}

// conflictRangesNowLocked returns the transaction's current read and write
// conflict ranges as a commit would ship them, for the paths that need them
// outside a commit (the dummy transaction, OnError's maybe-committed retry), as new
// vectors. Caller holds conflictMu.
func (tx *Transaction) conflictRangesNowLocked() (reads, writes []KeyRange) {
	s := tx.snapshotConflictsLocked()
	defer s.release(tx)
	writes, fresh := s.writeConflicts()
	if !fresh {
		writes = slices.Clone(writes)
	}
	return s.readConflicts(tx.rywDisabled), writes
}

// takeWriteConflictLocked reports whether a write adds its conflict range: not under SetWriteConflictsDisabled,
// nor under NEXT_WRITE_NO_WRITE_CONFLICT_RANGE, which it consumes (RYW:2085-2091). Caller holds conflictMu.
func (tx *Transaction) takeWriteConflictLocked() bool {
	if tx.writeConflictsDisabled {
		return false
	}
	if tx.nextWriteNoConflict {
		tx.nextWriteNoConflict = false
		return false
	}
	return true
}

// recordWriteConflictLocked records [begin, end): with RYW the caller puts it in the write map and only the
// size is charged here; without RYW the native list keeps it. Caller holds conflictMu.
func (tx *Transaction) recordWriteConflictLocked(begin, end []byte, owned bool) {
	if !tx.rywDisabled {
		tx.rywConflictSize += int64(len(begin)+len(end)) + sizeofKeyRangeRef
		return
	}
	tx.appendWriteConflictListLocked(begin, end, owned)
}

// appendWriteConflictListLocked appends [begin, end) to the native list, copied
// into the conflict arena unless owned. Caller holds conflictMu.
func (tx *Transaction) appendWriteConflictListLocked(begin, end []byte, owned bool) {
	if !owned {
		buf := tx.conflictBufAlloc(len(begin) + len(end))
		nb := len(begin)
		copy(buf, begin)
		copy(buf[nb:], end)
		begin, end = buf[:nb:nb], buf[nb:]
	}
	tx.writeConflicts = append(tx.writeConflicts, KeyRange{Begin: begin, End: end})
}

// writeConflictLocked is a write's conflict range: taken, then recorded.
func (tx *Transaction) writeConflictLocked(begin, end []byte, owned bool) bool {
	if !tx.takeWriteConflictLocked() {
		return false
	}
	tx.recordWriteConflictLocked(begin, end, owned)
	return true
}

// explicitWriteConflictLocked is addWriteConflictRange's: it ignores the next-write flag, which only writes
// consume (RYW:2461-2500 vs :2220, 2328, 2358, 2407). Caller holds conflictMu.
func (tx *Transaction) explicitWriteConflictLocked(begin, end []byte, owned bool) bool {
	if tx.writeConflictsDisabled {
		return false
	}
	tx.recordWriteConflictLocked(begin, end, owned)
	return true
}

// writeConflictPointLocked is writeConflictLocked for [key, keyAfter(key)),
// which only the native list materializes. Caller holds conflictMu.
func (tx *Transaction) writeConflictPointLocked(key []byte) bool {
	if !tx.takeWriteConflictLocked() {
		return false
	}
	if !tx.rywDisabled {
		tx.rywConflictSize += int64(2*len(key)+1) + sizeofKeyRangeRef
		return true
	}
	tx.appendWriteConflictPointListLocked(key)
	return true
}

// commitVectors builds the mutations and write conflicts a commit sizes and
// ships (#28 / RFC-172). With RYW it is writeRangeToNativeTransaction's pass
// over the frozen write map (ReadYourWrites.actor.cpp:1392, 1997-2071): the
// coalesced mutations and the nodes' conflict runs; a transaction carrying a
// versionstamped op ships its op-log as is (coalesceCommitVectors) with the
// write map's conflicts. Without RYW the op-log and the conflict list ship as
// added. mutsOwned and conflictsFresh report new vectors; coalesced reports
// conflicts sorted and merged.
func (tx *Transaction) commitVectors(muts []Mutation, s *conflictSnapshot) (shipMuts []Mutation, mutsOwned bool, conflicts []KeyRange, conflictsFresh, coalesced bool) {
	if !s.ryw {
		shipMuts, conflicts, _ = coalesceCommitVectors(muts, s.writes, true)
		return shipMuts, false, conflicts, false, false
	}
	conflicts, conflictsFresh = s.writeConflicts()
	if s.view == nil || mutationsHaveVersionstamp(muts) {
		return muts, false, conflicts, conflictsFresh, true // no writes, or the op-log as is
	}
	return s.view.materializeCommit(), true, conflicts, conflictsFresh, true
}

// readConflictRangesLocked returns the read conflicts as a new vector: without
// RYW the list as added; with it the read-conflict map, coalesced with anything
// in the list. Caller holds conflictMu.
func (tx *Transaction) readConflictRangesLocked() []KeyRange {
	if tx.rywDisabled {
		return slices.Clone(tx.readConflicts)
	}
	out := make([]KeyRange, 0, tx.readMap.Len()+len(tx.readConflicts))
	for c := tx.readMap.cursor(0); c.valid(); c.next() {
		out = append(out, KeyRange{Begin: c.item().begin, End: c.item().end})
	}
	if len(tx.readConflicts) > 0 {
		out = readConflictMap(append(out, tx.readConflicts...))
	}
	return out
}

// readConflictRanges returns the transaction's read conflicts as a new vector, as
// a commit would ship them before the self-conflicting range: with RYW sorted and
// coalesced (the map, merged with anything in the list), without it as added.
func (tx *Transaction) readConflictRanges() []KeyRange {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	return tx.readConflictRangesLocked()
}

// writeConflictRanges returns the transaction's write conflicts as a new vector,
// as a commit would ship them before the self-conflicting range: with RYW the
// write map's conflict runs, sorted and coalesced with anything in the list;
// without it the list as added.
func (tx *Transaction) writeConflictRanges() []KeyRange {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if tx.rywDisabled {
		return slices.Clone(tx.writeConflicts)
	}
	_, writes := tx.conflictRangesNowLocked()
	return writes
}
