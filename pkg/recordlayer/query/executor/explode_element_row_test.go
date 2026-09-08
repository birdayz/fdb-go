package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/testutil/allocs"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestExplodeElementRow_DeclaredOrder pins that a row-valued explode element
// (map[string]any — a constant record array / VALUES list) is laid out in the
// element type's DECLARED field order, not alphabetical. A baked ordinal reads
// by POSITION, so an alphabetical sort would make a reference baked at a field's
// declared ordinal silently read a DIFFERENT field (the wrong-slot class).
func TestExplodeElementRow_DeclaredOrder(t *testing.T) {
	t.Parallel()
	// RECORD<Z, A> — declared Z before A (NON-alphabetical on purpose).
	elemType := &values.RecordType{Fields: []values.Field{
		{Name: "Z", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 1},
	}}
	row := explodeElementRow(map[string]any{"Z": int64(100), "A": int64(200)}, mustExecutorConstruct(values.SnapshotExactType(elemType)))
	if v, _ := row.Get(0); v != int64(100) {
		t.Fatalf("slot 0 = Z (declared first) must be 100, got %v (an alphabetical sort puts A here = 200)", v)
	}
	if v, _ := row.Get(1); v != int64(200) {
		t.Fatalf("slot 1 = A must be 200, got %v", v)
	}
	if row.Type.Fields[0].Name != "Z" || row.Type.Fields[1].Name != "A" {
		t.Fatalf("row type must carry declared order [Z A], got %+v", row.Type.Fields)
	}

	// Case-insensitive element-map keys, and an absent declared field -> NULL slot.
	row2 := explodeElementRow(map[string]any{"z": int64(7)}, mustExecutorConstruct(values.SnapshotExactType(elemType)))
	if v, _ := row2.Get(0); v != int64(7) {
		t.Fatalf("case-insensitive key: slot 0 (Z) must be 7, got %v", v)
	}
	if v, _ := row2.Get(1); v != nil {
		t.Fatalf("absent declared field A must be a NULL slot, got %v", v)
	}

	// nil element type -> best-effort sorted fallback (unchanged behavior).
	row3 := explodeElementRow(map[string]any{"B": int64(1), "A": int64(2)}, nil)
	if v, _ := row3.Get(0); v != int64(2) {
		t.Fatalf("nil elemType fallback is sorted: slot 0 (A) must be 2, got %v", v)
	}
}

func TestExplodeElementRow_ProtoMessageRequiresExactDeclaredRecord(t *testing.T) {
	t.Parallel()

	message, declared := explodeProtoRecordFixture(t)
	row := explodeElementRow(message, mustExecutorConstruct(values.SnapshotExactType(declared)))
	if row == nil || !row.Type.Equals(declared) {
		t.Fatalf("exact proto element row type = %v, want declared type %v", row.Type, declared)
	}
	sub, ok := row.Slots[0].([]any)
	if !ok || len(sub) != 2 || sub[0] != int64(7) || sub[1] != int64(9) {
		t.Fatalf("repeated SUB slot = %#v, want [7 9]", row.Slots[0])
	}
	if row.Slots[1] != int64(11) {
		t.Fatalf("K slot = %#v, want 11", row.Slots[1])
	}
	if _, ok := row.Slots[2].(proto.Message); !ok {
		t.Fatalf("nested CHILD slot = %T, want raw proto.Message", row.Slots[2])
	}
	if _, err := row.AttachOrdinalLayout(explodeRecordLayout(t, declared), explodeRecordLayout(t, declared).Carrier().FlowedType()); err != nil {
		t.Fatalf("exact declared proto row rejected its layout: %v", err)
	}

	for _, nullable := range []bool{false, true} {
		widened := values.WithNullability(declared, nullable).(*values.RecordType)
		materialized := explodeElementRow(message, mustExecutorConstruct(values.SnapshotExactType(widened)))
		if materialized == nil || !materialized.Type.Equals(widened) {
			t.Fatalf("valid root nullability %v rejected", nullable)
		}
	}

	cloneFields := func() []values.Field {
		fields := make([]values.Field, len(declared.Fields))
		copy(fields, declared.Fields)
		return fields
	}
	nested := declared.Fields[2].FieldType.(*values.RecordType)
	nestedFields := make([]values.Field, len(nested.Fields))
	copy(nestedFields, nested.Fields)
	nestedFields[0].FieldType = values.NullableLong

	mutations := map[string]*values.RecordType{
		"foreign_width": values.NewRecordType(declared.RecordName, false, cloneFields()[:2]),
		"leaf_type": func() *values.RecordType {
			fields := cloneFields()
			fields[1].FieldType = values.NullableString
			return values.NewRecordType(declared.RecordName, false, fields)
		}(),
		"nested_type": func() *values.RecordType {
			fields := cloneFields()
			fields[2].FieldType = values.NewRecordType(nested.RecordName, nested.Nullable, nestedFields)
			return values.NewRecordType(declared.RecordName, false, fields)
		}(),
	}
	for name, drift := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutated := explodeElementRow(message, mustExecutorConstruct(values.SnapshotExactType(drift)))
			if mutated == nil || mutated.Type == drift || mutated.Type.Equals(drift) {
				t.Fatalf("structurally foreign declaration %s was stamped onto runtime row: %v", drift, mutated)
			}
			if _, err := mutated.AttachOrdinalLayout(explodeRecordLayout(t, drift), explodeRecordLayout(t, drift).Carrier().FlowedType()); err == nil {
				t.Fatalf("structurally foreign declaration %s attached silently", drift)
			}
		})
	}
}

func TestExplodePlanElementRow_TrustsOnlyItsFinalizedElementConstructor(t *testing.T) {
	t.Parallel()

	newFixture := func(t *testing.T) (*plans.RecordQueryExplodePlan, *values.ArrayConstructorValue, *values.RecordConstructorValue, proto.Message) {
		t.Helper()
		constructor := values.NewRawRecordConstructorValue(
			values.RecordConstructorField{Name: "Z", Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(7)}},
			values.RecordConstructorField{Name: "A", Value: &values.ConstantValue{Typ: values.NotNullLong, Value: int64(9)}},
		)
		rowType := constructor.Type().(*values.RecordType)
		array := values.NewArrayConstructorValue(rowType, []values.Value{constructor})
		plan, err := plans.NewRecordQueryExplodePlan(array)
		if err != nil {
			t.Fatalf("construct explode: %v", err)
		}
		if err := cascades.FinalizePlan(plan); err != nil {
			t.Fatalf("FinalizePlan: %v", err)
		}
		if constructor.MessageDescriptor() == nil {
			t.Fatal("FinalizePlan did not stamp inline record constructor")
		}
		evaluated, err := constructor.Evaluate(nil)
		if err != nil {
			t.Fatalf("evaluate finalized element: %v", err)
		}
		message, ok := evaluated.(proto.Message)
		if !ok {
			t.Fatalf("finalized element = %T, want proto.Message", evaluated)
		}
		return plan, array, constructor, message
	}

	assertRejected := func(t *testing.T, plan *plans.RecordQueryExplodePlan, message proto.Message) {
		t.Helper()
		row, err := explodePlanElementRow(plan, mustExecutorConstruct(values.ExactTypeForValue(plan.GetResultValue())), message, 0)
		var resolution *values.ResolutionError
		if row != nil || !errors.As(err, &resolution) || resolution.ErrorCode != values.LayoutTypeMismatch {
			t.Fatalf("mutated source constructor accepted: row=%v error=%v", row, err)
		}
	}

	t.Run("exact finalized constructor", func(t *testing.T) {
		t.Parallel()
		plan, array, constructor, message := newFixture(t)
		beforeType := constructor.Type()
		beforeDescriptor := constructor.MessageDescriptor()
		beforeElement := array.Elements[0]
		row, err := explodePlanElementRow(plan, mustExecutorConstruct(values.ExactTypeForValue(plan.GetResultValue())), message, 0)
		if err != nil {
			t.Fatal(err)
		}
		declared := plan.GetElementType().(*values.RecordType)
		if row == nil || !row.Type.Equals(declared) {
			t.Fatalf("constructed row type = %v, want frozen declared %v", row.Type, declared)
		}
		if got, _ := row.Get(0); got != int64(7) {
			t.Fatalf("slot 0 (declared Z) = %#v, want 7", got)
		}
		if got, _ := row.Get(1); got != int64(9) {
			t.Fatalf("slot 1 (declared A) = %#v, want 9", got)
		}
		if array.Elements[0] != beforeElement || constructor.MessageDescriptor() != beforeDescriptor ||
			!constructor.Type().Equals(beforeType) {
			t.Fatal("constructed-row materialization mutated its source value graph")
		}
	})

	t.Run("foreign structurally same descriptor", func(t *testing.T) {
		t.Parallel()
		plan, _, constructor, _ := newFixture(t)
		foreignRepo := values.NewTypeProtoRepository()
		foreignDescriptor, err := foreignRepo.MessageDescriptorFor(constructor.Type())
		if err != nil {
			t.Fatalf("foreign descriptor: %v", err)
		}
		if foreignDescriptor == constructor.MessageDescriptor() {
			t.Fatal("independent repositories unexpectedly shared descriptor identity")
		}
		foreign := values.NewRawRecordConstructorValue(constructor.Fields...)
		foreign.SetMessageDescriptor(foreignDescriptor)
		evaluated, err := foreign.Evaluate(nil)
		if err != nil {
			t.Fatalf("evaluate foreign constructor: %v", err)
		}
		row, err := explodePlanElementRow(plan, mustExecutorConstruct(values.ExactTypeForValue(plan.GetResultValue())), evaluated.(proto.Message), 0)
		if err != nil || row == nil || !row.Type.Equals(plan.GetElementType()) || row.Slots[0] != int64(7) || row.Slots[1] != int64(9) {
			t.Fatalf("equivalent descriptor from independent repository rejected: %v, %v", row, err)
		}
	})

	mutations := map[string]func(*values.RecordConstructorValue){
		"width": func(constructor *values.RecordConstructorValue) {
			constructor.Fields = append(constructor.Fields,
				values.RecordConstructorField{Name: "EXTRA", Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(1)}})
		},
		"order": func(constructor *values.RecordConstructorValue) {
			constructor.Fields[0], constructor.Fields[1] = constructor.Fields[1], constructor.Fields[0]
		},
		"nullability": func(constructor *values.RecordConstructorValue) {
			constructor.Fields[0].Value = values.NewNullValue(values.NullableInt)
		},
	}
	for name, mutate := range mutations {
		t.Run("source "+name+" drift", func(t *testing.T) {
			t.Parallel()
			plan, _, constructor, message := newFixture(t)
			mutate(constructor)
			assertRejected(t, plan, message)
		})
	}
}

func explodeRecordLayout(t testing.TB, record *values.RecordType) values.OrdinalLayout {
	t.Helper()
	var tiles []values.OrdinalTileSpec
	if len(record.Fields) > 0 {
		tiles = []values.OrdinalTileSpec{{
			Start: 0, Width: len(record.Fields), Kind: values.OrdinalTileFlat,
		}}
	}
	layout, err := values.NewOrdinalLayoutForCarrierType(record, tiles, nil)
	if err != nil {
		t.Fatalf("record layout: %v", err)
	}
	return layout
}

func explodeProtoRecordFixture(t testing.TB) (proto.Message, *values.RecordType) {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	int32Type := descriptorpb.FieldDescriptorProto_TYPE_INT32
	int64Type := descriptorpb.FieldDescriptorProto_TYPE_INT64
	stringType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	messageType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("explode_element_row.proto"),
		Package: proto.String("executor.explode"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Child"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("LEAF"), Number: proto.Int32(1), Label: optional.Enum(), Type: stringType.Enum(),
				}},
			},
			{
				Name: proto.String("Element"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("SUB"), Number: proto.Int32(1), Label: repeated.Enum(), Type: int32Type.Enum()},
					{Name: proto.String("K"), Number: proto.Int32(2), Label: optional.Enum(), Type: int64Type.Enum()},
					{Name: proto.String("CHILD"), Number: proto.Int32(3), Label: optional.Enum(), Type: messageType.Enum(), TypeName: proto.String(".executor.explode.Child")},
				},
			},
			{
				Name: proto.String("Host"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("ELEMENTS"), Number: proto.Int32(1), Label: repeated.Enum(), Type: messageType.Enum(), TypeName: proto.String(".executor.explode.Element"),
				}},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("message descriptor: %v", err)
	}
	elementDesc := file.Messages().ByName("Element")
	childDesc := file.Messages().ByName("Child")
	message := dynamicpb.NewMessage(elementDesc)
	sub := message.Mutable(elementDesc.Fields().ByName("SUB")).List()
	sub.Append(protoreflect.ValueOfInt32(7))
	sub.Append(protoreflect.ValueOfInt32(9))
	message.Set(elementDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(11))
	child := dynamicpb.NewMessage(childDesc)
	child.Set(childDesc.Fields().ByName("LEAF"), protoreflect.ValueOfString("nested"))
	message.Set(elementDesc.Fields().ByName("CHILD"), protoreflect.ValueOfMessage(child))

	hostField := file.Messages().ByName("Host").Fields().ByName("ELEMENTS")
	declared, ok := values.WithNullability(values.ScalarTypeForProtoKind(hostField), false).(*values.RecordType)
	if !ok {
		t.Fatalf("repeated message element type = %T, want *values.RecordType", values.ScalarTypeForProtoKind(hostField))
	}
	return message, declared
}

func TestExecuteExplode_NullableLiteralElement(t *testing.T) {
	t.Parallel()
	constructor := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "Z", Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(7)}},
		values.RecordConstructorField{Name: "A", Value: &values.ConstantValue{Typ: values.NotNullLong, Value: int64(9)}},
	)
	declared := values.WithNullability(constructor.Type(), true)
	plan := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(values.NewArrayConstructorValue(declared, []values.Value{constructor})))
	if err := cascades.FinalizePlan(plan); err != nil {
		t.Fatal(err)
	}
	cur, err := ExecutePlan(context.Background(), plan, nil, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close()
	rows, err := CollectAll(context.Background(), cur)
	if err != nil || len(rows) != 1 {
		t.Fatalf("nullable literal: %v, %v", rows, err)
	}
	if !rows[0].Positional.Type.Equals(declared) || rows[0].Positional.Slots[0] != int64(7) || rows[0].Positional.Slots[1] != int64(9) {
		t.Fatalf("nullable literal lost its declared carrier or values: %v", rows[0])
	}
}

func TestExecuteExplode_ProtoTypeAllocation(t *testing.T) {
	t.Parallel()
	allocs.Run(t, func() {
		message, declared := explodeProtoRecordFixture(t)
		items := make([]any, 128)
		for i := range items {
			items[i] = message
		}
		plan := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: items, Typ: values.NewArrayType(false, declared)}))
		ctx, ec, props := context.Background(), EmptyEvaluationContext(), recordlayer.DefaultExecuteProperties()
		measurement := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				cursor, err := executeExplode(plan, ec, nil, props)
				if err != nil {
					b.Fatal(err)
				}
				count := 0
				for {
					row, err := cursor.OnNext(ctx)
					if err != nil {
						b.Fatal(err)
					}
					if !row.HasNext() {
						break
					}
					if row.GetValue().Positional.Slots[1] != int64(11) {
						b.Fatal("wrong record value")
					}
					count++
				}
				_ = cursor.Close()
				if count != len(items) {
					b.Fatalf("read %d rows, want %d", count, len(items))
				}
			}
		})
		if measurement.N == 0 {
			t.Fatal("empty allocation measurement")
		}
		// This fixture needs six allocations per materialized record, plus fixed
		// cursor overhead. Re-snapshotting its nested Type per row adds two more;
		// the small fixed allowance cannot hide that O(rows) regression.
		if got, ceiling := measurement.AllocsPerOp(), int64(6*len(items)+16); got > ceiling {
			t.Fatalf("Explode allocated %d times for %d rows (ceiling %d); reuse the frozen element handle", got, len(items), ceiling)
		}
		t.Logf("EXPLODE_ALLOC rows=%d allocs/op=%d bytes/op=%d ns/op=%d", len(items), measurement.AllocsPerOp(), measurement.AllocedBytesPerOp(), measurement.NsPerOp())
	})
}
