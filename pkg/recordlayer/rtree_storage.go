package recordlayer

import (
	"fmt"
	"math/big"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// rtreeStorage handles BY_NODE serialization of R-tree nodes in FDB.
// Each node is one FDB key-value pair: subspace.pack(nodeId) → tuple(nodeKind, slotList).
// slotList is a nested tuple containing each slot as a sub-tuple.
// Matches Java's ByNodeStorageAdapter.
type rtreeStorage struct {
	subspace subspace.Subspace
	config   RTreeConfig
}

func newRTreeStorage(ss subspace.Subspace, config RTreeConfig) *rtreeStorage {
	return &rtreeStorage{subspace: ss, config: config}
}

// fetchLeafNode loads a leaf node from FDB. Returns nil if not found.
func (s *rtreeStorage) fetchLeafNode(tx fdb.ReadTransaction, nodeID []byte) (*leafNode, error) {
	key := s.subspace.Pack(tuple.Tuple{nodeID})
	data, err := tx.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	t, err := fastUnpack(data)
	if err != nil {
		return nil, fmt.Errorf("rtree: unpack leaf node: %w", err)
	}
	if len(t) < 1 {
		return nil, fmt.Errorf("rtree: empty node tuple")
	}
	kind, _ := asInt64(t[0])
	if NodeKind(kind) != NodeKindLeaf {
		return nil, fmt.Errorf("rtree: expected leaf node, got kind %d", kind)
	}
	if len(t) < 2 {
		return nil, fmt.Errorf("rtree: leaf node tuple missing slot list")
	}
	slotList, ok := t[1].(tuple.Tuple)
	if !ok {
		return nil, fmt.Errorf("rtree: leaf node slot list is not a tuple")
	}
	slots, err := s.deserializeItemSlots(slotList)
	if err != nil {
		return nil, err
	}
	return &leafNode{id: nodeID, slots: slots}, nil
}

// fetchIntermediateNode loads an intermediate node from FDB. Returns nil if not found.
func (s *rtreeStorage) fetchIntermediateNode(tx fdb.ReadTransaction, nodeID []byte) (*intermediateNode, error) {
	key := s.subspace.Pack(tuple.Tuple{nodeID})
	data, err := tx.Get(fdb.Key(key)).Get()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	t, err := fastUnpack(data)
	if err != nil {
		return nil, fmt.Errorf("rtree: unpack intermediate node: %w", err)
	}
	if len(t) < 1 {
		return nil, fmt.Errorf("rtree: empty node tuple")
	}
	kind, _ := asInt64(t[0])
	if NodeKind(kind) != NodeKindIntermediate {
		return nil, fmt.Errorf("rtree: expected intermediate node, got kind %d", kind)
	}
	if len(t) < 2 {
		return nil, fmt.Errorf("rtree: intermediate node tuple missing slot list")
	}
	slotList, ok := t[1].(tuple.Tuple)
	if !ok {
		return nil, fmt.Errorf("rtree: intermediate node slot list is not a tuple")
	}
	slots, err := s.deserializeChildSlots(slotList)
	if err != nil {
		return nil, err
	}
	return &intermediateNode{id: nodeID, slots: slots}, nil
}

// fetchNode loads any node. Returns (leaf, nil, err) or (nil, intermediate, err).
func (s *rtreeStorage) fetchNode(tx fdb.ReadTransaction, nodeID []byte) (*leafNode, *intermediateNode, error) {
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
	switch NodeKind(kind) {
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
	key := s.subspace.Pack(tuple.Tuple{node.id})
	slotList := make(tuple.Tuple, 0, len(node.slots))
	for _, slot := range node.slots {
		slotList = append(slotList, s.serializeItemSlot(slot))
	}
	t := tuple.Tuple{int64(NodeKindLeaf), slotList}
	tx.Set(fdb.Key(key), t.Pack())
}

// writeIntermediateNode writes an intermediate node to FDB. If the node has no
// slots, it is deleted instead (empty nodes should not exist in the tree).
func (s *rtreeStorage) writeIntermediateNode(tx fdb.WritableTransaction, node *intermediateNode) {
	if len(node.slots) == 0 {
		s.deleteNode(tx, node.id)
		return
	}
	key := s.subspace.Pack(tuple.Tuple{node.id})
	slotList := make(tuple.Tuple, 0, len(node.slots))
	for _, slot := range node.slots {
		slotList = append(slotList, s.serializeChildSlot(slot))
	}
	t := tuple.Tuple{int64(NodeKindIntermediate), slotList}
	tx.Set(fdb.Key(key), t.Pack())
}

// deleteNode removes a node from FDB.
func (s *rtreeStorage) deleteNode(tx fdb.WritableTransaction, nodeID []byte) {
	key := s.subspace.Pack(tuple.Tuple{nodeID})
	tx.Clear(fdb.Key(key))
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
func (s *rtreeStorage) clearAll(tx fdb.WritableTransaction) error {
	r, err := fdb.PrefixRange(s.subspace.Bytes())
	if err != nil {
		return fmt.Errorf("rtree: clearAll prefix range: %w", err)
	}
	tx.ClearRange(r)
	return nil
}
