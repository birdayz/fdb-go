// Portions derived from FoundationDB Record Layer (TextScan.java,
// RecordQueryTextIndexPlan.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TextScan encapsulates the information necessary to scan a text-based
// index. Mirrors Java's `com.apple.foundationdb.record.query.plan.TextScan`.
//
// The typed fields are what the executor runs (through recordlayer.TextScan);
// the string fields are descriptive and only TextComparison is explained.
type TextScan struct {
	Predicate *predicates.Comparison
	KeyTypes  []values.Type
	Grouping  []values.Value
	Suffix    []*predicates.ComparisonRange
	// IndexName is the name of the text index being scanned.
	IndexName string
	// GroupingComparisons and SuffixComparisons describe the bounds; the
	// Cascades planner leaves them empty.
	GroupingComparisons string
	// TextComparison names the comparison; the planner sets its type symbol.
	TextComparison    string
	SuffixComparisons string
}

// RecordQueryTextIndexPlan executes a text index scan. Text indexes
// work differently from regular indexes — the comparison on a query
// might be split into multiple sub-scans that are intersected or
// unioned. Mirrors Java's RecordQueryTextIndexPlan; the old planner's port is
// recordlayer.TextIndexPlan, built by recordlayer.PlanTextQuery.
//
// It implements RecordQueryPlan as a leaf plan (no children).
type RecordQueryTextIndexPlan struct {
	PlanExprBase
	indexName string
	textScan  TextScan
	reverse   bool
}

// NewRecordQueryTextIndexPlan constructs a text index plan.
func NewRecordQueryTextIndexPlan(indexName string, textScan TextScan, flowedType values.Type, reverse bool) (*RecordQueryTextIndexPlan, error) {
	base, err := newPlanExprBaseForType("RecordQueryTextIndexPlan", flowedType)
	if err != nil {
		return nil, err
	}
	return &RecordQueryTextIndexPlan{
		PlanExprBase: base,
		indexName:    indexName,
		textScan:     copyTextScan(textScan),
		reverse:      reverse,
	}, nil
}

// GetIndexName returns the index name.
func (p *RecordQueryTextIndexPlan) GetIndexName() string { return p.indexName }

// GetTextScan returns the text scan descriptor.
func (p *RecordQueryTextIndexPlan) GetTextScan() TextScan { return copyTextScan(p.textScan) }

// WithTextScan returns a copy scanning with the given descriptor.
func (p *RecordQueryTextIndexPlan) WithTextScan(scan TextScan) *RecordQueryTextIndexPlan {
	cp := *p
	cp.textScan = copyTextScan(scan)
	return &cp
}

func copyTextScan(scan TextScan) TextScan {
	scan.KeyTypes = append([]values.Type(nil), scan.KeyTypes...)
	scan.Grouping = append([]values.Value(nil), scan.Grouping...)
	scan.Suffix = append([]*predicates.ComparisonRange(nil), scan.Suffix...)
	if scan.Predicate != nil {
		comparison := *scan.Predicate
		scan.Predicate = &comparison
	}
	return scan
}

// IsReverse reports the scan direction.
func (p *RecordQueryTextIndexPlan) IsReverse() bool { return p.reverse }

func (p *RecordQueryTextIndexPlan) GetResultType() values.Type { return p.GetResultValue().Type() }

// GetChildren returns nil — text index scans are leaves.
func (p *RecordQueryTextIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey includes typed bounds, physical key types and comparison
// metadata as well as the legacy explain fields. Equality and hashing share it.
func (p *RecordQueryTextIndexPlan) structuralKey() *structuralKey {
	k := newStructuralKey().Types(p.textScan.KeyTypes).Values(p.textScan.Grouping).ScanComps(p.textScan.Suffix)
	if p.textScan.Predicate != nil {
		k.Preds([]predicates.QueryPredicate{&predicates.ComparisonPredicate{Comparison: *p.textScan.Predicate}})
	}
	return k.
		Bool(p.reverse).
		Str(p.indexName).
		Str(p.textScan.IndexName).
		Str(p.textScan.GroupingComparisons).
		Str(p.textScan.TextComparison).
		Str(p.textScan.SuffixComparisons)
}

// EqualsPlanWithoutChildren compares index name, text scan, and reverse.
func (p *RecordQueryTextIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTextIndexPlan)
	return ok && p.keyFor(p).Equal(o.keyFor(o))
}

// HashCodeWithoutChildren mixes index name + text scan + reverse.
func (p *RecordQueryTextIndexPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.keyFor(p).Hash("textindexplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders TextIndexScan(indexName, textComparison).
func (p *RecordQueryTextIndexPlan) Explain() string {
	dir := ""
	if p.reverse {
		dir = " REVERSE"
	}
	return fmt.Sprintf("TextIndexScan(%s, %s%s)", p.indexName, p.textScan.TextComparison, dir)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryTextIndexPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryTextIndexPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryTextIndexPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryTextIndexPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryTextIndexPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetCorrelatedToWithoutChildren reports the bindings' correlations, such as
// an IN join's element bound to a grouping column.
func (p *RecordQueryTextIndexPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := scanComparisonCorrelations(p.textScan.Suffix)
	operands := append([]values.Value(nil), p.textScan.Grouping...)
	if p.textScan.Predicate != nil {
		operands = append(operands, p.textScan.Predicate.Operand)
	}
	for _, v := range operands {
		for a := range values.GetCorrelatedToOfValue(v) {
			out[a] = struct{}{}
		}
	}
	return out
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTextIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
