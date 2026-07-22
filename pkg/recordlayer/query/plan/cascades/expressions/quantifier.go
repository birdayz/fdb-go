package expressions

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// QuantifierKind enumerates the three flavours of Quantifier Java
// distinguishes: ForEach (the default, used by every logical
// operator), Existential (EXISTS / NOT EXISTS legs — consulted by
// compensation to keep existential legs out of result compensation),
// and Physical (unused in Go: the physical tree is the separate
// plans.RecordQueryPlan hierarchy bridged by the physical wrappers,
// not a quantifier kind).
type QuantifierKind int

const (
	// QuantifierForEach: each row of the inner expression flows
	// individually to the owning expression. The default kind for all
	// SQL constructs (FROM-list, JOIN inputs, sub-select inputs).
	QuantifierForEach QuantifierKind = iota

	// QuantifierExistential: the inner expression is consulted only
	// to determine whether at least one row exists. Used by EXISTS /
	// NOT EXISTS legs (see compensation.go's existential handling).
	QuantifierExistential

	// QuantifierPhysical: a quantifier ranging over a Reference whose
	// member is a physical plan. Minted by PLANNING implementation rules
	// (NewPhysicalQuantifier / NamedPhysicalQuantifier) to wire plan
	// wrappers over memoized children. Distinct from ForEach under memo
	// identity: a physical wrapper and its logical twin must not dedup
	// as one member.
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
	// strictSingle marks a correlated-scalar-subquery inner quantifier that must
	// yield AT MOST ONE row per outer row: the physical join lowering wraps such an
	// inner in a strict FirstOrDefault (a second row is a 21000 cardinality
	// violation). Distinct from nullOnEmpty (LEFT-JOIN null-extension, which keeps
	// all rows) — see ImplementNestedLoopJoinRule.yieldGeneralFlatMap.
	strictSingle bool
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

// NamedForEachNullOnEmptyQuantifier builds a ForEach quantifier with an explicit
// alias AND nullOnEmpty=true. This is Java's `Quantifier.forEachWithNullOnEmpty(ref,
// alias)`: it reuses the null-supplying side's alias so the outer SelectExpression's
// existing result value (which references that alias) stays correlated correctly, while
// emitting one all-NULL row when the inner is empty. RewriteOuterJoinRule uses it to
// carry LEFT-OUTER null-extension semantics on the rewritten correlated SUBSEL.
func NamedForEachNullOnEmptyQuantifier(alias values.CorrelationIdentifier, rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:        QuantifierForEach,
		alias:       alias,
		rangesOver:  rangesOver,
		nullOnEmpty: true,
	}
}

// NamedForEachStrictSingleQuantifier builds a ForEach quantifier with an explicit
// alias AND strictSingle=true. Used for a correlated scalar subquery with no user
// LIMIT: the join lowering wraps the inner in a strict FirstOrDefault so a second
// inner row per outer row raises a cardinality violation (21000) instead of being
// silently truncated.
func NamedForEachStrictSingleQuantifier(alias values.CorrelationIdentifier, rangesOver *Reference) Quantifier {
	return Quantifier{
		kind:         QuantifierForEach,
		alias:        alias,
		rangesOver:   rangesOver,
		strictSingle: true,
	}
}

// ExistentialQuantifier builds an Existential quantifier — the inner
// expression is consulted only to determine whether at least one row
// exists. Used by EXISTS / NOT EXISTS subqueries.
//
// The flowed-object semantics differ from ForEach: an Existential
// quantifier doesn't make rows of the inner available to the outer's
// predicates / projection — only the boolean "any row exists" signal.
// Consumers that honor the distinction: compensation construction
// (existential legs stay out of result compensation, compensation.go)
// and the existential ordering-push rule
// (PushRequestedOrderingThroughSelectExistentialRule).
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
// implementation rules to point quantifiers at new child References.
// Mirrors Java's Quantifier.toBuilder().build(reference).
func RebuildQuantifier(q Quantifier, newRef *Reference) Quantifier {
	return Quantifier{
		kind:         q.kind,
		alias:        q.alias,
		nullOnEmpty:  q.nullOnEmpty,
		strictSingle: q.strictSingle,
		rangesOver:   newRef,
	}
}

// Kind returns the Quantifier's flavour.
func (q Quantifier) Kind() QuantifierKind { return q.kind }

// GetAlias returns the symbolic identifier for rows flowing along
// this Quantifier.
func (q Quantifier) GetAlias() values.CorrelationIdentifier { return q.alias }

// GetRangesOver returns the Reference holding the inner expression.
//
// Resolves through Canonical() so that if the child Reference has been
// merged away (RFC-037 cross-group merging), every consumer transparently
// sees the surviving Reference. This single accessor is the only reader of
// the raw rangesOver field, so resolving here covers all consumers without
// rewriting in-flight expressions.
func (q Quantifier) GetRangesOver() *Reference { return q.rangesOver.Canonical() }

// IsNullOnEmpty returns true for ForEach quantifiers that should
// produce a NULL row when the inner is empty (LEFT JOIN semantics).
func (q Quantifier) IsNullOnEmpty() bool { return q.nullOnEmpty }

// IsStrictSingle returns true for a correlated-scalar-subquery inner quantifier
// that must yield at most one row per outer row (a second row → 21000).
func (q Quantifier) IsStrictSingle() bool { return q.strictSingle }

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
// set.
//
// DIVERGENCE (registered in DIVERGENCES.md): Java's Quantifier
// delegates to rangesOver.getCorrelatedTo(); Go returns the EMPTY set,
// even though Reference.GetCorrelatedTo (the transitive walk) is
// implemented. The consumers compensate structurally: the IN-like
// ordering rule's containsAll guard is disabled with the downstream
// implementation rules carrying the real check
// (rule_push_requested_ordering_through_in_like_select.go), AdjustMatch
// checks node-local correlations only (rule_adjust_match.go
// correlatedToEquals), PartitionSelectRule re-exposes buried merge-leg
// deps itself (computeTransitiveCorrelationOrder), and
// SplitSelectExtractIndependentQuantifiersRule computes its own walk
// (quantifierCorrelationSet). Delegating to the Reference walk changes
// what those guards reject and is plan-shape sensitive — it needs its
// own review cycle, not a drive-by rewire.
func (q Quantifier) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}
