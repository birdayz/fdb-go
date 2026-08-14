package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// bakedQovField builds a plan-time-BAKED FieldValue over a QuantifiedObjectValue
// child — the "QOV(alias).col @ ordinal" shape join predicates carry after
// construction-time ordinal binding.
func bakedQovField(t testing.TB, alias, col string, ord int) values.FieldValue {
	t.Helper()
	fields := make([]values.Field, ord+1)
	for i := range fields {
		fields[i] = values.Field{Name: "C" + string(rune('0'+i)), FieldType: values.NotNullLong, Ordinal: i}
	}
	fields[ord].Name = col
	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(alias),
		values.NewRecordType("", false, fields),
	)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%s): %v", alias, err)
	}
	resolved, err := values.ResolveFieldOrdinals(qov, []int{ord})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%s,%d): %v", alias, ord, err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("ResolveFieldOrdinals(%s,%d) = %T, want exact FieldValue", alias, ord, resolved)
	}
	return field
}

// fusedQovField builds an exact two-accessor QOV-child shape. It must DECLINE
// the hash fast path because its root addresses an intermediate record rather
// than a column in the supplied leg row.
func fusedQovField(t testing.TB, alias, col string) values.FieldValue {
	t.Helper()
	nested := values.NewRecordType("", false, []values.Field{{Name: col, FieldType: values.NotNullLong, Ordinal: 0}})
	root := values.NewRecordType("", false, []values.Field{{Name: "N", FieldType: nested, Ordinal: 0}})
	qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), root)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%s): %v", alias, err)
	}
	resolved, err := values.ResolveFieldOrdinals(qov, []int{0, 0})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%s,[0,0]): %v", alias, err)
	}
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		t.Fatalf("ResolveFieldOrdinals(%s,[0,0]) = %T, want exact FieldValue", alias, resolved)
	}
	return field
}

// TestExtractEquijoinOperands_SideClassification pins the NLJ hash-join operand
// extraction: the equijoin `t2.t1_id = t1.id` (inner=T2, outer=T1) must map the
// T1-correlated operand to the OUTER side and the T2-correlated operand to the
// INNER side regardless of which side of the predicate carries which alias.
// (The retired name-based extraction once picked the sides BACKWARDS for
// QOV-child operands — the hash index was then keyed on the wrong column and
// the ≥100-row fast path returned 0 rows, RFC-042 L3.)
func TestExtractEquijoinOperands_SideClassification(t *testing.T) {
	t.Parallel()

	innerOp := bakedQovField(t, "T2", "T1_ID", 1)
	outerOp := bakedQovField(t, "T1", "ID", 0)

	// p: t2.t1_id = t1.id  (inner side on the LHS, outer side on the RHS)
	p := predicates.NewComparisonPredicate(
		innerOp,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: outerOp},
	)
	gotOuter, gotInner := extractEquijoinOperands(
		[]predicates.QueryPredicate{p}, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
	if gotOuter != outerOp || gotInner != innerOp {
		t.Errorf("inner-on-LHS: got (outer=%v, inner=%v), want (T1.ID, T2.T1_ID)", gotOuter, gotInner)
	}

	// Mirror: t1.id = t2.t1_id
	p2 := predicates.NewComparisonPredicate(
		outerOp,
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: innerOp},
	)
	gotOuter, gotInner = extractEquijoinOperands(
		[]predicates.QueryPredicate{p2}, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
	if gotOuter != outerOp || gotInner != innerOp {
		t.Errorf("outer-on-LHS: got (outer=%v, inner=%v), want (T1.ID, T2.T1_ID)", gotOuter, gotInner)
	}
}

// TestExtractEquijoinOperands_Declines pins the decline shapes: the fast path
// keys ONLY on plan-time-baked single-accessor leg references — anything else
// falls to the linear predicate path (which evaluates, and loud-errors, the
// same operands).
func TestExtractEquijoinOperands_Declines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		pred predicates.QueryPredicate
	}{
		{
			// A fused path has an exact ordinal vector, but not the single leg-local
			// accessor required by the fast path.
			name: "fused operand",
			pred: predicates.NewComparisonPredicate(
				fusedQovField(t, "T2", "T1_ID"),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: bakedQovField(t, "T1", "ID", 0)},
			),
		},
		{
			// A computed scalar has no leg QOV to classify.
			name: "computed operand",
			pred: predicates.NewComparisonPredicate(
				&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}},
			),
		},
		{
			// Correlation naming NEITHER leg (a buried leg of a lower join).
			name: "foreign correlation",
			pred: predicates.NewComparisonPredicate(
				bakedQovField(t, "T9", "K", 0),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: bakedQovField(t, "T1", "ID", 0)},
			),
		},
		{
			// Same leg on both sides: not a cross-leg equijoin.
			name: "same-leg equality",
			pred: predicates.NewComparisonPredicate(
				bakedQovField(t, "T1", "A", 0),
				predicates.Comparison{Type: predicates.ComparisonEquals, Operand: bakedQovField(t, "T1", "B", 1)},
			),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, in := extractEquijoinOperands([]predicates.QueryPredicate{tc.pred}, values.NamedCorrelationIdentifier("T1"), values.NamedCorrelationIdentifier("T2"))
			if o != nil || in != nil {
				t.Errorf("%s: got (outer=%v, inner=%v), want decline (nil, nil)", tc.name, o, in)
			}
		})
	}
}

// TestEvalLegKey_ReadsLegLocalOrdinal pins the hash-key extraction semantics:
// the operand evaluates against its OWN leg row alone, by its baked leg-local
// ordinal — the exact resolution the linear predicate path performs per pair.
func TestEvalLegKey_ReadsLegLocalOrdinal(t *testing.T) {
	t.Parallel()

	corr := values.NamedCorrelationIdentifier("T2")
	op := bakedQovField(t, "T2", "T1_ID", 1)
	leg := &PositionalRow{
		Type:  positionalTypeFromNames([]string{"ID", "T1_ID"}),
		Slots: []any{int64(7), int64(42)},
	}

	v, ok := evalLegKey(op, corr, leg)
	if !ok {
		t.Fatal("evalLegKey: expected ok")
	}
	if v != int64(42) {
		t.Fatalf("evalLegKey: got %v, want 42 (slot 1, the baked ordinal)", v)
	}

	// Out-of-range ordinal: ok=false (the caller declines the fast path; the
	// linear path surfaces the loud OrdinalResolutionError).
	bad := bakedQovField(t, "T2", "MISSING", 9)
	if _, ok := evalLegKey(bad, corr, leg); ok {
		t.Fatal("evalLegKey: out-of-range ordinal must not resolve")
	}

	// A nil leg row never resolves.
	if _, ok := evalLegKey(op, corr, nil); ok {
		t.Fatal("evalLegKey: nil leg must not resolve")
	}
}
