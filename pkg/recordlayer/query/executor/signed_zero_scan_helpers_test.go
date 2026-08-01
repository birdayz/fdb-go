package executor

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestRejectContinuationForEmptyVectorScan(t *testing.T) {
	t.Parallel()
	if err := rejectContinuationForEmptyVectorScan(nil); err != nil {
		t.Fatalf("fresh empty scan: %v", err)
	}
	for _, continuation := range [][]byte{{}, {1, 2, 3}} {
		continuation := continuation
		t.Run(string(rune(len(continuation)+'0')), func(t *testing.T) {
			t.Parallel()
			err := rejectContinuationForEmptyVectorScan(continuation)
			var parseErr *recordlayer.ContinuationParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("error = %T(%v), want ContinuationParseError", err, err)
			}
			if !bytes.Equal(parseErr.RawBytes, continuation) {
				t.Fatalf("raw continuation = %v, want %v", parseErr.RawBytes, continuation)
			}
		})
	}
}

func TestVectorPartitionEqualityPrefix(t *testing.T) {
	t.Parallel()

	eqA := scanRangeTestEq(t, "a")
	eqB := scanRangeTestEq(t, float64(0))
	empty := predicates.EmptyComparisonRange()
	inequality := scanRangeTestComparison(
		t,
		predicates.ComparisonGreaterThan,
		values.LiteralValue(int64(1)),
	)
	types := []values.Type{values.NotNullString, values.NotNullDouble, values.NotNullLong}

	tests := []struct {
		name        string
		comparisons []*predicates.ComparisonRange
		wantLength  int
		wantErr     string
	}{
		{name: "no partition predicates", comparisons: nil},
		{name: "fixed width all unbound", comparisons: []*predicates.ComparisonRange{empty, nil}},
		{name: "one equality then empty", comparisons: []*predicates.ComparisonRange{eqA, empty}, wantLength: 1},
		{name: "two equalities then nil", comparisons: []*predicates.ComparisonRange{eqA, eqB, nil}, wantLength: 2},
		{name: "constraint after empty", comparisons: []*predicates.ComparisonRange{empty, eqB}, wantErr: "component 1 is constrained after an unbound"},
		{name: "constraint after nil", comparisons: []*predicates.ComparisonRange{nil, eqB}, wantErr: "component 1 is constrained after an unbound"},
		{name: "inequality", comparisons: []*predicates.ComparisonRange{eqA, inequality}, wantErr: "component 1 is not an equality"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotComparisons, gotTypes, err := vectorPartitionEqualityPrefix(test.comparisons, types)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(gotComparisons) != test.wantLength || len(gotTypes) != test.wantLength {
				t.Fatalf("prefix lengths = (%d comparisons, %d types), want %d", len(gotComparisons), len(gotTypes), test.wantLength)
			}
			for i := range gotTypes {
				if !gotTypes[i].Equals(types[i]) {
					t.Fatalf("type[%d] = %v, want %v", i, gotTypes[i], types[i])
				}
			}
		})
	}
}

func TestVectorPartitionEqualityPrefixFillsMissingPhysicalTypes(t *testing.T) {
	t.Parallel()

	comparisons, physicalTypes, err := vectorPartitionEqualityPrefix(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, "a"), scanRangeTestEq(t, float64(0))},
		[]values.Type{values.NotNullString},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparisons) != 2 || len(physicalTypes) != 2 {
		t.Fatalf("prefix lengths = (%d, %d), want (2, 2)", len(comparisons), len(physicalTypes))
	}
	if !physicalTypes[0].Equals(values.NotNullString) || !physicalTypes[1].Equals(values.UnknownType) {
		t.Fatalf("physical types = %v, want [STRING UNKNOWN]", physicalTypes)
	}
}

func TestValidateVectorPartitionPlanUsesMetadataTopology(t *testing.T) {
	t.Parallel()

	partitionedRoot := recordlayer.KeyWithValue(
		recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("vector")),
		1,
	)
	partitionedIndex := recordlayer.NewVectorIndex("vec", partitionedRoot, 2)
	validPartitioned := vectorPartitionValidationPlan(
		[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
		[]values.Type{values.NotNullLong},
		false,
	)
	if err := validateVectorPartitionPlan(partitionedIndex, validPartitioned); err != nil {
		t.Fatalf("valid partitioned plan: %v", err)
	}

	nonPartitionedIndex := recordlayer.NewVectorIndex("vec", recordlayer.Field("vector"), 2)
	if err := validateVectorPartitionPlan(
		nonPartitionedIndex,
		vectorPartitionValidationPlan(nil, nil, true),
	); err != nil {
		t.Fatalf("valid unpartitioned ordered plan: %v", err)
	}

	tests := []struct {
		name   string
		index  *recordlayer.Index
		plan   *plans.RecordQueryVectorIndexPlan
		reason string
	}{
		{
			name:   "comparison and type arity omitted",
			index:  partitionedIndex,
			plan:   vectorPartitionValidationPlan(nil, nil, false),
			reason: "plan arity does not match",
		},
		{
			name:   "partitioned ordered stream",
			index:  partitionedIndex,
			plan:   validPartitioned.WithOrderedStream(),
			reason: "ordered distance streaming is unsupported",
		},
		{
			name: "invalid split point",
			index: recordlayer.NewVectorIndex(
				"vec", recordlayer.KeyWithValue(recordlayer.Field("vector"), 1), 2,
			),
			plan: vectorPartitionValidationPlan(
				[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
				[]values.Type{values.NotNullLong},
				false,
			),
			reason: "leaves no valid vector value",
		},
		{
			name: "wrong metadata index type",
			index: &recordlayer.Index{
				Name: "vec", Type: recordlayer.IndexTypeValue, RootExpression: recordlayer.Field("vector"),
			},
			plan:   vectorPartitionValidationPlan(nil, nil, false),
			reason: "want a vector index",
		},
		{
			name: "partitioned SPFresh",
			index: &recordlayer.Index{
				Name: "vec", Type: recordlayer.IndexTypeVectorSPFresh, RootExpression: partitionedRoot,
			},
			plan:   validPartitioned,
			reason: "SPFresh does not support partitioned",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateVectorPartitionPlan(test.index, test.plan)
			var invalid *invalidVectorPartitionPlanError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %T(%v), want invalidVectorPartitionPlanError", err, err)
			}
			if !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error = %v, want containing %q", err, test.reason)
			}
		})
	}
}

func vectorPartitionValidationPlan(
	comparisons []*predicates.ComparisonRange,
	physicalTypes []values.Type,
	ordered bool,
) *plans.RecordQueryVectorIndexPlan {
	plan := plans.NewRecordQueryVectorIndexPlan(
		"vec",
		comparisons,
		values.LiteralValue([]float64{0, 1}),
		values.LiteralValue(2),
		predicates.ComparisonDistanceRankLessThanOrEq,
		nil,
		nil,
		[]string{"T"},
		values.UnknownType,
	).WithPartitionKeyComponentTypes(physicalTypes)
	if ordered {
		plan = plan.WithOrderedStream()
	}
	return plan
}

func TestPermutedAggregateGroupingLayout(t *testing.T) {
	t.Parallel()

	root := recordlayer.GroupBy(
		recordlayer.Field("value"),
		recordlayer.Field("a"),
		recordlayer.Field("b"),
	)

	tests := []struct {
		name       string
		index      *recordlayer.Index
		wantGroups int
		wantPrefix int
		wantErr    string
	}{
		{
			name:       "one trailing group is permuted",
			index:      recordlayer.NewPermutedMaxIndex("max_by_a", root, 1),
			wantGroups: 2,
			wantPrefix: 1,
		},
		{
			name:       "no grouping suffix is permuted",
			index:      recordlayer.NewPermutedMinIndex("min_by_ab", root, 0),
			wantGroups: 2,
			wantPrefix: 2,
		},
		{
			name:       "all grouping columns are permuted",
			index:      recordlayer.NewPermutedMinIndex("min_global_prefix", root, 2),
			wantGroups: 2,
			wantPrefix: 0,
		},
		{
			name: "missing option defaults to zero",
			index: &recordlayer.Index{
				Name:           "legacy_permuted",
				Type:           recordlayer.IndexTypePermutedMax,
				RootExpression: root,
				Options:        map[string]string{},
			},
			wantGroups: 2,
			wantPrefix: 2,
		},
		{
			name: "non grouping root",
			index: &recordlayer.Index{
				Name:           "bad_root",
				Type:           recordlayer.IndexTypePermutedMax,
				RootExpression: recordlayer.Field("value"),
				Options:        map[string]string{recordlayer.IndexOptionPermutedSize: "0"},
			},
			wantErr: "want *GroupingKeyExpression",
		},
		{
			name:    "not an integer",
			index:   permutedIndexWithOption(root, "not-a-number"),
			wantErr: "invalid permutedSize",
		},
		{
			name:    "negative",
			index:   permutedIndexWithOption(root, "-1"),
			wantErr: "invalid permutedSize",
		},
		{
			name:    "larger than grouping count",
			index:   permutedIndexWithOption(root, "3"),
			wantErr: "invalid permutedSize",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gotGroups, gotPrefix, err := permutedAggregateGroupingLayout(test.index)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotGroups != test.wantGroups || gotPrefix != test.wantPrefix {
				t.Fatalf("layout = (groups %d, prefix %d), want (%d, %d)", gotGroups, gotPrefix, test.wantGroups, test.wantPrefix)
			}
		})
	}
}

func permutedIndexWithOption(root recordlayer.KeyExpression, value string) *recordlayer.Index {
	return &recordlayer.Index{
		Name:           "bad_option",
		Type:           recordlayer.IndexTypePermutedMax,
		RootExpression: root,
		Options:        map[string]string{recordlayer.IndexOptionPermutedSize: value},
	}
}
