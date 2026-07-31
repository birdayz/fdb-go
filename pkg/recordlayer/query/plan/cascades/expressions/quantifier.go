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
// Java's `Quantifier.getFlowedObjectValue()` verbatim (Quantifier.java:801-803):
// `QuantifiedObjectValue.of(getAlias(), getFlowedObjectType())`, i.e. ALWAYS
// carrying the row type when the ranged-over reference states one.
//
// It used to mint the alias with no type, and the reason recorded for that was
// that typing it "changes expression identity across the whole planner". That is
// measured FALSE and pinned: a QuantifiedObjectValue's identity is its
// CORRELATION in all three paths that decide it — EqualsWithoutChildren,
// SemanticEqualsUnderAliasMap, and SemanticHashCode, which folds the tag "qov"
// with the alias excluded. Typing one changes what it SAYS, never which
// expression it IS. (TestTypingAQuantifiedObjectValueDoesNotChangeItsIdentity.)
//
// On a member DISAGREEMENT this returns the alias with no type, because it has no
// error channel to report one through. That is NOT the collapse
// GetFlowedObjectValueTyped's doc forbids: a caller that BAKES AN ORDINAL against
// this row must use that accessor and refuse to proceed, because for it a row
// shape chosen by memo insertion order is a wrong-slot read. A caller merely
// reporting what flows loses type information it never had.
func (q Quantifier) GetFlowedObjectValue() values.Value {
	rt, err := q.GetFlowedObjectType()
	if err != nil || rt == nil {
		return values.NewQuantifiedObjectValue(q.alias)
	}
	return values.NewQuantifiedObjectValueOfType(q.alias, rt)
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
			found = rt
			continue
		}
		refined, agree := refineRowTypes(found, rt)
		if !agree {
			// The ACCUMULATED row on the left, not the first typed member's. Those
			// differ from the third member onwards, and the error used to report the
			// first — a row that was not what the failing comparison saw. On the shape
			// this reduction exists for (one member unresolved, a later one resolving
			// it, a third conflicting) the reported left-hand row still carried the
			// UNKNOWN the second member had already resolved, so the message described
			// a conflict nobody had, and the real one was invisible. Both sides are the
			// unwrapped ROW types, because those are what refineRowTypes compared.
			return nil, &MemberResultTypeDisagreementError{
				Alias: q.alias, Left: found, Right: rt,
			}
		}
		found = refined
	}
	return found, nil
}

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

// GetFlowedObjectValueTyped is GetFlowedObjectValue with the member DISAGREEMENT
// surfaced instead of swallowed. Both accessors type the value; they differ only
// in what they do when the reference's members cannot agree on the row they flow.
//
// So the choice between them is NOT "do I want the type" — it is "can I proceed
// without one". Callers that BAKE ORDINALS against the flowed row use this and
// refuse to proceed on the error, because for them an invented row shape is a
// wrong-slot read that no test can predict; an untyped QOV degrades a reference
// to source-relative, and a source-relative operand pushed into a scan evaluates
// to NULL against the build-bound row, so the join returns zero rows with no
// error. Callers merely reporting what flows take GetFlowedObjectValue and lose
// type information they never had.
//
// The error is the DISAGREEMENT only (see MemberResultTypeDisagreementError),
// never the ordinary "no type yet": that returns the untyped QOV and a nil error.
// A caller must not collapse the two — falling back to the untyped value on a
// disagreement is choosing a row shape by memo insertion order.
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
