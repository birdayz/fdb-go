package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalFilterExpression is a logical operator that filters the rows
// flowing through its inner Quantifier by a list of QueryPredicates,
// implicitly AND-conjuncted.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalFilterExpression`.
// The Java class supports PlannerGraph rendering, TranslationMap
// rebasing, and a memoised conjuncted-predicate accessor. Seed exposes
// the structural surface — predicates + inner Quantifier, plus the
// RelationalExpression interface methods — and defers PlannerGraph /
// rebasing to subsequent shifts.
//
// CanCorrelate returns false: a LogicalFilter has exactly one
// Quantifier, so there's no second Quantifier whose evaluation could
// depend on this one's. The whole correlation concept is inert here.
type LogicalFilterExpression struct {
	queryPredicates []predicates.QueryPredicate
	inner           Quantifier
}

// NewLogicalFilterExpression constructs a LogicalFilter wrapping
// `inner` and filtering by the AND of `queryPredicates`. The
// predicates list is copied defensively.
func NewLogicalFilterExpression(queryPredicates []predicates.QueryPredicate, inner Quantifier) *LogicalFilterExpression {
	copied := make([]predicates.QueryPredicate, len(queryPredicates))
	copy(copied, queryPredicates)
	return &LogicalFilterExpression{
		queryPredicates: copied,
		inner:           inner,
	}
}

// GetPredicates returns the predicate list. Read-only — callers must
// not mutate.
func (e *LogicalFilterExpression) GetPredicates() []predicates.QueryPredicate {
	return e.queryPredicates
}

// GetInner returns the inner Quantifier.
func (e *LogicalFilterExpression) GetInner() Quantifier {
	return e.inner
}

// GetResultValue is the inner Quantifier's flowed object value — a
// LogicalFilter doesn't reshape rows, only drops some. Java's
// implementation is identical.
func (e *LogicalFilterExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalFilterExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false for LogicalFilter — one child means no
// inter-child correlation possible.
func (e *LogicalFilterExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child, ordering is trivially unique.
func (e *LogicalFilterExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the union of correlation
// sets across all predicates (including the Value trees those
// predicates carry — see predicates.GetCorrelatedToOfPredicate).
func (e *LogicalFilterExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, p := range e.queryPredicates {
		for k := range predicates.GetCorrelatedToOfPredicate(p) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares two LogicalFilterExpressions by
// predicate-list equality, treating CorrelationIdentifiers as equal
// under `aliases`. Children are not consulted — that's the caller's
// job (typically via SemanticEquals).
//
// The aliases parameter is currently unused: the seed's PredicateEquals
// is positional and structural, with no CorrelationIdentifier-aware
// matcher yet. As predicates gain alias-aware semantic equality
// (B2 follow-on), thread `aliases` through.
func (e *LogicalFilterExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalFilterExpression)
	if !ok {
		return false
	}
	_ = aliases // see doc comment
	if len(e.queryPredicates) != len(o.queryPredicates) {
		return false
	}
	for i := range e.queryPredicates {
		if !predicates.PredicateEquals(e.queryPredicates[i], o.queryPredicates[i]) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren hashes the predicate list. Two
// LogicalFilters that agree on EqualsWithoutChildren under the empty
// alias map MUST agree here — we use the predicate count + a recursive
// fold over predicate text via Explain() (PredicateEquals'
// reference-comparable companion). When QueryPredicate gains a real
// hash method, switch to that.
func (e *LogicalFilterExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	for _, p := range e.queryPredicates {
		h.Write([]byte(p.Explain()))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *LogicalFilterExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	return &LogicalFilterExpression{
		inner:           quantifiers[0],
		queryPredicates: e.queryPredicates,
	}
}

// Compile-time check that LogicalFilterExpression implements
// RelationalExpression.
var _ RelationalExpression = (*LogicalFilterExpression)(nil)
