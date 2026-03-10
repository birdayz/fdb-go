package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FanType controls how repeated (list) proto fields are handled in key expressions.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyExpression.FanType.
type FanType int

const (
	// FanTypeNone means the field must not be repeated. This is the default.
	FanTypeNone FanType = iota
	// FanTypeFanOut produces one tuple per repeated value — used for multi-entry indexes.
	FanTypeFanOut
	// FanTypeConcatenate puts all repeated values into a single tuple element.
	FanTypeConcatenate
)

// FieldKeyExpression extracts a single field value from a record for use as a key component.
// For repeated fields, use FanOut() or Concatenate() to control multi-value handling.
type FieldKeyExpression struct {
	fieldName string
	fanType   FanType
}

// Field creates a key expression that extracts a single (non-repeated) field.
func Field(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeNone}
}

// FanOut creates a key expression for a repeated field that produces one index entry
// per repeated value. Matches Java's Key.Expressions.field("name", FanType.FanOut).
func FanOut(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeFanOut}
}

// Evaluate extracts the field value(s) from the message.
// For non-repeated fields, returns one tuple with the single value.
// For repeated fields with FanOut, returns one tuple per value.
// For repeated fields with Concatenate, returns one tuple containing a nested tuple of all values.
func (f *FieldKeyExpression) Evaluate(msg proto.Message) ([][]any, error) {
	if msg == nil {
		// Nil message → result depends on FanType, matching Java's getNullResult().
		return f.getNullResult(), nil
	}
	m := msg.ProtoReflect()

	fd := m.Descriptor().Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return nil, fmt.Errorf("field %s not found in message", f.fieldName)
	}

	if fd.IsList() {
		return f.evaluateRepeated(m, fd)
	}

	// Scalar field — check proto field presence before reading value.
	// For proto2 optional fields, unset → nil (matching Java's hasField() check).
	// For proto3 fields (no presence), always returns the value.
	if fd.HasPresence() && !m.Has(fd) {
		return [][]any{{nil}}, nil
	}
	value := m.Get(fd)
	result, err := scalarToInterface(fd, value)
	if err != nil {
		return nil, err
	}
	return [][]any{{result}}, nil
}

// getNullResult returns the appropriate result for a nil message based on FanType.
// Matches Java's FieldKeyExpression.getNullResult():
//   - FanOut → empty (no index entries)
//   - Concatenate → [[emptyList]]
//   - None → [[nil]]
func (f *FieldKeyExpression) getNullResult() [][]any {
	switch f.fanType {
	case FanTypeFanOut:
		return nil // No entries — matching Java's Collections.emptyList()
	case FanTypeConcatenate:
		return [][]any{{[]any{}}} // One entry containing an empty list
	default:
		return [][]any{{nil}} // One entry with null
	}
}

// evaluateRepeated handles repeated proto fields according to FanType.
func (f *FieldKeyExpression) evaluateRepeated(m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]any, error) {
	list := m.Get(fd).List()
	count := list.Len()

	switch f.fanType {
	case FanTypeFanOut:
		if count == 0 {
			return nil, nil // Empty list → no index entries (matches Java)
		}
		result := make([][]any, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			result[i] = []any{val}
		}
		return result, nil

	case FanTypeConcatenate:
		values := make([]any, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			values[i] = val
		}
		// All values packed into a single tuple element (as a nested tuple)
		return [][]any{{values}}, nil

	default: // FanTypeNone
		return nil, fmt.Errorf("field %s is repeated with FanType.None", f.fieldName)
	}
}

// scalarToInterface converts a protoreflect.Value to a Go any suitable for FDB tuples.
// All integer types → int64, all floats → float64 (FDB tuple layer constraint).
func scalarToInterface(fd protoreflect.FieldDescriptor, value protoreflect.Value) (any, error) {
	kind := fd.Kind()
	// For list elements, get the element kind from the field descriptor
	if fd.IsList() {
		kind = fd.Kind()
	}
	switch kind {
	case protoreflect.StringKind:
		return value.String(), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return value.Int(), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(value.Uint()), nil
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return value.Float(), nil
	case protoreflect.BoolKind:
		return value.Bool(), nil
	case protoreflect.BytesKind:
		return value.Bytes(), nil
	case protoreflect.EnumKind:
		return int64(value.Enum()), nil
	default:
		return nil, fmt.Errorf("unsupported field type %s for key expression", kind)
	}
}

// FieldNames returns the field name accessed by this expression
func (f *FieldKeyExpression) FieldNames() []string {
	return []string{f.fieldName}
}

// RecordTypeKeyExpression represents the special record type key prefix.
// Matches Java's RecordTypeKeyExpression: evaluates to the record type key
// (an integer derived from the union descriptor field number).
type RecordTypeKeyExpression struct {
	// nested is the optional nested key expression
	nested KeyExpression
	// typeKeys maps proto message full name → record type key (int64).
	// Populated by metadata builder. Matches Java's record.getRecordType().getRecordTypeKey().
	typeKeys map[string]int64
}

// RecordTypeKey creates a key expression that prefixes with the record type
func RecordTypeKey() *RecordTypeKeyExpression {
	return &RecordTypeKeyExpression{}
}

// Nest adds a nested key expression after the record type prefix
func (r *RecordTypeKeyExpression) Nest(expr KeyExpression) KeyExpression {
	r.nested = expr
	return r
}

// bindTypeKeys populates the type key lookup map. Called by metadata builder.
func (r *RecordTypeKeyExpression) bindTypeKeys(typeKeys map[string]int64) {
	r.typeKeys = typeKeys
}

// Evaluate returns the record type key (integer), optionally followed by nested values.
// Matches Java's RecordTypeKeyExpression.evaluateMessage() which returns
// record.getRecordType().getRecordTypeKey() — the union descriptor field number.
func (r *RecordTypeKeyExpression) Evaluate(msg proto.Message) ([][]any, error) {
	if msg == nil {
		// Nil message → null type key. Matches Java's null check:
		// record != null ? scalar(record.getRecordType().getRecordTypeKey()) : Key.Evaluated.NULL
		return [][]any{{nil}}, nil
	}
	typeName := string(msg.ProtoReflect().Descriptor().Name())

	// Look up the integer record type key (proto field number from union descriptor).
	var typeKey any
	if r.typeKeys != nil {
		if k, ok := r.typeKeys[typeName]; ok {
			typeKey = k
		} else {
			typeKey = typeName // fallback for unbound expressions
		}
	} else {
		typeKey = typeName // fallback for unbound expressions
	}

	if r.nested == nil {
		return [][]any{{typeKey}}, nil
	}

	nestedTuples, err := r.nested.Evaluate(msg)
	if err != nil {
		return nil, err
	}

	result := make([][]any, len(nestedTuples))
	for i, nt := range nestedTuples {
		combined := make([]any, 0, 1+len(nt))
		combined = append(combined, typeKey)
		combined = append(combined, nt...)
		result[i] = combined
	}
	return result, nil
}

// FieldNames returns the field names accessed by nested expression
func (r *RecordTypeKeyExpression) FieldNames() []string {
	if r.nested != nil {
		return r.nested.FieldNames()
	}
	return []string{}
}

// IsRecordTypeExpression checks if a key expression starts with record type
func IsRecordTypeExpression(expr KeyExpression) bool {
	_, ok := expr.(*RecordTypeKeyExpression)
	return ok
}

// GetNestedExpression returns the nested expression of a RecordTypeKeyExpression
func GetNestedExpression(expr KeyExpression) KeyExpression {
	if rt, ok := expr.(*RecordTypeKeyExpression); ok {
		return rt.nested
	}
	return nil
}

// EmptyKeyExpression produces an empty tuple — used for ungrouped record counting.
// When used as a recordCountKey, a single total count is maintained.
type EmptyKeyExpression struct{}

// EmptyKey creates a key expression that produces an empty tuple.
func EmptyKey() KeyExpression {
	return &EmptyKeyExpression{}
}

// Evaluate returns one empty tuple (no key components).
func (e *EmptyKeyExpression) Evaluate(_ proto.Message) ([][]any, error) {
	return [][]any{{}}, nil
}

// FieldNames returns no field names.
func (e *EmptyKeyExpression) FieldNames() []string {
	return nil
}

// CompositeKeyExpression combines multiple key expressions
type CompositeKeyExpression struct {
	expressions []KeyExpression
}

// Concat creates a composite key from multiple expressions
func Concat(exprs ...KeyExpression) KeyExpression {
	return &CompositeKeyExpression{expressions: exprs}
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
func (c *CompositeKeyExpression) Evaluate(msg proto.Message) ([][]any, error) {
	// Start with a single empty tuple
	result := [][]any{{}}

	for _, expr := range c.expressions {
		childTuples, err := expr.Evaluate(msg)
		if err != nil {
			return nil, err
		}

		// Cross-product: for each existing tuple, combine with each child tuple
		var crossed [][]any
		for _, existing := range result {
			for _, child := range childTuples {
				combined := make([]any, 0, len(existing)+len(child))
				combined = append(combined, existing...)
				combined = append(combined, child...)
				crossed = append(crossed, combined)
			}
		}
		result = crossed
	}

	return result, nil
}

// FieldNames returns all field names from component expressions
func (c *CompositeKeyExpression) FieldNames() []string {
	var names []string
	for _, expr := range c.expressions {
		names = append(names, expr.FieldNames()...)
	}
	return names
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
func (n *NestingKeyExpression) Evaluate(msg proto.Message) ([][]any, error) {
	if msg == nil {
		// Nil message → depends on FanType, matching Java's parent.evaluateMessage(null).
		// FanOut parent on nil → empty (no repeated field to iterate).
		// Non-FanOut parent on nil → child evaluates on nil sub-message.
		if n.fanType == FanTypeFanOut {
			return nil, nil
		}
		return n.child.Evaluate(nil)
	}
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(n.parentField))
	if fd == nil {
		return nil, fmt.Errorf("field %s not found in message", n.parentField)
	}
	if fd.Kind() != protoreflect.MessageKind {
		return nil, fmt.Errorf("field %s is not a message type, cannot nest", n.parentField)
	}

	if fd.IsList() {
		return n.evaluateRepeated(m, fd)
	}

	// Scalar message field — get the sub-message and evaluate child on it.
	if !m.Has(fd) {
		// Unset message field → evaluate child on nil (returns null-like results).
		// Match Java: evaluates child on null message → returns null key components.
		return n.child.Evaluate(nil)
	}
	subMsg := m.Get(fd).Message().Interface()
	return n.child.Evaluate(subMsg)
}

// evaluateRepeated handles repeated message fields.
func (n *NestingKeyExpression) evaluateRepeated(m protoreflect.Message, fd protoreflect.FieldDescriptor) ([][]any, error) {
	if n.fanType != FanTypeFanOut {
		return nil, fmt.Errorf("field %s is repeated, must use NestFanOut", n.parentField)
	}

	list := m.Get(fd).List()
	count := list.Len()
	if count == 0 {
		return nil, nil // Empty repeated → no results
	}

	var result [][]any
	for i := 0; i < count; i++ {
		subMsg := list.Get(i).Message().Interface()
		childTuples, err := n.child.Evaluate(subMsg)
		if err != nil {
			return nil, err
		}
		result = append(result, childTuples...)
	}
	return result, nil
}

// FieldNames returns the parent field name plus child field names.
func (n *NestingKeyExpression) FieldNames() []string {
	childNames := n.child.FieldNames()
	result := make([]string, 0, 1+len(childNames))
	result = append(result, n.parentField)
	result = append(result, childNames...)
	return result
}

// createsDuplicates returns true if a key expression can produce multiple tuples
// for a single record (e.g., FanOut on a repeated field). Matches Java's
// KeyExpression.createsDuplicates() — used to validate primary keys don't fan out.
func createsDuplicates(expr KeyExpression) bool {
	switch e := expr.(type) {
	case *FieldKeyExpression:
		return e.fanType == FanTypeFanOut
	case *NestingKeyExpression:
		return e.fanType == FanTypeFanOut || createsDuplicates(e.child)
	case *CompositeKeyExpression:
		for _, child := range e.expressions {
			if createsDuplicates(child) {
				return true
			}
		}
		return false
	case *RecordTypeKeyExpression:
		if e.nested != nil {
			return createsDuplicates(e.nested)
		}
		return false
	case *KeyWithValueExpression:
		return createsDuplicates(e.innerKey)
	default:
		return false
	}
}

// normalizeKeyForPositions flattens a key expression into a list of atomic
// components for position matching. CompositeKeyExpression is flattened
// recursively; NestingKeyExpression re-wraps each child; all others return
// themselves as a single-element list.
// Matches Java's KeyExpression.normalizeKeyForPositions().
func normalizeKeyForPositions(expr KeyExpression) []KeyExpression {
	switch e := expr.(type) {
	case *CompositeKeyExpression:
		var result []KeyExpression
		for _, child := range e.expressions {
			result = append(result, normalizeKeyForPositions(child)...)
		}
		return result
	case *NestingKeyExpression:
		childNorms := normalizeKeyForPositions(e.child)
		result := make([]KeyExpression, len(childNorms))
		for i, cn := range childNorms {
			result[i] = &NestingKeyExpression{
				parentField: e.parentField,
				fanType:     e.fanType,
				child:       cn,
			}
		}
		return result
	case *KeyWithValueExpression:
		// Delegates to full inner key, matching Java's KeyWithValueExpression.normalizeKeyForPositions().
		// NOTE: PK components must NOT be placed in the value portion (positions >= splitPoint).
		// Doing so causes IndexOutOfBoundsException in Java and similar issues in Go, because
		// getEntryPrimaryKey reads from the FDB key which only has splitPoint columns.
		return normalizeKeyForPositions(e.innerKey)
	default:
		return []KeyExpression{expr}
	}
}

// keyExpressionEquals returns true if two key expressions are structurally
// identical. Used by buildPrimaryKeyComponentPositions to find overlapping
// components between index key and primary key.
// Matches Java's KeyExpression.equals() semantics.
func keyExpressionEquals(a, b KeyExpression) bool {
	switch av := a.(type) {
	case *FieldKeyExpression:
		bv, ok := b.(*FieldKeyExpression)
		return ok && av.fieldName == bv.fieldName && av.fanType == bv.fanType
	case *RecordTypeKeyExpression:
		_, ok := b.(*RecordTypeKeyExpression)
		return ok // All RecordTypeKeyExpressions are structurally equal for position matching
	case *EmptyKeyExpression:
		_, ok := b.(*EmptyKeyExpression)
		return ok
	case *CompositeKeyExpression:
		bv, ok := b.(*CompositeKeyExpression)
		if !ok || len(av.expressions) != len(bv.expressions) {
			return false
		}
		for i := range av.expressions {
			if !keyExpressionEquals(av.expressions[i], bv.expressions[i]) {
				return false
			}
		}
		return true
	case *NestingKeyExpression:
		bv, ok := b.(*NestingKeyExpression)
		return ok && av.parentField == bv.parentField && av.fanType == bv.fanType &&
			keyExpressionEquals(av.child, bv.child)
	case *LiteralKeyExpression:
		bv, ok := b.(*LiteralKeyExpression)
		if !ok {
			return false
		}
		// Compare via proto serialization for type-safe equality
		ap, _ := valueToProto(av.value)
		bp, _ := valueToProto(bv.value)
		return proto.Equal(ap, bp)
	case *KeyWithValueExpression:
		bv, ok := b.(*KeyWithValueExpression)
		return ok && av.splitPoint == bv.splitPoint &&
			keyExpressionEquals(av.innerKey, bv.innerKey)
	default:
		return false
	}
}

// buildPrimaryKeyComponentPositions computes the overlap between an index key
// expression and a primary key expression. Returns nil if there's no overlap.
// Matches Java's RecordMetaDataBuilder.buildPrimaryKeyComponentPositions().
func buildPrimaryKeyComponentPositions(indexKey, primaryKey KeyExpression) []int {
	indexNorm := normalizeKeyForPositions(indexKey)
	pkNorm := normalizeKeyForPositions(primaryKey)

	positions := make([]int, len(pkNorm))
	anyFound := false
	for i, pkExpr := range pkNorm {
		positions[i] = -1
		for j, idxExpr := range indexNorm {
			if keyExpressionEquals(pkExpr, idxExpr) {
				positions[i] = j
				anyFound = true
				break
			}
		}
	}
	if !anyFound {
		return nil
	}
	return positions
}

// GroupingKeyExpression wraps a key expression and divides its columns into
// "grouping" (leading) and "grouped" (trailing) parts. The grouping columns
// form the GROUP BY key, and the grouped columns are the aggregated values.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression.
type GroupingKeyExpression struct {
	wholeKey     KeyExpression
	groupedCount int // number of trailing columns that are "grouped" (aggregated)
}

// GroupBy creates a GroupingKeyExpression that groups by the given expressions
// and treats the receiver as the aggregated value.
// Example: Field("score").GroupBy(Field("game_id")) →
//   wholeKey = Concat(game_id, score), groupedCount = 1
// Matches Java's KeyExpression.groupBy().
func GroupBy(grouped KeyExpression, groupBy ...KeyExpression) *GroupingKeyExpression {
	groupedColCount := keyExpressionColumnSize(grouped)
	allExprs := make([]KeyExpression, 0, len(groupBy)+1)
	allExprs = append(allExprs, groupBy...)
	allExprs = append(allExprs, grouped)

	var wholeKey KeyExpression
	if len(allExprs) == 1 {
		wholeKey = allExprs[0]
	} else {
		wholeKey = Concat(allExprs...)
	}
	return &GroupingKeyExpression{wholeKey: wholeKey, groupedCount: groupedColCount}
}

// Ungrouped creates a GroupingKeyExpression where all columns are "grouped"
// (aggregated) and there are no grouping columns. Used for ungrouped counts.
// Example: Ungrouped(EmptyKey()) → count all records with no grouping.
// Matches Java's KeyExpression.ungrouped().
func Ungrouped(expr KeyExpression) *GroupingKeyExpression {
	return &GroupingKeyExpression{
		wholeKey:     expr,
		groupedCount: keyExpressionColumnSize(expr),
	}
}

// GroupAll creates a GroupingKeyExpression where all columns are "grouping"
// and there are no aggregated columns. Used for COUNT indexes where the
// entire expression forms the GROUP BY key.
// Example: GroupAll(Field("price")) → count grouped by price.
// Matches Java's new GroupingKeyExpression(expr, 0).
func GroupAll(expr KeyExpression) *GroupingKeyExpression {
	return &GroupingKeyExpression{
		wholeKey:     expr,
		groupedCount: 0,
	}
}

// Evaluate delegates to the whole key expression.
// The grouping/grouped split is metadata, not evaluation logic.
func (g *GroupingKeyExpression) Evaluate(msg proto.Message) ([][]any, error) {
	return g.wholeKey.Evaluate(msg)
}

// FieldNames returns field names from the whole key.
func (g *GroupingKeyExpression) FieldNames() []string {
	return g.wholeKey.FieldNames()
}

// GetWholeKey returns the underlying key expression.
func (g *GroupingKeyExpression) GetWholeKey() KeyExpression {
	return g.wholeKey
}

// GetGroupedCount returns the number of trailing "grouped" (aggregated) columns.
func (g *GroupingKeyExpression) GetGroupedCount() int {
	return g.groupedCount
}

// GetGroupingCount returns the number of leading "grouping" (GROUP BY) columns.
func (g *GroupingKeyExpression) GetGroupingCount() int {
	return keyExpressionColumnSize(g.wholeKey) - g.groupedCount
}

// LiteralKeyExpression represents a static constant value in a key expression.
// The value does not depend on the record — it evaluates to the same fixed value for every record.
// Primary use case: passing static arguments to function key expressions, or creating
// composite indexes with a fixed prefix.
// Matches Java's LiteralKeyExpression.
type LiteralKeyExpression struct {
	value any
}

// Literal creates a key expression that always evaluates to the given constant value.
// Supported types: nil, int, int32, int64, float32, float64, bool, string, []byte.
// Matches Java's Key.Expressions.value(Object).
func Literal(value any) *LiteralKeyExpression {
	return &LiteralKeyExpression{value: value}
}

// Evaluate returns the constant value regardless of the record.
// Matches Java's LiteralKeyExpression.evaluateMessage() which ignores the record parameter.
func (l *LiteralKeyExpression) Evaluate(_ proto.Message) ([][]any, error) {
	return [][]any{{l.value}}, nil
}

// FieldNames returns an empty slice — literal expressions don't access any fields.
func (l *LiteralKeyExpression) FieldNames() []string {
	return nil
}

// GetValue returns the constant value held by this expression.
func (l *LiteralKeyExpression) GetValue() any {
	return l.value
}

// KeyWithValueExpression wraps an inner key expression and splits its evaluated
// columns into a "key" portion and a "value" portion. The key portion (columns
// 0..splitPoint-1) is stored in the FDB key, and the value portion (columns
// splitPoint..end) is stored in the FDB value. This enables "covering indexes"
// where additional query-relevant columns can be read without fetching the record.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyWithValueExpression.
type KeyWithValueExpression struct {
	innerKey   KeyExpression
	splitPoint int
}

// KeyWithValue creates a KeyWithValueExpression. splitPoint is the number of
// leading columns that go into the FDB key; remaining columns go into the value.
// Matches Java's Key.Expressions.keyWithValue(inner, splitPoint).
func KeyWithValue(inner KeyExpression, splitPoint int) *KeyWithValueExpression {
	return &KeyWithValueExpression{innerKey: inner, splitPoint: splitPoint}
}

// Evaluate delegates to the inner key expression. The key/value split is only
// applied when writing/reading index entries, not during evaluation.
func (k *KeyWithValueExpression) Evaluate(msg proto.Message) ([][]any, error) {
	return k.innerKey.Evaluate(msg)
}

// FieldNames delegates to the inner key expression.
func (k *KeyWithValueExpression) FieldNames() []string {
	return k.innerKey.FieldNames()
}

// InnerKey returns the wrapped key expression.
func (k *KeyWithValueExpression) InnerKey() KeyExpression {
	return k.innerKey
}

// SplitPoint returns the number of leading columns that form the key portion.
func (k *KeyWithValueExpression) SplitPoint() int {
	return k.splitPoint
}

// SplitEvaluatedKey splits a full evaluated result into key and value portions.
// key = columns[0:splitPoint], value = columns[splitPoint:].
// Matches Java's KeyWithValueExpression.getKey() / getValue().
func (k *KeyWithValueExpression) SplitEvaluatedKey(fullKey []any) (keyPart []any, valuePart []any) {
	if k.splitPoint >= len(fullKey) {
		return fullKey, nil
	}
	return fullKey[:k.splitPoint], fullKey[k.splitPoint:]
}