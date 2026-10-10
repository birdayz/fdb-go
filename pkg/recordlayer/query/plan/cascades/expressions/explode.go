// Portions derived from FoundationDB Record Layer (ExplodeExpression.java,
// QueriedValue.java),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package expressions

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ExplodeExpression is a table-function expression that "explodes"
// a repeated / array-typed Value into a stream of its element
// values. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.ExplodeExpression`.
//
// Conceptually: SQL UNNEST(array_column). For example,
// `UNNEST(tags)` over a row with `tags=['a', 'b', 'c']` produces 3
// rows, one per array element.
//
// No Quantifier children — Explode is a leaf-shaped expression
// whose "data source" is the CollectionValue (typically a column
// reference Value or a literal array Value). The CollectionValue
// is correlation-bearing — Explode's GetCorrelatedToWithoutChildren
// returns the CollectionValue's correlation set.
//
// Result type: the array's element type wrapped in a QueriedValue
// (Type is derived from the CollectionValue's array element type —
// caller is expected to verify the CollectionValue's Type is an
// ArrayType before constructing).
type ExplodeExpression struct {
	collectionValue values.Value
	// withOrdinality, when true, makes the Explode produce a 2-field
	// anonymous record (element, 1-based ordinal) per element instead of
	// the bare element. Mirrors Java's `ExplodeExpression.withOrdinality`
	// (the `WITH ORDINALITY` / `AT atAlias` companion, Java #4112).
	withOrdinality bool
	// zeroBasedOrdinality starts the ordinals at 0 instead of 1. Valid only
	// with ordinality; no SQL form builds it (`AT` is 1-based). Mirrors Java's
	// `ExplodeExpression.zeroBasedOrdinality`.
	zeroBasedOrdinality bool
	// ordinalityNames name the two ordinality slots (`_0`/`_1` unless a SQL
	// unnest's AS/AT aliases name them; see NewExplodeExpressionWithOrdinalityNames).
	ordinalityNames [2]string
	elementType     values.ExactTypeHandle
	resultType      values.ExactTypeHandle
}

// NewExplodeExpression builds a non-ordinal Explode over the given
// collection Value. Caller is responsible for ensuring the
// CollectionValue's Type is an ArrayType (Java's constructor uses
// Verify.verify; Go defers the check to caller — invalid
// construction surfaces as a degenerate result type).
func NewExplodeExpression(collection values.Value) (*ExplodeExpression, error) {
	return newExplodeExpression(collection, false, false, [2]string{})
}

// NewExplodeExpressionWithOrdinalityBase is Java's three-argument
// `new ExplodeExpression(collectionValue, withOrdinality, zeroBasedOrdinality)`:
// a zero-based Explode numbers its elements from 0. Zero-based ordinals without
// ordinality are refused ("cannot base ordinals that are not produced").
func NewExplodeExpressionWithOrdinalityBase(collection values.Value, withOrdinality, zeroBasedOrdinality bool) (*ExplodeExpression, error) {
	if !withOrdinality {
		return newExplodeExpression(collection, false, zeroBasedOrdinality, [2]string{})
	}
	return newExplodeExpression(collection, true, zeroBasedOrdinality, [2]string{values.OrdinalFieldName(0), values.OrdinalFieldName(1)})
}

// NewExplodeExpressionWithOrdinality builds an Explode that also emits a
// 1-based ordinal alongside each element (the `WITH ORDINALITY` variant).
// Mirrors Java's `new ExplodeExpression(collectionValue, withOrdinality)`.
func NewExplodeExpressionWithOrdinality(collection values.Value, withOrdinality bool) (*ExplodeExpression, error) {
	return NewExplodeExpressionWithOrdinalityBase(collection, withOrdinality, false)
}

// NewExplodeExpressionWithOrdinalityNames builds a WITH ORDINALITY Explode
// whose element and ordinal slots carry the given names. Java leaves them
// anonymous and binds a reference by alias; Go's references are exact about
// the row they read, and a SQL unnest's references read its AS/AT names.
func NewExplodeExpressionWithOrdinalityNames(collection values.Value, elementName, ordinalName string) (*ExplodeExpression, error) {
	if elementName == "" || ordinalName == "" || elementName == ordinalName {
		return nil, fmt.Errorf("ExplodeExpression ordinality names must be two distinct names, got %q and %q", elementName, ordinalName)
	}
	return newExplodeExpression(collection, true, false, [2]string{elementName, ordinalName})
}

func newExplodeExpression(collection values.Value, withOrdinality, zeroBasedOrdinality bool, names [2]string) (*ExplodeExpression, error) {
	if collection == nil {
		return nil, fmt.Errorf("ExplodeExpression collection: value is nil")
	}
	if zeroBasedOrdinality && !withOrdinality {
		return nil, fmt.Errorf("ExplodeExpression: cannot base ordinals that are not produced")
	}
	arrayType, ok := collection.Type().(*values.ArrayType)
	if !ok || arrayType == nil || arrayType.ElementType == nil {
		return nil, fmt.Errorf("ExplodeExpression collection: expected an array with an exact element type, got %v", collection.Type())
	}
	elementType, err := snapshotExpressionResultType("ExplodeExpression element", arrayType.ElementType)
	if err != nil {
		return nil, err
	}
	result := elementType.Type()
	if withOrdinality {
		result = values.ExplodeOrdinalityResultTypeNamed(result, names[0], names[1])
	}
	resultType, err := snapshotExpressionResultType("ExplodeExpression", result)
	if err != nil {
		return nil, err
	}
	return &ExplodeExpression{
		collectionValue:     collection,
		withOrdinality:      withOrdinality,
		zeroBasedOrdinality: zeroBasedOrdinality,
		ordinalityNames:     names,
		elementType:         elementType,
		resultType:          resultType,
	}, nil
}

// GetOrdinalityNames returns the element and ordinal slot names of a WITH
// ORDINALITY Explode (empty for the bare variant).
func (e *ExplodeExpression) GetOrdinalityNames() (string, string) {
	return e.ordinalityNames[0], e.ordinalityNames[1]
}

// WithCollection rebuilds this Explode over another collection, keeping its
// ordinality and slot names.
func (e *ExplodeExpression) WithCollection(collection values.Value) (*ExplodeExpression, error) {
	return newExplodeExpression(collection, e.withOrdinality, e.zeroBasedOrdinality, e.ordinalityNames)
}

// GetCollectionValue returns the underlying collection Value (the
// "data source" being exploded).
func (e *ExplodeExpression) GetCollectionValue() values.Value {
	return e.collectionValue
}

// GetWithOrdinality reports whether this Explode produces 1-based
// ordinals alongside the elements.
func (e *ExplodeExpression) GetWithOrdinality() bool { return e.withOrdinality }

// GetZeroBasedOrdinality reports whether the ordinals start at 0.
func (e *ExplodeExpression) GetZeroBasedOrdinality() bool { return e.zeroBasedOrdinality }

// GetElementType returns the element type of the collection value, or
// UnknownType when the collection is not array-typed.
func (e *ExplodeExpression) GetElementType() values.Type { return e.elementType.Type() }

// GetResultValue returns a QueriedValue typed at the explode result
// type. For the bare (non-ordinal) variant this is the array's element
// type — Java's `new QueriedValue(elementType)`. For the WITH ORDINALITY
// variant it is the anonymous 2-field record (element, INT NOT NULL).
//
// If the collection's Type isn't an ArrayType, returns a QueriedValue
// typed at UnknownType (matches Java's invariant failure but doesn't
// panic).
func (e *ExplodeExpression) GetResultValue() values.Value {
	return values.NewQueriedValue(nil, e.resultType.Type())
}

// GetExplodeResultType returns the type a single explode row carries:
// the bare element type, or — under WITH ORDINALITY — the anonymous
// 2-field record (element, INT NOT NULL). Mirrors Java's
// `ExplodeExpression.getExplodeResultType()`.
func (e *ExplodeExpression) GetExplodeResultType() values.Type {
	return e.resultType.Type()
}

// GetQuantifiers returns the empty slice — Explode is a leaf-shaped
// expression with no quantifier children.
func (*ExplodeExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{}
}

// CanCorrelate is false — Explode doesn't introduce a new
// correlation scope; it consumes the collection Value's existing
// correlations.
func (*ExplodeExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — Explode has no children.
func (*ExplodeExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the collection Value's
// correlation set — Explode is correlation-bearing through its
// CollectionValue. Mirrors Java's
// `collectionValue.getCorrelatedTo()`.
func (e *ExplodeExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if e.collectionValue == nil {
		return map[values.CorrelationIdentifier]struct{}{}
	}
	return values.GetCorrelatedToOfValue(e.collectionValue)
}

// EqualsWithoutChildren is true iff `other` is an ExplodeExpression,
// its CollectionValue is semantically equal under the supplied correlation
// mapping, and its ordinality mode is the same. This is Java's
// collectionValue.semanticEquals(other.collectionValue, aliasMap) contract.
// Structural Value equality keeps different fields distinct; alias-aware
// recursion is required for a query Explode correlated to query alias q to
// match a candidate Explode correlated to candidate alias c under q→c.
//
// withOrdinality is folded into the comparison: an ordinal and a
// non-ordinal Explode over the SAME array are distinct expressions
// (different result shape), so the memo must not conflate them. Java
// hashes/equals `(collectionValue, withOrdinality)` for the same reason.
func (e *ExplodeExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*ExplodeExpression)
	if !ok {
		return false
	}
	return values.SemanticEqualsUnderAliasMap(
		e.collectionValue,
		o.collectionValue,
		aliases.ToValuesAliasMap(),
	) && e.withOrdinality == o.withOrdinality && e.zeroBasedOrdinality == o.zeroBasedOrdinality &&
		e.ordinalityNames == o.ordinalityNames
}

// HashCodeWithoutChildren mixes the class discriminator + the collection
// Value's SEMANTIC hash + the ordinality flag. Mirrors Java's
// `Objects.hash(collectionValue, withOrdinality)` — Java hashes the full
// collectionValue (its content), so a Name()-only mix was both
// less faithful and too coarse: two explodes over DIFFERENT array
// fields hashed identically ("field"), which left the extraction
// tie-break blind and degraded memo bucketing. SemanticHashCode excludes
// correlation names, preserving equal-under-alias-map ⇒ equal hash.
func (e *ExplodeExpression) HashCodeWithoutChildren() uint64 {
	const classDisc uint64 = 0xE1730DE
	var h uint64 = classDisc
	if e.collectionValue != nil {
		h = h*0x100000001b3 ^ values.SemanticHashCode(e.collectionValue)
	}
	if e.withOrdinality {
		h = h*31 + 1
	}
	// Only a zero-based Explode mixes the flag, so every existing hash is
	// unchanged (Java ExplodeExpression.hashCodeWithoutChildren).
	if e.zeroBasedOrdinality {
		h = h*31 + 1
	}
	return h
}

func (e *ExplodeExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("ExplodeExpression", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return e, nil
}

var _ RelationalExpression = (*ExplodeExpression)(nil)
