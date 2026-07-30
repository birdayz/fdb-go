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
