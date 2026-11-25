package executor

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The filter executors drop a row whose predicate evaluates to UNKNOWN. They
// never pass it as TRUE. That single line — `res != predicates.TriTrue` in both
// executeFilter and executePredicatesFilter — is the ground truth underneath
// RFC-210's R2, and until this file it was asserted nowhere.
//
// R2's whole argument is: if a conjunct strictly between the scan and the
// DISTINCT rejects NULL on every key column of a UNIQUE index, then no row
// reaching the DISTINCT carries a NULL key component, so the index's exempt set
// (the rows NULLS DISTINCT lets it hold arbitrarily many of) is EMPTY on this
// stream — and the DISTINCT may be removed outright. The step that makes the
// exempt set empty is not the planner's; it is the executor DROPPING the
// NULL-keyed rows. A row whose key column is NULL evaluates `email <> 'z'` and
// `email = <anything>` to UNKNOWN, and if the executor ever treated UNKNOWN as
// TRUE those rows would flow to a DISTINCT that is no longer there, and the
// statement would return the duplicate NULLs R2 licensed itself to ignore.
//
// The dangerous property of that failure is that it is INVISIBLE to every one of
// R2's own tests: the planner still proves what it proved, the plan still has no
// operator, the EXPLAIN still carries the stamp. Only the rows change. So the
// pin belongs here, at the executor, named as R2's ground rather than as a
// generic three-valued-logic check.
//
// Both filter plan types are covered because R2 admits either as the
// NULL-rejecting conjunct's carrier: the Cascades path builds a
// RecordQueryPredicatesFilterPlan and the legacy path a RecordQueryFilterPlan,
// and the two hold the drop decision in two separate copies of the loop.
func TestFilterDropsUnknownRows_R2Ground(t *testing.T) {
	t.Parallel()

	// The NULL-bearing stream a nullable UNIQUE key produces. Two NULLs, because
	// one NULL cannot express the duplicate: under NULLS DISTINCT the index holds
	// both, and it is exactly this pair that R2's elided DISTINCT would no longer
	// collapse if the filter let them through.
	rows := []map[string]any{
		{"EMAIL": "a@example.com"},
		{"EMAIL": nil},
		{"EMAIL": "b@example.com"},
		{"EMAIL": nil},
	}
	rowType := exactTestRowType(values.Field{Name: "EMAIL", FieldType: values.TypeString})
	directEmail := mustNamedTestField(t, "EMAIL", values.TypeString)

	cases := []struct {
		name string
		cmp  predicates.Comparison
		want []any
		why  string
	}{
		{
			// R2 admits NotEquals precisely because `NULL <> 'z'` is UNKNOWN and
			// not TRUE. It is the allow-list entry whose admission is a claim
			// about the executor rather than about a scan range, so it is the
			// one that most needs the executor to hold up its end.
			name: "not_equals_drops_null",
			cmp: predicates.Comparison{
				Type:    predicates.ComparisonNotEquals,
				Operand: &values.ConstantValue{Value: "zzz@example.com", Typ: values.NotNullString},
			},
			want: []any{"a@example.com", "b@example.com"},
			why: "`EMAIL <> 'zzz@example.com'` is UNKNOWN for a NULL EMAIL. If the " +
				"NULL rows survive, R2's exempt-set-is-empty conclusion is false on " +
				"the stream it was drawn about",
		},
		{
			// `col = NULL` is UNKNOWN for EVERY row including the NULL ones —
			// the shape a user writes when they mean IS NULL. Nothing survives.
			// Its value here is that it separates "UNKNOWN is dropped" from
			// "NULL operands are dropped": an executor that passed UNKNOWN as
			// TRUE would return all four rows on a predicate that is true of
			// none of them.
			name: "equals_null_drops_everything",
			cmp: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: values.NewNullValue(values.TypeString),
			},
			want: nil,
			why: "`EMAIL = NULL` is UNKNOWN for every row. Passing UNKNOWN as TRUE " +
				"turns a predicate no row satisfies into one every row satisfies",
		},
		{
			// The discriminating control. Without it the two rows above are
			// satisfiable by an executor that drops everything unconditionally,
			// which would also make R2 look sound while returning no rows at all.
			name: "control_equals_keeps_the_true_row",
			cmp: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: &values.ConstantValue{Value: "a@example.com", Typ: values.NotNullString},
			},
			want: []any{"a@example.com"},
			why:  "a TRUE predicate must still pass its row",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			directPred := predicates.NewComparisonPredicate(directEmail, tc.cmp)

			// The tri-state itself, asserted before the executor is asked about
			// it. Without this the row-count assertions below cannot tell a
			// correctly-dropped UNKNOWN from a predicate that merely returned
			// FALSE, and R2's admission of NotEquals rests on the former.
			if tc.name != "control_equals_keeps_the_true_row" {
				got, err := directPred.Eval(&PositionalRow{Type: rowType, Slots: []any{nil}})
				if err != nil {
					t.Fatalf("Eval over a NULL EMAIL: %v", err)
				}
				if got != predicates.TriUnknown {
					t.Fatalf("`EMAIL %v` over a NULL EMAIL evaluates to %v, want UNKNOWN. "+
						"R2 admits this comparison kind to its allow-list ONLY because a "+
						"NULL subject makes it UNKNOWN; if it now yields a definite value "+
						"the allow-list entry has to be re-derived, not the test relaxed",
						tc.cmp.Type, got)
				}
			}

			for _, carrier := range []struct {
				kind string
				plan func(inner plans.RecordQueryPlan, pred predicates.QueryPredicate) plans.RecordQueryPlan
			}{
				{"RecordQueryFilterPlan", func(inner plans.RecordQueryPlan, pred predicates.QueryPredicate) plans.RecordQueryPlan {
					return mustExecutorConstruct(plans.NewRecordQueryFilterPlan([]predicates.QueryPredicate{pred}, inner))
				}},
				{"RecordQueryPredicatesFilterPlan", func(inner plans.RecordQueryPlan, pred predicates.QueryPredicate) plans.RecordQueryPlan {
					return mustExecutorConstruct(plans.NewRecordQueryPredicatesFilterPlan(
						inner, []predicates.QueryPredicate{pred}))
				}},
			} {
				carrier := carrier
				t.Run(carrier.kind, func(t *testing.T) {
					t.Parallel()
					alias := values.NamedCorrelationIdentifier(
						"r2_ground_" + tc.name + "_" + carrier.kind)
					evalCtx := EmptyEvaluationContext()
					table := evalCtx.GetOrCreateTempTable(alias, nil)
					for i, m := range rows {
						if err := table.Add(QueryResult{
							Positional: &PositionalRow{Type: rowType, Slots: []any{m["EMAIL"]}},
							PrimaryKey: tuple.Tuple{"T", int64(i)},
						}); err != nil {
							t.Fatalf("seed temp table: %v", err)
						}
					}
					scan := mustTempTableScan(t, evalCtx, alias)
					planPred := predicates.NewComparisonPredicate(
						mustTestFieldOrdinal(t, scan.GetResultValue(), 0), tc.cmp)
					plan := carrier.plan(scan, planPred)
					out := executePKDistinctCardinalityPlan(t, plan, evalCtx)

					got := make([]any, 0, len(out))
					for _, row := range out {
						got = append(got, rowMap(row)["EMAIL"])
					}
					if len(got) != len(tc.want) {
						t.Fatalf("%s emitted %d rows %v, want %d %v.\n%s",
							carrier.kind, len(got), got, len(tc.want), tc.want, tc.why)
					}
					for i := range got {
						if got[i] != tc.want[i] {
							t.Fatalf("%s row %d = %v, want %v.\n%s",
								carrier.kind, i, got[i], tc.want[i], tc.why)
						}
					}
				})
			}
		})
	}
}
