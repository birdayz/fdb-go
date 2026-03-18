package recordlayer

import (
	"fmt"
	"math/big"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// rtreeStorage handles BY_NODE serialization of R-tree nodes in FDB.
// Each node is one FDB key-value pair: subspace.pack(nodeId) → tuple(nodeKind, slots...).
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
	t, err := tuple.Unpack(data)
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
	slots, err := s.deserializeItemSlots(t[1:])
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
	t, err := tuple.Unpack(data)
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
	slots, err := s.deserializeChildSlots(t[1:])
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
	t, err := tuple.Unpack(data)
	if err != nil {
		return nil, nil, fmt.Errorf("rtree: unpack node: %w", err)
	}
	if len(t) < 1 {
		return nil, nil, fmt.Errorf("rtree: empty node tuple")
	}
	kind, _ := asInt64(t[0])
	switch NodeKind(kind) {
	case NodeKindLeaf:
		slots, err := s.deserializeItemSlots(t[1:])
		if err != nil {
			return nil, nil, err
		}
		return &leafNode{id: nodeID, slots: slots}, nil, nil
	case NodeKindIntermediate:
		slots, err := s.deserializeChildSlots(t[1:])
		if err != nil {
			return nil, nil, err
		}
		return nil, &intermediateNode{id: nodeID, slots: slots}, nil
	default:
		return nil, nil, fmt.Errorf("rtree: unknown node kind %d", kind)
	}
}

// writeLeafNode writes a leaf node to FDB.
func (s *rtreeStorage) writeLeafNode(tx fdb.Transaction, node *leafNode) {
	key := s.subspace.Pack(tuple.Tuple{node.id})
	t := make(tuple.Tuple, 0, 1+len(node.slots)*3)
	t = append(t, int64(NodeKindLeaf))
	for _, slot := range node.slots {
		t = append(t, s.serializeItemSlot(slot)...)
	}
	tx.Set(fdb.Key(key), t.Pack())
}

// writeIntermediateNode writes an intermediate node to FDB.
func (s *rtreeStorage) writeIntermediateNode(tx fdb.Transaction, node *intermediateNode) {
	key := s.subspace.Pack(tuple.Tuple{node.id})
	t := make(tuple.Tuple, 0, 1+len(node.slots)*6)
	t = append(t, int64(NodeKindIntermediate))
	for _, slot := range node.slots {
		t = append(t, s.serializeChildSlot(slot)...)
	}
	tx.Set(fdb.Key(key), t.Pack())
}

// deleteNode removes a node from FDB.
func (s *rtreeStorage) deleteNode(tx fdb.Transaction, nodeID []byte) {
	key := s.subspace.Pack(tuple.Tuple{nodeID})
	tx.Clear(fdb.Key(key))
}

// serializeItemSlot serializes a leaf slot to tuple elements.
// Format: (hilbertValue, (pointCoords, keySuffix), (value))
func (s *rtreeStorage) serializeItemSlot(slot ItemSlot) tuple.Tuple {
	var hv any
	if s.config.StoreHilbertValues && slot.HilbertValue != nil {
		hv = slot.HilbertValue
	}

	// itemKey = tuple.Tuple{pointCoords, keySuffix}
	itemKey := tuple.Tuple{slot.Point.Coordinates, slot.KeySuffix}

	return tuple.Tuple{hv, itemKey, tuple.Tuple{slot.Value}}
}

// deserializeItemSlots deserializes leaf slots from remaining tuple elements.
// Each slot is 3 elements: (hv, itemKey, value).
func (s *rtreeStorage) deserializeItemSlots(t tuple.Tuple) ([]ItemSlot, error) {
	const elementsPerSlot = 3
	if len(t)%elementsPerSlot != 0 {
		return nil, fmt.Errorf("rtree: leaf tuple length %d not divisible by %d", len(t), elementsPerSlot)
	}

	n := len(t) / elementsPerSlot
	slots := make([]ItemSlot, n)
	for i := 0; i < n; i++ {
		base := i * elementsPerSlot

		// Hilbert value.
		if t[base] != nil {
			switch v := t[base].(type) {
			case *big.Int:
				slots[i].HilbertValue = v
			case big.Int:
				cp := new(big.Int).Set(&v)
				slots[i].HilbertValue = cp
			}
		}

		// Item key: (pointCoords, keySuffix)
		if itemKeyTuple, ok := t[base+1].(tuple.Tuple); ok && len(itemKeyTuple) >= 2 {
			if pointTuple, ok := itemKeyTuple[0].(tuple.Tuple); ok {
				slots[i].Point = Point{Coordinates: pointTuple}
			}
			if suffix, ok := itemKeyTuple[1].(tuple.Tuple); ok {
				slots[i].KeySuffix = suffix
			}
		}

		// Value.
		if valueTuple, ok := t[base+2].(tuple.Tuple); ok && len(valueTuple) > 0 {
			if innerValue, ok := valueTuple[0].(tuple.Tuple); ok {
				slots[i].Value = innerValue
			}
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

// deserializeChildSlots deserializes intermediate slots from remaining tuple elements.
// Each slot is 6 elements.
func (s *rtreeStorage) deserializeChildSlots(t tuple.Tuple) ([]ChildSlot, error) {
	const elementsPerSlot = 6
	if len(t)%elementsPerSlot != 0 {
		return nil, fmt.Errorf("rtree: intermediate tuple length %d not divisible by %d", len(t), elementsPerSlot)
	}

	n := len(t) / elementsPerSlot
	slots := make([]ChildSlot, n)
	for i := 0; i < n; i++ {
		base := i * elementsPerSlot

		if v, ok := t[base].(*big.Int); ok {
			slots[i].SmallestHV = v
		} else {
			slots[i].SmallestHV = big.NewInt(0)
		}
		if v, ok := t[base+1].(tuple.Tuple); ok {
			slots[i].SmallestKey = v
		}
		if v, ok := t[base+2].(*big.Int); ok {
			slots[i].LargestHV = v
		} else {
			slots[i].LargestHV = big.NewInt(0)
		}
		if v, ok := t[base+3].(tuple.Tuple); ok {
			slots[i].LargestKey = v
		}
		if v, ok := t[base+4].([]byte); ok {
			slots[i].ChildID = v
		}
		if v, ok := t[base+5].(tuple.Tuple); ok {
			slots[i].ChildMBR = MBRFromTuple(v, s.config.NumDimensions)
		}
	}
	return slots, nil
}

// clearAll removes all nodes in this R-tree's subspace.
func (s *rtreeStorage) clearAll(tx fdb.Transaction) {
	r, err := fdb.PrefixRange(s.subspace.Bytes())
	if err != nil {
		return
	}
	tx.ClearRange(r)
}
