package cascades

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
		matcher: NewExpressionMatcher[*expressions.LogicalUnionExpression]("implement_unordered_union"),
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
		childPlans := make([]plans.RecordQueryPlan, 0, len(partitions))
		newQuantifiers := make([]expressions.Quantifier, 0, len(partitions))

		// A union carries EVERY leg or it is not this union. A leg whose
		// partition holds no expression cannot be represented, and skipping it
		// would emit a narrower union that silently drops that leg's rows —
		// so the whole combination is declined instead.
		legMissing := false
		for i, partition := range partitions {
			planExprs := partition.GetExpressions()
			if len(planExprs) == 0 {
				legMissing = true
				break
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

		// Arity follows Java: RecordQueryUnorderedUnionPlan.fromQuantifiers
		// imposes no minimum, so a ONE-leg logical union implements as a
		// one-leg concat. UnionSingletonElimRule normally rewrites that shape
		// away first, but the implementation rule must not depend on another
		// rule having fired — with a Go-only two-leg floor here, a surviving
		// singleton union had no implementer at all and planning returned no
		// plan and no error.
		if legMissing || len(newQuantifiers) == 0 {
			continue
		}

		// SQL standard: UNION result uses the first branch's column
		// names. Wrap non-first branches with a MapPlan that renames
		// columns when they differ. This is the Cascades-native
		// approach — column renaming is a plan-level operation, not
		// an executor band-aid.
		// The renaming Map is a COMPENSATING operator, so the branch
		// quantifier advances with it. Without that, the Map existed only in
		// the plan and nothing in the quantifier's group could produce it —
		// the memo costed the un-renamed branch while the renamed one
		// executed (10 unreachable edges, RFC-183 §14).
		//
		// This only ADDS reachability; the group keeps every member it had.
		// Narrowing a group to the one member the plan happens to use is a
		// different and WRONG change — doing that to the IN-join rule
		// destroyed the InUnion alternative and regressed
		// `IN (…) ORDER BY id`. A group holding alternatives is the memo
		// working correctly.
		//
		// MemoizeFinalExpression, not MemoizeExpression: two branches that
		// rename to the same shape would otherwise intern together, which is
		// exactly how the recursive-DFS legs collapsed into one group.
		//
		// childPlans appends only for members that are physical while
		// newQuantifiers appends unconditionally, so the two can fall out of
		// step; renaming under a mismatched index would attach a Map to the
		// wrong branch. Skip the rename entirely rather than guess.
		if len(childPlans) == len(newQuantifiers) {
			firstCols := physicalPlanColumnNames(childPlans[0])
			if len(firstCols) > 0 {
				for i := 1; i < len(childPlans); i++ {
					branchCols := physicalPlanColumnNames(childPlans[i])
					if len(branchCols) == len(firstCols) && !colNamesEqual(branchCols, firstCols) {
						// The rename projection (Map) is its own cascades expression
						// carrying the live newQuantifiers[i] edge (RFC-184 W2).
						rename, err := columnRenameValue(childPlans[i].GetResultType(), firstCols)
						if err != nil {
							call.Fail(err)
							return
						}
						mapPlan, err := plans.NewRecordQueryMapPlanFromQuantifier(
							newQuantifiers[i], rename)
						if err != nil {
							call.Fail(err)
							return
						}
						childPlans[i] = mapPlan
						newQuantifiers[i] = expressions.NewPhysicalQuantifier(
							call.MemoizeFinalExpression(mapPlan))
					}
				}
			}
		}

		// The unordered union is its own cascades expression carrying the live
		// newQuantifiers leg edges (RFC-184 W2); each leg's per-ordering winner
		// resolves at extraction via ref.Winner(). childPlans is unused now.
		unionPlan, err := plans.NewRecordQueryUnorderedUnionPlanFromQuantifiers(newQuantifiers)
		if err != nil {
			call.Fail(err)
			return
		}
		call.YieldFinalExpression(unionPlan)
	}
}

var _ ImplementationRule = (*ImplementUnorderedUnionRule)(nil)

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
			return proj.GetOutputNames()
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
		// would read columns absent from the aggregate row → NULLs (RFC-080). Return
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

func colNamesEqual(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// columnRenameValue builds a RecordConstructorValue that renames
// columns positionally from src to dst names: field i reads the input
// row's SLOT i (the read is positional by definition of the rename, so the
// ordinal is baked at plan time) and writes to dst[i].
func columnRenameValue(srcType values.Type, dstCols []string) (*values.RecordConstructorValue, error) {
	srcRecord, ok := srcType.(*values.RecordType)
	if !ok || len(srcRecord.Fields) != len(dstCols) {
		return nil, fmt.Errorf("union rename source is %T width %d, want record width %d",
			srcType, recordTypeFieldCount(srcRecord), len(dstCols))
	}
	root, err := values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier(), srcRecord)
	if err != nil {
		return nil, err
	}
	fields := make([]values.RecordConstructorField, len(dstCols))
	for i := range dstCols {
		field, resolveErr := values.ResolveFieldOrdinals(root, []int{i})
		if resolveErr != nil {
			return nil, resolveErr
		}
		fields[i] = values.RecordConstructorField{
			Name:  dstCols[i],
			Value: field,
		}
	}
	return values.NewRecordConstructorValue(fields...), nil
}

func recordTypeFieldCount(record *values.RecordType) int {
	if record == nil {
		return -1
	}
	return len(record.Fields)
}
