package values

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

type promotionCountedChild struct {
	typ   Type
	value any
	err   error
	calls int
}

func (c *promotionCountedChild) Type() Type                { return c.typ }
func (*promotionCountedChild) Name() string                { return "counted" }
func (*promotionCountedChild) Children() []Value           { return nil }
func (c *promotionCountedChild) Evaluate(any) (any, error) { c.calls++; return c.value, c.err }

func TestPromotePreparedOwnershipAndDrift(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"child", "source", "target"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			source := &PrimitiveType{TypeCode: TypeCodeInt}
			target := &PrimitiveType{TypeCode: TypeCodeLong, Nullable: true}
			child := &promotionCountedChild{typ: source, value: int32(7)}
			p, err := NewPromoteValueChecked(child, target)
			if err != nil {
				t.Fatal(err)
			}
			// Mutating a constructor argument cannot mutate the owned target.
			target.TypeCode = TypeCodeString
			if p.Type().Code() != TypeCodeLong || !p.Type().IsNullable() {
				t.Fatal("constructor retained caller's target")
			}
			got, err := p.Evaluate(nil)
			if err != nil || got != int64(7) || child.calls != 1 {
				t.Fatalf("first evaluation = %v, %v, calls=%d", got, err, child.calls)
			}
			switch kind {
			case "child":
				p.Child = &promotionCountedChild{typ: NotNullInt, value: int32(9)}
			case "source":
				source.TypeCode = TypeCodeDouble
			case "target":
				p.Target = NotNullDouble
			}
			_, err = p.Evaluate(nil)
			var drift *PromotionError
			if !errors.As(err, &drift) || child.calls != 1 {
				t.Fatalf("drift did not fail before child evaluation: %v, calls=%d", err, child.calls)
			}
		})
	}
}

func TestPromoteLiteralAndCheckedReconstruction(t *testing.T) {
	t.Parallel()
	child := &promotionCountedChild{typ: NotNullInt, value: int32(3)}
	literal := &PromoteValue{Child: child, Target: NotNullLong}
	if _, err := literal.Evaluate(nil); err == nil || child.calls != 0 {
		t.Fatal("unprepared literal evaluated its child")
	}
	if err := literal.Prepare(); err != nil {
		t.Fatal(err)
	}
	if got, err := literal.Evaluate(nil); err != nil || got != int64(3) || child.calls != 1 {
		t.Fatalf("prepared literal: %v, %v", got, err)
	}
	rebuilt, err := WithChildrenChecked(literal, []Value{&ConstantValue{Value: int64(9), Typ: NotNullLong}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rebuilt.Evaluate(nil); err != nil || got != int64(9) {
		t.Fatalf("rebuilt source relation = %v, %v", got, err)
	}
	bad, err := WithChildrenChecked(literal, []Value{&ConstantValue{Value: float64(9), Typ: NotNullDouble}})
	if err == nil || bad != nil {
		t.Fatalf("narrowing reconstruction = %v, %v", bad, err)
	}
	if literal.Child != child {
		t.Fatal("reconstruction mutated original child")
	}
}

func TestPromotePreparedNestedRecordArrayAndEnum(t *testing.T) {
	t.Parallel()
	enum := NewEnumType("E", false, []EnumValue{{Name: "A", Number: 7}, {Name: "B", Number: 12}})
	leaf := NewRecordType("SourceLeaf", false, []Field{{Name: "OLD", FieldType: NotNullInt}, {Name: "E", FieldType: enum}})
	source := NewRecordType("Source", false, []Field{{Name: "ITEMS", FieldType: NewArrayType(false, leaf)}})
	targetLeaf := NewRecordType("TargetLeaf", false, []Field{{Name: "NEW", FieldType: NullableDouble}, {Name: "ENUM", FieldType: enum}})
	target := NewRecordType("Target", true, []Field{{Name: "RENAMED", FieldType: NewArrayType(true, targetLeaf)}})
	inner := NewRecordConstructorValue(
		RecordConstructorField{Name: "OLD", Value: &ConstantValue{Value: int32(4), Typ: NotNullInt}},
		RecordConstructorField{Name: "E", Value: &ConstantValue{Value: int64(12), Typ: enum}},
	)
	repo := NewTypeProtoRepository()
	if err := repo.RegisterType(source); err != nil {
		t.Fatal(err)
	}
	if err := repo.Seal(); err != nil {
		t.Fatal(err)
	}
	ld, err := repo.MessageDescriptorFor(leaf)
	if err != nil {
		t.Fatal(err)
	}
	inner.SetMessageDescriptor(ld)
	element, err := inner.Evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	outer := NewRecordConstructorValue(RecordConstructorField{Name: "ITEMS", Value: &ConstantValue{Value: []any{element}, Typ: NewArrayType(false, leaf)}})
	sd, err := repo.MessageDescriptorFor(source)
	if err != nil {
		t.Fatal(err)
	}
	outer.SetMessageDescriptor(sd)
	input, err := outer.Evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	before := proto.Clone(input.(proto.Message))
	promotion, err := NewPromoteValueChecked(&ConstantValue{Value: input, Typ: source}, target)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			t.Parallel()
			result, err := promotion.Evaluate(nil)
			if err != nil {
				t.Fatal(err)
			}
			message, ok := result.(proto.Message)
			if !ok {
				t.Fatalf("record carrier = %T", result)
			}
			m := message.ProtoReflect()
			fd := m.Descriptor().Fields().Get(0)
			items := ProtoFieldToRowValue(fd, m.Get(fd)).([]any)
			if len(items) != 1 {
				t.Fatalf("array length = %d", len(items))
			}
			item := items[0].(proto.Message).ProtoReflect()
			if item.Get(item.Descriptor().Fields().Get(0)).Float() != 4 || item.Get(item.Descriptor().Fields().Get(1)).Enum() != 12 {
				t.Fatal("nested width/enum promotion lost data")
			}
			if item.Descriptor() != fd.Message().Fields().Get(0).Message() {
				t.Fatal("nested result has foreign descriptor")
			}
			if !proto.Equal(before, input.(proto.Message)) {
				t.Fatal("promotion mutated protobuf input")
			}
		})
	}
}

func TestPromotePreparedRawCarrierAndForeignShape(t *testing.T) {
	t.Parallel()
	source := NewRecordType("", false, []Field{{Name: "S", FieldType: NotNullInt}})
	target := NewRecordType("", false, []Field{{Name: "T", FieldType: NotNullLong}})
	input := map[string]any{"S": int32(5)}
	p := NewPromoteValue(&ConstantValue{Value: input, Typ: source}, target)
	got, err := p.Evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := got.(map[string]any)
	if !ok || raw["T"] != int64(5) || input["S"] != int32(5) {
		t.Fatalf("raw promotion = %#v", got)
	}
	foreign := NewRecordConstructorValue(RecordConstructorField{Name: "S", Value: &ConstantValue{Value: "wrong kind", Typ: NotNullString}})
	stampRecordConstructorForMessageTest(t, foreign)
	message, err := foreign.Evaluate(nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := NewPromoteValue(&ConstantValue{Value: message, Typ: source}, target)
	if _, err := bad.Evaluate(nil); err == nil {
		t.Fatal("foreign message bypassed declared-source admission")
	}
}

// The exported planner's runtime binder is a Go boundary, distinct from Java's
// typed parameter literals. Only its existing UUID/enum comparands admit UNKNOWN.
func TestPromotePreparedRuntimeComparands(t *testing.T) {
	t.Parallel()
	enum := NewEnumType("E", true, []EnumValue{{Name: "A", Number: 7}})
	uuid := [16]byte{15: 1}
	for _, tc := range []struct {
		name        string
		target      Type
		input, want any
		bad         bool
	}{
		{"enum_text", enum, "A", int64(7), false},
		{"enum_number", enum, int64(7), int64(7), false},
		{"enum_null", enum, nil, nil, false},
		{"enum_invalid_text", enum, "B", nil, true},
		{"uuid_text", NullableUuid, "00000000-0000-0000-0000-000000000001", uuid, false},
		{"uuid_native", NullableUuid, uuid, uuid, false},
		{"uuid_null", NullableUuid, nil, nil, false},
		{"uuid_invalid_text", NullableUuid, "bad uuid", nil, true},
		{"uuid_unconverted", NullableUuid, int64(7), int64(7), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := &promotionCountedChild{typ: UnknownType, value: tc.input}
			p, err := NewPromoteValueChecked(child, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Evaluate(nil)
			if (err != nil) != tc.bad || (!tc.bad && got != tc.want) || child.calls != 1 {
				t.Fatalf("runtime comparand = %#v, %v, calls=%d; want %#v, error=%t", got, err, child.calls, tc.want, tc.bad)
			}
		})
	}
	for _, target := range []Type{NotNullDouble, NewArrayType(false, NotNullInt), NewRecordType("", false, []Field{{Name: "X", FieldType: NotNullLong}})} {
		child := &promotionCountedChild{typ: UnknownType, value: int64(7)}
		if _, err := NewPromoteValueChecked(child, target); err == nil || child.calls != 0 {
			t.Fatalf("UNKNOWN acquired a runtime-inferred coercion to %s: %v", target, err)
		}
	}
}

func TestPromotePreparedNestedTypeDrift(t *testing.T) {
	t.Parallel()
	for _, side := range []string{"source", "target"} {
		for _, mutation := range []string{"name", "ordinal", "nullability", "enum"} {
			t.Run(side+"/"+mutation, func(t *testing.T) {
				t.Parallel()
				enum := NewEnumType("E", false, []EnumValue{{Name: "A", Number: 7}})
				leaf := NewRecordType("", false, []Field{{Name: "E", FieldType: enum}})
				typ := NewArrayType(false, leaf)
				child := &promotionCountedChild{typ: typ, value: []any{map[string]any{"E": int64(7)}}}
				p, err := NewPromoteValueChecked(child, typ)
				if err != nil {
					t.Fatal(err)
				}
				if side == "target" {
					leaf = p.Target.(*ArrayType).ElementType.(*RecordType)
				}
				switch mutation {
				case "name":
					leaf.Fields[0].Name = "OTHER"
				case "ordinal":
					leaf.Fields[0].Ordinal = 8
				case "nullability":
					leaf.Nullable = true
				case "enum":
					leaf.Fields[0].FieldType.(*EnumType).Values[0].Number = 8
				}
				_, err = p.Evaluate(nil)
				var drift *PromotionError
				if !errors.As(err, &drift) || child.calls != 0 {
					t.Fatalf("nested drift: %v, calls=%d", err, child.calls)
				}
			})
		}
	}
}

func TestPromotePreparedArrayPresence(t *testing.T) {
	t.Parallel()
	for _, sourceNullable := range []bool{false, true} {
		for _, targetNullable := range []bool{false, true} {
			for _, state := range []string{"empty", "populated", "absent"} {
				if state == "absent" && !sourceNullable {
					continue
				} // repeated fields are always present
				t.Run(fmt.Sprintf("sourceNullable=%t/targetNullable=%t/%s", sourceNullable, targetNullable, state), func(t *testing.T) {
					t.Parallel()
					source := NewRecordType("S", false, []Field{{Name: "A", FieldType: NewArrayType(sourceNullable, NotNullInt)}, {Name: "MISSING", FieldType: NullableLong}})
					target := NewRecordType("T", false, []Field{{Name: "B", FieldType: NewArrayType(targetNullable, NotNullDouble)}, {Name: "RENAMED", FieldType: NullableLong}})
					repo := NewTypeProtoRepository()
					md, err := repo.MessageDescriptorFor(source)
					if err != nil {
						t.Fatal(err)
					}
					input := dynamicpb.NewMessage(md)
					if state != "absent" {
						items := []any{}
						if state == "populated" {
							items = []any{int32(7)}
						}
						field, err := rowValueToProtoField(input, md.Fields().Get(0), items)
						if err != nil {
							t.Fatal(err)
						}
						input.Set(md.Fields().Get(0), field)
					}
					before := proto.Clone(input)
					p := NewPromoteValue(&ConstantValue{Value: input, Typ: source}, target)
					got, err := p.Evaluate(nil)
					if err != nil {
						t.Fatal(err)
					}
					result := got.(proto.Message).ProtoReflect()
					field := result.Descriptor().Fields().Get(0)
					if result.Has(result.Descriptor().Fields().Get(1)) {
						t.Fatal("absent scalar became present")
					}
					if targetNullable && result.Has(field) != (state != "absent") {
						t.Fatalf("wrapper presence = %t for %s", result.Has(field), state)
					}
					if state != "absent" || !targetNullable {
						items := ProtoFieldToRowValue(field, result.Get(field)).([]any)
						if state == "populated" {
							if len(items) != 1 || items[0] != float64(7) {
								t.Fatalf("promoted array = %#v", items)
							}
						} else if items == nil || len(items) != 0 {
							t.Fatalf("empty array = %#v, want present empty", items)
						}
					}
					if !proto.Equal(input, before) {
						t.Fatal("input mutated")
					}
				})
			}
		}
	}
}

func FuzzPreparedRecordPromotion(f *testing.F) {
	f.Add(int64(7), true)
	f.Add(int64(1<<53+1), false)
	f.Add(int64(-1<<63), true)
	f.Fuzz(func(t *testing.T, n int64, planned bool) {
		t.Parallel()
		source := NewRecordType("", false, []Field{{Name: "S", FieldType: NotNullLong}})
		target := NewRecordType("", false, []Field{{Name: "T", FieldType: NotNullDouble}})
		input := map[string]any{"S": n}
		p, err := NewPromoteValueChecked(&ConstantValue{Value: input, Typ: source}, target)
		if err != nil {
			t.Fatal(err)
		}
		if planned {
			repo := NewTypeProtoRepository()
			if err := p.RegisterPromotionTypes(repo); err != nil {
				t.Fatal(err)
			}
			if err := repo.Seal(); err != nil {
				t.Fatal(err)
			}
			if err := p.BindPromotionTypes(repo); err != nil {
				t.Fatal(err)
			}
		}
		got, err := p.Evaluate(nil)
		if err != nil {
			t.Fatal(err)
		}
		var number any
		if planned {
			message, ok := got.(proto.Message)
			if !ok {
				t.Fatalf("planned carrier = %T", got)
			}
			m := message.ProtoReflect()
			number = m.Get(m.Descriptor().Fields().Get(0)).Float()
		} else {
			record, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("raw carrier = %T", got)
			}
			number = record["T"]
		}
		if number != float64(n) || input["S"] != n {
			t.Fatalf("promotion = %#v, source = %#v", got, input)
		}
	})
}

func TestPromotePreparedChildErrorAndNull(t *testing.T) {
	t.Parallel()
	childErr := &PromotionError{Reason: "child evaluation failed"}
	child := &promotionCountedChild{typ: NotNullInt, value: int32(4), err: childErr}
	p := NewPromoteValue(child, NotNullLong)
	if got, err := p.Evaluate(nil); got != nil || !errors.Is(err, childErr) || child.calls != 1 {
		t.Fatalf("child failure = %#v, %v, calls=%d", got, err, child.calls)
	}
	child.err, child.value = nil, nil
	if got, err := p.Evaluate(nil); got != nil || err != nil || child.calls != 2 {
		t.Fatalf("root NULL = %#v, %v, calls=%d", got, err, child.calls)
	}
}
