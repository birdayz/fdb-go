package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The EXISTENTIAL QUANTIFIER'S OBJECT IS ITS DATUM, and the two FlatMap result
// paths have to agree on that.
//
// Java's quantifier object is the datum, never the carrier around it:
// QuantifiedObjectValue.eval (QuantifiedObjectValue.java:84-95) returns
// `binding.getDatum()` for a non-record, non-relation result type, and
// ExistsValue.eval (ExistsValue.java:98-100) is `getChild().eval() != null` over
// exactly that. An existential whose subplan yielded nothing therefore reaches
// EXISTS as a NULL object and answers FALSE — the FirstOrDefault that emits the
// NullValue default is what makes the emptiness visible as a null datum.
//
// Go wraps a computed scalar in a one-slot `_0` row (scalarPositionalRow), so a
// path that binds THAT ROW under the quantifier makes the object non-null
// whatever it holds, and EXISTS becomes an unconditional TRUE. That is exactly
// what happened: computeResultLegs' non-build path unwrapped a bare-scalar inner
// to `Slots[0]`, the ordinal-BUILD path bound the row, and a projected EXISTS
// that landed on the build path answered TRUE for every row of a three-way join
// over an EMPTY existential table
// (TestFDB_ProjectedExistsCorrelatedToNonFirstLeg pins the SQL).
//
// So the asymmetry this pins is the whole content: the INNER binds its datum,
// the OUTER binds its row. It is asserted here rather than argued in a comment
// because the corpus does NOT distinguish the two — unwrapping the outer as well
// leaves the whole sqldriver suite green (measured), so nothing else would fail
// if the restriction were dropped, and a restriction nothing checks is a
// restriction that drifts.
func TestOrdinalJoinBuild_ScalarInnerBindsItsDatum(t *testing.T) {
	t.Parallel()

	inner := values.NamedCorrelationIdentifier("q$exists")
	outer := values.NamedCorrelationIdentifier("m")
	b := &ordinalJoinBuild{Enabled: true}

	// The shape a FirstOrDefault emits for an EMPTY subplan: one `_0` slot
	// holding the NullValue default.
	emptyExistential := &QueryResult{Positional: scalarPositionalRow(nil)}
	// And for a non-empty one: the same carrier around the constant 1 the
	// existential's inner result value is (PartitionSelectRule.java:264's
	// `LiteralValue.ofScalar(1)`).
	nonEmptyExistential := &QueryResult{Positional: scalarPositionalRow(int64(1))}

	outerRow := &QueryResult{Positional: &PositionalRow{
		Type:  ojLegTypeAV(),
		Slots: []any{int64(7), int64(8)},
	}}

	for _, tc := range []struct {
		name  string
		inner *QueryResult
		exist bool
	}{
		{"empty subplan", emptyExistential, false},
		{"non-empty subplan", nonEmptyExistential, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			legs, raw, err := b.legRows(outer.Name(), inner.Name(), outerRow, tc.inner)
			if err != nil {
				t.Fatalf("legRows: %v", err)
			}
			binder := &buildLegBinder{legs: legs, raw: raw}
			got, evErr := values.NewExistsValue(inner).Evaluate(
				&values.RowEvalContext{Correlations: binder})
			if evErr != nil {
				t.Fatalf("ExistsValue.Evaluate: %v", evErr)
			}
			if got != tc.exist {
				bound, _ := binder.GetCorrelationBinding(inner)
				t.Fatalf("EXISTS over a %s = %v, want %v (bound object %#v).\n"+
					"  The existential quantifier's object must be the DATUM the subplan\n"+
					"  computed, not the one-slot row Go wraps it in. Binding the CARRIER\n"+
					"  makes the object non-null whichever datum it holds, so EXISTS stops\n"+
					"  being able to report an empty subplan at all — a silent TRUE on every\n"+
					"  row, which is what this arm was added to stop.",
					tc.name, got, tc.exist, bound)
			}
		})
	}

	// The OUTER keeps its ROW. A projection over the outer reads its columns, so
	// unwrapping a one-column outer to its datum would hand a row-shaped
	// reference a scalar. Nothing in the corpus separates this from the inner
	// rule (measured), which is precisely why it is stated here.
	t.Run("scalar outer keeps its row", func(t *testing.T) {
		t.Parallel()
		legs, raw, err := b.legRows(outer.Name(), inner.Name(), emptyExistential, outerRow)
		if err != nil {
			t.Fatalf("legRows: %v", err)
		}
		if _, unwrapped := raw[outer]; unwrapped {
			t.Fatalf("the OUTER's one-slot scalar row was unwrapped to its datum "+
				"(raw = %#v). The two sides are bound differently on purpose, mirroring "+
				"computeResultLegs — which unwraps its inner and calls "+
				"qualifyOuterPositional on its outer — and the two paths disagreeing "+
				"about exactly this is the defect this arm exists to close.", raw)
		}
		if _, bound := legs[outer]; !bound {
			t.Fatalf("the OUTER bound no row at all; legs = %#v raw = %#v", legs, raw)
		}
	})
}
