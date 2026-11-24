// Portions derived from FoundationDB Record Layer (FunctionKeyExpression.java,
// KeyExpression.java, FieldKeyExpression.java, NestingKeyExpression.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
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

// NullStandin is Java's Key.Evaluated.NullStandin (Key.java:394-421).
type NullStandin uint8

const (
	// NullStandinNull is a null that a unique index ignores: two records whose
	// keys hold it do not collide. Java's NULL, the default, stored as
	// NOT_UNIQUE.
	NullStandinNull NullStandin = iota
	// NullStandinNullUnique is a null that a unique index does not ignore: two
	// records whose keys hold it collide. Java's NULL_UNIQUE, stored as UNIQUE.
	NullStandinNullUnique
	// NullStandinNotNull evaluates an absent field as its type's default value.
	// Java's NOT_NULL, stored as NOT_NULL. A field of a message that is itself
	// null still evaluates to null (FieldKeyExpression.java:80-89, the target's
	// issue #4141), and a unique index does not ignore it.
	NullStandinNotNull
)

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

// DirectPacker can encode field values directly into a Packer without any boxing.
type DirectPacker interface {
	PackDirect(pk *tuple.Packer, record *FDBStoredRecord[proto.Message], msg proto.Message) bool
}

// createsDuplicates returns true if a key expression can produce multiple tuples
// for a single record (e.g., FanOut on a repeated field). Matches Java's
// KeyExpression.createsDuplicates() — used to validate primary keys don't fan
// out. An UNRECOGNIZED (custom/external) type returns false here (master
// behavior for validateSplitKeyExpression, which reads this as POSITIVE proof —
// a fail-closed true would wrongly admit Split(customScalar) that master
// rejects).
func createsDuplicates(expr KeyExpression) bool {
	return createsDuplicatesRec(expr, false)
}

// createsDuplicatesRec is createsDuplicates with an explicit result for an
// UNRECOGNIZED key expression type, threaded through the recursion. The two
// callers want OPPOSITE unknown-handling (Java sidesteps this by making
// createsDuplicates() an abstract method every subclass implements): Split
// validation passes unrecognized=false (see above); index.CreatesDuplicates
// passes unrecognized=true — FAIL CLOSED, so the M4 DistinctRecords signal never
// reports an unknown-fan-out index root as producing distinct records and elides
// a required DISTINCT (duplicate rows). Every standard index-root type has an
// explicit arm, so the default is unreachable via proto/SQL metadata; it only
// guards a custom/externally-implemented expression.
func createsDuplicatesRec(expr KeyExpression, unrecognized bool) bool {
	switch e := expr.(type) {
	case *FieldKeyExpression:
		return e.fanType == FanTypeFanOut
	case *NestingKeyExpression:
		return e.fanType == FanTypeFanOut || createsDuplicatesRec(e.child, unrecognized)
	case *CompositeKeyExpression:
		for _, child := range e.expressions {
			if createsDuplicatesRec(child, unrecognized) {
				return true
			}
		}
		return false
	case *RecordTypeKeyExpression:
		return false
	case *KeyWithValueExpression:
		return createsDuplicatesRec(e.innerKey, unrecognized)
	case *VersionKeyExpression:
		return false
	case *CardinalityFunctionKeyExpression:
		// Matches Java's CardinalityFunctionKeyExpression.createsDuplicates(),
		// which overrides FunctionKeyExpression's default to false — a count is
		// single-valued (getColumnSize()==1, one entry per record).
		return false
	case *FunctionKeyExpression:
		// Java's OrderFunctionKeyExpression overrides the FunctionKeyExpression
		// default and DELEGATES to its argument
		// (OrderFunctionKeyExpression.java:83-86: createsDuplicates() {
		// return getArguments().createsDuplicates(); }) — an order wrapper is
		// one entry per argument tuple. Go dispatches the same override by the
		// registered function name, since the factory-built Java subclass is a
		// plain FunctionKeyExpression here. CollateFunctionKeyExpression
		// delegates the same way (CollateFunctionKeyExpression.java:177-179).
		if isOrderFunctionName(e.name) || IsLongArithmeticFunction(e.name) ||
			e.name == CollateFuncJRE || e.name == CollateFuncICU {
			return createsDuplicatesRec(e.arguments, unrecognized)
		}
		// Matches Java's FunctionKeyExpression.createsDuplicates() which returns true.
		// Functions can potentially produce multiple values.
		return true
	case *SplitKeyExpression:
		// Matches Java's SplitKeyExpression.createsDuplicates() which returns true.
		return true
	case *GroupingKeyExpression:
		// Java GroupingKeyExpression.createsDuplicates() delegates to the whole
		// key — a grouping/aggregate index over a fan-out field still fans out.
		return createsDuplicatesRec(e.wholeKey, unrecognized)
	case *EmptyKeyExpression:
		// No columns → one (empty) tuple per record, no fan-out.
		return false
	case *LiteralKeyExpression:
		// A constant → single value per record, no fan-out.
		return false
	case *DimensionsKeyExpression:
		return createsDuplicatesRec(e.WholeKey, unrecognized)
	case *ListKeyExpression:
		for _, child := range e.children {
			if createsDuplicatesRec(child, unrecognized) {
				return true
			}
		}
		return false
	default:
		return unrecognized
	}
}

// NormalizeKeyForPositions flattens a key expression into a list of atomic
// components for position matching. CompositeKeyExpression is flattened
// recursively; NestingKeyExpression re-wraps each child; all others return
// themselves as a single-element list.
// Matches Java's KeyExpression.normalizeKeyForPositions().
func NormalizeKeyForPositions(expr KeyExpression) []KeyExpression {
	return normalizeKeyForPositions(expr)
}

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
				parentField:       e.parentField,
				fanType:           e.fanType,
				child:             cn,
				parentNullStandin: e.parentNullStandin,
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

// keyExpressionEquals is Java's KeyExpression.equals: an expression equals
// itself, and otherwise one of the same class with equal parts. It serves
// buildPrimaryKeyComponentPositions, which looks for index components that
// are primary-key components, and Index.equalsJava.
//
// Every KeyExpression type in this package has an arm; one without an arm
// equals only itself. A FunctionKeyExpression equals any function expression
// of the same name and arguments, a CardinalityFunctionKeyExpression included,
// because Java's FunctionKeyExpression.equals tests instanceof rather than the
// class (FunctionKeyExpression.java:286-300).
func keyExpressionEquals(a, b KeyExpression) bool {
	// Not a == b: KeyExpression is exported, and == on two values of one
	// non-comparable type panics.
	if javaObjectsEqual(a, b) {
		return true
	}
	switch av := a.(type) {
	case *FieldKeyExpression:
		bv, ok := b.(*FieldKeyExpression)
		// The null standin is not compared, as Java's FieldKeyExpression.equals
		// does not compare it (FieldKeyExpression.java:406-410).
		return ok && av.fieldName == bv.fieldName && av.fanType == bv.fanType
	case *RecordTypeKeyExpression:
		_, ok := b.(*RecordTypeKeyExpression)
		return ok // A record type key has no state, so every one equals every other
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
		ap, _ := ValueToProto(av.value)
		bp, _ := ValueToProto(bv.value)
		return proto.Equal(ap, bp)
	case *KeyWithValueExpression:
		bv, ok := b.(*KeyWithValueExpression)
		return ok && av.splitPoint == bv.splitPoint &&
			keyExpressionEquals(av.innerKey, bv.innerKey)
	case *VersionKeyExpression:
		_, ok := b.(*VersionKeyExpression)
		return ok
	case *FunctionKeyExpression:
		return functionKeyExpressionsEqual(av, b)
	case *CardinalityFunctionKeyExpression:
		return functionKeyExpressionsEqual(&av.FunctionKeyExpression, b)
	case *DimensionsKeyExpression:
		bv, ok := b.(*DimensionsKeyExpression)
		return ok && av.PrefixSize == bv.PrefixSize && av.DimensionsSize == bv.DimensionsSize &&
			keyExpressionsEqualNilSafe(av.WholeKey, bv.WholeKey)
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

// functionKeyExpressionsEqual is FunctionKeyExpression.equals: b must be a
// function expression of any class, with the same name and equal arguments.
func functionKeyExpressionsEqual(a *FunctionKeyExpression, b KeyExpression) bool {
	var bf *FunctionKeyExpression
	switch bv := b.(type) {
	case *FunctionKeyExpression:
		bf = bv
	case *CardinalityFunctionKeyExpression:
		bf = &bv.FunctionKeyExpression
	default:
		return false
	}
	return a.name == bf.name && keyExpressionsEqualNilSafe(a.arguments, bf.arguments)
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
