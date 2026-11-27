// Portions derived from FoundationDB Record Layer (RTree.java, ChildSlot.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rtree

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Delete removes an item from the R-tree.
// Matches Java's RTree.delete().
func (rt *RTree) Delete(tx fdb.WritableTransaction, point Point, keySuffix tuple.Tuple) error {
	rt.storage.beginOperation()
	if err := rt.delete(tx, point, keySuffix); err != nil {
		return err
	}
	if err := rt.storage.checkWritten(); err != nil {
		return err
	}
	return rt.storage.flushNodeSlotIndex(tx)
}

func (rt *RTree) delete(tx fdb.WritableTransaction, point Point, keySuffix tuple.Tuple) error {
	coords := make([]int64, point.NumDimensions())
	for d := 0; d < len(coords); d++ {
		coords[d] = point.Coordinate(d)
	}
	hv := hilbertValue(coords)
	itemKey := tuple.Tuple{point.Coordinates, keySuffix}

	path, err := rt.fetchUpdatePathToLeaf(tx, hv, itemKey, false)
	if err != nil {
		return err
	}

	leaf := path.leaf
	if leaf == nil {
		return nil // empty tree or item not reachable
	}
	if len(leaf.slots) > rt.config.MaxM { // RTree.java:1134
		return &VerifyError{}
	}

	// Find the item.
	found := -1
	for i := range leaf.slots {
		if compareHilbertValueAndKey(leaf.slots[i].HilbertValue, leaf.slots[i].ItemKey(), hv, itemKey) == 0 {
			found = i
			break
		}
	}
	if found < 0 {
		return nil // not found
	}

	// Remove the item.
	leaf.slots = append(leaf.slots[:found], leaf.slots[found+1:]...)

	// Handle underflow.
	return rt.handleLeafUnderflow(tx, path)
}

// handleLeafUnderflow handles a leaf that may be below minM.
func (rt *RTree) handleLeafUnderflow(tx fdb.WritableTransaction, path *updatePath) error {
	leaf := path.leaf

	// Root leaf can have any number of items (even 0).
	if len(path.parents) == 0 {
		if len(leaf.slots) == 0 {
			// Tree is now empty — delete root.
			rt.storage.deleteLeafNode(tx, leaf)
		} else {
			rt.storage.writeLeafNode(tx, leaf)
		}
		return nil
	}

	if len(leaf.slots) == 0 && rt.config.MinM <= 0 {
		// minM 0 or below never fuses; Java then computes the empty node's MBR
		// (RTree.java:1447, NodeHelpers.java:148).
		return &NullPointerError{}
	}
	if len(leaf.slots) >= rt.config.MinM {
		// No underflow.
		rt.storage.writeLeafNode(tx, leaf)
		rt.propagateMBRUp(tx, path)
		return nil
	}

	// Underflow — redistribute with siblings.
	parentIdx := len(path.parents) - 1
	parent := path.parents[parentIdx]
	childIdx := path.indices[parentIdx]

	siblings, startIdx, err := rt.gatherLeafSiblings(tx, parent, childIdx)
	if err != nil {
		return err
	}

	// Replace the re-fetched copy of the underflowing leaf with the in-memory
	// version that has the item already removed.
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

	// Check if we need to fuse (all siblings at minM).
	needFuse := true
	for _, sib := range siblings {
		if len(sib.slots) > rt.config.MinM {
			needFuse = false
			break
		}
	}

	if needFuse && len(siblings) < 2 {
		return rt.loneFuseError(len(siblings), len(parent.slots))
	}
	if needFuse {
		// Fuse: remove the last sibling, redistribute across S-1 nodes.
		removedSib := siblings[len(siblings)-1]
		rt.storage.deleteLeafNode(tx, removedSib)
		siblings = siblings[:len(siblings)-1]
	}

	// Redistribute items evenly.
	rt.redistributeItems(allItems, siblings)

	for _, sib := range siblings {
		rt.storage.writeLeafNode(tx, sib)
	}

	// Update parent's child slots.
	newParentSlots := make([]ChildSlot, 0, len(parent.slots))
	newParentSlots = append(newParentSlots, parent.slots[:startIdx]...)
	for _, sib := range siblings {
		newParentSlots = append(newParentSlots, rt.childSlotForLeaf(sib))
	}
	endIdx := startIdx + len(siblings)
	if needFuse {
		endIdx++ // account for removed sibling
	}
	if endIdx < len(parent.slots) {
		newParentSlots = append(newParentSlots, parent.slots[endIdx:]...)
	}
	parent.slots = newParentSlots

	// Check if parent intermediate node now underflows.
	return rt.handleIntermediateUnderflow(tx, path, parentIdx)
}

// handleIntermediateUnderflow handles an intermediate node that may have dropped
// below minM after a child was removed. Cascades upward if needed.
func (rt *RTree) handleIntermediateUnderflow(tx fdb.WritableTransaction, path *updatePath, level int) error {
	node := path.parents[level]

	if level == 0 {
		// Root can have any count. Special cases:
		if len(node.slots) == 1 {
			// Single child — promote it to root.
			return rt.promoteOnlyChild(tx, node)
		}
		if len(node.slots) == 0 {
			// No children — tree is empty.
			rt.storage.deleteIntermediateNode(tx, node)
			return nil
		}
		// Root with 2+ children — just write it.
		rt.storage.writeIntermediateNode(tx, node)
		return nil
	}

	if len(node.slots) >= rt.config.MinM {
		// No underflow — just write and propagate.
		rt.storage.writeIntermediateNode(tx, node)
		rt.propagateParentMBRUp(tx, path, level)
		return nil
	}

	// Underflow — redistribute with siblings at intermediate level.
	return rt.fuseIntermediate(tx, path, level)
}

// promoteOnlyChild promotes the single child of a root intermediate node to become
// the new root. The old child node is deleted and its contents written at the root ID.
func (rt *RTree) promoteOnlyChild(tx fdb.WritableTransaction, root *intermediateNode) error {
	childID := root.slots[0].ChildID
	leaf, inter, err := rt.storage.fetchNode(tx, childID)
	if err != nil {
		return err
	}

	if leaf == nil && inter == nil {
		return fmt.Errorf("rtree: promoteOnlyChild: child node not found")
	}

	if leaf != nil {
		rt.storage.deleteLeafNode(tx, leaf)
		// The root stops being an intermediate node: its one child slot
		// leaves the node slot index.
		rt.storage.deleteIntermediateNode(tx, root)
		leaf.id = rootNodeID
		rt.storage.writeLeafNode(tx, leaf)
		return nil
	}
	setNodeHeight(inter, root.height-1)
	rt.storage.deleteIntermediateNode(tx, inter)
	// The child's slots move to the root one level down. The root's own
	// struct takes them, so what it was fetched with (its one slot) stays
	// what the node slot index difference starts from.
	root.slots, root.height = inter.slots, inter.height
	rt.storage.writeIntermediateNode(tx, root)
	return nil
}

// fuseIntermediate redistributes child slots among sibling intermediate nodes
// when a non-root intermediate node underflows. Mirrors handleLeafUnderflow logic.
func (rt *RTree) fuseIntermediate(tx fdb.WritableTransaction, path *updatePath, level int) error {
	grandparentIdx := level - 1
	grandparent := path.parents[grandparentIdx]
	childIdx := path.indices[grandparentIdx]

	siblings, startIdx, err := rt.gatherIntermediateSiblings(tx, grandparent, childIdx)
	if err != nil {
		return err
	}

	// Replace the re-fetched copy of the underflowing node with the in-memory
	// version that has the updated child slots.
	underflowNode := path.parents[level]
	for i, sib := range siblings {
		if nodeIDEqual(sib.id, underflowNode.id) {
			siblings[i] = underflowNode
			break
		}
	}

	// Collect all child slots.
	var allSlots []ChildSlot
	for _, sib := range siblings {
		allSlots = append(allSlots, sib.slots...)
	}

	// Check if we need to fuse (all siblings at minM).
	needFuse := true
	for _, sib := range siblings {
		if len(sib.slots) > rt.config.MinM {
			needFuse = false
			break
		}
	}

	if needFuse && len(siblings) > 1 {
		// Fuse: remove the last sibling, redistribute across S-1 nodes.
		removedSib := siblings[len(siblings)-1]
		rt.storage.deleteIntermediateNode(tx, removedSib)
		siblings = siblings[:len(siblings)-1]
	}

	// Redistribute child slots evenly.
	rt.redistributeChildSlots(allSlots, siblings)

	// Write all siblings.
	for _, sib := range siblings {
		rt.storage.writeIntermediateNode(tx, sib)
	}

	// Update grandparent's child slots.
	newGPSlots := make([]ChildSlot, 0, len(grandparent.slots))
	newGPSlots = append(newGPSlots, grandparent.slots[:startIdx]...)
	for _, sib := range siblings {
		newGPSlots = append(newGPSlots, rt.childSlotForIntermediate(sib))
	}
	endIdx := startIdx + len(siblings)
	if needFuse {
		endIdx++ // account for removed sibling
	}
	if endIdx < len(grandparent.slots) {
		newGPSlots = append(newGPSlots, grandparent.slots[endIdx:]...)
	}
	grandparent.slots = newGPSlots

	// Recursively check grandparent for underflow.
	return rt.handleIntermediateUnderflow(tx, path, grandparentIdx)
}

// loneFuseError is how Java's fuse fails with fewer than two siblings: splitS
// 0 tombstones sibling -1 (RTree.java:1273), splitS 1 tombstones the node itself
// and divides by the zero left (:1304), and a parent with fewer children than
// splitS makes fetchSiblings read past them (:1646), where Go takes those it has.
func (rt *RTree) loneFuseError(siblings, parentSlots int) error {
	switch {
	case siblings == 0:
		return &IndexOutOfBoundsError{Index: -1, Length: 0}
	case rt.config.SplitS == 1:
		return &ArithmeticError{Message: "/ by zero"}
	default:
		return &IndexOutOfBoundsError{Index: parentSlots, Length: parentSlots}
	}
}
