package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryIndexPlan is an index scan over a secondary index —
// reads index entries whose key prefix satisfies the scan comparisons,
// then fetches the corresponding records. Mirrors Java's
// `RecordQueryIndexPlan`.
//
// Seed surface:
//   - IndexName: name of the index being scanned.
//   - ScanComparisons: ordered list of ComparisonRanges (one per
//     index key column, left-to-right). The prefix defines the FDB
//     key range: equality ranges become exact prefix bytes, the first
//     inequality becomes range bounds, and the rest are empty (full
//     scan for those suffix columns).
//   - RecordTypes: which record types the index covers.
//   - Reverse: scan direction.
//   - FlowedType: rich Type of the row stream.
//
// The index scan is a LEAF in the plan tree — it reads directly from
// FDB (the index subspace). A follow-up fetch step may be needed if
// the index is non-covering; that lands as a separate plan node
// (RecordQueryFetchFromPartialRecordPlan in Java) when covering-index
// rules port.
type RecordQueryIndexPlan struct {
	indexName       string
	scanComparisons []*predicates.ComparisonRange
	recordTypes     []string
	flowedType      values.Type
	reverse         bool
	strictlySorted  bool
	covering        bool
	coveringColumns []string
}

// NewRecordQueryIndexPlan constructs an index scan plan.
func NewRecordQueryIndexPlan(
	indexName string,
	scanComparisons []*predicates.ComparisonRange,
	recordTypes []string,
	flowedType values.Type,
	reverse bool,
) *RecordQueryIndexPlan {
	if flowedType == nil {
		flowedType = values.UnknownType
	}
	comps := make([]*predicates.ComparisonRange, len(scanComparisons))
	copy(comps, scanComparisons)
	return &RecordQueryIndexPlan{
		indexName:       indexName,
		scanComparisons: comps,
		recordTypes:     dedupSortedStrings(recordTypes),
		flowedType:      flowedType,
		reverse:         reverse,
	}
}

// GetIndexName returns the index name.
func (p *RecordQueryIndexPlan) GetIndexName() string { return p.indexName }

// GetScanComparisons returns the per-column comparison ranges.
func (p *RecordQueryIndexPlan) GetScanComparisons() []*predicates.ComparisonRange {
	return p.scanComparisons
}

// WithScanComparisons returns a copy of the plan with new per-column comparison
// ranges, preserving every other field (covering/coveringColumns/strictlySorted/
// reverse/flowedType/recordTypes). Used by the RFC-153 buried-merge correlation
// rebase to rewrite a SARG comparand without losing the index's covering metadata.
func (p *RecordQueryIndexPlan) WithScanComparisons(comps []*predicates.ComparisonRange) *RecordQueryIndexPlan {
	copied := make([]*predicates.ComparisonRange, len(comps))
	copy(copied, comps)
	return &RecordQueryIndexPlan{
		indexName:       p.indexName,
		scanComparisons: copied,
		recordTypes:     p.recordTypes,
		flowedType:      p.flowedType,
		reverse:         p.reverse,
		strictlySorted:  p.strictlySorted,
		covering:        p.covering,
		coveringColumns: p.coveringColumns,
	}
}

// GetRecordTypes returns the covered record types.
func (p *RecordQueryIndexPlan) GetRecordTypes() []string { return p.recordTypes }

// GetFlowedType returns the rich row Type.
func (p *RecordQueryIndexPlan) GetFlowedType() values.Type { return p.flowedType }

// IsReverse reports the scan direction.
func (p *RecordQueryIndexPlan) IsReverse() bool { return p.reverse }

// IsStrictlySorted reports whether the scan's ordering uniquely
// determines each record (no two adjacent records share the same key).
// Set by RemoveSortRule when DISTINCT covers all ordering keys or a
// unique index satisfies the full key set.
func (p *RecordQueryIndexPlan) IsStrictlySorted() bool { return p.strictlySorted }

// WithStrictlySorted returns a shallow copy with strictlySorted=true.
func (p *RecordQueryIndexPlan) WithStrictlySorted() *RecordQueryIndexPlan {
	cp := *p
	cp.strictlySorted = true
	return &cp
}

// IsCovering reports whether the index provides all columns needed by
// the query, eliminating the need to fetch the full record by PK.
func (p *RecordQueryIndexPlan) IsCovering() bool { return p.covering }

// GetCoveringColumns returns the index column names when covering.
func (p *RecordQueryIndexPlan) GetCoveringColumns() []string { return p.coveringColumns }

// WithCovering returns a shallow copy marked as a covering index scan.
func (p *RecordQueryIndexPlan) WithCovering(columns []string) *RecordQueryIndexPlan {
	cp := *p
	cp.covering = true
	cp.coveringColumns = make([]string, len(columns))
	copy(cp.coveringColumns, columns)
	return &cp
}

// GetResultType returns the row Type.
func (p *RecordQueryIndexPlan) GetResultType() values.Type { return p.flowedType }

// GetChildren returns nil — index scans are leaves.
func (p *RecordQueryIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// EqualsWithoutChildren compares index name, scan comparisons (shape AND
// comparands), record types, and reverse flag. The comparands are load-bearing:
// an index scan is a memo LEAF, deduped by HashCodeWithoutChildren +
// EqualsWithoutChildren, so comparing only the range SHAPE would collapse
// IndexScan([= 5]) and IndexScan([= 7]) into one Reference and let extraction
// materialize the wrong-comparand scan. Mirrors Java's
// RecordQueryIndexPlan.equalsWithoutChildren, which compares
// Objects.equals(scanParameters, that.scanParameters) — full comparand equality.
func (p *RecordQueryIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryIndexPlan)
	if !ok {
		return false
	}
	if p.indexName != o.indexName || p.reverse != o.reverse || p.strictlySorted != o.strictlySorted || p.covering != o.covering {
		return false
	}
	if !typeEquals(p.flowedType, o.flowedType) {
		return false
	}
	if len(p.recordTypes) != len(o.recordTypes) {
		return false
	}
	for i := range p.recordTypes {
		if p.recordTypes[i] != o.recordTypes[i] {
			return false
		}
	}
	return scanComparisonRangesEqual(p.scanComparisons, o.scanComparisons)
}

// HashCodeWithoutChildren mixes index name + scan comparisons (shape AND
// comparands) + reverse flag. Folding the comparands (not just the range
// shape) is the hash analog of EqualsWithoutChildren: two different-comparand
// scans hash apart, so the memo leaf dedup keeps them in distinct References.
func (p *RecordQueryIndexPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("indexplan|"))
	h.Write([]byte(p.indexName))
	h.Write([]byte{0})
	writeScanComparisonRangesHash(h, p.scanComparisons)
	if p.reverse {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	if p.strictlySorted {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	if p.covering {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders a one-line label.
func (p *RecordQueryIndexPlan) Explain() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("IndexScan(%s, [", p.indexName))
	for i, cr := range p.scanComparisons {
		if i > 0 {
			b.WriteString(", ")
		}
		switch cr.GetRangeType() {
		case predicates.ComparisonRangeEmpty:
			b.WriteString("*")
		case predicates.ComparisonRangeEquality:
			b.WriteString("=")
		case predicates.ComparisonRangeInequality:
			b.WriteString("<>")
		}
	}
	b.WriteString("]")
	if p.covering {
		b.WriteString(" COVERING")
	}
	if p.reverse {
		b.WriteString(") REVERSE")
	} else {
		b.WriteString(")")
	}
	return b.String()
}

var _ RecordQueryPlan = (*RecordQueryIndexPlan)(nil)
