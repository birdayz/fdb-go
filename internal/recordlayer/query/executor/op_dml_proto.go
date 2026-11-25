// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/internal/recordlayer/protoscope"
	"fdb.dev/internal/recordlayer/vectorcodec"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// rematerializeProtoValue copies composite planning values into the target
// schema descriptor. This is the Go equivalent of Java
// MessageHelpers.deepCopyMessageIfNeeded: field numbers and wire bytes are
// preserved, but every nested message/list is owned by the descriptor on which
// it will be set.
func rematerializeProtoValue(fd protoreflect.FieldDescriptor, value protoreflect.Value) (protoreflect.Value, error) {
	if fd.IsMap() {
		holder := dynamicpb.NewMessage(fd.ContainingMessage())
		target := holder.Mutable(fd).Map()
		var copyErr error
		value.Map().Range(func(key protoreflect.MapKey, source protoreflect.Value) bool {
			copied, err := rematerializeProtoScalar(fd.MapValue(), source)
			if err != nil {
				copyErr = err
				return false
			}
			target.Set(key, copied)
			return true
		})
		if copyErr != nil {
			return protoreflect.Value{}, copyErr
		}
		return protoreflect.ValueOfMap(target), nil
	}
	if fd.IsList() {
		holder := dynamicpb.NewMessage(fd.ContainingMessage())
		target := holder.Mutable(fd).List()
		source := value.List()
		for i := 0; i < source.Len(); i++ {
			copied, err := rematerializeProtoScalar(fd, source.Get(i))
			if err != nil {
				return protoreflect.Value{}, err
			}
			target.Append(copied)
		}
		return protoreflect.ValueOfList(target), nil
	}
	return rematerializeProtoScalar(fd, value)
}

func rematerializeProtoScalar(fd protoreflect.FieldDescriptor, value protoreflect.Value) (protoreflect.Value, error) {
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return value, nil
	}
	source := value.Message()
	if source.Descriptor() == fd.Message() {
		return value, nil
	}
	wireBytes, err := (proto.MarshalOptions{AllowPartial: true}).Marshal(source.Interface())
	if err != nil {
		return protoreflect.Value{}, fmt.Errorf("executor: copying composite insert value: %w", err)
	}
	target := dynamicpb.NewMessage(fd.Message())
	// Read as the target type's record value is read (proto_closed_enums.go): a
	// closed enum's undeclared number the source held as an unknown field is
	// not taken back into the field by the re-parse.
	if err := recordlayer.UnmarshalRecordAsJava(wireBytes, target, true); err != nil {
		return protoreflect.Value{}, fmt.Errorf("executor: copying composite insert value: %w", err)
	}
	return protoreflect.ValueOfMessage(target), nil
}

func goToProtoValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	// A repeated field (an ARRAY column) takes the []any an evaluated
	// array literal produces (UPDATE … SET arr = [..] / INSERT … SELECT
	// of a computed array) and converts each element through the same
	// scalar lanes — the executor-side mirror of the relational
	// ConvertToProtoValue repeated arm, so an array written via UPDATE
	// is byte-identical to one written via INSERT … VALUES.
	if inner, wrapped, ok := values.EffectiveListField(fd); ok {
		elems, elemsOK := v.([]any)
		if !elemsOK {
			// Fall to the scalar lanes' verbatim 22000 below.
			return protoreflect.Value{}, errIncompatibleAssignment()
		}
		var list protoreflect.List
		var result protoreflect.Value
		if wrapped {
			// NULLABLE array: write through the NullableArrayWrapper — a
			// present wrapper with an empty list keeps [] distinct from
			// NULL, Java's stored shape.
			wrapperMsg, wl := values.NewWrappedArrayMessage(fd)
			list = wl
			result = protoreflect.ValueOfMessage(wrapperMsg)
		} else {
			parent := dynamicpb.NewMessage(fd.ContainingMessage())
			lv := parent.NewField(fd)
			list = lv.List()
			result = lv
		}
		for _, e := range elems {
			if e == nil {
				// A write backstop: admission refuses NULL elements first.
				return protoreflect.Value{}, &values.NullArrayElementError{}
			}
			pv, err := goToProtoScalarValue(inner, e)
			if err != nil {
				return protoreflect.Value{}, err
			}
			list.Append(pv)
		}
		return result, nil
	}
	return goToProtoScalarValue(fd, v)
}

func goToProtoScalarValue(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error) {
	if pv, handled, err := goToProtoNumericScalar(fd, v); handled {
		return pv, err
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		switch s := v.(type) {
		case string:
			return protoreflect.ValueOfString(s), nil
		}
	case protoreflect.BytesKind:
		switch b := v.(type) {
		case []byte:
			if target, ok := values.FieldTypeForProtoField(fd).(*values.VectorType); ok {
				_, payload, stride, valid := vectorcodec.Payload(b)
				if !valid || stride*8 != target.Precision || len(payload) != target.Dimensions*stride {
					return protoreflect.Value{}, incompatibleTypef("Vector precision or dimensions do not match the target column")
				}
			}
			return protoreflect.ValueOfBytes(b), nil
		}
	case protoreflect.EnumKind:
		switch n := v.(type) {
		case int64:
			// The enum carrier: an enum-typed value holds its declared number.
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
		case string:
			// A string assigned to an enum column is promoted as Java's
			// STRING_TO_ENUM does (the INSERT … VALUES converter's rule).
			num, err := values.StringToEnumNumber(fd.Enum(), n)
			if err != nil {
				return protoreflect.Value{}, err
			}
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(num)), nil
		}
	case protoreflect.MessageKind:
		if pv, handled, err := goToProtoMessageScalar(fd, v); handled {
			return pv, err
		}
	}
	// All promotable conversions are handled above, so anything reaching here is
	// a genuinely incompatible assignment (e.g. a float64/DOUBLE into an integer
	// column — DOUBLE→LONG has no edge in Java's promotion lattice). Emit the
	// verbatim 22000 SemanticException, matching Java's PromoteValue rejection and
	// the sibling ConvertToProtoValue fallthrough — not a generic Go error.
	return protoreflect.Value{}, errIncompatibleAssignment()
}

func goToProtoNumericScalar(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, bool, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if b, ok := v.(bool); ok {
			return protoreflect.ValueOfBool(b), true, nil
		}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		switch n := v.(type) {
		case int64:
			if n < math.MinInt32 || n > math.MaxInt32 {
				return protoreflect.Value{}, true, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfInt32(int32(n)), true, nil
		case int32:
			return protoreflect.ValueOfInt32(n), true, nil
		case int:
			if int64(n) < math.MinInt32 || int64(n) > math.MaxInt32 {
				return protoreflect.Value{}, true, &NumericRangeOverflowError{Value: int64(n), Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfInt32(int32(n)), true, nil
		}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		switch n := v.(type) {
		case int64:
			return protoreflect.ValueOfInt64(n), true, nil
		case int:
			return protoreflect.ValueOfInt64(int64(n)), true, nil
		}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		// An INT column holding a signed Integer in the target (see
		// functions.ConvertToProtoValue): the INT range, stored as its 32 bits.
		switch n := v.(type) {
		case int64:
			if n < math.MinInt32 || n > math.MaxInt32 {
				return protoreflect.Value{}, true, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfUint32(uint32(int32(n))), true, nil
		case uint32:
			return protoreflect.ValueOfUint32(n), true, nil
		}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// A signed Long in the target: every int64 is stored as its 64 bits.
		switch n := v.(type) {
		case int64:
			return protoreflect.ValueOfUint64(uint64(n)), true, nil
		case uint64:
			return protoreflect.ValueOfUint64(n), true, nil
		}
	case protoreflect.FloatKind:
		switch n := v.(type) {
		case float64:
			// Narrowing a FLOAT's widened carrier is exact, ±Infinity included;
			// only a finite value beyond the float32 range would change, and no
			// FLOAT-typed value is one (functions.ConvertToProtoValue's FLOAT
			// arm, which answers the same).
			if !math.IsInf(n, 0) && (n > math.MaxFloat32 || n < -math.MaxFloat32) {
				return protoreflect.Value{}, true, &NumericRangeOverflowError{Value: n, Column: string(fd.Name()), TypeName: fd.Kind().String()}
			}
			return protoreflect.ValueOfFloat32(float32(n)), true, nil
		case float32:
			return protoreflect.ValueOfFloat32(n), true, nil
		// INT/LONG→FLOAT are promotable in Java's lattice; widen rather than
		// falling through to the 22000 reject (e.g. SUM(BIGINT) into a FLOAT
		// column). Matches ConvertToProtoValue's VALUES path. No range check,
		// deliberately: Java's PromoteValue.LONG_TO_FLOAT is the plain
		// widening cast Float.valueOf((Long)in) — precision-lossy above 2^24,
		// never ±Inf (MaxInt64 ≈ 9.2e18 < MaxFloat32 ≈ 3.4e38, so overflow is
		// unreachable from an integer; the float64 arm above range-checks
		// because float64 CAN exceed float32 range). Go's float32(n) is the
		// identical semantics.
		case int64:
			return protoreflect.ValueOfFloat32(float32(n)), true, nil
		case int:
			return protoreflect.ValueOfFloat32(float32(n)), true, nil
		}
	case protoreflect.DoubleKind:
		switch n := v.(type) {
		case float64:
			return protoreflect.ValueOfFloat64(n), true, nil
		// INT/LONG→DOUBLE are promotable; a SUM(BIGINT)/COUNT into a DOUBLE
		// column must widen (this path previously fell through and errored).
		case int64:
			return protoreflect.ValueOfFloat64(float64(n)), true, nil
		case int:
			return protoreflect.ValueOfFloat64(float64(n)), true, nil
		}
	}
	return protoreflect.Value{}, false, nil
}

func goToProtoMessageScalar(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, bool, error) {
	// A UUID column is the tuple_fields.UUID message. UPDATE SET uuid_col =
	// … and INSERT … SELECT flow the value through here (INSERT … VALUES uses
	// functions.ConvertToProtoValue instead). Accept the neutral [16]byte
	// (SET v = v, an index/record-sourced value) and the canonical string
	// (SET v = '<uuid>'), building the same msb/lsb message Java writes —
	// otherwise a valid UUID assignment fell through to the 22000 reject.
	if msg := fd.Message(); msg != nil && string(msg.FullName()) == uuidProtoMessageName {
		switch u := v.(type) {
		case [16]byte:
			pv, err := uuidBytesToProtoMessage(fd, u)
			return pv, true, err
		case uuid.UUID:
			pv, err := uuidBytesToProtoMessage(fd, u)
			return pv, true, err
		case string:
			parsed, ok := values.ParseJavaUUID(u)
			if !ok {
				return protoreflect.Value{}, true, &values.InvalidUUIDValueError{Value: u}
			}
			pv, err := uuidBytesToProtoMessage(fd, parsed)
			return pv, true, err
		}
	}
	// A STRUCT value flowing from a SELECT (INSERT … SELECT, UPDATE SET
	// struct_col = other_struct_col): the value layer carries a struct
	// cell as the proto message it was read from
	// (values.protoScalarToRowValue), and Java writes it by COPYING that
	// message into the target's descriptor rather than rebuilding it
	// field by field — RecordConstructorValue.deepCopyIfNeeded's record
	// arm (RecordConstructorValue.java:207-215), which exists precisely
	// because the source message may be backed by a different (but
	// wire-compatible) descriptor than the target.
	if src, isMsg := v.(protoreflect.ProtoMessage); isMsg {
		if pv, err := copyMessageIntoDescriptor(fd, src.ProtoReflect()); err == nil {
			return pv, true, nil
		}
	}
	// A struct LITERAL assigned by UPDATE: the typed row constructor the
	// SET push-down built evaluates to a field map, and the message is
	// assembled by the same builder the plan-time writer uses
	// (values.BuildStructMessage — Java's RecordConstructorValue.eval
	// structure), with THIS path's scalar lanes.
	if m, isMap := v.(map[string]any); isMap {
		pv, err := values.BuildStructMessage(fd.Message(), m, goToProtoValue)
		if err != nil {
			var nn *values.NonNullableFieldError
			if errors.As(err, &nn) {
				return protoreflect.Value{}, true, &ExecutionError{Kind: ExecNotNullViolation, Message: nn.Error()}
			}
			return protoreflect.Value{}, true, err
		}
		return pv, true, nil
	}
	return protoreflect.Value{}, false, nil
}

// incompatibleTypef is an incompatible assignment (22000) with a formatted message.
func incompatibleTypef(format string, args ...any) error {
	return &values.ScalarTypeMismatchError{Message: fmt.Sprintf(format, args...)}
}

// errIncompatibleAssignment is Java's PromoteValue rejection, verbatim (22000).
func errIncompatibleAssignment() error {
	return &values.ScalarTypeMismatchError{
		Message: "A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable.",
	}
}

// uuidBytesToProtoMessage builds a tuple_fields.UUID proto message (msb/lsb,
// big-endian) from a 16-byte UUID — the write-side inverse of uuidMessageToBytes
// and the mirror of functions.uuidStringToProtoMessage, so a UUID written via
// UPDATE / INSERT…SELECT is byte-identical to one written via INSERT … VALUES.
func uuidBytesToProtoMessage(fd protoreflect.FieldDescriptor, b [16]byte) (protoreflect.Value, error) {
	msgDesc := fd.Message()
	mostFD := msgDesc.Fields().ByName("most_significant_bits")
	leastFD := msgDesc.Fields().ByName("least_significant_bits")
	if mostFD == nil || leastFD == nil {
		return protoreflect.Value{}, &ExecutionError{
			Kind:    ExecInternalError,
			Message: "UUID message descriptor missing most/least_significant_bits fields",
		}
	}
	dyn := dynamicpb.NewMessage(msgDesc)
	dyn.Set(mostFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(b[0:8]))))   //nolint:gosec
	dyn.Set(leastFD, protoreflect.ValueOfInt64(int64(binary.BigEndian.Uint64(b[8:16])))) //nolint:gosec
	return protoreflect.ValueOfMessage(dyn), nil
}

// copyMessageIntoDescriptor is Java's MessageHelpers.deepCopyMessageIfNeeded
// (MessageHelpers.java:247-255): a message already backed by the target
// descriptor is used as-is; otherwise its fields are copied into a fresh
// message of the target descriptor, keyed BY FIELD NUMBER, recursively
// (:258-295).
//
// The copy exists because the SAME struct type reaches the write path
// through descriptors built independently — the scan's record descriptor and
// the insert target's — and protobuf identity is per-descriptor-instance.
// Field NUMBER is the join key (never name), which is the same wire-identity
// rule the rest of the layer follows; enum values cross by NAME, as Java's
// copy does, because two descriptors may number an enum differently.
func copyMessageIntoDescriptor(fd protoreflect.FieldDescriptor, src protoreflect.Message) (protoreflect.Value, error) {
	target := fd.Message()
	if target == nil {
		return protoreflect.Value{}, incompatibleTypef("field %s is not a message field", fd.FullName())
	}
	if src.Descriptor() == target {
		return protoreflect.ValueOfMessage(src), nil
	}
	dst := dynamicpb.NewMessage(target)
	if err := deepCopyMessageFields(dst, src); err != nil {
		return protoreflect.Value{}, err
	}
	return protoreflect.ValueOfMessage(dst), nil
}

// deepCopyMessageFields copies every PRESENT field of src onto dst, matching
// fields by number. Java's merge branch for an already-set message field
// (MessageHelpers.java:277-286) has no counterpart here because dst is always
// freshly built, so the "existing value is the default instance" test it
// guards is unconditionally true.
func deepCopyMessageFields(dst, src protoreflect.Message) error {
	var err error
	src.Range(func(sfd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		tfd := dst.Descriptor().Fields().ByNumber(sfd.Number())
		if tfd == nil {
			// A field the target does not declare. Java's copy would throw
			// on the null target field; the assignment is genuinely
			// incompatible, so it takes the caller's 22000.
			err = incompatibleTypef("source field %s (number %d) has no counterpart in %s",
				sfd.FullName(), sfd.Number(), dst.Descriptor().FullName())
			return false
		}
		switch {
		case sfd.IsList():
			list := dst.Mutable(tfd).List()
			srcList := v.List()
			for i := 0; i < srcList.Len(); i++ {
				ev, cerr := copyElement(tfd, sfd, srcList.Get(i))
				if cerr != nil {
					err = cerr
					return false
				}
				list.Append(ev)
			}
		case sfd.IsMap():
			err = incompatibleTypef("map field %s cannot be assigned", sfd.FullName())
			return false
		default:
			ev, cerr := copyElement(tfd, sfd, v)
			if cerr != nil {
				err = cerr
				return false
			}
			dst.Set(tfd, ev)
		}
		return true
	})
	return err
}

// copyElement copies one non-repeated value from a source field onto its
// target field: messages recurse, enums cross by NAME, scalars pass through.
func copyElement(tfd, sfd protoreflect.FieldDescriptor, v protoreflect.Value) (protoreflect.Value, error) {
	switch sfd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return copyMessageIntoDescriptor(tfd, v.Message())
	case protoreflect.EnumKind:
		srcVal := sfd.Enum().Values().ByNumber(v.Enum())
		if srcVal == nil {
			return protoreflect.Value{}, incompatibleTypef("enum field %s carries unknown value %d", sfd.FullName(), v.Enum())
		}
		tgtVal := tfd.Enum().Values().ByName(srcVal.Name())
		if tgtVal == nil {
			return protoreflect.Value{}, incompatibleTypef("enum value %s is not declared by %s", srcVal.Name(), protoscope.JavaFullName(tfd.Enum()))
		}
		return protoreflect.ValueOfEnum(tgtVal.Number()), nil
	default:
		return v, nil
	}
}
