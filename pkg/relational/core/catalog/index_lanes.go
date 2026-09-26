package catalog

import (
	"sort"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
)

// checkIndexLanes is the build path's lane check (ws-j-design.md section 3.2):
// an index key the target cannot plan is refused before Go stores it. Java
// builds an index's arithmetic function keys (add, sub, mul, div, mod, the bit
// operators and the bitmap functions) into Values through
// ArithmeticValue.encapsulate, which fails every query of the table when the
// operands' types name no row of its operator table (values.LookupArithmeticLane);
// Java's programmatic API stores such a key without complaint, and Go refuses it
// here instead (a declared divergence, section 9 (m)). A DDL-origin key never
// reaches this: the key generator refuses it at its clause with the target's
// XX000 first. What this refuses is meta-data built by hand.
//
// Every operand is typed by the RESULT type of the Value Java builds for it
// (keyOperandType): a field by its descriptor type, a literal by its carrier, a
// nested arithmetic key by its lane's result, an order function by
// ToOrderedBytesValue's BYTES, CARDINALITY by INT and a collation key by
// CollateValue's BYTES; anything else is UNKNOWN, which no lane names.
//
// only names the indexes the save DEFINES (a fresh template's every index; a
// new version's NEW and CHANGED ones); an EQUIVALENT index carries the bytes the
// tenant already has and is not re-checked, so a stored lane-less key never
// refuses a new version that keeps it. nil checks every index.
func checkIndexLanes(md *recordlayer.RecordMetaData, p *gen.MetaData, only map[string]bool) error {
	for _, idx := range p.GetIndexes() {
		if only != nil && !only[idx.GetName()] {
			continue
		}
		for _, desc := range indexRecordDescriptors(md, idx) {
			c := laneChecker{index: idx.GetName()}
			if err := c.walk(idx.GetRootExpression(), desc); err != nil {
				return err
			}
		}
	}
	return nil
}

// indexRecordDescriptors is the descriptors of the record types an index
// covers, every record type (in name order) for an index that names none.
func indexRecordDescriptors(md *recordlayer.RecordMetaData, idx *gen.Index) []protoreflect.MessageDescriptor {
	names := idx.GetRecordType()
	if len(names) == 0 {
		for name := range md.RecordTypes() {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	out := make([]protoreflect.MessageDescriptor, 0, len(names))
	for _, name := range names {
		if rt := md.GetRecordType(name); rt != nil && rt.Descriptor != nil {
			out = append(out, rt.Descriptor)
		}
	}
	return out
}

type laneChecker struct{ index string }

// walk finds every function key in e and checks it (keyOperandType checks an
// arithmetic function's operands, and walks any other function's arguments).
func (c laneChecker) walk(e *gen.KeyExpression, desc protoreflect.MessageDescriptor) error {
	switch {
	case e == nil:
		return nil
	case e.GetThen() != nil:
		for _, child := range e.GetThen().GetChild() {
			if err := c.walk(child, desc); err != nil {
				return err
			}
		}
	case e.GetList() != nil:
		for _, child := range e.GetList().GetChild() {
			if err := c.walk(child, desc); err != nil {
				return err
			}
		}
	case e.GetNesting() != nil:
		return c.walk(e.GetNesting().GetChild(), nestedDescriptor(desc, e.GetNesting().GetParent()))
	case e.GetGrouping() != nil:
		return c.walk(e.GetGrouping().GetWholeKey(), desc)
	case e.GetKeyWithValue() != nil:
		return c.walk(e.GetKeyWithValue().GetInnerKey(), desc)
	case e.GetSplit() != nil:
		return c.walk(e.GetSplit().GetJoined(), desc)
	case e.GetDimensions() != nil:
		return c.walk(e.GetDimensions().GetWholeKey(), desc)
	case e.GetFunction() != nil:
		_, err := c.keyOperandType(e, desc)
		return err
	}
	return nil
}

// keyOperandType is the type code of the Value Java builds for e as an operand,
// checking e's lane when e is an arithmetic function.
func (c laneChecker) keyOperandType(e *gen.KeyExpression, desc protoreflect.MessageDescriptor) (values.TypeCode, error) {
	switch {
	case e == nil:
		return values.TypeCodeUnknown, nil
	case e.GetField() != nil:
		return fieldOperandType(desc, e.GetField()), nil
	case e.GetNesting() != nil:
		return c.keyOperandType(e.GetNesting().GetChild(), nestedDescriptor(desc, e.GetNesting().GetParent()))
	case e.GetValue() != nil:
		return literalOperandType(e.GetValue()), nil
	case e.GetRecordTypeKey() != nil:
		// RecordTypeValue's type (RecordTypeValue.java:126).
		return values.TypeCodeLong, nil
	case e.GetFunction() != nil:
		fn := e.GetFunction()
		valueName := keyValueFunctionName(fn.GetName())
		if !values.IsArithmeticFunction(valueName) {
			if err := c.walk(fn.GetArguments(), desc); err != nil {
				return values.TypeCodeUnknown, err
			}
			switch fn.GetName() {
			case recordlayer.OrderFuncAscNullsFirst, recordlayer.OrderFuncAscNullsLast,
				recordlayer.OrderFuncDescNullsFirst, recordlayer.OrderFuncDescNullsLast,
				recordlayer.CollateFuncJRE, recordlayer.CollateFuncICU:
				return values.TypeCodeBytes, nil
			case recordlayer.FunctionNameCardinality:
				return values.TypeCodeInt, nil
			}
			return values.TypeCodeUnknown, nil
		}
		args := []*gen.KeyExpression{fn.GetArguments()}
		if then := fn.GetArguments().GetThen(); then != nil {
			args = then.GetChild()
		}
		if len(args) != 2 {
			return values.TypeCodeUnknown, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"index %s cannot be stored: its key function %s takes two operands, not %d",
				c.index, fn.GetName(), len(args))
		}
		left, err := c.keyOperandType(args[0], desc)
		if err != nil {
			return values.TypeCodeUnknown, err
		}
		right, err := c.keyOperandType(args[1], desc)
		if err != nil {
			return values.TypeCodeUnknown, err
		}
		lane, ok := values.LookupArithmeticLane(valueName, left, right)
		if !ok {
			return values.TypeCodeUnknown, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"index %s cannot be stored: its key function %s has no lane for operand types (%s, %s), so no query of its table can be planned",
				c.index, fn.GetName(), left, right)
		}
		return lane.Result, nil
	}
	return values.TypeCodeUnknown, c.walk(e, desc)
}

// keyValueFunctionName is the BuiltInFunction a key function's Value is built
// with: LongArithmethicFunctionKeyExpression's valueFunctionName, which names
// subtract, multiply and divide by the Value's sub, mul and div
// (LongArithmethicFunctionKeyExpression.java:121-123, 247-252); every other key
// function's Value has its own name.
func keyValueFunctionName(name string) string {
	switch name {
	case "subtract":
		return "sub"
	case "multiply":
		return "mul"
	case "divide":
		return "div"
	}
	return name
}

// nestedDescriptor is the message a nesting's parent field holds, nil when
// the field is not a message.
func nestedDescriptor(desc protoreflect.MessageDescriptor, parent *gen.Field) protoreflect.MessageDescriptor {
	if desc == nil || parent == nil {
		return nil
	}
	fd := desc.Fields().ByName(protoreflect.Name(parent.GetFieldName()))
	if fd == nil {
		return nil
	}
	return fd.Message()
}

// fieldOperandType is Java's Type.TypeCode.fromProtobufFieldDescriptor for the
// field f names in desc, and ARRAY for a repeated field not fanned out (or a
// field holding a nullable-array wrapper). A field desc lacks is UNKNOWN.
func fieldOperandType(desc protoreflect.MessageDescriptor, f *gen.Field) values.TypeCode {
	if desc == nil {
		return values.TypeCodeUnknown
	}
	fd := desc.Fields().ByName(protoreflect.Name(f.GetFieldName()))
	if fd == nil {
		return values.TypeCodeUnknown
	}
	if fd.Cardinality() == protoreflect.Repeated && f.GetFanType() != gen.Field_FAN_OUT {
		return values.TypeCodeArray
	}
	switch fd.Kind() {
	case protoreflect.DoubleKind:
		return values.TypeCodeDouble
	case protoreflect.FloatKind:
		return values.TypeCodeFloat
	case protoreflect.Int64Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.Sfixed64Kind, protoreflect.Sint64Kind:
		return values.TypeCodeLong
	case protoreflect.Int32Kind, protoreflect.Fixed32Kind, protoreflect.Uint32Kind,
		protoreflect.Sfixed32Kind, protoreflect.Sint32Kind:
		return values.TypeCodeInt
	case protoreflect.BoolKind:
		return values.TypeCodeBoolean
	case protoreflect.StringKind:
		return values.TypeCodeString
	case protoreflect.EnumKind:
		return values.TypeCodeEnum
	case protoreflect.BytesKind:
		return values.TypeCodeBytes
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if fd.Cardinality() != protoreflect.Repeated && values.IsWrappedArrayDescriptor(fd.Message()) {
			return values.TypeCodeArray
		}
		return values.TypeCodeRecord
	}
	return values.TypeCodeUnknown
}

// literalOperandType is the type of the LiteralValue Java builds for a
// key-expression literal: its carrier's (LiteralKeyExpression.fromProto).
func literalOperandType(v *gen.Value) values.TypeCode {
	switch {
	case v.IntValue != nil:
		return values.TypeCodeInt
	case v.LongValue != nil:
		return values.TypeCodeLong
	case v.FloatValue != nil:
		return values.TypeCodeFloat
	case v.DoubleValue != nil:
		return values.TypeCodeDouble
	case v.StringValue != nil:
		return values.TypeCodeString
	case v.BoolValue != nil:
		return values.TypeCodeBoolean
	case v.BytesValue != nil:
		return values.TypeCodeBytes
	}
	return values.TypeCodeNull
}
