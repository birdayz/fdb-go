package values

import "fmt"

// IncompatibleTypeError is Java's SemanticException INCOMPATIBLE_TYPE raised
// while a value is admitted to a slot of another type: no promotion takes the
// value's type to the slot's. The SQL layer renders it as 22000, "A value
// cannot be assigned to a variable because …".
type IncompatibleTypeError struct {
	// Field is the slot's path from the admitted record, dotted; empty for
	// a bare value.
	Field    string
	From, To Type
}

func (e *IncompatibleTypeError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("incompatible type: %s cannot be promoted to %s (field %s)", e.From, e.To, e.Field)
	}
	return fmt.Sprintf("incompatible type: %s cannot be promoted to %s", e.From, e.To)
}

// CheckPromotionsTrie is the admission half of Java's
// PromoteValue.computePromotionsTrie (PromoteValue.java:355-443): whether a
// value of type current can be stored in a slot of type target, walking
// records field by field (by position) and arrays element-wise. It answers
// only the verdict; carrying out the promotion is the caller's converter.
//
//   - ANY takes anything.
//   - A primitive (Java's TypeCode.isPrimitive: NULL, BOOLEAN, BYTES, DOUBLE,
//     FLOAT, INT, LONG, STRING, VERSION, and Go's DATE and TIMESTAMP) of the
//     slot's code needs nothing; into an array or a record it is refused
//     (isPromotionNeeded's check); otherwise it needs a promotion the
//     promotion map holds (IsPromotable).
//   - NONE (the untyped `[]`) needs NONE_TO_ARRAY.
//   - An enum takes only an enum equal to it but for nullability (Type.Enum
//     compares its values, not its name); a UUID a UUID.
//   - Records need as many fields, each admitted; arrays their elements.
//
// Where the two codes differ past the primitive arm (an enum into a string, an
// array into a scalar) Java fails a bare Verify.verify, an internal error
// ("XX000 null"); that is a refusal Java did not design, and this answers it
// with the designed one, INCOMPATIBLE_TYPE.
//
// An UNKNOWN current type (Go's unresolved placeholder, which Java never plans
// with) is not a verdict: it is admitted here and left to the converter.
func CheckPromotionsTrie(target, current Type) error {
	return checkPromotions("", target, current)
}

func checkPromotions(path string, target, current Type) error {
	if target == nil || current == nil {
		return nil
	}
	if target.Code() == TypeCodeAny || current.Code() == TypeCodeUnknown || target.Code() == TypeCodeUnknown {
		return nil
	}
	refuse := func() error { return &IncompatibleTypeError{Field: path, From: current, To: target} }
	if isPromotionPrimitive(current.Code()) {
		if current.Code() == target.Code() {
			return nil
		}
		if current.Code() != TypeCodeNull && !isPromotionPrimitive(target.Code()) &&
			target.Code() != TypeCodeEnum && target.Code() != TypeCodeUuid {
			return refuse()
		}
		if !IsPromotable(current, target) {
			return refuse()
		}
		return nil
	}
	if current.Code() == TypeCodeNone {
		if target.Code() == TypeCodeArray {
			return nil
		}
		return refuse()
	}
	if current.Code() != target.Code() {
		return refuse()
	}
	switch c := current.(type) {
	case *EnumType:
		if !WithNullability(c, false).Equals(WithNullability(target, false)) {
			return refuse()
		}
		return nil
	case *ArrayType:
		t, ok := target.(*ArrayType)
		if !ok || c.ElementType == nil || t.ElementType == nil {
			return refuse()
		}
		return checkPromotions(path, t.ElementType, c.ElementType)
	case *RecordType:
		t, ok := target.(*RecordType)
		if !ok || len(t.Fields) != len(c.Fields) {
			return refuse()
		}
		for i := range t.Fields {
			name := t.Fields[i].Name
			if path != "" {
				name = path + "." + name
			}
			if err := checkPromotions(name, t.Fields[i].FieldType, c.Fields[i].FieldType); err != nil {
				return err
			}
		}
		return nil
	}
	// UUID, and any other code equal on both sides.
	return nil
}

// isPromotionPrimitive is Java's TypeCode.isPrimitive over Go's codes, with
// Go's temporal extension codes (STRING-backed) counted as primitives.
func isPromotionPrimitive(c TypeCode) bool {
	switch c {
	case TypeCodeNull, TypeCodeBoolean, TypeCodeBytes, TypeCodeDouble, TypeCodeFloat,
		TypeCodeInt, TypeCodeLong, TypeCodeString, TypeCodeVersion, TypeCodeDate, TypeCodeTimestamp:
		return true
	}
	return false
}

// NullAssignmentError is Java's SemanticException NULL_ASSIGNMENT: a NULL
// assigned to a slot whose type is not nullable, found when the value is
// applied (MessageHelpers.coerceObject, per row). The SQL layer renders it as
// XX000, "A null value cannot be assigned to a variable that is of a
// non-nullable type."
type NullAssignmentError struct {
	// Field is the slot's path, dotted.
	Field string
	To    Type
}

func (e *NullAssignmentError) Error() string {
	return fmt.Sprintf("a null value cannot be assigned to %s of the non-nullable type %s", e.Field, e.To)
}
