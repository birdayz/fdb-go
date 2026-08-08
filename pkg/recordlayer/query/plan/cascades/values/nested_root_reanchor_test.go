package values

import "testing"

// The RFC-218 re-anchor harness, kept as a test rather than as prose.
//
// The projected-EXISTS fold re-derives a nested sort key from a RENDERED name
// and loses the resolved path, which is a live defect. The fix carries the path
// and RE-ANCHORS its root ordinal into the layout the fold evaluates against.
// Everything below drives that mechanism directly, because the design was
// specified three times and its cited primitives were wrong each time — twice
// because a guard was read instead of run, once because the guard collapsed the
// two cases the design needed separated. The mechanism now lives here, driven,
// so a later reader can re-run it instead of re-deriving it.
//
// EVERY ASSERTION BELOW DRIVES THE PRODUCTION METHODS ON FieldPath, and that
// sentence is load-bearing rather than decorative. An earlier version of this
// file defined LOCAL COPIES of the algorithm and drove those; the file is
// `package values`, so it compiled happily against the real source while
// asserting nothing about it. Mutating production to first-match on duplicate
// root names left every assertion GREEN. If you add a case here, call the
// method — never re-implement it to "keep the test self-contained".
//
// A NOTE ON WHAT THIS FILE DOES NOT PROVE. These exercises run against
// hand-built layouts. They establish that the composition behaves as specified
// when handed a given layout; they do NOT establish that the fold hands it that
// layout. The fold has TWO relevant layouts — the outer/merged scan row that the
// hidden sort column's VALUE is built against, and the folded projection that
// the sort key is RESOLVED against — and wiring must state which one it is
// re-anchoring into and assert it, or a wrong-but-plausible layout will
// re-anchor to some other column and return rows with no error at all.

// rawRecord builds a RecordType WITHOUT NewRecordType's duplicate-name panic.
// The real merged join row genuinely holds two columns named ID
// (`[ID T1_ID ID N]`), so the duplicate case is the point, not an edge case.
func rawRecord(names ...string) *RecordType {
	fs := make([]Field, len(names))
	for i, n := range names {
		fs[i] = Field{Name: n, FieldType: UnknownType, Ordinal: i}
	}
	return &RecordType{Fields: fs}
}

func nestedKeyPath() *FieldPath {
	return &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 1}, {Field: "SK", Ordinal: 0}},
		Domain:    OrdinalDomainOfType(rawRecord("ID", "N")),
	}
}

// TestOrdinalInRefusesOnArityBeforeDomain pins WHY a new accessor is needed.
// If OrdinalIn ever starts answering for multi-accessor paths, the re-anchor
// should use it and RootOrdinalIn should go away — this test is what
// tells the next reader that.
func TestOrdinalInRefusesOnArityBeforeDomain(t *testing.T) {
	t.Parallel()
	multi := nestedKeyPath()
	matching := OrdinalDomainOfType(rawRecord("ID", "N"))
	mismatched := OrdinalDomainOfType(rawRecord("ID", "T1_ID", "ID", "N"))

	if _, ok := multi.OrdinalIn(matching); ok {
		t.Fatal("OrdinalIn answered for a MULTI-accessor path whose domain matches — " +
			"the arity gate is gone. The re-anchor's separate root accessor exists " +
			"only because this returned false; re-read it before keeping both")
	}
	if _, ok := multi.OrdinalIn(mismatched); ok {
		t.Fatal("OrdinalIn answered for a multi-accessor path against a MISMATCHED domain")
	}
	// CONTROL: the refusal above must be about arity, not about the harness.
	single := &FieldPath{
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 1}},
		Domain:    matching,
	}
	got, ok := single.OrdinalIn(matching)
	if !ok || got != 1 {
		t.Fatalf("CONTROL failed: OrdinalIn(single, matching) = (%d,%v), want (1,true). "+
			"The two refusals above prove nothing until this succeeds", got, ok)
	}
}

// TestRootOrdinalInSeparatesArityFromDomain drives the accessor on exactly the
// two cases OrdinalIn collapses into one answer.
func TestRootOrdinalInSeparatesArityFromDomain(t *testing.T) {
	t.Parallel()
	multi := nestedKeyPath()
	matching := OrdinalDomainOfType(rawRecord("ID", "N"))
	mismatched := OrdinalDomainOfType(rawRecord("ID", "T1_ID", "ID", "N"))

	got, ok := multi.RootOrdinalIn(matching)
	if !ok || got != 1 {
		t.Fatalf("matching domain: got (%d,%v), want (1,true) — the accessor no longer "+
			"answers for a multi-accessor root, which is its entire purpose", got, ok)
	}
	if _, ok := multi.RootOrdinalIn(mismatched); ok {
		t.Fatal("MISMATCHED domain answered true — the domain gate is gone, and a " +
			"carried pre-merge ordinal would now be trusted against a merged row")
	}
}

// TestReAnchorRootAcrossFlowedLayouts drives every arm of the re-anchor.
//
// The ambiguous case is the one that must not regress into a first match: the
// real merged join row is `[ID T1_ID ID N]`, which already carries a duplicate
// name, and the fold can manufacture a duplicate of the ROOT's name itself
// because the hidden sort column it appends is named by the same rendering.
func TestReAnchorRootAcrossFlowedLayouts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		flowed   *RecordType
		wantOK   bool
		wantRoot int
		wantWhy  string
	}{
		{
			// The single-table fold. This is the case the fix exists for; if it
			// declines, the design has collapsed into "decline every nested key".
			name: "single_table_flowed_succeeds", flowed: rawRecord("ID", "N"),
			wantOK: true, wantRoot: 1,
		},
		{
			// The REAL merged join row, duplicate ID and all.
			name: "real_merged_join_row_reanchors", flowed: rawRecord("ID", "T1_ID", "ID", "N"),
			wantOK: true, wantRoot: 3,
		},
		{
			// N lands at 1 BY ACCIDENT in a different, larger layout. Correct
			// because DERIVED, not because a token compared equal.
			name: "coincidental_same_slot_is_derived", flowed: rawRecord("ID", "N", "X", "Y"),
			wantOK: true, wantRoot: 1,
		},
		{
			name: "root_absent_declines", flowed: rawRecord("ID", "Q"),
			wantOK: false, wantWhy: "root column absent from the flowed layout",
		},
		{
			// Two columns named N. FieldIndex would first-match slot 1.
			name: "duplicate_root_name_declines", flowed: rawRecord("ID", "N", "ID2", "N"),
			wantOK: false, wantWhy: "root column name is ambiguous in the flowed layout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, why, ok := nestedKeyPath().ReAnchorRootInto(tc.flowed)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (why=%q)", ok, tc.wantOK, why)
			}
			if !tc.wantOK {
				if why != tc.wantWhy {
					t.Fatalf("decline reason = %q, want %q", why, tc.wantWhy)
				}
				return
			}
			if got.Accessors[0].Ordinal != tc.wantRoot {
				t.Fatalf("root ordinal = %d, want %d (flowed=%v) — a re-anchor that "+
					"lands on the wrong slot returns the WRONG COLUMN silently",
					got.Accessors[0].Ordinal, tc.wantRoot, tc.flowed.Fields)
			}
			if got.Accessors[0].Field != "N" || len(got.Accessors) != 2 ||
				got.Accessors[1].Field != "SK" {
				t.Fatalf("path shape = %v, want {N@%d}{SK@0} — the suffix must survive",
					got.Accessors, tc.wantRoot)
			}
			if name := tc.flowed.Fields[got.Accessors[0].Ordinal].Name; name != "N" {
				t.Fatalf("re-anchored root points at %q, not N", name)
			}
		})
	}
}

// TestReAnchorRejectsContradictoryCarriedOrdinal drives the tripwire arm.
//
// It fires ONLY where the carried ordinal is comparable — same domain as the
// layout being re-anchored into. That narrowness is deliberate and is why the
// coincidence case above passes on derivation alone rather than on this check.
func TestReAnchorRejectsContradictoryCarriedOrdinal(t *testing.T) {
	t.Parallel()
	flowed := rawRecord("ID", "N")
	bad := &FieldPath{
		// Claims the [ID N] layout but puts N at 0, where ID is.
		Accessors: []ResolvedAccessor{{Field: "N", Ordinal: 0}, {Field: "SK", Ordinal: 0}},
		Domain:    OrdinalDomainOfType(flowed),
	}
	_, why, ok := bad.ReAnchorRootInto(flowed)
	if ok {
		t.Fatal("a carried ordinal contradicting its own stated layout was accepted — " +
			"the tripwire is gone, and a rebased-one-channel-not-the-other producer " +
			"would now go undetected")
	}
	if why != "carried ordinal disagrees with the flowed layout" {
		t.Fatalf("decline reason = %q, want the disagreement reason", why)
	}
}
