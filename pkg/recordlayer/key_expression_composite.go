// Portions derived from FoundationDB Record Layer (FunctionKeyExpression.java,
// KeyExpression.java, FieldKeyExpression.java, NestingKeyExpression.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CompositeKeyExpression combines multiple key expressions
type CompositeKeyExpression struct {
	expressions []KeyExpression
	// arityFaultSeq is nonzero when Java's constructor would have thrown
	// "Then must have at least 2 children" building this Then or a Then it
	// flattened: the program-order place of that throw (nextBuildFaultSeq),
	// which Build reports as a builder fault (keyConstructionFault).
	arityFaultSeq uint64
}

// EvaluateFlat returns the single concatenated tuple directly, avoiding the
// outer [][]any wrapper. Only valid when no child uses fan-out.
// Saves 1 alloc per child (no [][]any) + 1 alloc for the result (no outer wrapper).
func (c *CompositeKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	result := make([]any, 0, len(c.expressions))
	for _, expr := range c.expressions {
		// Try scalar first (zero alloc per value)
		if se, ok := expr.(scalarEvaluator); ok {
			val, err := se.EvaluateScalar(record, msg)
			if err == nil {
				result = append(result, val)
				continue
			}
		}
		if fe, ok := expr.(flatEvaluator); ok {
			childFlat, err := fe.EvaluateFlat(record, msg)
			if err == nil {
				result = append(result, childFlat...)
				continue
			}
		}
		childTuples, err := expr.Evaluate(record, msg)
		if err != nil {
			return nil, err
		}
		if len(childTuples) != 1 {
			return nil, fmt.Errorf("EvaluateFlat: child produced %d tuples, expected 1", len(childTuples))
		}
		result = append(result, childTuples[0]...)
	}
	return result, nil
}

// PackDirect encodes all child fields directly into a Packer.
// Returns false if any child can't be directly packed.
func (c *CompositeKeyExpression) PackDirect(pk *tuple.Packer, record *FDBStoredRecord[proto.Message], msg proto.Message) bool {
	for _, expr := range c.expressions {
		if dp, ok := expr.(directPacker); ok {
			if !dp.PackDirect(pk, record, msg) {
				return false
			}
			continue
		}
		return false
	}
	return true
}

// Concat creates a composite key from multiple expressions. It is Java's
// ThenKeyExpression constructor: a child that is itself a composite
// contributes its children, not itself (ThenKeyExpression.add,
// ThenKeyExpression.java:264-271), so no composite ever holds a composite and
// every Then Go writes has the flat shape Java writes.
//
// Java's list constructor refuses a Then of fewer than two children with
// RecordCoreException "Then must have at least 2 children"
// (ThenKeyExpression.java:63-65), at the call. Concat has no error channel, so
// it records the place of that throw in program order, and Build returns it as
// the first builder fault when the Then reaches the builder (keyConstructionFault). A
// Then built from one that was refused keeps the earlier place, as the Java
// program would have died there.
func Concat(exprs ...KeyExpression) KeyExpression {
	flat := exprs
	flattened := false
	var fault uint64
	for i, e := range exprs {
		if then, ok := e.(*CompositeKeyExpression); ok {
			if !flattened {
				flat, flattened = flattenThenChildren(exprs, i), true
			}
			fault = earlierFault(fault, then.arityFaultSeq)
		}
	}
	if len(flat) < 2 {
		fault = earlierFault(fault, nextBuildFaultSeq())
	}
	return &CompositeKeyExpression{expressions: flat, arityFaultSeq: fault}
}

// earlierFault is the earlier of two fault places, where zero is none.
func earlierFault(a, b uint64) uint64 {
	if a == 0 || (b != 0 && b < a) {
		return b
	}
	return a
}

// flattenThenChildren is exprs with each composite replaced by its children,
// exprs[:first] known to hold none.
func flattenThenChildren(exprs []KeyExpression, first int) []KeyExpression {
	flat := make([]KeyExpression, first, len(exprs)+2)
	copy(flat, exprs[:first])
	for _, e := range exprs[first:] {
		if then, ok := e.(*CompositeKeyExpression); ok {
			flat = append(flat, then.expressions...)
			continue
		}
		flat = append(flat, e)
	}
	return flat
}

// Evaluate computes the Cartesian product of all child expression results.
// Matches Java's ThenKeyExpression which crosses children:
//
//	child0 returns [[1], [2]]
//	child1 returns [[a], [b]]
//	result = [[1,a], [1,b], [2,a], [2,b]]
//
// For the common case where each child returns exactly one tuple, the result
// is a single tuple that is the concatenation of all child tuples — identical
// to the old flat-append behavior.
func (c *CompositeKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	// Each child is evaluated once. A child with one tuple contributes a
	// segment of flat (through the scalar and flat evaluators where it has
	// them, which allocate least); a child that can fan out goes straight to
	// Evaluate, and its tuples are kept for the cross product.
	flat := make([]any, 0, len(c.expressions))
	var parts []compositePart // set once a child yields other than one tuple
	for i, expr := range c.expressions {
		start := len(flat)
		if !createsDuplicatesRec(expr, true) {
			if se, ok := expr.(scalarEvaluator); ok {
				if val, err := se.EvaluateScalar(record, msg); err == nil {
					flat = append(flat, val)
					parts = appendFlatPart(parts, start, len(flat))
					continue
				}
				// Fall through on error (e.g. repeated field)
			}
			if fe, ok := expr.(flatEvaluator); ok {
				if childFlat, err := fe.EvaluateFlat(record, msg); err == nil {
					flat = append(flat, childFlat...)
					parts = appendFlatPart(parts, start, len(flat))
					continue
				}
			}
		}
		childTuples, err := expr.Evaluate(record, msg)
		if err != nil {
			return nil, err
		}
		if len(childTuples) == 1 {
			flat = append(flat, childTuples[0]...)
			parts = appendFlatPart(parts, start, len(flat))
			continue
		}
		if parts == nil {
			// The children so far are one flat segment.
			parts = make([]compositePart, 0, len(c.expressions))
			if i > 0 {
				parts = append(parts, compositePart{lo: 0, hi: start})
			}
		}
		parts = append(parts, compositePart{fan: true, tuples: childTuples})
	}
	if parts == nil {
		return [][]any{flat}, nil
	}
	return crossCompositeParts(parts, flat), nil
}

// compositePart is one or more children's contribution to a composite's
// tuples: flat[lo:hi], or, for a fan-out child (fan), one of tuples, of
// which there may be none.
type compositePart struct {
	lo, hi int
	fan    bool
	tuples [][]any
}

// appendFlatPart records flat[lo:hi] once parts is in use (a fan-out child
// was seen); before that the whole prefix is one implicit segment.
func appendFlatPart(parts []compositePart, lo, hi int) []compositePart {
	if parts == nil {
		return nil
	}
	return append(parts, compositePart{lo: lo, hi: hi})
}

// crossCompositeParts is the cross product of parts in child order, the
// earlier part most significant, as the composite's tuples have always been
// ordered. Each tuple's capacity is its length, so a caller appending to one
// cannot write into the next.
func crossCompositeParts(parts []compositePart, flat []any) [][]any {
	count := 1
	for _, p := range parts {
		if p.fan {
			count *= len(p.tuples)
		}
	}
	if count == 0 {
		return nil
	}
	result := make([][]any, 0, count)
	idx := make([]int, len(parts))
	for {
		width := 0
		for i, p := range parts {
			if p.fan {
				width += len(p.tuples[idx[i]])
			} else {
				width += p.hi - p.lo
			}
		}
		t := make([]any, 0, width)
		for i, p := range parts {
			if p.fan {
				t = append(t, p.tuples[idx[i]]...)
			} else {
				t = append(t, flat[p.lo:p.hi]...)
			}
		}
		result = append(result, t)
		// Advance the least significant fan-out part.
		i := len(parts) - 1
		for ; i >= 0; i-- {
			if !parts[i].fan {
				continue
			}
			idx[i]++
			if idx[i] < len(parts[i].tuples) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			return result
		}
	}
}

// FieldNames returns all field names from component expressions
func (c *CompositeKeyExpression) FieldNames() []string {
	var names []string
	for _, expr := range c.expressions {
		names = append(names, expr.FieldNames()...)
	}
	return names
}

// SubKeyExpressions returns the child key expressions in key order. Mirrors
// Java's ThenKeyExpression.getChildren(); used by callers that need to inspect
// the composite's structure (e.g. tagging which columns are function-keyed).
// The returned slice is the live backing slice — callers must not mutate it.
func (c *CompositeKeyExpression) SubKeyExpressions() []KeyExpression {
	return c.expressions
}

// ColumnSize returns the sum of all child column sizes.
func (c *CompositeKeyExpression) ColumnSize() int {
	total := 0
	for _, child := range c.expressions {
		total += child.ColumnSize()
	}
	return total
}

// NestingKeyExpression navigates into a nested protobuf message and evaluates
// a child expression against it. Matches Java's NestingKeyExpression.
//
// Example: Nest("flower", Field("type")) indexes the "type" field inside
// a nested "flower" message.
//
// For repeated message fields, use NestFanOut to fan out over each sub-message.
type NestingKeyExpression struct {
	parentField string
	fanType     FanType
	child       KeyExpression
	// parentNullStandin is the parent field's NullStandin (see
	// FieldKeyExpression): under NOT_NULL an absent parent message evaluates
	// the child over the parent's default (empty) message, not over null.
	parentNullStandin NullStandin
}

// Nest creates a nesting expression: navigate into a message field and evaluate
// the child expression on the sub-message. Matches Java's field("parent").nest("child").
func Nest(parentField string, child KeyExpression) KeyExpression {
	return &NestingKeyExpression{parentField: parentField, fanType: FanTypeNone, child: child}
}

// NestFanOut creates a nesting expression for a repeated message field.
// Each element of the repeated field is evaluated with the child expression,
// producing fan-out. Matches Java's field("parent", FanType.FanOut).nest(child).
func NestFanOut(parentField string, child KeyExpression) KeyExpression {
	return &NestingKeyExpression{parentField: parentField, fanType: FanTypeFanOut, child: child}
}

// Evaluate navigates into the parent message field and evaluates the child
// expression on the sub-message(s).
func (n *NestingKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if msg == nil {
		// Nil message → depends on FanType, matching Java's parent.evaluateMessage(null).
		// FanOut parent on nil → empty (no repeated field to iterate).
		// Non-FanOut parent on nil → child evaluates on nil sub-message.
		if n.fanType == FanTypeFanOut {
			return nil, nil
		}
		return n.child.Evaluate(record, nil)
	}
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(n.parentField))
	if fd == nil {
		// Same deliberate divergence as FieldKeyExpression.resolveFieldDescriptor:
		// Java's NestingKeyExpression.evaluateMessage delegates the parent step
		// to FieldKeyExpression.evaluateMessage, so an absent parent field
		// yields getNullResult() there and the child evaluates on a null
		// submessage. Go refuses instead, for the reason given at that helper.
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s not found in message", n.parentField)}
	}
	if !isMessageField(fd) {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s is not a message type, cannot nest", n.parentField)}
	}

	if fd.IsList() {
		return n.evaluateRepeated(record, m, fd)
	}
	if fd.IsMap() {
		return n.evaluateMap(record, m, fd)
	}

	// Scalar message field — get the sub-message and evaluate child on it.
	// Java evaluates the parent as a field (NestingKeyExpression.evaluateMessage
	// over FieldKeyExpression.evaluateMessage): an absent parent is the null
	// standin, and the child is evaluated over a null message, unless the
	// parent's standin is NOT_NULL, which yields the parent's default message.
	if !m.Has(fd) && n.parentNullStandin != NullStandinNotNull {
		return n.child.Evaluate(record, nil)
	}
	subMsg := m.Get(fd).Message().Interface()
	return n.child.Evaluate(record, subMsg)
}

// evaluateRepeated handles repeated message fields.
func (n *NestingKeyExpression) evaluateRepeated(record *FDBStoredRecord[proto.Message], m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]any, error) {
	if n.fanType != FanTypeFanOut {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s is repeated, must use NestFanOut", n.parentField)}
	}

	list := m.Get(fd).List()
	count := list.Len()
	if count == 0 {
		return nil, nil // Empty repeated → no results
	}

	result := make([][]any, 0, count)
	// A child that cannot fan out is one tuple per element: its flat form
	// skips Evaluate's one-tuple wrapper.
	fe, flat := n.child.(flatEvaluator)
	flat = flat && !createsDuplicatesRec(n.child, true)
	for i := 0; i < count; i++ {
		subMsg := list.Get(i).Message().Interface()
		if flat {
			if t, err := fe.EvaluateFlat(record, subMsg); err == nil {
				result = append(result, t)
				continue
			}
		}
		childTuples, err := n.child.Evaluate(record, subMsg)
		if err != nil {
			return nil, err
		}
		result = append(result, childTuples...)
	}
	return result, nil
}

// evaluateMap fans out a map field's entries, as Java does: protobuf-java
// reads a map as the repeated entry messages (key = 1, value = 2) it is on the
// wire, and NestingKeyExpression evaluates the child over each, in the order
// the record's bytes hold them (record_wire_map_order.go): a record decoded
// from stored bytes in wire order, and one Go is saving in key order, which is
// the order it is written in.
func (n *NestingKeyExpression) evaluateMap(record *FDBStoredRecord[proto.Message], m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]any, error) {
	if n.fanType != FanTypeFanOut {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s is repeated, must use NestFanOut", n.parentField)}
	}
	mp := m.Get(fd).Map()
	var entries []protoreflect.Message
	inWireOrder := false
	if record != nil {
		entries, inWireOrder = record.wire.mapEntries(record.Record, m, fd)
	}
	if !inWireOrder {
		if mp.Len() == 0 {
			return nil, nil
		}
		entries = sortedMapEntries(mp, fd)
	}
	var result [][]any
	for _, entry := range entries {
		childTuples, err := n.child.Evaluate(record, entry.Interface())
		if err != nil {
			return nil, err
		}
		result = append(result, childTuples...)
	}
	return result, nil
}

// mapKeyLess orders map keys of one kind: bool, integers, or strings.
func mapKeyLess(a, b protoreflect.MapKey) bool {
	switch av := a.Interface().(type) {
	case bool:
		return !av && b.Bool()
	case string:
		return av < b.String()
	case int32, int64:
		return a.Int() < b.Int()
	default:
		return a.Uint() < b.Uint()
	}
}

// FieldNames returns the parent field name plus child field names.
func (n *NestingKeyExpression) FieldNames() []string {
	childNames := n.child.FieldNames()
	result := make([]string, 0, 1+len(childNames))
	result = append(result, n.parentField)
	result = append(result, childNames...)
	return result
}

// ColumnSize returns the child's column size — the parent message field doesn't
// contribute a tuple element.
func (n *NestingKeyExpression) ColumnSize() int {
	return n.child.ColumnSize()
}

// ParentField is the field name of Java's NestingKeyExpression.getParent().
func (n *NestingKeyExpression) ParentField() string { return n.parentField }

// FanType is the fan type of Java's NestingKeyExpression.getParent().
func (n *NestingKeyExpression) FanType() FanType { return n.fanType }

// Child is Java's NestingKeyExpression.getChild().
func (n *NestingKeyExpression) Child() KeyExpression { return n.child }
