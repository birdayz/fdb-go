package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// A value that reads the outer leg by its direct binding AND a leg buried in
// the inner FlatMap must cross both lineages. Naming the outer binding is not
// evidence that the value is outer-only: the inner's buried legs are reached
// through the inner binding without ever naming it.
func TestFlatMapPlan_MixedOuterAndBuriedInnerValueCrossesBothLineages(t *testing.T) {
	t.Parallel()
	upper, lRoot, mRoot := mixedLineageFixture(t)
	// L.A * M.S: one operand on the direct outer binding, one buried in MR.
	mixed := &values.ArithmeticValue{Op: values.OpMul, Left: mustField(t, lRoot, 1), Right: mustField(t, mRoot, 1)}
	reanchored, err := upper.reanchorInputValueToOutput(mixed)
	if err != nil {
		t.Fatal(err)
	}
	requireOutputOrdinals(t, reanchored, requireProvidedLayout(t, upper).Carrier(), 1, 3)
}

// A partially reanchored program — one operand already on the output row, one
// still source-relative — must not let its current part decide that no child
// lineage applies to the rest. This is the shape a filter's predicate takes when
// it was built over a not-yet-selected inner and relinked afterwards.
func TestReanchorCurrentValueForInput_MixedCurrentAndSourceRelativeProgram(t *testing.T) {
	t.Parallel()
	upper, _, mRoot := mixedLineageFixture(t)
	carrier := requireProvidedLayout(t, upper).Carrier()
	inputQ := expressions.NewPhysicalQuantifier(
		expressions.FinalOfAtStage(upper, expressions.StageCanonical))
	mixed := &values.ArithmeticValue{Op: values.OpMul, Left: mustField(t, carrier, 1), Right: mustField(t, mRoot, 1)}
	reanchored, err := reanchorCurrentValueForInput(mixed, inputQ)
	if err != nil {
		t.Fatal(err)
	}
	requireOutputOrdinals(t, reanchored, carrier, 1, 3)
}

func mustField(t *testing.T, root values.Value, path ...int) values.Value {
	t.Helper()
	v, err := values.ResolveFieldOrdinals(root, path)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func requireOutputOrdinals(t *testing.T, reanchored values.Value, carrier values.QuantifiedObjectValue, want ...int) {
	t.Helper()
	seen := map[int]struct{}{}
	values.WalkValue(reanchored, func(node values.Value) bool {
		f, ok := values.AsFieldValue(node)
		if !ok {
			return true
		}
		if f.ChildValue() != carrier {
			t.Fatalf("operand %s stayed on %v, want the FlatMap's exact output carrier",
				values.ExplainValue(node), f.ChildValue())
		}
		path := f.Path().Ordinals()
		if len(path) != 1 {
			t.Fatalf("operand path = %v, want one output ordinal", path)
		}
		seen[path[0]] = struct{}{}
		return false
	})
	if len(seen) != len(want) {
		t.Fatalf("output ordinals = %v, want %v", seen, want)
	}
	for _, ordinal := range want {
		if _, ok := seen[ordinal]; !ok {
			t.Fatalf("output ordinals = %v, want %v", seen, want)
		}
	}
}

// mixedLineageFixture is FlatMap(L, MR) whose inner MR is FlatMap(M, R); the
// upper result row is [L.ID, L.A, M.ID, M.S, R.ID].
func mixedLineageFixture(t *testing.T) (*RecordQueryFlatMapPlan, values.QuantifiedObjectValue, values.QuantifiedObjectValue) {
	t.Helper()
	newLeg := func(alias string, fields ...values.Field) (values.CorrelationIdentifier, expressions.Quantifier, values.QuantifiedObjectValue) {
		t.Helper()
		correlation := values.NamedCorrelationIdentifier(alias)
		scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{alias}, values.NewRecordType(alias, false, fields), false)
		})
		quantifier := expressions.NamedPhysicalQuantifier(
			correlation, expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		root, err := quantifier.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("%s flowed object: %v", alias, err)
		}
		return correlation, quantifier, root
	}
	lAlias, lQ, lRoot := newLeg("L",
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "A", FieldType: values.NullableLong})
	mAlias, mQ, mRoot := newLeg("M",
		values.Field{Name: "ID", FieldType: values.NotNullLong},
		values.Field{Name: "S", FieldType: values.NullableLong})
	rAlias, rQ, rRoot := newLeg("R",
		values.Field{Name: "ID", FieldType: values.NotNullLong})

	inner := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			mQ, rQ, mAlias, rAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "_0", Value: mRoot},
				values.RecordConstructorField{Name: "_1", Value: rRoot}), false)
	})
	innerAlias := values.NamedCorrelationIdentifier("MR")
	innerQ := expressions.NamedPhysicalQuantifier(
		innerAlias, expressions.FinalOfAtStage(inner, expressions.StageCanonical))
	innerRoot, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	upper := mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
		return NewRecordQueryFlatMapPlanFromQuantifiers(
			lQ, innerQ, lAlias, innerAlias,
			values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "ID", Value: mustField(t, lRoot, 0)},
				values.RecordConstructorField{Name: "A", Value: mustField(t, lRoot, 1)},
				values.RecordConstructorField{Name: "ID", Value: mustField(t, innerRoot, 0, 0)},
				values.RecordConstructorField{Name: "S", Value: mustField(t, innerRoot, 0, 1)},
				values.RecordConstructorField{Name: "ID", Value: mustField(t, innerRoot, 1, 0)}), false)
	})
	return upper, lRoot, mRoot
}
