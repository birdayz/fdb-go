package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// OrderedPrimaryScanRule matches Sort over FullUnorderedScan and
// produces a primary scan when the sort keys match the PK columns.
// For DESC, a reverse primary scan is produced.
//
//	Sort([pk ASC/DESC]) over FullUnorderedScan
//	  → Scan(reverse=DESC)
//
// Complements OrderedIndexScanRule which handles secondary indexes.
type OrderedPrimaryScanRule struct {
	matcher matching.BindingMatcher
}

func NewOrderedPrimaryScanRule() *OrderedPrimaryScanRule {
	return &OrderedPrimaryScanRule{
		matcher: NewExpressionMatcher[*expressions.LogicalSortExpression]("sort_for_ordered_pk"),
	}
}

func (r *OrderedPrimaryScanRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *OrderedPrimaryScanRule) OnMatch(call *ExpressionRuleCall) {
	s := matching.Get[*expressions.LogicalSortExpression](call.Bindings, r.matcher)
	if s.IsUnsorted() {
		return
	}

	sortKeys := s.GetSortKeys()
	if len(sortKeys) == 0 {
		return
	}

	innerRef := s.GetInner().GetRangesOver()
	if innerRef == nil {
		return
	}

	scan := findFullScan(innerRef)
	if scan == nil {
		return
	}

	if call.Context == nil || len(scan.GetRecordTypes()) != 1 {
		return
	}

	pkCols := call.Context.GetPrimaryKeyColumns(scan.GetRecordTypes()[0])
	if len(pkCols) == 0 || len(sortKeys) > len(pkCols) {
		return
	}

	matches := true
	reverse := false
	for i, sk := range sortKeys {
		fv, ok := sk.Value.(*values.FieldValue)
		if !ok {
			matches = false
			break
		}
		if !eqFold(fv.Field, pkCols[i]) {
			matches = false
			break
		}
		if i == 0 {
			reverse = sk.Reverse
		} else if sk.Reverse != reverse {
			matches = false
			break
		}
	}
	if !matches {
		return
	}

	// A reverse scan reverses ALL PK columns. If the sort covers only a
	// prefix of the PK (e.g. ORDER BY a DESC on PK (a,b)), reverse scan
	// produces (a DESC, b DESC) — wrong for uncovered suffix columns.
	if reverse && len(sortKeys) < len(pkCols) {
		return
	}

	plan := plans.NewRecordQueryScanPlan(scan.GetRecordTypes(), scan.GetFlowedType(), reverse)
	pkVals := make([]values.Value, len(pkCols))
	for i, col := range pkCols {
		pkVals[i] = &values.FieldValue{Field: col, Typ: values.UnknownType}
	}
	plan = plan.WithPrimaryKey(pkVals)

	call.Yield(&physicalScanWrapper{plan: plan})
}

var _ ExpressionRule = (*OrderedPrimaryScanRule)(nil)
