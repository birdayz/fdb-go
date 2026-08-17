package values

import (
	"strings"
	"testing"
)

// TestProjectionColumnName_ComposedNameIsOrdinalFree pins the naming lockstep on
// the arm that could break it. ProjectionColumnName's first two arms return
// SCHEMA text (a resolved path, a field name), which no plan-time decision
// moves; its third arm DERIVES a name from a whole Value tree, and that tree
// carries references whose ordinals are bound at plan time. Rendering that arm
// with the ordinals in it makes a column's name depend on WHEN it was derived
// relative to the bake — the exact drift ColumnNameValue exists to prevent, and
// the reason the derivation belongs to the name renderer rather than to
// EXPLAIN's.
//
// The lazy/baked pair is the assertion that detects the regression; the literal
// `want` is what keeps it from being vacuous, because a renderer that dropped
// the field names entirely would satisfy the pair perfectly.
func TestProjectionColumnName_ComposedNameIsOrdinalFree(t *testing.T) {
	t.Parallel()

	computed := func(field Value) Value {
		return &ArithmeticValue{
			Op:    OpAdd,
			Left:  field,
			Right: &ConstantValue{Value: int64(1), Typ: NotNullLong},
		}
	}
	lazy := computed(newFlatFieldValue("ID", UnknownType))
	baked := computed(newFieldValueWithResolvedOrdinal("ID", 0, UnknownType))

	const want = "(ID + 1)"
	if got := ProjectionColumnName(baked); got != want {
		t.Fatalf("ProjectionColumnName(baked composite) = %q, want %q — a plan-time ordinal reached an output column NAME", got, want)
	}
	if got := ProjectionColumnName(lazy); got != want {
		t.Fatalf("ProjectionColumnName(lazy composite) = %q, want %q", got, want)
	}
	if strings.Contains(ProjectionColumnName(baked), "#") {
		t.Fatalf("derived column name %q carries an ordinal discriminator", ProjectionColumnName(baked))
	}

	// The counterpart, so the pin above cannot be satisfied by a renderer that
	// simply lost the ability to show ordinals: EXPLAIN still shows them.
	if got := ExplainValue(baked); got != "(ID#0 + 1)" {
		t.Fatalf("ExplainValue(baked composite) = %q, want (ID#0 + 1) — the discriminator must survive in EXPLAIN", got)
	}
}

// TestProjectionColumnName_DerivedNameSurvivesReReadAsField pins the SHAPE that
// broke, not a simpler cousin of it. A derived name does not stay a label: the
// enclosing operator re-reads the column by that name, so the name becomes a
// downstream fieldValue's `Field` text — and rendering THAT field ran it through
// the escape. With an ordinal in the derived name the escape had something to
// escape, and the slot key came out as `(C1.ID##0 + 1)`.
//
// Both halves are needed to hold the property: an ordinal-free derivation (no
// `#` to escape) and an escape confined to the ordinal rendering (nothing to
// escape it WITH). Either one alone leaves the composition reachable, so both
// are driven here through one round trip.
//
// Corpus provenance: cte.yaml#25 — `WITH c2(a) AS (SELECT id + 1 FROM c1)` over
// a two-source inner CTE, the one query in the explaindiff corpus (17229 lines
// at the time of writing) whose rendering contained a doubled `#`.
func TestProjectionColumnName_DerivedNameSurvivesReReadAsField(t *testing.T) {
	t.Parallel()

	inner := &ArithmeticValue{
		Op: OpAdd,
		Left: newCorrelatedFieldValueWithResolvedOrdinal(
			mustQOV(t, NamedCorrelationIdentifier("C1")), "ID", 0, UnknownType),
		Right: &ConstantValue{Value: int64(1), Typ: NotNullLong},
	}

	derived := ProjectionColumnName(inner)
	if derived != "(C1.ID + 1)" {
		t.Fatalf("derived slot name = %q, want (C1.ID + 1)", derived)
	}

	// The enclosing projection reads that slot back by name.
	reRead := newFieldValueWithResolvedOrdinal(derived, 0, UnknownType)
	if got := ProjectionColumnName(reRead); got != derived {
		t.Fatalf("round trip changed the slot key: minted %q, re-read as %q", derived, got)
	}
	if got := ExplainValue(reRead); got != derived+"#0" {
		t.Fatalf("EXPLAIN of the re-read = %q, want %q — an escape fired on a name with nothing to disambiguate", got, derived+"#0")
	}
}

// TestFieldRendering_EscapeIsScopedToTheOrdinalForm pins the gate in BOTH
// directions, because the escape is only pointless in one of them. A field whose
// text literally contains `#` is unreachable through DDL (a struct member must
// be a valid protobuf identifier), but it is reachable through name derivation,
// and the two forms have opposite requirements: the ordinal form must stay
// INJECTIVE over (field text, ordinal) — that is what the doubling buys — while
// the name form must return the text VERBATIM, because that text is the map key
// a sibling arm (ProjectionColumnName's plain-field arm) hands back unescaped.
//
// Before the gate the two name-derivation routes disagreed on the same field.
func TestFieldRendering_EscapeIsScopedToTheOrdinalForm(t *testing.T) {
	t.Parallel()

	// `X#0` is a plain field NAME, not a read of X at slot 0.
	literal := newFieldValueWithResolvedOrdinal("X#0", 4, UnknownType)

	if got := ColumnNameValue(literal); got != "X#0" {
		t.Fatalf("ColumnNameValue = %q, want X#0 verbatim — a name is not escaped", got)
	}
	if got := ProjectionColumnName(literal); got != "X#0" {
		t.Fatalf("ProjectionColumnName(plain field) = %q, want X#0 verbatim", got)
	}
	// The two name-derivation arms must agree on the same text. The composite
	// arm reaches the same field through ColumnNameValue.
	composite := ProjectionColumnName(&ArithmeticValue{
		Op:    OpAdd,
		Left:  literal,
		Right: &ConstantValue{Value: int64(1), Typ: NotNullLong},
	})
	if composite != "(X#0 + 1)" {
		t.Fatalf("composite name arm = %q, want (X#0 + 1) — it disagreed with the plain-field arm on escaping", composite)
	}

	// The ordinal form keeps the property the doubling exists for: a rendering
	// ends in an UNPAIRED `#`+digits iff it is an ordinal read. Without it these
	// two collapse to the same text.
	nameLikeAnOrdinalRead := ExplainValue(literal)
	realOrdinalRead := ExplainValue(newFieldValueWithResolvedOrdinal("X", 0, UnknownType))
	if nameLikeAnOrdinalRead != "X##0#4" {
		t.Fatalf("ExplainValue(literal `X#0` at slot 4) = %q, want X##0#4", nameLikeAnOrdinalRead)
	}
	if realOrdinalRead != "X#0" {
		t.Fatalf("ExplainValue(X at slot 0) = %q, want X#0", realOrdinalRead)
	}
	if nameLikeAnOrdinalRead == realOrdinalRead {
		t.Fatalf("EXPLAIN collapsed a literal field name with an ordinal read, both %q", realOrdinalRead)
	}

	// Multi-accessor steps take the same gate.
	path := &fieldValue{
		Field: "C#1",
		Typ:   UnknownType,
		Resolved: &fieldPath{Accessors: []resolvedAccessor{
			{Field: "A#0", Ordinal: 2},
			{Field: "C#1", Ordinal: 3},
		}},
	}
	if got := ColumnNameValue(path); got != "A#0.C#1" {
		t.Fatalf("ColumnNameValue(multi-accessor) = %q, want A#0.C#1 verbatim", got)
	}
	if got := ExplainValue(path); got != "A##0#2.C##1#3" {
		t.Fatalf("ExplainValue(multi-accessor) = %q, want A##0#2.C##1#3", got)
	}
}

// TestExplainPlanValues_ScalarSubqueryWithoutAliasHasNoOrphanSpace pins the
// rendering of a scalar subquery under the sole-correlation collapse, where the
// alias is ABSENT rather than blank. The separator belongs to the alias.
func TestExplainPlanValues_ScalarSubqueryWithoutAliasHasNoOrphanSpace(t *testing.T) {
	t.Parallel()

	sole := &ScalarSubqueryValue{Alias: UniqueCorrelationIdentifier(), Typ: NotNullLong}
	got := ExplainPlanValues([]Value{sole})
	if len(got) != 1 {
		t.Fatalf("ExplainPlanValues returned %d renderings, want 1", len(got))
	}
	if got[0] != "(SCALAR_SUBQUERY)" {
		t.Fatalf("collapsed rendering = %q, want (SCALAR_SUBQUERY)", got[0])
	}

	// Vacuity guard: the space is not gone, it is conditional. With two unique
	// roots the collapse does not apply and both aliases render.
	pair := ExplainPlanValues([]Value{
		sole,
		&ScalarSubqueryValue{Alias: UniqueCorrelationIdentifier(), Typ: NotNullLong},
	})
	if len(pair) != 2 {
		t.Fatalf("ExplainPlanValues returned %d renderings, want 2", len(pair))
	}
	if pair[0] != "(SCALAR_SUBQUERY q$0)" || pair[1] != "(SCALAR_SUBQUERY q$1)" {
		t.Fatalf("uncollapsed renderings = %q — want the numbered roots with their separator", pair)
	}
}
