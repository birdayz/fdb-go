package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// MatchableSortExpression is a RelationalExpression that wraps an inner
// expression with ordering metadata derived from index key parameters.
// It is used ONLY in candidate Traversals — never in the query-side
// expression tree. The sort is expressed by explicitly naming the
// constituent parts of an index (via parameter IDs) rather than by
// KeyExpression-based sort keys (which is what LogicalSortExpression
// uses on the query side).
//
// This separation exists because the sort-by-index-key-parameter
// approach sidesteps the problem of expressing order over nested
// repeated fields — the order comes directly from the index scan, not
// from computing on the base record.
//
// During matching, AdjustMatchRule (in the cascades package) recognises
// this expression and absorbs it by adorning the existing match memo
// with the correct ordering property. The expression itself never
// transforms into a physical sort operator.
//
// Ports Java's
// com.apple.foundationdb.record.query.plan.cascades.expressions.MatchableSortExpression.
type MatchableSortExpression struct {
	sortParameterIDs []values.CorrelationIdentifier
	isReverse        bool
	inner            Quantifier
}

// NewMatchableSortExpression constructs a MatchableSortExpression with
// an explicit inner Quantifier. sortParameterIDs is defensively copied.
func NewMatchableSortExpression(
	sortParameterIDs []values.CorrelationIdentifier,
	isReverse bool,
	inner Quantifier,
) *MatchableSortExpression {
	ids := make([]values.CorrelationIdentifier, len(sortParameterIDs))
	copy(ids, sortParameterIDs)
	return &MatchableSortExpression{
		sortParameterIDs: ids,
		isReverse:        isReverse,
		inner:            inner,
	}
}

// NewMatchableSortExpressionFromExpr is a convenience constructor that
// wraps innerExpr in a ForEach quantifier over an initial Reference.
// Mirrors Java's MatchableSortExpression(List, boolean, RelationalExpression).
func NewMatchableSortExpressionFromExpr(
	sortParameterIDs []values.CorrelationIdentifier,
	isReverse bool,
	innerExpr RelationalExpression,
) *MatchableSortExpression {
	return NewMatchableSortExpression(
		sortParameterIDs,
		isReverse,
		ForEachQuantifier(InitialOf(innerExpr)),
	)
}

// GetSortParameterIDs returns the list of parameter IDs defining the
// order. Read-only — the returned slice must not be mutated.
func (e *MatchableSortExpression) GetSortParameterIDs() []values.CorrelationIdentifier {
	return e.sortParameterIDs
}

// IsReverse reports whether this expression flows data in descending
// (reverse) order.
func (e *MatchableSortExpression) IsReverse() bool {
	return e.isReverse
}

// GetInner returns the inner Quantifier.
func (e *MatchableSortExpression) GetInner() Quantifier {
	return e.inner
}

// ---------------------------------------------------------------------------
// RelationalExpression interface
// ---------------------------------------------------------------------------

// GetQuantifiers returns the single inner Quantifier.
func (e *MatchableSortExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false — single child, no correlation anchoring.
func (e *MatchableSortExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *MatchableSortExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set — the sort
// parameter IDs are candidate-side parameters, not external correlations.
// Mirrors Java's computeCorrelatedToWithoutChildren returning
// ImmutableSet.of().
func (e *MatchableSortExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares two MatchableSortExpressions by their
// sort parameter IDs (position-by-position) and reverse flag.
//
// NOTE: Java's implementation has a bug — it returns true when
// sortParameterIds are NOT equal (uses `!` negation on the list
// equality). We port the CORRECT semantics here: equal when both the
// reverse flag and the parameter ID list match. This is a deliberate
// divergence from the Java bug; see line 264 of the Java source.
func (e *MatchableSortExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*MatchableSortExpression)
	if !ok {
		return false
	}
	if e.isReverse != o.isReverse {
		return false
	}
	if len(e.sortParameterIDs) != len(o.sortParameterIDs) {
		return false
	}
	for i := range e.sortParameterIDs {
		if e.sortParameterIDs[i] != o.sortParameterIDs[i] {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren hashes the sort parameter IDs and the
// reverse flag. Mirrors Java's computeHashCodeWithoutChildren using
// Objects.hash(sortParameterIds, isReverse).
func (e *MatchableSortExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	for _, id := range e.sortParameterIDs {
		h.Write([]byte(id.String()))
		h.Write([]byte{0xff}) // separator
	}
	if e.isReverse {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// GetResultValue returns the inner Quantifier's flowed object value.
// The sort doesn't change the row shape, only the order.
func (e *MatchableSortExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// WithQuantifiers returns a copy of this expression with the given
// quantifiers replacing the original children (single child expected).
func (e *MatchableSortExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &MatchableSortExpression{
		sortParameterIDs: e.sortParameterIDs,
		isReverse:        e.isReverse,
		inner:            quantifiers[0],
	}
}

var _ RelationalExpression = (*MatchableSortExpression)(nil)
