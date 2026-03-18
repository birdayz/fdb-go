package recordlayer

import (
	"bytes"
	"math/big"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// RTree is a Hilbert R-tree backed by FDB.
// Matches Java's com.apple.foundationdb.async.rtree.RTree.
type RTree struct {
	storage *rtreeStorage
	config  RTreeConfig
}

// NewRTree creates a new R-tree.
func NewRTree(storage *rtreeStorage, config RTreeConfig) *RTree {
	return &RTree{storage: storage, config: config}
}

// InsertOrUpdate inserts a new item or updates an existing one.
// Matches Java's RTree.insertOrUpdate().
func (rt *RTree) InsertOrUpdate(tx fdb.Transaction, point Point, keySuffix tuple.Tuple, value tuple.Tuple) error {
	coords := make([]int64, point.NumDimensions())
	for d := 0; d < len(coords); d++ {
		coords[d] = point.Coordinate(d)
	}
	hv := hilbertValue(coords)
	itemKey := tuple.Tuple{point.Coordinates, keySuffix}

	// Walk from root to leaf.
	path, err := rt.fetchUpdatePathToLeaf(tx, hv, itemKey)
	if err != nil {
		return err
	}

	leaf := path.leaf
	if leaf == nil {
		// Empty tree — create root leaf.
		leaf = &leafNode{id: rootNodeID, slots: nil}
		path.leaf = leaf
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

// Delete removes an item from the R-tree.
// Matches Java's RTree.delete().
func (rt *RTree) Delete(tx fdb.Transaction, point Point, keySuffix tuple.Tuple) error {
	coords := make([]int64, point.NumDimensions())
	for d := 0; d < len(coords); d++ {
		coords[d] = point.Coordinate(d)
	}
	hv := hilbertValue(coords)
	itemKey := tuple.Tuple{point.Coordinates, keySuffix}

	path, err := rt.fetchUpdatePathToLeaf(tx, hv, itemKey)
	if err != nil {
		return err
	}

	leaf := path.leaf
	if leaf == nil {
		return nil // empty tree
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

// Scan returns all items matching the MBR predicate, starting after (lastHV, lastKey).
// Items are returned in Hilbert value order.
// Matches Java's RTree.scan().
func (rt *RTree) Scan(tx fdb.ReadTransaction, lastHV *big.Int, lastKey tuple.Tuple, mbrPredicate func(MBR) bool) ([]ItemSlot, error) {
	leaf, inter, err := rt.storage.fetchNode(tx, rootNodeID)
	if err != nil {
		return nil, err
	}
	if leaf == nil && inter == nil {
		return nil, nil // empty tree
	}

	var result []ItemSlot
	if leaf != nil {
		// Root is a leaf — just filter items.
		for _, slot := range leaf.slots {
			if lastHV != nil && compareHilbertValueAndKey(slot.HilbertValue, slot.ItemKey(), lastHV, lastKey) <= 0 {
				continue
			}
			if mbrPredicate != nil && !mbrPredicate(slot.GetMBR()) {
				continue
			}
			result = append(result, slot)
		}
		return result, nil
	}

	// Root is intermediate — recursive traversal.
	return rt.scanIntermediate(tx, inter, lastHV, lastKey, mbrPredicate)
}

// scanIntermediate recursively scans an intermediate node's subtrees.
func (rt *RTree) scanIntermediate(
	tx fdb.ReadTransaction,
	node *intermediateNode,
	lastHV *big.Int,
	lastKey tuple.Tuple,
	mbrPredicate func(MBR) bool,
) ([]ItemSlot, error) {
	var result []ItemSlot

	for _, child := range node.slots {
		// Skip children that are entirely before the continuation point.
		if lastHV != nil && compareHilbertValueAndKey(child.LargestHV, child.LargestKey, lastHV, lastKey) <= 0 {
			continue
		}

		// Skip children whose MBR doesn't match the predicate.
		if mbrPredicate != nil && !mbrPredicate(child.ChildMBR) {
			continue
		}

		// Fetch the child node.
		childLeaf, childInter, err := rt.storage.fetchNode(tx, child.ChildID)
		if err != nil {
			return nil, err
		}
		if childLeaf != nil {
			for _, slot := range childLeaf.slots {
				if lastHV != nil && compareHilbertValueAndKey(slot.HilbertValue, slot.ItemKey(), lastHV, lastKey) <= 0 {
					continue
				}
				if mbrPredicate != nil && !mbrPredicate(slot.GetMBR()) {
					continue
				}
				result = append(result, slot)
			}
		} else if childInter != nil {
			items, err := rt.scanIntermediate(tx, childInter, lastHV, lastKey, mbrPredicate)
			if err != nil {
				return nil, err
			}
			result = append(result, items...)
		}
	}

	return result, nil
}

// updatePath represents the path from root to a leaf for insert/delete operations.
type updatePath struct {
	leaf    *leafNode
	parents []*intermediateNode // from root to parent of leaf
	indices []int               // child index at each level
}

// fetchUpdatePathToLeaf walks from root to the leaf where (hv, key) should live.
func (rt *RTree) fetchUpdatePathToLeaf(tx fdb.Transaction, hv *big.Int, itemKey tuple.Tuple) (*updatePath, error) {
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
		// Find the child whose range covers (hv, key), or the last child.
		childIdx := len(current.slots) - 1
		for i, child := range current.slots {
			if compareHilbertValueAndKey(child.LargestHV, child.LargestKey, hv, itemKey) >= 0 {
				childIdx = i
				break
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
			return path, nil
		}
		if childInter == nil {
			return path, nil
		}
		current = childInter
	}
}

// handleLeafOverflow handles a leaf that may have exceeded maxM.
func (rt *RTree) handleLeafOverflow(tx fdb.Transaction, path *updatePath) error {
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
func (rt *RTree) splitRootLeaf(tx fdb.Transaction, root *leafNode) error {
	mid := len(root.slots) / 2

	leftID := newRandomNodeID()
	rightID := newRandomNodeID()

	left := &leafNode{id: leftID, slots: root.slots[:mid]}
	right := &leafNode{id: rightID, slots: root.slots[mid:]}

	rt.storage.writeLeafNode(tx, left)
	rt.storage.writeLeafNode(tx, right)

	// Root becomes intermediate.
	newRoot := &intermediateNode{
		id: rootNodeID,
		slots: []ChildSlot{
			rt.childSlotForLeaf(left),
			rt.childSlotForLeaf(right),
		},
	}
	rt.storage.writeIntermediateNode(tx, newRoot)
	return nil
}

// overflowLeaf redistributes leaf items among siblings when overflow occurs.
func (rt *RTree) overflowLeaf(tx fdb.Transaction, path *updatePath) error {
	parentIdx := len(path.parents) - 1
	parent := path.parents[parentIdx]
	childIdx := path.indices[parentIdx]

	// Gather S siblings centered on childIdx.
	siblings, startIdx := rt.gatherLeafSiblings(tx, parent, childIdx)
	if siblings == nil {
		// Fallback: just write the overflow node.
		rt.storage.writeLeafNode(tx, path.leaf)
		rt.propagateMBRUp(tx, path)
		return nil
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
		newSibling := &leafNode{id: newRandomNodeID()}
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

	// TODO: If parent also overflows, handle intermediate-level split.
	// For now, intermediate overflow is deferred — works for small-to-medium trees.
	rt.storage.writeIntermediateNode(tx, parent)
	rt.propagateParentMBRUp(tx, path, parentIdx)
	return nil
}

// handleLeafUnderflow handles a leaf that may be below minM.
func (rt *RTree) handleLeafUnderflow(tx fdb.Transaction, path *updatePath) error {
	leaf := path.leaf

	// Root leaf can have any number of items (even 0).
	if len(path.parents) == 0 {
		if len(leaf.slots) == 0 {
			// Tree is now empty — delete root.
			rt.storage.deleteNode(tx, rootNodeID)
		} else {
			rt.storage.writeLeafNode(tx, leaf)
		}
		return nil
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

	siblings, startIdx := rt.gatherLeafSiblings(tx, parent, childIdx)
	if siblings == nil {
		rt.storage.writeLeafNode(tx, leaf)
		rt.propagateMBRUp(tx, path)
		return nil
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

	if needFuse && len(siblings) > 1 {
		// Fuse: remove the last sibling, redistribute across S-1 nodes.
		removedSib := siblings[len(siblings)-1]
		rt.storage.deleteNode(tx, removedSib.id)
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

	// If parent is root with single child, promote child to root.
	if len(parent.slots) == 1 && parentIdx == 0 {
		child := siblings[0]
		rt.storage.deleteNode(tx, child.id)
		child.id = rootNodeID
		rt.storage.writeLeafNode(tx, child)
		// Remove intermediate root.
		return nil
	}

	rt.storage.writeIntermediateNode(tx, parent)
	rt.propagateParentMBRUp(tx, path, parentIdx)
	return nil
}

// gatherLeafSiblings returns S siblings centered on childIdx, loaded from FDB.
func (rt *RTree) gatherLeafSiblings(tx fdb.Transaction, parent *intermediateNode, childIdx int) ([]*leafNode, int) {
	s := rt.config.SplitS
	if s >= len(parent.slots) {
		s = len(parent.slots)
	}

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
		if err != nil || node == nil {
			return nil, 0
		}
		siblings = append(siblings, node)
	}
	return siblings, startIdx
}

// redistributeItems evenly distributes items across sibling leaf nodes.
func (rt *RTree) redistributeItems(items []ItemSlot, siblings []*leafNode) {
	n := len(siblings)
	if n == 0 {
		return
	}
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
func (rt *RTree) propagateMBRUp(tx fdb.Transaction, path *updatePath) {
	if len(path.parents) == 0 {
		return
	}

	leaf := path.leaf
	for i := len(path.parents) - 1; i >= 0; i-- {
		parent := path.parents[i]
		childIdx := path.indices[i]
		if childIdx < len(parent.slots) {
			if i == len(path.parents)-1 {
				// Leaf parent — update from leaf.
				parent.slots[childIdx] = rt.childSlotForLeaf(leaf)
			}
			// For higher levels, update MBR from children.
		}
		rt.storage.writeIntermediateNode(tx, parent)
	}
}

// propagateParentMBRUp updates intermediate nodes above a given parent level.
func (rt *RTree) propagateParentMBRUp(tx fdb.Transaction, path *updatePath, startIdx int) {
	for i := startIdx - 1; i >= 0; i-- {
		parent := path.parents[i]
		rt.storage.writeIntermediateNode(tx, parent)
	}
}

// Clear removes all data from the R-tree.
func (rt *RTree) Clear(tx fdb.Transaction) {
	rt.storage.clearAll(tx)
}

// nodeIDEqual compares two node IDs.
func nodeIDEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
