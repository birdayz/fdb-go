package cascades

import (
	"fmt"
	"sort"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// ---------------------------------------------------------------------------
// IntersectionResult
// ---------------------------------------------------------------------------

// IntersectionResult captures the result of attempting to intersect
// multiple data accesses. When viable, it carries a common ordering,
// a compensation, and the participating expressions.
//
// Ports Java's AbstractDataAccessRule.IntersectionResult.
type IntersectionResult struct {
	commonOrdering *RichOrdering
	compensation   Compensation
	expressions    []expressions.RelationalExpression
}

// NewIntersectionResult creates an IntersectionResult. When
// commonOrdering is nil the expressions slice must be empty (mirrors
// Java's Verify.verify precondition).
func NewIntersectionResult(
	ordering *RichOrdering,
	comp Compensation,
	exprs []expressions.RelationalExpression,
) *IntersectionResult {
	if ordering == nil && len(exprs) > 0 {
		panic("IntersectionResult: nil ordering requires empty expressions")
	}
	copied := make([]expressions.RelationalExpression, len(exprs))
	copy(copied, exprs)
	return &IntersectionResult{
		commonOrdering: ordering,
		compensation:   comp,
		expressions:    copied,
	}
}

// NoViableIntersection returns an IntersectionResult that indicates no
// viable intersection was found. Mirrors Java's
// IntersectionResult.noViableIntersection().
func NoViableIntersection() *IntersectionResult {
	return &IntersectionResult{
		commonOrdering: nil,
		compensation:   NoCompensation,
		expressions:    nil,
	}
}

// IsViable reports whether this result represents a viable
// intersection (i.e. a common ordering was found). Mirrors Java's
// IntersectionResult.hasViableIntersection().
func (r *IntersectionResult) IsViable() bool {
	return r.commonOrdering != nil
}

// GetCommonOrdering returns the common intersection ordering. Panics
// if the intersection is not viable.
func (r *IntersectionResult) GetCommonOrdering() *RichOrdering {
	if r.commonOrdering == nil {
		panic("IntersectionResult: no common ordering (not viable)")
	}
	return r.commonOrdering
}

// GetCompensation returns the compensation for this intersection.
func (r *IntersectionResult) GetCompensation() Compensation {
	return r.compensation
}

// GetExpressions returns the participating expressions.
func (r *IntersectionResult) GetExpressions() []expressions.RelationalExpression {
	return r.expressions
}

// String returns a human-readable representation. Mirrors Java's
// IntersectionResult.toString().
func (r *IntersectionResult) String() string {
	orderingStr := "no common ordering"
	if r.commonOrdering != nil {
		orderingStr = fmt.Sprintf("%v", r.commonOrdering)
	}
	return fmt.Sprintf("[ordering=%s, %v]", orderingStr, r.compensation)
}

// ---------------------------------------------------------------------------
// IntersectionInfo
// ---------------------------------------------------------------------------

// IntersectionInfo tracks the state of a single data access within the
// intersection sieve. It carries the access's ordering, compensation,
// participating expressions, and an estimated max cardinality.
//
// Ports Java's AbstractDataAccessRule.IntersectionInfo.
type IntersectionInfo struct {
	ordering       *RichOrdering
	compensation   Compensation
	expressions    []expressions.RelationalExpression
	maxCardinality int64 // -1 = unknown
}

// CardinalityUnknown is the sentinel value for unknown cardinality,
// mirroring Java's Cardinality.unknownCardinality().
const CardinalityUnknown int64 = -1

// NewIntersectionInfo creates an IntersectionInfo with all fields.
func NewIntersectionInfo(
	ordering *RichOrdering,
	comp Compensation,
	exprs []expressions.RelationalExpression,
	maxCard int64,
) *IntersectionInfo {
	copied := make([]expressions.RelationalExpression, len(exprs))
	copy(copied, exprs)
	return &IntersectionInfo{
		ordering:       ordering,
		compensation:   comp,
		expressions:    copied,
		maxCardinality: maxCard,
	}
}

// IntersectionInfoOfSingleAccess creates an IntersectionInfo for a
// single data access (one expression). Mirrors Java's
// IntersectionInfo.ofSingleAccess().
func IntersectionInfoOfSingleAccess(
	ordering *RichOrdering,
	comp Compensation,
	expr expressions.RelationalExpression,
	maxCard int64,
) *IntersectionInfo {
	return &IntersectionInfo{
		ordering:       ordering,
		compensation:   comp,
		expressions:    []expressions.RelationalExpression{expr},
		maxCardinality: maxCard,
	}
}

// IntersectionInfoOfImpossibleAccess creates an IntersectionInfo for
// an impossible access (no expressions, unknown cardinality). Mirrors
// Java's IntersectionInfo.ofImpossibleAccess().
func IntersectionInfoOfImpossibleAccess(
	ordering *RichOrdering,
	comp Compensation,
) *IntersectionInfo {
	return &IntersectionInfo{
		ordering:       ordering,
		compensation:   comp,
		expressions:    nil,
		maxCardinality: CardinalityUnknown,
	}
}

// IntersectionInfoOfIntersection creates an IntersectionInfo for a
// computed intersection (multiple expressions, unknown cardinality).
// Mirrors Java's IntersectionInfo.ofIntersection().
func IntersectionInfoOfIntersection(
	ordering *RichOrdering,
	comp Compensation,
	exprs []expressions.RelationalExpression,
) *IntersectionInfo {
	copied := make([]expressions.RelationalExpression, len(exprs))
	copy(copied, exprs)
	return &IntersectionInfo{
		ordering:       ordering,
		compensation:   comp,
		expressions:    copied,
		maxCardinality: CardinalityUnknown,
	}
}

// GetOrdering returns the ordering for this access.
func (i *IntersectionInfo) GetOrdering() *RichOrdering {
	return i.ordering
}

// GetCompensation returns the compensation for this access.
func (i *IntersectionInfo) GetCompensation() Compensation {
	return i.compensation
}

// GetExpressions returns the participating expressions.
func (i *IntersectionInfo) GetExpressions() []expressions.RelationalExpression {
	return i.expressions
}

// GetMaxCardinality returns the estimated max cardinality.
// Returns CardinalityUnknown (-1) if unknown.
func (i *IntersectionInfo) GetMaxCardinality() int64 {
	return i.maxCardinality
}

// EvictExpressions clears the expressions list. Mirrors Java's
// IntersectionInfo.evictExpressions().
func (i *IntersectionInfo) EvictExpressions() {
	i.expressions = nil
}

// ---------------------------------------------------------------------------
// Vectored[T]
// ---------------------------------------------------------------------------

// Vectored pairs a value with its position index. Used by the
// bit-sieve intersection logic to track which element came from which
// position in the original list.
//
// Ports Java's AbstractDataAccessRule.Vectored<T>.
type Vectored[T any] struct {
	Value    T
	Position int
}

// NewVectored creates a Vectored with the given value and position.
func NewVectored[T any](value T, position int) Vectored[T] {
	return Vectored[T]{Value: value, Position: position}
}

// String returns a human-readable representation. Mirrors Java's
// Vectored.toString().
func (v Vectored[T]) String() string {
	return fmt.Sprintf("[%v:%d]", v.Value, v.Position)
}

// ---------------------------------------------------------------------------
// BitSet
// ---------------------------------------------------------------------------

// BitSet is a simple bit set backed by a map, providing the subset of
// java.util.BitSet operations needed by the intersection sieve logic.
type BitSet struct {
	bits map[int]bool
}

// NewBitSet creates an empty BitSet.
func NewBitSet() *BitSet {
	return &BitSet{bits: make(map[int]bool)}
}

// Set sets the bit at position pos.
func (b *BitSet) Set(pos int) {
	b.bits[pos] = true
}

// Get returns whether the bit at position pos is set.
func (b *BitSet) Get(pos int) bool {
	return b.bits[pos]
}

// Or returns a new BitSet that is the union of b and other.
func (b *BitSet) Or(other *BitSet) *BitSet {
	result := NewBitSet()
	for k := range b.bits {
		result.bits[k] = true
	}
	for k := range other.bits {
		result.bits[k] = true
	}
	return result
}

// And returns a new BitSet that is the intersection of b and other.
func (b *BitSet) And(other *BitSet) *BitSet {
	result := NewBitSet()
	for k := range b.bits {
		if other.bits[k] {
			result.bits[k] = true
		}
	}
	return result
}

// IsSubsetOf reports whether every bit in b is also set in other.
func (b *BitSet) IsSubsetOf(other *BitSet) bool {
	for k := range b.bits {
		if !other.bits[k] {
			return false
		}
	}
	return true
}

// Cardinality returns the number of set bits.
func (b *BitSet) Cardinality() int {
	return len(b.bits)
}

// Equal reports whether b and other have exactly the same set bits.
func (b *BitSet) Equal(other *BitSet) bool {
	if len(b.bits) != len(other.bits) {
		return false
	}
	for k := range b.bits {
		if !other.bits[k] {
			return false
		}
	}
	return true
}

// String returns a human-readable representation of the set bits in
// ascending order, e.g. "{0, 2, 5}".
func (b *BitSet) String() string {
	if len(b.bits) == 0 {
		return "{}"
	}
	positions := make([]int, 0, len(b.bits))
	for k := range b.bits {
		positions = append(positions, k)
	}
	sort.Ints(positions)
	parts := make([]string, len(positions))
	for i, p := range positions {
		parts[i] = fmt.Sprintf("%d", p)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// ---------------------------------------------------------------------------
// ScanDirection
// ---------------------------------------------------------------------------

// ScanDirection indicates whether an access scans forward, reverse, or
// both. Ports Java's AbstractDataAccessRule.ScanDirection.
type ScanDirection int

const (
	ScanDirectionForward ScanDirection = iota
	ScanDirectionReverse
	ScanDirectionBoth
)
