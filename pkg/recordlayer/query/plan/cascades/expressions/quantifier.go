package expressions

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// QuantifierKind enumerates the three flavours of Quantifier Java
// distinguishes: ForEach, Existential, Physical. The seed implements
// only ForEach (most common, used by every logical operator). The
// other two land as the planner needs them — Existential when EXISTS-
// subquery rules port (B5 Batch B), Physical when the executor tree
// materialises (Track C).
type QuantifierKind int

const (
	// QuantifierForEach: each row of the inner expression flows
	// individually to the owning expression. The default kind for all
	// SQL constructs (FROM-list, JOIN inputs, sub-select inputs).
	QuantifierForEach QuantifierKind = iota

	// QuantifierExistential: the inner expression is consulted only
	// to determine whether at least one row exists. Used by EXISTS /
	// NOT EXISTS. NOT IMPLEMENTED in the seed.
	QuantifierExistential

	// QuantifierPhysical: a quantifier in the physical (post-planning)
	// tree. NOT IMPLEMENTED in the seed.
	QuantifierPhysical
)

// Quantifier ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.Quantifier`.
//
// A Quantifier connects two RelationalExpressions: the inner one
// (which produces records) and the outer/owning one (which consumes
// records via this Quantifier). The Quantifier carries:
//
//   - an alias (CorrelationIdentifier) — symbolic name for the rows
//     flowing along this Quantifier; used by predicates / projections
//     in the owning expression to refer back to the inner;
//   - a Reference — handle on the equivalence class of the inner
//     expression (the Memo group, once B3 lands; a single-element
//     class today).
//
// Java models the kinds (ForEach, Existential, Physical) as
// `Quantifier.ForEach`, `Quantifier.Existential`, `Quantifier.Physical`
// — distinct subclasses sharing the abstract base. Go has no sealed
// classes; we use a single struct with a `Kind` field, since every kind
// shares the same fields (alias + ranges-over). Subclass-only state
// (Java's `ForEach.isNullOnEmpty`) is added when a kind needs it.
type Quantifier struct {
	kind       QuantifierKind
	alias      values.CorrelationIdentifier
	rangesOver *Reference
}

// ForEachQuantifier builds a ForEach quantifier ranging over the given
// Reference, with a freshly-allocated unique alias. Equivalent to
// Java's `Quantifier.forEach(reference)`.
func ForEachQuantifier(rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:       QuantifierForEach,
		alias:      values.UniqueCorrelationIdentifier(),
		rangesOver: rangesOver,
	}
}

// NamedForEachQuantifier builds a ForEach quantifier with an explicit
// alias. Used when the alias must match an existing CorrelationIdentifier
// (e.g. when a SQL alias is already chosen by the parser).
func NamedForEachQuantifier(alias values.CorrelationIdentifier, rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:       QuantifierForEach,
		alias:      alias,
		rangesOver: rangesOver,
	}
}

// Kind returns the Quantifier's flavour.
func (q Quantifier) Kind() QuantifierKind { return q.kind }

// GetAlias returns the symbolic identifier for rows flowing along
// this Quantifier.
func (q Quantifier) GetAlias() values.CorrelationIdentifier { return q.alias }

// GetRangesOver returns the Reference holding the inner expression.
func (q Quantifier) GetRangesOver() *Reference { return q.rangesOver }

// GetFlowedObjectValue returns a Value representing "the row currently
// flowing along this Quantifier". Predicates / projections in the
// owning expression use this Value (via FieldValue accesses) to refer
// to columns of the inner expression's output.
//
// Equivalent to Java's `Quantifier.getFlowedObjectValue()`. Implemented
// via QuantifiedObjectValue, which already exists in cascades/values/.
func (q Quantifier) GetFlowedObjectValue() values.Value {
	return values.NewQuantifiedObjectValue(q.alias)
}

// GetCorrelatedTo returns the set of CorrelationIdentifiers the inner
// expression depends on — i.e. the Quantifier's transitive correlation
// set. Used by the planner to compute correlation order across an
// expression tree.
//
// The seed defers correlation-set computation through the inner
// expression to a follow-on shift (needs every concrete RelationalExpression
// to expose GetCorrelatedToWithoutChildren — which the seed does — but
// also requires walking children, which only matters when there are
// multi-level expression trees in flight). Today returns the empty
// set; revisit when multi-level rules port.
func (q Quantifier) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}
