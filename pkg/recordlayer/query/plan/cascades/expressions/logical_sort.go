package expressions

import (
	"encoding/binary"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// SortKey is one component of a sort specification: a Value
// (typically a FieldValue resolving to a column of the inner) and a
// boolean Reverse flag — false for ASC, true for DESC.
type SortKey struct {
	Value      values.Value
	Reverse    bool
	NullsFirst *bool // nil = use default (ASC→true, DESC→false)
}

// LogicalSortExpression represents an unimplemented sort over the inner
// Quantifier's rows. The sort keys list is the requested ordering — the
// planner may either materialise it (RecordQuerySortPlan) or eliminate
// it if the chosen scan plan already satisfies it.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalSortExpression`.
// Java models the ordering via a rich `RequestedOrdering` type
// (sort-keys + distinctness + correlation set). The seed uses the
// equivalent of Java's now-deprecated 2-arg constructor: a list of
// (Value, reverse) pairs. Distinctness lands when the planner needs it
// (the relevant rules port in B5 Batch B+).
type LogicalSortExpression struct {
	sortKeys []SortKey
	inner    Quantifier
}

// NewLogicalSortExpression constructs a sort. sortKeys is copied.
func NewLogicalSortExpression(sortKeys []SortKey, inner Quantifier) *LogicalSortExpression {
	copied := make([]SortKey, len(sortKeys))
	copy(copied, sortKeys)
	return &LogicalSortExpression{sortKeys: copied, inner: inner}
}

// UnsortedLogicalSortExpression is the no-op sort — preserves the
// inner's order. Used as a placeholder before any concrete ordering is
// requested. Mirrors Java's `LogicalSortExpression.unsorted(inner)`.
func UnsortedLogicalSortExpression(inner Quantifier) *LogicalSortExpression {
	return NewLogicalSortExpression(nil, inner)
}

// GetSortKeys returns the sort key list. Read-only.
func (e *LogicalSortExpression) GetSortKeys() []SortKey { return e.sortKeys }

// GetInner returns the inner Quantifier.
func (e *LogicalSortExpression) GetInner() Quantifier { return e.inner }

// IsUnsorted reports whether this is a no-op sort.
func (e *LogicalSortExpression) IsUnsorted() bool { return len(e.sortKeys) == 0 }

// GetResultValue is the inner's flowed object value (sort doesn't
// change the row shape, only the order).
func (e *LogicalSortExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalSortExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false for a sort — single child.
func (e *LogicalSortExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalSortExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren — Java returns the empty set, but
// sort keys CAN reference correlated values. We surface them so the
// planner can detect the case where a sort depends on an outer
// quantifier (e.g. `ORDER BY outer.col` inside a correlated subquery).
// This is a deliberate Go-side strengthening; if it ever causes a
// rule-level mismatch, switch back to the Java-empty-set behaviour
// and surface correlations via a separate accessor.
func (e *LogicalSortExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, k := range e.sortKeys {
		for cid := range values.GetCorrelatedToOfValue(k.Value) {
			out[cid] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares two sorts by their sort key lists,
// position-by-position, with Reverse flags matching exactly. Sort key
// Values are compared by Explain text (same bridge as projection).
func (e *LogicalSortExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalSortExpression)
	if !ok {
		return false
	}
	if len(e.sortKeys) != len(o.sortKeys) {
		return false
	}
	// Alias-aware sort-key Value equality (RFC-040 040.2). Inert under the
	// memo's empty-alias path until PR-A threads real alias maps.
	vm := aliases.ToValuesAliasMap()
	for i := range e.sortKeys {
		if e.sortKeys[i].Reverse != o.sortKeys[i].Reverse {
			return false
		}
		// NullsFirst is part of the ordering identity (was previously
		// ignored — pinned here, RFC-040 review).
		if !nullsFirstEqual(e.sortKeys[i].NullsFirst, o.sortKeys[i].NullsFirst) {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(e.sortKeys[i].Value, o.sortKeys[i].Value, vm) {
			return false
		}
	}
	return true
}

// nullsFirstEqual compares two optional NullsFirst flags (nil = default).
func nullsFirstEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// nullsFirstTag returns a stable byte for the optional NullsFirst flag.
func nullsFirstTag(p *bool) byte {
	switch {
	case p == nil:
		return 0
	case *p:
		return 2
	default:
		return 1
	}
}

// HashCodeWithoutChildren hashes the sort key list.
func (e *LogicalSortExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for _, k := range e.sortKeys {
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(k.Value))
		h.Write(buf[:])
		if k.Reverse {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
		h.Write([]byte{nullsFirstTag(k.NullsFirst)})
		h.Write([]byte{0xff})
	}
	return h.Sum64()
}

func (e *LogicalSortExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &LogicalSortExpression{
		inner:    quantifiers[0],
		sortKeys: e.sortKeys,
	}
}

var _ RelationalExpression = (*LogicalSortExpression)(nil)
