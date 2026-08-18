package plans

import (
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
	// keyComponentTypes is aligned with scanComparisons and records the
	// authoritative physical primary-key type for each bound component. The
	// executor must use these types, rather than the RHS value's declaration,
	// when choosing the FDB tuple representation.
	keyComponentTypes []values.Type
	// resultValue is the stable per-instance QuantifiedObjectValue standing
	// for the rows this scan emits. Minted ONCE in NewRecordQueryScanPlan and
	// carried through every copy (WithPrimaryKey/WithScanComparisons), it gives
	// a bare scan a single stable correlation identity when it stands as its own
	// Cascades expression in the memo — the role a physical scan wrapper's
	// GetResultValue used to play (RFC-184 W2). Deliberately EXCLUDED from
	// EqualsPlanWithoutChildren/HashCodeWithoutChildren: its correlation id is
	// unique per instance, so folding it into identity would make two
	// structurally-identical scans compare unequal. nil for struct-literal test
	// plans that bypass the constructor — GetResultValue falls back to
	// PlanExprBase's fresh-QOV there.
	resultValue values.Value
	// distinctProofIndexName names the secondary UNIQUE index whose uniqueness
	// licensed eliding a DISTINCT above this scan (distinct_proof_stamp.go).
	distinctProofIndexName string
}

// NewRecordQueryScanPlan builds a scan over the given record types
// in the given direction. recordTypes is normalised (sorted +
// deduped); empty slice → scan over all types.
func NewRecordQueryScanPlan(recordTypes []string, flowedType values.Type, reverse bool) (*RecordQueryScanPlan, error) {
	base, err := newPlanExprBaseForType("RecordQueryScanPlan", flowedType)
	if err != nil {
		return nil, err
	}
	return &RecordQueryScanPlan{
		PlanExprBase: base,
		recordTypes:  dedupSortedStrings(recordTypes),
		flowedType:   base.resultValue.Type(),
		reverse:      reverse,
		resultValue:  base.resultValue,
	}, nil
}

// WithPrimaryKey returns a copy of the scan plan with PK values set.
// Preserves scanComparisons (a copy must carry every scan field — dropping the
// comparisons here silently un-narrows the scan; production happens to set the
// PK before the comparisons, so the drop was latent, but the asymmetry with
// WithScanComparisons was a footgun).
func (p *RecordQueryScanPlan) WithPrimaryKey(pk []values.Value) *RecordQueryScanPlan {
	copied := make([]values.Value, len(pk))
	copy(copied, pk)
	cp := *p
	cp.primaryKeyVals = copied
	return &cp
}

// WithScanComparisons returns a copy with the given scan comparisons.
// Mirrors Java's RecordQueryScanPlan constructor that accepts ScanComparisons.
func (p *RecordQueryScanPlan) WithScanComparisons(comps []*predicates.ComparisonRange) *RecordQueryScanPlan {
	copied := make([]*predicates.ComparisonRange, len(comps))
	copy(copied, comps)
	cp := *p
	cp.scanComparisons = copied
	cp.keyComponentTypes = normalizeKeyComponentTypes(p.keyComponentTypes, len(comps))
	return &cp
}

// WithKeyComponentTypes returns a copy carrying authoritative physical
// primary-key types. The vector may extend past the bound comparison prefix:
// ordering proofs need the domains of unbound suffix components too. Missing
// entries required by comparisons are made explicit as UnknownType.
func (p *RecordQueryScanPlan) WithKeyComponentTypes(types []values.Type) *RecordQueryScanPlan {
	cp := *p
	cp.keyComponentTypes = normalizeKeyComponentTypes(types, len(p.scanComparisons))
	return &cp
}

// GetKeyComponentTypes returns the authoritative physical types aligned with
// GetScanComparisons.
func (p *RecordQueryScanPlan) GetKeyComponentTypes() []values.Type {
	return append([]values.Type(nil), p.keyComponentTypes...)
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

// GetResultValue returns the scan's STABLE per-instance result value — the
// single correlation identity a bare scan carries as its own memo expression
// (RFC-184 W2), the role a physical scan wrapper's GetResultValue used to play. Unlike
// PlanExprBase's method (a fresh QOV per call), this returns the same value the
// constructor minted, so repeated interrogations of one plan instance agree.
// Falls back to PlanExprBase for struct-literal test plans that bypass the
// constructor (resultValue is nil there).
func (p *RecordQueryScanPlan) GetResultValue() values.Value {
	return p.resultValue
}

// structuralKey folds the scan's identity: the record-type set (ordered), the
// SARG comparison ranges (the PK bounds — a comparand-blind compare would
// collapse Scan(pk=5) and Scan(pk=7) into one Reference, the F21 defect), the
// reverse flag, and the flowed type (equals-only, matching the hand-rolled hash
// which never folded it). The stable per-instance resultValue is deliberately
// excluded (RFC-184 W2 — an object identity, not part of plan identity). The
// same key drives EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryScanPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Strs(p.recordTypes).
		ScanComps(p.scanComparisons).
		Types(p.keyComponentTypes).
		Bool(p.reverse).
		Type(p.flowedType).
		Str(p.distinctProofIndexName)
}

func normalizeKeyComponentTypes(types []values.Type, size int) []values.Type {
	if len(types) > size {
		size = len(types)
	}
	if size <= 0 {
		return nil
	}
	result := make([]values.Type, size)
	for i := range result {
		result[i] = values.UnknownType
		if i < len(types) && types[i] != nil {
			result[i] = types[i]
		}
	}
	return result
}

// normalizePrimaryKeyComponentTypes returns an exact-width vector aligned with
// the visible PK name list. Unlike index-key metadata, which may intentionally
// include unbound coordinates beyond a comparison prefix, an appended-PK type
// has no meaning without its parallel visible name.
func normalizePrimaryKeyComponentTypes(types []values.Type, size int) []values.Type {
	if size <= 0 {
		return nil
	}
	result := make([]values.Type, size)
	for i := range result {
		result[i] = values.UnknownType
		if i < len(types) && types[i] != nil {
			result[i] = types[i]
		}
	}
	return result
}

func (p *RecordQueryScanPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryScanPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryScanPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("scanplan|")
	p.storeStructuralHash(p, hash)
	return hash
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
	b.WriteString(explainDistinctProofSuffix(p.distinctProofIndexName))
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
func (p *RecordQueryScanPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryScanPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetCorrelatedToWithoutChildren reports the correlations reached through this
// scan's comparison operands.
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

// GetDistinctProofIndexName implements DistinctProofStamped. A base-record scan
// is the shape the secondary-UNIQUE DISTINCT elision actually produces on the
// unordered `SELECT DISTINCT <unique column>` regime the optimization is for, so
// it must be able to carry the proof directly — the elided plan is sometimes a
// bare scan with no projection above it (`SELECT DISTINCT *`).
func (p *RecordQueryScanPlan) GetDistinctProofIndexName() string {
	return p.distinctProofIndexName
}

// WithDistinctProofIndexName implements DistinctProofStampable.
func (p *RecordQueryScanPlan) WithDistinctProofIndexName(indexName string) RecordQueryPlan {
	cp := *p
	cp.distinctProofIndexName = indexName
	return &cp
}

var _ DistinctProofStampable = (*RecordQueryScanPlan)(nil)
