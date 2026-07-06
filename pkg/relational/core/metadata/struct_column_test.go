package metadata

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/relational/api"
)

// structCol builds a named struct type with the given fields (long "SUB" +
// integer-array "ARR" by default when fields is nil).
func longField(name string, idx int) api.StructField {
	return api.NewStructField(name, api.NewLongType(true), idx)
}

// TestBuilder_StructColumn_NestedDescriptor pins that a STRUCT column
// materializes as a nested proto message type of its table, with the
// declared fields (a scalar + an array), reachable through the built
// descriptor by the qualified path the unnest classifier walks.
func TestBuilder_StructColumn_NestedDescriptor(t *testing.T) {
	t.Parallel()
	rec := api.NewStructType("RECT", []api.StructField{
		longField("SUB", 0),
		api.NewStructField("ARR", api.NewArrayType(api.NewIntegerType(false), true), 1),
	}, true)
	tmpl, err := NewSchemaTemplateBuilder().SetName("st").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("ID", api.NewLongType(false), 1),
			NewColumnSpec("REC", rec, 2),
		}, []string{"ID"}).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	desc := tmpl.Underlying().GetRecordType("T").Descriptor
	fd := desc.Fields().ByName("REC")
	if fd == nil || fd.Kind() != protoreflect.MessageKind {
		t.Fatalf("REC field = %v, want a message-typed field", fd)
	}
	nrec := fd.Message()
	if nrec == nil || string(nrec.Name()) != "RECT" {
		t.Fatalf("nested message = %v, want RECT", nrec)
	}
	sub := nrec.Fields().ByName("SUB")
	arr := nrec.Fields().ByName("ARR")
	if sub == nil || sub.Kind() != protoreflect.Int64Kind {
		t.Fatalf("REC.SUB = %v, want int64", sub)
	}
	if arr == nil || !arr.IsList() || arr.Kind() != protoreflect.Int32Kind {
		t.Fatalf("REC.ARR = %v, want repeated int32", arr)
	}
}

// TestBuilder_StructColumn_NestedStruct pins struct-in-struct: a struct field
// whose type is itself a struct emits a second nested message type.
func TestBuilder_StructColumn_NestedStruct(t *testing.T) {
	t.Parallel()
	inner := api.NewStructType("INNER", []api.StructField{longField("X", 0)}, true)
	outer := api.NewStructType("OUTER", []api.StructField{
		longField("Y", 0),
		api.NewStructField("CHILD", inner, 1),
	}, true)
	tmpl, err := NewSchemaTemplateBuilder().SetName("st2").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("ID", api.NewLongType(false), 1),
			NewColumnSpec("O", outer, 2),
		}, []string{"ID"}).Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	desc := tmpl.Underlying().GetRecordType("T").Descriptor
	om := desc.Fields().ByName("O").Message()
	child := om.Fields().ByName("CHILD")
	if child == nil || child.Kind() != protoreflect.MessageKind || string(child.Message().Name()) != "INNER" {
		t.Fatalf("O.CHILD = %v, want the nested INNER message", child)
	}
	if x := child.Message().Fields().ByName("X"); x == nil || x.Kind() != protoreflect.Int64Kind {
		t.Fatalf("INNER.X = %v, want int64", x)
	}
}

// TestBuilder_StructColumn_DuplicateNameSameShape accepts two columns sharing
// a struct name IFF the declared shape is identical (one name, one shape).
func TestBuilder_StructColumn_DuplicateNameSameShape(t *testing.T) {
	t.Parallel()
	mk := func() *api.StructType {
		return api.NewStructType("P", []api.StructField{longField("A", 0)}, true)
	}
	_, err := NewSchemaTemplateBuilder().SetName("st3").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("ID", api.NewLongType(false), 1),
			NewColumnSpec("P1", mk(), 2),
			NewColumnSpec("P2", mk(), 3),
		}, []string{"ID"}).Build()
	if err != nil {
		t.Fatalf("identical dup struct names must build, got %v", err)
	}
}

// TestBuilder_StructColumn_DuplicateNameDifferentShape REJECTS a name reused
// with a different shape: a count-only guard silently gave the second column
// the first's descriptor, so a different FIELD NAME at the same field count
// must error (the full-shape equality guard's regression net).
func TestBuilder_StructColumn_DuplicateNameDifferentShape(t *testing.T) {
	t.Parallel()
	p1 := api.NewStructType("P", []api.StructField{longField("A", 0)}, true)
	p2 := api.NewStructType("P", []api.StructField{longField("B", 0)}, true) // same count, different name
	_, err := NewSchemaTemplateBuilder().SetName("st4").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("ID", api.NewLongType(false), 1),
			NewColumnSpec("P1", p1, 2),
			NewColumnSpec("P2", p2, 3),
		}, []string{"ID"}).Build()
	if err == nil {
		t.Fatal("a struct name reused with a DIFFERENT shape must error (else the second column silently gets the first's descriptor)")
	}
	if !strings.Contains(err.Error(), "different shapes") {
		t.Fatalf("error = %v, want a 'different shapes' diagnostic", err)
	}
}

// TestBuilder_StructColumn_UnnamedRejected: an anonymous struct type has no
// nested message name to emit — must error, not panic.
func TestBuilder_StructColumn_UnnamedRejected(t *testing.T) {
	t.Parallel()
	anon := api.NewStructType("", []api.StructField{longField("A", 0)}, true)
	_, err := NewSchemaTemplateBuilder().SetName("st5").
		AddTable("T", []ColumnSpec{
			NewColumnSpec("ID", api.NewLongType(false), 1),
			NewColumnSpec("A", anon, 2),
		}, []string{"ID"}).Build()
	if err == nil {
		t.Fatal("an unnamed struct column type must error")
	}
}
