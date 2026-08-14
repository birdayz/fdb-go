package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// RecordQueryMultiIntersectionOnValuesPlan
// ---------------------------------------------------------------------------

func TestMultiIntersectionPlan_Construction(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	c := stub("C")
	keys := []values.Value{testField(t, "group_id", values.NotNullLong)}
	rv := testField(t, "result", values.NotNullString)
	p := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan(
			[]RecordQueryPlan{a, b, c}, keys, rv,
		)
	})
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetChildren()) != 3 {
		t.Fatalf("GetChildren() len = %d, want 3", len(p.GetChildren()))
	}
	if len(p.GetComparisonKey()) != 1 {
		t.Fatalf("GetComparisonKey() len = %d, want 1", len(p.GetComparisonKey()))
	}
	if p.GetResultValue() != rv {
		t.Fatal("GetResultValue() does not return the provided resultValue")
	}
}

func TestMultiIntersectionPlan_CopiesSlices(t *testing.T) {
	t.Parallel()
	children := []RecordQueryPlan{stub("A"), stub("B")}
	keys := []values.Value{testField(t, "pk", values.NullableLong)}
	rv := testField(t, "rv", values.NullableLong)
	p := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan(children, keys, rv)
	})

	// Mutate originals — plan should be unaffected.
	children[0] = stub("Z")
	keys[0] = testField(t, "xx", values.NullableLong)

	if p.GetChildren()[0].Explain() != "A" {
		t.Fatal("plan should have an independent copy of children")
	}
	if values.ExplainValue(p.GetComparisonKey()[0]) != values.ExplainValue(testField(t, "pk", values.NullableLong)) {
		t.Fatal("plan should have an independent copy of comparison keys")
	}
}

func TestMultiIntersectionPlan_RelinkRebasesRetainedProgramsAndDrivingAlias(t *testing.T) {
	t.Parallel()
	rowType := exactTestRecordType()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, rowType, false)
	})
	oldAliases := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("multi_old_a"),
		values.NamedCorrelationIdentifier("multi_old_b"),
	}
	newAliases := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("multi_new_a"),
		values.NamedCorrelationIdentifier("multi_new_b"),
	}
	oldQs := make([]expressions.Quantifier, 2)
	newQs := make([]expressions.Quantifier, 2)
	for i := range oldQs {
		oldQs[i] = expressions.NamedPhysicalQuantifier(
			oldAliases[i], expressions.FinalOfAtStage(scan, expressions.StageCanonical))
		newQs[i] = expressions.NamedPhysicalQuantifier(
			newAliases[i], expressions.FinalOfAtStage(scan, expressions.StageCanonical))
	}
	key := testFieldIn(t, rowType, oldAliases[0].Name(), "K")
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name: "ID", Value: testFieldIn(t, rowType, oldAliases[0].Name(), "ID"),
		},
		values.RecordConstructorField{
			Name: "V", Value: testFieldIn(t, rowType, oldAliases[1].Name(), "V"),
		},
	)
	original := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlanFromQuantifiers(
			oldQs, []values.Value{key}, result)
	}).WithDrivingStream(oldAliases[1])

	relinkedExpr, err := original.WithQuantifiers(newQs)
	if err != nil {
		t.Fatalf("WithQuantifiers: %v", err)
	}
	relinked := relinkedExpr.(*RecordQueryMultiIntersectionOnValuesPlan)
	if relinked.GetDrivingAlias() != newAliases[1] || relinked.DrivingStreamIndex() != 1 {
		t.Fatalf("driving stream = (%s,%d), want (%s,1)",
			relinked.GetDrivingAlias(), relinked.DrivingStreamIndex(), newAliases[1])
	}
	requireValueCorrelations(t, relinked.GetComparisonKey()[0], newAliases[0])
	requireValueCorrelations(t, relinked.GetResultValue(), newAliases...)
	requireValueCorrelations(t, original.GetComparisonKey()[0], oldAliases[0])
	requireValueCorrelations(t, original.GetResultValue(), oldAliases...)
	if relinked.GetResultValue() == original.GetResultValue() {
		t.Fatal("relink retained the source result program")
	}
}

func requireValueCorrelations(
	t *testing.T,
	value values.Value,
	want ...values.CorrelationIdentifier,
) {
	t.Helper()
	correlated := values.GetCorrelatedToOfValue(value)
	if len(correlated) != len(want) {
		t.Fatalf("value correlations = %v, want %v", correlated, want)
	}
	for _, alias := range want {
		if _, ok := correlated[alias]; !ok {
			t.Fatalf("value correlations = %v, missing %s", correlated, alias)
		}
	}
}

func TestMultiIntersectionPlan_GetResultType_FromResultValue(t *testing.T) {
	t.Parallel()
	rv := testField(t, "x", values.NotNullString)
	p := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("Inner")}, nil, rv)
	})
	if !values.NotNullString.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullString", p.GetResultType())
	}
}

func TestMultiIntersectionPlan_ConstructorRejectsNilResultValue(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryMultiIntersectionOnValuesPlan(
		[]RecordQueryPlan{stub("Inner")}, nil, nil); err == nil {
		t.Fatal("constructor accepted a nil result value")
	}
}

func TestMultiIntersectionPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "gk", values.NotNullLong)}
	p := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan(
			[]RecordQueryPlan{stub("A"), stub("B")}, keys, exactEmptyRecordValue(),
		)
	})
	got := p.Explain()
	if !strings.Contains(got, "MultiIntersection") {
		t.Fatalf("Explain = %q, missing 'MultiIntersection'", got)
	}
	if !strings.Contains(got, "A") || !strings.Contains(got, "B") {
		t.Fatalf("Explain = %q, missing child labels", got)
	}
	if !strings.Contains(got, "keys=") {
		t.Fatalf("Explain = %q, missing 'keys='", got)
	}
}

func TestMultiIntersectionPlan_Explain_NilChild(t *testing.T) {
	t.Parallel()
	p := &RecordQueryMultiIntersectionOnValuesPlan{
		childQs: QuantifiersOverPlans([]RecordQueryPlan{nil, stub("B")}),
	}
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil child", got)
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_SameShape(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "gk", values.NotNullLong)}
	rv := testField(t, "rv", values.NotNullString)
	a := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("A")}, keys, rv)
	})
	b := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("B")}, keys, rv)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same-shape multi intersections should be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_DifferentKeyCount(t *testing.T) {
	t.Parallel()
	keys1 := []values.Value{testField(t, "a", values.NullableLong)}
	keys2 := []values.Value{testField(t, "a", values.NullableLong), testField(t, "b", values.NullableLong)}
	rv := exactEmptyRecordValue()
	a := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("A")}, keys1, rv)
	})
	b := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("B")}, keys2, rv)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different key counts should NOT be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_DifferentKeys(t *testing.T) {
	t.Parallel()
	keys1 := []values.Value{testField(t, "x", values.NotNullLong)}
	keys2 := []values.Value{testField(t, "y", values.NotNullLong)}
	rv := exactEmptyRecordValue()
	a := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("A")}, keys1, rv)
	})
	b := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("B")}, keys2, rv)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different key values should NOT be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_DifferentResultValue(t *testing.T) {
	t.Parallel()
	rv1 := testField(t, "sum", values.NotNullLong)
	rv2 := testField(t, "count", values.NotNullLong)
	a := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("A")}, nil, rv1)
	})
	b := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("B")}, nil, rv2)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different result values should NOT be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	mi := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("MultiInner")}, nil, exactEmptyRecordValue())
	})
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("UnionInner")})
	})
	if mi.EqualsPlanWithoutChildren(u) {
		t.Fatal("MultiIntersection should not equal UnionPlan")
	}
}

func TestMultiIntersectionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{testField(t, "gk", values.NullableLong)}
	rv := testField(t, "rv", values.NotNullLong)
	p := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("Inner")}, keys, rv)
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestMultiIntersectionPlan_HashCodeWithoutChildren_DiffersForDifferentKeys(t *testing.T) {
	t.Parallel()
	rv := exactEmptyRecordValue()
	a := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("A")},
			[]values.Value{testField(t, "pk", values.NullableLong)}, rv)
	})
	b := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("B")},
			[]values.Value{testField(t, "pk", values.NullableLong), testField(t, "sk", values.NullableLong)}, rv)
	})
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different key counts should (very likely) produce different hashes")
	}
}

func TestMultiIntersectionPlan_HashDiffersFromIntersection(t *testing.T) {
	t.Parallel()
	mi := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("MultiInner")}, nil, exactEmptyRecordValue())
	})
	ip := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{stub("IntersectionInner")}, nil)
	})
	if mi.HashCodeWithoutChildren() == ip.HashCodeWithoutChildren() {
		t.Fatal("MultiIntersection and Intersection plans should hash differently")
	}
}

func TestMultiIntersectionPlan_HashDiffersFromUnion(t *testing.T) {
	t.Parallel()
	mi := mustChecked(t, func() (*RecordQueryMultiIntersectionOnValuesPlan, error) {
		return NewRecordQueryMultiIntersectionOnValuesPlan([]RecordQueryPlan{stub("MultiInner")}, nil, exactEmptyRecordValue())
	})
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("UnionInner")})
	})
	if mi.HashCodeWithoutChildren() == u.HashCodeWithoutChildren() {
		t.Fatal("MultiIntersection and Union plans should hash differently")
	}
}
