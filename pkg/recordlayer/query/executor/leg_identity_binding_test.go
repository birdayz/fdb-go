package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The leg binders decide WHICH ROW'S SLOTS a correlated value reads. They answer
// that by leg IDENTITY (values.SameLeg, exact — the same equality Java's
// CorrelationIdentifier uses), never by matching the leg's Name text.
//
// These tests pin the two ways that can regress, separately, because a fix that
// satisfies one and not the other leaves the defect live:
//
//   - a reader reverted to comparing Name: the FORGERY shapes below bind a
//     case-variant leg, which a text/folding comparison accepts;
//   - a producer that stops STATING identity: the unstated-identity shapes below
//     start binding by name again.
//
// The forgery shape is not decorative. Alias namespaces in this planner are
// deliberately case-DISJOINT — the semantic scope upper-folds every user
// correlation at its single registration chokepoint while
// UniqueCorrelationIdentifier mints the machine counter LOWERCASE — so a quoted
// user alias "q$5" must not be able to bind a planner-minted q$5's window. A
// forged bind there is a wrong-rows plan, not a lost optimization.

// legRowFixture builds a two-leg merged row whose legs' identities and texts are
// supplied independently, so a test can make them disagree the way a folding
// comparison would need them to.
func legRowFixture(legs []values.RecordTypeLeg) *PositionalRow {
	return &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{
				{Name: "AK", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "BK", FieldType: values.NotNullLong, Ordinal: 1},
			},
			Legs: legs,
		},
		Slots: []any{int64(10), int64(20)},
	}
}

func TestRowLegsBinder_BindsByIdentityNotText(t *testing.T) {
	t.Parallel()

	minted := values.NamedCorrelationIdentifier("q$5")
	// The leg belongs to the MACHINE namespace (lowercase). Its Name is the UPPER
	// fold — the spelling a text channel would carry.
	legs := []values.RecordTypeLeg{
		{Alias: minted, Name: "Q$5", Start: 0, Width: 1},
		{Alias: values.NamedCorrelationIdentifier("B"), Name: "B", Start: 1, Width: 1},
	}
	b := &rowLegsBinder{row: legRowFixture(legs)}

	// The leg's OWN identity binds.
	if _, ok := b.GetCorrelationBinding(minted); !ok {
		t.Error("the leg's own identity did not bind its window — exactness must still " +
			"admit a leg against itself, or every correlated read declines")
	}

	// FORGERY: a user alias that upper-folds onto the minted leg's text. A
	// text-or-folding comparison accepts it; identity must not.
	forged := values.NamedCorrelationIdentifier("Q$5")
	if _, ok := b.GetCorrelationBinding(forged); ok {
		t.Error(`GetCorrelationBinding(Q$5) bound the q$5 leg's window — a quoted user ` +
			`alias forged a planner-minted leg. The binder is matching leg TEXT (or ` +
			`folding), which erases the case-disjointness of the alias namespaces and ` +
			`serves the wrong row's slots to a correlated read.`)
	}

	// A different leg entirely still declines.
	if _, ok := b.GetCorrelationBinding(values.NamedCorrelationIdentifier("ZZ")); ok {
		t.Error("an unrelated correlation bound a leg window")
	}
}

func TestRowLegsBinder_UnstatedIdentityDoesNotBindByName(t *testing.T) {
	t.Parallel()

	// A leg carrying TEXT but no identity — what a construction site that forgot to
	// thread the quantifier alias produces.
	legs := []values.RecordTypeLeg{
		{Name: "A", Start: 0, Width: 1},
		{Alias: values.NamedCorrelationIdentifier("B"), Name: "B", Start: 1, Width: 1},
	}
	b := &rowLegsBinder{row: legRowFixture(legs)}

	if _, ok := b.GetCorrelationBinding(values.NamedCorrelationIdentifier("A")); ok {
		t.Error(`a leg that STATES NO identity bound correlation A anyway — the binder ` +
			`fell back to its Name. A producer that drops the typed alias must lose the ` +
			`bind (a loud decline downstream), not silently keep working through text: ` +
			`the text fallback is what lets an unstated identity go unnoticed until it ` +
			`binds the wrong leg.`)
	}
	// The leg that DID state its identity is unaffected — the decline is per-leg,
	// not a whole-row failure.
	if _, ok := b.GetCorrelationBinding(values.NamedCorrelationIdentifier("B")); !ok {
		t.Error("the sibling leg that stated its identity stopped binding")
	}
}

func TestBuriedLegWindow_BindsByIdentityNotText(t *testing.T) {
	t.Parallel()

	minted := values.NamedCorrelationIdentifier("q$7")
	boxTyp := &values.RecordType{
		Fields: []values.Field{
			{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "EID", FieldType: values.NotNullLong, Ordinal: 1},
		},
		Legs: []values.RecordTypeLeg{
			{Alias: minted, Name: "Q$7", Start: 0, Width: 1},
			{Alias: values.NamedCorrelationIdentifier("E"), Name: "E", Start: 1, Width: 1},
		},
	}
	row := &PositionalRow{
		Type:  &values.RecordType{Fields: boxTyp.Fields},
		Slots: []any{int64(1), int64(2)},
	}
	span := legSpan{Alias: values.NamedCorrelationIdentifier("E"), LegType: boxTyp, Offset: 0, Width: 2}

	if _, ok := buriedLegWindow(row, span, minted); !ok {
		t.Fatal("the buried leg's own identity did not window it")
	}
	if _, ok := buriedLegWindow(row, span, values.NamedCorrelationIdentifier("Q$7")); ok {
		t.Error(`buriedLegWindow served the q$7 sub-window to a quoted "Q$7" — a buried ` +
			`leg was selected by TEXT, so a case-variant alias reads another source's ` +
			`slots inside the box concat`)
	}
}

func TestBuriedLegWindow_UnstatedIdentityDoesNotBindByName(t *testing.T) {
	t.Parallel()

	boxTyp := &values.RecordType{
		Fields: []values.Field{
			{Name: "BID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "EID", FieldType: values.NotNullLong, Ordinal: 1},
		},
		// The buried leg names itself but states no identity.
		Legs: []values.RecordTypeLeg{{Name: "B", Start: 0, Width: 1}},
	}
	row := &PositionalRow{
		Type:  &values.RecordType{Fields: boxTyp.Fields},
		Slots: []any{int64(1), int64(2)},
	}
	span := legSpan{Alias: values.NamedCorrelationIdentifier("E"), LegType: boxTyp, Offset: 0, Width: 2}

	if _, ok := buriedLegWindow(row, span, values.NamedCorrelationIdentifier("B")); ok {
		t.Error("a buried leg with no stated identity was windowed by its Name — the " +
			"buried-bounds producer can then omit the alias without any test noticing")
	}
}

// The leg window a bind returns must be the leg's OWN slot range. A binder that
// compared identity correctly but returned the wrong window would pass every test
// above while still reading the wrong row, so the window is pinned too.
func TestRowLegsBinder_BoundWindowIsTheLegsOwnSlots(t *testing.T) {
	t.Parallel()

	aCorr := values.NamedCorrelationIdentifier("A")
	bCorr := values.NamedCorrelationIdentifier("B")
	row := legRowFixture([]values.RecordTypeLeg{
		{Alias: aCorr, Name: "A", Start: 0, Width: 1},
		{Alias: bCorr, Name: "B", Start: 1, Width: 1},
	})
	b := &rowLegsBinder{row: row}

	for _, tc := range []struct {
		corr values.CorrelationIdentifier
		want int64
	}{{aCorr, 10}, {bCorr, 20}} {
		bound, ok := b.GetCorrelationBinding(tc.corr)
		if !ok {
			t.Fatalf("leg %s did not bind", tc.corr)
		}
		lw, isLW := bound.(*legWindowRow)
		if !isLW {
			t.Fatalf("leg %s bound %T, want a leg window", tc.corr, bound)
		}
		got, gotOK := lw.Get(0)
		if !gotOK || got != tc.want {
			t.Errorf("leg %s window slot 0 = (%v, %v), want %d — the bind resolved the "+
				"right leg but windowed the wrong slots", tc.corr, got, gotOK, tc.want)
		}
	}
}
