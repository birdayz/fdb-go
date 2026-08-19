package cascades

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func lazyLegLayout(name string, columns ...string) *values.RecordType {
	fields := make([]values.Field, len(columns))
	for i, column := range columns {
		fields[i] = values.Field{Name: column, FieldType: values.NullableLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

func lazyLegLayouts() map[string]*values.RecordType {
	return map[string]*values.RecordType{
		"OUTER":  lazyLegLayout("OUTER", "ID", "CATEGORY"),
		"INNER":  lazyLegLayout("INNER", "ID", "OUTER_ID"),
		"SHADOW": lazyLegLayout("SHADOW", "ID", "NOTE"),
	}
}

// buildTwoLegExistentialSelect assembles the shape OnMatch routes to
// the existential peel: exactly two ForEach legs plus one trailing
// Existential in a single flat Select (`SELECT … FROM L, R WHERE EXISTS
// (SELECT 1 FROM E WHERE E.OUTER_ID = L.ID)`), with the existential's
// correlation predicate pointing at a LEG column.
//
// That last part is what makes the leg-match arm reachable: the step-2 FlatMap
// binds only the MERGED outer row, so a predicate reading QOV(L).ID has to be
// re-anchored onto the merge correlation before it can be lifted, and
// rebaseOuterLegRefsToMerged is what does the re-anchoring.
//
// It returns the two leg QUANTIFIERS alongside the yielded plans so a test can
// assert what the layout derivation says about this exact shape. Without that,
// "the mint still fires" is unfalsifiable: it would also hold for a shape whose
// layouts are underivable, which is the case that proves nothing.
func buildTwoLegExistentialSelect(t testing.TB) ([]expressions.RelationalExpression, expressions.Quantifier, expressions.Quantifier) {
	legA := values.NamedCorrelationIdentifier("L")
	legB := values.NamedCorrelationIdentifier("R")
	existAlias := values.NamedCorrelationIdentifier("E")

	layouts := lazyLegLayouts()
	aType := values.Type(layouts["OUTER"])  // ID, CATEGORY
	bType := values.Type(layouts["SHADOW"]) // ID, NOTE
	eType := values.Type(layouts["INNER"])  // ID, OUTER_ID

	newLeg := func(table string, rt values.Type) *expressions.Reference {
		ref := expressions.InitialOf(mustFullUnorderedScan(t, []string{table}, rt))
		scan, err := plans.NewRecordQueryScanPlan([]string{table}, rt, false)
		ref.InsertFinal(mustConstruct(t, scan, err))
		return ref
	}

	qA := expressions.NamedForEachQuantifier(legA, newLeg("OUTER", aType))
	qB := expressions.NamedForEachQuantifier(legB, newLeg("SHADOW", bType))
	qE := expressions.NamedExistentialQuantifier(existAlias, newLeg("INNER", eType))

	// E.OUTER_ID = L.ID — an inner↔outer correlation predicate, the only kind
	// existsInnerCorrelation lifts, and the one whose outer half must be
	// rebased onto the merged row.
	//
	// The OUTER half is built BAKED — a single accessor at the column's ordinal
	// in the leg's own row layout — because that is the shape production
	// produces. It used to be built lazy, which predates the resolver carrying
	// the row its correlated quantifier object flows
	// (Quantifier.java:801-803's `QuantifiedObjectValue.of(getAlias(),
	// getFlowedObjectType())`); measured over the real-FDB corpus, every one of
	// the arm's firings arrives with a resolved path in its leg's own domain and
	// none arrives lazy. A lazy fixture therefore drove the arm's DECLINE while
	// claiming to cover the path production takes.
	innerRoot, err := values.NewQuantifiedObjectValue(existAlias, eType)
	exactInnerRoot := mustConstruct(t, innerRoot, err)
	innerRef, err := values.ResolveFieldOrdinals(exactInnerRoot, []int{1})
	innerRef = mustConstruct(t, innerRef, err)
	outerRoot, err := values.NewQuantifiedObjectValue(legA, aType)
	exactOuterRoot := mustConstruct(t, outerRoot, err)
	outerLegRef, err := values.ResolveFieldOrdinals(exactOuterRoot, []int{0})
	outerLegRef = mustConstruct(t, outerLegRef, err)

	selectResult, err := values.NewQuantifiedObjectValue(legA, aType)
	exactSelectResult := mustConstruct(t, selectResult, err)
	sel, err := expressions.NewSelectExpressionWithAliases(
		exactSelectResult,
		[]expressions.Quantifier{qA, qB, qE},
		[]predicates.QueryPredicate{
			predicates.NewComparisonPredicate(innerRef,
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerLegRef}),
			mustExistentialAlias(t, existAlias, eType),
		},
		[]string{"L", "R", "E"},
	)
	return mustFireExpressionRule(
		t,
		NewImplementNestedLoopJoinRule(),
		expressions.InitialOf(mustConstruct(t, sel, err)),
	), qA, qB
}

// dottedLegRefsOf collects every FieldValue reachable from the yielded plans
// whose Field is DOTTED and whose child is a QuantifiedObjectValue — the exact
// signature rebaseOuterLegValue's leg-match arm emits. Nothing else in this
// scenario produces a dotted Field: both leaf row types declare only flat
// top-level columns, and the predicates are built here from bare names.
//
// The walk covers the predicate surfaces a rebased existential predicate lands
// on (the compensation chain's filter, the existential subplan's own
// predicates and scan bounds) plus every node's result value, since a
// projected EXISTS carries its rebased projection in the FlatMap's result
// value. A surface this misses makes the test FAIL, never pass vacuously —
// the safe direction for a liveness assertion.
func dottedLegRefsOf(yielded []expressions.RelationalExpression) []values.FieldValue {
	var out []values.FieldValue
	visit := func(v values.Value) values.Value {
		if fv, ok := values.AsFieldValue(v); ok && strings.Contains(fv.DisplayName(), ".") {
			if _, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
				out = append(out, fv)
			}
		}
		return v
	}
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, visit)
		}
	}
	collectRanges := func(crs []*predicates.ComparisonRange) {
		for _, cr := range crs {
			switch {
			case cr.IsEquality():
				collectComparison(cr.GetEqualityComparison())
			case cr.IsInequality():
				for _, c := range cr.GetInequalityComparisons() {
					collectComparison(c)
				}
			}
		}
	}
	for _, y := range yielded {
		rp, ok := y.(plans.RecordQueryPlan)
		if !ok {
			continue
		}
		plans.Walk(rp, func(p plans.RecordQueryPlan) bool {
			if rv := p.GetResultValue(); rv != nil {
				values.Replace(rv, visit)
			}
			switch t := p.(type) {
			case *plans.RecordQueryPredicatesFilterPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryFilterPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryNestedLoopJoinPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryScanPlan:
				collectRanges(t.GetScanComparisons())
			case *plans.RecordQueryIndexPlan:
				collectRanges(t.GetScanComparisons())
			}
			return true
		})
	}
	return out
}

// legLocalLegRefsOf collects every FieldValue reachable from the yielded plans
// that reads a column off a LEG quantifier — the shape the leg-match arm now
// produces when it can state the leg's layout. Mirrors dottedLegRefsOf's walk
// exactly (same surfaces, same node kinds) so the two are comparable: a reference
// counted by one and not the other has genuinely changed form.
func legLocalLegRefsOf(yielded []expressions.RelationalExpression, leg values.CorrelationIdentifier) []values.FieldValue {
	var out []values.FieldValue
	visit := func(v values.Value) values.Value {
		if fv, ok := values.AsFieldValue(v); ok {
			if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV && qov.Correlation() == leg {
				out = append(out, fv)
			}
		}
		return v
	}
	collectComparison := func(c *predicates.Comparison) {
		if c != nil && c.Operand != nil {
			values.Replace(c.Operand, visit)
		}
	}
	collectRanges := func(crs []*predicates.ComparisonRange) {
		for _, cr := range crs {
			switch {
			case cr.IsEquality():
				collectComparison(cr.GetEqualityComparison())
			case cr.IsInequality():
				for _, c := range cr.GetInequalityComparisons() {
					collectComparison(c)
				}
			}
		}
	}
	for _, y := range yielded {
		rp, ok := y.(plans.RecordQueryPlan)
		if !ok {
			continue
		}
		plans.Walk(rp, func(p plans.RecordQueryPlan) bool {
			if rv := p.GetResultValue(); rv != nil {
				values.Replace(rv, visit)
			}
			switch t := p.(type) {
			case *plans.RecordQueryPredicatesFilterPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryFilterPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryNestedLoopJoinPlan:
				for _, pr := range t.GetPredicates() {
					predicates.ReplaceValues(pr, visit)
				}
			case *plans.RecordQueryScanPlan:
				collectRanges(t.GetScanComparisons())
			case *plans.RecordQueryIndexPlan:
				collectRanges(t.GetScanComparisons())
			}
			return true
		})
	}
	return out
}
