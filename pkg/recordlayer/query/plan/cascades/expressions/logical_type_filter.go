package expressions

import (
	"hash/fnv"
	"sort"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalTypeFilterExpression narrows its inner stream to a subset of
// record types — used when a query targets `Order` records but the
// inner is an unfiltered scan over the full union descriptor.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalTypeFilterExpression`.
// Java's full implementation tracks a `recordTypePredicate`, a
// `resultType`, and integrates with the type-narrowing PullUp /
// MaxMatchMap machinery. The seed keeps just the record-type set and
// the inner Quantifier; the result Type narrowing lands when its
// consumer (the index-pushdown rule batch) is ported.
type LogicalTypeFilterExpression struct {
	recordTypes []string // sorted; canonical form for equality + hash
	inner       Quantifier
}

// NewLogicalTypeFilterExpression builds a type-filter narrowing the
// inner stream to the given record-type names. The set is normalised
// (deduped + sorted) for canonical equality.
func NewLogicalTypeFilterExpression(recordTypes []string, inner Quantifier) *LogicalTypeFilterExpression {
	deduped := dedupSortedStrings(recordTypes)
	return &LogicalTypeFilterExpression{recordTypes: deduped, inner: inner}
}

func dedupSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	tmp := make([]string, len(in))
	copy(tmp, in)
	sort.Strings(tmp)
	out := tmp[:0]
	for i, s := range tmp {
		if i == 0 || s != tmp[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// GetRecordTypes returns the record-type-name set. Read-only.
func (e *LogicalTypeFilterExpression) GetRecordTypes() []string { return e.recordTypes }

// GetInner returns the inner Quantifier.
func (e *LogicalTypeFilterExpression) GetInner() Quantifier { return e.inner }

// GetResultValue — the seed passes the inner's flowed object through.
// When Type narrowing lands (alongside the type-narrow rule batch),
// this returns a QuantifiedObjectValue whose Type is the narrow
// union-of-recordTypes.
func (e *LogicalTypeFilterExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalTypeFilterExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false — single child.
func (e *LogicalTypeFilterExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalTypeFilterExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren — Java returns the empty set; record
// types are static metadata.
func (e *LogicalTypeFilterExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares two type-filters by their canonical
// record-type-name slice.
func (e *LogicalTypeFilterExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalTypeFilterExpression)
	if !ok {
		return false
	}
	_ = aliases
	if len(e.recordTypes) != len(o.recordTypes) {
		return false
	}
	for i := range e.recordTypes {
		if e.recordTypes[i] != o.recordTypes[i] {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren hashes the canonical record-type list.
func (e *LogicalTypeFilterExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	for _, name := range e.recordTypes {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *LogicalTypeFilterExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &LogicalTypeFilterExpression{
		inner:       quantifiers[0],
		recordTypes: e.recordTypes,
	}
}

var _ RelationalExpression = (*LogicalTypeFilterExpression)(nil)
