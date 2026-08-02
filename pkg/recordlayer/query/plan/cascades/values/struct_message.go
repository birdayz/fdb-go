package values

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// NonNullableFieldError reports a NULL assigned to a field whose type forbids
// it — Java's `Verify.verify(fieldType.isNullable(), "Cannot set a
// non-nullable field to the NULL value")` inside RecordConstructorValue.eval
// (RecordConstructorValue.java:135). Carried as a typed error so each caller
// can state it in its own error vocabulary (the SQL layer as 23502) without a
// second copy of the rule.
type NonNullableFieldError struct {
	Field string
}

func (e *NonNullableFieldError) Error() string {
	return fmt.Sprintf("NULL value in column %q violates NOT NULL constraint", e.Field)
}

// UndeclaredStructFieldError reports a record constructor carrying a name the
// target struct does not declare.
type UndeclaredStructFieldError struct {
	Struct string
	Extra  int
	Total  int
}

func (e *UndeclaredStructFieldError) Error() string {
	return fmt.Sprintf("record constructor for %q carries %d fields, %d of which the target struct declares",
		e.Struct, e.Total, e.Total-e.Extra)
}

// BuildStructMessage builds the nested message a STRUCT value stores, from
// the evaluated record constructor's field map. It is the structural half of
// Java's RecordConstructorValue.eval (RecordConstructorValue.java:113-139):
// walk the TARGET descriptor's fields, set each field present in the map,
// leave a NULL field ABSENT (:135's null branch — absence is what makes a
// nullable struct field read back as NULL), and reject a NULL at a
// non-nullable field.
//
// The per-field VALUE conversion is the caller's, passed in: the plan-time
// writer (INSERT … VALUES) and the executor writer (UPDATE, INSERT … SELECT)
// have their own scalar lanes, already documented as siblings, and this
// exists so the STRUCTURE is not implemented twice alongside them.
func BuildStructMessage(
	md protoreflect.MessageDescriptor,
	fields map[string]any,
	convert func(fd protoreflect.FieldDescriptor, v any) (protoreflect.Value, error),
) (protoreflect.Value, error) {
	msg := dynamicpb.NewMessage(md)
	fds := md.Fields()
	matched := 0
	for i := 0; i < fds.Len(); i++ {
		sub := fds.Get(i)
		v, present := fields[string(sub.Name())]
		if !present {
			continue
		}
		matched++
		if v == nil {
			// The only non-nullable type the SQL layer can declare is a NOT
			// NULL array, stored as a FLAT repeated field; proto2 `required`
			// is the same signal from a hand-authored descriptor.
			if sub.Cardinality() == protoreflect.Required || sub.IsList() {
				return protoreflect.Value{}, &NonNullableFieldError{Field: string(sub.Name())}
			}
			continue
		}
		pv, err := convert(sub, v)
		if err != nil {
			return protoreflect.Value{}, err
		}
		msg.Set(sub, pv)
	}
	if matched != len(fields) {
		return protoreflect.Value{}, &UndeclaredStructFieldError{
			Struct: string(md.Name()), Extra: len(fields) - matched, Total: len(fields),
		}
	}
	return protoreflect.ValueOfMessage(msg), nil
}
