package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TextScan encapsulates the information necessary to scan a text-based
// index. Mirrors Java's `com.apple.foundationdb.record.query.plan.TextScan`.
//
// This is a STRUCTURE-ONLY port — no execution logic. The fields carry
// enough information for plan equality, hashing, and explain rendering.
type TextScan struct {
	// IndexName is the name of the text index being scanned.
	IndexName string
	// GroupingComparisons is a human-readable description of the
	// grouping-key prefix comparisons (may be empty).
	GroupingComparisons string
	// TextComparison is a human-readable description of the text
	// comparison (e.g. "TEXT_CONTAINS_ALL 'hello world'").
	TextComparison string
	// SuffixComparisons is a human-readable description of the suffix
	// comparisons (may be empty).
	SuffixComparisons string
}

// RecordQueryTextIndexPlan executes a text index scan. Text indexes
// work differently from regular indexes — the comparison on a query
// might be split into multiple sub-scans that are intersected or
// unioned. Mirrors Java's RecordQueryTextIndexPlan.
//
// This is a STRUCTURE-ONLY port — no execution logic. It implements
// RecordQueryPlan as a leaf plan (no children).
type RecordQueryTextIndexPlan struct {
	PlanExprBase
	indexName string
	textScan  TextScan
	reverse   bool
}

// NewRecordQueryTextIndexPlan constructs a text index plan.
func NewRecordQueryTextIndexPlan(indexName string, textScan TextScan, reverse bool) *RecordQueryTextIndexPlan {
	return &RecordQueryTextIndexPlan{
		indexName: indexName,
		textScan:  textScan,
		reverse:   reverse,
	}
}

// GetIndexName returns the index name.
func (p *RecordQueryTextIndexPlan) GetIndexName() string { return p.indexName }

// GetTextScan returns the text scan descriptor.
func (p *RecordQueryTextIndexPlan) GetTextScan() TextScan { return p.textScan }

// IsReverse reports the scan direction.
func (p *RecordQueryTextIndexPlan) IsReverse() bool { return p.reverse }

// GetResultType returns UnknownType — the text index plan's result
// type is determined at execution time from the index metadata.
// Mirrors Java where getResultValue() returns new QueriedValue()
// (untyped).
func (p *RecordQueryTextIndexPlan) GetResultType() values.Type { return values.UnknownType }

// GetChildren returns nil — text index scans are leaves.
func (p *RecordQueryTextIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the text-scan identity: reverse flag, index name, and the
// TextScan descriptor decomposed into its four string fields (the comparable
// struct's `==` compared all four; folding each keeps that exact equality).
// Drives both Equals and Hash.
func (p *RecordQueryTextIndexPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Bool(p.reverse).
		Str(p.indexName).
		Str(p.textScan.IndexName).
		Str(p.textScan.GroupingComparisons).
		Str(p.textScan.TextComparison).
		Str(p.textScan.SuffixComparisons)
}

// EqualsWithoutChildren compares index name, text scan, and reverse.
func (p *RecordQueryTextIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTextIndexPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren mixes index name + text scan + reverse.
func (p *RecordQueryTextIndexPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("textindexplan|")
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
func (p *RecordQueryTextIndexPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryTextIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
