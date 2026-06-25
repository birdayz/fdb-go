package recordlayer

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// fieldDescCache is an atomically-swapped cache entry for FieldKeyExpression.
// FieldKeyExpressions are shared across goroutines (via RecordMetaData), so
// the cache must be safe for concurrent reads/writes. Using atomic.Pointer
// ensures no torn reads (unlike bare struct fields which would race).
type fieldDescCache struct {
	msgName string
	fd      protoreflect.FieldDescriptor
}

// FanType controls how repeated (list) proto fields are handled in key expressions.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyExpression.FanType.
type FanType int

// Pre-allocated return values for Evaluate — avoids [][]any allocation on every call.
// These are safe to share because callers only read the values, never mutate.
var (
	emptyKeyResult = [][]any{{}}    // EmptyKeyExpression result
	nilKeyResult   = [][]any{{nil}} // FieldKeyExpression unset field result
)

// FlatEvaluator is an optional interface for KeyExpressions that can return
// a single tuple directly without the [][]any wrapper. Avoids 1 alloc per call.
type FlatEvaluator interface {
	EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error)
}

// ScalarEvaluator is an optional interface for KeyExpressions that extract
// a single scalar value. Avoids both the outer [][]any and inner []any allocs.
type ScalarEvaluator interface {
	EvaluateScalar(record *FDBStoredRecord[proto.Message], msg proto.Message) (any, error)
}

// Int64Evaluator extracts a single int64 value without boxing to any.
// Go's convT64 allocates for every non-zero int64→any conversion.
// This interface bypasses that by returning int64 directly.
type Int64Evaluator interface {
	EvaluateInt64(record *FDBStoredRecord[proto.Message], msg proto.Message) (int64, bool, error)
}

// evaluateKeyFlat calls EvaluateFlat if available, otherwise falls back to Evaluate.
// Saves 1 alloc per call on the hot path (avoids the outer [][]any).
func evaluateKeyFlat(expr KeyExpression, record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if fe, ok := expr.(FlatEvaluator); ok {
		return fe.EvaluateFlat(record, msg)
	}
	tuples, err := expr.Evaluate(record, msg)
	if err != nil {
		return nil, err
	}
	if len(tuples) == 0 {
		return nil, nil
	}
	return tuples[0], nil
}

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
	// fdCache caches the protoreflect.FieldDescriptor for this field name,
	// keyed by the message full name. Avoids ByName() map lookup per Evaluate.
	// Uses atomic.Pointer for goroutine safety (metadata is shared across txns).
	fdCache atomic.Pointer[fieldDescCache]
}

// Field creates a key expression that extracts a single (non-repeated) field.
func Field(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeNone}
}

// resolveFieldDescriptor returns the cached field descriptor for the given message,
// or resolves and caches it atomically. Thread-safe.
func (f *FieldKeyExpression) resolveFieldDescriptor(m protoreflect.Message) (protoreflect.FieldDescriptor, error) {
	msgName := string(m.Descriptor().FullName())
	if cached := f.fdCache.Load(); cached != nil && cached.msgName == msgName {
		return cached.fd, nil
	}
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(f.fieldName))
	if fd == nil {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s not found in message", f.fieldName)}
	}
	f.fdCache.Store(&fieldDescCache{msgName: msgName, fd: fd})
	return fd, nil
}

// FanOut creates a key expression for a repeated field that produces one index entry
// per repeated value. Matches Java's Key.Expressions.field("name", FanType.FanOut).
func FanOut(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeFanOut}
}

// FieldConcatenate creates a key expression for a repeated field that collects all
// repeated values into a single tuple element. Matches Java's
// Key.Expressions.field("name", FanType.Concatenate). This is the argument shape a
// CARDINALITY() index uses: the whole array is materialised into one Key.Evaluated
// so its element count can be taken.
func FieldConcatenate(name string) KeyExpression {
	return &FieldKeyExpression{fieldName: name, fanType: FanTypeConcatenate}
}

// Evaluate extracts the field value(s) from the message.
// For non-repeated fields, returns one tuple with the single value.
// For repeated fields with FanOut, returns one tuple per value.
// For repeated fields with Concatenate, returns one tuple containing a nested tuple of all values.
func (f *FieldKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if msg == nil {
		return f.getNullResult(), nil
	}
	m := msg.ProtoReflect()

	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return nil, err
	}

	if fd.IsList() {
		return f.evaluateRepeated(m, fd)
	}

	// Scalar field — check proto field presence before reading value.
	// For proto2 optional fields, unset → nil (matching Java's hasField() check).
	// For proto3 fields (no presence), always returns the value.
	if fd.HasPresence() && !m.Has(fd) {
		return nilKeyResult, nil
	}
	value := m.Get(fd)
	result, err := scalarToInterface(fd, value)
	if err != nil {
		return nil, err
	}
	// Return single-element result. Inner slice is 1 alloc; outer reuses fixed capacity.
	return [][]any{{result}}, nil
}

// EvaluateFlat returns the scalar value directly as a single-element []any.
func (f *FieldKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if msg == nil {
		return []any{nil}, nil
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return nil, err
	}
	if fd.IsList() {
		// Repeated fields can't be flattened — signal caller to fall through.
		return nil, fmt.Errorf("EvaluateFlat: repeated field %s", f.fieldName)
	}
	if fd.HasPresence() && !m.Has(fd) {
		return []any{nil}, nil
	}
	value := m.Get(fd)
	result, err := scalarToInterface(fd, value)
	if err != nil {
		return nil, err
	}
	return []any{result}, nil
}

// EvaluateScalar returns the single scalar value directly — zero allocs for the return.
func (f *FieldKeyExpression) EvaluateScalar(record *FDBStoredRecord[proto.Message], msg proto.Message) (any, error) {
	if msg == nil {
		return nil, nil
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return nil, err
	}
	if fd.IsList() {
		return nil, fmt.Errorf("EvaluateScalar on repeated field")
	}
	if fd.HasPresence() && !m.Has(fd) {
		return nil, nil
	}
	return scalarToInterface(fd, m.Get(fd))
}

// EvaluateInt64 returns the field value as int64 without boxing.
// Returns (0, false, nil) for nil/unset fields, (val, true, nil) for integer fields.
func (f *FieldKeyExpression) EvaluateInt64(record *FDBStoredRecord[proto.Message], msg proto.Message) (int64, bool, error) {
	if msg == nil {
		return 0, false, nil
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil {
		return 0, false, err
	}
	if fd.HasPresence() && !m.Has(fd) {
		return 0, false, nil
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return m.Get(fd).Int(), true, nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return int64(m.Get(fd).Uint()), true, nil
	case protoreflect.EnumKind:
		return int64(m.Get(fd).Enum()), true, nil
	default:
		return 0, false, nil // not an integer field
	}
}

// PackDirect encodes the field value directly into a Packer without boxing into any.
// Returns false if the field is nil/unset or not a directly-packable type.
func (f *FieldKeyExpression) PackDirect(pk *tuple.Packer, _ *FDBStoredRecord[proto.Message], msg proto.Message) bool {
	if msg == nil {
		return false
	}
	m := msg.ProtoReflect()
	fd, err := f.resolveFieldDescriptor(m)
	if err != nil || fd.IsList() {
		return false
	}
	if fd.HasPresence() && !m.Has(fd) {
		return false
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		pk.EncodeInt(m.Get(fd).Int())
		return true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		pk.EncodeInt(int64(m.Get(fd).Uint()))
		return true
	case protoreflect.EnumKind:
		pk.EncodeInt(int64(m.Get(fd).Enum()))
		return true
	case protoreflect.StringKind:
		pk.EncodeString(m.Get(fd).String())
		return true
	default:
		return false
	}
}

// DirectPacker can encode field values directly into a Packer without any boxing.
type DirectPacker interface {
	PackDirect(pk *tuple.Packer, record *FDBStoredRecord[proto.Message], msg proto.Message) bool
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
		return [][]any{{tuple.Tuple{}}} // One entry containing an empty nested tuple (packable; a raw []any panics)
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
		// The repeated field's values become a single index-key element encoded as a
		// NESTED TUPLE, matching Java's Tuple.addObject(List) → nested Tuple. It must
		// be a tuple.Tuple, not a raw []any: the FDB tuple packer has no case for a
		// bare []any and would panic ("unencodable element") on every save of a record
		// with this index (index_maintainer pack path).
		nested := make(tuple.Tuple, count)
		for i := 0; i < count; i++ {
			val, err := scalarToInterface(fd, list.Get(i))
			if err != nil {
				return nil, err
			}
			nested[i] = val
		}
		return [][]any{{nested}}, nil

	default: // FanTypeNone
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s is repeated with FanType.None", f.fieldName)}
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
	case protoreflect.FloatKind:
		// Must return float32 so FDB tuple encodes as 0x20 (4 bytes).
		// Java protobuf returns java.lang.Float → Tuple.add(Float) → 0x20.
		// value.Float() returns float64; narrowing to float32 matches Java.
		return float32(value.Float()), nil
	case protoreflect.DoubleKind:
		return value.Float(), nil
	case protoreflect.BoolKind:
		return value.Bool(), nil
	case protoreflect.BytesKind:
		return value.Bytes(), nil
	case protoreflect.EnumKind:
		return int64(value.Enum()), nil
	default:
		return nil, &KeyExpressionError{Message: fmt.Sprintf("unsupported field type %s for key expression", kind)}
	}
}

// FieldNames returns the field name accessed by this expression
func (f *FieldKeyExpression) FieldNames() []string {
	return []string{f.fieldName}
}

// ColumnSize returns 1 — a field expression produces a single tuple element.
func (f *FieldKeyExpression) ColumnSize() int {
	return 1
}

// RecordTypeKeyExpression represents the special record type key prefix.
// Matches Java's RecordTypeKeyExpression: evaluates to the record type key
// (an integer derived from the union descriptor field number).
type RecordTypeKeyExpression struct {
	// cachedResults caches the Evaluate return for each type name.
	// RecordTypeKey is deterministic per record type, so cache is safe.
	// Uses sync.Map for concurrent access safety.
	cachedResults sync.Map // string → [][]any
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
func (r *RecordTypeKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if msg == nil {
		return nilKeyResult, nil
	}
	typeName := string(msg.ProtoReflect().Descriptor().Name())

	// Check cache — RecordTypeKey is deterministic per type name
	if r.nested == nil {
		if cached, ok := r.cachedResults.Load(typeName); ok {
			return cached.([][]any), nil
		}
	}

	// Look up the integer record type key (proto field number from union descriptor).
	var typeKey any
	if r.typeKeys != nil {
		if k, ok := r.typeKeys[typeName]; ok {
			typeKey = k
		} else {
			typeKey = typeName
		}
	} else {
		typeKey = typeName
	}

	if r.nested == nil {
		result := [][]any{{typeKey}}
		r.cachedResults.Store(typeName, result)
		return result, nil
	}

	// EvaluateFlat for non-nested case — returns single-element []any
	// (implementing FlatEvaluator interface)

	nestedTuples, err := r.nested.Evaluate(record, msg)
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

// EvaluateScalar returns the type key directly — zero alloc.
func (r *RecordTypeKeyExpression) EvaluateScalar(record *FDBStoredRecord[proto.Message], msg proto.Message) (any, error) {
	if msg == nil {
		return nil, nil
	}
	typeName := string(msg.ProtoReflect().Descriptor().Name())
	if r.typeKeys != nil {
		if k, ok := r.typeKeys[typeName]; ok {
			return k, nil
		}
	}
	return typeName, nil
}

// EvaluateFlat returns the type key as a single-element []any.
func (r *RecordTypeKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if msg == nil {
		return []any{nil}, nil
	}
	typeName := string(msg.ProtoReflect().Descriptor().Name())
	var typeKey any
	if r.typeKeys != nil {
		if k, ok := r.typeKeys[typeName]; ok {
			typeKey = k
		} else {
			typeKey = typeName
		}
	} else {
		typeKey = typeName
	}
	return []any{typeKey}, nil
}

// PackDirect encodes the record type key directly into a Packer.
func (r *RecordTypeKeyExpression) PackDirect(pk *tuple.Packer, record *FDBStoredRecord[proto.Message], msg proto.Message) bool {
	if msg == nil {
		return false
	}
	typeName := string(msg.ProtoReflect().Descriptor().Name())
	if r.typeKeys != nil {
		if k, ok := r.typeKeys[typeName]; ok {
			pk.EncodeElement(k)
			return true
		}
	}
	pk.EncodeElement(typeName)
	return true
}

// FieldNames returns the field names accessed by nested expression
func (r *RecordTypeKeyExpression) FieldNames() []string {
	if r.nested != nil {
		return r.nested.FieldNames()
	}
	return []string{}
}

// ColumnSize returns 1 for the type key itself, plus the nested expression's
// column size if present.
func (r *RecordTypeKeyExpression) ColumnSize() int {
	if r.nested != nil {
		return 1 + r.nested.ColumnSize()
	}
	return 1
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
func (e *EmptyKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	return emptyKeyResult, nil
}

// EvaluateFlat returns empty (no elements to append).
func (e *EmptyKeyExpression) EvaluateFlat(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([]any, error) {
	return nil, nil
}

// FieldNames returns no field names.
func (e *EmptyKeyExpression) FieldNames() []string {
	return nil
}

// ColumnSize returns 0 — an empty expression produces no tuple elements.
func (e *EmptyKeyExpression) ColumnSize() int {
	return 0
}

// CompositeKeyExpression combines multiple key expressions
type CompositeKeyExpression struct {
	expressions []KeyExpression
}

// EvaluateFlat returns the single concatenated tuple directly, avoiding the
// outer [][]any wrapper. Only valid when no child uses fan-out.
// Saves 1 alloc per child (no [][]any) + 1 alloc for the result (no outer wrapper).
func (c *CompositeKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	result := make([]any, 0, len(c.expressions))
	for _, expr := range c.expressions {
		// Try scalar first (zero alloc per value)
		if se, ok := expr.(ScalarEvaluator); ok {
			val, err := se.EvaluateScalar(record, msg)
			if err == nil {
				result = append(result, val)
				continue
			}
		}
		if fe, ok := expr.(FlatEvaluator); ok {
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
		if dp, ok := expr.(DirectPacker); ok {
			if !dp.PackDirect(pk, record, msg) {
				return false
			}
			continue
		}
		return false
	}
	return true
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
func (c *CompositeKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	// Fast path: use EvaluateFlat for children to avoid [][]any allocs.
	// If all children support it, we build a single tuple directly.
	fast := make([]any, 0, len(c.expressions))
	allFlat := true

	for _, expr := range c.expressions {
		// Try scalar first (zero alloc)
		if se, ok := expr.(ScalarEvaluator); ok {
			val, err := se.EvaluateScalar(record, msg)
			if err == nil {
				fast = append(fast, val)
				continue
			}
			// Fall through on error (e.g. repeated field)
		}
		if fe, ok := expr.(FlatEvaluator); ok {
			childFlat, err := fe.EvaluateFlat(record, msg)
			if err == nil {
				fast = append(fast, childFlat...)
				continue
			}
			// Fall through
		}
		childTuples, err := expr.Evaluate(record, msg)
		if err != nil {
			return nil, err
		}
		if len(childTuples) != 1 {
			allFlat = false
			break
		}
		fast = append(fast, childTuples[0]...)
	}

	if allFlat {
		return [][]any{fast}, nil
	}

	// Slow path: cross-product for fan-out expressions
	result := [][]any{{}}
	for _, expr := range c.expressions {
		childTuples, err := expr.Evaluate(record, msg)
		if err != nil {
			return nil, err
		}
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
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s not found in message", n.parentField)}
	}
	if fd.Kind() != protoreflect.MessageKind {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("field %s is not a message type, cannot nest", n.parentField)}
	}

	if fd.IsList() {
		return n.evaluateRepeated(record, m, fd)
	}

	// Scalar message field — get the sub-message and evaluate child on it.
	if !m.Has(fd) {
		// Unset message field → evaluate child on nil (returns null-like results).
		// Match Java: evaluates child on null message → returns null key components.
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

	var result [][]any
	for i := 0; i < count; i++ {
		subMsg := list.Get(i).Message().Interface()
		childTuples, err := n.child.Evaluate(record, subMsg)
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

// ColumnSize returns the child's column size — the parent message field doesn't
// contribute a tuple element.
func (n *NestingKeyExpression) ColumnSize() int {
	return n.child.ColumnSize()
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
	case *VersionKeyExpression:
		return false
	case *CardinalityFunctionKeyExpression:
		// Matches Java's CardinalityFunctionKeyExpression.createsDuplicates(),
		// which overrides FunctionKeyExpression's default to false — a count is
		// single-valued (getColumnSize()==1, one entry per record).
		return false
	case *FunctionKeyExpression:
		// Matches Java's FunctionKeyExpression.createsDuplicates() which returns true.
		// Functions can potentially produce multiple values.
		return true
	case *SplitKeyExpression:
		// Matches Java's SplitKeyExpression.createsDuplicates() which returns true.
		return true
	case *DimensionsKeyExpression:
		return createsDuplicates(e.WholeKey)
	case *ListKeyExpression:
		for _, child := range e.children {
			if createsDuplicates(child) {
				return true
			}
		}
		return false
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
	case *GroupingKeyExpression:
		// Delegates to the underlying whole key expression.
		// Matches Java's GroupingKeyExpression.normalizeKeyForPositions()
		// which calls getWholeKey().normalizeKeyForPositions().
		return normalizeKeyForPositions(e.wholeKey)
	case *KeyWithValueExpression:
		// Delegates to full inner key, matching Java's KeyWithValueExpression.normalizeKeyForPositions().
		// NOTE: PK components must NOT be placed in the value portion (positions >= splitPoint).
		// Doing so causes IndexOutOfBoundsException in Java and similar issues in Go, because
		// getEntryPrimaryKey reads from the FDB key which only has splitPoint columns.
		return normalizeKeyForPositions(e.innerKey)
	case *VersionKeyExpression:
		return []KeyExpression{expr}
	case *CardinalityFunctionKeyExpression:
		return []KeyExpression{expr}
	case *FunctionKeyExpression:
		return []KeyExpression{expr}
	case *SplitKeyExpression:
		// Matches Java's SplitKeyExpression.normalizeKeyForPositions()
		// which returns Collections.nCopies(splitSize, getJoined()).
		result := make([]KeyExpression, e.splitSize)
		for i := range result {
			result[i] = e.joined
		}
		return result
	case *DimensionsKeyExpression:
		return normalizeKeyForPositions(e.WholeKey)
	case *ListKeyExpression:
		// Matches Java's ListKeyExpression.normalizeKeyForPositions():
		// each child is wrapped in a single-child ListKeyExpression to
		// preserve the nesting semantics in position matching.
		if len(e.children) == 0 {
			return nil
		}
		if len(e.children) == 1 {
			return []KeyExpression{expr}
		}
		result := make([]KeyExpression, len(e.children))
		for i, child := range e.children {
			result[i] = ListExpr(child)
		}
		return result
	default:
		return []KeyExpression{expr}
	}
}

// keyExpressionEquals returns true if two key expressions are structurally
// keyExpressionsEqualNilSafe compares two key expressions for structural equality,
// handling nil on either side.
func keyExpressionsEqualNilSafe(a, b KeyExpression) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return keyExpressionEquals(a, b)
}

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
	case *VersionKeyExpression:
		_, ok := b.(*VersionKeyExpression)
		return ok
	case *FunctionKeyExpression:
		bv, ok := b.(*FunctionKeyExpression)
		return ok && av.name == bv.name && keyExpressionEquals(av.arguments, bv.arguments)
	case *SplitKeyExpression:
		bv, ok := b.(*SplitKeyExpression)
		return ok && av.splitSize == bv.splitSize && keyExpressionEquals(av.joined, bv.joined)
	case *ListKeyExpression:
		bv, ok := b.(*ListKeyExpression)
		if !ok || len(av.children) != len(bv.children) {
			return false
		}
		for i := range av.children {
			if !keyExpressionEquals(av.children[i], bv.children[i]) {
				return false
			}
		}
		return true
	case *GroupingKeyExpression:
		bv, ok := b.(*GroupingKeyExpression)
		return ok && av.groupedCount == bv.groupedCount &&
			keyExpressionEquals(av.wholeKey, bv.wholeKey)
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
//
//	wholeKey = Concat(game_id, score), groupedCount = 1
//
// Matches Java's KeyExpression.groupBy().
func GroupBy(grouped KeyExpression, groupBy ...KeyExpression) *GroupingKeyExpression {
	groupedColCount := grouped.ColumnSize()
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
		groupedCount: expr.ColumnSize(),
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
func (g *GroupingKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	return g.wholeKey.Evaluate(record, msg)
}

// EvaluateFlat delegates to the whole key's EvaluateFlat if available.
func (g *GroupingKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if fe, ok := g.wholeKey.(FlatEvaluator); ok {
		return fe.EvaluateFlat(record, msg)
	}
	tuples, err := g.wholeKey.Evaluate(record, msg)
	if err != nil {
		return nil, err
	}
	if len(tuples) == 0 {
		return nil, nil
	}
	return tuples[0], nil
}

// FieldNames returns field names from the whole key.
func (g *GroupingKeyExpression) FieldNames() []string {
	return g.wholeKey.FieldNames()
}

// ColumnSize delegates to the whole key expression's column size.
func (g *GroupingKeyExpression) ColumnSize() int {
	return g.wholeKey.ColumnSize()
}

// GetGroupedCount returns the number of trailing "grouped" (aggregated) columns.
func (g *GroupingKeyExpression) GetGroupedCount() int {
	return g.groupedCount
}

// GetGroupingCount returns the number of leading "grouping" (GROUP BY) columns.
func (g *GroupingKeyExpression) GetGroupingCount() int {
	return g.wholeKey.ColumnSize() - g.groupedCount
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
	// Validate type at construction time — unsupported types would silently
	// produce nil proto fields in ToKeyExpression() and wrong equality results.
	switch value.(type) {
	case nil, int, int32, int64, float32, float64, bool, string, []byte:
		// supported
	default:
		panic(fmt.Sprintf("Literal: unsupported value type %T", value))
	}
	return &LiteralKeyExpression{value: value}
}

// Evaluate returns the constant value regardless of the record.
// Matches Java's LiteralKeyExpression.evaluateMessage() which ignores the record parameter.
func (l *LiteralKeyExpression) Evaluate(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	return [][]any{{l.value}}, nil
}

// FieldNames returns an empty slice — literal expressions don't access any fields.
func (l *LiteralKeyExpression) FieldNames() []string {
	return nil
}

// ColumnSize returns 1 — a literal expression produces a single tuple element.
func (l *LiteralKeyExpression) ColumnSize() int {
	return 1
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
func (k *KeyWithValueExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	return k.innerKey.Evaluate(record, msg)
}

// FieldNames delegates to the inner key expression.
func (k *KeyWithValueExpression) FieldNames() []string {
	return k.innerKey.FieldNames()
}

// ColumnSize returns the split point — only key columns count, not value columns.
// Matches Java's KeyWithValueExpression.getColumnSize().
func (k *KeyWithValueExpression) ColumnSize() int {
	return k.splitPoint
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

// VersionKeyExpression extracts the record version as a Versionstamp tuple element.
// Used by VERSION indexes to index records by their commit version.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.VersionKeyExpression.
type VersionKeyExpression struct{}

// VersionKey creates a key expression that evaluates to the record's version.
// Matches Java's Key.Expressions.version().
func VersionKey() *VersionKeyExpression {
	return &VersionKeyExpression{}
}

// Evaluate returns the record's version as a Versionstamp.
// For complete versions, returns a complete tuple.Versionstamp.
// For incomplete versions, returns an incomplete tuple.Versionstamp (with placeholder bytes).
// If record or version is nil, returns [[nil]].
// Matches Java's VersionKeyExpression.evaluateMessage().
func (v *VersionKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	if record == nil || record.Version == nil {
		return [][]any{{nil}}, nil
	}

	ver := record.Version
	vs := tuple.Versionstamp{
		UserVersion: uint16(ver.GetLocalVersion()),
	}
	copy(vs.TransactionVersion[:], ver.ToBytes()[:GlobalVersionBytes])

	return [][]any{{vs}}, nil
}

// FieldNames returns an empty slice — version expressions don't access proto fields.
func (v *VersionKeyExpression) FieldNames() []string {
	return nil
}

// ColumnSize returns 1 — a version expression produces a single tuple element.
func (v *VersionKeyExpression) ColumnSize() int {
	return 1
}

// FunctionEvaluator evaluates a named function on a record.
// Arguments are the pre-evaluated argument tuples from the arguments expression.
type FunctionEvaluator func(record *FDBStoredRecord[proto.Message], msg proto.Message, arguments [][]any) ([][]any, error)

// globalFunctionRegistry maps function names to their evaluators.
// Matches Java's FunctionKeyExpression.Registry.
// Protected by globalFunctionRegistryMu for concurrent access safety.
var (
	globalFunctionRegistryMu sync.RWMutex
	globalFunctionRegistry   = map[string]FunctionEvaluator{
		"get_versionstamp_incarnation": evaluateGetVersionstampIncarnation,
	}
)

// RegisterFunction registers a named function evaluator in the global registry.
// Call this before building metadata that uses the function.
func RegisterFunction(name string, evaluator FunctionEvaluator) {
	globalFunctionRegistryMu.Lock()
	defer globalFunctionRegistryMu.Unlock()
	globalFunctionRegistry[name] = evaluator
}

// FunctionKeyExpression evaluates a named function on records to produce index
// key values. The function is resolved from the global registry by name.
// Matches Java's FunctionKeyExpression abstract class.
type FunctionKeyExpression struct {
	name      string
	arguments KeyExpression
}

// FunctionExpr creates a FunctionKeyExpression with the given name and arguments.
// The function must be registered in the global registry before evaluation.
// Matches Java's FunctionKeyExpression.create(name, arguments).
func FunctionExpr(name string, arguments KeyExpression) *FunctionKeyExpression {
	return &FunctionKeyExpression{name: name, arguments: arguments}
}

// Evaluate resolves the named function from the registry, evaluates arguments,
// and applies the function. Matches Java's FunctionKeyExpression.evaluateMessage().
func (f *FunctionKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	globalFunctionRegistryMu.RLock()
	evaluator, ok := globalFunctionRegistry[f.name]
	globalFunctionRegistryMu.RUnlock()
	if !ok {
		return nil, &KeyExpressionError{Message: fmt.Sprintf("unknown function key expression: %s", f.name)}
	}

	argTuples, err := f.arguments.Evaluate(record, msg)
	if err != nil {
		return nil, fmt.Errorf("evaluating arguments for function %s: %w", f.name, err)
	}

	return evaluator(record, msg, argTuples)
}

// FieldNames returns field names from the arguments expression.
func (f *FunctionKeyExpression) FieldNames() []string {
	return f.arguments.FieldNames()
}

// ColumnSize returns 1 — a function expression produces a single tuple element.
func (f *FunctionKeyExpression) ColumnSize() int {
	return 1
}

// Name returns the function name.
func (f *FunctionKeyExpression) Name() string {
	return f.name
}

// Arguments returns the arguments key expression.
func (f *FunctionKeyExpression) Arguments() KeyExpression {
	return f.arguments
}

// evaluateGetVersionstampIncarnation implements the get_versionstamp_incarnation function.
// Returns the store's incarnation value as a single int64 tuple element.
// Requires record.Store to be set (non-nil).
// Matches Java's GetVersionstampIncarnationFn.
func evaluateGetVersionstampIncarnation(record *FDBStoredRecord[proto.Message], _ proto.Message, _ [][]any) ([][]any, error) {
	if record == nil || record.Store == nil {
		return nil, &KeyExpressionError{Message: "get_versionstamp_incarnation requires store context on record"}
	}
	return [][]any{{int64(record.Store.GetIncarnation())}}, nil
}
