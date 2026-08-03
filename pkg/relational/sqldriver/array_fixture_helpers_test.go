package sqldriver_test

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// setArrayField sets an ARRAY column on a dynamic message regardless of the
// stored shape (flat repeated vs NullableArrayWrapper — nullable arrays are
// stored wrapped, Java's wire shape). Element values for struct arrays must
// be built from arrayElementMessageDescriptor(fd), not fd.Message(), because
// fd.Message() is the wrapper for nullable struct arrays.
func setArrayField(m *dynamicpb.Message, fd protoreflect.FieldDescriptor, vals ...protoreflect.Value) {
	_, wrapped, ok := values.EffectiveListField(fd)
	if !ok {
		panic(fmt.Sprintf("setArrayField: %s is not an array-shaped field", fd.FullName()))
	}
	if wrapped {
		wrapper, list := values.NewWrappedArrayMessage(fd)
		for _, v := range vals {
			list.Append(v)
		}
		m.Set(fd, protoreflect.ValueOfMessage(wrapper))
		return
	}
	list := m.NewField(fd).List()
	for _, v := range vals {
		list.Append(v)
	}
	m.Set(fd, protoreflect.ValueOfList(list))
}

// arrayElementMessageDescriptor returns the element MESSAGE descriptor of a
// struct-array column (through the NullableArrayWrapper when present).
func arrayElementMessageDescriptor(fd protoreflect.FieldDescriptor) protoreflect.MessageDescriptor {
	inner, _, ok := values.EffectiveListField(fd)
	if !ok {
		panic(fmt.Sprintf("arrayElementMessageDescriptor: %s is not an array-shaped field", fd.FullName()))
	}
	if inner.Kind() != protoreflect.MessageKind {
		panic(fmt.Sprintf("arrayElementMessageDescriptor: %s has non-message elements", fd.FullName()))
	}
	return inner.Message()
}
