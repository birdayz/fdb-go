package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestMatchedSortOrder_IsDirectional(t *testing.T) {
	t.Parallel()
	for _, s := range []MatchedSortOrder{
		MatchedSortOrderAscending,
		MatchedSortOrderDescending,
		MatchedSortOrderAscendingNullsLast,
		MatchedSortOrderDescendingNullsFirst,
	} {
		if !s.IsDirectional() {
			t.Fatalf("%v should be directional", s)
		}
	}
}

func TestMatchedSortOrder_IsAnyAscendingDescending(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    MatchedSortOrder
		asc  bool
		desc bool
	}{
		{MatchedSortOrderAscending, true, false},
		{MatchedSortOrderDescending, false, true},
		{MatchedSortOrderAscendingNullsLast, true, false},
		{MatchedSortOrderDescendingNullsFirst, false, true},
	}
	for _, c := range cases {
		if c.s.IsAnyAscending() != c.asc {
			t.Fatalf("%v: IsAnyAscending=%v, want %v", c.s, c.s.IsAnyAscending(), c.asc)
		}
		if c.s.IsAnyDescending() != c.desc {
			t.Fatalf("%v: IsAnyDescending=%v, want %v", c.s, c.s.IsAnyDescending(), c.desc)
		}
	}
}

func TestMatchedSortOrder_String(t *testing.T) {
	t.Parallel()
	if MatchedSortOrderAscending.String() != "ASCENDING" {
		t.Fatalf("got %q", MatchedSortOrderAscending.String())
	}
	if MatchedSortOrderDescending.String() != "DESCENDING" {
		t.Fatalf("got %q", MatchedSortOrderDescending.String())
	}
	if MatchedSortOrderAscendingNullsLast.String() != "ASCENDING_NULLS_LAST" {
		t.Fatalf("got %q", MatchedSortOrderAscendingNullsLast.String())
	}
	if MatchedSortOrderDescendingNullsFirst.String() != "DESCENDING_NULLS_FIRST" {
		t.Fatalf("got %q", MatchedSortOrderDescendingNullsFirst.String())
	}
}

func TestMatchedSortOrder_ArrowIndicator(t *testing.T) {
	t.Parallel()
	if MatchedSortOrderAscending.ArrowIndicator() != "↑" {
		t.Fatal("ascending arrow")
	}
	if MatchedSortOrderDescending.ArrowIndicator() != "↓" {
		t.Fatal("descending arrow")
	}
}

func TestMatchedSortOrder_ToProvidedSortOrder(t *testing.T) {
	t.Parallel()
	if MatchedSortOrderAscending.ToProvidedSortOrder(false) != ProvidedSortOrderAscending {
		t.Fatal("asc forward")
	}
	if MatchedSortOrderDescending.ToProvidedSortOrder(false) != ProvidedSortOrderDescending {
		t.Fatal("desc forward")
	}
	// Reverse flips direction.
	if MatchedSortOrderAscending.ToProvidedSortOrder(true) != ProvidedSortOrderDescending {
		t.Fatal("asc reverse")
	}
	if MatchedSortOrderDescending.ToProvidedSortOrder(true) != ProvidedSortOrderAscending {
		t.Fatal("desc reverse")
	}
	// NullsLast/NullsFirst variants.
	if MatchedSortOrderAscendingNullsLast.ToProvidedSortOrder(false) != ProvidedSortOrderAscendingNullsFirst {
		t.Fatal("asc-nulls-last forward")
	}
	if MatchedSortOrderAscendingNullsLast.ToProvidedSortOrder(true) != ProvidedSortOrderDescendingNullsLast {
		t.Fatal("asc-nulls-last reverse")
	}
	if MatchedSortOrderDescendingNullsFirst.ToProvidedSortOrder(false) != ProvidedSortOrderDescendingNullsLast {
		t.Fatal("desc-nulls-first forward")
	}
	if MatchedSortOrderDescendingNullsFirst.ToProvidedSortOrder(true) != ProvidedSortOrderAscendingNullsFirst {
		t.Fatal("desc-nulls-first reverse")
	}
}

func TestNewMatchedOrderingPart_Getters(t *testing.T) {
	t.Parallel()
	pid := values.NamedCorrelationIdentifier("p1")
	v := &values.FieldValue{Field: "col_a", Typ: values.UnknownType}
	cr := predicates.EmptyComparisonRange()

	mop := NewMatchedOrderingPart(pid, v, cr, MatchedSortOrderAscending)

	if mop.GetParameterId() != pid {
		t.Fatal("parameterId mismatch")
	}
	if mop.GetValue() != v {
		t.Fatal("value mismatch")
	}
	if mop.GetComparisonRange() != cr {
		t.Fatal("comparisonRange mismatch")
	}
	if mop.GetComparisonRangeType() != predicates.ComparisonRangeEmpty {
		t.Fatal("comparisonRangeType should be empty")
	}
	if mop.GetMatchedSortOrder() != MatchedSortOrderAscending {
		t.Fatal("matchedSortOrder mismatch")
	}
}

func TestNewMatchedOrderingPart_NilComparisonRangeDefaultsToEmpty(t *testing.T) {
	t.Parallel()
	pid := values.NamedCorrelationIdentifier("p2")
	v := &values.FieldValue{Field: "col_b", Typ: values.UnknownType}

	mop := NewMatchedOrderingPart(pid, v, nil, MatchedSortOrderDescending)

	if !mop.GetComparisonRange().IsEmpty() {
		t.Fatal("nil comparisonRange should default to empty")
	}
	if mop.GetMatchedSortOrder() != MatchedSortOrderDescending {
		t.Fatal("sort order mismatch")
	}
}

func TestMatchedOrderingPart_Demote(t *testing.T) {
	t.Parallel()
	pid := values.NamedCorrelationIdentifier("p3")
	v := &values.FieldValue{Field: "col_c", Typ: values.UnknownType}

	// Build an equality range.
	eqComp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42))
	cr := predicates.EmptyComparisonRange()
	res := cr.Merge(&eqComp)
	if !res.Ok {
		t.Fatal("merge should succeed")
	}
	cr = res.Range
	if !cr.IsEquality() {
		t.Fatal("range should be equality")
	}

	mop := NewMatchedOrderingPart(pid, v, cr, MatchedSortOrderAscending)
	demoted := mop.Demote()

	// Demoted part has empty range.
	if !demoted.GetComparisonRange().IsEmpty() {
		t.Fatal("demoted range should be empty")
	}
	// Preserves other fields.
	if demoted.GetParameterId() != pid {
		t.Fatal("demoted parameterId mismatch")
	}
	if demoted.GetValue() != v {
		t.Fatal("demoted value mismatch")
	}
	if demoted.GetMatchedSortOrder() != MatchedSortOrderAscending {
		t.Fatal("demoted sort order mismatch")
	}
	// Original unchanged.
	if !mop.GetComparisonRange().IsEquality() {
		t.Fatal("original should still be equality")
	}
}

func TestMatchedOrderingPart_DemotePanicsOnNonEquality(t *testing.T) {
	t.Parallel()
	pid := values.NamedCorrelationIdentifier("p4")
	v := &values.FieldValue{Field: "col_d", Typ: values.UnknownType}

	// Empty range (not equality) should panic on Demote.
	mop := NewMatchedOrderingPart(pid, v, predicates.EmptyComparisonRange(), MatchedSortOrderAscending)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Demote on non-equality should panic")
		}
	}()
	mop.Demote()
}

func TestMatchedOrderingPart_String(t *testing.T) {
	t.Parallel()
	pid := values.NamedCorrelationIdentifier("p5")
	v := &values.FieldValue{Field: "col_e", Typ: values.UnknownType}
	mop := NewMatchedOrderingPart(pid, v, nil, MatchedSortOrderDescending)

	s := mop.String()
	if s == "" {
		t.Fatal("String() should not be empty")
	}
	// Should contain the arrow indicator for descending.
	if !matchedOrderingPartContains(s, "↓") {
		t.Fatalf("expected descending arrow in %q", s)
	}
}

func matchedOrderingPartContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
