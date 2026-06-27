package expressions

import (
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
// (the seed exposes Type via the CollectionValue's array element
// type — caller is expected to verify the CollectionValue's Type
// is an ArrayType before constructing).
type ExplodeExpression struct {
	collectionValue values.Value
	// withOrdinality, when true, makes the Explode produce a 2-field
	// anonymous record (element, 1-based ordinal) per element instead of
	// the bare element. Mirrors Java's `ExplodeExpression.withOrdinality`
	// (the `WITH ORDINALITY` / `AT atAlias` companion, Java #4112).
	withOrdinality bool
}

// NewExplodeExpression builds a non-ordinal Explode over the given
// collection Value. Caller is responsible for ensuring the
// CollectionValue's Type is an ArrayType (Java's constructor uses
// Verify.verify; the Go seed defers the check to caller — invalid
// construction surfaces as a degenerate result type).
func NewExplodeExpression(collection values.Value) *ExplodeExpression {
	return &ExplodeExpression{collectionValue: collection}
}

// NewExplodeExpressionWithOrdinality builds an Explode that also emits a
// 1-based ordinal alongside each element (the `WITH ORDINALITY` variant).
// Mirrors Java's `new ExplodeExpression(collectionValue, withOrdinality)`.
func NewExplodeExpressionWithOrdinality(collection values.Value, withOrdinality bool) *ExplodeExpression {
	return &ExplodeExpression{collectionValue: collection, withOrdinality: withOrdinality}
}

// GetCollectionValue returns the underlying collection Value (the
// "data source" being exploded).
func (e *ExplodeExpression) GetCollectionValue() values.Value {
	return e.collectionValue
}

// GetWithOrdinality reports whether this Explode produces 1-based
// ordinals alongside the elements.
func (e *ExplodeExpression) GetWithOrdinality() bool { return e.withOrdinality }

// GetElementType returns the element type of the collection value, or
// UnknownType when the collection is not array-typed.
func (e *ExplodeExpression) GetElementType() values.Type {
	if e.collectionValue == nil {
		return values.UnknownType
	}
	if at, ok := e.collectionValue.Type().(*values.ArrayType); ok && at.ElementType != nil {
		return at.ElementType
	}
	return values.UnknownType
}

// GetResultValue returns a QueriedValue typed at the explode result
// type. For the bare (non-ordinal) variant this is the array's element
// type — Java's `new QueriedValue(elementType)`. For the WITH ORDINALITY
// variant it is the anonymous 2-field record (element, INT NOT NULL).
//
// If the collection's Type isn't an ArrayType, returns a QueriedValue
// typed at UnknownType (matches Java's invariant failure but doesn't
// panic).
func (e *ExplodeExpression) GetResultValue() values.Value {
	return values.NewQueriedValue(nil, e.GetExplodeResultType())
}

// GetExplodeResultType returns the type a single explode row carries:
// the bare element type, or — under WITH ORDINALITY — the anonymous
// 2-field record (element, INT NOT NULL). Mirrors Java's
// `ExplodeExpression.getExplodeResultType()`.
func (e *ExplodeExpression) GetExplodeResultType() values.Type {
	elem := e.GetElementType()
	if e.withOrdinality {
		return values.ExplodeOrdinalityResultType(elem)
	}
	return elem
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

// EqualsWithoutChildren is true iff `other` is an ExplodeExpression
// AND its CollectionValue is pointer-equal to ours.
//
// The seed conservatively requires pointer equality on the
// CollectionValue. Java's `collectionValue.semanticEquals(...)`
// would dispatch through a structural-equality walker, but that's
// gated on porting `values.SemanticEquals` as a free function.
//
// A previous version used a `Name()`-fallback which produced false
// positives — `*FieldValue` (and several other concrete Values)
// returns a constant `Name()` for all instances, so two
// `Explode(FieldValue{"tags"})` and `Explode(FieldValue{"categories"})`
// would compare equal and `Reference.Insert` would silently drop
// one. Reverted to pointer equality only — strictly conservative,
// produces no false positives.
//
// withOrdinality is folded into the comparison: an ordinal and a
// non-ordinal Explode over the SAME array are distinct expressions
// (different result shape), so the memo must not conflate them. Java
// hashes/equals `(collectionValue, withOrdinality)` for the same reason.
func (e *ExplodeExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*ExplodeExpression)
	if !ok {
		return false
	}
	return e.collectionValue == o.collectionValue && e.withOrdinality == o.withOrdinality
}

// HashCodeWithoutChildren mixes the class discriminator + the
// collection Value's name + the ordinality flag. Mirrors Java's
// `Objects.hash(collectionValue, withOrdinality)` (Java preserves the
// pre-ordinality hash for the withOrdinality=false case; the Go hash is
// not wire-stable so we simply fold the flag in unconditionally).
func (e *ExplodeExpression) HashCodeWithoutChildren() uint64 {
	const classDisc uint64 = 0xE1730DE
	var h uint64 = classDisc
	if e.collectionValue != nil {
		// Cheap mix — the collection Value's Name() string hashed.
		for _, b := range []byte(e.collectionValue.Name()) {
			h = h*31 + uint64(b)
		}
	}
	if e.withOrdinality {
		h = h*31 + 1
	}
	return h
}

func (e *ExplodeExpression) WithQuantifiers(_ []Quantifier) RelationalExpression {
	return e
}

var _ RelationalExpression = (*ExplodeExpression)(nil)
