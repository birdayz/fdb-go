package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The physical union's constructor is the asserted bridge RFC-242 rests on:
// the translator aligns a union's legs onto one exact row, and this check is
// what makes an unaligned leg loud at construction instead of a silent
// re-typing at execution. It compares flowed types EXACTLY — a field name in
// another case is a different name — so a rename that folded a leg's names
// was rejected here, which is how the fuzz surfaced the defect. The check had
// no test driving it; these do.

func legTypeRow(name string, fields ...string) values.Type {
	out := make([]values.Field, len(fields))
	for i, f := range fields {
		out[i] = values.Field{Name: f, Ordinal: i, FieldType: values.NullableLong}
	}
	return values.NewRecordType(name, false, out)
}

func TestUnorderedUnion_RejectsLegsWhoseNamesDifferOnlyInCase(t *testing.T) {
	t.Parallel()
	lower, err := NewRecordQueryScanPlan([]string{"T"}, legTypeRow("Row", "id", "k"), false)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := NewRecordQueryScanPlan([]string{"T"}, legTypeRow("Row", "ID", "K"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{
		QuantifierOverPlan(lower), QuantifierOverPlan(upper),
	})
	if err == nil {
		t.Fatal("legs RECORD<id, k> and RECORD<ID, K> were accepted as one union: names compared under a fold, not exactly")
	}
	if !strings.Contains(err.Error(), "input quantifier 0 type") || !strings.Contains(err.Error(), "disagrees with input quantifier 1") {
		t.Fatalf("rejection names the wrong thing: %v", err)
	}
}

func TestUnorderedUnion_AcceptsLegsWithIdenticalRows(t *testing.T) {
	t.Parallel()
	// Positive control for the test above: the same two names, the same
	// spelling, one from a named record and one anonymous — Equals compares
	// the fields, and both legs flow the same fields.
	left, err := NewRecordQueryScanPlan([]string{"T"}, legTypeRow("Row", "id", "k"), false)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewRecordQueryScanPlan([]string{"T"}, legTypeRow("", "id", "k"), false)
	if err != nil {
		t.Fatal(err)
	}
	union, err := NewRecordQueryUnorderedUnionPlanFromQuantifiers([]expressions.Quantifier{
		QuantifierOverPlan(left), QuantifierOverPlan(right),
	})
	if err != nil {
		t.Fatalf("identical legs rejected: %v", err)
	}
	if got := union.GetResultType(); !got.Equals(left.GetResultType()) {
		t.Fatalf("union states %s, want leg 0's %s", got, left.GetResultType())
	}
}
