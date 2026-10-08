package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func coveringValueReader(t *testing.T, ordinal int) *values.RecordConstructorValue {
	t.Helper()
	leaf, err := values.NewIndexEntryObjectValue(values.CurrentCorrelation(), values.TupleSourceKey, []int{ordinal}, values.NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	return values.NewRecordConstructorValue(values.RecordConstructorField{Name: "A", Value: leaf})
}

func coveringValueFixture(t *testing.T, recordType string, ordinal int) (*RecordQueryIndexPlan, *RecordQueryCoveringIndexValuePlan) {
	t.Helper()
	index, err := NewRecordQueryIndexPlan("idx", nil, []string{"T"}, exactTestRecordType(), false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewRecordQueryCoveringIndexValuePlan(index, recordType, coveringValueReader(t, ordinal))
	if err != nil {
		t.Fatal(err)
	}
	return index, plan
}

// TestCoveringIndexValuePlan_DelegatesAndKeepsItsReader ports
// delegatesToTheIndexPlanUnderneathIt and rewritingThePlanKeepsTheReader.
func TestCoveringIndexValuePlan_DelegatesAndKeepsItsReader(t *testing.T) {
	t.Parallel()
	index, plan := coveringValueFixture(t, "T", 0)
	if plan.GetIndexName() != "idx" || plan.IsReverse() != index.IsReverse() ||
		plan.IsStrictlySorted() != index.IsStrictlySorted() || plan.GetChildren() != nil ||
		len(plan.GetQuantifiers()) != 0 || plan.ProducesDistinctRecords() != index.ProducesDistinctRecords() {
		t.Fatal("the value plan does not answer as its index plan")
	}
	if got, ok := IndexPlanOf(plan); !ok || got != index {
		t.Fatal("IndexPlanOf does not see the value plan's index scan")
	}
	reversed, err := NewRecordQueryIndexPlan("idx", nil, []string{"T"}, exactTestRecordType(), true)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := plan.WithIndexPlan(reversed)
	if !rewritten.IsReverse() || rewritten.GetIndexEntryToRecordValue() != plan.GetIndexEntryToRecordValue() {
		t.Fatal("rewriting the index plan must keep the reader")
	}
}

// TestCoveringIndexValuePlan_Identity ports equalPlansHaveEqualHashCodes: equal
// parts are equal plans; a different record type or reader is a different
// plan, and so is the covering plan over the same scan.
func TestCoveringIndexValuePlan_Identity(t *testing.T) {
	t.Parallel()
	index, plan := coveringValueFixture(t, "T", 0)
	_, same := coveringValueFixture(t, "T", 0)
	if !plan.EqualsPlanWithoutChildren(same) || plan.HashCodeWithoutChildren() != same.HashCodeWithoutChildren() {
		t.Fatal("equal value plans must be equal and hash alike")
	}
	_, otherType := coveringValueFixture(t, "U", 0)
	_, otherReader := coveringValueFixture(t, "T", 1)
	covering, err := NewRecordQueryCoveringIndexPlan(index)
	if err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]RecordQueryPlan{"record type": otherType, "reader": otherReader, "covering plan": covering} {
		if plan.EqualsPlanWithoutChildren(other) {
			t.Errorf("a value plan equals one differing by %s", name)
		}
	}
}

// TestCoveringIndexValuePlan_Explain: the two covering plans explain alike up
// to the arrow, after which the value plan shows its reader.
func TestCoveringIndexValuePlan_Explain(t *testing.T) {
	t.Parallel()
	index, plan := coveringValueFixture(t, "T", 1)
	covering, err := NewRecordQueryCoveringIndexPlan(index)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := covering.Explain(), "IndexScan(idx, [] COVERING)"; got != want {
		t.Fatalf("covering explain = %q, want %q", got, want)
	}
	if got, want := plan.Explain(), "IndexScan(idx, [] COVERING -> {A: KEY:[1]})"; got != want {
		t.Fatalf("value plan explain = %q, want %q", got, want)
	}
}
