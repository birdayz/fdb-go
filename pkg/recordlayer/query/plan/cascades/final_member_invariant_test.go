package cascades

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestVerifyExtractionIsUnambiguousWalksSelectedFallbackPath(t *testing.T) {
	t.Parallel()

	selectedChild := expressions.InitialOf(rfc224Scan(t, "SELECTED"))
	selectedParent, err := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(selectedChild), 1, 0, nil)
	if err != nil {
		t.Fatalf("selected parent: %v", err)
	}

	// This decoy is reachable through an unselected alternative and contains
	// no physical plan. Walking every final/member branch would report it; the
	// extractor and verifier must follow only the stamped selected parent.
	decoyChild := expressions.InitialOf(rfc224LogicalScan(t, "DECOY"))
	decoyParent, err := plans.NewRecordQueryLimitPlanFromQuantifier(
		expressions.NewPhysicalQuantifier(decoyChild), 1, 0, nil)
	if err != nil {
		t.Fatalf("decoy parent: %v", err)
	}
	root := expressions.InitialOf(decoyParent)
	if !root.InsertFinal(selectedParent) {
		t.Fatal("selected parent deduplicated from decoy")
	}
	root.SetWinner(selectedParent)

	report := VerifyExtractionIsUnambiguous(root, nil, properties.DefaultStatistics{})
	if report.DeadEnds != 0 || len(report.Violations) != 0 {
		t.Fatalf("selected-path report = deadEnds %d, violations %v", report.DeadEnds, report.Violations)
	}
	if report.VisitedReferences != 2 {
		t.Fatalf("visited References = %d, want root + selected child", report.VisitedReferences)
	}
	if _, ok := report.visited[selectedChild]; !ok {
		t.Fatal("physical exploratory fallback child was not visited")
	}
	if _, ok := report.visited[decoyChild]; ok {
		t.Fatal("unselected decoy branch was visited")
	}

	extracted, err := ExtractBestPlanFromSelector(root, nil, properties.DefaultStatistics{})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	limit, ok := extracted.(*plans.RecordQueryLimitPlan)
	if !ok || limit.GetInner() == nil {
		t.Fatalf("extracted selected path = %T with inner %v", extracted, ok && limit.GetInner() != nil)
	}
}

func TestVerifyExtractionIsUnambiguousReportsDeadEnd(t *testing.T) {
	t.Parallel()
	root := expressions.InitialOf(rfc224LogicalScan(t, "LOGICAL_ONLY"))

	report := VerifyExtractionIsUnambiguous(root, nil, nil)
	if report.VisitedReferences != 1 || report.DeadEnds != 1 {
		t.Fatalf("reach = visited %d, deadEnds %d; want 1,1",
			report.VisitedReferences, report.DeadEnds)
	}
	if !containsInvariantViolation(report.Violations, "extraction dead end") {
		t.Fatalf("violations %v do not name the dead end", report.Violations)
	}
}

func TestVerifyExtractionIsUnambiguousReportsWinnerOutsideFinals(t *testing.T) {
	t.Parallel()
	final := rfc224Scan(t, "FINAL")
	stale := rfc224Scan(t, "STALE")
	root := expressions.FinalOf(final)
	root.SetWinner(stale)

	report := VerifyExtractionIsUnambiguous(root, nil, nil)
	if report.DeadEnds != 0 {
		t.Fatalf("physical stale winner is not a dead end: %d", report.DeadEnds)
	}
	if !containsInvariantViolation(report.Violations, "not a final member") {
		t.Fatalf("violations %v do not report the stale winner", report.Violations)
	}
}

func TestVerifyExtractionIsUnambiguousReportsUnlicensedRetainedFinal(t *testing.T) {
	t.Parallel()
	winner := rfc224Scan(t, "WINNER")
	deadWeight := rfc224Scan(t, "DEAD_WEIGHT")
	root := expressions.FinalOf(winner)
	if !root.InsertFinal(deadWeight) {
		t.Fatal("distinct retained scan deduplicated")
	}
	root.SetWinner(winner)

	report := VerifyExtractionIsUnambiguous(root, nil, nil)
	if report.MultiFinalReferences != 1 {
		t.Fatalf("multi-final References = %d, want 1", report.MultiFinalReferences)
	}
	if !containsInvariantViolation(report.Violations, "not the winner for any required physical property") {
		t.Fatalf("violations %v do not report the unlicensed retained final", report.Violations)
	}
}

// This is the load-bearing "property, not merely ordering" pin. The primary
// scan and an unstamped index belong to different DistinctRecords partitions.
// OptimizeGroup intentionally retains both without a requested ordering, and
// the RFC-224 verifier must recognize that non-ordering property license.
func TestVerifyExtractionIsUnambiguousAllowsNonOrderingPropertyRetention(t *testing.T) {
	t.Parallel()
	rowType := rfc224RowType()
	index, err := plans.NewRecordQueryIndexPlan("idx_x", nil, []string{"T"}, rowType, false)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	logicalRoot, logicalErr := expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType)
	if logicalErr != nil {
		t.Fatalf("logical root: %v", logicalErr)
	}
	root := expressions.InitialOf(logicalRoot)
	root.InsertFinal(index)
	root.InsertFinal(scan)
	planner := NewPlanner(nil, nil)
	planner.constraintMap = NewConstraintMap()
	(&OptimizeGroupTask{Phase: PhasePlanning, Ref: root}).Run(context.Background(), planner)
	if got := len(root.FinalMembers()); got != 2 {
		t.Fatalf("fixture retained finals = %d, want two property partitions", got)
	}

	report := VerifyExtractionIsUnambiguous(root, planner, nil)
	if report.MultiFinalReferences != 1 {
		t.Fatalf("multi-final References = %d, want 1", report.MultiFinalReferences)
	}
	if report.DeadEnds != 0 || len(report.Violations) != 0 {
		t.Fatalf("legitimate property retention rejected: deadEnds %d, violations %v",
			report.DeadEnds, report.Violations)
	}
}

func TestVerifyExtractionIsUnambiguousAgreesOnPositionalRequirement(t *testing.T) {
	t.Parallel()
	parent, childRef, compatibleLayout, _ := ordinalLayoutSelectionFixture(t)
	requirements, err := ordinalInputRequirementsOf(parent)
	if err != nil || len(requirements) != 1 {
		t.Fatalf("parent requirements = (%d,%v), want 1,nil", len(requirements), err)
	}
	compatible := compatiblePhysicalMember(t, childRef, compatibleLayout)

	planner := NewPlanner(nil, nil)
	planner.constraintMap = NewConstraintMap()
	Set(planner.constraintMap, childRef, OrdinalLayoutConstraintKey, requirements)
	root := expressions.FinalOf(parent)

	report := VerifyExtractionIsUnambiguous(root, planner, nil)
	if report.DeadEnds != 0 || len(report.Violations) != 0 {
		t.Fatalf("requirement-aware report = deadEnds %d, violations %v",
			report.DeadEnds, report.Violations)
	}
	selected := report.selected[childRef]
	if len(selected) != 1 || selected[0] != compatible {
		t.Fatalf("verifier selected %v, want compatible member %T", selected, compatible)
	}

	extracted, err := ExtractBestPlanFromSelector(root, planner, nil)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	limit, ok := extracted.(*plans.RecordQueryLimitPlan)
	if !ok || limit.GetInner() == nil {
		t.Fatalf("extracted = %T, want Limit with inner", extracted)
	}
	gotLayout, err := limit.GetInner().ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("extracted inner layout: %v", err)
	}
	if !gotLayout.RawEqual(compatibleLayout) {
		t.Fatal("verifier and extraction disagreed on the requirement-compatible child")
	}
}

func rfc224RowType() *values.RecordType {
	return values.NewRecordType("ExtractionInvariant", false, []values.Field{
		{Name: "X", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func rfc224Scan(t testing.TB, recordType string) *plans.RecordQueryScanPlan {
	t.Helper()
	scan, err := plans.NewRecordQueryScanPlan([]string{recordType}, rfc224RowType(), false)
	if err != nil {
		t.Fatalf("scan %s: %v", recordType, err)
	}
	return scan
}

func rfc224LogicalScan(t testing.TB, recordType string) *expressions.FullUnorderedScanExpression {
	t.Helper()
	scan, err := expressions.NewFullUnorderedScanExpression([]string{recordType}, rfc224RowType())
	if err != nil {
		t.Fatalf("logical scan %s: %v", recordType, err)
	}
	return scan
}

func containsInvariantViolation(violations []string, needle string) bool {
	for _, violation := range violations {
		if strings.Contains(violation, needle) {
			return true
		}
	}
	return false
}
