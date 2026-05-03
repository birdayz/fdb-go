package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ExpressionPartition holds a subset of a Reference's expressions that
// share some common property. Used by ImplementationRules to reason
// about groups of expressions with shared characteristics.
//
// Ports Java's ExpressionPartition (simplified — we defer the full
// ExpressionProperty/ExpressionPropertiesMap system until a rule
// actually needs property-based partitioning).
type ExpressionPartition struct {
	exprs []expressions.RelationalExpression
}

// NewExpressionPartition creates a partition from expressions.
func NewExpressionPartition(exprs []expressions.RelationalExpression) *ExpressionPartition {
	return &ExpressionPartition{exprs: exprs}
}

// GetExpressions returns the partition's expressions.
func (p *ExpressionPartition) GetExpressions() []expressions.RelationalExpression {
	return p.exprs
}

// PlanPartition is an ExpressionPartition specialized for
// RecordQueryPlans. Used by ImplementationRules that match over
// physical plans in a child Reference's final members.
//
// Ports Java's PlanPartition.
type PlanPartition struct {
	plans []plans.RecordQueryPlan
}

// NewPlanPartition creates a partition from plans.
func NewPlanPartition(ps []plans.RecordQueryPlan) *PlanPartition {
	return &PlanPartition{plans: ps}
}

// GetPlans returns the partition's plans.
func (p *PlanPartition) GetPlans() []plans.RecordQueryPlan {
	return p.plans
}

// PlanPartitionFromReference extracts all physical plans from a
// Reference's final members (falling back to all members if no finals
// exist) and wraps them in a PlanPartition.
func PlanPartitionFromReference(ref *expressions.Reference) *PlanPartition {
	if ref == nil {
		return &PlanPartition{}
	}
	members := ref.FinalMembers()
	if len(members) == 0 {
		members = ref.AllMembers()
	}
	var ps []plans.RecordQueryPlan
	for _, m := range members {
		if ph, ok := m.(physicalPlanExpression); ok {
			ps = append(ps, ph.GetRecordQueryPlan())
		}
	}
	return &PlanPartition{plans: ps}
}
