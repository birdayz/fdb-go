package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// A grouped SUM read from its index is a residue (0) for a live group whose
// non-NULL values were all deleted or NULLed, where SQL's SUM is NULL; the
// COUNT(*) companion proves the group live but not valued. A SUM over a
// nullable operand therefore needs a COUNT(col) companion over the same
// grouping and operand, and reads NULL where it reads 0 or nothing. Rows ride on
// aggregate_index_sum_null_residue.yaml and the vacated-group FDB oracle.

func nonNullCompanionRowType(vNullable bool) *values.RecordType {
	v := values.NullableLong
	if !vNullable {
		v = values.NotNullLong
	}
	return values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "G", FieldType: values.NullableString},
		{Name: "V", FieldType: v},
		{Name: "W", FieldType: values.NullableLong},
	})
}

func nonNullCompanionCandidate(name string, fn expressions.AggregateFunction, column string, countsRows, vNullable bool) *AggregateIndexMatchCandidate {
	return NewAggregateIndexMatchCandidate(name, []string{"T"}, []string{"G"}, fn, column,
		nonNullCompanionRowType(vNullable), []values.Type{values.NullableString}, 1).
		WithGroupExistence(countsRows, []byte("grouping-g")).
		WithGroupExistenceCompanionNeed(nil, !countsRows && fn != expressions.AggMin && fn != expressions.AggMax)
}

func TestNeedsNonNullCompanion(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		cand *AggregateIndexMatchCandidate
		want bool
	}{
		{"sum_over_nullable", nonNullCompanionCandidate("S", expressions.AggSum, "V", false, true), true},
		{"sum_over_not_null", nonNullCompanionCandidate("S", expressions.AggSum, "V", false, false), false},
		{"sum_over_unknown_layout", NewAggregateIndexMatchCandidate("S", []string{"T"}, []string{"G"},
			expressions.AggSum, "V", nil, []values.Type{values.NullableString}, 1), true},
		{"sum_over_unresolved_column", nonNullCompanionCandidate("S", expressions.AggSum, "NOPE", false, true), true},
		{"count_col", nonNullCompanionCandidate("C", expressions.AggCount, "V", false, true), false},
		{"count_star", nonNullCompanionCandidate("C", expressions.AggCount, "", true, true), false},
		{"ungrouped_sum", NewAggregateIndexMatchCandidate("S", []string{"T"}, nil, expressions.AggSum, "V",
			nonNullCompanionRowType(true), nil, 0), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.cand.NeedsNonNullCompanion(); got != c.want {
				t.Fatalf("NeedsNonNullCompanion = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFindNonNullCountCompanion(t *testing.T) {
	t.Parallel()
	owner := nonNullCompanionCandidate("S", expressions.AggSum, "V", false, true)
	countV := nonNullCompanionCandidate("C_V", expressions.AggCount, "V", false, true)
	countW := nonNullCompanionCandidate("C_W", expressions.AggCount, "W", false, true)
	countStar := nonNullCompanionCandidate("C", expressions.AggCount, "", true, true)
	otherGrouping := nonNullCompanionCandidate("C_V2", expressions.AggCount, "V", false, true).
		WithGroupExistence(false, []byte("grouping-h"))
	sparse := nonNullCompanionCandidate("C_V3", expressions.AggCount, "V", false, true).
		WithGroupExistenceCompanionNeed([]byte("p"), true)
	if got := findNonNullCountCompanion(owner, []MatchCandidate{owner, countW, countStar, otherGrouping, sparse}); got != nil {
		t.Fatalf("found %s, which counts another column, rows, grouping or row population", got.CandidateName())
	}
	if got := findNonNullCountCompanion(owner, []MatchCandidate{countStar, countW, countV}); got != countV {
		t.Fatalf("found %v, want C_V", got)
	}
}

func TestAggregateDataAccessRule_SumNeedsNonNullCompanion(t *testing.T) {
	t.Parallel()
	gb := mustAggregateDataConstruct(func() (*expressions.GroupByExpression, error) {
		scan, err := expressions.NewFullUnorderedScanExpression([]string{"T"}, nonNullCompanionRowType(true))
		if err != nil {
			return nil, err
		}
		q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
		row := mustAggregateDataConstruct(q.RequireFlowedObjectValue())
		g := mustAggregateDataConstruct(values.ResolveFieldOrdinals(row, []int{1}))
		v := mustAggregateDataConstruct(values.ResolveFieldOrdinals(row, []int{2}))
		return expressions.NewGroupByExpression([]values.Value{g},
			[]expressions.AggregateSpec{{Function: expressions.AggSum, Operand: v}}, q)
	}())
	sum := nonNullCompanionCandidate("S", expressions.AggSum, "V", false, true)
	countStar := nonNullCompanionCandidate("C", expressions.AggCount, "", true, true)
	countV := nonNullCompanionCandidate("C_V", expressions.AggCount, "V", false, true)
	fire := func(candidates ...MatchCandidate) []expressions.RelationalExpression {
		return mustFireExpressionRuleWithMemo(t, NewAggregateDataAccessRule(), expressions.InitialOf(gb),
			&indexTestPlanContext{candidates: candidates}, nil)
	}
	if results := fire(sum, countStar); len(results) != 0 {
		t.Fatalf("a SUM over a nullable operand was served with no COUNT(col) companion: %T", results[0])
	}
	results := fire(sum, countStar, countV)
	if len(results) == 0 {
		t.Fatal("the SUM with both companions was not served")
	}
	merge, ok := results[0].(*plans.RecordQueryMultiIntersectionOnValuesPlan)
	if !ok {
		t.Fatalf("want the group-existence merge, got %T", results[0])
	}
	if legs := len(merge.GetChildren()); legs != 3 || merge.DrivingStreamIndex() != 0 {
		t.Fatalf("merge has %d legs driven by %d, want the COUNT(*), SUM and COUNT(V) legs driven by the COUNT(*)\n  %s",
			legs, merge.DrivingStreamIndex(), merge.Explain())
	}
	rc, ok := merge.GetResultValue().(*values.RecordConstructorValue)
	if !ok || len(rc.Fields) != 2 {
		t.Fatalf("merge result is %v", merge.GetResultValue())
	}
	gated, ok := rc.Fields[1].Value.(*values.ScalarFunctionValue)
	if !ok || gated.FuncName != "IF" {
		t.Fatalf("the SUM is not gated by its COUNT(col): %v", rc.Fields[1].Value)
	}
}
