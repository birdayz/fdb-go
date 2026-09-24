package recordlayer

import (
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sort"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// rtreeStorage serializes R-tree nodes in FDB in the layout the index's
// rtreeStorage option names (RTreeConfig.Storage):
//
//   - BY_NODE (Java's ByNodeStorageAdapter): each node is one key-value pair,
//     subspace.pack(nodeId) → tuple(nodeKind, slotList), slotList a nested
//     tuple holding each slot as a sub-tuple (its key items, then its value
//     items).
//   - BY_SLOT (Java's BySlotStorageAdapter): each slot is its own pair under
//     the node, subspace.pack(nodeId, nodeKind, slotKey...) → tuple(slotValue...).
//
// With the rtreeUseNodeSlotIndex option it also maintains Java's node slot
// index (NodeSlotIndexAdapter): an empty-valued key (level, largestHilbertValue,
// largestKey items..., childId) in the index's secondary subspace for every
// child slot of every intermediate node, level being the child's (a leaf is
// level 0). The index is a function of the tree alone, so Go maintains it as
// the difference the operation made to the intermediate nodes it touched
// (flushNodeSlotIndex) rather than per slot change as Java's change sets do;
// the stored entries are the same.
type rtreeStorage struct {
	subspace subspace.Subspace
	config   RTreeConfig

	// nodeSlotIndex is the node slot index's subspace (the index's secondary
	// subspace, then the indicator 0, then the prefix; Java's
	// MultidimensionalIndexMaintainer.getNodeSlotIndexSubspace), and
	// hasNodeSlotIndex says the maintainer set it.
	nodeSlotIndex    subspace.Subspace
	hasNodeSlotIndex bool
	// touched records, per operation, the node slot index entries of each
	// intermediate node the operation wrote or deleted: the entries it held
	// when fetched and the ones it holds now.
	touched map[string]*nodeSlotIndexTouch

	// env is the DST Tier-0 environment routing node-ID entropy. Nil means
	// production (crypto/rand); the maintainer sets it from the record
	// context so a simulation run mints reproducible node IDs. The *dst.Env
	// accessors are nil-safe, so leaving it unset keeps production byte-identical.
	env *dst.Env
}

func newRTreeStorage(ss subspace.Subspace, config RTreeConfig) *rtreeStorage {
	return &rtreeStorage{subspace: ss, config: config}
}

// withNodeSlotIndex sets the node slot index's subspace, which an R-tree with
// the rtreeUseNodeSlotIndex option maintains.
func (s *rtreeStorage) withNodeSlotIndex(sub subspace.Subspace) *rtreeStorage {
	s.nodeSlotIndex, s.hasNodeSlotIndex = sub, true
	return s
}

// nodeSlotIndexTouch is one intermediate node's node slot index entries
// before and after an operation.
type nodeSlotIndexTouch struct {
	orig, cur [][]byte
}

// newRandomNodeID generates a random 16-byte UUID for a new node, drawing
// entropy through the DST randomness seam (crypto/rand in production, the
// seeded source in simulation). Matches Java's NodeHelpers.newRandomNodeId().
func (s *rtreeStorage) newRandomNodeID() ([]byte, error) {
	id := make([]byte, 16)
	if _, err := s.env.Read(id); err != nil {
		return nil, fmt.Errorf("rtree: generate node ID: %w", err)
	}
	return id, nil
}

// fetchLeafNode loads a leaf node from FDB. Returns nil if not found.
func (s *rtreeStorage) fetchLeafNode(tx fdb.ReadTransaction, nodeID []byte) (*leafNode, error) {
	leaf, inter, err := s.fetchNode(tx, nodeID)
	if err != nil {
		return nil, err
	}
	if inter != nil {
		return nil, fmt.Errorf("rtree: expected leaf node, got kind %d", NodeKindIntermediate)
	}
	return leaf, nil
}

// fetchIntermediateNode loads an intermediate node from FDB. Returns nil if not found.
func (s *rtreeStorage) fetchIntermediateNode(tx fdb.ReadTransaction, nodeID []byte) (*intermediateNode, error) {
	leaf, inter, err := s.fetchNode(tx, nodeID)
	if err != nil {
		return nil, err
	}
	if leaf != nil {
		return nil, fmt.Errorf("rtree: expected intermediate node, got kind %d", NodeKindLeaf)
	}
	return inter, nil
}

// fetchNode loads any node. Returns (leaf, nil, err) or (nil, intermediate, err).
// An intermediate node keeps a copy of the slots it was fetched with, the
// node slot index entries it held before the operation.
func (s *rtreeStorage) fetchNode(tx fdb.ReadTransaction, nodeID []byte) (*leafNode, *intermediateNode, error) {
	var leaf *leafNode
	var inter *intermediateNode
	var err error
	if s.config.Storage == RTreeStorageBySlot {
		leaf, inter, err = s.fetchNodeBySlot(tx, nodeID)
	} else {
		leaf, inter, err = s.fetchNodeByNode(tx, nodeID)
	}
	if inter != nil {
		inter.orig, inter.fetched = slices.Clone(inter.slots), true
	}
	return leaf, inter, err
}

// fetchNodeByNode reads a BY_NODE node's one key-value pair.
func (s *rtreeStorage) fetchNodeByNode(tx fdb.ReadTransaction, nodeID []byte) (*leafNode, *intermediateNode, error) {
	key := s.subspace.Pack(tuple.Tuple{nodeID})
	data, err := tx.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, nil, err
	}
	if data == nil {
		return nil, nil, nil
	}
	t, err := fastUnpack(data)
	if err != nil {
		return nil, nil, fmt.Errorf("rtree: unpack node: %w", err)
	}
	if len(t) < 1 {
		return nil, nil, fmt.Errorf("rtree: empty node tuple")
	}
	kind, _ := asInt64(t[0])
	if len(t) < 2 {
		return nil, nil, fmt.Errorf("rtree: node tuple missing slot list")
	}
	slotList, ok := t[1].(tuple.Tuple)
	if !ok {
		return nil, nil, fmt.Errorf("rtree: node slot list is not a tuple")
	}
	return s.nodeFromSlotList(nodeID, NodeKind(kind), slotList)
}

// fetchNodeBySlot reads a BY_SLOT node, one key-value pair per slot, as Java's
// BySlotStorageAdapter.fetchNodeInternal and fromKeyValues do: every pair must
// carry the same node kind, and a leaf's slots are sorted by Hilbert value and
// key when the values are not stored (FDB's order is then the key's alone).
func (s *rtreeStorage) fetchNodeBySlot(tx fdb.ReadTransaction, nodeID []byte) (*leafNode, *intermediateNode, error) {
	prefix := s.subspace.Pack(tuple.Tuple{nodeID})
	r, err := fdb.PrefixRange(prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("rtree: node prefix range: %w", err)
	}
	kvs, err := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, nil, err
	}
	if len(kvs) == 0 {
		return nil, nil, nil
	}
	var kind NodeKind
	haveKind := false
	slotList := make(tuple.Tuple, 0, len(kvs))
	for _, kv := range kvs {
		keyTuple, err := tuple.Unpack(kv.Key[len(prefix):])
		if err != nil {
			return nil, nil, fmt.Errorf("rtree: unpack slot key: %w", err)
		}
		valueTuple, err := tuple.Unpack(kv.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("rtree: unpack slot value: %w", err)
		}
		if len(keyTuple) < 1 {
			return nil, nil, fmt.Errorf("rtree: empty slot key tuple")
		}
		k, _ := asInt64(keyTuple[0])
		if !haveKind {
			kind, haveKind = NodeKind(k), true
		} else if kind != NodeKind(k) {
			return nil, nil, &IllegalArgumentError{Message: "same node id uses different node kinds"}
		}
		slot := make(tuple.Tuple, 0, len(keyTuple)-1+len(valueTuple))
		slot = append(slot, keyTuple[1:]...)
		slot = append(slot, valueTuple...)
		slotList = append(slotList, slot)
	}
	leaf, inter, err := s.nodeFromSlotList(nodeID, kind, slotList)
	if err == nil && leaf != nil && !s.config.StoreHilbertValues {
		sort.SliceStable(leaf.slots, func(i, j int) bool {
			return compareHilbertValueAndKey(leaf.slots[i].HilbertValue, leaf.slots[i].ItemKey(), leaf.slots[j].HilbertValue, leaf.slots[j].ItemKey()) < 0
		})
	}
	return leaf, inter, err
}

// nodeFromSlotList builds a node of the given kind from its slot tuples.
func (s *rtreeStorage) nodeFromSlotList(nodeID []byte, kind NodeKind, slotList tuple.Tuple) (*leafNode, *intermediateNode, error) {
	switch kind {
	case NodeKindLeaf:
		slots, err := s.deserializeItemSlots(slotList)
		if err != nil {
			return nil, nil, err
		}
		return &leafNode{id: nodeID, slots: slots}, nil, nil
	case NodeKindIntermediate:
		slots, err := s.deserializeChildSlots(slotList)
		if err != nil {
			return nil, nil, err
		}
		return nil, &intermediateNode{id: nodeID, slots: slots}, nil
	default:
		return nil, nil, fmt.Errorf("rtree: unknown node kind %d", kind)
	}
}

// writeLeafNode writes a leaf node to FDB. If the node has no slots, it is
// deleted instead (empty nodes should not exist in the tree).
func (s *rtreeStorage) writeLeafNode(tx fdb.WritableTransaction, node *leafNode) {
	if len(node.slots) == 0 {
		s.deleteNode(tx, node.id)
		return
	}
	slotList := make(tuple.Tuple, 0, len(node.slots))
	for _, slot := range node.slots {
		slotList = append(slotList, s.serializeItemSlot(slot))
	}
	s.writeSlots(tx, node.id, NodeKindLeaf, slotList, itemSlotKeySize)
}

// writeIntermediateNode writes an intermediate node to FDB. If the node has no
// slots, it is deleted instead (empty nodes should not exist in the tree).
// With the node slot index, the node's entries are recorded for the
// operation's flush.
func (s *rtreeStorage) writeIntermediateNode(tx fdb.WritableTransaction, node *intermediateNode) {
	if t := s.touch(node); t != nil {
		t.cur = s.nodeSlotIndexEntries(node.slots, node.height-1)
	}
	if len(node.slots) == 0 {
		s.deleteNode(tx, node.id)
		return
	}
	slotList := make(tuple.Tuple, 0, len(node.slots))
	for _, slot := range node.slots {
		slotList = append(slotList, s.serializeChildSlot(slot))
	}
	s.writeSlots(tx, node.id, NodeKindIntermediate, slotList, childSlotKeySize)
}

// deleteIntermediateNode removes an intermediate node, and with the node slot
// index records that it holds no entries any more.
func (s *rtreeStorage) deleteIntermediateNode(tx fdb.WritableTransaction, node *intermediateNode) {
	if t := s.touch(node); t != nil {
		t.cur = nil
	}
	s.deleteNode(tx, node.id)
}

// The number of leading items of a slot tuple that form its key in the
// BY_SLOT layout (ItemSlot.getSlotKey, ChildSlot.getSlotKey); the rest are
// its value (getSlotValue).
const (
	itemSlotKeySize  = 2 // (hilbertValue, itemKey) | (value)
	childSlotKeySize = 4 // (smallestHV, smallestKey, largestHV, largestKey) | (childId, mbr)
)

// writeSlots writes a node's slot tuples in the configured layout. BY_SLOT
// clears the node's pairs first, so the pairs of slots it no longer holds go;
// the stored pairs then equal those Java's per-slot change sets leave.
func (s *rtreeStorage) writeSlots(tx fdb.WritableTransaction, nodeID []byte, kind NodeKind, slotList tuple.Tuple, keySize int) {
	if s.config.Storage != RTreeStorageBySlot {
		tx.Set(fdb.Key(s.subspace.Pack(tuple.Tuple{nodeID})), tuple.Tuple{int64(kind), slotList}.Pack())
		return
	}
	s.deleteNode(tx, nodeID)
	for _, elem := range slotList {
		slot := elem.(tuple.Tuple)
		key := make(tuple.Tuple, 0, 2+keySize)
		key = append(key, nodeID, int64(kind))
		key = append(key, slot[:keySize]...)
		tx.Set(fdb.Key(s.subspace.Pack(key)), slot[keySize:].Pack())
	}
}

// deleteLeafNode removes a leaf from FDB. A leaf has no node slot index
// entries of its own (its parent's slot names it, and the parent's write or
// delete maintains that), so nothing else changes.
func (s *rtreeStorage) deleteLeafNode(tx fdb.WritableTransaction, node *leafNode) {
	s.deleteNode(tx, node.id)
}

// deleteNode removes a node's pairs from FDB: its one pair (BY_NODE) or every
// pair under it (BY_SLOT). It does not maintain the node slot index, so only
// the storage layer calls it; the tree deletes through deleteLeafNode and
// deleteIntermediateNode, which say what kind of node goes.
func (s *rtreeStorage) deleteNode(tx fdb.WritableTransaction, nodeID []byte) {
	key := s.subspace.Pack(tuple.Tuple{nodeID})
	if s.config.Storage != RTreeStorageBySlot {
		tx.Clear(fdb.Key(key))
		return
	}
	if r, err := fdb.PrefixRange(key); err == nil {
		tx.ClearRange(r)
	}
}

// touch returns the node's entry in the operation's node slot index record,
// recording the entries it held when fetched at its first touch; nil without
// the node slot index.
func (s *rtreeStorage) touch(node *intermediateNode) *nodeSlotIndexTouch {
	if !s.config.UseNodeSlotIndex {
		return nil
	}
	if s.touched == nil {
		s.touched = make(map[string]*nodeSlotIndexTouch)
	}
	id := string(node.id)
	t, ok := s.touched[id]
	if !ok {
		t = &nodeSlotIndexTouch{}
		if node.fetched {
			t.orig = s.nodeSlotIndexEntries(node.orig, node.origHeight-1)
		}
		s.touched[id] = t
	}
	return t
}

// nodeSlotIndexEntries are the node slot index keys of an intermediate node's
// child slots, its children at childLevel (NodeSlotIndexAdapter.
// createIndexKeyTuple: the level, the largest Hilbert value, the largest
// key's items, the child id).
func (s *rtreeStorage) nodeSlotIndexEntries(slots []ChildSlot, childLevel int) [][]byte {
	out := make([][]byte, 0, len(slots))
	for _, slot := range slots {
		t := make(tuple.Tuple, 0, 3+len(slot.LargestKey))
		t = append(t, int64(childLevel), slot.LargestHV)
		t = append(t, slot.LargestKey...)
		t = append(t, slot.ChildID)
		out = append(out, s.nodeSlotIndex.Pack(t))
	}
	return out
}

// beginOperation starts an insert or delete's node slot index record.
func (s *rtreeStorage) beginOperation() {
	s.touched = nil
}

// flushNodeSlotIndex writes the node slot index difference of the operation:
// an entry the touched nodes held before and hold no more is cleared, one
// they hold now and did not is set. Entries of untouched nodes did not change.
func (s *rtreeStorage) flushNodeSlotIndex(tx fdb.WritableTransaction) error {
	defer func() { s.touched = nil }()
	if !s.config.UseNodeSlotIndex || len(s.touched) == 0 {
		return nil
	}
	if !s.hasNodeSlotIndex {
		return fmt.Errorf("rtree: the node slot index is on but its subspace was not set")
	}
	before, after := map[string]bool{}, map[string]bool{}
	for _, t := range s.touched {
		for _, k := range t.orig {
			before[string(k)] = true
		}
		for _, k := range t.cur {
			after[string(k)] = true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(before)) {
		if !after[k] {
			tx.Clear(fdb.Key(k))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if !before[k] {
			tx.Set(fdb.Key(k), []byte{})
		}
	}
	return nil
}

// serializeItemSlot serializes a leaf slot to a tuple.
// Format: (hilbertValue, (pointCoords, keySuffix), value)
func (s *rtreeStorage) serializeItemSlot(slot ItemSlot) tuple.Tuple {
	var hv any
	if s.config.StoreHilbertValues && slot.HilbertValue != nil {
		hv = slot.HilbertValue
	}

	// itemKey = tuple.Tuple{pointCoords, keySuffix}
	itemKey := tuple.Tuple{slot.Point.Coordinates, slot.KeySuffix}

	return tuple.Tuple{hv, itemKey, slot.Value}
}

// deserializeItemSlots deserializes leaf slots from the slot list tuple.
// Each element in slotList is a nested tuple: (hv, itemKey, value).
func (s *rtreeStorage) deserializeItemSlots(slotList tuple.Tuple) ([]ItemSlot, error) {
	slots := make([]ItemSlot, len(slotList))
	for i, elem := range slotList {
		slotTuple, ok := elem.(tuple.Tuple)
		if !ok {
			return nil, fmt.Errorf("rtree: slot %d is not a tuple", i)
		}
		if len(slotTuple) < 3 {
			return nil, fmt.Errorf("rtree: slot %d has %d elements, need 3", i, len(slotTuple))
		}

		// Hilbert value — may be *big.Int (large) or int64 (small, FDB decodes as int).
		if slotTuple[0] != nil {
			switch v := slotTuple[0].(type) {
			case *big.Int:
				slots[i].HilbertValue = v
			case big.Int:
				cp := new(big.Int).Set(&v)
				slots[i].HilbertValue = cp
			case int64:
				slots[i].HilbertValue = big.NewInt(v)
			}
		}

		// Item key: (pointCoords, keySuffix)
		itemKeyTuple, ok := slotTuple[1].(tuple.Tuple)
		if !ok {
			return nil, fmt.Errorf("rtree: slot %d itemKey is not a tuple, got %T", i, slotTuple[1])
		}
		if len(itemKeyTuple) < 2 {
			return nil, fmt.Errorf("rtree: slot %d itemKey has %d elements, need 2", i, len(itemKeyTuple))
		}
		if pointTuple, ok := itemKeyTuple[0].(tuple.Tuple); ok {
			slots[i].Point = Point{Coordinates: pointTuple}
		}
		if suffix, ok := itemKeyTuple[1].(tuple.Tuple); ok {
			slots[i].KeySuffix = suffix
		}

		// Value — single tuple, no extra wrapping.
		if valueTuple, ok := slotTuple[2].(tuple.Tuple); ok {
			slots[i].Value = valueTuple
		}

		// Recompute Hilbert value if not stored.
		if slots[i].HilbertValue == nil && len(slots[i].Point.Coordinates) > 0 {
			coords := make([]int64, slots[i].Point.NumDimensions())
			for d := 0; d < len(coords); d++ {
				coords[d] = slots[i].Point.Coordinate(d)
			}
			slots[i].HilbertValue = hilbertValue(coords)
		}
	}
	return slots, nil
}

// serializeChildSlot serializes an intermediate slot to tuple elements.
// Format: (smallestHV, smallestKey, largestHV, largestKey, childId, mbr)
func (s *rtreeStorage) serializeChildSlot(slot ChildSlot) tuple.Tuple {
	return tuple.Tuple{
		slot.SmallestHV,
		slot.SmallestKey,
		slot.LargestHV,
		slot.LargestKey,
		slot.ChildID,
		MBRToTuple(slot.ChildMBR),
	}
}

// deserializeChildSlots deserializes intermediate slots from the slot list tuple.
// Each element in slotList is a nested tuple with 6 elements:
// (smallestHV, smallestKey, largestHV, largestKey, childId, mbr).
func (s *rtreeStorage) deserializeChildSlots(slotList tuple.Tuple) ([]ChildSlot, error) {
	slots := make([]ChildSlot, len(slotList))
	for i, elem := range slotList {
		slotTuple, ok := elem.(tuple.Tuple)
		if !ok {
			return nil, fmt.Errorf("rtree: child slot %d is not a tuple", i)
		}
		if len(slotTuple) < 6 {
			return nil, fmt.Errorf("rtree: child slot %d has %d elements, need 6", i, len(slotTuple))
		}

		switch v := slotTuple[0].(type) {
		case *big.Int:
			slots[i].SmallestHV = v
		case int64:
			slots[i].SmallestHV = big.NewInt(v)
		default:
			slots[i].SmallestHV = big.NewInt(0)
		}
		if v, ok := slotTuple[1].(tuple.Tuple); ok {
			slots[i].SmallestKey = v
		}
		switch v := slotTuple[2].(type) {
		case *big.Int:
			slots[i].LargestHV = v
		case int64:
			slots[i].LargestHV = big.NewInt(v)
		default:
			slots[i].LargestHV = big.NewInt(0)
		}
		if v, ok := slotTuple[3].(tuple.Tuple); ok {
			slots[i].LargestKey = v
		}
		if v, ok := slotTuple[4].([]byte); ok {
			slots[i].ChildID = v
		}
		if v, ok := slotTuple[5].(tuple.Tuple); ok {
			mbr, mbrErr := MBRFromTuple(v, s.config.NumDimensions)
			if mbrErr != nil {
				return nil, fmt.Errorf("deserialize child MBR at slot %d: %w", i, mbrErr)
			}
			slots[i].ChildMBR = mbr
		}
	}
	return slots, nil
}

// clearAll removes all nodes in this R-tree's subspace.
//
// The node slot index under the same prefix goes too, whatever the option
// says, as Java's MultidimensionalIndexMaintainer.deleteWhere clears it.
func (s *rtreeStorage) clearAll(tx fdb.WritableTransaction) error {
	r, err := fdb.PrefixRange(s.subspace.Bytes())
	if err != nil {
		return fmt.Errorf("rtree: clearAll prefix range: %w", err)
	}
	tx.ClearRange(r)
	if s.hasNodeSlotIndex {
		nr, err := fdb.PrefixRange(s.nodeSlotIndex.Bytes())
		if err != nil {
			return fmt.Errorf("rtree: clearAll node slot index prefix range: %w", err)
		}
		tx.ClearRange(nr)
	}
	return nil
}
