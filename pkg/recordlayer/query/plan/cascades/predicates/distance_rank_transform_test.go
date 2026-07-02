package predicates

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func intPtr(i int) *int { return &i }

// rowNumberOverDistance builds ROW_NUMBER() OVER (PARTITION BY part
// ORDER BY <op>(field, queryVec)) with the given HNSW knobs.
func rowNumberOverDistance(op values.DistanceOperator, part, field, queryVec values.Value, efSearch *int) *values.RowNumberValue {
	dist := values.NewDistanceValue(op, field, queryVec)
	var partitions []values.Value
	if part != nil {
		partitions = []values.Value{part}
	}
	return values.NewRowNumberValue(partitions, []values.Value{dist}, efSearch, nil)
}

func TestTransformRowNumberDistanceRank_Euclidean(t *testing.T) {
	t.Parallel()
	field := values.LiteralValue("embedding")
	queryVec := values.LiteralValue([]float64{1, 0, 0})
	part := values.LiteralValue("zone")
	rn := rowNumberOverDistance(values.DistanceEuclidean, part, field, queryVec, intPtr(100))

	pred, ok := TransformRowNumberDistanceRankMaybe(rn, ComparisonLessThanOrEq, values.LiteralValue(3))
	if !ok {
		t.Fatal("expected transform to fire for euclidean_distance ROW_NUMBER <= 3")
	}
	lhs, ok := pred.Operand.(*values.EuclideanDistanceRowNumberValue)
	if !ok {
		t.Fatalf("LHS is %T, want *EuclideanDistanceRowNumberValue", pred.Operand)
	}
	if len(lhs.PartitioningValues) != 1 || lhs.PartitioningValues[0] != part {
		t.Errorf("partitioning values = %v, want [%v]", lhs.PartitioningValues, part)
	}
	// The row-number value's argument is the indexed field only — the query
	// vector moves into the comparison.
	if len(lhs.ArgumentValues) != 1 || lhs.ArgumentValues[0] != field {
		t.Errorf("argument values = %v, want [%v] (index field)", lhs.ArgumentValues, field)
	}
	cmp := pred.Comparison
	if cmp.Type != ComparisonDistanceRankLessThanOrEq {
		t.Errorf("comparison type = %v, want ComparisonDistanceRankLessThanOrEq", cmp.Type)
	}
	if cmp.QueryVector != queryVec {
		t.Errorf("query vector = %v, want %v", cmp.QueryVector, queryVec)
	}
	if cmp.Operand == nil {
		t.Errorf("comparand (k) = <nil>, want 3")
	} else if v, _ := cmp.Operand.Evaluate(nil); v != 3 {
		t.Errorf("comparand (k) = %v, want 3", cmp.Operand)
	}
	if cmp.EfSearch == nil || *cmp.EfSearch != 100 {
		t.Errorf("ef_search = %v, want 100", cmp.EfSearch)
	}
}

func TestTransformRowNumberDistanceRank_MetricAndOpMapping(t *testing.T) {
	t.Parallel()
	field := values.LiteralValue("v")
	q := values.LiteralValue([]float64{1})
	cases := []struct {
		name    string
		op      values.DistanceOperator
		cmpIn   ComparisonType
		wantCmp ComparisonType
		assert  func(t *testing.T, lhs values.Value)
	}{
		{
			"cosine <", values.DistanceCosine, ComparisonLessThan, ComparisonDistanceRankLessThan,
			func(t *testing.T, lhs values.Value) {
				if _, ok := lhs.(*values.CosineDistanceRowNumberValue); !ok {
					t.Errorf("LHS %T, want *CosineDistanceRowNumberValue", lhs)
				}
			},
		},
		{
			"dot product <", values.DistanceDotProduct, ComparisonLessThan, ComparisonDistanceRankLessThan,
			func(t *testing.T, lhs values.Value) {
				if _, ok := lhs.(*values.DotProductDistanceRowNumberValue); !ok {
					t.Errorf("LHS %T, want *DotProductDistanceRowNumberValue", lhs)
				}
			},
		},
		{
			"euclidean square <=", values.DistanceEuclideanSquare, ComparisonLessThanOrEq, ComparisonDistanceRankLessThanOrEq,
			func(t *testing.T, lhs values.Value) {
				if _, ok := lhs.(*values.EuclideanSquareDistanceRowNumberValue); !ok {
					t.Errorf("LHS %T, want *EuclideanSquareDistanceRowNumberValue", lhs)
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rn := rowNumberOverDistance(tc.op, nil, field, q, nil)
			pred, ok := TransformRowNumberDistanceRankMaybe(rn, tc.cmpIn, values.LiteralValue(5))
			if !ok {
				t.Fatalf("transform did not fire for %s", tc.name)
			}
			if pred.Comparison.Type != tc.wantCmp {
				t.Errorf("comparison type = %v, want %v", pred.Comparison.Type, tc.wantCmp)
			}
			tc.assert(t, pred.Operand)
		})
	}
}

// TestDistanceRankComparison_EvalPanics pins that a DistanceRank comparison
// must be lowered to a vector scan during planning. If
// it ever reaches row-by-row evaluation, fail loud rather than silently
// returning UNKNOWN (which would drop every row and pass green).
func TestDistanceRankComparison_EvalPanics(t *testing.T) {
	t.Parallel()
	cmp, ok := NewDistanceRankComparison(
		ComparisonDistanceRankLessThanOrEq,
		values.LiteralValue([]float64{1, 0, 0}),
		values.LiteralValue(3), nil, nil)
	if !ok {
		t.Fatal("NewDistanceRankComparison rejected a valid LessThanOrEq type")
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when a DistanceRank comparison is evaluated row-by-row")
		}
	}()
	cmp.EvalAgainst(int64(1), int64(3))
}

// TestTransformRowNumberDistanceRank_EqualsRejected pins Java conformance:
// `ROW_NUMBER() = K` maps to DISTANCE_RANK_EQUALS in the switch but Java's
// DistanceRankValueComparison constructor rejects it
// (Verify.verify allows only LESS_THAN / LESS_THAN_OR_EQUAL). So the transform
// must NOT fire for `=`, leaving the row-number comparison un-lowered (which
// then fails to plan) rather than silently degrading to a top-K scan.
func TestTransformRowNumberDistanceRank_EqualsRejected(t *testing.T) {
	t.Parallel()
	field := values.LiteralValue("v")
	q := values.LiteralValue([]float64{1})
	rn := rowNumberOverDistance(values.DistanceEuclidean, nil, field, q, nil)
	if _, ok := TransformRowNumberDistanceRankMaybe(rn, ComparisonEquals, values.LiteralValue(3)); ok {
		t.Fatal("transform fired for `= K`; Java rejects DISTANCE_RANK_EQUALS at construction")
	}
	// And NewDistanceRankComparison itself rejects the EQUALS type (mirrors Verify).
	if _, ok := NewDistanceRankComparison(ComparisonDistanceRankEquals,
		q, values.LiteralValue(3), nil, nil); ok {
		t.Fatal("NewDistanceRankComparison accepted DISTANCE_RANK_EQUALS")
	}
}

func TestTransformRowNumberDistanceRank_NoMatch(t *testing.T) {
	t.Parallel()
	field := values.LiteralValue("v")
	q := values.LiteralValue([]float64{1})

	// Non-distance argument → no transform.
	rnPlain := values.NewRowNumberValue(nil, []values.Value{values.LiteralValue(1)}, nil, nil)
	if _, ok := TransformRowNumberDistanceRankMaybe(rnPlain, ComparisonLessThanOrEq, values.LiteralValue(3)); ok {
		t.Error("transform fired on non-distance ROW_NUMBER argument")
	}

	// More than one argument → bail out.
	dist := values.NewDistanceValue(values.DistanceEuclidean, field, q)
	rnMulti := values.NewRowNumberValue(nil, []values.Value{dist, values.LiteralValue(2)}, nil, nil)
	if _, ok := TransformRowNumberDistanceRankMaybe(rnMulti, ComparisonLessThanOrEq, values.LiteralValue(3)); ok {
		t.Error("transform fired on multi-argument ROW_NUMBER")
	}

	// Unsupported comparison type (>) → no transform.
	rn := rowNumberOverDistance(values.DistanceEuclidean, nil, field, q, nil)
	if _, ok := TransformRowNumberDistanceRankMaybe(rn, ComparisonGreaterThan, values.LiteralValue(3)); ok {
		t.Error("transform fired on unsupported comparison (>)")
	}

	// nil receiver → no panic, no match.
	if _, ok := TransformRowNumberDistanceRankMaybe(nil, ComparisonEquals, values.LiteralValue(1)); ok {
		t.Error("transform fired on nil RowNumberValue")
	}
}
