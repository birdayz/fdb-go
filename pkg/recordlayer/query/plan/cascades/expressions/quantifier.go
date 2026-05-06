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
	kind        QuantifierKind
	alias       values.CorrelationIdentifier
	rangesOver  *Reference
	nullOnEmpty bool
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

// ForEachNullOnEmptyQuantifier builds a ForEach quantifier with
// nullOnEmpty=true. Used for LEFT JOIN semantics where the inner
// side should produce a NULL row when empty.
func ForEachNullOnEmptyQuantifier(rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:        QuantifierForEach,
		alias:       values.UniqueCorrelationIdentifier(),
		rangesOver:  rangesOver,
		nullOnEmpty: true,
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

// ExistentialQuantifier builds an Existential quantifier — the inner
// expression is consulted only to determine whether at least one row
// exists. Used by EXISTS / NOT EXISTS subqueries.
//
// The flowed-object semantics differ from ForEach: an Existential
// quantifier doesn't make rows of the inner available to the outer's
// predicates / projection — only the boolean "any row exists" signal.
// Most planner rules that operate on Quantifiers care about this
// distinction; today the seed has no such rule, so the kind is
// available for the SQL parser to construct EXISTS shapes that future
// rules will recognise.
func ExistentialQuantifier(rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:       QuantifierExistential,
		alias:      values.UniqueCorrelationIdentifier(),
		rangesOver: rangesOver,
	}
}

// NamedExistentialQuantifier builds an Existential quantifier with an
// explicit alias. Used when the alias is already pinned by the parser.
func NamedExistentialQuantifier(alias values.CorrelationIdentifier, rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:       QuantifierExistential,
		alias:      alias,
		rangesOver: rangesOver,
	}
}

// NewPhysicalQuantifier builds a Physical quantifier ranging over the
// given Reference, with a freshly-allocated unique alias. Used in the
// PLANNING phase when ImplementationRules create physical plan wrappers.
func NewPhysicalQuantifier(rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:       QuantifierPhysical,
		alias:      values.UniqueCorrelationIdentifier(),
		rangesOver: rangesOver,
	}
}

// NamedPhysicalQuantifier builds a Physical quantifier with a specific
// alias. Used when the alias must match the inner quantifier's alias
// so predicates/projections continue to resolve correctly.
func NamedPhysicalQuantifier(alias values.CorrelationIdentifier, rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:       QuantifierPhysical,
		alias:      alias,
		rangesOver: rangesOver,
	}
}

// RebuildQuantifier creates a new Quantifier with the same kind and
// alias but ranging over a different Reference. Used by
// FinalizeExpressionsRule to point quantifiers at disentangled child
// References. Mirrors Java's Quantifier.toBuilder().build(reference).
func RebuildQuantifier(q Quantifier, newRef *Reference) Quantifier {
	return Quantifier{
		kind:        q.kind,
		alias:       q.alias,
		nullOnEmpty: q.nullOnEmpty,
		rangesOver:  newRef,
	}
}

// Kind returns the Quantifier's flavour.
func (q Quantifier) Kind() QuantifierKind { return q.kind }

// GetAlias returns the symbolic identifier for rows flowing along
// this Quantifier.
func (q Quantifier) GetAlias() values.CorrelationIdentifier { return q.alias }

// GetRangesOver returns the Reference holding the inner expression.
func (q Quantifier) GetRangesOver() *Reference { return q.rangesOver }

// IsNullOnEmpty returns true for ForEach quantifiers that should
// produce a NULL row when the inner is empty (LEFT JOIN semantics).
func (q Quantifier) IsNullOnEmpty() bool { return q.nullOnEmpty }

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
