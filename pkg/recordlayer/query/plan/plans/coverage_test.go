package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Coverage tests for plan types not exercised in plan_test.go:
// Distinct, TypeFilter, Union, Intersection, Insert, Delete, Update.
// Pins for each: GetResultType, GetChildren, EqualsWithoutChildren
// (type discriminator + plan-specific node-info), HashCodeWithoutChildren
// (consistent under repeat call), Explain (renders something
// non-empty).

func TestRecordQueryDistinctPlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	})
	d := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(scan)
	})
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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T", "U"}, exactTestRecordType(), false)
	})
	tf := mustChecked(t, func() (*RecordQueryTypeFilterPlan, error) {
		return NewRecordQueryTypeFilterPlan([]string{"T"}, scan)
	})
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
	scanA := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"A"}, exactTestRecordType(), false)
	})
	scanB := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"B"}, exactTestRecordType(), false)
	})
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{scanA, scanB})
	})
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

func TestSetPlans_RejectEmptyChildren(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryUnionPlan(nil); err == nil {
		t.Fatal("expected empty union children to be rejected")
	}
	if _, err := NewRecordQueryIntersectionPlan(nil, nil); err == nil {
		t.Fatal("expected empty intersection children to be rejected")
	}
}

func TestRecordQueryIntersectionPlan_CarriesComparisonKeys(t *testing.T) {
	t.Parallel()
	scanA := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"A"}, exactTestRecordType(), false)
	})
	scanB := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"B"}, exactTestRecordType(), false)
	})
	keys := []values.Value{testField(t, "id", values.NotNullLong)}
	i := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{scanA, scanB}, keys)
	})
	if got := i.GetInners(); len(got) != 2 {
		t.Fatalf("intersection inners = %d, want 2", len(got))
	}
	if got := i.GetComparisonKeyValues(); len(got) != 1 {
		t.Fatalf("comparison keys = %d, want 1", len(got))
	}
}

func TestRecordQueryIntersectionPlan_DistinctHashFromUnion(t *testing.T) {
	t.Parallel()
	// Same valid child shape — Intersection's hash MUST differ from Union's,
	// otherwise plan-cache keys collide.
	inner := stub("inner")
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{inner})
	})
	i := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan([]RecordQueryPlan{inner}, nil)
	})
	if u.HashCodeWithoutChildren() == i.HashCodeWithoutChildren() {
		t.Fatalf("Union and Intersection plans should hash differently")
	}
}

func TestRecordQueryIntersectionPlan_EqualsWithoutChildrenSameKeyCount(t *testing.T) {
	t.Parallel()
	keys1 := []values.Value{testField(t, "id", values.NotNullLong)}
	keys2 := []values.Value{testField(t, "name", values.NotNullString)}
	inners := []RecordQueryPlan{stub("left"), stub("right")}
	i1 := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan(inners, keys1)
	})
	i2 := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan(inners, keys2)
	})
	// Same key COUNT but different key Values → NOT equal (RFC-180 B2:
	// count-only identity collapsed different-key intersections in the memo).
	if i1.EqualsPlanWithoutChildren(i2) {
		t.Fatal("Intersections with different comparison keys should NOT be equal")
	}
	i1b := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan(inners, []values.Value{testField(t, "id", values.NotNullLong)})
	})
	if !i1.EqualsPlanWithoutChildren(i1b) {
		t.Fatal("Intersections with identical comparison keys should be equal")
	}

	// Different key count → not equal.
	i3 := mustChecked(t, func() (*RecordQueryIntersectionPlan, error) {
		return NewRecordQueryIntersectionPlan(inners, []values.Value{testField(t, "a", values.NullableLong), testField(t, "b", values.NullableLong)})
	})
	if i1.EqualsPlanWithoutChildren(i3) {
		t.Fatal("Intersections with different key counts should NOT be equal")
	}
}

func TestRecordQueryInsertPlan_WrapsInner(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Source"}, exactTestRecordType(), false)
	})
	ins := mustChecked(t, func() (*RecordQueryInsertPlan, error) {
		return NewRecordQueryInsertPlan(scan, "Target", exactTestRecordType())
	})
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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order"}, exactTestRecordType(), false)
	})
	d := mustChecked(t, func() (*RecordQueryDeletePlan, error) {
		return NewRecordQueryDeletePlan(scan, "Order")
	})
	if cs := d.GetChildren(); len(cs) != 1 || cs[0] != scan {
		t.Fatalf("delete children = %v, want [scan]", cs)
	}
	if got := d.GetTargetRecordType(); got != "Order" {
		t.Fatalf("target = %q, want Order", got)
	}
}

func TestRecordQueryUpdatePlan_TransformsCarried(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order"}, exactTestRecordType(), false)
	})
	transforms := []expressions.UpdateTransform{
		{FieldPath: "qty", NewValue: values.LiteralValue(int64(0))},
	}
	u := mustChecked(t, func() (*RecordQueryUpdatePlan, error) {
		return NewRecordQueryUpdatePlan(scan, "Order", transforms)
	})
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
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"Order"}, exactTestRecordType(), false)
	})
	ins := mustChecked(t, func() (*RecordQueryInsertPlan, error) {
		return NewRecordQueryInsertPlan(scan, "Order", exactTestRecordType())
	})
	del := mustChecked(t, func() (*RecordQueryDeletePlan, error) {
		return NewRecordQueryDeletePlan(scan, "Order")
	})
	upd := mustChecked(t, func() (*RecordQueryUpdatePlan, error) {
		return NewRecordQueryUpdatePlan(scan, "Order", nil)
	})

	insH := ins.HashCodeWithoutChildren()
	delH := del.HashCodeWithoutChildren()
	updH := upd.HashCodeWithoutChildren()
	if insH == delH || insH == updH || delH == updH {
		t.Fatalf("DML plan hashes collide: ins=%d del=%d upd=%d", insH, delH, updH)
	}
}

func TestRecordQueryUnionPlan_HashIsConsistent(t *testing.T) {
	t.Parallel()
	u := mustChecked(t, func() (*RecordQueryUnionPlan, error) {
		return NewRecordQueryUnionPlan([]RecordQueryPlan{stub("inner")})
	})
	h1 := u.HashCodeWithoutChildren()
	h2 := u.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren is non-deterministic: %d vs %d", h1, h2)
	}
}

func TestRecordQueryDistinctPlan_HashIsConsistent(t *testing.T) {
	t.Parallel()
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	d := mustChecked(t, func() (*RecordQueryDistinctPlan, error) {
		return NewRecordQueryDistinctPlan(scan)
	})
	h1 := d.HashCodeWithoutChildren()
	h2 := d.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren is non-deterministic: %d vs %d", h1, h2)
	}
}
