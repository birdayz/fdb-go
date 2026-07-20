package plans

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryNestedLoopJoinPlan represents a nested-loop join of two
// child plans. For each row in the outer (left) plan, the inner (right)
// plan is evaluated and the join predicate is applied to the combined
// row. This is the simplest and most general join strategy — it handles
// all join types (inner, left, cross) without requiring ordered input.
//
// Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryFlatMapPlan`
// which is the underlying implementation of nested-loop joins in the
// Record Layer.
//
// The two legs are stored ONCE, as Quantifiers over References — Java's shape
// (`RecordQueryFlatMapPlan`'s outer/inner `Quantifier.Physical`). The raw
// `outer`/`inner` pointers they replace were a second storage location for the
// same edges. They stay two separately-named fields rather than a slice
// because the accessors and the join predicates address them by ROLE — the
// outer is the driving side — not by position. RFC-183 P5 step 2.
type RecordQueryNestedLoopJoinPlan struct {
	PlanExprBase
	outerQ      expressions.Quantifier
	innerQ      expressions.Quantifier
	predicates  []predicates.QueryPredicate
	joinType    JoinType
	outerAlias  string
	innerAlias  string
	resultValue values.Value
}

// JoinType distinguishes inner vs outer vs cross joins.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeftOuter
	JoinCross
	// Slots 3 and 4 were JoinExists / JoinNotExists, removed in RFC-141 Phase 2:
	// EXISTS is no longer a fused join mode — the existential semi-join is emergent
	// (FirstOrDefault-wrapped inner + a separate IS-NOT-NULL filter, matching Java).
	// The slots are kept blank so the subsequent iota values stay stable.
	_
	_
	// JoinFullOuter — FULL OUTER JOIN: every left row (matched or
	// NULL-padded right) plus every right row that matched no left row
	// (NULL-padded left). Go-only query extension; Java's SQL layer has
	// no outer joins. Appended (not inserted) to keep prior iota values
	// stable. Implemented only by the materialized nested-loop cursor,
	// never the correlated FlatMap path (which cannot observe global
	// inner-match state).
	JoinFullOuter
)

func (jt JoinType) String() string {
	switch jt {
	case JoinInner:
		return "INNER"
	case JoinLeftOuter:
		return "LEFT OUTER"
	case JoinCross:
		return "CROSS"
	case JoinFullOuter:
		return "FULL OUTER"
	}
	return "UNKNOWN"
}

// NewRecordQueryNestedLoopJoinPlan constructs a nested-loop join plan.
// outerAlias/innerAlias are the SQL-level table aliases (e.g. "E", "M"
// for `FROM Employee AS e, Employee AS m`). Used by the executor to
// qualify merged-row keys so predicates can resolve alias-qualified
// column references.
func NewRecordQueryNestedLoopJoinPlan(
	outer, inner RecordQueryPlan,
	joinPredicates []predicates.QueryPredicate,
	joinType JoinType,
	outerAlias, innerAlias string,
	resultValue values.Value,
) *RecordQueryNestedLoopJoinPlan {
	preds := make([]predicates.QueryPredicate, len(joinPredicates))
	copy(preds, joinPredicates)
	return &RecordQueryNestedLoopJoinPlan{
		outerQ:      QuantifierOverPlan(outer),
		innerQ:      QuantifierOverPlan(inner),
		predicates:  preds,
		joinType:    joinType,
		outerAlias:  outerAlias,
		innerAlias:  innerAlias,
		resultValue: resultValue,
	}
}

func (p *RecordQueryNestedLoopJoinPlan) GetResultType() values.Type { return values.UnknownType }

// GetChildren returns the outer leg then the inner leg, dereferenced through
// the quantifiers. The pair is always two entries wide — a nil leg stays a nil
// entry rather than shrinking the arity.
func (p *RecordQueryNestedLoopJoinPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.GetOuter(), p.GetInner()}
}

// GetQuantifiers reports the real leg quantifiers in GetChildren order
// (outer, inner), overriding PlanExprBase's none. That order is what
// WithQuantifiers indexes into.
func (p *RecordQueryNestedLoopJoinPlan) GetQuantifiers() []expressions.Quantifier {
	if p.outerQ.GetRangesOver() == nil || p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.outerQ, p.innerQ}
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers, in
// GetQuantifiers order. The receiver is never mutated, which is what keeps a
// memoized plan safe to share.
func (p *RecordQueryNestedLoopJoinPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 2 {
		return p
	}
	cp := *p
	cp.outerQ = qs[0]
	cp.innerQ = qs[1]
	return &cp
}

func (p *RecordQueryNestedLoopJoinPlan) GetOuter() RecordQueryPlan {
	return planFromQuantifier(p.outerQ)
}

func (p *RecordQueryNestedLoopJoinPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}
func (p *RecordQueryNestedLoopJoinPlan) GetJoinType() JoinType { return p.joinType }
func (p *RecordQueryNestedLoopJoinPlan) GetOuterAlias() string { return p.outerAlias }
func (p *RecordQueryNestedLoopJoinPlan) GetInnerAlias() string { return p.innerAlias }
func (p *RecordQueryNestedLoopJoinPlan) GetResultValue() values.Value {
	return p.resultValue
}

func (p *RecordQueryNestedLoopJoinPlan) GetPredicates() []predicates.QueryPredicate {
	return p.predicates
}

// structuralKey lists the fields that distinguish this join in the memo: the
// join type, the outer/inner table aliases, the join predicate list, and the
// result Value. Children (the two legs) are excluded. The outer/inner aliases
// are SQL-level strings (not correlation identifiers) — they qualify merged-row
// keys, so two joins differing only in an alias resolve columns differently.
// The same key drives both EqualsPlanWithoutChildren and
// HashCodeWithoutChildren, so the two can never disagree on which fields matter.
func (p *RecordQueryNestedLoopJoinPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Int(int(p.joinType)).
		Str(p.outerAlias).
		Str(p.innerAlias).
		Preds(p.predicates).
		Value(p.resultValue)
}

func (p *RecordQueryNestedLoopJoinPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryNestedLoopJoinPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

// HashCodeWithoutChildren folds the structural discriminators. Predicates
// fold predicates.SemanticHashCode (alias-invariant, coarser than the
// structural PredicateEquals — equal⟹same-hash holds), NOT Explain()
// display text, which is for humans and carries no identity contract. The
// resultValue joins identity — the Java counterpart (RecordQueryFlatMapPlan)
// compares via semanticEqualsForResults; two joins differing only in the
// combined-row shape they emit are not interchangeable.
func (p *RecordQueryNestedLoopJoinPlan) HashCodeWithoutChildren() uint64 {
	return p.structuralKey().Hash("nljoin|")
}

func (p *RecordQueryNestedLoopJoinPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString("NestedLoopJoin(")
	sb.WriteString(p.joinType.String())
	if len(p.predicates) > 0 {
		sb.WriteString(fmt.Sprintf(", [%d preds]", len(p.predicates)))
	}
	sb.WriteString(", ")
	if outer := p.GetOuter(); outer != nil {
		sb.WriteString(outer.Explain())
	}
	sb.WriteString(", ")
	if inner := p.GetInner(); inner != nil {
		sb.WriteString(inner.Explain())
	}
	sb.WriteString(")")
	return sb.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryNestedLoopJoinPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryNestedLoopJoinPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryNestedLoopJoinPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// GetCorrelatedToWithoutChildren walks this plan's own predicates, mirroring
// physicalNestedLoopJoinWrapper. The predicates are this node's information — a
// correlation reached only through them would be invisible to
// correlation-driven rules if this returned the empty default.
func (p *RecordQueryNestedLoopJoinPlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, pred := range p.GetPredicates() {
		for k := range predicates.GetCorrelatedToOfPredicate(pred) {
			out[k] = struct{}{}
		}
	}
	return out
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryNestedLoopJoinPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
