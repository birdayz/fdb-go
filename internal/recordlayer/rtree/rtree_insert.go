// Portions derived from FoundationDB Record Layer (RTree.java, ChildSlot.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rtree

import (
	"sort"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// InsertOrUpdate inserts a new item or updates an existing one.
// Matches Java's RTree.insertOrUpdate().
func (rt *RTree) InsertOrUpdate(tx fdb.WritableTransaction, point Point, keySuffix tuple.Tuple, value tuple.Tuple) error {
	rt.storage.beginOperation()
	if err := rt.insertOrUpdate(tx, point, keySuffix, value); err != nil {
		return err
	}
	if err := rt.storage.checkWritten(); err != nil {
		return err
	}
	return rt.storage.flushNodeSlotIndex(tx)
}

func (rt *RTree) insertOrUpdate(tx fdb.WritableTransaction, point Point, keySuffix tuple.Tuple, value tuple.Tuple) error {
	coords := make([]int64, point.NumDimensions())
	for d := 0; d < len(coords); d++ {
		coords[d] = point.Coordinate(d)
	}
	hv := hilbertValue(coords)
	itemKey := tuple.Tuple{point.Coordinates, keySuffix}

	// Walk from root to leaf.
	path, err := rt.fetchUpdatePathToLeaf(tx, hv, itemKey, true)
	if err != nil {
		return err
	}

	leaf := path.leaf
	if leaf == nil {
		// Empty tree — create root leaf.
		leaf = &leafNode{id: rootNodeID, slots: nil}
		path.leaf = leaf
	}
	// RTree.java:797: a negative maxM fails the first insert.
	if len(leaf.slots) > rt.config.MaxM {
		return &VerifyError{}
	}

	// Check if item already exists (update).
	for i := range leaf.slots {
		if compareHilbertValueAndKey(leaf.slots[i].HilbertValue, leaf.slots[i].ItemKey(), hv, itemKey) == 0 {
			// Update value.
			leaf.slots[i].Value = value
			rt.storage.writeLeafNode(tx, leaf)
			// Update parent MBR if needed.
			rt.propagateMBRUp(tx, path)
			return nil
		}
	}

	// Insert new item.
	newSlot := ItemSlot{
		HilbertValue: hv,
		Point:        point,
		KeySuffix:    keySuffix,
		Value:        value,
	}

	insertPos := sort.Search(len(leaf.slots), func(i int) bool {
		return compareHilbertValueAndKey(leaf.slots[i].HilbertValue, leaf.slots[i].ItemKey(), hv, itemKey) > 0
	})

	leaf.slots = append(leaf.slots, ItemSlot{})
	copy(leaf.slots[insertPos+1:], leaf.slots[insertPos:])
	leaf.slots[insertPos] = newSlot

	// Handle overflow.
	return rt.handleLeafOverflow(tx, path)
}

// handleLeafOverflow handles a leaf that may have exceeded maxM.
func (rt *RTree) handleLeafOverflow(tx fdb.WritableTransaction, path *updatePath) error {
	leaf := path.leaf
	if len(leaf.slots) <= rt.config.MaxM {
		// No overflow — just write and propagate.
		rt.storage.writeLeafNode(tx, leaf)
		rt.propagateMBRUp(tx, path)
		return nil
	}

	// Root leaf overflow → split root.
	if len(path.parents) == 0 {
		return rt.splitRootLeaf(tx, leaf)
	}

	// Non-root overflow → redistribute with siblings.
	return rt.overflowLeaf(tx, path)
}

// splitRootLeaf splits a root leaf into two leaves + new intermediate root.
func (rt *RTree) splitRootLeaf(tx fdb.WritableTransaction, root *leafNode) error {
	if len(root.slots) < 2 { // maxM 0: an empty left half (RTree.java:1070)
		return &IndexOutOfBoundsError{Index: 0, Length: 0}
	}
	mid := len(root.slots) / 2

	leftID, err := rt.storage.newRandomNodeID()
	if err != nil {
		return err
	}
	rightID, err := rt.storage.newRandomNodeID()
	if err != nil {
		return err
	}

	left := &leafNode{id: leftID, slots: make([]ItemSlot, mid)}
	copy(left.slots, root.slots[:mid])
	right := &leafNode{id: rightID, slots: make([]ItemSlot, len(root.slots)-mid)}
	copy(right.slots, root.slots[mid:])

	rt.storage.writeLeafNode(tx, left)
	rt.storage.writeLeafNode(tx, right)

	// Root becomes intermediate. It held no child slots before (it was a
	// leaf), so it has no node slot index entries to replace.
	newRoot := &intermediateNode{
		id: rootNodeID,
		slots: []ChildSlot{
			rt.childSlotForLeaf(left),
			rt.childSlotForLeaf(right),
		},
		height: 1,
	}
	rt.storage.writeIntermediateNode(tx, newRoot)
	return nil
}

// overflowLeaf redistributes leaf items among siblings when overflow occurs.
func (rt *RTree) overflowLeaf(tx fdb.WritableTransaction, path *updatePath) error {
	parentIdx := len(path.parents) - 1
	parent := path.parents[parentIdx]
	childIdx := path.indices[parentIdx]

	// Gather S siblings centered on childIdx.
	siblings, startIdx, err := rt.gatherLeafSiblings(tx, parent, childIdx)
	if err != nil {
		return err
	}
	if len(siblings) == 0 { // splitS 0: the split links after sibling -1 (RTree.java:952)
		return &IndexOutOfBoundsError{Index: -1, Length: 0}
	}

	// Replace the re-fetched copy of the overflowing leaf with the in-memory
	// version that contains the newly inserted item. gatherLeafSiblings reads
	// from FDB which still has the old data since we haven't written yet.
	for i, sib := range siblings {
		if nodeIDEqual(sib.id, path.leaf.id) {
			siblings[i] = path.leaf
			break
		}
	}

	// Collect all items.
	var allItems []ItemSlot
	for _, sib := range siblings {
		allItems = append(allItems, sib.slots...)
	}

	// Check if we need to split (all siblings at maxM).
	needSplit := true
	for _, sib := range siblings {
		if len(sib.slots) < rt.config.MaxM {
			needSplit = false
			break
		}
	}

	if needSplit {
		// Split: create one new sibling, redistribute across S+1 nodes.
		newID, err := rt.storage.newRandomNodeID()
		if err != nil {
			return err
		}
		newSibling := &leafNode{id: newID}
		siblings = append(siblings, newSibling)
	}

	// Redistribute items evenly.
	rt.redistributeItems(allItems, siblings)

	// Write all siblings.
	for _, sib := range siblings {
		rt.storage.writeLeafNode(tx, sib)
	}

	// Update parent's child slots.
	// Remove old slots.
	newParentSlots := make([]ChildSlot, 0, len(parent.slots)+1)
	newParentSlots = append(newParentSlots, parent.slots[:startIdx]...)
	for _, sib := range siblings {
		newParentSlots = append(newParentSlots, rt.childSlotForLeaf(sib))
	}
	if startIdx+len(siblings)-1 < len(parent.slots) {
		endIdx := startIdx + len(siblings)
		if needSplit {
			endIdx-- // one less old slot since we added a new one
		}
		if endIdx < len(parent.slots) {
			newParentSlots = append(newParentSlots, parent.slots[endIdx:]...)
		}
	}
	parent.slots = newParentSlots

	// Check if parent intermediate node now overflows.
	return rt.handleIntermediateOverflow(tx, path, parentIdx)
}

// handleIntermediateOverflow handles an intermediate node that may have exceeded maxM.
// Called after modifying an intermediate node's child slots (e.g., after leaf overflow
// added a new child slot). Cascades upward if needed.
func (rt *RTree) handleIntermediateOverflow(tx fdb.WritableTransaction, path *updatePath, level int) error {
	node := path.parents[level]
	if len(node.slots) <= rt.config.MaxM {
		// No overflow — just write and propagate.
		rt.storage.writeIntermediateNode(tx, node)
		rt.propagateParentMBRUp(tx, path, level)
		return nil
	}

	if level == 0 {
		// Root intermediate overflow — split root.
		return rt.splitRootIntermediate(tx, node)
	}

	// Non-root intermediate overflow — redistribute with siblings.
	return rt.overflowIntermediate(tx, path, level)
}

// splitRootIntermediate splits an overflowing root intermediate node into two
// intermediate children under a new root.
func (rt *RTree) splitRootIntermediate(tx fdb.WritableTransaction, root *intermediateNode) error {
	mid := len(root.slots) / 2

	leftID, err := rt.storage.newRandomNodeID()
	if err != nil {
		return err
	}
	rightID, err := rt.storage.newRandomNodeID()
	if err != nil {
		return err
	}

	left := &intermediateNode{id: leftID, slots: make([]ChildSlot, mid), height: root.height}
	copy(left.slots, root.slots[:mid])

	right := &intermediateNode{id: rightID, slots: make([]ChildSlot, len(root.slots)-mid), height: root.height}
	copy(right.slots, root.slots[mid:])

	rt.storage.writeIntermediateNode(tx, left)
	rt.storage.writeIntermediateNode(tx, right)

	// Root becomes an intermediate pointing to left + right, one level up.
	// The root's own struct takes the new slots, so what it was fetched with
	// stays what the node slot index difference starts from.
	root.slots = []ChildSlot{
		rt.childSlotForIntermediate(left),
		rt.childSlotForIntermediate(right),
	}
	root.height++
	rt.storage.writeIntermediateNode(tx, root)
	return nil
}

// overflowIntermediate redistributes child slots among sibling intermediate nodes
// when a non-root intermediate node overflows. Mirrors overflowLeaf logic.
func (rt *RTree) overflowIntermediate(tx fdb.WritableTransaction, path *updatePath, level int) error {
	grandparentIdx := level - 1
	grandparent := path.parents[grandparentIdx]
	childIdx := path.indices[grandparentIdx]

	// Gather S siblings centered on childIdx.
	siblings, startIdx, err := rt.gatherIntermediateSiblings(tx, grandparent, childIdx)
	if err != nil {
		return err
	}

	// Replace the re-fetched copy of the overflowing node with the in-memory
	// version that has the updated child slots.
	overflowNode := path.parents[level]
	for i, sib := range siblings {
		if nodeIDEqual(sib.id, overflowNode.id) {
			siblings[i] = overflowNode
			break
		}
	}

	// Collect all child slots from siblings.
	var allSlots []ChildSlot
	for _, sib := range siblings {
		allSlots = append(allSlots, sib.slots...)
	}

	// Check if we need to split (all siblings at maxM).
	needSplit := true
	for _, sib := range siblings {
		if len(sib.slots) < rt.config.MaxM {
			needSplit = false
			break
		}
	}

	if needSplit {
		// Split: create one new sibling, redistribute across S+1 nodes.
		newID, err := rt.storage.newRandomNodeID()
		if err != nil {
			return err
		}
		newSibling := &intermediateNode{id: newID, height: overflowNode.height}
		siblings = append(siblings, newSibling)
	}

	// Redistribute child slots evenly.
	rt.redistributeChildSlots(allSlots, siblings)

	// Write all siblings.
	for _, sib := range siblings {
		rt.storage.writeIntermediateNode(tx, sib)
	}

	// Update grandparent's child slots.
	newGPSlots := make([]ChildSlot, 0, len(grandparent.slots)+1)
	newGPSlots = append(newGPSlots, grandparent.slots[:startIdx]...)
	for _, sib := range siblings {
		newGPSlots = append(newGPSlots, rt.childSlotForIntermediate(sib))
	}
	if startIdx+len(siblings)-1 < len(grandparent.slots) {
		endIdx := startIdx + len(siblings)
		if needSplit {
			endIdx-- // one less old slot since we added a new one
		}
		if endIdx < len(grandparent.slots) {
			newGPSlots = append(newGPSlots, grandparent.slots[endIdx:]...)
		}
	}
	grandparent.slots = newGPSlots

	// Recursively check grandparent for overflow.
	return rt.handleIntermediateOverflow(tx, path, grandparentIdx)
}
