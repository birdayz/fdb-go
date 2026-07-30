package expressions

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// rowOfTypes builds a row from explicit (name, type) pairs, so a fixture can
// state that two members resolved DIFFERENT AMOUNTS of the same row.
func rowOfTypes(pairs ...any) *values.RecordType {
	fields := make([]values.Field, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		fields = append(fields, values.Field{
			Name:      pairs[i].(string),
			FieldType: pairs[i+1].(values.Type),
			Ordinal:   len(fields),
		})
	}
	return &values.RecordType{Nullable: true, Fields: fields}
}

func flowedTypeOf(t *testing.T, members ...RelationalExpression) (*values.RecordType, error) {
	t.Helper()
	if len(members) == 0 {
		t.Fatal("fixture: no members")
	}
	ref := InitialOf(members[0])
	for _, m := range members[1:] {
		ref.Insert(m)
	}
	if len(ref.AllMembers()) != len(members) {
		t.Fatalf("fixture: reference holds %d members, want %d — the members interned "+
			"together and there is no disagreement left to resolve",
			len(ref.AllMembers()), len(members))
	}
	return NamedForEachQuantifier(values.NamedCorrelationIdentifier("Q"), ref).GetFlowedObjectType()
}

// TestGetFlowedObjectType_AnUnresolvedFieldIsNotADisagreement pins the one place
// Java's member-agreement reduction cannot be ported as literal equality.
//
// Java reduces the members' result types with `Verify.verify(left.equals(right))`
// (Reference.java:504-513) and that is sound there because every Java Value
// carries a resolved type: two members of one equivalence class either describe
// the same row or the memo is broken. Go has a third state — UnknownType, "not
// inferred yet" — and the SAME row reached by two different rules routinely
// arrives with different amounts of it resolved. Literal equality then reports a
// disagreement between a row and itself.
//
// That is not a cosmetic misreport. The callers treat a disagreement as a reason
// to STOP: PartitionSelectRule's Case-2 declines the bipartition outright, and a
// declined bipartition on a multi-EXISTS shape leaves a SelectExpression with no
// physical member at all — measured as `best expression is not a physical plan:
// *expressions.LogicalProjectionExpression` on
// `SELECT "X" FROM A, B, A."ARR" AS "X" WHERE EXISTS (…) AND EXISTS (…)`, where
// the two members differed in exactly one field: the unnest element, `X INT` down
// one path and `X UNKNOWN` down the other.
func TestGetFlowedObjectType_AnUnresolvedFieldIsNotADisagreement(t *testing.T) {
	t.Parallel()

	resolved := rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong)
	unresolved := rowOfTypes("A", values.NotNullLong, "X", values.UnknownType)

	// BOTH ORDERS. The refinement must not depend on which member the scan reaches
	// first, or "the resolved row wins" degrades into "the first row wins" — memo
	// insertion order deciding a row shape, which is the exact choice this
	// verification exists to refuse.
	for _, tc := range []struct {
		name     string
		a, b     *values.RecordType
		wantSame *values.RecordType
	}{
		{"unresolved first", unresolved, resolved, resolved},
		{"resolved first", resolved, unresolved, resolved},
	} {
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "m1", typ: tc.a},
			&typedStubExpr{name: "m2", typ: tc.b})
		if err != nil {
			t.Errorf("%s: members differing only in an UNRESOLVED field reported a "+
				"disagreement: %v\n"+
				"  An unstated type states nothing, so it cannot contradict a stated one —\n"+
				"  the same rule the member scan already applies to a member carrying no row\n"+
				"  type at all. Callers treat the error as a STOP, so this misreport costs a\n"+
				"  plan, not a log line.", tc.name, err)
			continue
		}
		if got == nil || !got.Equals(tc.wantSame) {
			t.Errorf("%s: resolved to %v, want the more RESOLVED row %v — a later member "+
				"must not un-resolve what an earlier one established",
				tc.name, got, tc.wantSame)
		}
	}
}

// TestGetFlowedObjectType_StillCatchesRealDisagreements is the other direction,
// and it is the one that keeps the fix above from being "never report anything".
// A genuine disagreement is still a memo defect and must still surface: two
// members of one equivalence class flowing different rows means some caller is
// about to pick a row shape by insertion order.
func TestGetFlowedObjectType_StillCatchesRealDisagreements(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		a, b *values.RecordType
	}{
		{
			// Both STATE a type for X, and they state different ones.
			"conflicting stated field types",
			rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong),
			rowOfTypes("A", values.NotNullLong, "X", values.NotNullString),
		},
		{
			"different field count",
			rowOfTypes("A", values.NotNullLong),
			rowOfTypes("A", values.NotNullLong, "B", values.UnknownType),
		},
		{
			"different field names",
			rowOfTypes("A", values.UnknownType),
			rowOfTypes("B", values.UnknownType),
		},
		{
			// A nested row that disagrees on a STATED leaf. Recursion must not
			// turn "descend into records" into "records always agree".
			"nested record with a conflicting stated leaf",
			rowOfTypes("A", rowOfTypes("N", values.NotNullLong)),
			rowOfTypes("A", rowOfTypes("N", values.NotNullString)),
		},
	} {
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "d1", typ: tc.a},
			&typedStubExpr{name: "d2", typ: tc.b})
		var de *MemberResultTypeDisagreementError
		if !errors.As(err, &de) {
			t.Errorf("%s: resolved to (%v, %v), want a disagreement — two members of one "+
				"equivalence class flowing different rows is a memo defect, and swallowing "+
				"it hands the next caller a row shape chosen by insertion order",
				tc.name, got, err)
		}
	}
}

// rawTypedValue reports a Type VERBATIM. Every stock Value that carries a type
// annotation normalises it — QuantifiedObjectValue.Type and NullValue.Type both
// run it through WithNullability(_, true), and RecordConstructorValue.Type builds
// a fresh nullable row — so a member built from any of them can never state a
// NOT-NULL row, and the nullability axis of the refinement is unreachable through
// them.
//
// That is a fact about the fixtures, not about production: a member's result value
// is any Value, and the ones that report a row type verbatim are what reach the
// guard. Stating the row exactly is the only way a test can vary the axis.
type rawTypedValue struct{ typ values.Type }

func (v *rawTypedValue) Children() []values.Value { return []values.Value{} }
func (v *rawTypedValue) Type() values.Type        { return v.typ }
func (*rawTypedValue) Name() string               { return "rawtyped" }
func (*rawTypedValue) Evaluate(any) (any, error)  { return nil, nil }

// rawTypedExpr is typedStubExpr whose result value states its row verbatim.
type rawTypedExpr struct {
	name string
	typ  *values.RecordType
}

func (s *rawTypedExpr) GetResultValue() values.Value    { return &rawTypedValue{typ: s.typ} }
func (s *rawTypedExpr) GetQuantifiers() []Quantifier    { return nil }
func (s *rawTypedExpr) CanCorrelate() bool              { return false }
func (s *rawTypedExpr) ChildrenAsSet() bool             { return false }
func (s *rawTypedExpr) HashCodeWithoutChildren() uint64 { return 0 }
func (s *rawTypedExpr) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (s *rawTypedExpr) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*rawTypedExpr)
	return ok && o.name == s.name
}
func (s *rawTypedExpr) WithQuantifiers(_ []Quantifier) RelationalExpression { return s }

// TestGetFlowedObjectType_TheUntestedGuardsAreLoadBearing pins the guards
// refineRowTypes carries that no fixture could reach, because rowOfTypes builds
// every row the same way: Nullable true, RecordName empty, Ordinal equal to the
// slice index. Three axes hardcoded means three guards that could be deleted with
// the suite green.
//
// That is not a theoretical gap. Deleting the nullability guard and deleting the
// ordinal guard each left every refinement test passing — the disagreement cases
// were all reachable through the field-type and field-name arms, which are the
// two axes the fixture DOES vary. A guard nothing can reach is a comment.
func TestGetFlowedObjectType_TheUntestedGuardsAreLoadBearing(t *testing.T) {
	t.Parallel()

	// Same fields, opposite nullability. A row's nullability decides whether a
	// LEFT-JOIN null-extended row is representable in it, so two members
	// disagreeing about it are describing different rows.
	notNullRow := rowOfTypes("A", values.NotNullLong)
	notNullRow.Nullable = false
	nullableRow := rowOfTypes("A", values.NotNullLong)

	// Same names and types, DIFFERENT ordinals. The ordinal is the slot an
	// ordinal-baked reference indexes; two members that put the same column at
	// different slots agree on nothing that matters.
	slot0 := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 0},
	}}
	slot7 := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: "A", FieldType: values.NotNullLong, Ordinal: 7},
	}}

	// Two members that both STATE a record name, and state different ones.
	namedT := rowOfTypes("A", values.NotNullLong)
	namedT.RecordName = "T"
	namedU := rowOfTypes("A", values.NotNullLong)
	namedU.RecordName = "U"

	for _, tc := range []struct {
		name string
		why  string
		a, b *values.RecordType
	}{
		{
			"nullability mismatch", "a row that admits NULL and one that does not are " +
				"different rows; the LEFT-JOIN null-supplying wrap is exactly where the flip " +
				"happens",
			notNullRow, nullableRow,
		},
		{
			"ordinal mismatch", "the ordinal is the slot a baked reference indexes, so two " +
				"members placing one column at different slots disagree about every read " +
				"through it",
			slot0, slot7,
		},
		{
			"two STATED and different record names", "an empty name is anonymous and " +
				"refines; two stated names that differ are a genuine conflict, and collapsing " +
				"the two cases is how the unstated rule turns into no rule",
			namedT, namedU,
		},
	} {
		// The fixture must actually differ on its axis, or the case is vacuous for
		// the same reason the guard was.
		if tc.a.Equals(tc.b) {
			t.Fatalf("fixture %s: the two rows are Equals, so nothing distinguishes them "+
				"and this case cannot see the guard", tc.name)
		}
		got, err := flowedTypeOf(t,
			&rawTypedExpr{name: "u1", typ: tc.a},
			&rawTypedExpr{name: "u2", typ: tc.b})
		var de *MemberResultTypeDisagreementError
		if !errors.As(err, &de) {
			t.Errorf("%s: resolved to (%v, %v), want a disagreement — %s",
				tc.name, got, err, tc.why)
		}
	}
}

// TestGetFlowedObjectType_AnAnonymousRecordNameRefines is the other side of the
// record-name guard, and it is the one that was WRONG.
//
// An empty RecordName means ANONYMOUS — the absence of a name, documented as such
// on the field itself, and the ordinary state of a projection result row that was
// never bound to a named struct. Go's own type merge already treats it that way
// (MaximumType takes the other side's name when one is empty). Comparing it
// strictly here made this function disagree with the type system it reduces over,
// and disagree in the direction that costs a plan: two members of one class, one
// anonymous only because inference had no name to give it, reported as flowing
// different rows — the same misreport the UnknownType arm exists to prevent, one
// field up.
func TestGetFlowedObjectType_AnAnonymousRecordNameRefines(t *testing.T) {
	t.Parallel()

	anonymous := rowOfTypes("A", values.NotNullLong, "X", values.UnknownType)
	named := rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong)
	named.RecordName = "T"

	// BOTH ORDERS, for the reason the unresolved-field test states: "the named row
	// wins" must not decay into "the first row wins".
	for _, tc := range []struct {
		name string
		a, b *values.RecordType
	}{
		{"anonymous first", anonymous, named},
		{"named first", named, anonymous},
	} {
		got, err := flowedTypeOf(t,
			&rawTypedExpr{name: "a1", typ: tc.a},
			&rawTypedExpr{name: "a2", typ: tc.b})
		if err != nil || got == nil {
			t.Errorf("%s: an ANONYMOUS row and a NAMED one reported (%v, %v).\n"+
				"  \"\" is the absence of a record name, not a name — the field's own doc says\n"+
				"  so and MaximumType already refines it that way. Reporting it as a conflict\n"+
				"  makes this reduction disagree with the type system it reduces over, and the\n"+
				"  caller treats a disagreement as a STOP.", tc.name, got, err)
			continue
		}
		if got.RecordName != "T" {
			t.Errorf("%s: the merged row is named %q, want the STATED name \"T\" — a later "+
				"member must not un-resolve the name an earlier one established",
				tc.name, got.RecordName)
		}
	}
}

// withLegs returns rt carrying the given buried-leg boundary table.
func withLegs(rt *values.RecordType, legs ...values.RecordTypeLeg) *values.RecordType {
	out := *rt
	out.Legs = legs
	return &out
}

// legOf is a boundary entry whose four fields are all stated, so a fixture can
// vary exactly one of them.
func legOf(name string, start, width int) values.RecordTypeLeg {
	return values.NewRecordTypeLeg(values.NamedCorrelationIdentifier(name), name, start, width)
}

// TestGetFlowedObjectType_CarriesTheBuriedLegTable pins that the member
// refinement does not drop RecordType.Legs on its way through.
//
// The merged row this returns is what the leg-layout derivation hands to
// addBuriedLegLayouts, what the translator's `len(rt.Legs)` gate reads, and what
// the seed-window authority walks in finalizeSeedWindows. All three get NOTHING
// from a row rebuilt field-by-field without its leg table — and none of them
// fails loudly on an empty one: a buried source simply stops having a window, and
// the read filed against it falls back to the qualified NAME channel. So the
// symptom is a silent return to the channel RFC-197 exists to remove, on exactly
// the clustered-box shapes the boundary table was added for.
//
// This is the same defect values.WithNullability's comment already warns about on
// the nullability flip; the refinement is the second rebuild of a RecordType on
// this path and it had the identical hole.
func TestGetFlowedObjectType_CarriesTheBuriedLegTable(t *testing.T) {
	t.Parallel()

	table := []values.RecordTypeLeg{legOf("L", 0, 2)}
	resolved := withLegs(rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong), table...)
	unresolved := withLegs(rowOfTypes("A", values.NotNullLong, "X", values.UnknownType), table...)

	// Both orders, and both must take the SLOW path: the two rows differ in a
	// field, so the Equals fast path cannot return one of them verbatim and the
	// rebuild is what has to carry the table.
	for _, tc := range []struct {
		name string
		a, b *values.RecordType
	}{
		{"unresolved first", unresolved, resolved},
		{"resolved first", resolved, unresolved},
	} {
		if tc.a.Equals(tc.b) {
			t.Fatalf("fixture %s: the two members are Equals, so the fast path returns one "+
				"verbatim and this test cannot see whether the REBUILD carries the table",
				tc.name)
		}
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "g1", typ: tc.a},
			&typedStubExpr{name: "g2", typ: tc.b})
		if err != nil || got == nil {
			t.Fatalf("%s: two members carrying the SAME leg table reported (%v, %v)",
				tc.name, got, err)
		}
		if !legTablesAgree(got.Legs, table) {
			t.Errorf("%s: the merged row carries Legs %v, want the members' own table %v.\n"+
				"  The refinement rebuilt the row without its buried-leg boundaries, so every\n"+
				"  consumer of them — addBuriedLegLayouts, the translator's len(rt.Legs) gate,\n"+
				"  finalizeSeedWindows — sees a row with no buried sources and the reads filed\n"+
				"  against them fall back to the qualified NAME channel, silently.",
				tc.name, got.Legs, table)
		}
	}
}

// TestGetFlowedObjectType_DifferentLegTablesAreADisagreement is the other
// direction of the same fix, and it is the one the Equals FAST PATH swallowed.
//
// RecordType.Equals ignores Legs on purpose — the table is layout metadata and
// carries no identity semantics. So two members stating identical FIELDS under
// DIFFERENT boundary tables are Equals, the fast path returned whichever member
// the memo scan reached first, and the buried windows of the losing member simply
// vanished. That is a row layout picked by insertion order, which is the precise
// thing this whole member verification exists to refuse.
//
// The table is not subject to the unstated/stated rule the field types are: an
// EMPTY table says "this row has no buried-leg boundaries", a statement about the
// row's structure rather than a gap in inference. So the empty-vs-stated pair is
// a disagreement too, and it is included.
func TestGetFlowedObjectType_DifferentLegTablesAreADisagreement(t *testing.T) {
	t.Parallel()

	base := rowOfTypes("A", values.NotNullLong, "X", values.NotNullLong)

	for _, tc := range []struct {
		name string
		a, b *values.RecordType
	}{
		{
			// The FAST-PATH case: identical fields, so a.Equals(b) is true.
			"same fields, different leg WIDTHS",
			withLegs(base, legOf("L", 0, 2)),
			withLegs(base, legOf("L", 0, 1)),
		},
		{
			"same fields, different leg START",
			withLegs(base, legOf("L", 0, 1)),
			withLegs(base, legOf("L", 1, 1)),
		},
		{
			"same fields, different leg IDENTITY",
			withLegs(base, legOf("L", 0, 2)),
			withLegs(base, legOf("M", 0, 2)),
		},
		{
			"same fields, one member states boundaries the other denies",
			withLegs(base, legOf("L", 0, 2)),
			base,
		},
		{
			// A SLOW-PATH case: the fields also differ, so the disagreement has to
			// be caught by the rebuild rather than before it.
			"differing unresolved field AND different leg tables",
			withLegs(rowOfTypes("A", values.NotNullLong, "X", values.UnknownType), legOf("L", 0, 2)),
			withLegs(base, legOf("M", 0, 2)),
		},
	} {
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "l1", typ: tc.a},
			&typedStubExpr{name: "l2", typ: tc.b})
		var de *MemberResultTypeDisagreementError
		if !errors.As(err, &de) {
			t.Errorf("%s: resolved to (%v, %v), want a disagreement.\n"+
				"  RecordType.Equals ignores Legs, so a fast path that trusts it resolves a\n"+
				"  boundary-table conflict by memo insertion order — the losing member's\n"+
				"  buried windows disappear and the reads filed against them silently\n"+
				"  re-anchor onto the qualified name channel.", tc.name, got, err)
		}
	}
}

// TestGetFlowedObjectType_RefinesNestedUnresolvedFields pins that the refinement
// reaches BELOW the top level. A row whose only difference is an unresolved field
// two levels down is the same row for exactly the reason it is one level down, and
// a top-level-only refinement would report a disagreement on it.
func TestGetFlowedObjectType_RefinesNestedUnresolvedFields(t *testing.T) {
	t.Parallel()

	nestedResolved := rowOfTypes("OUT", rowOfTypes("IN", values.NotNullLong))
	nestedUnresolved := rowOfTypes("OUT", rowOfTypes("IN", values.UnknownType))

	got, err := flowedTypeOf(t,
		&typedStubExpr{name: "n1", typ: nestedUnresolved},
		&typedStubExpr{name: "n2", typ: nestedResolved})
	if err != nil {
		t.Fatalf("members differing only in a NESTED unresolved field reported a "+
			"disagreement: %v", err)
	}
	if got == nil || !got.Equals(nestedResolved) {
		t.Errorf("nested refinement resolved %v, want %v", got, nestedResolved)
	}
}

// TestGetFlowedObjectType_RefinesInsideEveryContainer pins that the refinement
// reaches through ARRAY and RELATION, not only through RECORD.
//
// The rule that forces the recursion is indifferent to which container it is
// crossing — an unstated type states nothing — so stopping at RECORD was an
// accident of where the first misreport was found. The array case is not exotic:
// ArrayType.ElementType is nil while inference has not filled it in, and Go does
// not infer array element types far, so `ARRAY<?>` meeting `ARRAY<INT>` on two
// members of one class is the commonest shape of all. Reported as a disagreement,
// it costs a plan — the caller treats the error as a STOP.
func TestGetFlowedObjectType_RefinesInsideEveryContainer(t *testing.T) {
	t.Parallel()

	arrayOf := func(e values.Type) values.Type { return &values.ArrayType{Nullable: true, ElementType: e} }
	relationOf := func(e values.Type) values.Type { return &values.RelationType{InnerType: e} }

	for _, tc := range []struct {
		name           string
		unstated, said values.Type
	}{
		{
			"array with an uninferred element",
			arrayOf(nil), arrayOf(values.NotNullLong),
		},
		{
			"array whose element is UNKNOWN rather than nil",
			arrayOf(values.UnknownType), arrayOf(values.NotNullString),
		},
		{
			// The nested case: an array OF records differing one field down.
			// Two containers deep is where a single-level recursion still fails.
			"array of records with an unresolved leaf",
			arrayOf(rowOfTypes("N", values.UnknownType)),
			arrayOf(rowOfTypes("N", values.NotNullLong)),
		},
		{
			"erased relation",
			relationOf(nil), relationOf(rowOfTypes("N", values.NotNullLong)),
		},
		{
			"relation whose inner row has an unresolved leaf",
			relationOf(rowOfTypes("N", values.UnknownType)),
			relationOf(rowOfTypes("N", values.NotNullLong)),
		},
	} {
		// BOTH ORDERS: the more resolved container must win regardless of which
		// member the memo scan reaches first.
		for _, order := range []struct {
			label string
			a, b  values.Type
		}{
			{"unstated first", tc.unstated, tc.said},
			{"stated first", tc.said, tc.unstated},
		} {
			got, err := flowedTypeOf(t,
				&typedStubExpr{name: "c1", typ: rowOfTypes("F", order.a)},
				&typedStubExpr{name: "c2", typ: rowOfTypes("F", order.b)})
			if err != nil || got == nil {
				t.Errorf("%s / %s: reported (%v, %v).\n"+
					"  The two members differ only in a type the container has not inferred.\n"+
					"  An unstated type cannot contradict a stated one at ANY depth, in ANY\n"+
					"  container — a refinement that recurses into RECORD alone reports a row\n"+
					"  as disagreeing with itself the moment the gap is one container over.",
					tc.name, order.label, got, err)
				continue
			}
			if !got.Fields[0].FieldType.Equals(tc.said) {
				t.Errorf("%s / %s: refined to %v, want the more RESOLVED %v",
					tc.name, order.label, got.Fields[0].FieldType, tc.said)
			}
		}
	}
}

// TestGetFlowedObjectType_ContainersStillCatchRealDisagreements keeps the arms
// above from degrading into "containers always agree". A container that recurses
// must still report a conflict its contents genuinely have, or the recursion has
// bought a misreport in the other direction — two members flowing different rows
// merged into one, which is the wrong-slot read the whole verification exists to
// refuse.
func TestGetFlowedObjectType_ContainersStillCatchRealDisagreements(t *testing.T) {
	t.Parallel()

	arrayOf := func(n bool, e values.Type) values.Type {
		return &values.ArrayType{Nullable: n, ElementType: e}
	}

	for _, tc := range []struct {
		name string
		a, b values.Type
	}{
		{
			"array elements both STATED and different",
			arrayOf(true, values.NotNullLong), arrayOf(true, values.NotNullString),
		},
		{
			// An array that admits NULL and one that does not are different
			// columns, exactly as two rows are.
			"array nullability differs",
			arrayOf(true, values.UnknownType), arrayOf(false, values.NotNullLong),
		},
		{
			"array of records with a conflicting stated leaf",
			arrayOf(true, rowOfTypes("N", values.NotNullLong)),
			arrayOf(true, rowOfTypes("N", values.NotNullString)),
		},
		{
			"relation inner rows conflict on a stated leaf",
			&values.RelationType{InnerType: rowOfTypes("N", values.NotNullLong)},
			&values.RelationType{InnerType: rowOfTypes("N", values.NotNullString)},
		},
		{
			// Different CONTAINERS are a disagreement, not something to unwrap.
			"array versus relation",
			arrayOf(true, values.NotNullLong),
			&values.RelationType{InnerType: rowOfTypes("N", values.NotNullLong)},
		},
	} {
		got, err := flowedTypeOf(t,
			&typedStubExpr{name: "k1", typ: rowOfTypes("F", tc.a)},
			&typedStubExpr{name: "k2", typ: rowOfTypes("F", tc.b)})
		var de *MemberResultTypeDisagreementError
		if !errors.As(err, &de) {
			t.Errorf("%s: resolved to (%v, %v), want a disagreement — recursing into a "+
				"container must not turn it into a container that always agrees",
				tc.name, got, err)
		}
	}
}

// TestGetFlowedObjectType_AnAnonymousEnumIsNotUnstated pins the container that
// deliberately does NOT recurse, so its absence stays a decision.
//
// Next to RECORD's anonymous name this looks like an omission, and it is not. A
// record's empty name is documented as "not bound to a named struct" — an
// inference gap, which is why it refines. An enum's empty name is documented as an
// anonymous enum, "rare in real schemas but legal" — a legal schema state, the
// exact opposite. And an enum has no other content to refine: its value list is
// DECLARED, with no "not inferred yet" member. Go's own type merge declines
// anonymous-enum handling for the same reason.
//
// So two enums either state the same type or different ones, and if this ever
// becomes wrong the fix is a rule about enum names, not another recursion arm.
func TestGetFlowedObjectType_AnAnonymousEnumIsNotUnstated(t *testing.T) {
	t.Parallel()

	vals := []values.EnumValue{{Name: "A", Number: 0}, {Name: "B", Number: 1}}
	anonymous := &values.EnumType{EnumName: "", Nullable: true, Values: vals}
	named := &values.EnumType{EnumName: "E", Nullable: true, Values: vals}

	got, err := flowedTypeOf(t,
		&typedStubExpr{name: "e1", typ: rowOfTypes("F", values.Type(anonymous))},
		&typedStubExpr{name: "e2", typ: rowOfTypes("F", values.Type(named))})
	var de *MemberResultTypeDisagreementError
	if !errors.As(err, &de) {
		t.Errorf("an ANONYMOUS enum and a NAMED one resolved to (%v, %v) instead of "+
			"disagreeing.\n"+
			"  If an enum-name refinement arm was added, that is a change of position, not\n"+
			"  a cleanup: an empty EnumName is a legal anonymous enum, not an inference gap\n"+
			"  the way an empty RecordName is. Say so where the arm goes in, and retarget\n"+
			"  this test — do not delete it.", got, err)
	}
}
