package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"

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
// :140), so there is no side for a rule to key on. Selected layout and explicit
// transport provenance distinguish the scalar envelope from a genuine record.
// Their logical field shapes may be identical, including the title `_0`; the
// anonymous-record and non-build tests below pin that distinction.
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
			legs, raw, err := b.legRows(outer, inner, outerRow, tc.inner)
			if err != nil {
				t.Fatalf("legRows: %v", err)
			}
			binder := &buildLegBinder{legs: legs, raw: raw}
			exists := mustExecutorConstruct(values.NewExistsValue(inner, values.NullableLong))
			got, evErr := exists.Evaluate(
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

	// A named one-column record stays whole on both sides, just like the
	// anonymous `_0` record in TestOrdinalJoinBuildAnonymousRecordRemainsWhole.
	// Neither field spelling nor the side of a FlatMap selects the object kind.
	t.Run("a named one-column leg keeps its row", func(t *testing.T) {
		t.Parallel()
		oneCol := &QueryResult{Positional: &PositionalRow{
			Type: values.NewRecordType("", false, []values.Field{
				{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
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
				legs, raw, err := b.legRows(outer, inner, side.outer, side.inner)
				if err != nil {
					t.Fatalf("legRows: %v", err)
				}
				if _, unwrapped := raw[side.corr]; unwrapped {
					t.Fatalf("a NAMED one-column leg (%s) was unwrapped to its datum "+
						"(raw = %#v).\n"+
						"  A genuine record remains a record regardless of its width, title,\n"+
						"  or join side; unwrapping it destroys ordinal field access.", side.name, raw)
				}
				if _, bound := legs[side.corr]; !bound {
					t.Fatalf("the one-column leg (%s) bound no row at all; legs = %#v raw = %#v",
						side.name, legs, raw)
				}
			})
		}
	})
}

func TestOrdinalJoinBuildAnonymousRecordRemainsWhole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
	}{
		{"present", int64(7)},
		{"null-valued", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			row := &QueryResult{Positional: &PositionalRow{
				Type: values.NewRecordType("", false, []values.Field{{
					Name: values.OrdinalFieldName(0), FieldType: values.NullableLong, Ordinal: 0,
				}}),
				Slots: []any{test.value},
			}}
			outer, inner := values.NamedCorrelationIdentifier("outer"), values.NamedCorrelationIdentifier("inner")
			for _, side := range []struct {
				name        string
				correlation values.CorrelationIdentifier
			}{
				{"outer", outer},
				{"inner", inner},
			} {
				t.Run(side.name, func(t *testing.T) {
					t.Parallel()
					build := &ordinalJoinBuild{Enabled: true}
					legs, raw, err := build.legRows(outer, inner, row, row)
					if err != nil {
						t.Fatal(err)
					}
					if _, unwrapped := raw[side.correlation]; unwrapped {
						t.Fatalf("genuine _0 record was unwrapped on %s: %#v", side.name, raw)
					}
					bound, present := legs[side.correlation]
					if !present || bound != row.Positional {
						t.Fatalf("%s record binding = (%#v, %v), want the whole original record", side.name, bound, present)
					}
					exists := mustExecutorConstruct(values.NewExistsValue(side.correlation, row.Positional.Type))
					got, err := exists.Evaluate(&values.RowEvalContext{Correlations: &buildLegBinder{legs: legs, raw: raw}})
					if err != nil || got != true {
						t.Fatalf("present record with scalar slot %v: EXISTS=(%v, %v), want true", test.value, got, err)
					}
				})
			}
		})
	}
}

func TestFlatMapNonBuildBindingUsesRecordOrDatumKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"record", "scalar"} {
		for _, presence := range []string{"value", "null-slot"} {
			for _, side := range []string{"outer", "inner"} {
				t.Run(kind+"/"+presence+"/"+side, func(t *testing.T) {
					t.Parallel()
					var datum any = int64(7)
					if presence == "null-slot" {
						datum = nil
					}
					seedPlan := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{values.NewNullValue(values.NullableLong)}))
					recordPlan := mustExecutorConstruct(plans.NewRecordQueryMapPlan(seedPlan, values.NewRawRecordConstructorValue(values.RecordConstructorField{
						Name: values.OrdinalFieldName(0), Value: values.NewNullValue(values.NullableLong),
					})))
					recordType := recordPlan.GetResultType().(*values.RecordType)
					row := &PositionalRow{Type: recordType, Slots: []any{datum}}
					var plan plans.RecordQueryPlan = recordPlan
					var objectType values.Type = recordType
					if kind == "scalar" {
						row = scalarPositionalRowOfType(datum, values.NullableLong)
						objectType = values.NullableLong
						plan = mustExecutorConstruct(plans.NewRecordQueryMapPlan(recordPlan, values.NewNullValue(values.NullableLong)))
					}
					if !row.Type.Equals(recordType) {
						t.Fatalf("record/scalar shape premise differs: %v vs %v", row.Type, recordType)
					}
					outer, inner := values.NamedCorrelationIdentifier("outer"), values.NamedCorrelationIdentifier("inner")
					correlation := outer
					if side == "inner" {
						correlation = inner
					}
					result := mustExecutorConstruct(values.NewExistsValue(correlation, objectType))
					cursor, err := newFlatMapCursorWithOuterProperties(recordlayer.FromList([]QueryResult{}), plan, plan, nil, EmptyEvaluationContext(), outer, inner, result, recordlayer.ExecuteProperties{}, false)
					if err != nil {
						t.Fatal(err)
					}
					defer cursor.Close()
					if cursor.build.enabled() {
						t.Fatal("non-build binding witness entered ordinal build instead")
					}
					input := QueryResult{Positional: row}
					got, err := cursor.computeResultLegs(input, &input)
					if err != nil {
						t.Fatal(err)
					}
					want := kind == "record" || datum != nil
					if got.Positional == nil || len(got.Positional.Slots) != 1 || got.Positional.Slots[0] != want {
						t.Fatalf("EXISTS on %s %s %s = %#v, want %v", side, kind, presence, got.Positional, want)
					}
				})
			}
		}
	}
}
