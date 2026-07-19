package plans

import (
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryScanPlan is a primary-key scan over a set of record
// types — the leaf physical-plan that reads records sequentially
// from the FDB store. Mirrors Java's `RecordQueryScanPlan`.
//
// Seed surface:
//   - RecordTypes: which record types to emit. Empty = all types.
//   - FlowedType: the rich Type of the row stream (RecordType for
//     a single type, UnionType for multi-type scans).
//   - Reverse: whether to scan in reverse PK order.
//
// What's NOT in the seed: range bounds, scan-property bag,
// continuation, scan-comparison thunk. Those land when consumers
// (Batch A index rules) need them.
type RecordQueryScanPlan struct {
	PlanExprBase
	recordTypes     []string
	flowedType      values.Type
	reverse         bool
	primaryKeyVals  []values.Value
	scanComparisons []*predicates.ComparisonRange
}

// NewRecordQueryScanPlan builds a scan over the given record types
// in the given direction. recordTypes is normalised (sorted +
// deduped); empty slice → scan over all types.
func NewRecordQueryScanPlan(recordTypes []string, flowedType values.Type, reverse bool) *RecordQueryScanPlan {
	if flowedType == nil {
		flowedType = values.UnknownType
	}
	return &RecordQueryScanPlan{
		recordTypes: dedupSortedStrings(recordTypes),
		flowedType:  flowedType,
		reverse:     reverse,
	}
}

// WithPrimaryKey returns a copy of the scan plan with PK values set.
func (p *RecordQueryScanPlan) WithPrimaryKey(pk []values.Value) *RecordQueryScanPlan {
	copied := make([]values.Value, len(pk))
	copy(copied, pk)
	return &RecordQueryScanPlan{
		recordTypes:    p.recordTypes,
		flowedType:     p.flowedType,
		reverse:        p.reverse,
		primaryKeyVals: copied,
	}
}

// WithScanComparisons returns a copy with the given scan comparisons.
// Mirrors Java's RecordQueryScanPlan constructor that accepts ScanComparisons.
func (p *RecordQueryScanPlan) WithScanComparisons(comps []*predicates.ComparisonRange) *RecordQueryScanPlan {
	copied := make([]*predicates.ComparisonRange, len(comps))
	copy(copied, comps)
	return &RecordQueryScanPlan{
		recordTypes:     p.recordTypes,
		flowedType:      p.flowedType,
		reverse:         p.reverse,
		primaryKeyVals:  p.primaryKeyVals,
		scanComparisons: copied,
	}
}

// GetScanComparisons returns the per-column comparison ranges for PK narrowing.
func (p *RecordQueryScanPlan) GetScanComparisons() []*predicates.ComparisonRange {
	return p.scanComparisons
}

// GetPrimaryKeyValues returns the primary key values, or nil if not set.
func (p *RecordQueryScanPlan) GetPrimaryKeyValues() []values.Value { return p.primaryKeyVals }

// GetRecordTypes returns the canonical record-type-name list.
func (p *RecordQueryScanPlan) GetRecordTypes() []string { return p.recordTypes }

// GetFlowedType returns the rich Type of rows flowing out.
func (p *RecordQueryScanPlan) GetFlowedType() values.Type { return p.flowedType }

// IsReverse reports the scan direction.
func (p *RecordQueryScanPlan) IsReverse() bool { return p.reverse }

// GetResultType returns the row Type — same as FlowedType for the
// seed (no per-row projection in a scan).
func (p *RecordQueryScanPlan) GetResultType() values.Type { return p.flowedType }

// GetChildren returns the empty slice — scans are leaves.
func (p *RecordQueryScanPlan) GetChildren() []RecordQueryPlan { return nil }

func (p *RecordQueryScanPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryScanPlan)
	if !ok {
		return false
	}
	if p.reverse != o.reverse {
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
	// Compare scan comparisons by shape AND comparand — the PK bounds that
	// define the key range. A primary scan is a memo leaf too, so a
	// comparand-blind compare would collapse Scan(pk = 5) and Scan(pk = 7) into
	// one Reference (the F21 defect, same as RecordQueryIndexPlan).
	return scanComparisonRangesEqual(p.scanComparisons, o.scanComparisons)
}

func (p *RecordQueryScanPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("scanplan|"))
	for _, name := range p.recordTypes {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	writeScanComparisonRangesHash(h, p.scanComparisons)
	if p.reverse {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// Explain renders a one-line label.
func (p *RecordQueryScanPlan) Explain() string {
	var b strings.Builder
	b.WriteString("Scan(")
	for i, name := range p.recordTypes {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
	}
	if len(p.scanComparisons) > 0 {
		b.WriteString(", [")
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
	}
	if p.reverse {
		b.WriteString(") REVERSE")
	} else {
		b.WriteString(")")
	}
	return b.String()
}

// dedupSortedStrings normalises a string slice: sorts + dedupes.
// Returns nil if the input is empty.
func dedupSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	// Insertion sort — tiny slices, no need for sort.Strings overhead.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	// Dedup adjacent duplicates.
	w := 1
	for r := 1; r < len(out); r++ {
		if out[r] != out[w-1] {
			out[w] = out[r]
			w++
		}
	}
	return out[:w]
}

// typeEquals compares two Types via the Equals method, with nil
// handling.
func typeEquals(a, b values.Type) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equals(b)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryScanPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryScanPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryScanPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryScanPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetCorrelatedToWithoutChildren reports the correlations reached through this
// scan's comparison operands, mirroring physicalScanWrapper.
func (p *RecordQueryScanPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return scanComparisonCorrelations(p.GetScanComparisons())
}

// GetRecordQueryPlan returns the plan itself.
//
// This method, present on every plan type, is what lets a bare plan stand in
// for a physical wrapper in the memo: cascades recognises a physical
// expression by asserting `RelationalExpression + GetRecordQueryPlan()`
// (physical_wrapper.go's physicalPlanExpression), and step 1 already gave
// plans the RelationalExpression half.
//
// It is deliberately PER-TYPE rather than a method on PlanExprBase. The
// embedded base is a zero-size struct with no back-pointer to the value that
// embeds it, so a base implementation could only ever return nil or itself —
// never the outer plan. Go has no `self` type; each type must name itself.
func (p *RecordQueryScanPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
