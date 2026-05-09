package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ImplementDistinctFinalRule is the PLANNING-phase rule for
// LogicalDistinctExpression. Ports Java's ImplementDistinctRule
// (ImplementationCascadesRule<LogicalDistinctExpression>).
//
// When the inner plan already guarantees uniqueness — either via the
// DistinctRecordsProperty or because the projected columns cover a
// primary key / unique index — the Distinct operator is elided and
// inner plans are yielded directly. Otherwise, the inner is wrapped
// with RecordQueryDistinctPlan.
//
// This rule subsumes the former DistinctOnUniqueElimRule (which ran
// during EXPLORE). Moving the elimination check to PLANNING matches
// Java's architecture: ImplementDistinctRule is an
// ImplementationCascadesRule, not an exploration rule.
type ImplementDistinctFinalRule struct {
	matcher matching.BindingMatcher
}

func NewImplementDistinctFinalRule() *ImplementDistinctFinalRule {
	return &ImplementDistinctFinalRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct_final"),
	}
}

func (r *ImplementDistinctFinalRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctFinalRule) OnMatch(call *ImplementationRuleCall) {
	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	qs := d.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	// Check if the inner logical expression's projected columns cover
	// a unique key (PK or unique index). This is the column-coverage
	// check that Java performs via DistinctRecordsProperty on the
	// physical plan. We check at the logical level because Go's
	// DistinctRecordsProperty for ProjectionPlan propagates from child
	// (record-level), while SQL DISTINCT requires value-level uniqueness.
	innerExpr := innerRef.Get()
	if call.Context != nil && distinctEliminatedByUniqueKey(innerExpr, call.Context) {
		for _, m := range innerRef.FinalMembers() {
			if _, ok := m.(physicalPlanExpression); ok {
				call.YieldFinalExpression(m)
			}
		}
		return
	}

	for _, m := range innerRef.FinalMembers() {
		ph, ok := m.(physicalPlanExpression)
		if !ok {
			continue
		}
		distPlan := plans.NewRecordQueryDistinctPlan(ph.GetRecordQueryPlan())
		innerQ := expressions.ForEachQuantifier(expressions.InitialOf(m))
		call.YieldFinalExpression(NewPhysicalDistinctWrapper(distPlan, innerQ))
	}
}

// distinctEliminatedByUniqueKey checks whether the projected column set
// covers all columns of a primary key or unique index, making the
// DISTINCT operator redundant. Ported from DistinctOnUniqueElimRule.
func distinctEliminatedByUniqueKey(
	innerExpr expressions.RelationalExpression,
	ctx PlanContext,
) bool {
	projectedCols := collectProjectedFieldNames(innerExpr)

	recordTypes := findRecordTypes(innerExpr)
	if len(recordTypes) == 0 {
		return false
	}

	for _, rt := range recordTypes {
		pkCols := ctx.GetPrimaryKeyColumns(rt)
		if len(pkCols) > 0 && uniqueKeysCovered(pkCols, projectedCols) {
			return true
		}
	}

	for _, cand := range ctx.GetMatchCandidates() {
		if !cand.IsUnique() {
			continue
		}
		cols := cand.GetColumnNames()
		if len(cols) > 0 && uniqueKeysCovered(cols, projectedCols) {
			return true
		}
	}

	return false
}

// collectProjectedFieldNames extracts field names from a
// LogicalProjectionExpression's projected values. If the inner
// expression is not a projection, returns nil to indicate "all
// columns available" (full row).
func collectProjectedFieldNames(expr expressions.RelationalExpression) map[string]struct{} {
	proj, ok := expr.(*expressions.LogicalProjectionExpression)
	if !ok {
		return nil
	}

	cols := make(map[string]struct{})
	for _, v := range proj.GetProjectedValues() {
		extractFieldNames(v, cols)
	}
	return cols
}

// extractFieldNames recursively collects FieldValue.Field names from
// a Value tree.
func extractFieldNames(v values.Value, out map[string]struct{}) {
	if fv, ok := v.(*values.FieldValue); ok {
		out[fv.Field] = struct{}{}
	}
	for _, child := range v.Children() {
		extractFieldNames(child, out)
	}
}

// findRecordTypes walks down through transparent operators (projection,
// filter, sort, distinct, unique, type-filter) to find a
// FullUnorderedScanExpression and returns its record types.
func findRecordTypes(expr expressions.RelationalExpression) []string {
	switch e := expr.(type) {
	case *expressions.FullUnorderedScanExpression:
		return e.GetRecordTypes()
	case *expressions.LogicalProjectionExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalSortExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalDistinctExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalUniqueExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalTypeFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	}
	return nil
}

func findRecordTypesViaQuantifier(q expressions.Quantifier) []string {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findRecordTypes(ref.Get())
}

// uniqueKeysCovered reports whether every column in uniqueKeyCols
// appears in projectedCols. If projectedCols is nil, all columns are
// considered available (no projection = full row).
func uniqueKeysCovered(uniqueKeyCols []string, projectedCols map[string]struct{}) bool {
	if projectedCols == nil {
		return true
	}
	for _, col := range uniqueKeyCols {
		found := false
		for pc := range projectedCols {
			if eqFold(col, pc) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

var _ ImplementationRule = (*ImplementDistinctFinalRule)(nil)
