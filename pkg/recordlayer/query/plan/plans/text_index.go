package plans

import (
	"fmt"
	"hash/fnv"

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

// EqualsWithoutChildren compares index name, text scan, and reverse.
func (p *RecordQueryTextIndexPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryTextIndexPlan)
	if !ok {
		return false
	}
	return p.reverse == o.reverse &&
		p.indexName == o.indexName &&
		p.textScan == o.textScan
}

// HashCodeWithoutChildren mixes index name + text scan + reverse.
func (p *RecordQueryTextIndexPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("textindexplan|"))
	h.Write([]byte(p.indexName))
	h.Write([]byte{0})
	h.Write([]byte(p.textScan.TextComparison))
	h.Write([]byte{0})
	h.Write([]byte(p.textScan.GroupingComparisons))
	h.Write([]byte{0})
	h.Write([]byte(p.textScan.SuffixComparisons))
	if p.reverse {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders TextIndexScan(indexName, textComparison).
func (p *RecordQueryTextIndexPlan) Explain() string {
	dir := ""
	if p.reverse {
		dir = " REVERSE"
	}
	return fmt.Sprintf("TextIndexScan(%s, %s%s)", p.indexName, p.textScan.TextComparison, dir)
}

var _ RecordQueryPlan = (*RecordQueryTextIndexPlan)(nil)
