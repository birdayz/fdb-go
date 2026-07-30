package expressions

import (
	"fmt"

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
func (q Quantifier) GetRangesOver() *Reference {
	if q.rangesOver == nil {
		return nil
	}
	return q.rangesOver.Canonical()
}

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

// MemberResultTypeDisagreementError reports that a Reference's members do not
// agree on their result type, so no member is authoritative for the quantifier's
// flowed row.
//
// Java's counterpart is a Verify failure (Reference.java:504-513 reduces the
// members' result types with `Verify.verify(left.equals(right))`), i.e. a crash.
// Go returns it as an error so the caller decides, but the caller must NOT treat
// it as "type unavailable" and fall back to an untyped row: an untyped merge slot
// leaves a reference source-relative, a source-relative operand pushed into a
// leg's scan evaluates to NULL against the build-bound row, and the join returns
// zero rows with no error. A disagreement is a memo defect — two members of one
// equivalence class flowing different row shapes — and it must surface as one.
type MemberResultTypeDisagreementError struct {
	Alias values.CorrelationIdentifier
	Left  values.Type
	Right values.Type
}

func (e *MemberResultTypeDisagreementError) Error() string {
	return fmt.Sprintf("quantifier %s: reference members disagree on result type (%v vs %v)",
		e.Alias.Name(), e.Left, e.Right)
}

// GetFlowedObjectType returns the ROW type of the rows flowing along this
// quantifier — Java's Quantifier.getFlowedObjectType() (Quantifier.java:806-810:
// the ranged-over Reference's result type, unwrapped from its RELATION wrapper).
//
// Java resolves it from the Reference, whose getResultType() REDUCES over every
// member expression and verifies each pair agrees (Reference.java:504-513). That
// verification is the reason "any member is authoritative" is a sound step, so it
// is ported rather than cited: every member's row type is compared, and a
// disagreement is returned as an error instead of silently taking members[0].
// Taking the first member on a disagreement would pick a row shape by memo
// insertion order, which is exactly the kind of choice that produces a
// wrong-slot read that no test can predict.
//
// (nil, nil) when there is nothing to report: no reference, no member, or a
// result value whose type is not a row type. Java Verify-fails on the last case
// because every Java expression carries a typed result value; Go's logical
// expressions do not all reach that yet, so callers treat nil as "type
// unavailable" and keep the untyped QOV they used before. That is a REPORTING
// gap, never a substitute — a caller that needs the type to bake an ordinal must
// not invent one.
func (q Quantifier) GetFlowedObjectType() (*values.RecordType, error) {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil, nil
	}
	// Java's getAllMemberExpressions() — exploratory AND final. A final member is
	// the one a physical plan is built from, so excluding it would verify the
	// agreement over exactly the members that do not end up in the plan.
	members := ref.AllMembers()
	if len(members) == 0 {
		return nil, nil
	}
	var found *values.RecordType
	var foundRaw values.Type
	for _, member := range members {
		if member == nil {
			continue
		}
		rv := member.GetResultValue()
		if rv == nil {
			continue
		}
		rt := rowTypeOf(rv.Type())
		if rt == nil {
			// An untyped member cannot contradict a typed one — it reports nothing.
			// Java has no such member; Go's logical expressions do, and the reporting
			// gap is documented above.
			continue
		}
		if found == nil {
			found, foundRaw = rt, rv.Type()
			continue
		}
		if !found.Equals(rt) {
			return nil, &MemberResultTypeDisagreementError{
				Alias: q.alias, Left: foundRaw, Right: rv.Type(),
			}
		}
	}
	return found, nil
}

// rowTypeOf unwraps a member's result type to its ROW type, through the RELATION
// wrapper a relational expression's result value carries. nil when it is neither.
func rowTypeOf(t values.Type) *values.RecordType {
	switch t := t.(type) {
	case *values.RecordType:
		return t
	case *values.RelationType:
		if rt, isRT := t.InnerType.(*values.RecordType); isRT {
			return rt
		}
	}
	return nil
}

// GetFlowedObjectValueTyped is GetFlowedObjectValue carrying the quantifier's
// flowed ROW type when that type is resolvable — Java's getFlowedObjectValue()
// exactly (`QuantifiedObjectValue.of(getAlias(), getFlowedObjectType())`,
// Quantifier.java:801-803), which is ALWAYS typed.
//
// It is a separate accessor rather than a change to GetFlowedObjectValue
// because the untyped form is what ~40 GetResultValue() implementations return
// and what the memo has interned on; typing every one of them at once changes
// expression identity across the whole planner. Callers that BAKE ORDINALS
// against the flowed row — the ones for which an untyped QOV silently degrades
// a reference to source-relative and then to NULL at runtime — use this.
//
// The error is the member DISAGREEMENT (see MemberResultTypeDisagreementError),
// never the ordinary "no type yet": that returns the untyped QOV and a nil error,
// as before. A caller must not collapse the two — falling back to the untyped
// value on a disagreement is choosing a row shape by memo insertion order.
func (q Quantifier) GetFlowedObjectValueTyped() (values.Value, error) {
	rt, err := q.GetFlowedObjectType()
	if err != nil {
		return nil, err
	}
	if rt != nil {
		return values.NewQuantifiedObjectValueOfType(q.alias, rt), nil
	}
	return values.NewQuantifiedObjectValue(q.alias), nil
}

// GetCorrelatedTo returns the set of CorrelationIdentifiers the inner
// expression depends on — the Quantifier's transitive correlation set,
// delegating to the ranged-over Reference exactly as Java's
// Quantifier.getCorrelatedTo() delegates to getRangesOver().getCorrelatedTo().
// (Reference.GetCorrelatedTo already excludes each member's own bound
// quantifier aliases; q.alias is bound at the PARENT, not inside the
// ranged-over reference, so it is not in this set.)
func (q Quantifier) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	if ref := q.GetRangesOver(); ref != nil {
		return ref.GetCorrelatedTo()
	}
	return map[values.CorrelationIdentifier]struct{}{}
}
