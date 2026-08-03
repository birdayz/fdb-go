package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// FunctionNameCardinality is the registry name for the CARDINALITY() key
// expression. Matches Java's FunctionNames.CARDINALITY ("cardinality") byte
// for byte — the proto Function.name written for a cardinality-keyed index is
// identical across engines, so a Go-written cardinality index round-trips
// wire-compatibly with Java.
const FunctionNameCardinality = "cardinality"

// wrappedArrayValuesFieldName is the repeated field name inside the
// nullable-array wrapper message Java writes for a nullable array column
// (message M { repeated R values = 1; }). Matches Java's
// NullableArrayTypeUtils.REPEATED_FIELD_NAME byte for byte, mirroring the
// metadata-layer constant of the same value (proto_types.go).
const wrappedArrayValuesFieldName = "values"

// init registers the "cardinality" registry evaluator. The registry path is
// the materialize-and-count fallback (Java's evaluateFunction); the two
// protobuf fast paths live on CardinalityFunctionKeyExpression below, which is
// the concrete root a cardinality index uses. Registering the fallback keeps
// any bare FunctionExpr("cardinality", …) evaluable too.
func init() {
	RegisterFunction(FunctionNameCardinality, evaluateCardinalityMaterialized)
}

// CardinalityFunctionKeyExpression is the root key expression of a CARDINALITY()
// index. It counts the argument array's elements to produce the single key
// column. Ports Java's CardinalityFunctionKeyExpression:
//
//   - getMinArguments()==getMaxArguments()==1, getColumnSize()==1
//   - createsDuplicates()==false (a count is single-valued — see createsDuplicates)
//   - evaluateMessage's two protobuf fast paths (plain repeated field, nullable
//     wrapper) plus the materialize-and-count fallback for deeper nestings.
//
// It embeds the generic FunctionKeyExpression so it serialises to the identical
// proto (Function{name:"cardinality", arguments:…}) and reuses FieldNames /
// ColumnSize / Name / Arguments. Only Evaluate is overridden to add the fast
// paths; the result is identical to the registry fallback either way.
type CardinalityFunctionKeyExpression struct {
	FunctionKeyExpression
}

// CardinalityExpr builds a CARDINALITY() index root over the given argument key
// expression. The argument is field("arr", Concatenate) for a plain array
// column, or field("arr").nest(field("values", Concatenate)) for a Java-written
// nullable-array wrapper. Mirrors Java's
// function("cardinality", field("arr", …)).
func CardinalityExpr(arguments KeyExpression) *CardinalityFunctionKeyExpression {
	return &CardinalityFunctionKeyExpression{
		FunctionKeyExpression: FunctionKeyExpression{
			name:      FunctionNameCardinality,
			arguments: arguments,
		},
	}
}

// Evaluate applies Java's two protobuf fast paths against the record message,
// falling back to materialize-and-count for shapes the fast paths do not
// recognise (deeper nesting, the null cases).
//
// Fast path 1 — nullable-array wrapper: the argument is
// field("wrapper").nest(field("values", Concatenate)). If the wrapper message
// is present, count its "values" field directly (a present wrapper with zero
// elements is a non-null empty array → 0). If absent, the array is NULL → fall
// through to the materialized null result.
//
// Fast path 2 — plain repeated field: the argument is
// field("arr", Concatenate). Count the repeated field directly
// (Message.getRepeatedFieldCount). A zero count keys 0, NOT NULL: a plain
// repeated field is the NOT NULL array's storage shape, and an empty one is
// [] whose cardinality is 0. NULL keys arise only where the array itself can
// be absent — an absent wrapper, or a null parent on a deeper nesting — both
// of which reach the materialized fallback.
func (c *CardinalityFunctionKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	if msg != nil {
		if result, handled := c.evaluateFast(msg); handled {
			return result, nil
		}
	}
	// Fallback: evaluate the argument expression and count the materialized
	// array — Java's super.evaluateMessage → evaluateFunction.
	argTuples, err := c.arguments.Evaluate(record, msg)
	if err != nil {
		return nil, fmt.Errorf("evaluating arguments for function %s: %w", c.name, err)
	}
	return evaluateCardinalityMaterialized(record, msg, argTuples)
}

// evaluateFast tries the two protobuf fast paths. Returns (result, true) when
// the argument shape was recognised and the array field was present, or
// (nil, false) to defer to the materialized fallback (deeper nesting, or the
// wrapper/field absent → NULL via the fallback).
func (c *CardinalityFunctionKeyExpression) evaluateFast(msg proto.Message) ([][]any, bool) {
	m := msg.ProtoReflect()
	desc := m.Descriptor()

	// Fast path 1: nullable-array wrapper —
	// field("wrapper").nest(field("values", Concatenate)).
	if nest, ok := c.arguments.(*NestingKeyExpression); ok {
		if child, ok := nest.child.(*FieldKeyExpression); ok &&
			child.fieldName == wrappedArrayValuesFieldName &&
			child.fanType == FanTypeConcatenate {
			wrapperFD := desc.Fields().ByName(protoreflect.Name(nest.parentField))
			if wrapperFD != nil && wrapperFD.Kind() == protoreflect.MessageKind && !wrapperFD.IsList() {
				if !m.Has(wrapperFD) {
					return nil, false // wrapper absent → NULL via fallback
				}
				wrapper := m.Get(wrapperFD).Message()
				valuesFD := wrapper.Descriptor().Fields().ByName(wrappedArrayValuesFieldName)
				if valuesFD != nil && valuesFD.IsList() {
					// Present wrapper → non-null array; zero elements is a valid 0.
					return cardinalityCountRepeated(wrapper, valuesFD), true
				}
			}
		}
		return nil, false
	}

	// Fast path 2: plain repeated field — field("arr", Concatenate).
	if field, ok := c.arguments.(*FieldKeyExpression); ok && field.fanType == FanTypeConcatenate {
		fd := desc.Fields().ByName(protoreflect.Name(field.fieldName))
		if fd != nil && fd.IsList() {
			// A repeated field is always an array; an empty one is []
			// and counts 0. The column is NOT NULL by construction here.
			return cardinalityCountRepeated(m, fd), true
		}
	}

	return nil, false
}

// cardinalityCountRepeated counts a repeated proto field directly —
// Message.getRepeatedFieldCount — and keys that count VERBATIM, zero included.
// Java does the same in both fast paths with no zero special-case
// (CardinalityFunctionKeyExpression.java:115-117 and :132-133 both end in
// Key.Evaluated.scalar(cardinality)).
//
// A zero must NOT become a NULL key. The count is a property of the ARRAY, and
// a repeated field is always present as an array — an empty one is [], whose
// cardinality is 0. Keying NULL there would put the index at odds with the base
// table, which materializes the same absent repeated field as [] and counts 0,
// so an index-backed `CARDINALITY(a) = 0` and a full-scan one would disagree.
// It would also diverge on the WIRE: Java writes the 0 key for this record.
func cardinalityCountRepeated(m protoreflect.Message, fd protoreflect.FieldDescriptor) [][]any {
	return [][]any{{int64(m.Get(fd).List().Len())}}
}

// evaluateCardinalityMaterialized counts the materialized argument list — Java's
// evaluateFunction over a single-element Key.Evaluated. arguments is the
// pre-evaluated argument-expression result ([][]any): one row whose single
// element is either a tuple.Tuple of the array's elements (FanType.Concatenate)
// or nil (the array was NULL/unset).
//
// Only a NULL ARGUMENT yields a NULL key; a present-but-EMPTY list counts 0.
// Java draws the line in exactly that place
// (CardinalityFunctionKeyExpression.java:146-152): `arg == null` returns
// Key.Evaluated.NULL, everything else returns scalar(argList.size()), and an
// empty list's size is 0. Collapsing empty into NULL here would contradict the
// read path, which materializes an empty repeated field as [].
//
// This is also the bare-registry evaluator for any
// FunctionExpr("cardinality", …) that is not a CardinalityFunctionKeyExpression.
func evaluateCardinalityMaterialized(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
	if len(arguments) == 0 || len(arguments[0]) == 0 {
		return nilKeyResult, nil
	}
	n, ok := materializedArrayLen(arguments[0][0])
	if !ok {
		// The argument itself is nil — a NULL array (an absent wrapper, or a
		// null parent on a deeper nesting like struct.arr). Java: `arg == null`
		// → Key.Evaluated.NULL.
		return nilKeyResult, nil
	}
	// Present list, possibly EMPTY: Java returns scalar(argList.size()), and 0
	// is a real cardinality — not NULL.
	return [][]any{{int64(n)}}, nil
}

// materializedArrayLen returns the element count of a materialized
// FanType.Concatenate argument. The concatenated array is a tuple.Tuple (a
// named []TupleElement; key_expression.evaluateRepeated builds it). Returns
// (0, false) for a nil argument (NULL array).
func materializedArrayLen(arg any) (int, bool) {
	switch a := arg.(type) {
	case nil:
		return 0, false
	case tuple.Tuple:
		return len(a), true
	case []any:
		return len(a), true
	default:
		return 0, false
	}
}
