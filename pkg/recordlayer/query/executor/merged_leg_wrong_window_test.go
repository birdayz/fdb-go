package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The wrong-window instrument's floor counts reads that resolved to a MISAIMED
// window, and that count is only worth anything if "misaimed" means the window
// serves another leg's slots. A flag set beside a window that still reads its own
// leg would make the FDB instrument
// (TestFDB_MergedLegBinding_WrongWindowsAreUnobservable) report a perturbation it
// never performed — the exact vacuity it was built to end, reintroduced one level
// down. So the perturbation is pinned on VALUES, not on the flag.

// TestMisaimMergedLegWindows_ServesTheSiblingsSlots pins that the rotation moves
// each window onto its sibling's span, in both directions, and that the reads
// through those windows change accordingly.
func TestMisaimMergedLegWindows_ServesTheSiblingsSlots(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()
	// Leg A is slots [0,2) = {10, 11}; leg B is slot [2,3) = {22}.
	row := mergedOuterRowFixture(aAlias, bAlias)
	ctx := bindMergedOuterLegs(EmptyEvaluationContext(), row, joinAlias)

	misaimMergedLegWindows(ctx.bindings, []values.CorrelationIdentifier{aAlias, bAlias})

	// A now reads B's ONE slot: its own slot 1 has gone out of range, and its slot
	// 0 answers with B's value. B now reads A's TWO.
	for _, tc := range []struct {
		name    string
		alias   values.CorrelationIdentifier
		ordinal int
		want    any
		present bool
	}{
		{"A slot 0 now serves B's slot", aAlias, 0, int64(22), true},
		{"A slot 1 is past B's one-column window", aAlias, 1, nil, false},
		{"B slot 0 now serves A's first slot", bAlias, 0, int64(10), true},
		{"B slot 1 now serves A's second slot", bAlias, 1, int64(11), true},
	} {
		bound, ok := ctx.GetCorrelationBinding(tc.alias)
		if !ok {
			t.Fatalf("%s: leg %s lost its binding to the rotation. Misaiming must move a "+
				"window, never remove it — a removed binding is the OTHER perturbation "+
				"(the redundancy pin's bypass) and proves a different thing.",
				tc.name, tc.alias.Name())
		}
		w, isWindow := bound.(*legWindowRow)
		if !isWindow {
			t.Fatalf("%s: leg %s bound to %T, want a leg window", tc.name, tc.alias.Name(), bound)
		}
		if !w.misaimed {
			t.Fatalf("%s: leg %s's window is not marked misaimed, but its span was "+
				"rotated onto a sibling. The FDB instrument's engagement floor counts "+
				"this mark; unmarked, a genuinely perturbed run reports as never having "+
				"engaged.", tc.name, tc.alias.Name())
		}
		got, present := w.Get(tc.ordinal)
		if present != tc.present {
			t.Fatalf("%s: leg %s ordinal %d present=%v, want %v — the rotated window is "+
				"not bounded by the span it was moved to.",
				tc.name, tc.alias.Name(), tc.ordinal, present, tc.present)
		}
		if present && got != tc.want {
			t.Fatalf("%s: leg %s ordinal %d read %v, want %v.\n"+
				"  The rotation left the window reading its OWN leg, so the standing "+
				"wrong-window mutation is not wrong at all and the suite's greenness "+
				"under it means nothing.",
				tc.name, tc.alias.Name(), tc.ordinal, got, tc.want)
		}
	}
}

// TestMisaimMergedLegWindows_IdenticalSpansAreNotCountedAsMisaimed pins the
// honesty of the engagement floor at its one degenerate case: two legs occupying
// the SAME span rotate onto each other and nothing moves, so nothing may be
// claimed as misaimed.
//
// Counting it would let the FDB instrument's floor pass on a run where every
// window still serves its own slots — a floor that reports engagement without
// engagement is worse than no floor, because it reads as a positive control.
func TestMisaimMergedLegWindows_IdenticalSpansAreNotCountedAsMisaimed(t *testing.T) {
	t.Parallel()

	aAlias := values.NamedCorrelationIdentifier("A")
	bAlias := values.NamedCorrelationIdentifier("B")
	joinAlias := values.UniqueCorrelationIdentifier()
	// Two distinct aliases over the SAME span. First-claim-wins dedups by alias,
	// not by span, so this is reachable in principle.
	row := &PositionalRow{
		Type: &values.RecordType{
			Fields: []values.Field{
				{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
				{Name: "K", FieldType: values.NotNullLong, Ordinal: 1},
			},
			Legs: []values.RecordTypeLeg{
				values.NewRecordTypeLeg(aAlias, aAlias.Name(), 0, 2),
				values.NewRecordTypeLeg(bAlias, bAlias.Name(), 0, 2),
			},
		},
		Slots: []any{int64(7), int64(8)},
	}
	ctx := bindMergedOuterLegs(EmptyEvaluationContext(), row, joinAlias)

	misaimMergedLegWindows(ctx.bindings, []values.CorrelationIdentifier{aAlias, bAlias})

	for _, alias := range []values.CorrelationIdentifier{aAlias, bAlias} {
		bound, _ := ctx.GetCorrelationBinding(alias)
		w, isWindow := bound.(*legWindowRow)
		if !isWindow {
			t.Fatalf("leg %s bound to %T, want a leg window", alias.Name(), bound)
		}
		if w.misaimed {
			t.Fatalf("leg %s's window was marked misaimed by a rotation that moved it "+
				"onto its own span [%d,%d). Nothing was perturbed, so nothing may be "+
				"reported as perturbed.", alias.Name(), w.offset, w.offset+w.width)
		}
		if v, ok := w.Get(0); !ok || v != int64(7) {
			t.Fatalf("leg %s ordinal 0 read (%v, %v), want (7, true) — the window moved "+
				"despite the spans being identical.", alias.Name(), v, ok)
		}
	}
}
