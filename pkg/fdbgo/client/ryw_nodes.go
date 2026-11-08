package client

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
)

// rywNodes ports the node set of C++ WriteMap (fdbclient/WriteMap.cpp,
// fdbclient/include/fdbclient/WriteMap.h, FDB 7.3.77) without operation stacks
// (RFC-262 D1b). Nodes, not written keys, are the write-side segment boundaries:
// a node with an operation gives the segment [k, keyAfter(k)) and then the range
// [keyAfter(k), next) unless next is keyAfter(k); a node without one gives
// [k, next) (WriteMap.cpp:312-347, WriteMap.h:176-177).
//
// The five mutators below are the only writers of seq, each a line-for-line
// port of its C++ counterpart's effect on keys and flags. Keys handed to a
// mutator are retained and must never be written afterwards; clone shares them.
//
// Lazy build: until build is first called, mutators append to log, so a
// write-only transaction pays O(1) per write instead of an O(log n) insert.
// build replays the log in order, merging a long run of consecutive mutate ops
// in one sorted pass (mergeMutates). After the first build, mutators apply directly.
type rywNode struct {
	key []byte
	op  bool // the C++ operation stack is non-empty
	// WriteMapEntry flags (WriteMap.h:72-76).
	followingCleared, followingConflict, isConflict, followingUnreadable, isUnreadable bool
}

type rywNodes struct {
	// seq holds the nodes by key once built, the sentinels included. It is a
	// persistent sequence, as C++'s PTree is, so clone is O(1).
	seq   pseq[rywNode]
	built bool
	log   []rywNodeOp // structural ops not yet applied (only while !built)
	// first holds the log's first op, so a transaction's first write allocates
	// no log. clone copies the log out of it.
	first [1]rywNodeOp
}

// logOp appends a structural op to the lazy log.
func (n *rywNodes) logOp(op rywNodeOp) {
	if n.log == nil {
		n.log = n.first[:0]
	}
	n.log = append(n.log, op)
}

type rywNodeOpKind uint8

const (
	rywNodeOpMutate rywNodeOpKind = iota
	rywNodeOpClear
	rywNodeOpConflict
	rywNodeOpUnreadable
)

type rywNodeOp struct {
	kind         rywNodeOpKind
	begin, end   []byte // mutate: begin is the key
	addConflict  bool   // mutate, clear
	versionstamp bool   // mutate
}

// rywNodeSeg is one node-derived write segment: [begin, end) with the flags the
// C++ iterator reports there (is_operation, is_cleared_range, is_conflict_range,
// is_unreadable; WriteMap.h:165-169).
type rywNodeSeg struct {
	begin, end                        rywBoundary
	op, cleared, conflict, unreadable bool
}

// afterAllKeysBytes is \xff\xff\x00, C++ afterAllKeys.
var afterAllKeysBytes = []byte{0xff, 0xff, 0x00}

// rywNodeSentinels returns the constructor's three nodes (WriteMap.h:129-134):
// allKeys.begin, allKeys.end and afterAllKeys, all with empty stacks and no flags.
func rywNodeSentinels() []rywNode {
	return []rywNode{{key: allKeysBegin}, {key: allKeysEnd}, {key: afterAllKeysBytes}}
}

// equalsKeyAfter reports whether compareKey == keyAfter(key) (FDBTypes.h:309).
func equalsKeyAfter(key, compareKey []byte) bool {
	return len(compareKey) == len(key)+1 && compareKey[len(key)] == 0 && bytes.Equal(compareKey[:len(key)], key)
}

// --- mutators -------------------------------------------------------------------

// mutate is WriteMap::mutate (WriteMap.cpp:87-158), structural effect only.
// versionstamp is operation == SetVersionstampedValue || SetVersionstampedKey.
func (n *rywNodes) mutate(key []byte, addConflict, versionstamp bool) {
	if !n.built {
		n.logOp(rywNodeOp{kind: rywNodeOpMutate, begin: key, addConflict: addConflict, versionstamp: versionstamp})
		return
	}
	n.applyMutate(key, addConflict, versionstamp)
}

// clear is WriteMap::clear (WriteMap.cpp:160-203), or clearNoConflict
// (:527-596) when !addConflict (:162-165). An empty range is a no-op; the RYW
// callers never pass one (ReadYourWrites.actor.cpp:2397-2399).
func (n *rywNodes) clear(begin, end []byte, addConflict bool) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	if !n.built {
		n.logOp(rywNodeOp{kind: rywNodeOpClear, begin: begin, end: end, addConflict: addConflict})
		return
	}
	n.applyClear(begin, end, addConflict)
}

// addConflictRange is WriteMap::addConflictRange (WriteMap.cpp:244-302). An
// empty range is a no-op, as at ReadYourWrites.actor.cpp:2489-2491.
func (n *rywNodes) addConflictRange(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	if !n.built {
		n.logOp(rywNodeOp{kind: rywNodeOpConflict, begin: begin, end: end})
		return
	}
	n.applyConflictRange(begin, end)
}

// addUnmodifiedAndUnreadableRange is WriteMap::addUnmodifiedAndUnreadableRange
// (WriteMap.cpp:205-242), the SetVersionstampedKey candidate range
// (ReadYourWrites.actor.cpp:2264-2272). An empty range is a no-op.
func (n *rywNodes) addUnmodifiedAndUnreadableRange(begin, end []byte) {
	if bytes.Compare(begin, end) >= 0 {
		return
	}
	if !n.built {
		n.logOp(rywNodeOp{kind: rywNodeOpUnreadable, begin: begin, end: end})
		return
	}
	n.applyUnreadableRange(begin, end)
}

// clone returns a copy that shares keys (never written after insertion) and,
// in O(1), the node sequence, whose writing it takes over (pseq.handoff);
// mutating either never changes the other.
func (n *rywNodes) clone() rywNodes {
	return rywNodes{seq: n.seq.handoff(), built: n.built, log: slices.Clone(n.log)}
}

// build applies the pending log. Idempotent; the owner calls it under its lock
// before any query, and every query calls it too (a no-op once built).
func (n *rywNodes) build() {
	if n.built {
		return
	}
	n.built = true
	n.seq.build(rywNodeSentinels())
	log := n.log
	n.log = nil
	for i := 0; i < len(log); {
		if log[i].kind == rywNodeOpMutate {
			j := i + 1
			for j < len(log) && log[j].kind == rywNodeOpMutate {
				j++
			}
			// A run short against the nodes applies in O(log n) per op; a long one
			// merges in one O(n) pass. Both are the ops applied in order.
			if run := log[i:j]; len(run) < n.seq.Len()/8 {
				for _, op := range run {
					n.applyMutate(op.begin, op.addConflict, op.versionstamp)
				}
			} else {
				n.mergeMutates(run)
			}
			i = j
			continue
		}
		op := log[i]
		switch op.kind {
		case rywNodeOpClear:
			n.applyClear(op.begin, op.end, op.addConflict)
		case rywNodeOpConflict:
			n.applyConflictRange(op.begin, op.end)
		case rywNodeOpUnreadable:
			n.applyUnreadableRange(op.begin, op.end)
		}
		i++
	}
}

// --- WriteMap::iterator ---------------------------------------------------------

// rywNodeIter is WriteMap::iterator (WriteMap.h:152-205) for the mutators, which
// stop iterating before they change the nodes; the walk uses rywNodeCursor.
type rywNodeIter struct {
	s      *pseq[rywNode]
	cur    pfinger[rywNode] // at entry(), finger[beginLen-1] (WriteMap.h:195, :203)
	next   *rywNode         // nextEntry(), finger[endLen-1] (:196)
	offset bool             // false: the operation at entry; true: the range after it
}

func (it *rywNodeIter) entry() *rywNode     { return it.cur.item() } // WriteMap.h:195
func (it *rywNodeIter) nextEntry() *rywNode { return it.next }       // WriteMap.h:196

func (it *rywNodeIter) isClearedRange() bool    { return it.offset && it.entry().followingCleared }  // :165
func (it *rywNodeIter) isUnmodifiedRange() bool { return it.offset && !it.entry().followingCleared } // :166
func (it *rywNodeIter) isOperation() bool       { return !it.offset }                                // :167

// isConflictRange is WriteMap.h:168.
func (it *rywNodeIter) isConflictRange() bool {
	if it.offset {
		return it.entry().followingConflict
	}
	return it.entry().isConflict
}

// isUnreadable is WriteMap.h:169.
func (it *rywNodeIter) isUnreadable() bool {
	if it.offset {
		return it.entry().followingUnreadable
	}
	return it.entry().isUnreadable
}

// beginKey is WriteMap.h:176: ExtStringRef(entry().key, offset && stack.size()).
func (it *rywNodeIter) beginKey() rywBoundary {
	return rywBoundary{key: it.entry().key, after: it.offset && it.entry().op}
}

// endKey is WriteMap.h:177.
func (it *rywNodeIter) endKey() rywBoundary {
	if it.offset {
		return rywBoundary{key: it.nextEntry().key}
	}
	return rywBoundary{key: it.entry().key, after: true}
}

// keyAtBegin is WriteMap.h:198.
func (it *rywNodeIter) keyAtBegin() bool { return !it.offset || !it.entry().op }

// skip is WriteMap.cpp:335-347: the segment containing key. upper_bound puts the
// next entry at the first node > key and the entry at the one before; for
// allKeys.end, last() (:339-340) lands on the same pair. A key >= \xff\xff\x00 has
// no containing segment in C++ (its finger would be empty); it clamps to the
// sentinel segment [\xff\xff, \xff\xff\x00).
func (it *rywNodeIter) skip(key []byte) {
	it.cur.seekFirst(it.s, func(nd *rywNode) bool { return bytes.Compare(nd.key, key) > 0 })
	if last := it.s.Len() - 1; it.cur.idx > last {
		it.cur.reset(it.s, last) // past afterAllKeys: the entry is allKeys.end (:339-340)
	}
	it.next = it.cur.item()
	it.cur.prev()
	it.offset = !it.entry().op || !bytes.Equal(it.entry().key, key) // :346
}

// inc is operator++ (WriteMap.cpp:312-322).
func (it *rywNodeIter) inc() {
	if !it.offset && !equalsKeyAfter(it.entry().key, it.nextEntry().key) { // :313
		it.offset = true // :314
	} else {
		it.cur.next() // :316-318
		it.next = it.cur.peekNext()
		it.offset = !it.entry().op // :319
	}
}

// dec is operator-- (WriteMap.cpp:323-333).
func (it *rywNodeIter) dec() {
	if it.offset && it.entry().op { // :324
		it.offset = false // :325
	} else {
		it.next = it.cur.item() // :327-329
		it.cur.prev()
		it.offset = !it.entry().op || !equalsKeyAfter(it.entry().key, it.nextEntry().key) // :330
	}
}

func (n *rywNodes) iter() rywNodeIter { return rywNodeIter{s: &n.seq} }

// --- PTreeImpl helpers ----------------------------------------------------------

// insert is PTreeImpl::insert (VersionedMap.h:284-303): an equal key replaces
// the node (:288-289).
func (n *rywNodes) insert(node rywNode) {
	i, pnode := n.seq.findNode(func(e *rywNode) bool { return bytes.Compare(e.key, node.key) >= 0 })
	if pnode != nil && bytes.Equal(pnode.item.key, node.key) {
		*n.seq.mutNode(i, pnode) = node
		return
	}
	n.seq.insert(i, node)
}

// removeRange is PTreeImpl::remove(begin, end) (VersionedMap.h:409-444): every
// node with begin <= key < end, the bounds being ExtStringRefs.
func (n *rywNodes) removeRange(begin, end rywBoundary) {
	lo := n.seq.search(func(nd *rywNode) bool { return (rywBoundary{key: nd.key}).compare(begin) >= 0 })
	hi := n.seq.search(func(nd *rywNode) bool { return (rywBoundary{key: nd.key}).compare(end) >= 0 })
	n.seq.delete(lo, hi)
}

// --- mutator ports --------------------------------------------------------------

// mutatedEntry is the node WriteMap::mutate leaves at key when the iterator,
// skipped to key, is at (e, offset). Every branch of :102-157 writes the same
// structure: an operation (a push or coalesce never empties a stack), the
// following_* flags of it.entry(), is_conflict = addConflict ||
// it.is_conflict_range() (:95) and is_unreadable = it.is_unreadable() ||
// versionstamped (:97-98); :103-111 and :113-122 agree because offset is set
// whenever entry().key != key (:346), so is_cleared_range() is
// following_keys_cleared. The comment at WriteMap.h:143 follows: a write without
// a conflict never clears an existing is_conflict.
func mutatedEntry(key []byte, e rywNode, offset, addConflict, versionstamp bool) rywNode {
	isConflictRange, isUnreadable := e.isConflict, e.isUnreadable // WriteMap.h:168-169
	if offset {
		isConflictRange, isUnreadable = e.followingConflict, e.followingUnreadable
	}
	return rywNode{
		key:                 key,
		op:                  true,
		followingCleared:    e.followingCleared,             // :93
		followingConflict:   e.followingConflict,            // :94
		isConflict:          addConflict || isConflictRange, // :95
		followingUnreadable: e.followingUnreadable,          // :96
		isUnreadable:        isUnreadable || versionstamp,   // :97-98
	}
}

// applyMutate is WriteMap::mutate (WriteMap.cpp:87-158) on built nodes.
func (n *rywNodes) applyMutate(key []byte, addConflict, versionstamp bool) {
	// skip(key) (:89-91): the entry is the last node <= key, which for a key below
	// allKeys.end is never past the node skip clamps to.
	i, pnode := n.seq.beforeNode(func(nd *rywNode) bool { return bytes.Compare(nd.key, key) > 0 })
	if i > n.seq.Len()-2 {
		it := n.iter()
		it.skip(key)
		n.insert(mutatedEntry(key, *it.entry(), it.offset, addConflict, versionstamp))
		return
	}
	e := &pnode.item
	at := bytes.Equal(e.key, key)
	node := mutatedEntry(key, *e, !e.op || !at, addConflict, versionstamp) // offset (:346)
	// :102-123 insert a node at key; :124-156 remove the node at key and insert
	// the updated one. Both are an insert at key (VersionedMap.h:288-289): the
	// node at key is replaced, or the new one goes right after the entry.
	if at {
		*n.seq.mutNode(i, pnode) = node
	} else {
		n.seq.insert(i+1, node)
	}
}

// mergeMutates applies a run of consecutive mutate ops in one sorted pass. It is
// exact: a mutate at key reads only the node at key, or failing that the floor
// node's following_* flags (mutatedEntry), and writes only the node at key. A
// node a run inserts copies its floor's following_* flags, so a later key in the
// same range sees the same flags whichever of the two is its floor. Ops on one
// key fold commutatively: the first fixes the following_* flags and the base of
// is_conflict / is_unreadable, and every op only ORs addConflict / versionstamp
// onto those, so the sort need not be stable. run is sorted in place: build
// detached the log, and clone never shares it.
func (n *rywNodes) mergeMutates(run []rywNodeOp) {
	slices.SortFunc(run, func(a, b rywNodeOp) int { return bytes.Compare(a.begin, b.begin) })
	sorted := n.seq.appendTo(make([]rywNode, 0, n.seq.Len()))
	out := make([]rywNode, 0, len(sorted)+len(run))
	si := 0
	for g := 0; g < len(run); {
		key := run[g].begin
		for si < len(sorted) && bytes.Compare(sorted[si].key, key) < 0 {
			out = append(out, sorted[si])
			si++
		}
		var cur rywNode
		have := si < len(sorted) && bytes.Equal(sorted[si].key, key)
		if have {
			cur = sorted[si]
			si++
		}
		for ; g < len(run) && bytes.Equal(run[g].begin, key); g++ {
			if have {
				cur = mutatedEntry(cur.key, cur, !cur.op, run[g].addConflict, run[g].versionstamp) // skip: offset = !stack.size() (:346)
			} else {
				cur = mutatedEntry(key, out[len(out)-1], true, run[g].addConflict, run[g].versionstamp) // the "" sentinel is <= every key
				have = true
			}
		}
		out = append(out, cur)
	}
	n.seq.build(append(out, sorted[si:]...))
}

// applyClear is WriteMap::clear (WriteMap.cpp:160-203) on built nodes.
func (n *rywNodes) applyClear(begin, end []byte, addConflict bool) {
	if !addConflict { // :162-165
		n.applyClearNoConflict(begin, end)
		return
	}
	endB := rywBoundary{key: end}
	it := n.iter()
	it.skip(begin) // :167-169

	insertBegin := !it.isClearedRange() || !it.isConflictRange() || it.isUnreadable() // :171

	if c := it.endKey().compare(endB); c == 0 { // :173-177
		it.inc()
	} else if c < 0 {
		it.skip(end)
	}

	insertEnd := (it.isUnmodifiedRange() || !it.isConflictRange() || it.isUnreadable()) &&
		(!it.keyAtBegin() || !it.beginKey().equal(endB)) // :179-180
	endCoalesceClear := it.isClearedRange() && it.beginKey().equal(endB) &&
		it.isConflictRange() && !it.isUnreadable() // :181-182
	endConflict := it.isConflictRange() // :183
	endCleared := it.isClearedRange()   // :184
	endUnreadable := it.isUnreadable()  // :185

	n.removeRange(rywBoundary{key: begin, after: !insertBegin}, rywBoundary{key: end, after: endCoalesceClear}) // :189-192

	if insertBegin { // :194-195
		n.insert(rywNode{key: begin, followingCleared: true, followingConflict: true, isConflict: true})
	}
	if insertEnd { // :197-202
		n.insert(rywNode{
			key: end, followingCleared: endCleared, followingConflict: endConflict, isConflict: endConflict,
			followingUnreadable: endUnreadable, isUnreadable: endUnreadable,
		})
	}
}

// applyUnreadableRange is WriteMap::addUnmodifiedAndUnreadableRange
// (WriteMap.cpp:205-242) on built nodes.
func (n *rywNodes) applyUnreadableRange(begin, end []byte) {
	endB := rywBoundary{key: end}
	it := n.iter()
	it.skip(begin) // :206-208

	insertBegin := !it.isUnmodifiedRange() || it.isConflictRange() || !it.isUnreadable() // :210

	if c := it.endKey().compare(endB); c == 0 { // :212-216
		it.inc()
	} else if c < 0 {
		it.skip(end)
	}

	insertEnd := (it.isClearedRange() || it.isConflictRange() || !it.isUnreadable()) &&
		(!it.keyAtBegin() || !it.beginKey().equal(endB)) // :218-219
	endCoalesceUnmodified := it.isUnmodifiedRange() && it.beginKey().equal(endB) &&
		!it.isConflictRange() && it.isUnreadable() // :220-221
	endConflict := it.isConflictRange() // :222
	endCleared := it.isClearedRange()   // :223
	endUnreadable := it.isUnreadable()  // :224

	n.removeRange(rywBoundary{key: begin, after: !insertBegin}, rywBoundary{key: end, after: endCoalesceUnmodified}) // :228-231

	if insertBegin { // :233-234
		n.insert(rywNode{key: begin, followingUnreadable: true, isUnreadable: true})
	}
	if insertEnd { // :236-241
		n.insert(rywNode{
			key: end, followingCleared: endCleared, followingConflict: endConflict, isConflict: endConflict,
			followingUnreadable: endUnreadable, isUnreadable: endUnreadable,
		})
	}
}

// applyConflictRange is WriteMap::addConflictRange (WriteMap.cpp:244-302) on built nodes, with its
// edits applied by iterator position, the insertion its "SOMEDAY" (:292) asks for.
func (n *rywNodes) applyConflictRange(begin, end []byte) {
	endB := rywBoundary{key: end}
	it := n.iter()
	it.skip(begin) // :246-248

	type edit struct {
		at      int
		replace bool
		pnode   *pnode[rywNode]
		node    rywNode
	}
	var buf [4]edit
	edits := buf[:0]                                            // :250-251
	if e := it.entry(); !e.followingConflict || !e.isConflict { // :253
		node := rywNode{ // :257-263
			key:                 begin,
			op:                  it.isOperation(), // OperationStack(it.op()) or OperationStack()
			followingCleared:    e.followingCleared,
			followingConflict:   true,
			isConflict:          true,
			followingUnreadable: e.followingUnreadable,
			isUnreadable:        e.isUnreadable,
		}
		if bytes.Equal(e.key, begin) { // removals.push_back(keys.begin) (:254-256)
			edits = append(edits, edit{at: it.cur.idx, replace: true, pnode: it.cur.node(), node: node})
		} else {
			edits = append(edits, edit{at: it.cur.idx + 1, node: node})
		}
	}

	for it.endKey().compare(endB) < 0 { // :266
		it.inc()                                                                         // :267
		if e := it.entry(); it.keyAtBegin() && (!e.followingConflict || !e.isConflict) { // :268
			c := *e // :269-273
			c.followingConflict = true
			c.isConflict = true
			edits = append(edits, edit{at: it.cur.idx, replace: true, pnode: it.cur.node(), node: c})
		}
	}

	// ASSERT(it.beginKey() != keys.end) (:277)
	if e := it.entry(); !e.followingConflict || !e.isConflict { // :278
		isCleared := e.followingCleared              // :279
		isUnreadable := e.isUnreadable               // :280
		followingUnreadable := e.followingUnreadable // :281
		// end lies in the segment of the entry at this position: a new node
		// at end follows that entry.
		at := it.cur.idx + 1
		it.inc()                                            // :282
		if !it.keyAtBegin() || !it.beginKey().equal(endB) { // :284-287
			edits = append(edits, edit{at: at, node: rywNode{
				key: end, followingCleared: isCleared, followingUnreadable: followingUnreadable, isUnreadable: isUnreadable,
			}})
		}
	}

	// :293-302, by position: from the last, so earlier positions stay valid.
	for i := len(edits) - 1; i >= 0; i-- {
		if e := edits[i]; e.replace {
			*n.seq.mutNode(e.at, e.pnode) = e.node
		} else {
			n.seq.insert(e.at, e.node)
		}
	}
}

// applyClearNoConflict is WriteMap::clearNoConflict (WriteMap.cpp:527-596) on
// built nodes: a clear that keeps the conflict state of every key it covers, so
// it leaves a node at each conflict/non-conflict toggle.
func (n *rywNodes) applyClearNoConflict(begin, end []byte) {
	endB := rywBoundary{key: end}
	it := n.iter()
	it.skip(begin) // :528-532

	insertBegin := !it.isClearedRange() || it.isUnreadable() // :534
	lastConflicted := it.isConflictRange()                   // :536
	conflicted := lastConflicted                             // :538
	var conflictRanges []rywBoundary                         // :539

	if insertBegin { // :541-545
		conflictRanges = append(conflictRanges, rywBoundary{key: begin})
	} else {
		conflicted = !conflicted
	}

	for it.endKey().compare(endB) < 0 { // :547
		it.inc()                                    // :548
		if lastConflicted != it.isConflictRange() { // :549-552
			conflictRanges = append(conflictRanges, it.beginKey())
			lastConflicted = it.isConflictRange()
		}
	}

	if it.endKey().equal(endB) { // :555-556
		it.inc()
	}
	// ASSERT(it.beginKey() <= keys.end && keys.end < it.endKey()) (:558)

	beginIsEnd := it.beginKey().equal(endB)
	insertEnd := ((it.isUnmodifiedRange() || it.isUnreadable()) && (!it.keyAtBegin() || !beginIsEnd)) ||
		(it.entry().isConflict && !it.entry().followingConflict && beginIsEnd && !it.keyAtBegin()) // :560-563
	endCleared := it.isClearedRange() // :564
	endCoalesceClear := it.isClearedRange() && beginIsEnd &&
		it.isConflictRange() == lastConflicted && !it.isUnreadable() // :565-566
	endConflict := it.isConflictRange() // :567
	endUnreadable := it.isUnreadable()  // :568

	n.removeRange(rywBoundary{key: begin, after: !insertBegin}, rywBoundary{key: end, after: endCoalesceClear}) // :574-577

	for _, b := range conflictRanges { // :579-586
		n.insert(rywNode{key: b.materialize(), followingCleared: true, followingConflict: conflicted, isConflict: conflicted})
		conflicted = !conflicted
	}
	// ASSERT(conflicted != lastConflicted) (:588)

	if insertEnd { // :590-595
		n.insert(rywNode{
			key: end, followingCleared: endCleared, followingConflict: endConflict, isConflict: endConflict,
			followingUnreadable: endUnreadable, isUnreadable: endUnreadable,
		})
	}
}

// --- boundary queries (RFC-262 D2) ----------------------------------------------
//
// segmentAt returns the node-derived write segment containing p
// (WriteMap::iterator::skip, WriteMap.cpp:335-347).
func (n *rywNodes) segmentAt(p []byte) rywNodeSeg {
	n.build()
	it := n.iter()
	it.skip(p)
	return it.seg()
}

func (it *rywNodeIter) seg() rywNodeSeg {
	return rywNodeSeg{
		begin: it.beginKey(), end: it.endKey(),
		op: it.isOperation(), cleared: it.isClearedRange(), conflict: it.isConflictRange(), unreadable: it.isUnreadable(),
	}
}

// dump renders every node after build, one per line: the quoted key and the
// set flags in the order o (operation), c (following_keys_cleared),
// f (following_keys_conflict), i (is_conflict), u (following_keys_unreadable),
// v (is_unreadable), or "-" when none is set.
func (n *rywNodes) dump() string {
	n.build()
	var b strings.Builder
	for _, nd := range n.seq.appendTo(nil) {
		b.WriteString(strconv.Quote(string(nd.key)))
		b.WriteByte(' ')
		b.WriteString(nd.flagString())
		b.WriteByte('\n')
	}
	return b.String()
}

func (nd rywNode) flagString() string {
	var f []byte
	for _, x := range []struct {
		set bool
		c   byte
	}{
		{nd.op, 'o'},
		{nd.followingCleared, 'c'},
		{nd.followingConflict, 'f'},
		{nd.isConflict, 'i'},
		{nd.followingUnreadable, 'u'},
		{nd.isUnreadable, 'v'},
	} {
		if x.set {
			f = append(f, x.c)
		}
	}
	if len(f) == 0 {
		return "-"
	}
	return string(f)
}
