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
	"testing"

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
