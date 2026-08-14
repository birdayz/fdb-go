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
	target := testRecordType()
	ins := mustExpression(NewInsertExpression(q, "Order", target))
	if ins.GetTargetRecordType() != "Order" {
		t.Fatalf("targetRecordType=%q, want Order", ins.GetTargetRecordType())
	}
	if !ins.GetTargetType().Equals(target) {
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
	ins, err := NewInsertExpression(q, "Order", nil)
	if err == nil || ins != nil {
		t.Fatalf("nil target type returned (%v, %v), want nil object and error", ins, err)
	}
}

func TestInsert_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := mustExpression(NewInsertExpression(q, "Order", testRecordType()))
	b := mustExpression(NewInsertExpression(q, "Order", testRecordType()))
	c := mustExpression(NewInsertExpression(q, "Customer", testRecordType()))
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
	del := mustExpression(NewDeleteExpression(q, "Order"))
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
	a := mustExpression(NewDeleteExpression(q, "Order"))
	b := mustExpression(NewDeleteExpression(q, "Order"))
	c := mustExpression(NewDeleteExpression(q, "Customer"))
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
	ins := mustExpression(NewInsertExpression(q, "Order", testRecordType()))
	del := mustExpression(NewDeleteExpression(q, "Order"))
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
	upd := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), ts1))
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
	upd := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), src))
	src[0].FieldPath = "MUTATED"
	if upd.GetTransforms()[0].FieldPath != "name" {
		t.Fatal("constructor failed to defensively copy transforms")
	}
}

func TestUpdate_EqualsWithoutChildren_TextualOrderIndependent(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
		{FieldPath: "active", NewValue: values.NewBooleanValue(false)},
	}))

	b := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), []UpdateTransform{
		{FieldPath: "active", NewValue: values.NewBooleanValue(false)},
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	}))

	if !a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("UPDATEs with same SET-list in different order reported unequal")
	}
}

func TestUpdate_EqualsWithoutChildren_DifferentValue(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	}))

	b := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(false)},
	}))

	if a.EqualsWithoutChildren(b, EmptyAliasMap()) {
		t.Fatal("UPDATEs with different replacement Values reported equal")
	}
}

func TestUpdate_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	a := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	}))

	b := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), []UpdateTransform{
		{FieldPath: "name", NewValue: values.NewBooleanValue(true)},
	}))

	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("structurally identical UPDATEs produced different hashes")
	}
}

func TestUpdate_NotEqualToInsertOrDelete(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	upd := mustExpression(NewUpdateExpression(q, "Order", testRecordType(), nil))
	ins := mustExpression(NewInsertExpression(q, "Order", testRecordType()))
	del := mustExpression(NewDeleteExpression(q, "Order"))
	if upd.HashCodeWithoutChildren() == ins.HashCodeWithoutChildren() {
		t.Fatal("UPDATE and INSERT collide on class-discriminating hash")
	}
	if upd.HashCodeWithoutChildren() == del.HashCodeWithoutChildren() {
		t.Fatal("UPDATE and DELETE collide on class-discriminating hash")
	}
}
