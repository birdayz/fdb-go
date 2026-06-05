package cascades

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementUnorderedUnionRule implements LogicalUnionExpression as a
// RecordQueryUnorderedUnionPlan. It extracts physical plans from each
// child Reference's plan partitions and creates a concatenating union
// plan over them.
//
// Ports Java's ImplementUnorderedUnionRule.
type ImplementUnorderedUnionRule struct {
	matcher matching.BindingMatcher
}

func NewImplementUnorderedUnionRule() *ImplementUnorderedUnionRule {
	return &ImplementUnorderedUnionRule{
		matcher: &logicalUnionMatcher{},
	}
}

func (r *ImplementUnorderedUnionRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementUnorderedUnionRule) OnMatch(call *ImplementationRuleCall) {
	expr := call.Bindings.Get(r.matcher).(*expressions.LogicalUnionExpression)

	quantifiers := expr.GetQuantifiers()
	if len(quantifiers) == 0 {
		return
	}

	childPartitions := make([][]*PlanPartition, len(quantifiers))
	for i, q := range quantifiers {
		ref := q.GetRangesOver()
		if ref == nil {
			return
		}
		parts := ToPlanPartitions(ref)
		rolled := RollUpPlanPartitions(parts)
		if len(rolled) == 0 {
			return
		}
		childPartitions[i] = rolled
	}

	for _, partitions := range crossProductPartitions(childPartitions) {
		var childPlans []plans.RecordQueryPlan
		var newQuantifiers []expressions.Quantifier

		for i, partition := range partitions {
			planExprs := partition.GetExpressions()
			if len(planExprs) == 0 {
				continue
			}

			newRef := call.MemoizeFinalExpressionsFromOther(
				quantifiers[i].GetRangesOver(),
				planExprs,
			)
			newQuantifiers = append(newQuantifiers,
				expressions.NewPhysicalQuantifier(newRef))

			if ph, ok := planExprs[0].(physicalPlanExpression); ok {
				childPlans = append(childPlans, ph.GetRecordQueryPlan())
			}
		}

		if len(childPlans) < 2 {
			continue
		}

		// SQL standard: UNION result uses the first branch's column
		// names. Wrap non-first branches with a MapPlan that renames
		// columns when they differ. This is the Cascades-native
		// approach — column renaming is a plan-level operation, not
		// an executor band-aid.
		firstCols := physicalPlanColumnNames(childPlans[0])
		if len(firstCols) > 0 {
			for i := 1; i < len(childPlans); i++ {
				branchCols := physicalPlanColumnNames(childPlans[i])
				if len(branchCols) == len(firstCols) && !colNamesEqual(branchCols, firstCols) {
					childPlans[i] = plans.NewRecordQueryMapPlan(
						childPlans[i],
						columnRenameValue(branchCols, firstCols),
					)
				}
			}
		}

		unionPlan := plans.NewRecordQueryUnorderedUnionPlan(childPlans)
		wrapper := NewPhysicalUnorderedUnionWrapper(unionPlan, newQuantifiers)
		call.YieldFinalExpression(wrapper)
	}
}

var _ ImplementationRule = (*ImplementUnorderedUnionRule)(nil)

type logicalUnionMatcher struct{}

func (m *logicalUnionMatcher) RootType() string { return "LogicalUnionExpression" }

func (m *logicalUnionMatcher) BindMatches(outer *matching.PlannerBindings, in any) []*matching.PlannerBindings {
	if _, ok := in.(*expressions.LogicalUnionExpression); !ok {
		return nil
	}
	return []*matching.PlannerBindings{outer.Bind(m, in)}
}

// crossProductPartitions returns the Cartesian product of per-child
// partition lists. Delegates to the generic CrossProduct.
func crossProductPartitions(childPartitions [][]*PlanPartition) [][]*PlanPartition {
	return CrossProduct(childPartitions)
}

// physicalPlanColumnNames extracts column names from a physical plan
// by unwrapping through inner plans to find a ProjectionPlan or
// MapPlan with extractable column info. Returns nil when names can't
// be determined.
func physicalPlanColumnNames(p plans.RecordQueryPlan) []string {
	type inner interface{ GetInner() plans.RecordQueryPlan }
	for {
		if proj, ok := p.(*plans.RecordQueryProjectionPlan); ok {
			projs := proj.GetProjections()
			names := make([]string, len(projs))
			aliases := proj.GetAliases()
			for i, v := range projs {
				if i < len(aliases) && aliases[i] != "" {
					names[i] = strings.ToUpper(aliases[i])
				} else {
					names[i] = unionProjectionColumnName(v)
				}
			}
			return names
		}
		if mp, ok := p.(*plans.RecordQueryMapPlan); ok {
			if rv := mp.GetResultValue(); rv != nil {
				if rcv, ok := rv.(*values.RecordConstructorValue); ok {
					names := make([]string, len(rcv.Fields))
					for i, f := range rcv.Fields {
						names[i] = strings.ToUpper(f.Name)
					}
					return names
				}
			}
		}
		// A StreamingAgg defines its OWN output schema (group keys + aggregate outputs);
		// do NOT unwrap through GetInner() to the pre-aggregation input column names — those
		// are NOT the aggregate's output names, and renaming a later union branch to them
		// would read columns absent from the aggregate row → NULLs (codex, RFC-080). Return
		// nil so no plan-time rename Map is inserted for this branch; the executor's
		// position-remap (executeUnorderedUnion → planColumnNamesWithMD, which DOES report a
		// StreamingAgg's output schema, RFC-078) normalizes it at runtime instead.
		if _, ok := p.(*plans.RecordQueryStreamingAggregationPlan); ok {
			return nil
		}
		if ip, ok := p.(inner); ok {
			p = ip.GetInner()
		} else {
			break
		}
	}
	if rt, ok := p.GetResultType().(*values.RecordType); ok && len(rt.Fields) > 0 {
		names := make([]string, len(rt.Fields))
		for i, f := range rt.Fields {
			names[i] = strings.ToUpper(f.Name)
		}
		return names
	}
	return nil
}

func unionProjectionColumnName(v values.Value) string {
	if fv, ok := v.(*values.FieldValue); ok {
		return strings.ToUpper(fv.Field)
	}
	return strings.ToUpper(values.ExplainValue(v))
}

func colNamesEqual(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// columnRenameValue builds a RecordConstructorValue that renames
// columns positionally from src to dst names. When evaluated against
// a datum map, each field reads src[i] and writes to dst[i].
func columnRenameValue(srcCols, dstCols []string) *values.RecordConstructorValue {
	fields := make([]values.RecordConstructorField, len(dstCols))
	for i := range dstCols {
		fields[i] = values.RecordConstructorField{
			Name:  dstCols[i],
			Value: &values.FieldValue{Field: srcCols[i]},
		}
	}
	return values.NewRecordConstructorValue(fields...)
}
