package cascades

import (
	"slices"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func nestedAggregateRowType() *values.RecordType {
	addr := func() *values.RecordType {
		return &values.RecordType{Nullable: true, Fields: []values.Field{
			{Name: "CITY", Ordinal: 0, FieldType: values.NullableString},
			{Name: "ZIP", Ordinal: 1, FieldType: values.NullableLong},
		}}
	}
	return values.NewRecordType("T_S", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "HOME", FieldType: addr()},
		{Name: "OFFICE", FieldType: addr()},
		{Name: "ZIP", FieldType: values.NullableLong},
		{Name: "CAT", FieldType: values.NullableString},
		{Name: "SOLO", FieldType: &values.RecordType{Nullable: true, Fields: []values.Field{
			{Name: "CITY", Ordinal: 0, FieldType: values.NullableString},
		}}},
	})
}

// nestedAggregateGroupBy groups T_S by the field paths (ordinal paths into
// nestedAggregateRowType) and SUMs the operand path.
func nestedAggregateGroupBy(t *testing.T, grouping [][]int, operand []int) *expressions.GroupByExpression {
	t.Helper()
	scan := mustAggregateDataConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T_S"}, nestedAggregateRowType()))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	row := mustAggregateDataConstruct(scanQ.RequireFlowedObjectValue())
	keys := make([]values.Value, len(grouping))
	for i, path := range grouping {
		keys[i] = mustAggregateDataConstruct(values.ResolveFieldOrdinals(row, path))
	}
	sum := expressions.AggregateSpec{
		Function: expressions.AggSum,
		Operand:  mustAggregateDataConstruct(values.ResolveFieldOrdinals(row, operand)),
	}
	return mustAggregateDataConstruct(expressions.NewGroupByExpression(keys, []expressions.AggregateSpec{sum}, scanQ))
}

// TestAggregateDataAccessRule_NestedColumnPaths: an aggregate candidate's
// grouping and aggregated columns are their full field paths, as the
// expansion's FieldValues are (AggregateIndexExpansionVisitor). A nested
// aggregated column is not creatable through SQL (both engines refuse it, the
// WS-J "sum_grouped_by_nested_and_top" shape) but is valid metadata, which Java
// matches; so it is pinned here, at the rule.
func TestAggregateDataAccessRule_NestedColumnPaths(t *testing.T) {
	t.Parallel()
	const (
		home, office, zip, cat, solo = 1, 2, 3, 4, 5
		leafCity, leafZip            = 0, 1
	)
	candidate := func(groupPaths [][]string, aggPath []string) *AggregateIndexMatchCandidate {
		groupCols := make([]string, len(groupPaths))
		groupTypes := make([]values.Type, len(groupPaths))
		for i, path := range groupPaths {
			groupCols[i] = path[len(path)-1]
			groupTypes[i] = values.NullableString
		}
		return NewAggregateIndexMatchCandidate("SUM_IDX", []string{"T_S"}, groupCols, expressions.AggSum,
			aggPath[len(aggPath)-1], nestedAggregateRowType(), groupTypes, len(groupCols)).
			WithColumnPaths(groupPaths, aggPath)
	}
	fires := func(gb *expressions.GroupByExpression, cand *AggregateIndexMatchCandidate) bool {
		t.Helper()
		results := mustFireExpressionRuleWithMemo(t, NewAggregateDataAccessRule(), expressions.InitialOf(gb),
			&indexTestPlanContext{candidates: []MatchCandidate{cand}}, nil)
		if len(results) > 0 {
			aggregateDataPublishedPlan(t, results[0], gb.GetResultValue().Type())
		}
		return len(results) > 0
	}
	for _, c := range []struct {
		name       string
		grouping   [][]int
		operand    []int
		groupPaths [][]string
		aggPath    []string
		want       bool
	}{
		{"nested_operand", [][]int{{cat}}, []int{home, leafZip}, [][]string{{"CAT"}}, []string{"HOME", "ZIP"}, true},
		{"nested_operand_other_parent", [][]int{{cat}}, []int{office, leafZip}, [][]string{{"CAT"}}, []string{"HOME", "ZIP"}, false},
		{"top_level_operand_same_leaf", [][]int{{cat}}, []int{zip}, [][]string{{"CAT"}}, []string{"HOME", "ZIP"}, false},
		{"top_level_candidate_nested_operand", [][]int{{cat}}, []int{home, leafZip}, [][]string{{"CAT"}}, []string{"ZIP"}, false},
		{"nested_grouping", [][]int{{home, leafCity}, {cat}}, []int{zip}, [][]string{{"HOME", "CITY"}, {"CAT"}}, []string{"ZIP"}, true},
		{"nested_grouping_other_parent", [][]int{{office, leafCity}, {cat}}, []int{zip}, [][]string{{"HOME", "CITY"}, {"CAT"}}, []string{"ZIP"}, false},
		// A RECORD-typed key expands to the candidate's leaves, but the result
		// value cannot be pulled up through them.
		{"record_key_over_leaves", [][]int{{home}}, []int{zip}, [][]string{{"HOME", "CITY"}, {"HOME", "ZIP"}}, []string{"ZIP"}, false},
		// One leaf: the widths agree, the RECORD-typed slot does not.
		{"record_key_over_one_leaf", [][]int{{solo}}, []int{zip}, [][]string{{"SOLO", "CITY"}}, []string{"ZIP"}, false},
		{"one_leaf_record_by_its_leaf", [][]int{{solo, leafCity}}, []int{zip}, [][]string{{"SOLO", "CITY"}}, []string{"ZIP"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := fires(nestedAggregateGroupBy(t, c.grouping, c.operand), candidate(c.groupPaths, c.aggPath)); got != c.want {
				t.Fatalf("AggregateDataAccessRule fired = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDescribeAggregateIndexKey drives every admission arm of the aggregate
// base expansion: a column is a scalar field path or the candidate declines.
func TestDescribeAggregateIndexKey(t *testing.T) {
	t.Parallel()
	field := func(name string, fan gen.Field_FanType) *gen.KeyExpression {
		return &gen.KeyExpression{Field: &gen.Field{FieldName: &name, FanType: fan.Enum()}}
	}
	scalar := func(name string) *gen.KeyExpression { return field(name, gen.Field_SCALAR) }
	nest := func(parent string, fan gen.Field_FanType, child *gen.KeyExpression) *gen.KeyExpression {
		return &gen.KeyExpression{Nesting: &gen.Nesting{
			Parent: &gen.Field{FieldName: &parent, FanType: fan.Enum()}, Child: child,
		}}
	}
	then := func(children ...*gen.KeyExpression) *gen.KeyExpression {
		return &gen.KeyExpression{Then: &gen.Then{Child: children}}
	}
	function := func(name string, argument *gen.KeyExpression) *gen.KeyExpression {
		return &gen.KeyExpression{Function: &gen.Function{Name: &name, Arguments: argument}}
	}
	for _, c := range []struct {
		name          string
		key           *gen.KeyExpression
		groupingCount int
		grouping      [][]string
		grouped       [][]string
		declines      bool
	}{
		{"flat", then(scalar("A"), scalar("V")), 1, [][]string{{"A"}}, [][]string{{"V"}}, false},
		{
			"nested_grouping_and_operand", then(nest("HOME", gen.Field_SCALAR, then(scalar("CITY"), scalar("ZIP"))), scalar("CAT"),
				nest("OFFICE", gen.Field_SCALAR, scalar("ZIP"))), 3,
			[][]string{{"HOME", "CITY"}, {"HOME", "ZIP"}, {"CAT"}},
			[][]string{{"OFFICE", "ZIP"}},
			false,
		},
		{
			"deep_nesting", nest("O", gen.Field_SCALAR, nest("I", gen.Field_SCALAR, scalar("A"))), 1,
			[][]string{{"O", "I", "A"}},
			[][]string{},
			false,
		},
		{"count_star_grouping_only", then(scalar("A"), scalar("B")), 2, [][]string{{"A"}, {"B"}}, [][]string{}, false},
		{"ungrouped_operand", scalar("V"), 0, [][]string{}, [][]string{{"V"}}, false},
		{"fan_out_field", then(field("TAGS", gen.Field_FAN_OUT), scalar("V")), 1, nil, nil, true},
		{"concatenated_field", then(field("TAGS", gen.Field_CONCATENATE), scalar("V")), 1, nil, nil, true},
		{"field_under_fan_out_parent", then(nest("ITEMS", gen.Field_FAN_OUT, scalar("SKU")), scalar("V")), 1, nil, nil, true},
		{"function_grouping", then(function("add", then(scalar("A"), scalar("B"))), scalar("V")), 1, nil, nil, true},
		{"version_grouping", then(&gen.KeyExpression{Version: &gen.Version{}}, scalar("V")), 1, nil, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			grouping, grouped, err := DescribeAggregateIndexKey(c.key, c.groupingCount)
			if c.declines {
				if err == nil {
					t.Fatalf("admitted %v / %v, want a decline", grouping, grouped)
				}
				return
			}
			if err != nil {
				t.Fatalf("declined: %v", err)
			}
			equal := func(a, b [][]string) bool { return slices.EqualFunc(a, b, slices.Equal[[]string]) }
			if !equal(grouping, c.grouping) || !equal(grouped, c.grouped) {
				t.Fatalf("paths = %v / %v, want %v / %v", grouping, grouped, c.grouping, c.grouped)
			}
		})
	}
}
