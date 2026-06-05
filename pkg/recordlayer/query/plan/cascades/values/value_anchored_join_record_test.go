package values

import "testing"

// TestNewAnchoredJoinRecord_ComposeResolvesEveryColumn pins RFC-077's name-miss guard: for
// every column the SARG/derivation pulls up, composeFieldOverConstructor over the anchored RC
// must resolve to the leg FieldValue (never nil — the simplifier_value.go silent-nil
// landmine). Unique columns resolve by bare name; cross-leg duplicates by ALIAS.COL.
func TestNewAnchoredJoinRecord_ComposeResolvesEveryColumn(t *testing.T) {
	a := NamedCorrelationIdentifier("A")
	b := NamedCorrelationIdentifier("B")
	legs := []AnchoredJoinLeg{
		{Alias: a, Columns: []Field{{Name: "ID", FieldType: UnknownType}, {Name: "NAME", FieldType: UnknownType}}},
		{Alias: b, Columns: []Field{{Name: "ID", FieldType: UnknownType}, {Name: "AMOUNT", FieldType: UnknownType}}},
	}
	rc := NewAnchoredJoinRecord(legs)

	resolvesToLegField := func(field string) {
		t.Helper()
		got := composeFieldOverConstructor(NewFieldValue(rc, field, UnknownType))
		if got == nil {
			t.Fatalf("field %q must resolve over the anchored RC, got nil (silent-nil landmine)", field)
		}
		fv, ok := got.(*FieldValue)
		if !ok {
			t.Fatalf("field %q: expected a leg *FieldValue, got %T", field, got)
		}
		if _, ok := fv.Child.(*QuantifiedObjectValue); !ok {
			t.Fatalf("field %q: leg FieldValue must be anchored to a QuantifiedObjectValue, got child %T", field, fv.Child)
		}
	}

	// Unique columns resolve by bare name.
	resolvesToLegField("NAME")
	resolvesToLegField("AMOUNT")
	// The cross-leg duplicate column ID is disambiguated as ALIAS.COL and each resolves.
	resolvesToLegField("A.ID")
	resolvesToLegField("B.ID")

	// Each duplicate's leg FieldValue is anchored to the RIGHT leg.
	aID := composeFieldOverConstructor(NewFieldValue(rc, "A.ID", UnknownType)).(*FieldValue)
	if qov, _ := aID.Child.(*QuantifiedObjectValue); qov == nil || qov.Correlation != a {
		t.Fatalf("A.ID must anchor to leg A, got %+v", aID.Child)
	}
	bID := composeFieldOverConstructor(NewFieldValue(rc, "B.ID", UnknownType)).(*FieldValue)
	if qov, _ := bID.Child.(*QuantifiedObjectValue); qov == nil || qov.Correlation != b {
		t.Fatalf("B.ID must anchor to leg B, got %+v", bID.Child)
	}
}

// TestNewAnchoredJoinRecord_EvaluatesNameKeyedRow pins that the anchored RC's Evaluate yields
// a column-named row (the SELECT */flow-through case where the RC survives to runtime).
func TestNewAnchoredJoinRecord_EvaluatesNameKeyedRow(t *testing.T) {
	a := NamedCorrelationIdentifier("A")
	legs := []AnchoredJoinLeg{
		{Alias: a, Columns: []Field{{Name: "NAME", FieldType: UnknownType}}},
	}
	rc := NewAnchoredJoinRecord(legs)
	row := rc.Evaluate(staticBinder{a: map[string]any{"NAME": "alice"}})
	m, ok := row.(map[string]any)
	if !ok {
		t.Fatalf("anchored RC Evaluate must yield a name-keyed map, got %T", row)
	}
	if m["NAME"] != "alice" {
		t.Fatalf("expected NAME=alice in the anchored row, got %v", m)
	}
}

// staticBinder is a minimal CorrelationBinder for the Evaluate test.
type staticBinder map[CorrelationIdentifier]map[string]any

func (s staticBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	v, ok := s[id]
	return v, ok
}
