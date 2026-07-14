package values

import "testing"

// TestColumnNameValue_IgnoresBakedOrdinals pins the RFC-173 item C rendering
// split: ExplainValue keeps the baked `#<ordinal>` accessor discriminator
// (EXPLAIN/debug — collapsing two different reads is a bug there), while
// ColumnNameValue — the NAME-derivation rendering every output-column-name
// derivation goes through — renders the SAME text for a lazy and a baked
// reference. Without the split, plan-time ordinal binding changes derived
// column names ("SUM(AMOUNT)" vs "SUM(AMOUNT#2)") and the naming lockstep
// between layers that render different instances of one reference breaks.
func TestColumnNameValue_IgnoresBakedOrdinals(t *testing.T) {
	t.Parallel()

	lazy := NewFlatFieldValue("AMOUNT", UnknownType)
	baked := NewFieldValueWithResolvedOrdinal("AMOUNT", 2, UnknownType)

	if got := ExplainValue(baked); got != "AMOUNT#2" {
		t.Fatalf("ExplainValue(baked) = %q, want AMOUNT#2 (ordinal discriminator kept)", got)
	}
	if got := ColumnNameValue(baked); got != "AMOUNT" {
		t.Fatalf("ColumnNameValue(baked) = %q, want AMOUNT (ordinal-free)", got)
	}
	if ColumnNameValue(lazy) != ColumnNameValue(baked) {
		t.Fatalf("lazy vs baked ColumnNameValue drift: %q vs %q",
			ColumnNameValue(lazy), ColumnNameValue(baked))
	}

	// A COMPUTED tree over baked references: the composite rendering must be
	// ordinal-free end-to-end (the aggregate-operand naming case).
	sum := &ArithmeticValue{Op: OpMul, Left: baked, Right: NewFieldValueWithResolvedOrdinal("QTY", 3, UnknownType)}
	if got := ColumnNameValue(sum); got != "(AMOUNT * QTY)" {
		t.Fatalf("ColumnNameValue(computed) = %q, want (AMOUNT * QTY)", got)
	}
	if got := ExplainValue(sum); got != "(AMOUNT#2 * QTY#3)" {
		t.Fatalf("ExplainValue(computed) = %q, want (AMOUNT#2 * QTY#3)", got)
	}

	// Multi-accessor baked path: every step renders name-only in the
	// column-name form, name#ordinal in the explain form.
	multi := &FieldValue{
		Field: "CITY",
		Typ:   UnknownType,
		Resolved: &FieldPath{Accessors: []ResolvedAccessor{
			{Field: "ADDR", Ordinal: 1},
			{Field: "CITY", Ordinal: 0},
		}},
	}
	if got := ColumnNameValue(multi); got != "ADDR.CITY" {
		t.Fatalf("ColumnNameValue(multi) = %q, want ADDR.CITY", got)
	}
	if got := ExplainValue(multi); got != "ADDR#1.CITY#0" {
		t.Fatalf("ExplainValue(multi) = %q, want ADDR#1.CITY#0", got)
	}
}
