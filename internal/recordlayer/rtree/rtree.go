// Portions derived from FoundationDB Record Layer (RTree.java, ChildSlot.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rtree

import (
	"bytes"
	"fmt"
	"math/big"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// RTree is a Hilbert R-tree backed by FDB.
// Matches Java's com.apple.foundationdb.async.rtree.RTree.
type RTree struct {
	storage *StorageAdapter
	config  RTreeConfig
}

// NewRTree creates a new R-tree. Like Java's RTree it takes any minM, maxM
// and splitS: a configuration that cannot keep the node invariants fails at
// the operation where Java's fails (see errors.go).
func NewRTree(storage *StorageAdapter, config RTreeConfig) (*RTree, error) {
	if config.NumDimensions < 1 {
		return nil, fmt.Errorf("rtree: NumDimensions must be >= 1, got %d", config.NumDimensions)
	}
	return &RTree{storage: storage, config: config}, nil
}

// ScanIterator returns a lazy iterator over items matching the MBR predicate,
// starting after (lastHV, lastKey). Items are yielded one at a time in Hilbert
// value order. Only fetches leaf nodes from FDB on demand.
func (rt *RTree) ScanIterator(tx fdb.ReadTransaction, lastHV *big.Int, lastKey tuple.Tuple, mbrPredicate func(MBR) bool) *RTreeIterator {
	return &RTreeIterator{
		rt:           rt,
		tx:           tx,
		lastHV:       lastHV,
		lastKey:      lastKey,
		mbrPredicate: mbrPredicate,
	}
}

// Scan returns all items matching the MBR predicate, starting after (lastHV, lastKey).
// Items are returned in Hilbert value order. Convenience wrapper around ScanIterator.
func (rt *RTree) Scan(tx fdb.ReadTransaction, lastHV *big.Int, lastKey tuple.Tuple, mbrPredicate func(MBR) bool) ([]ItemSlot, error) {
	iter := rt.ScanIterator(tx, lastHV, lastKey, mbrPredicate)
	var result []ItemSlot
	for {
		item, ok, err := iter.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		result = append(result, item)
	}
	return result, nil
}

// RTreeIterator lazily walks the R-tree depth-first in Hilbert order,
// fetching one leaf node at a time from FDB.
type RTreeIterator struct {
	rt           *RTree
	tx           fdb.ReadTransaction
	lastHV       *big.Int
	lastKey      tuple.Tuple
	mbrPredicate func(MBR) bool

	// Stack of intermediate nodes with current child position.
	stack []iterFrame
	// Current leaf items being yielded.
	leafItems []ItemSlot
	leafPos   int
	// Lifecycle flags.
	started bool
	done    bool
	err     error
}

// iterFrame tracks position within an intermediate node during iteration.
type iterFrame struct {
	node     *intermediateNode
	childIdx int // next child index to visit
}

// Next returns the next item from the iterator.
// Returns (item, true, nil) on success, (zero, false, nil) when exhausted,
// or (zero, false, err) on error.
func (it *RTreeIterator) Next() (ItemSlot, bool, error) {
	for {
		// Yield from current leaf if available.
		for it.leafPos < len(it.leafItems) {
			item := it.leafItems[it.leafPos]
			it.leafPos++
			// Skip items at or before continuation point.
			if it.lastHV != nil && compareHilbertValueAndKey(item.HilbertValue, item.ItemKey(), it.lastHV, it.lastKey) <= 0 {
				continue
			}
			return item, true, nil
		}

		if it.done {
			return ItemSlot{}, false, nil
		}

		// Initialize: load root on first call.
		if !it.started {
			it.started = true
			if err := it.loadRoot(); err != nil {
				it.done = true
				return ItemSlot{}, false, err
			}
			// If loadRoot populated leafItems or set done, loop back.
			continue
		}

		// Advance to next leaf by walking the stack.
		if !it.advanceToNextLeaf() {
			return ItemSlot{}, false, nil
		}
		// advanceToNextLeaf may have set an error.
		if it.err != nil {
			return ItemSlot{}, false, it.err
		}
	}
}

// loadRoot fetches the root node and initializes the iterator state.
func (it *RTreeIterator) loadRoot() error {
	leaf, inter, err := it.rt.storage.fetchNode(it.tx, rootNodeID)
	if err != nil {
		return err
	}
	if leaf == nil && inter == nil {
		it.done = true
		return nil
	}
	if leaf != nil {
		it.leafItems = leaf.slots
		it.leafPos = 0
		it.done = true // root leaf has no further nodes to visit
		return nil
	}
	// Root is intermediate — push onto stack.
	it.stack = append(it.stack, iterFrame{node: inter, childIdx: 0})
	return nil
}

// advanceToNextLeaf walks the stack to find and load the next leaf node.
// Returns true if a new leaf was loaded (or an error occurred), false if exhausted.
func (it *RTreeIterator) advanceToNextLeaf() bool {
	for len(it.stack) > 0 {
		frame := &it.stack[len(it.stack)-1]

		// Find next valid child in the current frame.
		for frame.childIdx < len(frame.node.slots) {
			child := frame.node.slots[frame.childIdx]
			frame.childIdx++

			// Skip children entirely before continuation point.
			if it.lastHV != nil && compareHilbertValueAndKey(child.LargestHV, child.LargestKey, it.lastHV, it.lastKey) <= 0 {
				continue
			}
			// Skip children not matching MBR predicate.
			if it.mbrPredicate != nil && !it.mbrPredicate(child.ChildMBR) {
				continue
			}

			// Fetch child node.
			childLeaf, childInter, err := it.rt.storage.fetchNode(it.tx, child.ChildID)
			if err != nil {
				it.done = true
				it.err = err
				return true
			}

			if childLeaf != nil {
				it.leafItems = childLeaf.slots
				it.leafPos = 0
				return true
			}
			if childInter != nil {
				// Push intermediate child onto stack and continue deeper.
				it.stack = append(it.stack, iterFrame{node: childInter, childIdx: 0})
				frame = &it.stack[len(it.stack)-1] // update frame pointer
			}
		}

		// No more children in this frame — pop.
		it.stack = it.stack[:len(it.stack)-1]
	}

	// Stack empty — exhausted.
	it.done = true
	return false
}

// updatePath represents the path from root to a leaf for insert/delete operations.
type updatePath struct {
	leaf    *leafNode
	parents []*intermediateNode // from root to parent of leaf
	indices []int               // child index at each level
}

// fetchUpdatePathToLeaf walks from root to the leaf where (hv, key) should live.
// For inserts (isInsert=true), defaults to the last child when no range covers the
// target. For deletes (isInsert=false), returns a nil-leaf path if no child's range
// can contain the target item.
func (rt *RTree) fetchUpdatePathToLeaf(tx fdb.WritableTransaction, hv *big.Int, itemKey tuple.Tuple, isInsert bool) (*updatePath, error) {
	path := &updatePath{}

	leaf, inter, err := rt.storage.fetchNode(tx, rootNodeID)
	if err != nil {
		return nil, err
	}
	if leaf == nil && inter == nil {
		return path, nil // empty tree
	}
	if leaf != nil {
		path.leaf = leaf
		return path, nil
	}

	// Walk down intermediate nodes.
	current := inter
	for {
		// Find the child whose range covers (hv, key).
		childIdx := -1
		for i, child := range current.slots {
			if compareHilbertValueAndKey(child.LargestHV, child.LargestKey, hv, itemKey) >= 0 {
				childIdx = i
				break
			}
		}

		if childIdx < 0 {
			if isInsert {
				// Insert: default to the last child (extend rightmost).
				childIdx = len(current.slots) - 1
			} else {
				// Delete: item is beyond all children's ranges — not in tree.
				return path, nil
			}
		}

		path.parents = append(path.parents, current)
		path.indices = append(path.indices, childIdx)

		childID := current.slots[childIdx].ChildID
		childLeaf, childInter, err := rt.storage.fetchNode(tx, childID)
		if err != nil {
			return nil, err
		}
		if childLeaf != nil {
			path.leaf = childLeaf
			path.setHeights()
			return path, nil
		}
		if childInter == nil {
			return path, nil
		}
		current = childInter
	}
}

// setHeights gives each intermediate node of a path that reached its leaf its
// height: the leaf's parent is at 1, the root at len(parents).
func (p *updatePath) setHeights() {
	for i, n := range p.parents {
		setNodeHeight(n, len(p.parents)-i)
	}
}

// setNodeHeight sets an intermediate node's height, and the height it was
// read at when this is the first time.
func setNodeHeight(n *intermediateNode, h int) {
	n.height = h
	if n.origHeight == 0 {
		n.origHeight = h
	}
}

// gatherLeafSiblings returns S siblings centered on childIdx, loaded from FDB.
func (rt *RTree) gatherLeafSiblings(tx fdb.WritableTransaction, parent *intermediateNode, childIdx int) ([]*leafNode, int, error) {
	s := rt.config.SplitS
	if s < 0 { // RTree.java:1622
		return nil, 0, &NegativeArraySizeError{Size: s}
	}
	// Java's fetchSiblings reads past a parent with fewer than splitS children
	// (IndexOutOfBoundsException, RTree.java:1631-1646); Go takes those it has.
	s = min(s, len(parent.slots))

	// Center the window.
	startIdx := childIdx - s/2
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx+s > len(parent.slots) {
		startIdx = len(parent.slots) - s
	}

	siblings := make([]*leafNode, 0, s)
	for i := startIdx; i < startIdx+s; i++ {
		node, err := rt.storage.fetchLeafNode(tx, parent.slots[i].ChildID)
		if err != nil {
			return nil, 0, fmt.Errorf("rtree: fetch leaf sibling %d: %w", i, err)
		}
		if node == nil {
			return nil, 0, fmt.Errorf("rtree: leaf sibling %d not found", i)
		}
		siblings = append(siblings, node)
	}
	return siblings, startIdx, nil
}

// redistributeItems evenly distributes items across sibling leaf nodes.
func (rt *RTree) redistributeItems(items []ItemSlot, siblings []*leafNode) {
	n := len(siblings)
	perNode := len(items) / n
	remainder := len(items) % n
	idx := 0
	for i, sib := range siblings {
		count := perNode
		if i < remainder {
			count++
		}
		sib.slots = make([]ItemSlot, count)
		copy(sib.slots, items[idx:idx+count])
		idx += count
	}
}

// childSlotForLeaf creates a ChildSlot from a leaf node.
func (rt *RTree) childSlotForLeaf(leaf *leafNode) ChildSlot {
	if len(leaf.slots) == 0 {
		return ChildSlot{
			SmallestHV:  big.NewInt(0),
			SmallestKey: tuple.Tuple{},
			LargestHV:   big.NewInt(0),
			LargestKey:  tuple.Tuple{},
			ChildID:     leaf.id,
			ChildMBR:    MBR{Low: make([]int64, rt.config.NumDimensions), High: make([]int64, rt.config.NumDimensions)},
		}
	}

	first := leaf.slots[0]
	last := leaf.slots[len(leaf.slots)-1]

	mbr := first.GetMBR()
	for _, slot := range leaf.slots[1:] {
		mbr = mbr.Union(slot.GetMBR())
	}

	return ChildSlot{
		SmallestHV:  first.HilbertValue,
		SmallestKey: first.ItemKey(),
		LargestHV:   last.HilbertValue,
		LargestKey:  last.ItemKey(),
		ChildID:     leaf.id,
		ChildMBR:    mbr,
	}
}

// propagateMBRUp updates parent ChildSlots with new MBR/HV info after a leaf change.
// Walks from the leaf parent up to the root, updating each level's ChildSlot.
// Matches Java's adjustSlotInParent: only writes the parent if the ChildSlot changed.
func (rt *RTree) propagateMBRUp(tx fdb.WritableTransaction, path *updatePath) {
	if len(path.parents) == 0 {
		return
	}

	leaf := path.leaf
	for i := len(path.parents) - 1; i >= 0; i-- {
		parent := path.parents[i]
		childIdx := path.indices[i]
		if childIdx >= len(parent.slots) {
			continue
		}
		var newSlot ChildSlot
		if i == len(path.parents)-1 {
			newSlot = rt.childSlotForLeaf(leaf)
		} else {
			child := path.parents[i+1]
			newSlot = rt.childSlotForIntermediate(child)
		}
		if childSlotEqual(parent.slots[childIdx], newSlot) {
			break // nothing changed at this level, stop propagation
		}
		parent.slots[childIdx] = newSlot
		rt.storage.writeIntermediateNode(tx, parent)
	}
}

// propagateParentMBRUp updates intermediate nodes above a given parent level.
// Recomputes the ChildSlot at each level from the child intermediate node below.
func (rt *RTree) propagateParentMBRUp(tx fdb.WritableTransaction, path *updatePath, startIdx int) {
	for i := startIdx - 1; i >= 0; i-- {
		parent := path.parents[i]
		childIdx := path.indices[i]
		if childIdx < len(parent.slots) {
			child := path.parents[i+1]
			parent.slots[childIdx] = rt.childSlotForIntermediate(child)
		}
		rt.storage.writeIntermediateNode(tx, parent)
	}
}

// gatherIntermediateSiblings returns S intermediate siblings centered on childIdx,
// loaded from FDB. Mirrors gatherLeafSiblings but for intermediate nodes.
func (rt *RTree) gatherIntermediateSiblings(tx fdb.WritableTransaction, parent *intermediateNode, childIdx int) ([]*intermediateNode, int, error) {
	// The leaf-level gather of the same operation already failed any splitS < 1.
	s := min(rt.config.SplitS, len(parent.slots)) // see gatherLeafSiblings

	// Center the window.
	startIdx := childIdx - s/2
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx+s > len(parent.slots) {
		startIdx = len(parent.slots) - s
	}

	siblings := make([]*intermediateNode, 0, s)
	for i := startIdx; i < startIdx+s; i++ {
		node, err := rt.storage.fetchIntermediateNode(tx, parent.slots[i].ChildID)
		if err != nil {
			return nil, 0, fmt.Errorf("rtree: fetch intermediate sibling %d: %w", i, err)
		}
		if node == nil {
			return nil, 0, fmt.Errorf("rtree: intermediate sibling %d not found", i)
		}
		setNodeHeight(node, parent.height-1)
		siblings = append(siblings, node)
	}
	return siblings, startIdx, nil
}

// redistributeChildSlots evenly distributes child slots across sibling intermediate nodes.
func (rt *RTree) redistributeChildSlots(items []ChildSlot, siblings []*intermediateNode) {
	n := len(siblings)
	perNode := len(items) / n
	remainder := len(items) % n
	idx := 0
	for i, sib := range siblings {
		count := perNode
		if i < remainder {
			count++
		}
		sib.slots = make([]ChildSlot, count)
		copy(sib.slots, items[idx:idx+count])
		idx += count
	}
}

// childSlotForIntermediate creates a ChildSlot summarizing an intermediate node.
// Computes the bounding HV/key range and union MBR from all child slots.
func (rt *RTree) childSlotForIntermediate(node *intermediateNode) ChildSlot {
	if len(node.slots) == 0 {
		return ChildSlot{
			SmallestHV:  big.NewInt(0),
			SmallestKey: tuple.Tuple{},
			LargestHV:   big.NewInt(0),
			LargestKey:  tuple.Tuple{},
			ChildID:     node.id,
			ChildMBR:    MBR{Low: make([]int64, rt.config.NumDimensions), High: make([]int64, rt.config.NumDimensions)},
		}
	}

	first := node.slots[0]
	last := node.slots[len(node.slots)-1]

	mbr := first.ChildMBR
	for _, slot := range node.slots[1:] {
		mbr = mbr.Union(slot.ChildMBR)
	}

	return ChildSlot{
		SmallestHV:  first.SmallestHV,
		SmallestKey: first.SmallestKey,
		LargestHV:   last.LargestHV,
		LargestKey:  last.LargestKey,
		ChildID:     node.id,
		ChildMBR:    mbr,
	}
}

// Clear removes all data from the R-tree.
func (rt *RTree) Clear(tx fdb.WritableTransaction) error {
	return rt.storage.clearAll(tx)
}

// nodeIDEqual compares two node IDs.
func nodeIDEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
