package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// renameFields rewrites a KeyExpression's field references in response to a field
// renaming, mapping each referenced field by NUMBER from sourceDesc to targetDesc — a
// rename is the same field number carrying a new name. It is used by
// MetaDataEvolutionValidator to compute the expected primary-key / index key expression
// across an allowed rename before comparing it to the new metadata's expression.
//
// This is the Go port of Java's
// com.apple.foundationdb.record.metadata.expressions.visitors.RenameFieldsVisitor.
// Java implements it as a KeyExpressionVisitor with a descriptor state stack; Go has no
// KeyExpressionVisitor infrastructure and already walks key expressions with inline
// type-switch recursion (e.g. metadata.go's bindRecordTypeKeyExpressions), so this is a
// recursive function with identical semantics rather than a visitor object. Field and
// Nesting are the only nodes a rename rewrites; container nodes recurse; Literal /
// Version / Empty / a childless RecordType are rename-invariant; anything else is an
// error (matching Java's "field renaming not supported for expression").
//
// On no-op subtrees it returns the original expression unchanged (Java's identity
// short-circuit), so an unrenamed expression compares proto-equal to itself.
func renameFields(expr KeyExpression, sourceDesc, targetDesc protoreflect.MessageDescriptor) (KeyExpression, error) {
	if sourceDesc == targetDesc {
		return expr, nil
	}
	switch e := expr.(type) {
	case *FieldKeyExpression:
		newName, err := renameFieldByNumber(e.fieldName, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newName == e.fieldName {
			return e, nil
		}
		return &FieldKeyExpression{fieldName: newName, fanType: e.fanType}, nil

	case *NestingKeyExpression:
		newParent, err := renameFieldByNumber(e.parentField, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		// The child applies to the parent field's message type, so re-derive the
		// source/target descriptors for the recursion (Java getMessageTypeForField).
		childSource, err := messageTypeForField(sourceDesc, e.parentField)
		if err != nil {
			return nil, err
		}
		childTarget, err := messageTypeForField(targetDesc, newParent)
		if err != nil {
			return nil, err
		}
		var newChild KeyExpression
		if childSource == childTarget {
			// Same descriptor object on both sides → no renaming reachable below; skip
			// the recursion (Java's childSource == childTarget short-circuit). Across
			// two independently-built metadata descriptors this rarely fires, but when
			// it does it is always safe (identical descriptors carry identical names).
			newChild = e.child
		} else {
			newChild, err = renameFields(e.child, childSource, childTarget)
			if err != nil {
				return nil, err
			}
		}
		if newParent == e.parentField && newChild == e.child {
			return e, nil
		}
		return &NestingKeyExpression{parentField: newParent, fanType: e.fanType, child: newChild}, nil

	case *CompositeKeyExpression:
		newChildren, changed, err := renameChildren(e.expressions, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if !changed {
			return e, nil
		}
		return &CompositeKeyExpression{expressions: newChildren}, nil

	case *ListKeyExpression:
		newChildren, changed, err := renameChildren(e.children, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if !changed {
			return e, nil
		}
		return &ListKeyExpression{children: newChildren}, nil

	case *GroupingKeyExpression:
		newWhole, err := renameFields(e.wholeKey, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newWhole == e.wholeKey {
			return e, nil
		}
		return &GroupingKeyExpression{wholeKey: newWhole, groupedCount: e.groupedCount}, nil

	case *FunctionKeyExpression:
		newArgs, err := renameFields(e.arguments, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newArgs == e.arguments {
			return e, nil
		}
		return &FunctionKeyExpression{name: e.name, arguments: newArgs}, nil

	case *SplitKeyExpression:
		newJoined, err := renameFields(e.joined, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newJoined == e.joined {
			return e, nil
		}
		return &SplitKeyExpression{joined: newJoined, splitSize: e.splitSize}, nil

	case *DimensionsKeyExpression:
		newWhole, err := renameFields(e.WholeKey, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newWhole == e.WholeKey {
			return e, nil
		}
		return &DimensionsKeyExpression{WholeKey: newWhole, PrefixSize: e.PrefixSize, DimensionsSize: e.DimensionsSize}, nil

	case *KeyWithValueExpression:
		newInner, err := renameFields(e.innerKey, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newInner == e.innerKey {
			return e, nil
		}
		return &KeyWithValueExpression{innerKey: newInner, splitPoint: e.splitPoint}, nil

	case *RecordTypeKeyExpression:
		// The record-type prefix itself is rename-invariant. Go (unlike Java) allows an
		// optional nested expression after the prefix; it applies to the same record
		// descriptor, so recurse into it with the same source/target.
		if e.nested == nil {
			return e, nil
		}
		newNested, err := renameFields(e.nested, sourceDesc, targetDesc)
		if err != nil {
			return nil, err
		}
		if newNested == e.nested {
			return e, nil
		}
		return &RecordTypeKeyExpression{nested: newNested, typeKeys: e.typeKeys}, nil

	case *VersionKeyExpression, *LiteralKeyExpression, *EmptyKeyExpression:
		// Invariant to field renamings (Java treats these as KeyExpressionWithValue /
		// EmptyKeyExpression that are returned as-is).
		return expr, nil

	default:
		return nil, &MetaDataEvolutionError{
			Message: fmt.Sprintf("field renaming not supported for expression of type %T", expr),
		}
	}
}

// renameChildren rewrites a slice of child expressions, returning the new slice and
// whether anything changed (so the caller can preserve the original on a no-op).
func renameChildren(children []KeyExpression, sourceDesc, targetDesc protoreflect.MessageDescriptor) ([]KeyExpression, bool, error) {
	out := make([]KeyExpression, len(children))
	changed := false
	for i, child := range children {
		renamed, err := renameFields(child, sourceDesc, targetDesc)
		if err != nil {
			return nil, false, err
		}
		out[i] = renamed
		if renamed != child {
			changed = true
		}
	}
	return out, changed, nil
}

// renameFieldByNumber maps a field name from sourceDesc to its equivalent name in
// targetDesc by matching field number. Java RenameFieldsVisitor.RenameFieldsState.renameField.
func renameFieldByNumber(sourceFieldName string, sourceDesc, targetDesc protoreflect.MessageDescriptor) (string, error) {
	sourceField := sourceDesc.Fields().ByName(protoreflect.Name(sourceFieldName))
	if sourceField == nil {
		return "", &MetaDataEvolutionError{
			Message: fmt.Sprintf("field %q not found in source descriptor %q", sourceFieldName, sourceDesc.FullName()),
		}
	}
	targetField := targetDesc.Fields().ByNumber(sourceField.Number())
	if targetField == nil {
		return "", &MetaDataEvolutionError{
			Message: fmt.Sprintf("field %q (number %d) not found in target descriptor %q",
				sourceFieldName, sourceField.Number(), targetDesc.FullName()),
		}
	}
	return string(targetField.Name()), nil
}

// messageTypeForField returns the message descriptor of a (message-typed) parent field,
// for re-deriving the descriptor pair when recursing into a nesting. Java
// RenameFieldsVisitor.getMessageTypeForField.
func messageTypeForField(desc protoreflect.MessageDescriptor, fieldName string) (protoreflect.MessageDescriptor, error) {
	fd := desc.Fields().ByName(protoreflect.Name(fieldName))
	if fd == nil {
		return nil, &MetaDataEvolutionError{
			Message: fmt.Sprintf("nesting parent field %q not found in %q", fieldName, desc.FullName()),
		}
	}
	if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
		return nil, &MetaDataEvolutionError{
			Message: fmt.Sprintf("nesting parent field %q in %q is not of message type", fieldName, desc.FullName()),
		}
	}
	return fd.Message(), nil
}
