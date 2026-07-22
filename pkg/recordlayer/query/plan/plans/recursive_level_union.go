package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryRecursiveLevelUnionPlan implements a recursive
// level-order (breadth-first) union: the initial-state plan seeds the
// first level, and the recursive-state plan is re-evaluated for each
// level using two temp tables (scan/insert) that are flipped between
// levels. Mirrors Java's RecordQueryRecursiveLevelUnionPlan.
//
// The two legs are stored ONCE, as Quantifiers over References — Java's shape
// (`Quantifier.Physical` for the initial and recursive states). The raw
// `initialState`/`recursiveState` pointers they replace were a second storage
// location for the same edges. They stay two separately-named fields rather
// than a slice because the legs are not interchangeable: one seeds level zero,
// the other is re-run per level against the flipped temp tables. RFC-183 P5
// step 2.
type RecordQueryRecursiveLevelUnionPlan struct {
	PlanExprBase
	initialQ             expressions.Quantifier
	recursiveQ           expressions.Quantifier
	tempTableScanAlias   values.CorrelationIdentifier
	tempTableInsertAlias values.CorrelationIdentifier
	distinct             bool // UNION DISTINCT deduplication for cycle detection
}

func NewRecordQueryRecursiveLevelUnionPlan(
	initialState, recursiveState RecordQueryPlan,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
) *RecordQueryRecursiveLevelUnionPlan {
	return &RecordQueryRecursiveLevelUnionPlan{
		initialQ:             QuantifierOverPlan(initialState),
		recursiveQ:           QuantifierOverPlan(recursiveState),
		tempTableScanAlias:   tempTableScanAlias,
		tempTableInsertAlias: tempTableInsertAlias,
	}
}

// NewRecordQueryRecursiveLevelUnionPlanDistinct creates a plan with
// UNION DISTINCT deduplication.
func NewRecordQueryRecursiveLevelUnionPlanDistinct(
	initialState, recursiveState RecordQueryPlan,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
) *RecordQueryRecursiveLevelUnionPlan {
	return &RecordQueryRecursiveLevelUnionPlan{
		initialQ:             QuantifierOverPlan(initialState),
		recursiveQ:           QuantifierOverPlan(recursiveState),
		tempTableScanAlias:   tempTableScanAlias,
		tempTableInsertAlias: tempTableInsertAlias,
		distinct:             true,
	}
}

// NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers builds a recursive level
// union whose two legs are LIVE memo quantifiers (the implementation rule passes
// ForEachQuantifiers over the freshly-memoized initial/recursive winners) instead
// of snapshots over plans. This makes the plan its own cascades expression
// carrying its leg edges directly — the memo holds it without a physical wrapper
// (RFC-184 W2). The temp-table aliases and distinct flag carry over verbatim.
func NewRecordQueryRecursiveLevelUnionPlanFromQuantifiers(
	initialQ, recursiveQ expressions.Quantifier,
	tempTableScanAlias, tempTableInsertAlias values.CorrelationIdentifier,
	distinct bool,
) *RecordQueryRecursiveLevelUnionPlan {
	return &RecordQueryRecursiveLevelUnionPlan{
		initialQ:             initialQ,
		recursiveQ:           recursiveQ,
		tempTableScanAlias:   tempTableScanAlias,
		tempTableInsertAlias: tempTableInsertAlias,
		distinct:             distinct,
	}
}

func (p *RecordQueryRecursiveLevelUnionPlan) IsDistinct() bool { return p.distinct }

func (p *RecordQueryRecursiveLevelUnionPlan) GetInitialState() RecordQueryPlan {
	return planFromQuantifier(p.initialQ)
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetRecursiveState() RecordQueryPlan {
	return planFromQuantifier(p.recursiveQ)
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetTempTableScanAlias() values.CorrelationIdentifier {
	return p.tempTableScanAlias
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetTempTableInsertAlias() values.CorrelationIdentifier {
	return p.tempTableInsertAlias
}

func (p *RecordQueryRecursiveLevelUnionPlan) GetResultType() values.Type { return values.UnknownType }

// GetChildren returns the initial-state leg then the recursive-state leg,
// dereferenced through the quantifiers. The pair is always two entries wide —
// a nil leg stays a nil entry rather than shrinking the arity.
func (p *RecordQueryRecursiveLevelUnionPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.GetInitialState(), p.GetRecursiveState()}
}

// GetQuantifiers reports the real leg quantifiers in GetChildren order
// (initial, recursive), overriding PlanExprBase's none. That order is what
// WithQuantifiers indexes into.
func (p *RecordQueryRecursiveLevelUnionPlan) GetQuantifiers() []expressions.Quantifier {
	if p.initialQ.GetRangesOver() == nil || p.recursiveQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.initialQ, p.recursiveQ}
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers, in
// GetQuantifiers order. The receiver is never mutated, which is what keeps a
// memoized plan safe to share.
func (p *RecordQueryRecursiveLevelUnionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 2 {
		return p
	}
	cp := *p
	cp.initialQ = qs[0]
	cp.recursiveQ = qs[1]
	return &cp
}

// structuralKey lists the fields that distinguish this recursive level union in
// the memo: the scan/insert temp-table correlation aliases and the distinct
// flag. Children (the initial and recursive legs) are excluded. The same key
// drives both EqualsPlanWithoutChildren and HashCodeWithoutChildren.
func (p *RecordQueryRecursiveLevelUnionPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Alias(p.tempTableScanAlias).
		Alias(p.tempTableInsertAlias).
		Bool(p.distinct)
}

func (p *RecordQueryRecursiveLevelUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryRecursiveLevelUnionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryRecursiveLevelUnionPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("recursivelevel|")
}

func (p *RecordQueryRecursiveLevelUnionPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString("RecursiveLevelUnion(")
	if initial := p.GetInitialState(); initial != nil {
		sb.WriteString(initial.Explain())
	}
	sb.WriteString(", ")
	if recursive := p.GetRecursiveState(); recursive != nil {
		sb.WriteString(recursive.Explain())
	}
	sb.WriteString(fmt.Sprintf(", scan=%s, insert=%s)", p.tempTableScanAlias.Name(), p.tempTableInsertAlias.Name()))
	return sb.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryRecursiveLevelUnionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryRecursiveLevelUnionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryRecursiveLevelUnionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// CanCorrelate matches Java's RecordQueryRecursiveLevelUnionPlan, which has NO
// canCorrelate override and so answers false (the default). The recursion's
// level-to-level binding is NOT a Cascades correlation anchored here: Java
// satisfies the temp-table scan/insert aliases explicitly in
// computeCorrelatedTo (filtered out), and the cursor (RecursiveUnionCursor)
// carries the per-level temp-table flip at execution — nothing in the memo
// binds a sibling leg's alias through this operator. Answering true would
// (via Reference.GetCorrelatedTo) SUPPRESS propagation of an outer alias a
// leg legitimately reads — a wrong-rows shape when a recursive CTE sits on the
// inner side of a lateral correlation and Go's human-readable alias reuse
// collides an outer alias with a leg's own. The sibling
// RecordQueryRecursiveDfsJoinPlan DOES override to true (Java parity) — the
// divergence is specific to the LEVEL union.
func (p *RecordQueryRecursiveLevelUnionPlan) CanCorrelate() bool { return false }

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The plan carries its two legs as LIVE memo edges, so the relink is a
// quantifier swap: WithQuantifiers rebinds the legs and GetChildren re-resolves
// through the new references (RFC-184 W2, replacing
// physicalRecursiveLevelUnionWrapper.WithChildren).
func (p *RecordQueryRecursiveLevelUnionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("RecordQueryRecursiveLevelUnionPlan.WithChildren: expected 2 children, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryRecursiveLevelUnionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
