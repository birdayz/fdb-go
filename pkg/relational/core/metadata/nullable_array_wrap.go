package metadata

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
)

// Port of Java's NullableArrayUtils.wrapArray (fdb-relational-core
// com.apple.foundationdb.relational.util.NullableArrayUtils:79-190): a
// descriptor-driven post-pass over a serialized key expression that inserts
// the `values` hop wherever a referenced field is a NullableArrayWrapper
// message (e.g. reviews.rating -> reviews.values.rating). The stored index
// key expression must match the stored descriptor shape — Java writes
// nullable arrays wrapped, and so does Go's emitter — so any index over a
// nullable array column is rewritten through this pass before the template
// is built.

// wrapArrayKeyExpression mirrors NullableArrayUtils.wrapArray: identity
// when the template contains no nullable array (the gate Java threads from
// DdlVisitor's containsNullableArray flag); otherwise the recursive rewrite
// against the record type's descriptor.
func wrapArrayKeyExpression(ke *gen.KeyExpression, parent protoreflect.MessageDescriptor, containsNullableArray bool) *gen.KeyExpression {
	if !containsNullableArray {
		return ke
	}
	return wrapArrayInternal(ke, parent)
}

func wrapArrayInternal(ke *gen.KeyExpression, parent protoreflect.MessageDescriptor) *gen.KeyExpression {
	switch {
	case ke.GetThen() != nil:
		then := &gen.Then{}
		for _, child := range ke.GetThen().GetChild() {
			then.Child = append(then.Child, wrapArrayInternal(child, parent))
		}
		return &gen.KeyExpression{Then: then}

	case ke.GetNesting() != nil:
		par := ke.GetNesting().GetParent()
		parentFieldName := par.GetFieldName()
		child := ke.GetNesting().GetChild()
		if isWrappedArrayFieldRef(parentFieldName, parent) {
			// parent -> child is really parent -> values -> child: split the
			// parent into field(name, SCALAR) nesting field("values", fanType)
			// and re-hang the child one level deeper.
			wrappedParent := splitFieldIntoNestedWithValues(par)
			elemDesc := parent.Fields().ByName(protoreflect.Name(parentFieldName)).
				Message().Fields().ByName(protoreflect.Name(wrappedArrayFieldName)).Message()
			wrappedChild := wrapArrayInternal(child, elemDesc)
			newChild := &gen.KeyExpression{Nesting: &gen.Nesting{
				Parent: wrappedParent.GetChild().GetField(),
				Child:  wrappedChild,
			}}
			return &gen.KeyExpression{Nesting: &gen.Nesting{
				Parent: wrappedParent.GetParent(),
				Child:  newChild,
			}}
		}
		var childDesc protoreflect.MessageDescriptor
		if fd := parent.Fields().ByName(protoreflect.Name(parentFieldName)); fd != nil && fd.Kind() == protoreflect.MessageKind {
			childDesc = fd.Message()
		}
		return &gen.KeyExpression{Nesting: &gen.Nesting{
			Parent: par,
			Child:  wrapArrayInternal(child, childDesc),
		}}

	case ke.GetField() != nil:
		if isWrappedArrayFieldRef(ke.GetField().GetFieldName(), parent) {
			return &gen.KeyExpression{Nesting: splitFieldIntoNestedWithValues(ke.GetField())}
		}
		return ke

	case ke.GetGrouping() != nil:
		g := proto2Clone(ke).GetGrouping()
		g.WholeKey = wrapArrayInternal(g.GetWholeKey(), parent)
		return &gen.KeyExpression{Grouping: g}

	case ke.GetSplit() != nil:
		s := proto2Clone(ke).GetSplit()
		s.Joined = wrapArrayInternal(s.GetJoined(), parent)
		return &gen.KeyExpression{Split: s}

	case ke.GetFunction() != nil:
		f := proto2Clone(ke).GetFunction()
		f.Arguments = wrapArrayInternal(f.GetArguments(), parent)
		return &gen.KeyExpression{Function: f}

	case ke.GetKeyWithValue() != nil:
		k := proto2Clone(ke).GetKeyWithValue()
		k.InnerKey = wrapArrayInternal(k.GetInnerKey(), parent)
		return &gen.KeyExpression{KeyWithValue: k}

	case ke.GetList() != nil:
		l := &gen.List{}
		for _, item := range ke.GetList().GetChild() {
			l.Child = append(l.Child, wrapArrayInternal(item, parent))
		}
		return &gen.KeyExpression{List: l}

	default:
		// Empty / Version / RecordTypeKey / Value / Dimensions — no field
		// references to rewrite (Java falls through unchanged).
		return ke
	}
}

// isWrappedArrayFieldRef mirrors NullableArrayUtils.isWrappedArrayDescriptor
// (String, Descriptor): the named field exists on parent, is message-typed,
// and its message is the wrapper shape (exactly one repeated field named
// "values").
func isWrappedArrayFieldRef(fieldName string, parent protoreflect.MessageDescriptor) bool {
	if parent == nil {
		return false
	}
	fd := parent.Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil || fd.Kind() != protoreflect.MessageKind || fd.IsList() {
		return false
	}
	md := fd.Message()
	if md.Fields().Len() != 1 {
		return false
	}
	values := md.Fields().ByName(protoreflect.Name(wrappedArrayFieldName))
	return values != nil && values.Cardinality() == protoreflect.Repeated
}

// splitFieldIntoNestedWithValues mirrors Java: field(name, fanType) becomes
// nesting(parent=field(name, SCALAR), child=field("values", fanType)), the
// null interpretation copied onto both halves.
func splitFieldIntoNestedWithValues(original *gen.Field) *gen.Nesting {
	ni := original.GetNullInterpretation()
	return &gen.Nesting{
		Parent: &gen.Field{
			FieldName:          original.FieldName,
			FanType:            gen.Field_SCALAR.Enum(),
			NullInterpretation: ni.Enum(),
		},
		Child: &gen.KeyExpression{Field: &gen.Field{
			FieldName:          proto.String(wrappedArrayFieldName),
			FanType:            original.GetFanType().Enum(),
			NullInterpretation: ni.Enum(),
		}},
	}
}

// proto2Clone preserves sibling fields (grouping count, split size,
// function name) in the arms that replace one child expression.
func proto2Clone(ke *gen.KeyExpression) *gen.KeyExpression {
	return proto.Clone(ke).(*gen.KeyExpression)
}
