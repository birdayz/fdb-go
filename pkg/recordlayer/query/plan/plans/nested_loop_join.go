package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
	outer      RecordQueryPlan
	inner      RecordQueryPlan
	predicates []predicates.QueryPredicate
	joinType   JoinType
	outerAlias string
	innerAlias string
	// sqlColumnOrderReversed is true when the physical outer/inner
	// assignment was swapped relative to the SQL FROM-clause order.
	// When true, SELECT * should emit inner columns before outer
	// columns to match SQL semantics.
	sqlColumnOrderReversed bool
}

// JoinType distinguishes inner vs outer vs cross joins.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeftOuter
	JoinCross
	JoinExists    // semi-join for EXISTS subqueries
	JoinNotExists // anti semi-join for NOT EXISTS subqueries
)

func (jt JoinType) String() string {
	switch jt {
	case JoinInner:
		return "INNER"
	case JoinLeftOuter:
		return "LEFT OUTER"
	case JoinCross:
		return "CROSS"
	case JoinExists:
		return "EXISTS"
	case JoinNotExists:
		return "NOT EXISTS"
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
) *RecordQueryNestedLoopJoinPlan {
	preds := make([]predicates.QueryPredicate, len(joinPredicates))
	copy(preds, joinPredicates)
	return &RecordQueryNestedLoopJoinPlan{
		outer:      outer,
		inner:      inner,
		predicates: preds,
		joinType:   joinType,
		outerAlias: outerAlias,
		innerAlias: innerAlias,
	}
}

func (p *RecordQueryNestedLoopJoinPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryNestedLoopJoinPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.outer, p.inner}
}

func (p *RecordQueryNestedLoopJoinPlan) GetOuter() RecordQueryPlan { return p.outer }
func (p *RecordQueryNestedLoopJoinPlan) GetInner() RecordQueryPlan { return p.inner }
func (p *RecordQueryNestedLoopJoinPlan) GetJoinType() JoinType     { return p.joinType }
func (p *RecordQueryNestedLoopJoinPlan) GetOuterAlias() string     { return p.outerAlias }
func (p *RecordQueryNestedLoopJoinPlan) GetInnerAlias() string     { return p.innerAlias }

// IsSQLColumnOrderReversed reports whether the physical outer/inner
// assignment was swapped relative to the SQL FROM-clause order.
func (p *RecordQueryNestedLoopJoinPlan) IsSQLColumnOrderReversed() bool {
	return p.sqlColumnOrderReversed
}

// SetSQLColumnOrderReversed marks the plan as having reversed column order.
func (p *RecordQueryNestedLoopJoinPlan) SetSQLColumnOrderReversed(v bool) {
	p.sqlColumnOrderReversed = v
}

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
