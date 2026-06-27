package expressions

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// --- InsertExpression -------------------------------------------------------

func TestInsert_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ins := NewInsertExpression(q, "Order", values.UnknownType)
	if ins.GetTargetRecordType() != "Order" {
		t.Fatalf("targetRecordType=%q, want Order", ins.GetTargetRecordType())
	}
	if ins.GetTargetType() != values.UnknownType {
		t.Fatal("targetType not preserved")
	}
	if ins.CanCorrelate() {
		t.Fatal("INSERT should not anchor a correlation")
	}
}

func TestInsert_NilTargetType(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ins := NewInsertExpression(q, "Order", nil)
	if ins.GetTargetType() != values.UnknownType {
		t.Fatal("nil targetType not normalised to UnknownType")
	}
}

func TestInsert_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := NewInsertExpression(q, "Order", values.UnknownType)
	b := NewInsertExpression(q, "Order", values.UnknownType)
	c := NewInsertExpression(q, "Customer", values.UnknownType)
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("identical INSERTs reported unequal")
	}
	if a.EqualsWithoutChildren(c, EmptyAliasMap()) {
		t.Fatal("INSERTs with different target reported equal")
	}
	if a.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("INSERT reported equal to non-INSERT")
	}
}

// --- DeleteExpression -------------------------------------------------------

func TestDelete_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	del := NewDeleteExpression(q, "Order")
	if del.GetTargetRecordType() != "Order" {
		t.Fatalf("targetRecordType=%q, want Order", del.GetTargetRecordType())
	}
	if del.CanCorrelate() {
		t.Fatal("DELETE should not anchor a correlation")
	}
}

func TestDelete_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := NewDeleteExpression(q, "Order")
	b := NewDeleteExpression(q, "Order")
	c := NewDeleteExpression(q, "Customer")
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("identical DELETEs reported unequal")
	}
	if a.EqualsWithoutChildren(c, EmptyAliasMap()) {
		t.Fatal("DELETEs with different target reported equal")
	}
	if a.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("DELETE reported equal to non-DELETE")
	}
}

func TestDelete_DistinctHashFromInsert(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ins := NewInsertExpression(q, "Order", values.UnknownType)
	del := NewDeleteExpression(q, "Order")
	if ins.HashCodeWithoutChildren() == del.HashCodeWithoutChildren() {
		t.Fatal("INSERT and DELETE on same target produced identical class-discriminating hashes")
	}
}

// --- UpdateExpression -------------------------------------------------------

func TestUpdate_CanonicalisesTransforms(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ts1 := []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
		{FieldPath: "active", NewValue: values.NewBooleanValue(false)},
	}
	upd := NewUpdateExpression(q, "Order", ts1)
	got := upd.GetTransforms()
	want := []string{"active", "name"} // sorted
	if !reflect.DeepEqual([]string{got[0].FieldPath, got[1].FieldPath}, want) {
		t.Fatalf("transform order=%v, want %v", []string{got[0].FieldPath, got[1].FieldPath}, want)
	}
}

func TestUpdate_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	src := []UpdateTransform{{FieldPath: "name", NewValue: values.NewBooleanValue(true)}}
	upd := NewUpdateExpression(q, "Order", src)
	src[0].FieldPath = "MUTATED"
	if upd.GetTransforms()[0].FieldPath != "name" {
		t.Fatal("constructor failed to defensively copy transforms")
	}
}

func TestUpdate_EqualsWithoutChildren_TextualOrderIndependent(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
		{FieldPath: "active", NewValue: values.NewBooleanValue(false)},
	})
	b := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "active", NewValue: values.NewBooleanValue(false)},
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	})
	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("UPDATEs with same SET-list in different order reported unequal")
	}
}

func TestUpdate_EqualsWithoutChildren_DifferentValue(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	})
	b := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(false)},
	})
	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("UPDATEs with different replacement Values reported equal")
	}
}

func TestUpdate_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	})
	b := NewUpdateExpression(q, "Order", []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	})
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("structurally identical UPDATEs produced different hashes")
	}
}

func TestUpdate_NotEqualToInsertOrDelete(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	upd := NewUpdateExpression(q, "Order", nil)
	ins := NewInsertExpression(q, "Order", values.UnknownType)
	del := NewDeleteExpression(q, "Order")
	if upd.HashCodeWithoutChildren() == ins.HashCodeWithoutChildren() {
		t.Fatal("UPDATE and INSERT collide on class-discriminating hash")
	}
	if upd.HashCodeWithoutChildren() == del.HashCodeWithoutChildren() {
		t.Fatal("UPDATE and DELETE collide on class-discriminating hash")
	}
}
