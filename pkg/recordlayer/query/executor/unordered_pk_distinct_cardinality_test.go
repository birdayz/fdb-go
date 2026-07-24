package executor

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestUnorderedPrimaryKeyDistinct_ExistentialCardinalityMatrix pins the
// cardinality repair needed when a query existential is matched to a candidate
// ForEach: zero candidate rows stay zero, while one or many fanout rows produce
// exactly one row per base-record primary key.
func TestUnorderedPrimaryKeyDistinct_ExistentialCardinalityMatrix(t *testing.T) {
	t.Parallel()

	pk1 := tuple.Tuple{"T", int64(1)}
	pk2 := tuple.Tuple{"T", int64(2)}
	tests := []struct {
		name string
		rows []QueryResult
		want int
	}{
		{
			name: "empty_existential",
			want: 0,
		},
		{
			name: "single_match",
			rows: []QueryResult{
				dmapPK(pk1, map[string]any{"FANOUT": "one"}),
			},
			want: 1,
		},
		{
			name: "many_matches_same_base",
			rows: []QueryResult{
				dmapPK(pk1, map[string]any{"FANOUT": "one"}),
				dmapPK(pk1, map[string]any{"FANOUT": "two"}),
				dmapPK(pk1, map[string]any{"FANOUT": "three"}),
			},
			want: 1,
		},
		{
			name: "many_matches_multiple_bases",
			rows: []QueryResult{
				dmapPK(pk1, map[string]any{"FANOUT": "one"}),
				dmapPK(pk1, map[string]any{"FANOUT": "two"}),
				dmapPK(pk2, map[string]any{"FANOUT": "one"}),
				dmapPK(pk2, map[string]any{"FANOUT": "two"}),
			},
			want: 2,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			alias := values.NamedCorrelationIdentifier(
				"existential_cardinality_" + strings.ReplaceAll(t.Name(), "/", "_"),
			)
			evalCtx := EmptyEvaluationContext()
			table := evalCtx.GetOrCreateTempTable(alias, nil)
			for _, row := range tc.rows {
				if err := table.Add(row); err != nil {
					t.Fatalf("seed temp table: %v", err)
				}
			}
			plan := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(
				plans.NewRecordQueryTempTableScanPlan(alias),
			)
			got := executePKDistinctCardinalityPlan(t, plan, evalCtx)
			if len(got) != tc.want {
				t.Fatalf("output cardinality = %d, want %d; rows = %#v", len(got), tc.want, got)
			}
		})
	}
}

// TestUnorderedPrimaryKeyDistinct_FiltersBeforeDedup is the discriminating
// ordering pin for compensation. If duplicate elimination runs first, the
// first same-PK row can be retained even though it fails a later residual,
// incorrectly discarding a later same-PK row that would pass.
func TestUnorderedPrimaryKeyDistinct_FiltersBeforeDedup(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("pk_distinct_filter_order")
	evalCtx := EmptyEvaluationContext()
	table := evalCtx.GetOrCreateTempTable(alias, nil)
	pk := tuple.Tuple{"T", int64(7)}
	for _, row := range []QueryResult{
		dmapPK(pk, map[string]any{"V": "drop"}),
		dmapPK(pk, map[string]any{"V": "keep"}),
	} {
		if err := table.Add(row); err != nil {
			t.Fatalf("seed temp table: %v", err)
		}
	}

	residual := predicates.NewComparisonPredicate(
		values.NewFieldValueWithResolvedOrdinal("V", 0, values.TypeString),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.LiteralValue("keep"),
		},
	)
	scan := plans.NewRecordQueryTempTableScanPlan(alias)

	correct := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(
		plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{residual},
			scan,
		),
	)
	correctRows := executePKDistinctCardinalityPlan(t, correct, evalCtx)
	if len(correctRows) != 1 || rowMap(correctRows[0])["V"] != "keep" {
		t.Fatalf("filter→PK-distinct rows = %#v, want the passing duplicate", correctRows)
	}

	// Discriminating control: the forbidden order drops the retained first row
	// in the filter and can no longer recover the later passing duplicate.
	wrong := plans.NewRecordQueryFilterPlan(
		[]predicates.QueryPredicate{residual},
		plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan),
	)
	if wrongRows := executePKDistinctCardinalityPlan(t, wrong, evalCtx); len(wrongRows) != 0 {
		t.Fatalf("PK-distinct→filter control rows = %#v, want zero", wrongRows)
	}
}

func executePKDistinctCardinalityPlan(
	t *testing.T,
	plan plans.RecordQueryPlan,
	evalCtx *EvaluationContext,
) []QueryResult {
	t.Helper()
	cursor, err := ExecutePlan(
		context.Background(),
		plan,
		nil,
		evalCtx,
		nil,
		recordlayer.DefaultExecuteProperties(),
	)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}
	defer cursor.Close()
	rows, err := CollectAll(context.Background(), cursor)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	return rows
}
