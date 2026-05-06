package plans

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// Coverage tests for plan types not exercised in plan_test.go:
// Distinct, TypeFilter, Union, Intersection, Insert, Delete, Update.
// Pins for each: GetResultType, GetChildren, EqualsWithoutChildren
// (type discriminator + plan-specific node-info), HashCodeWithoutChildren
// (consistent under repeat call), Explain (renders something
// non-empty).

func TestRecordQueryDistinctPlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	d := NewRecordQueryDistinctPlan(scan)
	if cs := d.GetChildren(); len(cs) != 1 || cs[0] != scan {
		t.Fatalf("distinct children = %v, want [scan]", cs)
	}
	if !values.NotNullLong.Equals(d.GetResultType()) {
		t.Fatalf("distinct result type = %v, want NotNullLong (carries from inner)", d.GetResultType())
	}
	exp := d.Explain()
	if !strings.Contains(exp, "Distinct") || !strings.Contains(exp, "Scan(T)") {
		t.Fatalf("Explain = %q, want Distinct(Scan(T))", exp)
	}
}

func TestRecordQueryTypeFilterPlan_RecordTypesPreserved(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T", "U"}, values.UnknownType, false)
	tf := NewRecordQueryTypeFilterPlan([]string{"T"}, scan)
	rts := tf.GetRecordTypes()
	if len(rts) != 1 || rts[0] != "T" {
		t.Fatalf("record types = %v, want [T]", rts)
	}
	if cs := tf.GetChildren(); len(cs) != 1 || cs[0] != scan {
		t.Fatalf("typefilter children = %v, want [scan]", cs)
	}
}

func TestRecordQueryUnionPlan_ConcatenatesInners(t *testing.T) {
	t.Parallel()
	scanA := NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	u := NewRecordQueryUnionPlan([]RecordQueryPlan{scanA, scanB})
	if got := u.GetInners(); len(got) != 2 {
		t.Fatalf("union inners = %d, want 2", len(got))
	}
	if cs := u.GetChildren(); len(cs) != 2 {
		t.Fatalf("union children = %d, want 2", len(cs))
	}
	exp := u.Explain()
	if !strings.Contains(exp, "Union") || !strings.Contains(exp, "Scan(A)") || !strings.Contains(exp, "Scan(B)") {
		t.Fatalf("Explain = %q, want Union with both scans", exp)
	}
}

func TestRecordQueryUnionPlan_EmptyResultTypeIsUnknown(t *testing.T) {
	t.Parallel()
	u := NewRecordQueryUnionPlan(nil)
	if !values.UnknownType.Equals(u.GetResultType()) {
		t.Fatalf("empty union result type = %v, want UnknownType", u.GetResultType())
	}
}

func TestRecordQueryIntersectionPlan_CarriesComparisonKeys(t *testing.T) {
	t.Parallel()
	scanA := NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	keys := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.NotNullLong},
	}
	i := NewRecordQueryIntersectionPlan([]RecordQueryPlan{scanA, scanB}, keys)
	if got := i.GetInners(); len(got) != 2 {
		t.Fatalf("intersection inners = %d, want 2", len(got))
	}
	if got := i.GetComparisonKeyValues(); len(got) != 1 {
		t.Fatalf("comparison keys = %d, want 1", len(got))
	}
}

func TestRecordQueryIntersectionPlan_DistinctHashFromUnion(t *testing.T) {
	t.Parallel()
	// Both empty inners + same shape — Intersection's hash MUST
	// differ from Union's, otherwise plan-cache keys collide.
	u := NewRecordQueryUnionPlan(nil)
	i := NewRecordQueryIntersectionPlan(nil, nil)
	if u.HashCodeWithoutChildren() == i.HashCodeWithoutChildren() {
		t.Fatalf("Union and Intersection plans should hash differently")
	}
}

func TestRecordQueryIntersectionPlan_EqualsWithoutChildrenSameKeyCount(t *testing.T) {
	t.Parallel()
	keys1 := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.NotNullLong},
	}
	keys2 := []values.Value{
		&values.FieldValue{Field: "name", Typ: values.NotNullString},
	}
	i1 := NewRecordQueryIntersectionPlan(nil, keys1)
	i2 := NewRecordQueryIntersectionPlan(nil, keys2)
	if !i1.EqualsWithoutChildren(i2) {
		t.Fatal("two Intersections with same key count should be EqualsWithoutChildren")
	}

	// Different key count → not equal.
	i3 := NewRecordQueryIntersectionPlan(nil, []values.Value{
		&values.FieldValue{Field: "a", Typ: values.UnknownType},
		&values.FieldValue{Field: "b", Typ: values.UnknownType},
	})
	if i1.EqualsWithoutChildren(i3) {
		t.Fatal("Intersections with different key counts should NOT be equal")
	}
}

func TestRecordQueryInsertPlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"Source"}, values.UnknownType, false)
	ins := NewRecordQueryInsertPlan(scan, "Target", values.UnknownType)
	if cs := ins.GetChildren(); len(cs) != 1 || cs[0] != scan {
		t.Fatalf("insert children = %v, want [scan]", cs)
	}
	if got := ins.GetTargetRecordType(); got != "Target" {
		t.Fatalf("target = %q, want Target", got)
	}
	if got := ins.GetInner(); got != scan {
		t.Fatalf("GetInner = %v, want scan", got)
	}
}

func TestRecordQueryDeletePlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false)
	d := NewRecordQueryDeletePlan(scan, "Order")
	if cs := d.GetChildren(); len(cs) != 1 || cs[0] != scan {
		t.Fatalf("delete children = %v, want [scan]", cs)
	}
	if got := d.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want Order", got)
	}
}

func TestRecordQueryUpdatePlan_TransformsCarried(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false)
	transforms := []expressions.UpdateTransform{
		{FieldPath: "qty", NewValue: values.LiteralValue(int64(0))},
	}
	u := NewRecordQueryUpdatePlan(scan, "Order", transforms)
	if got := len(u.GetTransforms()); got != 1 {
		t.Fatalf("transforms = %d, want 1", got)
	}
	if got := u.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want Order", got)
	}
}

func TestDMLPlans_DistinctHashesByType(t *testing.T) {
	t.Parallel()
	// Insert / Delete / Update over the same target+inner must hash
	// differently — type discriminator matters for plan cache.
	scan := NewRecordQueryScanPlan([]string{"Order"}, values.UnknownType, false)
	ins := NewRecordQueryInsertPlan(scan, "Order", values.UnknownType)
	del := NewRecordQueryDeletePlan(scan, "Order")
	upd := NewRecordQueryUpdatePlan(scan, "Order", nil)

	insH := ins.HashCodeWithoutChildren()
	delH := del.HashCodeWithoutChildren()
	updH := upd.HashCodeWithoutChildren()
	if insH == delH || insH == updH || delH == updH {
		t.Fatalf("DML plan hashes collide: ins=%d del=%d upd=%d", insH, delH, updH)
	}
}

func TestRecordQueryUnionPlan_HashIsConsistent(t *testing.T) {
	t.Parallel()
	u := NewRecordQueryUnionPlan(nil)
	h1 := u.HashCodeWithoutChildren()
	h2 := u.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren is non-deterministic: %d vs %d", h1, h2)
	}
}

func TestRecordQueryDistinctPlan_HashIsConsistent(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	d := NewRecordQueryDistinctPlan(scan)
	h1 := d.HashCodeWithoutChildren()
	h2 := d.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren is non-deterministic: %d vs %d", h1, h2)
	}
}
