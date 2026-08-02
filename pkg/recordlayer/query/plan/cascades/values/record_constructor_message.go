package values

import (
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildRecordMessage is the message half of Java's RecordConstructorValue.eval
// (RecordConstructorValue.java:113-139): build a DynamicMessage of the baked
// descriptor and set field i from child i.
//
// The binding is POSITIONAL, exactly as Java's is — Java indexes
// `fieldDescriptors.get(i)` against `getResultType().getFields().get(i)` and
// never consults a name. That matters here beyond mere fidelity: the
// descriptor's field names are the ESCAPED forms
// (protoname.ToProtoBufCompliantName, applied in defineRecordLocked), so a
// name-keyed set would miss any field whose identifier the escaper rewrites.
// Ordinal i of the descriptor is ordinal i of the record type by construction
// (defineRecordLocked numbers fields i+1 in declaration order), so position is
// the only sound key.
//
// A nil child leaves its field ABSENT rather than setting a zero — absence is
// what makes the field read back as SQL NULL. Java guards the same branch with
// `Verify.verify(fieldType.isNullable())`; the Go equivalent is
// NonNullableFieldError, raised only where the descriptor can actually express
// non-nullability (a repeated/required field), which is the same set
// BuildStructMessage rejects on.
func buildRecordMessage(md protoreflect.MessageDescriptor, fields []RecordConstructorField, evalCtx any) (any, error) {
	msg := dynamicpb.NewMessage(md)
	fds := md.Fields()
	if fds.Len() != len(fields) {
		// The descriptor was synthesised from this constructor's own Type(),
		// so a length disagreement means the stamp and the value drifted
		// apart — loud, because a positional bind under drift writes every
		// field to the wrong slot.
		return nil, &ProtoTypeError{
			TypeName: string(md.FullName()),
			Reason: fmt.Sprintf("baked descriptor has %d fields but the constructor has %d",
				fds.Len(), len(fields)),
		}
	}
	for i, f := range fields {
		fd := fds.Get(i)
		var childVal any
		if f.Value != nil {
			v, err := f.Value.Evaluate(evalCtx)
			if err != nil {
				return nil, err
			}
			childVal = v
		}
		if childVal == nil {
			if fd.Cardinality() == protoreflect.Required || fd.IsList() {
				return nil, &NonNullableFieldError{Field: string(fd.Name())}
			}
			continue
		}
		pv, err := rowValueToProtoField(msg, fd, childVal)
		if err != nil {
			return nil, err
		}
		msg.Set(fd, pv)
	}
	return msg, nil
}

// rowValueToProtoField converts one engine row value into the protoreflect
// value its BAKED field expects. It is the inverse of ProtoFieldToRowValue and
// the values-package sibling of the executor's goToProtoValue and the SQL
// layer's ConvertToProtoValue — three lanes because each has a different
// source domain, which is Java's shape too (RecordConstructorValue.eval has
// its own protoObjectForPrimitive/deepCopyIfNeeded lane distinct from the DML
// coercion path).
//
// This lane is narrower than the other two on purpose: the descriptor was
// synthesised FROM the same types the children evaluate to, so there is no
// cross-type promotion to perform. The width conversions below exist only
// because the engine's row domain is coarser than proto's (every SQL integer
// is an int64 in a row, whether the field is INT32 or INT64).
func rowValueToProtoField(parent protoreflect.Message, fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	if fd.IsList() {
		elems, ok := v.([]any)
		if !ok {
			return protoreflect.Value{}, &ProtoTypeError{
				TypeName: string(fd.FullName()),
				Reason:   fmt.Sprintf("repeated field needs a list, got %T", v),
			}
		}
		list := parent.NewField(fd).List()
		for _, e := range elems {
			if e == nil {
				// proto has no null list element; Java's array deep-copy has
				// the same hole (Verify.verifyNotNull on each element).
				return protoreflect.Value{}, &NonNullableFieldError{Field: string(fd.Name())}
			}
			ev, err := rowScalarToProtoValue(fd, e)
			if err != nil {
				return protoreflect.Value{}, err
			}
			list.Append(ev)
		}
		return protoreflect.ValueOfList(list), nil
	}
	return rowScalarToProtoValue(fd, v)
}

// rowScalarToProtoValue converts a single (non-repeated) row value.
func rowScalarToProtoValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if b, ok := v.(bool); ok {
			return protoreflect.ValueOfBool(b), nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		if n, ok := asInt64(v); ok {
			return protoreflect.ValueOfInt32(int32(n)), nil //nolint:gosec
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		if n, ok := asInt64(v); ok {
			return protoreflect.ValueOfInt64(n), nil
		}
	case protoreflect.FloatKind:
		if f, ok := asFloat64(v); ok {
			return protoreflect.ValueOfFloat32(float32(f)), nil
		}
	case protoreflect.DoubleKind:
		if f, ok := asFloat64(v); ok {
			return protoreflect.ValueOfFloat64(f), nil
		}
	case protoreflect.StringKind:
		if s, ok := v.(string); ok {
			return protoreflect.ValueOfString(s), nil
		}
	case protoreflect.BytesKind:
		if b, ok := v.([]byte); ok {
			return protoreflect.ValueOfBytes(b), nil
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return rowMessageToProtoValue(fd, v)
	}
	return protoreflect.Value{}, &ProtoTypeError{
		TypeName: string(fd.FullName()),
		Reason:   fmt.Sprintf("cannot store %T in a %s field", v, fd.Kind()),
	}
}

// rowMessageToProtoValue handles the two message-shaped row values a baked
// field can receive: a UUID (carried as a neutral [16]byte, matching
// PromoteValue's representation) and a nested record.
//
// A nested record arrives as a proto.Message because the plan-time bake stamps
// every constructor in the plan from ONE repository, so the child constructor
// already built a message of exactly this field's descriptor. That is the
// property that lets Java's deepCopyIfNeeded reconciliation
// (RecordConstructorValue.java:165-216) have nothing to reconcile here; when
// the descriptors disagree anyway the copy is still performed rather than
// assumed away, so a mixed-provenance message cannot be set into a foreign
// descriptor.
func rowMessageToProtoValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	md := fd.Message()
	if md == nil {
		return protoreflect.Value{}, &ProtoTypeError{
			TypeName: string(fd.FullName()), Reason: "message field has no descriptor",
		}
	}
	if string(md.FullName()) == uuidProtoMessageName {
		if b, ok := v.([16]byte); ok {
			dyn := dynamicpb.NewMessage(md)
			mostFD := md.Fields().ByName("most_significant_bits")
			leastFD := md.Fields().ByName("least_significant_bits")
			if mostFD == nil || leastFD == nil {
				return protoreflect.Value{}, &ProtoTypeError{
					TypeName: string(md.FullName()),
					Reason:   "UUID message is missing its most/least_significant_bits fields",
				}
			}
			dyn.Set(mostFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(b[0:8]))))   //nolint:gosec
			dyn.Set(leastFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(b[8:16])))) //nolint:gosec
			return protoreflect.ValueOfMessage(dyn), nil
		}
	}
	if pm, ok := v.(proto.Message); ok {
		src := pm.ProtoReflect()
		if src.Descriptor() == md {
			return protoreflect.ValueOfMessage(src), nil
		}
		dst := dynamicpb.NewMessage(md)
		if err := copyFieldsByNumber(dst, src); err != nil {
			return protoreflect.Value{}, err
		}
		return protoreflect.ValueOfMessage(dst), nil
	}
	return protoreflect.Value{}, &ProtoTypeError{
		TypeName: string(fd.FullName()),
		Reason:   fmt.Sprintf("cannot store %T in message field", v),
	}
}

// copyFieldsByNumber is the values-local half of Java's
// MessageHelpers.deepCopyMessageIfNeeded (MessageHelpers.java:247-295): copy
// every present field across by FIELD NUMBER, recursing into messages. Field
// number is the join key because it is the wire identity; the two descriptors
// are wire-compatible by construction or the copy is meaningless.
func copyFieldsByNumber(dst, src protoreflect.Message) error {
	var err error
	src.Range(func(sfd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		tfd := dst.Descriptor().Fields().ByNumber(sfd.Number())
		if tfd == nil {
			err = &ProtoTypeError{
				TypeName: string(dst.Descriptor().FullName()),
				Reason: fmt.Sprintf("source field %s (number %d) has no counterpart",
					sfd.FullName(), sfd.Number()),
			}
			return false
		}
		switch {
		case sfd.IsList():
			list := dst.Mutable(tfd).List()
			sl := v.List()
			for i := 0; i < sl.Len(); i++ {
				if sfd.Kind() == protoreflect.MessageKind || sfd.Kind() == protoreflect.GroupKind {
					sub := dynamicpb.NewMessage(tfd.Message())
					if cerr := copyFieldsByNumber(sub, sl.Get(i).Message()); cerr != nil {
						err = cerr
						return false
					}
					list.Append(protoreflect.ValueOfMessage(sub))
					continue
				}
				list.Append(sl.Get(i))
			}
		case sfd.Kind() == protoreflect.MessageKind || sfd.Kind() == protoreflect.GroupKind:
			sub := dynamicpb.NewMessage(tfd.Message())
			if cerr := copyFieldsByNumber(sub, v.Message()); cerr != nil {
				err = cerr
				return false
			}
			dst.Set(tfd, protoreflect.ValueOfMessage(sub))
		default:
			dst.Set(tfd, v)
		}
		return true
	})
	return err
}

// asInt64 widens the engine's integer carriers. The row domain uses int64 for
// every SQL integer, but hand-built values and constant folding can produce
// the narrower Go types.
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	}
	return 0, false
}

// asFloat64 widens the engine's floating carriers, including the integer
// literals that reach a DOUBLE field (`(1, 1.0)`'s first element typed against
// a double column).
func asFloat64(v any) (float64, bool) {
	switch f := v.(type) {
	case float64:
		return f, true
	case float32:
		return float64(f), true
	case int64:
		return float64(f), true
	case int32:
		return float64(f), true
	case int:
		return float64(f), true
	}
	return 0, false
}
