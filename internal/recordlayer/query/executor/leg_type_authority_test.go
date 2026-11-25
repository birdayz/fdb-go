package executor

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// legTypeOfWidth builds a leg record type of n LONG columns. WIDTH is what the
// agreement check compares, so the fixture varies only that.
func legTypeOfWidth(n int) *values.RecordType {
	fields := make([]values.Field, n)
	for i := range fields {
		fields[i] = values.Field{Name: "C" + string(rune('0'+i)), FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType("", false, fields)
}

// TestLegTypeAuthorityIsTheBakedReference pins WHICH of the two independently
// populated leg-type sources wins, and that a disagreement between them is
// rejected at build time rather than resolved by lookup order.
//
// The two sources are not redundant copies. LegTypes is recovered from the
// result value's own BAKED leg references, widened from the join predicates and
// from the plan walk — it is the type the expressions that will actually be
// evaluated were constructed against. Spans come from the pristine-seed leg
// window walk and exist only when WindowsOK. When both describe one leg they
// describe one flowed row, so agreement is an invariant, not a coincidence, and
// the precedence decides nothing on a well-formed plan.
//
// The arms are separated because the two failure modes are different bugs: a
// wrong winner silently adapts a leg to the wrong width, while a missing
// divergence report lets a malformed plan through as a plausible row.
func TestLegTypeAuthorityIsTheBakedReference(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("L")
	other := values.NamedCorrelationIdentifier("OTHER")

	t.Run("baked reference wins over an agreeing span", func(t *testing.T) {
		t.Parallel()
		baked := legTypeOfWidth(2)
		b := &ordinalJoinBuild{
			LegTypes:  map[values.CorrelationIdentifier]*values.RecordType{alias: baked},
			Spans:     []legSpan{{Alias: alias, LegType: legTypeOfWidth(2)}},
			WindowsOK: true,
		}
		if err := b.assertLegTypeSourcesAgree(); err != nil {
			t.Fatalf("agreeing sources reported a divergence: %v", err)
		}
		if got := b.legType(alias); got != baked {
			t.Fatalf("legType returned %p, want the baked reference %p — the span is a positional "+
				"description of the seed, not the type the baked expressions were built against", got, baked)
		}
	})

	t.Run("span types a leg no baked reference names", func(t *testing.T) {
		t.Parallel()
		span := legTypeOfWidth(3)
		b := &ordinalJoinBuild{
			Spans:     []legSpan{{Alias: alias, LegType: span}},
			WindowsOK: true,
		}
		if err := b.assertLegTypeSourcesAgree(); err != nil {
			t.Fatalf("span-only leg reported a divergence: %v", err)
		}
		if got := b.legType(alias); got != span {
			t.Fatalf("legType returned %p, want the span type %p", got, span)
		}
	})

	t.Run("a span is invisible unless WindowsOK", func(t *testing.T) {
		t.Parallel()
		b := &ordinalJoinBuild{
			Spans:     []legSpan{{Alias: alias, LegType: legTypeOfWidth(3)}},
			WindowsOK: false,
		}
		if err := b.assertLegTypeSourcesAgree(); err != nil {
			t.Fatalf("folded RV reported a divergence: %v", err)
		}
		if got := b.legType(alias); got != nil {
			t.Fatalf("legType = %v for a folded RV, want nil — Spans describe the pristine seed "+
				"only, so a folded result value must not be typed from them", got)
		}
	})

	t.Run("no source names the leg", func(t *testing.T) {
		t.Parallel()
		b := &ordinalJoinBuild{
			LegTypes:  map[values.CorrelationIdentifier]*values.RecordType{other: legTypeOfWidth(1)},
			Spans:     []legSpan{{Alias: other, LegType: legTypeOfWidth(1)}},
			WindowsOK: true,
		}
		if err := b.assertLegTypeSourcesAgree(); err != nil {
			t.Fatalf("unnamed leg reported a divergence: %v", err)
		}
		if got := b.legType(alias); got != nil {
			t.Fatalf("legType = %v for a leg no source names, want nil", got)
		}
	})

	// The arm the precedence made unobservable. Without it, whichever source the
	// lookup reaches first silently wins and the two widths never meet.
	t.Run("disagreeing sources are a rejected build", func(t *testing.T) {
		t.Parallel()
		b := &ordinalJoinBuild{
			LegTypes:  map[values.CorrelationIdentifier]*values.RecordType{alias: legTypeOfWidth(2)},
			Spans:     []legSpan{{Alias: alias, LegType: legTypeOfWidth(3)}},
			WindowsOK: true,
		}
		err := b.assertLegTypeSourcesAgree()
		if err == nil {
			t.Fatal("sources that disagree on width (2 vs 3) were accepted; a silent winner adapts " +
				"the leg to a width the other half of the plan does not expect")
		}
		for _, want := range []string{"DIVERGENT", "2", "3", "L"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("divergence error %q does not name %q; the message has to identify the "+
					"leg and both widths", err, want)
			}
		}
	})

	// A disagreement is only detectable while BOTH sources are live, so the
	// folded case must stay silent for the reason above and not because the
	// check has quietly stopped running. This is the vacuity guard for the arm
	// above: same disagreeing widths, WindowsOK off.
	t.Run("a folded RV has no second source to disagree with", func(t *testing.T) {
		t.Parallel()
		b := &ordinalJoinBuild{
			LegTypes:  map[values.CorrelationIdentifier]*values.RecordType{alias: legTypeOfWidth(2)},
			Spans:     []legSpan{{Alias: alias, LegType: legTypeOfWidth(3)}},
			WindowsOK: false,
		}
		if err := b.assertLegTypeSourcesAgree(); err != nil {
			t.Fatalf("folded RV reported a divergence against a span it must not consult: %v", err)
		}
	})
}
