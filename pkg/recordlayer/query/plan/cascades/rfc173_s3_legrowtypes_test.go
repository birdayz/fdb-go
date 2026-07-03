package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// chainLegType / starSpokeType / hubLegType rebuild the AUTHORITATIVE (flowed)
// leg row type — what a typed pull-up over the merge quantifier's flowed Type
// (Java Quantifier.java:759) yields, which is what legRowTypes must reconstruct
// for Go (whose Quantifier.GetFlowedObjectValue is untyped). A quantified
// object value flows a NULLABLE row (QuantifiedObjectValue.Type nullable-izes,
// like Java), so the authoritative type is the leg's fields under a nullable
// record — recovered via the same QOV path the seed uses, so the pin asserts
// legRowTypes maps each alias to its OWN correct-width type (no conflation, no
// drop, no nullability drift), not a tautology.
func flowedLegType(name string, fields []values.Field) *values.RecordType {
	rt := values.NewRecordType(name, false, fields)
	qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(name), rt)
	return qov.Type().(*values.RecordType)
}

func chainLegType(name string) *values.RecordType {
	return flowedLegType(name, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NEXT_ID", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

func starSpokeType(name string) *values.RecordType {
	return flowedLegType(name, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "HID", FieldType: values.NotNullLong, Ordinal: 1},
	})
}

// TestRFC173S3_LegRowTypesBridge is the RFC-173 Slice-3 Q4 gate: legRowTypes is
// BLESSED as the Go bridge for Java's typed merge-quantifier pull-up
// (positionalMergeCase recovers each leg's flowed ROW type ad-hoc because Go's
// Quantifier.GetFlowedObjectValue is untyped), conditioned on TWO pins:
//
//   - (i) NO-UNTYPED-SLOT: every live leg of an ordinal-seed select recovers a
//     concrete *RecordType. A leg recovered as absent flows an UNTYPED merge slot
//     (legRowTypes' documented failure mode), stripping the leg types the
//     executor's span recovery and downstream fused references resolve through.
//   - (ii) RECOVERED == FLOWED: the reconstruction equals the authoritative leg
//     type the seed baked (the type a typed pull-up over the merge quantifier's
//     flowed Type would yield) — the bridge is FAITHFUL, not lossy.
//
// (iii) the named typed-pull-up owner is Slice 5 (with the local-bind subtraction
// work): if a future N-way shape defeats ad-hoc recovery, THAT is when Go ports
// the first-class typed surface. Not required for S3.
func TestRFC173S3_LegRowTypesBridge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sel  *expressions.SelectExpression
		want map[string]*values.RecordType
	}{
		{
			name: "chain_3way",
			sel:  buildOrdinalChainSelect(3),
			want: map[string]*values.RecordType{"T1": chainLegType("T1"), "T2": chainLegType("T2"), "T3": chainLegType("T3")},
		},
		{
			name: "chain_4way",
			sel:  buildOrdinalChainSelect(4),
			want: map[string]*values.RecordType{"T1": chainLegType("T1"), "T2": chainLegType("T2"), "T3": chainLegType("T3"), "T4": chainLegType("T4")},
		},
		{
			// Mixed leg widths: hub [ID] + spokes [ID, HID] — the recovery must
			// not conflate a 1-field leg with a 2-field one.
			name: "star_3spoke",
			sel:  buildOrdinalStar(3),
			want: map[string]*values.RecordType{
				"H":  flowedLegType("H", []values.Field{{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0}}),
				"S1": starSpokeType("S1"), "S2": starSpokeType("S2"), "S3": starSpokeType("S3"),
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := legRowTypes(tc.sel.GetResultValue(), tc.sel.GetPredicates())
			for alias, wantRT := range tc.want {
				id := values.NamedCorrelationIdentifier(alias)
				recovered, ok := got[id]
				// (i) no-untyped-slot.
				if !ok || recovered == nil {
					t.Errorf("%s: leg %s recovered NO type — flows an UNTYPED merge slot (Q4 no-untyped-slot)", tc.name, alias)
					continue
				}
				if len(recovered.Fields) == 0 {
					t.Errorf("%s: leg %s recovered an empty RecordType — untyped slot", tc.name, alias)
					continue
				}
				// (ii) recovered == flowed.
				if !recovered.Equals(wantRT) {
					t.Errorf("%s: leg %s recovered %v, want authoritative %v (Q4 recovered==flowed)", tc.name, alias, recovered, wantRT)
				}
			}
			// No EXTRA legs beyond the expected set (a stray recovery would mean
			// the ad-hoc walk over-collects).
			if len(got) != len(tc.want) {
				t.Errorf("%s: legRowTypes recovered %d legs, want exactly %d", tc.name, len(got), len(tc.want))
			}
		})
	}
}

// TestRFC173S3_LegRowTypesBridge_UntypedSlotControl is the negative control: it
// proves the no-untyped-slot pin has teeth. A leg whose QOV appears NOWHERE in
// the value surfaces legRowTypes walks (result value + predicates) is recovered
// as ABSENT — the exact untyped-slot failure the pin above forbids for a real
// ordinal-seed select (where every LIVE leg is, by definition, referenced).
func TestRFC173S3_LegRowTypesBridge_UntypedSlotControl(t *testing.T) {
	t.Parallel()
	// A single-QOV result value referencing only T1 (typed) — T2 is not
	// referenced anywhere, so its type cannot be recovered.
	t1 := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("T1"), chainLegType("T1"))
	fv, err := values.NewFieldValueOfOrdinal(t1, 0)
	if err != nil {
		t.Fatal(err)
	}
	rv := values.NewRawRecordConstructorValue(values.RecordConstructorField{Name: "ID", Value: fv})

	got := legRowTypes(rv, nil)
	if _, ok := got[values.NamedCorrelationIdentifier("T1")]; !ok {
		t.Error("control: T1 IS referenced — its type must be recovered")
	}
	if _, ok := got[values.NamedCorrelationIdentifier("T2")]; ok {
		t.Error("control: T2 is referenced NOWHERE — legRowTypes must NOT recover it (this is the " +
			"untyped-slot the Q4 no-untyped-slot pin catches for a real ordinal-seed leg)")
	}
}
