package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRecordQueryInJoinPlan_BindingAliasInvariant pins RFC-164 WS-4: the InJoin
// binding is an internal correlation alias minted by a process-global counter
// (UniqueCorrelationIdentifier). Two InJoins that differ ONLY in that arbitrary
// alias are the SAME plan — their identity (EqualsWithoutChildren /
// HashCodeWithoutChildren) and Explain MUST be alias-invariant. Before the fix the
// raw alias was folded into all three, so every replanned `a IN (...)` query
// produced a non-equal, differently-hashed plan with a different Explain →
// plan-cache churn + a nondeterministic Explain oracle. The real alias is retained
// on the field for EXECUTION (GetBindingName), which is unaffected.
func TestRecordQueryInJoinPlan_BindingAliasInvariant(t *testing.T) {
	t.Parallel()
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	a := mustChecked(t, func() (*RecordQueryInJoinPlan, error) {
		return NewRecordQueryInJoinPlan(inner, "q$836", true, false)
	})
	b := mustChecked(t, func() (*RecordQueryInJoinPlan, error) {
		return NewRecordQueryInJoinPlan(inner, "q$2673", true, false)
	}) // same shape, different alias

	if !a.EqualsPlanWithoutChildren(b) {
		t.Error("InJoins differing only in the binding alias must be EqualsWithoutChildren-equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Errorf("alias-invariant hash expected equal, got %d vs %d", a.HashCodeWithoutChildren(), b.HashCodeWithoutChildren())
	}
	if a.Explain() != b.Explain() {
		t.Errorf("alias-invariant Explain expected equal:\n  %q\n  %q", a.Explain(), b.Explain())
	}

	// The real alias is still available for the executor.
	if a.GetBindingName() != "q$836" || b.GetBindingName() != "q$2673" {
		t.Errorf("GetBindingName must retain the real alias for execution, got %q / %q", a.GetBindingName(), b.GetBindingName())
	}

	// A GENUINE structural difference (scan direction) must still distinguish them —
	// alias-invariance must not collapse real differences.
	rev := mustChecked(t, func() (*RecordQueryInJoinPlan, error) {
		return NewRecordQueryInJoinPlan(inner, "q$836", true, true)
	})
	if a.EqualsPlanWithoutChildren(rev) {
		t.Error("InJoins differing in reverse must NOT be equal")
	}
	if a.HashCodeWithoutChildren() == rev.HashCodeWithoutChildren() {
		t.Error("InJoins differing in reverse should hash differently")
	}
}

func TestRecordQueryInJoinPlan_PreservesExactUniqueBindingAlias(t *testing.T) {
	t.Parallel()
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	unique := values.UniqueCorrelationIdentifier()
	plan := mustChecked(t, func() (*RecordQueryInJoinPlan, error) {
		return NewRecordQueryInJoinPlanWithBindingAlias(inner, unique, false, false)
	})
	if plan.GetBindingAlias() != unique {
		t.Fatalf("exact binding alias = %v, want original Unique handle %v", plan.GetBindingAlias(), unique)
	}
	if plan.GetBindingAlias() == values.NamedCorrelationIdentifier(unique.Name()) {
		t.Fatal("Unique binding was reconstructed as a rendered-equal Named identifier")
	}
	if plan.GetBindingName() != unique.Name() {
		t.Fatalf("display binding name = %q, want %q", plan.GetBindingName(), unique.Name())
	}
}

// TestRecordQueryInUnionPlan_BindingAliasInvariant pins the same RFC-164 WS-4
// alias-invariance for the IN-UNION path (a review follow-up): an `a IN (...)` that
// plans as a sorted/merge InUnion carries the same UniqueCorrelationIdentifier
// binding aliases, which were folded into Equals/Hash/Explain → the same
// plan-cache churn. Only the binding COUNT (number of IN columns) is structural.
func TestRecordQueryInUnionPlan_BindingAliasInvariant(t *testing.T) {
	t.Parallel()
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	ck := []values.Value{testField(t, "ID", values.NullableLong)}
	a := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
		return NewRecordQueryInUnionPlan(inner, []string{"q$5"}, ck, false)
	})
	b := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
		return NewRecordQueryInUnionPlan(inner, []string{"q$99"}, ck, false)
	}) // same shape, different alias

	if !a.EqualsPlanWithoutChildren(b) {
		t.Error("InUnions differing only in binding aliases must be EqualsWithoutChildren-equal")
	}
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Errorf("alias-invariant hash expected equal, got %d vs %d", a.HashCodeWithoutChildren(), b.HashCodeWithoutChildren())
	}
	if a.Explain() != b.Explain() {
		t.Errorf("alias-invariant Explain expected equal:\n  %q\n  %q", a.Explain(), b.Explain())
	}
	if a.GetBindingNames()[0] != "q$5" {
		t.Errorf("GetBindingNames must retain the real aliases for execution, got %v", a.GetBindingNames())
	}

	// The binding COUNT is structural — a different number of IN columns must NOT be equal.
	two := mustChecked(t, func() (*RecordQueryInUnionPlan, error) {
		return NewRecordQueryInUnionPlan(inner, []string{"q$5", "q$6"}, ck, false)
	})
	if a.EqualsPlanWithoutChildren(two) {
		t.Error("InUnions with a different number of bindings must NOT be equal")
	}
}
