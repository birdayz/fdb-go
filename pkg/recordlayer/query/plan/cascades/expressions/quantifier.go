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
// Resolves forwarding read-only so that if the child Reference has been merged
// away (RFC-037 cross-group merging), every consumer transparently sees the
// survivor without path-compressing shared topology during a fallible read.
func (q Quantifier) GetRangesOver() *Reference {
	return canonicalReferenceReadOnly(q.rangesOver)
}

// IsNullOnEmpty returns true for ForEach quantifiers that should
// produce a NULL row when the inner is empty (LEFT JOIN semantics).
func (q Quantifier) IsNullOnEmpty() bool { return q.nullOnEmpty }

// IsStrictSingle returns true for a correlated-scalar-subquery inner quantifier
// that must yield at most one row per outer row (a second row → 21000).
func (q Quantifier) IsStrictSingle() bool { return q.strictSingle }

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

// FlowedObjectTypeUnavailableError reports that a Quantifier cannot derive the
// exact object or scalar type required by a QOV. Empty and memberless
// References are invalid quantifier inputs; they are not represented by an
// untyped placeholder value.
type FlowedObjectTypeUnavailableError struct {
	Alias  values.CorrelationIdentifier
	Reason string
}

func (e *FlowedObjectTypeUnavailableError) Error() string {
	return fmt.Sprintf("quantifier %s: flowed object type unavailable: %s", e.Alias.Name(), e.Reason)
}

// GetFlowedObjectType returns the exact object or scalar type flowing along this
// quantifier. It follows Java's Quantifier.getFlowedObjectType(): every member's
// relational result is represented as exactly RELATION<result>, that one
// wrapper is removed, and NullOnEmpty widens the object once at this edge.
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
func (q Quantifier) GetFlowedObjectType() (values.Type, error) {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil, &FlowedObjectTypeUnavailableError{Alias: q.alias, Reason: "quantifier has no Reference"}
	}
	// Java's getAllMemberExpressions() — exploratory AND final. A final member is
	// the one a physical plan is built from, so excluding it would verify the
	// agreement over exactly the members that do not end up in the plan.
	members := ref.AllMembers()
	if len(members) == 0 {
		return nil, &FlowedObjectTypeUnavailableError{Alias: q.alias, Reason: "Reference has no members"}
	}
	var found values.Type
	for _, member := range members {
		if member == nil {
			return nil, &FlowedObjectTypeUnavailableError{Alias: q.alias, Reason: "Reference contains a nil member"}
		}
		rv := member.GetResultValue()
		if rv == nil {
			return nil, &FlowedObjectTypeUnavailableError{Alias: q.alias, Reason: "member has no result Value"}
		}
		relation, err := values.ExactRelationOf(rv.Type())
		if err != nil {
			return nil, fmt.Errorf("quantifier %s member result type: %w", q.alias.Name(), err)
		}
		inner, ok := relation.RelationInner()
		if !ok {
			return nil, &FlowedObjectTypeUnavailableError{Alias: q.alias, Reason: "member result is missing its relation wrapper"}
		}
		// The member's result VALUE states the leg boundaries; the row TYPE
		// derived from it does not. Carry them, or the quantifier flows a row
		// that has forgotten where each source's columns start — which does not
		// read downstream as "no legs" but as ONE run spanning the whole concat
		// keyed by the box's rightmost leaf, so a qualified column resolves into
		// the first leg's slots. Legs are not part of exact-type identity, so
		// this adds physical information without touching the agreement check
		// below.
		rt := values.WithSeedTilingLegs(inner.Type(), rv)
		if found == nil {
			found = rt
			continue
		}
		// LEG TABLES ARE COMPARED SEPARATELY, AND BEFORE Equals, because
		// RecordType.Equals deliberately IGNORES Legs — boundaries are physical
		// information, not type identity. So equality cannot see this
		// disagreement, and two members stating the same fields under different
		// leg tables would otherwise resolve by whichever the memo scan reached
		// first. Measured, before this check existed: the same pair of members
		// flowed a 2-leg row in one insertion order and a 0-leg row in the other
		// (TestGetFlowedObjectTypeRefusesDisagreeingLegTables drives both).
		//
		// AN EMPTY TABLE IS AN UNSTATED GAP, NOT A STATEMENT — so a POPULATED
		// table wins over an empty one, and only two DIFFERENT populated tables
		// conflict. That ruling is deliberate and it is the opposite of the one
		// `legTablesAgree` alone implements; both are defensible, so here is why
		// this is the one.
		//
		// The defect being fixed is that the answer DEPENDED ON INSERTION ORDER:
		// the same two members flowed a 2-leg row when the tiling one was
		// reached first and a 0-leg row when it was not. Either ruling removes
		// that, because both are order-independent. What separates them is the
		// cost of being wrong. Treating empty as a STATEMENT declines the
		// quantifier outright, and measured over this tree that rejected real,
		// previously-planning shapes — every correlated-EXISTS-over-a-derived-
		// source plan among them — because in practice the empty side is a
		// producer that never DERIVED boundaries, not one asserting it has none.
		// Trading a wrong row for a lost plan is not an improvement.
		//
		// Taking the populated table is also strictly more information rather
		// than a guess: SeedTilingLegs only yields a table that tiles the row
		// EXACTLY, so the boundaries it states are consistent with the field
		// list both members already agree on. And it is the safe direction —
		// a row with no boundaries does not read downstream as "no legs", it
		// reads as ONE run spanning the whole concat keyed by the box's
		// rightmost leaf, so an alias-qualified column resolves at
		// runOffset+ordinal, inside the FIRST leg. Empty is the shape that
		// produces the wrong slot; adopting the stated boundaries removes it.
		foundLegs, rtLegs := rowTypeLegsOf(found), rowTypeLegsOf(rt)
		switch {
		case len(foundLegs) == 0:
			// Adopt rt's boundaries (possibly also empty) — see above.
			found = values.WithRecordTypeLegs(found, rtLegs)
		case len(rtLegs) == 0:
			// Keep what `found` already states.
		case !legTablesAgree(foundLegs, rtLegs):
			return nil, &MemberResultTypeDisagreementError{
				Alias: q.alias, Left: found, Right: rt,
			}
		}
		if !found.Equals(rt) {
			return nil, &MemberResultTypeDisagreementError{
				Alias: q.alias, Left: found, Right: rt,
			}
		}
	}
	if found == nil {
		return nil, &FlowedObjectTypeUnavailableError{Alias: q.alias, Reason: "Reference has no usable members"}
	}
	if q.nullOnEmpty {
		found = values.WithNullability(found, true)
		if _, err := values.SnapshotExactType(found); err != nil {
			return nil, fmt.Errorf("quantifier %s NullOnEmpty flowed type: %w", q.alias.Name(), err)
		}
	}
	return found, nil
}

// NOT ON THE LIVE PATH. GetFlowedObjectType compares member rows with strict
// Equals plus the leg-table rule above; nothing in production calls this, and
// its only callers are in leg_table_population_blast_radius_test.go. Two things
// follow and both matter to anyone reading it as protection:
//
//   - its populated-vs-empty ruling is the OPPOSITE of the live one. This
//     declines that pair; the live scan adopts the stated boundaries. The
//     reasoning for the live choice is at the call site.
//   - the UNSTATED/stated refinement below (UNKNOWN cannot contradict a stated
//     type) is not applied by the live scan either, which requires members to
//     be exactly typed.
//
// It is kept only because the blast-radius tests still document why populating
// a leg table is not behaviour-neutral. Deleting it, and repointing those tests
// at the live path, is tracked in TODO.md.
//
// refineRowTypes reduces two members' row types to the single row the quantifier
// flows — Java's `Verify.verify(left.equals(right))` reduction
// (Reference.java:504-513), corrected for the one thing Go has that Java does
// not: a type that means "not inferred yet".
//
// Java can use bare equality because every Java Value carries a resolved type, so
// two members of one equivalence class either describe the same row or the memo
// is broken. Go's UnknownType is neither — it is the ABSENCE of a stated type,
// and the same row reached by two rules routinely arrives with different amounts
// of it resolved (an unnest element inferred as INT down one path and left
// UNKNOWN down the other). Comparing those with Equals reports a disagreement
// between a row and ITSELF, and the caller then declines work it should have
// done: a bipartition that yields nothing, a merge slot left untyped.
//
// So an unstated field cannot contradict a stated one — the same rule this
// function's member scan already applies to a member that states no row type at
// all, one level further down. Everything else stays a real disagreement:
// different field counts, names, ordinals, nullability, or two fields that both
// state a type and state different ones. Those are the memo defects the
// verification exists to catch, and they still surface as errors.
//
// The RECORD NAME follows the unstated rule rather than the strict one, because
// Go has an "unstated" spelling for it too and it is documented as such: the empty
// string means ANONYMOUS (RecordType.RecordName's own doc), which is the ordinary
// state of a projection result row that has not been bound to a named struct. Go's
// own type merge already treats it that way — MaximumType takes the other side's
// name when one is empty (values/type.go) — so treating "" as a conflict here
// would make this function disagree with the type system it is reducing over, and
// disagree in the direction that costs a plan: two members of one class, one of
// them anonymous because inference had no name to give it, reported as flowing
// different rows. Two members that both STATE a name and state different ones is a
// genuine conflict and still errors.
//
// The result is the more RESOLVED row, so a later member cannot un-resolve what
// an earlier one established — the record name included.
//
// The LEG TABLE (RecordType.Legs) is carried through, and it is checked BEFORE
// the equality fast path rather than after. Both halves matter. Carrying it
// matters because the merged row is what the leg-layout derivation hands to
// addBuriedLegLayouts and what the translator's `len(rt.Legs)` gate and the
// seed-window authority read: a merge that rebuilt the row without Legs would
// silently strip the buried-leg boundary table, which is the same defect
// WithNullability's comment warns about on the nullability flip (values/type.go).
// Checking it before the fast path matters because RecordType.Equals IGNORES Legs
// — deliberately, Legs carries no identity semantics — so two members stating the
// SAME fields under DIFFERENT leg tables satisfy Equals, and the fast path would
// then resolve a boundary-table conflict by returning whichever member the memo
// scan reached first. That is exactly the pick-by-insertion-order this whole
// verification exists to refuse.
//
// The leg table is not subject to the unstated/stated rule the FIELD types are.
// An unstated field type means "inference has not reached here"; an EMPTY leg
// table means "this row has no buried-leg boundaries", which is a statement about
// the row's structure, not a gap in inference. So two members must carry
// IDENTICAL tables, and a mismatch — including one member stating boundaries the
// other denies — is a genuine conflict and surfaces as one.
func refineRowTypes(a, b *values.RecordType) (*values.RecordType, bool) {
	if a == nil || b == nil {
		return nil, false
	}
	if !legTablesAgree(a.Legs, b.Legs) {
		return nil, false
	}
	if a.Equals(b) {
		return a, true
	}
	name, nameAgrees := refineRecordNames(a.RecordName, b.RecordName)
	if !nameAgrees || a.Nullable != b.Nullable || len(a.Fields) != len(b.Fields) {
		return nil, false
	}
	merged := make([]values.Field, len(a.Fields))
	for i := range a.Fields {
		af, bf := a.Fields[i], b.Fields[i]
		if af.Name != bf.Name || af.Ordinal != bf.Ordinal {
			return nil, false
		}
		ft, agree := refineFieldTypes(af.FieldType, bf.FieldType)
		if !agree {
			return nil, false
		}
		merged[i] = values.Field{Name: af.Name, FieldType: ft, Ordinal: af.Ordinal}
	}
	return &values.RecordType{RecordName: name, Nullable: a.Nullable, Fields: merged, Legs: a.Legs}, true
}

// refineRecordNames is the unstated/stated rule for the record NAME: "" is
// anonymous — the absence of a name, not a name of its own — so it takes the
// other side's, and two stated-and-different names are a conflict.
func refineRecordNames(a, b string) (string, bool) {
	switch {
	case a == "":
		return b, true
	case b == "" || a == b:
		return a, true
	}
	return "", false
}

// rowTypeLegsOf returns the leg table a flowed row states, or nil for any type
// that is not a record. A non-record row has no boundaries to disagree about,
// so two of them agree trivially.
func rowTypeLegsOf(t values.Type) []values.RecordTypeLeg {
	rt, ok := t.(*values.RecordType)
	if !ok || rt == nil {
		return nil
	}
	return rt.Legs
}

// legTablesAgree reports whether two members state the SAME buried-leg boundary
// table. nil and empty are the same statement ("no boundaries"); anything else is
// compared element-wise on all four fields, because a leg differing only in Start
// or Width relocates every buried read filed against it.
func legTablesAgree(a, b []values.RecordTypeLeg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// refineFieldTypes is refineRowTypes for one field: the more resolved of two
// types, or (nil, false) when both are stated and they differ.
//
// It recurses into every type that CARRIES another type, because the rule that
// makes the recursion necessary does not care about depth or container: an
// unstated type states nothing, so it cannot contradict a stated one, and a row
// whose only difference is an unresolved type two levels down is the same row for
// exactly the reason it is one level down. Stopping at RECORD only was an
// accident of which container the first misreport happened to be found in.
//
//   - RECORD recurses field-wise (refineRowTypes).
//   - ARRAY recurses into the element. An array's ElementType is nil while
//     inference has not filled it in — the type's own doc says so — and Go does
//     not infer array element types far, so `ARRAY<?>` against `ARRAY<INT>` is
//     the single commonest shape this rule exists for. Nullability is compared
//     strictly, as it is on a row.
//   - RELATION recurses into the inner row. An ERASED relation (nil inner) is the
//     unstated case and refines to the stated one.
//
// ENUM deliberately does NOT recurse, and the decision is worth stating because
// it looks like an omission next to RECORD's anonymous name. An enum's content is
// a DECLARED value list: there is no "not inferred yet" enum member the way there
// is an unresolved field type. Its EnumName can be empty, but the type's own doc
// calls that an anonymous enum — "rare in real schemas but legal" — a legal schema
// state rather than an unfilled inference slot, which is the exact opposite of
// RecordName's "not bound to a named struct yet". Go's own type merge declines
// anonymous-enum handling for the same reason (values/type.go). So two enums
// either state the same type or they state different ones, and Equals above
// already decides that. Pinned by
// TestGetFlowedObjectType_AnAnonymousEnumIsNotUnstated so this stays a decision.
func refineFieldTypes(a, b values.Type) (values.Type, bool) {
	switch {
	case isUnstatedType(a):
		return b, true
	case isUnstatedType(b):
		return a, true
	case a.Equals(b):
		return a, true
	}
	switch at := a.(type) {
	case *values.RecordType:
		if bt, ok := b.(*values.RecordType); ok {
			return refineRowTypes(at, bt)
		}
	case *values.ArrayType:
		bt, ok := b.(*values.ArrayType)
		if !ok || at.Nullable != bt.Nullable {
			return nil, false
		}
		elem, agree := refineFieldTypes(at.ElementType, bt.ElementType)
		if !agree {
			return nil, false
		}
		return &values.ArrayType{Nullable: at.Nullable, ElementType: elem}, true
	case *values.RelationType:
		bt, ok := b.(*values.RelationType)
		if !ok {
			return nil, false
		}
		inner, agree := refineFieldTypes(at.InnerType, bt.InnerType)
		if !agree {
			return nil, false
		}
		return &values.RelationType{InnerType: inner}, true
	}
	return nil, false
}

// isUnstatedType reports whether t carries no type information — Go's
// "inference has not reached here", which Java has no counterpart for.
//
// nil is included because the containers spell the gap that way: an ArrayType
// whose element inference has not reached carries a nil ElementType, and an
// ERASED RelationType carries a nil InnerType. Both mean the same thing
// UnknownType means in a record field.
func isUnstatedType(t values.Type) bool {
	return t == nil || t.Code() == values.TypeCodeUnknown
}

// RequireFlowedObjectValue constructs the only legal QOV for this edge. There
// is no reporting-only or UnknownType fallback: absence, invalidity, and member
// disagreement are returned before a Value is published.
func (q Quantifier) RequireFlowedObjectValue() (values.QuantifiedObjectValue, error) {
	flowedType, err := q.GetFlowedObjectType()
	if err != nil {
		return nil, err
	}
	qov, err := values.NewQuantifiedObjectValue(q.alias, flowedType)
	if err != nil {
		return nil, fmt.Errorf("quantifier %s flowed QOV: %w", q.alias.Name(), err)
	}
	return qov, nil
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
