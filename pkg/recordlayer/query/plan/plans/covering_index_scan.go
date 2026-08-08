package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryCoveringIndexPlan is an index scan that answers from the index
// entry alone — it reconstructs a PARTIAL record from the entry's covered
// columns instead of resolving the base record by primary key. Mirrors Java's
// `RecordQueryCoveringIndexPlan`.
//
// The inner index plan is held as a plain FIELD, never as a quantifier and
// never as a child. That mirrors Java, where the type
// `implements RecordQueryPlanWithNoChildren` and the access path memoizes only
// the covering plan (ValueIndexScanMatchCandidate.tryFetchCoveringIndexScan),
// leaving the index plan a field on it. Two things depend on the inner staying
// invisible to child traversal:
//
//   - Soundness of the memo. If the inner were a child quantifier, rules
//     matching a bare index plan would yield a group member into the inner's
//     Reference whose rows are FULL records, while this plan's rows are PARTIAL
//     records. Those are not interchangeable, so they must not share a group.
//   - Cost classification. The cost model's expression census counts an index
//     scan it can reach; reaching the inner would restore `indexScanCount == 1`
//     for a plan that performs no fetch, which `isSingularIndexScanWithFetch`
//     reads as "a singular index scan WITH a fetch" — a contradiction that
//     routes a fetchless covering scan to the contested cost tier.
//
// Because the inner is a field, nothing in generic child traversal folds it
// into identity. structuralKey therefore folds the inner's FULL identity (via
// the inner's own structural key), not merely the covering columns: two
// covering scans over the same index with different scan ranges are different
// plans and must not collapse into one Reference.
type RecordQueryCoveringIndexPlan struct {
	PlanExprBase
	// indexPlan is the scan this covering plan reads entries from. A FIELD,
	// deliberately — see the type comment.
	indexPlan *RecordQueryIndexPlan
	// coveringColumns is the entry's column list in ENTRY layout order (index
	// key columns, then the KeyWithValue VALUE part). The executor aligns it
	// positionally against (index key values ++ entry value tuple) to rebuild
	// the partial record. May be empty: an aggregate/grouping scan covers no
	// value columns and still answers from the entry.
	coveringColumns []string
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this leaf emits, minted once at construction and EXCLUDED from
	// identity (its correlation id is unique per instance). Same contract as
	// RecordQueryIndexPlan's, for the same reason: a bare leaf standing as its
	// own Cascades expression must present a consistent row identity across
	// repeated interrogations.
	resultValue values.Value
}

// NewRecordQueryCoveringIndexPlan wraps an index scan as a covering scan over
// the given entry-layout columns. Mirrors Java's construction at the access
// path, which is unconditional whenever the entry can be turned into a partial
// record and never consults the projection.
func NewRecordQueryCoveringIndexPlan(
	indexPlan *RecordQueryIndexPlan,
	coveringColumns []string,
) *RecordQueryCoveringIndexPlan {
	cols := make([]string, len(coveringColumns))
	copy(cols, coveringColumns)
	return &RecordQueryCoveringIndexPlan{
		indexPlan:       indexPlan,
		coveringColumns: cols,
		resultValue:     values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
	}
}

// GetIndexPlan returns the wrapped index scan. It is a field, not a child:
// callers that walk the plan tree will NOT reach it, by design.
func (p *RecordQueryCoveringIndexPlan) GetIndexPlan() *RecordQueryIndexPlan { return p.indexPlan }

// WithIndexPlan returns a shallow copy over a rewritten inner scan, preserving
// the covering columns and the stable result value. Used by rewrites that
// rebase the inner's scan comparisons.
func (p *RecordQueryCoveringIndexPlan) WithIndexPlan(inner *RecordQueryIndexPlan) *RecordQueryCoveringIndexPlan {
	cp := *p
	cp.indexPlan = inner
	return &cp
}

// GetCoveringColumns returns the entry-layout column names this scan covers.
func (p *RecordQueryCoveringIndexPlan) GetCoveringColumns() []string { return p.coveringColumns }

// GetIndexName delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetIndexName() string { return p.indexPlan.GetIndexName() }

// IsReverse delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) IsReverse() bool { return p.indexPlan.IsReverse() }

// IsStrictlySorted delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) IsStrictlySorted() bool {
	return p.indexPlan.IsStrictlySorted()
}

// ProducesDistinctRecords delegates to the inner scan: reconstructing a partial
// record from an entry neither creates nor removes duplicates.
func (p *RecordQueryCoveringIndexPlan) ProducesDistinctRecords() bool {
	return p.indexPlan.ProducesDistinctRecords()
}

// GetResultValue returns the STABLE per-instance result value, falling back to
// PlanExprBase for struct-literal test plans that bypass the constructor.
func (p *RecordQueryCoveringIndexPlan) GetResultValue() values.Value {
	if p.resultValue == nil {
		return p.PlanExprBase.GetResultValue()
	}
	return p.resultValue
}

// GetResultType returns the inner scan's flowed row Type. Java's covering plan
// likewise reports the base record's type rather than a projected/flattened
// shape.
func (p *RecordQueryCoveringIndexPlan) GetResultType() values.Type {
	return p.indexPlan.GetResultType()
}

// GetChildren returns nil. The inner index plan is a field, not a child — see
// the type comment for why that is load-bearing rather than incidental.
func (p *RecordQueryCoveringIndexPlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the covering columns AND the inner scan's full identity.
//
// Folding the inner in full is required, not defensive: the inner is a field,
// so no generic child traversal contributes it to this node's identity. Fold
// only the covering columns and two covering scans over the same index with
// DIFFERENT scan ranges hash and compare equal, collapsing into a single memo
// Reference from which extraction can materialize the wrong-range scan.
//
// The covering columns are folded for the mirror-image reason: two covering
// scans over the same range but different covered-column sets emit DIFFERENT
// partial records, so they are not interchangeable group members either.
func (p *RecordQueryCoveringIndexPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Sub(p.indexPlan.structuralKey()).
		Strs(p.coveringColumns)
}

func (p *RecordQueryCoveringIndexPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryCoveringIndexPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryCoveringIndexPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("coveringindexplan|")
}

// Explain renders the inner scan's label with the COVERING marker inserted
// before the closing paren, so a covering scan reads as
// `IndexScan(IDX, [=] COVERING)`. Java renders its covering plan as
// `COVERING(IDX [...] -> ...)`; converging on that shape is deliberately out of
// scope here and would churn every plan golden for an unrelated reason.
func (p *RecordQueryCoveringIndexPlan) Explain() string {
	inner := p.indexPlan.Explain()
	// The inner never carries COVERING now that the flag is gone, so the marker
	// is inserted at the same position the flag used to render it: after the
	// comparison-range bracket, before `)` / `) REVERSE`.
	if idx := strings.LastIndex(inner, "]"); idx >= 0 {
		return inner[:idx+1] + " COVERING" + inner[idx+1:]
	}
	return fmt.Sprintf("%s COVERING", inner)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryCoveringIndexPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryCoveringIndexPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryCoveringIndexPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers. The
// inner index plan is a field and is deliberately not exposed as one.
func (p *RecordQueryCoveringIndexPlan) WithQuantifiers(_ []expressions.Quantifier) expressions.RelationalExpression {
	return p
}

// GetCorrelatedToWithoutChildren reports the correlations reached through the
// inner scan's comparison operands. The inner is not a child, so its
// correlations would otherwise be invisible to correlation analysis — which
// would let a rule hoist this plan above the quantifier its range depends on.
func (p *RecordQueryCoveringIndexPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return p.indexPlan.GetCorrelatedToWithoutChildren()
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryCoveringIndexPlan) GetRecordQueryPlan() RecordQueryPlan { return p }

// GetDistinctProofIndexName implements DistinctProofStamped by delegating to
// the inner scan, which is where the stamp is carried.
func (p *RecordQueryCoveringIndexPlan) GetDistinctProofIndexName() string {
	return p.indexPlan.GetDistinctProofIndexName()
}

// WithDistinctProofIndexName implements DistinctProofStampable, stamping the
// inner scan and returning a copy over it.
func (p *RecordQueryCoveringIndexPlan) WithDistinctProofIndexName(indexName string) RecordQueryPlan {
	inner, ok := p.indexPlan.WithDistinctProofIndexName(indexName).(*RecordQueryIndexPlan)
	if !ok {
		return p
	}
	return p.WithIndexPlan(inner)
}

var _ DistinctProofStampable = (*RecordQueryCoveringIndexPlan)(nil)
