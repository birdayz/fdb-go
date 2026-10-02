package cascades

// RFC-153 follow-up on 05c742100: correlation-bookkeeping completeness.
// The EXECUTED rows were correct, but the REPORTED correlations were stale/missing — a
// latent planning hazard (wrong join-leg / winner / root bookkeeping in untested shapes),
// the same incomplete-coverage family as the fail-open verifier.
//
// P2#1: a SARGed PK RecordQueryScanPlan (`pk = QOV(outer).fk`) wrapped as
//       scanPlanExpression reported NO correlation (the data-access correlation wiring
//       reached the physical scan wrappers but not this plan-backed leaf).
// P2#2: when the RFC-153 buried-merge rebase rewrites the FlatMap inner's correlation
//       onto the merge alias $m, the memoized inner EXPRESSION must report $m too — not
//       the original buried alias — so the FlatMap wrapper doesn't aggregate a
//       correlation to an UNBOUND alias.

import (
	"maps"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRFC153P2Construct[T any](value T, err error) T {
	if err != nil {
		panic("construct RFC-153 P2 fixture: " + err.Error())
	}
	return value
}

func TestDataAccessCorrelationRespectsBindings(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"map", "filter", "legacy_filter"} {
		for _, source := range []string{"current", "bound", "foreign", "named_current", "constant", "constant_and_row"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				t.Parallel()
				scan := mustRFC153P2Construct(plans.NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false))
				bound := values.NamedCorrelationIdentifier("bound")
				foreign := values.NamedCorrelationIdentifier("foreign")
				q := plans.QuantifierOverPlan(scan).WithAlias(bound)
				var value values.Value = scan.GetResultValue()
				want := map[values.CorrelationIdentifier]struct{}{}
				switch source {
				case "bound":
					value = mustRFC153P2Construct(values.NewQuantifiedObjectValue(bound, values.NotNullLong))
				case "foreign", "named_current":
					if source == "named_current" {
						foreign = values.NamedCorrelationIdentifier("_current")
					}
					value = mustRFC153P2Construct(values.NewQuantifiedObjectValue(foreign, values.NotNullLong))
					want[foreign] = struct{}{}
				case "constant", "constant_and_row":
					value = values.NewConstantObjectValue(foreign, "c", values.NotNullLong)
					if source == "constant_and_row" {
						row := mustRFC153P2Construct(values.NewQuantifiedObjectValue(foreign, values.NotNullLong))
						value = values.NewRecordConstructorValue(values.RecordConstructorField{Name: "constant", Value: value}, values.RecordConstructorField{Name: "row", Value: row})
						want[foreign] = struct{}{}
					}
				}
				preds := []predicates.QueryPredicate{predicates.NewComparisonPredicate(value, predicates.Comparison{Type: predicates.ComparisonIsNotNull})}
				var plan plans.RecordQueryPlan
				switch kind {
				case "map":
					plan = mustRFC153P2Construct(plans.NewRecordQueryMapPlanFromQuantifier(q, value))
				case "filter":
					plan = mustRFC153P2Construct(plans.NewRecordQueryPredicatesFilterPlanFromQuantifier(q, preds))
				case "legacy_filter":
					plan = mustRFC153P2Construct(plans.NewRecordQueryFilterPlanFromQuantifier(preds, q))
				}
				for name, got := range map[string]map[values.CorrelationIdentifier]struct{}{
					"plan":    expressions.GetCorrelatedToOfExpression(plan),
					"wrapper": (&scanPlanExpression{plan: plan}).GetCorrelatedToWithoutChildren(),
				} {
					if !maps.Equal(got, want) {
						t.Errorf("%s correlations=%v, want %v", name, got, want)
					}
				}
			})
		}
	}
}

func TestConstantPoolCannotEraseRowCorrelation(t *testing.T) {
	t.Parallel()
	outer := values.NamedCorrelationIdentifier("outer")
	rowType := rfc153P2RowType("id")
	field := rfc153P2Field(outer, rowType)
	constant := values.NewConstantObjectValue(outer, "c", values.NotNullLong)
	mixed := values.NewRecordConstructorValue(values.RecordConstructorField{Name: "row", Value: field}, values.RecordConstructorField{Name: "constant", Value: constant})
	comparison := &predicates.Comparison{Type: predicates.ComparisonEquals, Operand: mixed}
	if !comparisonRowCorrelated(comparison) {
		t.Error("constant erased row dependency in intersection guard")
	}
	ranges := []*predicates.ComparisonRange{rfc153P2EqRange(mixed)}
	scan := mustRFC153P2Construct(plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)).WithScanComparisons(ranges)
	for name, got := range map[string]map[values.CorrelationIdentifier]struct{}{
		"scan":        expressions.GetCorrelatedToOfExpression(scan),
		"wrapper":     (&scanPlanExpression{plan: scan}).GetCorrelatedToWithoutChildren(),
		"scan_helper": scanComparisonCorrelations(ranges),
	} {
		if _, found := got[outer]; !found || len(got) != 1 {
			t.Errorf("%s lost row correlation: %v", name, got)
		}
	}
	uncorrelated := mustRFC153P2Construct(plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false))
	filter := mustRFC153P2Construct(expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{
		predicates.NewComparisonPredicate(field, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: constant}),
	}, expressions.ForEachQuantifier(expressions.InitialOf(uncorrelated))))
	if compensationResidualCorrelationSafe(filter) {
		t.Error("constant hid an unbound outer alias in a compensation residual")
	}
	vector := mustRFC153P2Construct(plans.NewRecordQueryVectorIndexPlan("vec", nil, values.LiteralValue([]float64{1, 2}), values.LiteralValue(int64(5)), predicates.ComparisonDistanceRankLessThanOrEq, nil, nil, []string{"T"}, rowType)).WithPartitionColumns([]string{"id"})
	if residualSelectsWholePartitions(filter, vector) {
		t.Error("outer field was mistaken for a local partition field")
	}
}

func rfc153P2RowType(fieldName string) *values.RecordType {
	return values.NewRecordType("", false, []values.Field{{
		Name:      fieldName,
		FieldType: values.NotNullLong,
	}})
}

func rfc153P2Field(alias values.CorrelationIdentifier, rowType values.Type) values.Value {
	root := mustRFC153P2Construct(values.NewQuantifiedObjectValue(alias, rowType))
	return mustRFC153P2Construct(values.ResolveFieldOrdinals(root, []int{0}))
}

func rfc153P2EqRange(comparand values.Value) *predicates.ComparisonRange {
	eq := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: comparand}
	return predicates.EmptyComparisonRange().Merge(&eq).Range
}

// TestRFC153P2_PKScanProbe_ReportsOuterCorrelation (P2#1): a primary-key
// RecordQueryScanPlan SARGed with a join predicate `pk = QOV(E).id` is a CORRELATED
// probe — scanPlanExpression.GetCorrelatedToWithoutChildren must report the outer alias
// E. (The old wrapper returned nil → the bare PK probe looked self-contained to
// join-leg/winner bookkeeping.)
func TestRFC153P2_PKScanProbe_ReportsOuterCorrelation(t *testing.T) {
	t.Parallel()
	outer := values.NamedCorrelationIdentifier("E")
	fk := rfc153P2Field(outer, rfc153P2RowType("id"))
	pkScan := mustRFC153P2Construct(plans.NewRecordQueryScanPlan(
		[]string{"M"}, rfc153P2RowType("id"), false)).
		WithScanComparisons([]*predicates.ComparisonRange{rfc153P2EqRange(fk)})

	expr := &scanPlanExpression{plan: pkScan}
	corr := expr.GetCorrelatedToWithoutChildren()
	if _, ok := corr[outer]; !ok {
		t.Fatalf("PK-scan probe SARGed on QOV(E).id must report outer correlation E, got %v", corr)
	}
}

// TestRFC153P2_PKScanProbe_ParamNotCorrelation (P2#1 boundary): a `pk = ?param` bind is
// an execution constant, NOT a row correlation — it must NOT be reported (the param
// exclusion the physical scan wrappers already apply).
func TestRFC153P2_PKScanProbe_ParamNotCorrelation(t *testing.T) {
	t.Parallel()
	litScan := mustRFC153P2Construct(plans.NewRecordQueryScanPlan(
		[]string{"M"}, rfc153P2RowType("id"), false)).
		WithScanComparisons([]*predicates.ComparisonRange{rfc153P2EqRange(predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(7)).Operand)})
	expr := &scanPlanExpression{plan: litScan}
	if corr := expr.GetCorrelatedToWithoutChildren(); len(corr) != 0 {
		t.Fatalf("PK-scan SARGed on a literal must report NO correlation, got %v", corr)
	}
}

// TestRFC153P2_RebasedInner_ReportsMergeNotBuried (P2#2): a buried-merge-rebased inner
// (an index probe whose SARG was rewritten from the buried alias A to a field of the
// merge correlation $m) must report $m and NOT the buried A. The original (un-rebased)
// inner reports A — so memoizing the original innerExpr after a rebase would leak a
// correlation to the unbound buried alias.
func TestRFC153P2_RebasedInner_ReportsMergeNotBuried(t *testing.T) {
	t.Parallel()
	merge := values.NamedCorrelationIdentifier(`$m"1`)
	buriedA := values.NamedCorrelationIdentifier("A")
	innerType := rfc153P2RowType("c_a_id")

	// Un-rebased inner: c_a_id = QOV(A).id → reports A.
	origComparand := rfc153P2Field(buriedA, rfc153P2RowType("id"))
	origIdx := mustRFC153P2Construct(plans.NewRecordQueryIndexPlan(
		"c_a_id", []*predicates.ComparisonRange{rfc153P2EqRange(origComparand)},
		[]string{"C"}, innerType, false))
	origInner := mustRFC153P2Construct(plans.NewRecordQueryDefaultOnEmptyPlan(
		origIdx, values.NewNullValue(innerType)))
	origCorr := (&scanPlanExpression{plan: origInner}).GetCorrelatedToWithoutChildren()
	if _, ok := origCorr[buriedA]; !ok {
		t.Fatalf("un-rebased inner must report the buried alias A (else the test fixture is wrong), got %v", origCorr)
	}

	// Rebased inner: c_a_id = FieldValue(QOV($m), "A.ID") → must report $m, NOT A.
	rebasedComparand := rfc153P2Field(merge, rfc153P2RowType("A.ID"))
	rebasedIdx := mustRFC153P2Construct(plans.NewRecordQueryIndexPlan(
		"c_a_id", []*predicates.ComparisonRange{rfc153P2EqRange(rebasedComparand)},
		[]string{"C"}, innerType, false))
	rebasedInner := mustRFC153P2Construct(plans.NewRecordQueryDefaultOnEmptyPlan(
		rebasedIdx, values.NewNullValue(innerType)))
	rebasedCorr := (&scanPlanExpression{plan: rebasedInner}).GetCorrelatedToWithoutChildren()
	if _, ok := rebasedCorr[merge]; !ok {
		t.Errorf("rebased inner must report the merge correlation $m, got %v", rebasedCorr)
	}
	if _, ok := rebasedCorr[buriedA]; ok {
		t.Errorf("rebased inner must NOT report the buried alias A (the P2#2 leak), got %v", rebasedCorr)
	}
}
