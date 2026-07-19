package plans

import (
	"fmt"
	"hash/fnv"
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

func (p *RecordQueryRecursiveLevelUnionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryRecursiveLevelUnionPlan)
	if !ok {
		return false
	}
	return p.tempTableScanAlias == o.tempTableScanAlias &&
		p.tempTableInsertAlias == o.tempTableInsertAlias &&
		p.distinct == o.distinct
}

func (p *RecordQueryRecursiveLevelUnionPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("recursivelevel|"))
	h.Write([]byte(p.tempTableScanAlias.Name()))
	h.Write([]byte("|"))
	h.Write([]byte(p.tempTableInsertAlias.Name()))
	if p.distinct {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
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

// CanCorrelate reports that this operator anchors a correlation between its
// children (each level binds what the next level reads), mirroring physicalRecursiveLevelUnionWrapper.
func (p *RecordQueryRecursiveLevelUnionPlan) CanCorrelate() bool { return true }
