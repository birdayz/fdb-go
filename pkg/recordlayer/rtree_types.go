package recordlayer

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// NodeKind identifies a node as leaf or intermediate.
type NodeKind byte

const (
	NodeKindLeaf         NodeKind = 0x00
	NodeKindIntermediate NodeKind = 0x01
)

// rootNodeID is all zeros — the well-known root node ID.
// Matches Java's NodeHelpers.rootNodeId().
var rootNodeID = make([]byte, 16)

// newRandomNodeID generates a random 16-byte UUID for a new node.
// Matches Java's NodeHelpers.newRandomNodeId().
func newRandomNodeID() ([]byte, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("rtree: generate node ID: %w", err)
	}
	return id, nil
}

// Point represents an N-dimensional point with int64 coordinates.
// Null coordinates are represented as nil in the tuple.
type Point struct {
	Coordinates tuple.Tuple
}

// NumDimensions returns the number of dimensions.
func (p Point) NumDimensions() int {
	return len(p.Coordinates)
}

// Coordinate returns the coordinate at the given dimension index, or MinInt64 if nil.
func (p Point) Coordinate(dim int) int64 {
	if dim >= len(p.Coordinates) || p.Coordinates[dim] == nil {
		return minInt64
	}
	v, ok := asInt64(p.Coordinates[dim])
	if !ok {
		return minInt64
	}
	return v
}

const minInt64 = -1 << 63

// MBR is a minimum bounding rectangle for N dimensions.
// Stored as [low0, low1, ..., lowN-1, high0, high1, ..., highN-1].
type MBR struct {
	Low  []int64
	High []int64
}

// NumDimensions returns the number of dimensions.
func (m MBR) NumDimensions() int {
	return len(m.Low)
}

// ContainsPoint returns true if the MBR contains the given point.
func (m MBR) ContainsPoint(p Point) bool {
	for d := 0; d < m.NumDimensions(); d++ {
		c := p.Coordinate(d)
		if c < m.Low[d] || c > m.High[d] {
			return false
		}
	}
	return true
}

// Overlaps returns true if this MBR overlaps with another.
// Returns false if the MBRs have different numbers of dimensions.
func (m MBR) Overlaps(other MBR) bool {
	if m.NumDimensions() != other.NumDimensions() {
		return false
	}
	for d := 0; d < m.NumDimensions(); d++ {
		if m.Low[d] > other.High[d] || m.High[d] < other.Low[d] {
			return false
		}
	}
	return true
}

// Union returns the smallest MBR containing both this and other.
// If the MBRs have different numbers of dimensions, returns self unchanged.
func (m MBR) Union(other MBR) MBR {
	n := m.NumDimensions()
	if other.NumDimensions() != n {
		// Should never happen in a well-formed tree.
		// Fall back to returning self to avoid panic.
		return m
	}
	result := MBR{Low: make([]int64, n), High: make([]int64, n)}
	for d := 0; d < n; d++ {
		result.Low[d] = m.Low[d]
		if other.Low[d] < result.Low[d] {
			result.Low[d] = other.Low[d]
		}
		result.High[d] = m.High[d]
		if other.High[d] > result.High[d] {
			result.High[d] = other.High[d]
		}
	}
	return result
}

// MBRFromPoint creates a degenerate MBR containing only a single point.
func MBRFromPoint(p Point) MBR {
	n := p.NumDimensions()
	mbr := MBR{Low: make([]int64, n), High: make([]int64, n)}
	for d := 0; d < n; d++ {
		c := p.Coordinate(d)
		mbr.Low[d] = c
		mbr.High[d] = c
	}
	return mbr
}

// MBRToTuple serializes an MBR to a flat tuple [low0, ..., lowN-1, high0, ..., highN-1].
// Matches Java's ChildSlot MBR serialization.
func MBRToTuple(m MBR) tuple.Tuple {
	n := m.NumDimensions()
	t := make(tuple.Tuple, 2*n)
	for d := 0; d < n; d++ {
		t[d] = m.Low[d]
	}
	for d := 0; d < n; d++ {
		t[n+d] = m.High[d]
	}
	return t
}

// MBRFromTuple deserializes an MBR from a flat tuple.
func MBRFromTuple(t tuple.Tuple, numDimensions int) (MBR, error) {
	m := MBR{Low: make([]int64, numDimensions), High: make([]int64, numDimensions)}
	for d := 0; d < numDimensions; d++ {
		if d < len(t) {
			v, ok := asInt64(t[d])
			if !ok {
				return MBR{}, fmt.Errorf("MBR low[%d]: cannot convert %T to int64", d, t[d])
			}
			m.Low[d] = v
		}
		if numDimensions+d < len(t) {
			v, ok := asInt64(t[numDimensions+d])
			if !ok {
				return MBR{}, fmt.Errorf("MBR high[%d]: cannot convert %T to int64", d, t[numDimensions+d])
			}
			m.High[d] = v
		}
	}
	return m, nil
}

// ItemSlot is a leaf node slot containing a data item.
type ItemSlot struct {
	HilbertValue *big.Int
	Point        Point
	KeySuffix    tuple.Tuple
	Value        tuple.Tuple
}

// ItemKey returns the combined item key (point coordinates + suffix).
func (s ItemSlot) ItemKey() tuple.Tuple {
	result := make(tuple.Tuple, 0, 2)
	result = append(result, tuple.Tuple(s.Point.Coordinates))
	result = append(result, s.KeySuffix)
	return result
}

// GetMBR returns the MBR for this item (a degenerate point MBR).
func (s ItemSlot) GetMBR() MBR {
	return MBRFromPoint(s.Point)
}

// ChildSlot is an intermediate node slot referencing a child node.
type ChildSlot struct {
	SmallestHV  *big.Int
	SmallestKey tuple.Tuple
	LargestHV   *big.Int
	LargestKey  tuple.Tuple
	ChildID     []byte
	ChildMBR    MBR
}

// GetMBR returns the MBR of the child subtree.
func (s ChildSlot) GetMBR() MBR {
	return s.ChildMBR
}

// childSlotEqual compares two ChildSlots for equality.
// Used by propagateMBRUp to skip writes when nothing changed (matching Java's adjustSlotInParent).
func childSlotEqual(a, b ChildSlot) bool {
	if a.SmallestHV == nil && b.SmallestHV != nil || a.SmallestHV != nil && b.SmallestHV == nil {
		return false
	}
	if a.SmallestHV != nil && a.SmallestHV.Cmp(b.SmallestHV) != 0 {
		return false
	}
	if a.LargestHV == nil && b.LargestHV != nil || a.LargestHV != nil && b.LargestHV == nil {
		return false
	}
	if a.LargestHV != nil && a.LargestHV.Cmp(b.LargestHV) != 0 {
		return false
	}
	if tupleCompare(a.SmallestKey, b.SmallestKey) != 0 || tupleCompare(a.LargestKey, b.LargestKey) != 0 {
		return false
	}
	if a.ChildMBR.NumDimensions() != b.ChildMBR.NumDimensions() {
		return false
	}
	for d := 0; d < a.ChildMBR.NumDimensions(); d++ {
		if a.ChildMBR.Low[d] != b.ChildMBR.Low[d] || a.ChildMBR.High[d] != b.ChildMBR.High[d] {
			return false
		}
	}
	return true
}

// RTreeConfig configures the R-tree behavior.
// Matches Java's RTree.Config.
type RTreeConfig struct {
	MinM               int  // Min slots per non-root node (default 16)
	MaxM               int  // Max slots per node (default 32)
	SplitS             int  // Siblings involved in split/fuse (default 2)
	StoreHilbertValues bool // Store HV in leaf slots (default true)
	NumDimensions      int  // Number of spatial dimensions
}

// DefaultRTreeConfig returns the default R-tree configuration.
func DefaultRTreeConfig(numDimensions int) RTreeConfig {
	return RTreeConfig{
		MinM:               16,
		MaxM:               32,
		SplitS:             2,
		StoreHilbertValues: true,
		NumDimensions:      numDimensions,
	}
}

// ValidateRTreeConfig validates the R-tree configuration parameters.
// Checks: MinM >= 1, MaxM >= 2, SplitS >= 1, NumDimensions >= 1,
// and S * MaxM >= (S+1) * MinM (split/fuse ratio constraint).
func ValidateRTreeConfig(config RTreeConfig) error {
	if config.NumDimensions < 1 {
		return fmt.Errorf("rtree: NumDimensions must be >= 1, got %d", config.NumDimensions)
	}
	if config.MinM < 1 {
		return fmt.Errorf("rtree: MinM must be >= 1, got %d", config.MinM)
	}
	if config.MaxM < 2 || config.MaxM > 1000 {
		return fmt.Errorf("rtree: MaxM must be in [2, 1000], got %d", config.MaxM)
	}
	if config.SplitS < 1 {
		return fmt.Errorf("rtree: SplitS must be >= 1, got %d", config.SplitS)
	}
	// S * MaxM >= (S+1) * MinM ensures that after a split among S+1 siblings,
	// each sibling still has at least MinM items.
	if config.SplitS*config.MaxM < (config.SplitS+1)*config.MinM {
		return fmt.Errorf("rtree: config violates split constraint: S*MaxM (%d) < (S+1)*MinM (%d)",
			config.SplitS*config.MaxM, (config.SplitS+1)*config.MinM)
	}
	return nil
}

// leafNode is a leaf-level node containing item slots.
type leafNode struct {
	id    []byte
	slots []ItemSlot
}

// intermediateNode is an internal node containing child references.
type intermediateNode struct {
	id    []byte
	slots []ChildSlot
}

// compareHilbertValueAndKey compares (hv1, key1) with (hv2, key2).
// Returns -1, 0, or 1. Matches Java's NodeSlot.compareHilbertValueAndKey().
func compareHilbertValueAndKey(hv1 *big.Int, key1 tuple.Tuple, hv2 *big.Int, key2 tuple.Tuple) int {
	if hv1 == nil && hv2 == nil {
		return tupleCompare(key1, key2)
	}
	if hv1 == nil {
		return -1
	}
	if hv2 == nil {
		return 1
	}
	// Compare Hilbert values first.
	cmp := hv1.Cmp(hv2)
	if cmp != 0 {
		return cmp
	}
	// Then compare keys by their packed bytes (FDB tuple order).
	return tupleCompare(key1, key2)
}
