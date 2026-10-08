package values

import (
	"fmt"
	"math"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/protoname"
	"google.golang.org/protobuf/proto"
)

// DerivedStorageName is the protobuf spelling Java gives a name that states
// none (Field.of, EnumValue.from: ProtoUtils.toProtoBufCompliantName).
func DerivedStorageName(name string) string {
	if name == "" {
		return ""
	}
	s, err := protoname.ToProtoBufCompliantName(name)
	if err != nil {
		return name
	}
	return s
}

// explicitStorageName is storage when name does not already imply it.
func explicitStorageName(name, storage string) string {
	if storage == DerivedStorageName(name) {
		return ""
	}
	return storage
}

// SerializationContext is Java's PlanSerializationContext for types: a record
// type is written in full once and by reference id afterwards, record types
// being the same when their names and structure are
// (RecordTypeWithNameEquivalence).
type SerializationContext struct {
	recordIDs map[string]int32
	records   map[int32]*RecordType
}

func NewSerializationContext() *SerializationContext {
	return &SerializationContext{recordIDs: map[string]int32{}, records: map[int32]*RecordType{}}
}

var primitiveTypeCodes = map[TypeCode]gen.PType_PTypeCode{
	TypeCodeBoolean: gen.PType_BOOLEAN,
	TypeCodeBytes:   gen.PType_BYTES,
	TypeCodeDouble:  gen.PType_DOUBLE,
	TypeCodeFloat:   gen.PType_FLOAT,
	TypeCodeInt:     gen.PType_INT,
	TypeCodeLong:    gen.PType_LONG,
	TypeCodeString:  gen.PType_STRING,
	TypeCodeVersion: gen.PType_VERSION,
	TypeCodeUnknown: gen.PType_UNKNOWN,
}

// TypeToProto is Java's Type.toTypeProto.
func (c *SerializationContext) TypeToProto(t Type) (*gen.PType, error) {
	switch tt := t.(type) {
	case *VectorType:
		return &gen.PType{SpecificType: &gen.PType_VectorType{VectorType: &gen.PType_PVectorType{IsNullable: proto.Bool(tt.Nullable), Precision: proto.Int32(int32(tt.Precision)), Dimensions: proto.Int32(int32(tt.Dimensions))}}}, nil
	case *RecordType:
		key := recordIdentityKey(tt)
		if id, ok := c.recordIDs[key]; ok {
			return &gen.PType{SpecificType: &gen.PType_RecordType{RecordType: &gen.PType_PRecordType{ReferenceId: proto.Int32(id)}}}, nil
		}
		id := int32(len(c.recordIDs))
		c.recordIDs[key] = id
		rt := &gen.PType_PRecordType{ReferenceId: proto.Int32(id), IsNullable: proto.Bool(tt.Nullable)}
		if tt.RecordName != "" {
			rt.Name = proto.String(tt.RecordName)
		}
		if tt.StorageName != "" && tt.StorageName != tt.RecordName {
			rt.StorageName = proto.String(tt.StorageName)
		}
		for i, f := range tt.Fields {
			pf, err := c.fieldToProto(f, i)
			if err != nil {
				return nil, err
			}
			rt.Fields = append(rt.Fields, pf)
		}
		return &gen.PType{SpecificType: &gen.PType_RecordType{RecordType: rt}}, nil
	case *EnumType:
		// Type.Enum.toProto: the members, then the name and a differing
		// storage name.
		et := &gen.PType_PEnumType{IsNullable: proto.Bool(tt.Nullable)}
		for _, v := range tt.Values {
			pv := &gen.PType_PEnumType_PEnumValue{Name: proto.String(v.Name), Number: proto.Int32(v.Number)}
			storage := v.StorageName
			if storage == "" {
				storage = DerivedStorageName(v.Name)
			}
			if storage != v.Name {
				pv.StorageName = proto.String(storage)
			}
			et.EnumValues = append(et.EnumValues, pv)
		}
		if tt.EnumName != "" {
			et.Name = proto.String(tt.EnumName)
		}
		if tt.StorageName != "" && tt.StorageName != tt.EnumName {
			et.StorageName = proto.String(tt.StorageName)
		}
		return &gen.PType{SpecificType: &gen.PType_EnumType{EnumType: et}}, nil
	case *ArrayType:
		elem, err := c.TypeToProto(tt.ElementType)
		if err != nil {
			return nil, err
		}
		return &gen.PType{SpecificType: &gen.PType_ArrayType{ArrayType: &gen.PType_PArrayType{
			IsNullable: proto.Bool(tt.Nullable), ElementType: elem,
		}}}, nil
	}
	if t == nil {
		return nil, fmt.Errorf("serialize type: nil")
	}
	switch t.Code() {
	case TypeCodeNull:
		return &gen.PType{SpecificType: &gen.PType_NullType{NullType: &gen.PType_PNullType{}}}, nil
	case TypeCodeUuid:
		return &gen.PType{SpecificType: &gen.PType_UuidType{UuidType: &gen.PType_PUuidType{IsNullable: proto.Bool(t.IsNullable())}}}, nil
	}
	code, ok := primitiveTypeCodes[t.Code()]
	if !ok {
		return nil, fmt.Errorf("serialize type: unsupported %s", t)
	}
	return &gen.PType{SpecificType: &gen.PType_PrimitiveType{PrimitiveType: &gen.PType_PPrimitiveType{
		TypeCode: code.Enum(), IsNullable: proto.Bool(t.IsNullable()),
	}}}, nil
}

// fieldToProto is Java's Type.Record.Field.toProto; field indexes are 1-based.
func (c *SerializationContext) fieldToProto(f Field, ordinal int) (*gen.PType_PRecordType_PField, error) {
	ft, err := c.TypeToProto(f.FieldType)
	if err != nil {
		return nil, err
	}
	pf := &gen.PType_PRecordType_PField{FieldType: ft, FieldIndex: proto.Int32(int32(ordinal + 1))}
	if f.Name != "" {
		pf.FieldName = proto.String(f.Name)
	}
	return pf, nil
}

// recordIdentityKey renders a record type with its names and structure.
func recordIdentityKey(t Type) string {
	var b strings.Builder
	var walk func(Type)
	walk = func(t Type) {
		switch tt := t.(type) {
		case *VectorType:
			fmt.Fprintf(&b, "V(%t,%d,%d)", tt.Nullable, tt.Precision, tt.Dimensions)
		case *RecordType:
			fmt.Fprintf(&b, "R(%q,%t", tt.RecordName, tt.Nullable)
			for _, f := range tt.Fields {
				fmt.Fprintf(&b, ",%q:", f.Name)
				walk(f.FieldType)
			}
			b.WriteByte(')')
		case *EnumType:
			// Type.Enum.equals: nullability and the members.
			fmt.Fprintf(&b, "E(%t", tt.Nullable)
			for _, v := range tt.Values {
				fmt.Fprintf(&b, ",%q=%d", v.Name, v.Number)
			}
			b.WriteByte(')')
		case *ArrayType:
			fmt.Fprintf(&b, "A(%t,", tt.Nullable)
			walk(tt.ElementType)
			b.WriteByte(')')
		case nil:
			b.WriteString("nil")
		default:
			fmt.Fprintf(&b, "P(%d,%t)", t.Code(), t.IsNullable())
		}
	}
	walk(t)
	return b.String()
}

// TypeFromProto is Java's Type.fromTypeProto.
func (c *SerializationContext) TypeFromProto(p *gen.PType) (Type, error) {
	switch {
	case p.GetVectorType() != nil:
		v := p.GetVectorType()
		if v.IsNullable == nil {
			return nil, fmt.Errorf("deserialize vector type: missing isNullable")
		}
		return NewVectorType(v.GetIsNullable(), int(v.GetPrecision()), int(v.GetDimensions())), nil
	case p.GetRecordType() != nil:
		rt := p.GetRecordType()
		if len(rt.GetFields()) == 0 && rt.Name == nil && rt.IsNullable == nil {
			if t, ok := c.records[rt.GetReferenceId()]; ok {
				return t, nil
			}
		}
		fields := make([]Field, len(rt.GetFields()))
		for i, pf := range rt.GetFields() {
			ft, err := c.TypeFromProto(pf.GetFieldType())
			if err != nil {
				return nil, err
			}
			fields[i] = Field{Name: pf.GetFieldName(), FieldType: ft, Ordinal: i}
		}
		t := NewRecordType(rt.GetName(), rt.GetIsNullable(), fields)
		t.StorageName = rt.GetStorageName()
		c.records[rt.GetReferenceId()] = t
		c.recordIDs[recordIdentityKey(t)] = rt.GetReferenceId()
		return t, nil
	case p.GetArrayType() != nil:
		elem, err := c.TypeFromProto(p.GetArrayType().GetElementType())
		if err != nil {
			return nil, err
		}
		return NewArrayType(p.GetArrayType().GetIsNullable(), elem), nil
	case p.GetEnumType() != nil:
		et := p.GetEnumType()
		if et.IsNullable == nil || len(et.GetEnumValues()) == 0 {
			return nil, fmt.Errorf("deserialize enum type: missing isNullable or members")
		}
		t := &EnumType{EnumName: et.GetName(), Nullable: et.GetIsNullable()}
		if et.StorageName != nil && et.GetStorageName() != et.GetName() {
			t.StorageName = et.GetStorageName()
		}
		for _, pv := range et.GetEnumValues() {
			v := EnumValue{Name: pv.GetName(), Number: pv.GetNumber()}
			if pv.StorageName != nil {
				v.StorageName = explicitStorageName(v.Name, pv.GetStorageName())
			} else if DerivedStorageName(v.Name) != v.Name {
				v.StorageName = v.Name
			}
			t.Values = append(t.Values, v)
		}
		return t, nil
	case p.GetNullType() != nil:
		return NullType, nil
	case p.GetUuidType() != nil:
		return NewPrimitiveType(TypeCodeUuid, p.GetUuidType().GetIsNullable()), nil
	case p.GetPrimitiveType() != nil:
		pt := p.GetPrimitiveType()
		for code, pc := range primitiveTypeCodes {
			if pc == pt.GetTypeCode() {
				return NewPrimitiveType(code, pt.GetIsNullable()), nil
			}
		}
	}
	return nil, fmt.Errorf("deserialize type: unsupported %v", p)
}

// ValueToProto is Java's Value.toValueProto for the values a SQL macro body
// is built from.
func (c *SerializationContext) ValueToProto(v Value) (*gen.PValue, error) {
	if qov, ok := AsQuantifiedObjectValue(v); ok {
		t, err := c.TypeToProto(qov.FlowedType())
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_QuantifiedObjectValue{QuantifiedObjectValue: &gen.PQuantifiedObjectValue{
			Alias: proto.String(qov.Correlation().Name()), ResultType: t,
		}}}, nil
	}
	if fv, ok := v.(FieldValue); ok {
		child, err := c.ValueToProto(fv.ChildValue())
		if err != nil {
			return nil, err
		}
		path := &gen.PFieldPath{}
		parent := fv.ChildValue().Type()
		for i := 0; i < fv.Path().Len(); i++ {
			acc, _ := fv.Path().Accessor(i)
			rt, ok := parent.(*RecordType)
			if !ok || acc.Ordinal() >= len(rt.Fields) {
				return nil, fmt.Errorf("serialize field path: %s has no field %d", parent, acc.Ordinal())
			}
			f := rt.Fields[acc.Ordinal()]
			pa := &gen.PFieldPath_PResolvedAccessor{Name: proto.String(f.Name), Ordinal: proto.Int32(int32(acc.Ordinal()))}
			if pa.Type, err = c.TypeToProto(f.FieldType); err != nil {
				return nil, err
			}
			if pa.Field, err = c.fieldToProto(f, acc.Ordinal()); err != nil {
				return nil, err
			}
			path.FieldAccessors = append(path.FieldAccessors, pa)
			parent = f.FieldType
		}
		return &gen.PValue{SpecificValue: &gen.PValue_FieldValue{FieldValue: &gen.PFieldValue{ChildValue: child, FieldPath: path}}}, nil
	}
	switch vv := v.(type) {
	case *ArithmeticValue:
		lane, ok := vv.Lane()
		if !ok {
			return nil, fmt.Errorf("serialize arithmetic: no physical operator")
		}
		op, ok := arithmeticLaneProtoOperator(lane)
		if !ok {
			return nil, fmt.Errorf("serialize arithmetic: unknown operator %v", lane)
		}
		left, err := c.ValueToProto(vv.Left)
		if err != nil {
			return nil, err
		}
		right, err := c.ValueToProto(vv.Right)
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_ArithmeticValue{ArithmeticValue: &gen.PArithmeticValue{
			Operator: op.Enum(), LeftChild: left, RightChild: right,
		}}}, nil
	case *NullValue:
		t, err := c.TypeToProto(vv.Typ)
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_NullValue{NullValue: &gen.PNullValue{ResultType: t}}}, nil
	case *ConstantValue:
		obj, err := literalToProto(vv.Value, vv.Typ)
		if err != nil {
			return nil, err
		}
		// The literal's type, NOT NULL unless it is NULL (Type.fromObject).
		t, err := c.TypeToProto(vv.Type())
		if err != nil {
			return nil, err
		}
		if vv.Typ.Code() == TypeCodeVector {
			// Java's comparable-object decoder needs this nested tag to restore
			// RealVector rather than byte[], independently of the literal type.
			obj.Type = t
		}
		return &gen.PValue{SpecificValue: &gen.PValue_LiteralValue{LiteralValue: &gen.PLiteralValue{Value: obj, ResultType: t}}}, nil
	case *RecordConstructorValue:
		rc := &gen.PRecordConstructorValue{}
		for i, f := range vv.Fields {
			pf, err := c.fieldToProto(Field{Name: f.Name, FieldType: f.Value.Type()}, i)
			if err != nil {
				return nil, err
			}
			pv, err := c.ValueToProto(f.Value)
			if err != nil {
				return nil, err
			}
			rc.Columns = append(rc.Columns, &gen.PRecordConstructorValue_PColumn{Field: pf, Value: pv})
		}
		t, err := c.TypeToProto(vv.Type())
		if err != nil {
			return nil, err
		}
		rc.ResultType = t
		return &gen.PValue{SpecificValue: &gen.PValue_RecordConstructorValue{RecordConstructorValue: rc}}, nil
	case *SubscriptValue:
		index, err := c.ValueToProto(vv.Index)
		if err != nil {
			return nil, err
		}
		source, err := c.ValueToProto(vv.Source)
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_SubscriptValue{SubscriptValue: &gen.PSubscriptValue{
			Index: index, Source: source,
		}}}, nil
	case *BooleanValue:
		// Java's boolean literal is a LiteralValue (LiteralValue.toProto).
		obj := &gen.PComparableObject{SpecificObject: &gen.PComparableObject_PrimitiveObject{PrimitiveObject: &gen.Value{}}}
		if vv.Value != nil {
			obj.GetPrimitiveObject().BoolValue = proto.Bool(*vv.Value)
		}
		t, err := c.TypeToProto(vv.Type())
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_LiteralValue{LiteralValue: &gen.PLiteralValue{Value: obj, ResultType: t}}}, nil
	case *LikeOperatorValue:
		src, err := c.ValueToProto(vv.Probe)
		if err != nil {
			return nil, err
		}
		pattern, err := c.ValueToProto(vv.Pattern)
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_LikeOperatorValue{LikeOperatorValue: &gen.PLikeOperatorValue{
			SrcChild: src, PatternChild: pattern,
		}}}, nil
	case *PatternForLikeValue:
		pattern, err := c.ValueToProto(vv.PatternChild)
		if err != nil {
			return nil, err
		}
		escape, err := c.ValueToProto(vv.EscapeChild)
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_PatternForLikeValue{PatternForLikeValue: &gen.PPatternForLikeValue{
			PatternChild: pattern, EscapeChild: escape,
		}}}, nil
	case *ArrayConstructorValue:
		ac := &gen.PAbstractArrayConstructorValue{}
		for _, e := range vv.Elements {
			pe, err := c.ValueToProto(e)
			if err != nil {
				return nil, err
			}
			ac.Children = append(ac.Children, pe)
		}
		t, err := c.TypeToProto(vv.ElementType)
		if err != nil {
			return nil, err
		}
		ac.ElementType = t
		return &gen.PValue{SpecificValue: &gen.PValue_LightArrayConstructorValue{
			LightArrayConstructorValue: &gen.PLightArrayConstructorValue{Super: ac},
		}}, nil
	}
	if pv, ok := v.(*PromoteValue); ok {
		in, err := c.ValueToProto(pv.Child)
		if err != nil {
			return nil, err
		}
		to, err := c.TypeToProto(pv.Target)
		if err != nil {
			return nil, err
		}
		trie, err := primitivePromotionTrie(pv.Child.Type(), pv.Target)
		if err != nil {
			return nil, err
		}
		return &gen.PValue{SpecificValue: &gen.PValue_PromoteValue{PromoteValue: &gen.PPromoteValue{
			InValue: in, PromoteToType: to, PromotionTrie: trie,
		}}}, nil
	}
	return nil, fmt.Errorf("serialize value: unsupported %T", v)
}

var javaTypeCodeNames = map[TypeCode]string{
	TypeCodeNull: "NULL", TypeCodeBoolean: "BOOLEAN", TypeCodeInt: "INT", TypeCodeLong: "LONG",
	TypeCodeFloat: "FLOAT", TypeCodeDouble: "DOUBLE", TypeCodeString: "STRING", TypeCodeBytes: "BYTES",
	TypeCodeVersion: "VERSION", TypeCodeEnum: "ENUM", TypeCodeUuid: "UUID", TypeCodeArray: "ARRAY",
	TypeCodeRecord: "RECORD", TypeCodeVector: "VECTOR",
}

// primitivePromotionTrie is PromoteValue.computePromotionsTrie for a
// primitive (or NULL) source: a leaf naming the physical coercion, or none
// when only nullability differs.
func primitivePromotionTrie(from, to Type) (*gen.PCoercionTrieNode, error) {
	if from == nil || to == nil {
		return nil, fmt.Errorf("serialize promotion: untyped")
	}
	if from.Code() == to.Code() {
		return nil, nil
	}
	if !from.Code().IsPrimitive() && from.Code() != TypeCodeNull {
		return nil, fmt.Errorf("serialize promotion: unsupported %s to %s", from, to)
	}
	name := javaTypeCodeNames[from.Code()] + "_TO_" + javaTypeCodeNames[to.Code()]
	op, ok := gen.PPrimitiveCoercionBiFunction_PPhysicalOperator_value[name]
	if !ok {
		return nil, fmt.Errorf("serialize promotion: no coercion %s", name)
	}
	return &gen.PCoercionTrieNode{
		Value: &gen.PCoercionBiFunction{SpecificFunction: &gen.PCoercionBiFunction_PrimitiveCoercionBiFunction{
			PrimitiveCoercionBiFunction: &gen.PPrimitiveCoercionBiFunction{Operator: gen.PPrimitiveCoercionBiFunction_PPhysicalOperator(op).Enum()},
		}},
		ChildrenMapIsNull: proto.Bool(true),
	}, nil
}

func literalToProto(v any, typ Type) (*gen.PComparableObject, error) {
	// SQL uses wide Go carriers even for INT/FLOAT literals. Java restores
	// the runtime box from the value field, not PLiteralValue.result_type;
	// writing the wide carrier would break Integer/Float arithmetic casts.
	if typ != nil {
		switch typ.Code() {
		case TypeCodeInt, TypeCodeLong:
			var n int64
			switch x := v.(type) {
			case int:
				n = int64(x)
			case int32:
				n = int64(x)
			case int64:
				n = x
			default:
				return nil, fmt.Errorf("serialize %s literal: incompatible carrier %T", typ, v)
			}
			v = n
			if typ.Code() == TypeCodeInt {
				if n < math.MinInt32 || n > math.MaxInt32 {
					return nil, fmt.Errorf("serialize INT literal: %d is out of range", n)
				}
				v = int32(n)
			}
		case TypeCodeFloat, TypeCodeDouble:
			var n float64
			switch x := v.(type) {
			case float32:
				n = float64(x)
			case float64:
				n = x
			default:
				return nil, fmt.Errorf("serialize %s literal: incompatible carrier %T", typ, v)
			}
			v = n
			if typ.Code() == TypeCodeFloat {
				v = float32(n)
			}
		}
	}
	pv := &gen.Value{}
	switch x := v.(type) {
	case int64:
		pv.LongValue = proto.Int64(x)
	case int32:
		pv.IntValue = proto.Int32(x)
	case float64:
		pv.DoubleValue = proto.Float64(x)
	case float32:
		pv.FloatValue = proto.Float32(x)
	case bool:
		pv.BoolValue = proto.Bool(x)
	case string:
		pv.StringValue = proto.String(x)
	case []byte:
		pv.BytesValue = x
	default:
		return nil, fmt.Errorf("serialize literal: unsupported %T", v)
	}
	return &gen.PComparableObject{SpecificObject: &gen.PComparableObject_PrimitiveObject{PrimitiveObject: pv}}, nil
}

func literalFromProto(p *gen.PComparableObject) (any, error) {
	pv := p.GetPrimitiveObject()
	switch {
	case pv == nil:
		return nil, fmt.Errorf("deserialize literal: unsupported %v", p)
	case pv.LongValue != nil:
		return pv.GetLongValue(), nil
	case pv.IntValue != nil:
		return pv.GetIntValue(), nil
	case pv.DoubleValue != nil:
		return pv.GetDoubleValue(), nil
	case pv.FloatValue != nil:
		return pv.GetFloatValue(), nil
	case pv.BoolValue != nil:
		return pv.GetBoolValue(), nil
	case pv.StringValue != nil:
		return pv.GetStringValue(), nil
	case pv.BytesValue != nil:
		return pv.GetBytesValue(), nil
	}
	return nil, nil
}

// ValueFromProto is Java's Value.fromValueProto for the same values.
func (c *SerializationContext) ValueFromProto(p *gen.PValue) (Value, error) {
	switch {
	case p.GetQuantifiedObjectValue() != nil:
		q := p.GetQuantifiedObjectValue()
		t, err := c.TypeFromProto(q.ResultType)
		if err != nil {
			return nil, err
		}
		return NewQuantifiedObjectValue(NamedCorrelationIdentifier(q.GetAlias()), t)
	case p.GetFieldValue() != nil:
		child, err := c.ValueFromProto(p.GetFieldValue().GetChildValue())
		if err != nil {
			return nil, err
		}
		var ordinals []int
		for _, a := range p.GetFieldValue().GetFieldPath().GetFieldAccessors() {
			if a.GetType() != nil {
				if _, err := c.TypeFromProto(a.GetType()); err != nil {
					return nil, err
				}
			}
			ordinals = append(ordinals, int(a.GetOrdinal()))
		}
		return ResolveFieldOrdinals(child, ordinals)
	case p.GetArithmeticValue() != nil:
		a := p.GetArithmeticValue()
		lane, ok := arithmeticLaneForProto(a.GetOperator())
		if !ok {
			return nil, fmt.Errorf("deserialize arithmetic: unknown operator %v", a.GetOperator())
		}
		left, err := c.ValueFromProto(a.GetLeftChild())
		if err != nil {
			return nil, err
		}
		right, err := c.ValueFromProto(a.GetRightChild())
		if err != nil {
			return nil, err
		}
		op, ok := ArithmeticOpForLogicalName(lane.Function)
		if !ok {
			return nil, fmt.Errorf("deserialize arithmetic: unknown function %s", lane.Function)
		}
		return NewArithmeticValue(op, left, right)
	case p.GetNullValue() != nil:
		t, err := c.TypeFromProto(p.GetNullValue().ResultType)
		if err != nil {
			return nil, err
		}
		return NewNullValue(t), nil
	case p.GetLiteralValue() != nil:
		t, err := c.TypeFromProto(p.GetLiteralValue().ResultType)
		if err != nil {
			return nil, err
		}
		v, err := literalFromProto(p.GetLiteralValue().GetValue())
		if err != nil {
			return nil, err
		}
		if t.Code() == TypeCodeBoolean {
			// A boolean literal is Go's BooleanValue, which a WHERE folds to a
			// constant predicate.
			switch b := v.(type) {
			case bool:
				return NewBooleanValue(b), nil
			case nil:
				return &BooleanValue{}, nil
			}
			return nil, fmt.Errorf("deserialize BOOLEAN literal: carrier %T", v)
		}
		return &ConstantValue{Value: v, Typ: t}, nil
	case p.GetRecordConstructorValue() != nil:
		rc := p.GetRecordConstructorValue()
		fields := make([]RecordConstructorField, len(rc.GetColumns()))
		for i, col := range rc.GetColumns() {
			if _, err := c.TypeFromProto(col.GetField().GetFieldType()); err != nil {
				return nil, err
			}
			v, err := c.ValueFromProto(col.GetValue())
			if err != nil {
				return nil, err
			}
			fields[i] = RecordConstructorField{Name: col.GetField().GetFieldName(), Value: v}
		}
		if _, err := c.TypeFromProto(rc.ResultType); err != nil {
			return nil, err
		}
		return NewRawRecordConstructorValue(fields...), nil
	case p.GetSubscriptValue() != nil:
		index, err := c.ValueFromProto(p.GetSubscriptValue().GetIndex())
		if err != nil {
			return nil, err
		}
		source, err := c.ValueFromProto(p.GetSubscriptValue().GetSource())
		if err != nil {
			return nil, err
		}
		array, ok := source.Type().(*ArrayType)
		if !ok || array.ElementType == nil {
			return nil, fmt.Errorf("deserialize subscript: source is %v, not an array", source.Type())
		}
		return NewSubscriptValue(source, index, WithNullability(array.ElementType, true)), nil
	case p.GetLightArrayConstructorValue() != nil:
		ac := p.GetLightArrayConstructorValue().GetSuper()
		elems := make([]Value, len(ac.GetChildren()))
		for i, e := range ac.GetChildren() {
			v, err := c.ValueFromProto(e)
			if err != nil {
				return nil, err
			}
			elems[i] = v
		}
		t, err := c.TypeFromProto(ac.GetElementType())
		if err != nil {
			return nil, err
		}
		return NewArrayConstructorValue(t, elems), nil
	case p.GetLikeOperatorValue() != nil:
		src, err := c.ValueFromProto(p.GetLikeOperatorValue().GetSrcChild())
		if err != nil {
			return nil, err
		}
		pattern, err := c.ValueFromProto(p.GetLikeOperatorValue().GetPatternChild())
		if err != nil {
			return nil, err
		}
		return NewLikeOperatorValue(src, pattern), nil
	case p.GetPatternForLikeValue() != nil:
		pattern, err := c.ValueFromProto(p.GetPatternForLikeValue().GetPatternChild())
		if err != nil {
			return nil, err
		}
		escape, err := c.ValueFromProto(p.GetPatternForLikeValue().GetEscapeChild())
		if err != nil {
			return nil, err
		}
		return NewPatternForLikeValue(pattern, escape), nil
	case p.GetPromoteValue() != nil:
		in, err := c.ValueFromProto(p.GetPromoteValue().GetInValue())
		if err != nil {
			return nil, err
		}
		to, err := c.TypeFromProto(p.GetPromoteValue().GetPromoteToType())
		if err != nil {
			return nil, err
		}
		return NewPromoteValue(in, to), nil
	}
	return nil, fmt.Errorf("deserialize value: unsupported %v", p)
}
