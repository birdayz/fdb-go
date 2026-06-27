package plans

import (
	"strings"
	"testing"

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
	keys := []values.Value{&values.FieldValue{Field: "group_id", Typ: values.NotNullLong}}
	rv := &values.FieldValue{Field: "result", Typ: values.NotNullString}
	p := NewRecordQueryMultiIntersectionOnValuesPlan(
		[]RecordQueryPlan{a, b, c}, keys, rv,
	)
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
	keys := []values.Value{&values.FieldValue{Field: "pk", Typ: values.UnknownType}}
	rv := &values.FieldValue{Field: "rv", Typ: values.UnknownType}
	p := NewRecordQueryMultiIntersectionOnValuesPlan(children, keys, rv)

	// Mutate originals — plan should be unaffected.
	children[0] = stub("Z")
	keys[0] = &values.FieldValue{Field: "xx", Typ: values.UnknownType}

	if p.GetChildren()[0].Explain() != "A" {
		t.Fatal("plan should have an independent copy of children")
	}
	if values.ExplainValue(p.GetComparisonKey()[0]) != values.ExplainValue(&values.FieldValue{Field: "pk", Typ: values.UnknownType}) {
		t.Fatal("plan should have an independent copy of comparison keys")
	}
}

func TestMultiIntersectionPlan_GetResultType_FromResultValue(t *testing.T) {
	t.Parallel()
	rv := &values.FieldValue{Field: "x", Typ: values.NotNullString}
	p := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, rv)
	if !values.NotNullString.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullString", p.GetResultType())
	}
}

func TestMultiIntersectionPlan_GetResultType_NilResultValue(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestMultiIntersectionPlan_Explain(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "gk", Typ: values.NotNullLong}}
	p := NewRecordQueryMultiIntersectionOnValuesPlan(
		[]RecordQueryPlan{stub("A"), stub("B")}, keys, nil,
	)
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
	p := NewRecordQueryMultiIntersectionOnValuesPlan(
		[]RecordQueryPlan{nil, stub("B")}, nil, nil,
	)
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil child", got)
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_SameShape(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "gk", Typ: values.NotNullLong}}
	rv := &values.FieldValue{Field: "rv", Typ: values.NotNullString}
	a := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys, rv)
	b := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys, rv)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same-shape multi intersections should be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_DifferentKeyCount(t *testing.T) {
	t.Parallel()
	keys1 := []values.Value{&values.FieldValue{Field: "a", Typ: values.UnknownType}}
	keys2 := []values.Value{
		&values.FieldValue{Field: "a", Typ: values.UnknownType},
		&values.FieldValue{Field: "b", Typ: values.UnknownType},
	}
	a := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys1, nil)
	b := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys2, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different key counts should NOT be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_DifferentKeys(t *testing.T) {
	t.Parallel()
	keys1 := []values.Value{&values.FieldValue{Field: "x", Typ: values.NotNullLong}}
	keys2 := []values.Value{&values.FieldValue{Field: "y", Typ: values.NotNullLong}}
	a := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys1, nil)
	b := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys2, nil)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different key values should NOT be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_DifferentResultValue(t *testing.T) {
	t.Parallel()
	rv1 := &values.FieldValue{Field: "sum", Typ: values.NotNullLong}
	rv2 := &values.FieldValue{Field: "count", Typ: values.NotNullLong}
	a := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, rv1)
	b := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, rv2)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different result values should NOT be equal")
	}
}

func TestMultiIntersectionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	mi := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, nil)
	u := NewRecordQueryUnionPlan(nil)
	if mi.EqualsWithoutChildren(u) {
		t.Fatal("MultiIntersection should not equal UnionPlan")
	}
}

func TestMultiIntersectionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	keys := []values.Value{&values.FieldValue{Field: "gk", Typ: values.UnknownType}}
	rv := &values.FieldValue{Field: "rv", Typ: values.NotNullLong}
	p := NewRecordQueryMultiIntersectionOnValuesPlan(nil, keys, rv)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestMultiIntersectionPlan_HashCodeWithoutChildren_DiffersForDifferentKeys(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryMultiIntersectionOnValuesPlan(nil,
		[]values.Value{&values.FieldValue{Field: "pk", Typ: values.UnknownType}}, nil)
	b := NewRecordQueryMultiIntersectionOnValuesPlan(nil,
		[]values.Value{
			&values.FieldValue{Field: "pk", Typ: values.UnknownType},
			&values.FieldValue{Field: "sk", Typ: values.UnknownType},
		}, nil)
	if a.HashCodeWithoutChildren() == b.HashCodeWithoutChildren() {
		t.Fatal("different key counts should (very likely) produce different hashes")
	}
}

func TestMultiIntersectionPlan_HashDiffersFromIntersection(t *testing.T) {
	t.Parallel()
	mi := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, nil)
	ip := NewRecordQueryIntersectionPlan(nil, nil)
	if mi.HashCodeWithoutChildren() == ip.HashCodeWithoutChildren() {
		t.Fatal("MultiIntersection and Intersection plans should hash differently")
	}
}

func TestMultiIntersectionPlan_HashDiffersFromUnion(t *testing.T) {
	t.Parallel()
	mi := NewRecordQueryMultiIntersectionOnValuesPlan(nil, nil, nil)
	u := NewRecordQueryUnionPlan(nil)
	if mi.HashCodeWithoutChildren() == u.HashCodeWithoutChildren() {
		t.Fatal("MultiIntersection and Union plans should hash differently")
	}
}
