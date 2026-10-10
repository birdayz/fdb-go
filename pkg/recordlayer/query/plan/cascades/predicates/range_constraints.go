// Portions derived from FoundationDB Record Layer (RangeConstraints.java),
// Copyright 2015-2023 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package predicates

import (
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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

// AsComparisonRange converts this RangeConstraints to a ComparisonRange by
// merging all comparisons. This is for backward compatibility with existing
// matching infrastructure that uses ComparisonRange.
//
// It returns false when the constraints cannot be expressed as ONE
// ComparisonRange: the merge is total and reports what did not fit as
// residuals (Java's asMergedComparisonRange returns the range plus that list),
// but this signature has nowhere to hand them on, so `x = 5 AND x > 7` is
// refused rather than answered with the WEAKER `x = 5` — a caller filtering on
// the weaker range would return rows the constraints excluded. A caller that
// can carry residuals should call MergeAll on GetComparisons directly.
//
// LATENT, and said so deliberately: this function has no non-test callers.
func (r *RangeConstraints) AsComparisonRange() (*ComparisonRange, bool) {
	comparisons := r.GetComparisons()
	ptrs := make([]*Comparison, len(comparisons))
	for i := range comparisons {
		ptrs[i] = &comparisons[i]
	}
	merged := MergeAll(ptrs)
	if !merged.Complete() {
		return nil, false
	}
	return merged.Range, true
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
// if the comparison was added successfully; false when the comparison
// kind cannot bound a scan prefix (Java's Builder.addComparisonMaybe,
// RangeConstraints.java:787-797, gated on canBeUsedInScanPrefix).
func (b *RangeConstraintsBuilder) AddComparisonMaybe(c Comparison) bool {
	if !canBeUsedInScanPrefix(c.Type) {
		return false
	}
	if comparisonIsCompileTime(c) {
		b.compilable = appendComparisonSet(b.compilable, c)
	} else {
		b.deferred = appendComparisonSet(b.deferred, c)
	}
	return true
}

func appendComparisonSet(comparisons []Comparison, added ...Comparison) []Comparison {
	for _, comparison := range added {
		found := false
		for _, existing := range comparisons {
			if SemanticEqualsUnderAliasMap(&ComparisonPredicate{Comparison: existing}, &ComparisonPredicate{Comparison: comparison}, nil) {
				found = true
				break
			}
		}
		if !found {
			comparisons = append(comparisons, comparison)
		}
	}
	return comparisons
}

// Java requires a supported comparison kind and an entirely range-matchable
// operand tree; absence of row correlations alone does not prove compilability.
func comparisonIsCompileTime(comparison Comparison) bool {
	if comparison.ParameterName != "" {
		return false
	}
	switch comparison.Type {
	case ComparisonIsNull, ComparisonIsNotNull:
		return true
	case ComparisonEquals, ComparisonLessThan, ComparisonLessThanOrEq,
		ComparisonGreaterThan, ComparisonGreaterThanEq, ComparisonNotDistinctFrom:
		return rangeMatchableValue(comparison.Operand)
	default:
		return false
	}
}

func rangeMatchableValue(value values.Value) bool {
	switch value.(type) {
	case *values.ConstantValue, *values.BooleanValue, *values.NullValue,
		*values.ConstantObjectValue, *values.PromoteValue, *values.CastValue,
		*values.EvaluatesToValue, *values.OfTypeValue, *values.IndexEntryObjectValue:
		for _, child := range value.Children() {
			if !rangeMatchableValue(child) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// canBeUsedInScanPrefix ports RangeConstraints.Builder.canBeUsedInScanPrefix
// (RangeConstraints.java:754-784): the comparison kinds that can bound an
// index scan prefix. NOT_EQUALS, IN, LIKE, SORT and the TEXT_* family cannot
// (Java returns false for them); Java's throw on an unexpected type is
// unreachable here because Go's ComparisonType enum is closed, so the
// default arm conservatively answers false.
func canBeUsedInScanPrefix(t ComparisonType) bool {
	switch t {
	case ComparisonEquals, ComparisonLessThan, ComparisonLessThanOrEq,
		ComparisonGreaterThan, ComparisonGreaterThanEq, ComparisonStartsWith,
		ComparisonIsNull, ComparisonIsNotNull,
		ComparisonNotDistinctFrom, ComparisonIsDistinctFrom,
		ComparisonDistanceRankEquals, ComparisonDistanceRankLessThan,
		ComparisonDistanceRankLessThanOrEq:
		return true
	default:
		return false
	}
}

// Add intersects a range without reclassifying its compiled/deferred lanes.
func (b *RangeConstraintsBuilder) Add(r *RangeConstraints) {
	b.compilable = appendComparisonSet(b.compilable, r.compilableComparisons...)
	b.deferred = appendComparisonSet(b.deferred, r.deferredRanges...)
}

// Build creates the RangeConstraints from accumulated comparisons.
func (b *RangeConstraintsBuilder) Build() *RangeConstraints {
	return NewRangeConstraints(b.compilable, b.deferred)
}
