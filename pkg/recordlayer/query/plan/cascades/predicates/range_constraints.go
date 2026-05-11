package predicates

import (
	"sync"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// RangeConstraints represents a conjunction of a compile-time evaluable
// range and a set of deferred (non-compile-time) ranges. Used during
// index matching to represent constraints on a single indexed column.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.predicates.RangeConstraints`.
//
// The compile-time range is represented as a list of Comparisons that
// can be evaluated against literal values. Deferred ranges are
// Comparisons that reference correlation variables and can only be
// evaluated at runtime (but can still form part of an index scan prefix).
//
// RangeConstraints can be converted to a ComparisonRange via
// AsComparisonRange() for backward compatibility with existing matching
// infrastructure.
type RangeConstraints struct {
	compilableComparisons []Comparison
	deferredRanges        []Comparison

	comparisonsOnce sync.Once
	comparisons     []Comparison

	correlationsOnce sync.Once
	correlations     map[values.CorrelationIdentifier]struct{}
}

// NewRangeConstraints constructs a RangeConstraints from compile-time
// and deferred comparisons.
func NewRangeConstraints(compilable []Comparison, deferred []Comparison) *RangeConstraints {
	cc := make([]Comparison, len(compilable))
	copy(cc, compilable)
	dd := make([]Comparison, len(deferred))
	copy(dd, deferred)
	return &RangeConstraints{
		compilableComparisons: cc,
		deferredRanges:        dd,
	}
}

// EmptyRangeConstraints returns a RangeConstraints with no comparisons
// (matches everything).
func EmptyRangeConstraints() *RangeConstraints {
	return &RangeConstraints{}
}

// IsConstraining reports whether this RangeConstraints has any
// comparisons (compilable or deferred).
func (r *RangeConstraints) IsConstraining() bool {
	return len(r.compilableComparisons) > 0 || len(r.deferredRanges) > 0
}

// IsCompileTime reports whether all comparisons in this RangeConstraints
// can be evaluated at compile time (no deferred ranges, no correlation
// references).
func (r *RangeConstraints) IsCompileTime() bool {
	if len(r.deferredRanges) > 0 {
		return false
	}
	for _, c := range r.compilableComparisons {
		if c.Operand != nil {
			corr := values.GetCorrelatedToOfValue(c.Operand)
			if len(corr) > 0 {
				return false
			}
		}
	}
	return true
}

// GetComparisons returns all comparisons (compilable + deferred),
// cached after first computation.
func (r *RangeConstraints) GetComparisons() []Comparison {
	r.comparisonsOnce.Do(func() {
		r.comparisons = make([]Comparison, 0, len(r.compilableComparisons)+len(r.deferredRanges))
		r.comparisons = append(r.comparisons, r.deferredRanges...)
		r.comparisons = append(r.comparisons, r.compilableComparisons...)
	})
	return r.comparisons
}

// GetDeferredRanges returns the deferred (non-compile-time) comparisons.
func (r *RangeConstraints) GetDeferredRanges() []Comparison {
	return r.deferredRanges
}

// GetCompilableComparisons returns the compile-time evaluable comparisons.
func (r *RangeConstraints) GetCompilableComparisons() []Comparison {
	return r.compilableComparisons
}

// GetCorrelatedTo returns the set of correlation identifiers referenced
// by all comparisons in this RangeConstraints.
func (r *RangeConstraints) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	r.correlationsOnce.Do(func() {
		r.correlations = map[values.CorrelationIdentifier]struct{}{}
		for _, c := range r.GetComparisons() {
			for alias := range c.GetCorrelatedTo() {
				r.correlations[alias] = struct{}{}
			}
		}
	})
	return r.correlations
}

// AsComparisonRange converts this RangeConstraints to a ComparisonRange
// by merging all comparisons. This is for backward compatibility with
// existing matching infrastructure that uses ComparisonRange.
func (r *RangeConstraints) AsComparisonRange() *ComparisonRange {
	result := EmptyComparisonRange()
	for _, c := range r.GetComparisons() {
		merged := result.Merge(&c)
		if merged.Ok {
			result = merged.Range
		}
	}
	return result
}

// RangeConstraintsBuilder builds a RangeConstraints incrementally.
type RangeConstraintsBuilder struct {
	compilable []Comparison
	deferred   []Comparison
}

// NewRangeConstraintsBuilder creates a new builder.
func NewRangeConstraintsBuilder() *RangeConstraintsBuilder {
	return &RangeConstraintsBuilder{}
}

// AddComparisonMaybe adds a comparison to the builder. Returns true
// if the comparison was added successfully.
func (b *RangeConstraintsBuilder) AddComparisonMaybe(c Comparison) bool {
	corr := c.GetCorrelatedTo()
	if len(corr) > 0 {
		b.deferred = append(b.deferred, c)
	} else {
		b.compilable = append(b.compilable, c)
	}
	return true
}

// Build creates the RangeConstraints from accumulated comparisons.
func (b *RangeConstraintsBuilder) Build() *RangeConstraints {
	return NewRangeConstraints(b.compilable, b.deferred)
}
