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
// The unwrap is UNIFORM across both sides, as Java's is: Java decides from the
// VALUE's own static result type (QuantifiedObjectValue.java:82-95), and the
// two FlatMap bindings are the identical call (RecordQueryFlatMapPlan.java:135,
// :140), so there is no side for a rule to key on. What keeps a row-shaped leg
// out of the unwrap is the SHAPE test, not a role: isBareScalarRow matches the
// 1-slot `_0` carrier, and a genuine one-column leg carries the COLUMN's name.
// That is the invariant the last subtest pins.
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

	// A NAMED ONE-COLUMN LEG KEEPS ITS ROW — the invariant that makes the
	// uniform unwrap safe, and the only thing standing between it and a
	// projection over a one-column source being handed a scalar.
	//
	// The unwrap's test is the CARRIER's shape (`_0`), not arity: a leg
	// projecting a single real column carries that column's NAME, so it is not
	// bare and the unwrap cannot reach it. Both sides are asserted because
	// neither side is exempt — the rule is uniform, so a regression on either is
	// the same regression.
	//
	// This replaces an earlier subtest that fed a `_0`-shaped carrier in as the
	// OUTER and asserted it kept its row. That shape cannot arise from a real
	// outer, so the assertion pinned the role flag rather than any property of
	// the data — and it went green either way once the flag was gone.
	t.Run("a named one-column leg keeps its row", func(t *testing.T) {
		t.Parallel()
		oneCol := &QueryResult{Positional: &PositionalRow{
			Type: values.NewRecordType("", false, []values.Field{
				{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
			}),
			Slots: []any{int64(7)},
		}}
		for _, side := range []struct {
			name         string
			outer, inner *QueryResult
			corr         values.CorrelationIdentifier
		}{
			{"as outer", oneCol, nonEmptyExistential, outer},
			{"as inner", outerRow, oneCol, inner},
		} {
			t.Run(side.name, func(t *testing.T) {
				t.Parallel()
				legs, raw, err := b.legRows(outer.Name(), inner.Name(), side.outer, side.inner)
				if err != nil {
					t.Fatalf("legRows: %v", err)
				}
				if _, unwrapped := raw[side.corr]; unwrapped {
					t.Fatalf("a NAMED one-column leg (%s) was unwrapped to its datum "+
						"(raw = %#v).\n"+
						"  The datum unwrap keys on the one-slot `_0` CARRIER Go wraps a\n"+
						"  computed scalar in — Java's \"result type is not a record\" test.\n"+
						"  A leg projecting one real column carries that column's NAME and is\n"+
						"  a row; unwrapping it hands a scalar to every reference that reads\n"+
						"  its columns. If the unwrap's test was widened to arity, this is the\n"+
						"  regression.", side.name, raw)
				}
				if _, bound := legs[side.corr]; !bound {
					t.Fatalf("the one-column leg (%s) bound no row at all; legs = %#v raw = %#v",
						side.name, legs, raw)
				}
			})
		}
	})
}
