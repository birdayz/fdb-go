package client

import (
	"bytes"
	"slices"
)

// With RYW, write conflicts are write-map node flags (WriteMap.h:72-76) and read conflicts a coalescing
// map (ReadYourWrites.h:237), as in C++; without it, tx.readConflicts and tx.writeConflicts as added.

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

// appendWriteConflicts appends a range per run of conflict segments (ReadYourWrites.actor.cpp:2020-2033);
// unbuilt nodes give the logged ranges' union, which is what replay would flag: WriteMap only sets them.
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
	c := &tx.readMapFinger
	c.seekFirst(s, func(r *rywRange) bool { return (rywBoundary{key: r.end}).compare(b) >= 0 })
	lo := c.idx
	hi := lo
	var merged rywRange
	var firstNode *pnode[rywRange]
	keepBegin, keepEnd := false, false
	if c.valid() {
		firstNode = c.node()
		first := &firstNode.item
		if (rywBoundary{key: first.begin}).compare(e) <= 0 {
			keepBegin = (rywBoundary{key: first.begin}).compare(b) <= 0
			if keepBegin && (rywBoundary{key: first.end}).compare(e) >= 0 {
				return // covered
			}
			// The ranges beginning at or before e join it; ranges never touch, so
			// they are consecutive from lo.
			last := first
			if (rywBoundary{key: first.end}).compare(e) < 0 {
				for c.next(); c.valid() && (rywBoundary{key: c.item().begin}).compare(e) <= 0; c.next() {
					last = c.item()
					hi++
				}
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
		*s.mutNode(lo, firstNode) = merged
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
	// view is the pinned write-map version, nil without RYW or writes; a later read may build its
	// nodes, so nodes keeps their state when frozen.
	view  *rywWrites
	nodes rywNodes
	// readMap is the read-conflict map with RYW.
	readMap pseq[rywRange]
	// reads and writes are the conflict lists (headers frozen; the lists are
	// append-only): everything without RYW, else only what was placed there.
	reads, writes []KeyRange
}

// snapshotConflictsLocked freezes the conflicts with the mutations, which also change under conflictMu.
// Caller holds conflictMu; the snapshot must be released.
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
		// A later read builds these nodes by sorting their log in place (rywNodes.mergeMutates).
		s.nodes = rywNodes{log: slices.Clone(n.log)}
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

// writeConflicts returns the list as added without RYW, else the nodes' runs coalesced with the list;
// fresh reports a vector the caller owns.
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

// readConflicts returns, as a new vector, the list as added without RYW, else the map's ranges
// coalesced with the list.
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

// conflictRangesNowLocked returns, as new vectors, the conflicts a commit would ship now, for the dummy
// transaction and OnError's maybe-committed retry. Caller holds conflictMu.
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

// commitVectors is writeRangeToNativeTransaction's pass (ReadYourWrites.actor.cpp:1392, 1997-2071), or the
// op-log with a versionstamp; mutsOwned/conflictsFresh report new vectors, coalesced sorted conflicts.
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

// readConflictRanges returns the read conflicts a commit would ship now, before the
// self-conflicting range, as a new vector.
func (tx *Transaction) readConflictRanges() []KeyRange {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	return tx.readConflictRangesLocked()
}

// readConflictRangesLocked is readConflictRanges under conflictMu.
func (tx *Transaction) readConflictRangesLocked() []KeyRange {
	reads, _ := tx.conflictRangesNowLocked()
	return reads
}

// writeConflictRanges returns the write conflicts a commit would ship now, before the
// self-conflicting range, as a new vector.
func (tx *Transaction) writeConflictRanges() []KeyRange {
	tx.conflictMu.Lock()
	defer tx.conflictMu.Unlock()
	if tx.rywDisabled {
		return slices.Clone(tx.writeConflicts)
	}
	_, writes := tx.conflictRangesNowLocked()
	return writes
}
