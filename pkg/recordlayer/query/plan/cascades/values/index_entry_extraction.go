// Portions derived from FoundationDB Record Layer (
// IndexEntryToRecordValueHelper.java, Value.java,
// ExtractFromIndexKeyValueRuleSet.java),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package values

// ExtractFromIndexEntry is Java's Value.extractFromIndexEntryMaybe: the
// ExtractFromIndexKeyValueRuleSet computed over an index entry column's Value v.
// It answers the base field the column holds and the reader that recovers that
// field from the entry bound under the current correlation. A column that is no
// field of the base (an arithmetic function, a field of an exploded element)
// has no extraction, nor does one whose type no entry leaf can carry.
func ExtractFromIndexEntry(v Value, baseAlias CorrelationIdentifier, source TupleSource, ordinalPath []int) (FieldValue, Value, bool) {
	if v == nil {
		return nil, nil, false
	}
	field, compensate, ok := matchIndexEntryField(v, baseAlias)
	if !ok {
		return nil, nil, false
	}
	leaf, err := NewIndexEntryObjectValue(CurrentCorrelation(), source, ordinalPath, v.Type())
	if err != nil {
		return nil, nil, false
	}
	return field, compensate(leaf), true
}

func identityCompensation(reader Value) Value { return reader }

// matchIndexEntryField runs the rules bottom-up.
func matchIndexEntryField(v Value, baseAlias CorrelationIdentifier) (FieldValue, func(Value) Value, bool) {
	if to, ok := v.(*ToOrderedBytesValue); ok {
		// CompensateToOrderedBytesValueRule: the child's field, read back
		// through the inverse.
		if to.Child == nil {
			return nil, nil, false
		}
		field, compensate, ok := matchIndexEntryField(to.Child, baseAlias)
		if !ok {
			return nil, nil, false
		}
		childType := to.Child.Type()
		return field, func(reader Value) Value {
			return compensate(to.CreateInverse(reader, childType))
		}, true
	}
	// MatchSimpleFieldValueRule. An admitted Go FieldValue is always one path
	// on its quantifier (ResolveFieldAccess fuses), so the target's
	// MatchFieldValueOverFieldValueRule has nothing left to fuse. A ROW_VERSION
	// field is refused: the queried record has no field for the version.
	fv, ok := AsFieldValue(v)
	if !ok || fv.Type() == nil || fv.Type().Code() == TypeCodeVersion {
		return nil, nil, false
	}
	qov, ok := AsQuantifiedObjectValue(fv.ChildValue())
	if !ok || qov.Correlation() != baseAlias {
		return nil, nil, false
	}
	return fv, identityCompensation, true
}

// IndexEntryRecordReader is Java's IndexEntryToRecordValueHelper: a trie over
// the queried record's field path, each node holding the reader of the field
// it covers. Go keys a node by the field's ordinal in its record type, the
// identity its FieldValues carry; Java keys by name, unique in a record.
type IndexEntryRecordReader struct {
	value    Value
	children map[int]*IndexEntryRecordReader
}

// Cover makes v this field's reader unless an earlier source covers it
// already: the first covering entry column wins.
func (n *IndexEntryRecordReader) Cover(v Value) {
	if n.value == nil {
		n.value = v
	}
}

// WithChild is the node of the field at ordinal, created on first use.
func (n *IndexEntryRecordReader) WithChild(ordinal int) *IndexEntryRecordReader {
	if n.children == nil {
		n.children = map[int]*IndexEntryRecordReader{}
	}
	child, ok := n.children[ordinal]
	if !ok {
		child = &IndexEntryRecordReader{}
		n.children[ordinal] = child
	}
	return child
}

// Covers reports whether any field is covered below this node.
func (n *IndexEntryRecordReader) Covers() bool { return len(n.children) > 0 }

// CoverField records the reader of field, descending a node per step of its
// path (ScanWithFetchMatchCandidate.recordCoveredField).
func (n *IndexEntryRecordReader) CoverField(field FieldValue, reader Value) bool {
	path := field.Path()
	if path == nil || path.Len() == 0 {
		return false
	}
	node := n
	for i := 0; i < path.Len(); i++ {
		accessor, ok := path.Accessor(i)
		if !ok {
			return false
		}
		node = node.WithChild(accessor.Ordinal())
	}
	node.Cover(reader)
	return true
}

// ToRecordValue builds the record of target from the covered fields, in
// target's field order; a field nothing covers is absent.
func (n *IndexEntryRecordReader) ToRecordValue(target *RecordType) *RecordConstructorValue {
	fields := make([]RecordConstructorField, len(target.Fields))
	for i, field := range target.Fields {
		child := n.children[i]
		var column Value
		switch {
		case child == nil:
			column = absentIndexEntryField(field.FieldType)
		case child.value != nil:
			column = child.value
		default:
			if nested, ok := field.FieldType.(*RecordType); ok {
				column = child.ToRecordValue(nested)
			} else {
				column = absentIndexEntryField(field.FieldType)
			}
		}
		fields[i] = RecordConstructorField{Name: field.Name, Value: column}
	}
	return NewRecordConstructorValue(fields...)
}

// absentIndexEntryField is an uncovered field: a non-null array is empty,
// anything else NULL of its type.
func absentIndexEntryField(t Type) Value {
	if array, ok := t.(*ArrayType); ok && !array.Nullable {
		return NewArrayConstructorValue(array.ElementType, nil)
	}
	return NewNullValue(t)
}
