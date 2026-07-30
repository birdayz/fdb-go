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
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
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
// outerAlias/innerAlias identify the two legs of the merged row this join emits:
// the executor qualifies merged-row keys by them and stamps them onto the row's
// leg boundaries, so they are what an alias-qualified column reference resolves
// through.
//
// They are CorrelationIdentifiers, not strings, because that is what they
// identify — a quantifier — and because the executor's leg boundaries compare
// them through values.SameLeg. Holding them as text meant the executor minted an
// identifier from a string at the plan boundary, and an exact comparison cannot
// protect against a forgery its own mint constructs: the mint decides the
// spelling, so the case-disjointness that keeps a quoted "Q$5" from binding a
// planner-minted q$5 was being re-decided at every consumer. Java holds the same
// thing typed end to end (RecordQueryFlatMapPlan carries Quantifier.Physical,
// not an alias string), and Go's own RecordQueryFlatMapPlan already did.
func NewRecordQueryNestedLoopJoinPlan(
	outer, inner RecordQueryPlan,
	joinPredicates []predicates.QueryPredicate,
	joinType JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
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

// NewRecordQueryNestedLoopJoinPlanFromQuantifiers builds a nested-loop join whose
// two legs are supplied memo quantifiers instead of snapshots over concrete
// plans. This makes the plan its own cascades expression carrying its child
// edges directly — the memo holds it without a physicalNestedLoopJoinWrapper
// (RFC-184 W2). The materialized NLJ is uncorrelated (CanCorrelate=false), so
// both legs carry the LIVE shared-group edge the emitter memoized; the join
// predicates, join type, table aliases and result value are preserved so
// EqualsPlanWithoutChildren / GetCorrelatedToWithoutChildren stay identical.
func NewRecordQueryNestedLoopJoinPlanFromQuantifiers(
	outerQ, innerQ expressions.Quantifier,
	joinPredicates []predicates.QueryPredicate,
	joinType JoinType,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
) *RecordQueryNestedLoopJoinPlan {
	preds := make([]predicates.QueryPredicate, len(joinPredicates))
	copy(preds, joinPredicates)
	return &RecordQueryNestedLoopJoinPlan{
		outerQ:      outerQ,
		innerQ:      innerQ,
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
func (p *RecordQueryNestedLoopJoinPlan) GetOuterAlias() values.CorrelationIdentifier {
	return p.outerAlias
}

func (p *RecordQueryNestedLoopJoinPlan) GetInnerAlias() values.CorrelationIdentifier {
	return p.innerAlias
}

func (p *RecordQueryNestedLoopJoinPlan) GetResultValue() values.Value {
	return p.resultValue
}

func (p *RecordQueryNestedLoopJoinPlan) GetPredicates() []predicates.QueryPredicate {
	return p.predicates
}

// structuralKey lists the fields that distinguish this join in the memo: the
// join type, the outer/inner leg identifiers, the join predicate list, and the
// result Value. Children (the two legs) are excluded. The leg identifiers
// qualify merged-row keys, so two joins differing only in one resolve columns
// differently. They enter the key as their raw Name(): a CorrelationIdentifier's
// equality IS equality of that string, so the key is unchanged by their retyping
// and every memo identity and plan hash it feeds stays byte-identical.
// The same key drives both EqualsPlanWithoutChildren and
// HashCodeWithoutChildren, so the two can never disagree on which fields matter.
func (p *RecordQueryNestedLoopJoinPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Int(int(p.joinType)).
		Str(p.outerAlias.Name()).
		Str(p.innerAlias.Name()).
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

// WithChildren is the extraction/relink hook (plan_extraction.go's WithChildren
// interface). The join carries its two legs as memo quantifiers, so the relink
// is a positional quantifier swap: WithQuantifiers copies the receiver
// (preserving the predicates, join type, table aliases and result value) and
// re-resolves GetOuter/GetInner through the new references. This replaces
// physicalNestedLoopJoinWrapper.WithChildren (RFC-184 W2), whose separate
// snapshot plan field held the yield-time children verbatim; the swap
// re-resolves to the memo winner instead.
func (p *RecordQueryNestedLoopJoinPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 2 {
		return nil, fmt.Errorf("RecordQueryNestedLoopJoinPlan.WithChildren: expected 2 children, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryNestedLoopJoinPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
