// Portions derived from FoundationDB Record Layer (NullableArrayUtils.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package metadata

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/metadatapb"
)

// Port of Java's NullableArrayUtils.wrapArray (fdb-relational-core
// com.apple.foundationdb.relational.util.NullableArrayUtils:79-219): a
// descriptor-driven post-pass over a serialized key expression that inserts
// the `values` hop wherever a referenced field is a NullableArrayWrapper
// message (e.g. reviews.rating -> reviews.values.rating). The stored index
// key expression must match the stored descriptor shape — Java writes
// nullable arrays wrapped, and so does Go's emitter — so any index over a
// nullable array column is rewritten through this pass before the template
// is built.
//
// Java's `wrapArray` is this function plus a `containsNullableArray` gate;
// Go applies that gate at the single funnel every index passes through
// (builder.go's registerIndex), so the entry point here is the ungated body.

func wrapArrayInternal(ke *metadatapb.KeyExpression, parent protoreflect.MessageDescriptor) *metadatapb.KeyExpression {
	switch {
	case ke.GetThen() != nil:
		then := &metadatapb.Then{}
		for _, child := range ke.GetThen().GetChild() {
			then.Child = append(then.Child, wrapArrayInternal(child, parent))
		}
		return &metadatapb.KeyExpression{Then: then}

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
			newChild := &metadatapb.KeyExpression{Nesting: &metadatapb.Nesting{
				Parent: wrappedParent.GetChild().GetField(),
				Child:  wrappedChild,
			}}
			return &metadatapb.KeyExpression{Nesting: &metadatapb.Nesting{
				Parent: wrappedParent.GetParent(),
				Child:  newChild,
			}}
		}
		var childDesc protoreflect.MessageDescriptor
		if fd := parent.Fields().ByName(protoreflect.Name(parentFieldName)); fd != nil && fd.Kind() == protoreflect.MessageKind {
			childDesc = fd.Message()
		}
		return &metadatapb.KeyExpression{Nesting: &metadatapb.Nesting{
			Parent: par,
			Child:  wrapArrayInternal(child, childDesc),
		}}

	case ke.GetField() != nil:
		if isWrappedArrayFieldRef(ke.GetField().GetFieldName(), parent) {
			return &metadatapb.KeyExpression{Nesting: splitFieldIntoNestedWithValues(ke.GetField())}
		}
		return ke

	case ke.GetGrouping() != nil:
		g := proto2Clone(ke).GetGrouping()
		g.WholeKey = wrapArrayInternal(g.GetWholeKey(), parent)
		return &metadatapb.KeyExpression{Grouping: g}

	case ke.GetSplit() != nil:
		s := proto2Clone(ke).GetSplit()
		s.Joined = wrapArrayInternal(s.GetJoined(), parent)
		return &metadatapb.KeyExpression{Split: s}

	case ke.GetFunction() != nil:
		f := proto2Clone(ke).GetFunction()
		f.Arguments = wrapArrayInternal(f.GetArguments(), parent)
		return &metadatapb.KeyExpression{Function: f}

	case ke.GetKeyWithValue() != nil:
		k := proto2Clone(ke).GetKeyWithValue()
		k.InnerKey = wrapArrayInternal(k.GetInnerKey(), parent)
		return &metadatapb.KeyExpression{KeyWithValue: k}

	case ke.GetList() != nil:
		l := &metadatapb.List{}
		for _, item := range ke.GetList().GetChild() {
			l.Child = append(l.Child, wrapArrayInternal(item, parent))
		}
		return &metadatapb.KeyExpression{List: l}

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
func splitFieldIntoNestedWithValues(original *metadatapb.Field) *metadatapb.Nesting {
	ni := original.GetNullInterpretation()
	return &metadatapb.Nesting{
		Parent: &metadatapb.Field{
			FieldName:          original.FieldName,
			FanType:            metadatapb.Field_SCALAR.Enum(),
			NullInterpretation: ni.Enum(),
		},
		Child: &metadatapb.KeyExpression{Field: &metadatapb.Field{
			FieldName:          proto.String(wrappedArrayFieldName),
			FanType:            original.GetFanType().Enum(),
			NullInterpretation: ni.Enum(),
		}},
	}
}

// proto2Clone preserves sibling fields (grouping count, split size,
// function name) in the arms that replace one child expression.
func proto2Clone(ke *metadatapb.KeyExpression) *metadatapb.KeyExpression {
	return proto.Clone(ke).(*metadatapb.KeyExpression)
}
