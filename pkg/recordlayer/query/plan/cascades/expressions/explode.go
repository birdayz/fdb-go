package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
}

// NewExplodeExpression builds an Explode over the given collection
// Value. Caller is responsible for ensuring the CollectionValue's
// Type is an ArrayType (Java's constructor uses Verify.verify; the
// Go seed defers the check to caller — invalid construction
// surfaces as a degenerate result type).
func NewExplodeExpression(collection values.Value) *ExplodeExpression {
	return &ExplodeExpression{collectionValue: collection}
}

// GetCollectionValue returns the underlying collection Value (the
// "data source" being exploded).
func (e *ExplodeExpression) GetCollectionValue() values.Value {
	return e.collectionValue
}

// GetResultValue returns a QueriedValue typed at the array's
// element type. Java does the same: `new QueriedValue(elementType)`.
//
// If the collection's Type isn't an ArrayType, returns a
// QueriedValue typed at UnknownType (matches Java's invariant
// failure but doesn't panic).
func (e *ExplodeExpression) GetResultValue() values.Value {
	if e.collectionValue == nil {
		return values.NewQueriedValue(nil, values.UnknownType)
	}
	t := e.collectionValue.Type()
	if at, ok := t.(*values.ArrayType); ok && at.ElementType != nil {
		return values.NewQueriedValue(nil, at.ElementType)
	}
	return values.NewQueriedValue(nil, values.UnknownType)
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
func (e *ExplodeExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*ExplodeExpression)
	if !ok {
		return false
	}
	return e.collectionValue == o.collectionValue
}

// HashCodeWithoutChildren mixes the class discriminator + the
// collection Value's name. Mirrors Java's `Objects.hash(collectionValue)`.
func (e *ExplodeExpression) HashCodeWithoutChildren() uint64 {
	const classDisc uint64 = 0xE1730DE
	if e.collectionValue == nil {
		return classDisc
	}
	// Cheap mix — the collection Value's Name() string hashed.
	var h uint64 = classDisc
	for _, b := range []byte(e.collectionValue.Name()) {
		h = h*31 + uint64(b)
	}
	return h
}

func (e *ExplodeExpression) WithQuantifiers(_ []Quantifier) RelationalExpression {
	return e
}

var _ RelationalExpression = (*ExplodeExpression)(nil)
