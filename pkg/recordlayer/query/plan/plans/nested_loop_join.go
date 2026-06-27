package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

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
type RecordQueryNestedLoopJoinPlan struct {
	outer       RecordQueryPlan
	inner       RecordQueryPlan
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
		outer:       outer,
		inner:       inner,
		predicates:  preds,
		joinType:    joinType,
		outerAlias:  outerAlias,
		innerAlias:  innerAlias,
		resultValue: resultValue,
	}
}

func (p *RecordQueryNestedLoopJoinPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryNestedLoopJoinPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.outer, p.inner}
}

func (p *RecordQueryNestedLoopJoinPlan) GetOuter() RecordQueryPlan    { return p.outer }
func (p *RecordQueryNestedLoopJoinPlan) GetInner() RecordQueryPlan    { return p.inner }
func (p *RecordQueryNestedLoopJoinPlan) GetJoinType() JoinType        { return p.joinType }
func (p *RecordQueryNestedLoopJoinPlan) GetOuterAlias() string        { return p.outerAlias }
func (p *RecordQueryNestedLoopJoinPlan) GetInnerAlias() string        { return p.innerAlias }
func (p *RecordQueryNestedLoopJoinPlan) GetResultValue() values.Value { return p.resultValue }

func (p *RecordQueryNestedLoopJoinPlan) GetPredicates() []predicates.QueryPredicate {
	return p.predicates
}

func (p *RecordQueryNestedLoopJoinPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryNestedLoopJoinPlan)
	if !ok {
		return false
	}
	if p.joinType != o.joinType {
		return false
	}
	if p.outerAlias != o.outerAlias || p.innerAlias != o.innerAlias {
		return false
	}
	if len(p.predicates) != len(o.predicates) {
		return false
	}
	for i := range p.predicates {
		if !predicates.PredicateEquals(p.predicates[i], o.predicates[i]) {
			return false
		}
	}
	return true
}

func (p *RecordQueryNestedLoopJoinPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("nljoin|"))
	h.Write([]byte{byte(p.joinType)})
	h.Write([]byte(p.outerAlias))
	h.Write([]byte{0})
	h.Write([]byte(p.innerAlias))
	h.Write([]byte{0})
	for _, pred := range p.predicates {
		h.Write([]byte(pred.Explain()))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (p *RecordQueryNestedLoopJoinPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString("NestedLoopJoin(")
	sb.WriteString(p.joinType.String())
	if len(p.predicates) > 0 {
		sb.WriteString(fmt.Sprintf(", [%d preds]", len(p.predicates)))
	}
	sb.WriteString(", ")
	if p.outer != nil {
		sb.WriteString(p.outer.Explain())
	}
	sb.WriteString(", ")
	if p.inner != nil {
		sb.WriteString(p.inner.Explain())
	}
	sb.WriteString(")")
	return sb.String()
}

var _ RecordQueryPlan = (*RecordQueryNestedLoopJoinPlan)(nil)
