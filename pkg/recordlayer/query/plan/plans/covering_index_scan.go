package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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

// NewRecordQueryCoveringIndexPlan wraps an index scan as a covering scan.
// Mirrors Java's construction at the access path, which is unconditional
// whenever the entry can be turned into a partial record and never consults the
// projection.
//
// The covered columns are DERIVED from the inner scan rather than accepted from
// the caller. Accepting them made an inconsistent pair representable — a column
// list naming something the entry does not carry, or omitting something it
// does — and the executor aligns that list POSITIONALLY against
// (index key values ++ entry value tuple). A mismatch there is not a loud
// failure; it silently reads a value into the wrong logical slot. Deriving
// removes the failure mode instead of documenting it.
func NewRecordQueryCoveringIndexPlan(indexPlan *RecordQueryIndexPlan) (*RecordQueryCoveringIndexPlan, error) {
	if indexPlan == nil {
		return nil, fmt.Errorf("RecordQueryCoveringIndexPlan: index plan is nil")
	}
	base, err := newPlanExprBaseForType("RecordQueryCoveringIndexPlan", indexPlan.GetResultType())
	if err != nil {
		return nil, err
	}
	return &RecordQueryCoveringIndexPlan{
		PlanExprBase:    base,
		indexPlan:       indexPlan,
		coveringColumns: indexPlan.AllCoveredEntryColumns(),
		resultValue:     base.resultValue,
	}, nil
}

// GetIndexPlan returns the wrapped index scan. It is a field, not a child:
// callers that walk the plan tree will NOT reach it, by design.
//
// Nil-tolerant on the receiver, via inner(): this is IndexScanCarrier's single
// method, and the hint-contract parity harnesses enumerate plan types as TYPED
// NILS. A carrier accessor that panics on the enumeration shape would make the
// interface unusable exactly where it is most useful — a generic walk that does
// not know which concrete type it holds.
func (p *RecordQueryCoveringIndexPlan) GetIndexPlan() *RecordQueryIndexPlan { return p.inner() }

// WithIndexPlan returns a shallow copy over a rewritten inner scan, preserving
// the stable result value. Used by rewrites that rebase the inner's scan
// comparisons.
//
// The covered columns are RE-DERIVED from the new inner rather than carried
// over: carrying them would let a rewrite that changes the inner's covered
// surface leave this plan describing the old one, which the executor would
// then align positionally against the new entry layout.
func (p *RecordQueryCoveringIndexPlan) WithIndexPlan(inner *RecordQueryIndexPlan) *RecordQueryCoveringIndexPlan {
	cp := *p
	cp.indexPlan = inner
	cp.coveringColumns = inner.AllCoveredEntryColumns()
	return &cp
}

// GetCoveringColumns returns the entry-layout column names this scan covers.
func (p *RecordQueryCoveringIndexPlan) GetCoveringColumns() []string { return p.coveringColumns }

// GetIndexName delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetIndexName() string { return p.indexPlan.GetIndexName() }

// IsReverse delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) IsReverse() bool { return p.indexPlan.IsReverse() }

// IsStrictlySorted delegates to the inner scan, mirroring Java's
// RecordQueryCoveringIndexPlan.isStrictlySorted
// (RecordQueryCoveringIndexPlan.java:174-176). Reconstructing a partial record
// from an entry cannot break a strict ordering the entry stream already has.
func (p *RecordQueryCoveringIndexPlan) IsStrictlySorted() bool {
	return p.indexPlan.IsStrictlySorted()
}

// ProducesDistinctRecords delegates to the inner scan: reconstructing a partial
// record from an entry neither creates nor removes duplicates.
func (p *RecordQueryCoveringIndexPlan) ProducesDistinctRecords() bool {
	return p.indexPlan.ProducesDistinctRecords()
}

// GetResultValue returns the STABLE per-instance result value minted by the
// constructor — the only way this type is built.
func (p *RecordQueryCoveringIndexPlan) GetResultValue() values.Value {
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
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("coveringindexplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

// Explain renders the inner scan's label carrying the COVERING marker, so a
// covering scan reads as `IndexScan(IDX, [=] COVERING)`. Java renders its
// covering plan as `COVERING(IDX [...] -> ...)`; converging on that shape is
// deliberately out of scope here and would churn every plan golden for an
// unrelated reason.
//
// The marker is passed DOWN to the inner's label builder rather than spliced
// into the finished string. A splice at the last "]" cannot tell "the marker is
// absent" from "the marker is already there", so it double-stamps
// (`[] COVERING COVERING`), and it silently relocates if the label's bracket
// ever moves.
func (p *RecordQueryCoveringIndexPlan) Explain() string {
	return p.indexPlan.explainWithCovering()
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
func (p *RecordQueryCoveringIndexPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryCoveringIndexPlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
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

// --- Delegating accessors -------------------------------------------------
//
// A covering scan reads the SAME physical index range as its inner; only the
// row it emits differs (a partial record rebuilt from the entry, rather than
// the base record). Everything that describes the RANGE therefore delegates.
//
// Each one here answers a LIVE consumer that reaches this plan type — the
// baked-reference walk, the ordering derivations, the primary-key property.
//
// GetFlowedType and IsUnique were briefly DELETED on the argument that they had
// no such consumer. That argument was wrong in a way worth recording, because
// the deletion of a delegator cannot fail loudly. Both names are reachable
// through ANONYMOUS-INTERFACE probes — `x.(interface{ IsUnique() bool })` — and
// when a probe stops matching there is no compiler error and no panic: the
// assertion simply returns false, which reads as "this plan is not unique"
// rather than as "nobody asked the right type". The tree has two such probes
// today (abstract_data_access_rule.go, in candidateScanProps and
// candidateUnique). Their receiver is a MatchCandidate, which a plan cannot be,
// so those two specifically could not have broken — but establishing that took
// a grep, and the next probe added does not come with one.
//
// So both are restored, and the standing objection to an unconsumed delegator
// is answered by giving them a consumer: TestCoveringPlanDelegatesUniqueAndFlowedType
// pins that each returns the wrapped scan's answer, so the delegation is a
// tested claim rather than a surface nobody exercises.

// GetScanComparisons delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetScanComparisons() []*predicates.ComparisonRange {
	return p.indexPlan.GetScanComparisons()
}

// GetKeyComponentTypes delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetKeyComponentTypes() []values.Type {
	return p.indexPlan.GetKeyComponentTypes()
}

// GetPrimaryKeyComponentTypes delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetPrimaryKeyComponentTypes() []values.Type {
	return p.indexPlan.GetPrimaryKeyComponentTypes()
}

// GetRecordTypes delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetRecordTypes() []string {
	return p.indexPlan.GetRecordTypes()
}

// GetFlowedType delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetFlowedType() values.Type {
	return p.indexPlan.GetFlowedType()
}

// IsUnique delegates to the inner scan: uniqueness is a property of the INDEX,
// and a covering scan reads the same index.
func (p *RecordQueryCoveringIndexPlan) IsUnique() bool { return p.indexPlan.IsUnique() }

// GetColumnNames delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetColumnNames() []string {
	return p.indexPlan.GetColumnNames()
}

// GetPKColumnNames delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetPKColumnNames() []string {
	return p.indexPlan.GetPKColumnNames()
}

// GetCommonPrimaryKeyValues delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) GetCommonPrimaryKeyValues() []values.Value {
	return p.indexPlan.GetCommonPrimaryKeyValues()
}

// --- Cost and ordering hints ----------------------------------------------
//
// Registered in hint_contracts.go. All four delegate: the covering scan's
// physical work IS the inner's range read, and it emits one row per entry in
// the same order. The base-record fetch that a non-covering access still pays
// is priced on the separate Fetch node above, exactly as before — so a plan
// unchanged in substance by RFC-220 is unchanged in cost.

// inner returns the wrapped scan, tolerating a TYPED-NIL receiver. The hint
// contracts are enumerated by typed nils (hint_contracts.go's
// CostedPlanPrototypes and the parity tests that drive it), so every hint
// implementation must answer on a nil receiver; the inner scan's own hints are
// nil-tolerant for the same reason.
func (p *RecordQueryCoveringIndexPlan) inner() *RecordQueryIndexPlan {
	if p == nil {
		return nil
	}
	return p.indexPlan
}

// HintCost delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) HintCost(children []properties.Cost, stats properties.StatisticsProvider) properties.Cost {
	return p.inner().HintCost(children, stats)
}

// ProvenCardinalities delegates to the inner scan.
func (p *RecordQueryCoveringIndexPlan) ProvenCardinalities(children []properties.Cardinalities) properties.Cardinalities {
	return p.inner().ProvenCardinalities(children)
}

// HintOrdering delegates to the inner scan. Without this the covering scan
// would derive NO ordering, RemoveSortRule could not fire above it, and every
// order-satisfying index access would sprout an in-memory sort.
func (p *RecordQueryCoveringIndexPlan) HintOrdering() properties.Ordering {
	return p.inner().HintOrdering()
}

// HintRichOrdering delegates to the inner scan, for the same reason as
// HintOrdering — and additionally because the rich form is what carries the
// equality-prefix bindings a sort-elimination match needs.
func (p *RecordQueryCoveringIndexPlan) HintRichOrdering() *properties.RichOrdering {
	return p.inner().HintRichOrdering()
}
