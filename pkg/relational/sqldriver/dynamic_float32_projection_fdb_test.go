package sqldriver_test

// Runtime FLOAT equality probes must be projected onto the discrete float32
// key domain without changing the predicate. Ordered FLOAT probes fail typed:
// even a correctly rounded endpoint cannot represent the logical predicate
// while stored raw negative/payload NaNs occupy incongruent tuple regions.

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

func TestFDB_DynamicFloat32IndexProjection(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), "dynamic-float32-projection"}.Pack())

	b := metadata.NewSchemaTemplateBuilder().SetName("dynamic_float32_projection")
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("F", api.NewFloatType(false), 2),
	}, []string{"ID"})
	b.AddIndex("T", "F_IDX", []string{"F"}, false)
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("T").Descriptor

	makeRec := func(id int64, value float32) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("F"), protoreflect.ValueOfFloat32(value))
		return m
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		for _, rec := range []proto.Message{
			makeRec(1, float32(math.Inf(-1))),
			makeRec(2, -math.MaxFloat32),
			makeRec(3, -math.SmallestNonzeroFloat32),
			makeRec(4, float32(math.Copysign(0, -1))),
			makeRec(5, 0),
			makeRec(6, math.SmallestNonzeroFloat32),
			makeRec(7, 16_777_216),
			makeRec(8, 16_777_218),
			makeRec(9, 16_777_220),
			makeRec(10, math.MaxFloat32),
			makeRec(11, float32(math.Inf(1))),
		} {
			if _, saveErr := store.SaveRecord(rec); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	comparisonRange := func(t *testing.T, comparisonType predicates.ComparisonType) *predicates.ComparisonRange {
		t.Helper()
		merged := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
			Type:    comparisonType,
			Operand: values.NewParameterValue(1),
		})
		if !merged.Ok {
			t.Fatalf("comparison range merge failed for %v", comparisonType)
		}
		return merged.Range
	}
	run := func(t *testing.T, comparisonType predicates.ComparisonType, parameter any) ([]int64, error) {
		t.Helper()
		plan, planErr := plans.NewRecordQueryIndexPlan(
			"F_IDX",
			[]*predicates.ComparisonRange{comparisonRange(t, comparisonType)},
			[]string{"T"},
			executor.PositionalTypeForDescriptor(desc),
			false,
		)
		if planErr != nil {
			t.Fatalf("construct exact FLOAT index plan: %v", planErr)
		}
		plan = plan.WithKeyComponentTypes([]values.Type{values.NotNullFloat})
		explain := plan.Explain()
		if !strings.Contains(explain, "IndexScan(F_IDX") || strings.Contains(explain, "Scan(T") {
			t.Fatalf("plan = %s, want the physical FLOAT index access path", explain)
		}

		var ids []int64
		_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, openErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if openErr != nil {
				return nil, openErr
			}
			cursor, execErr := executor.ExecutePlan(
				ctx,
				plan,
				store,
				executor.EmptyEvaluationContext().WithParams([]any{parameter}),
				nil,
				recordlayer.DefaultExecuteProperties(),
			)
			if execErr != nil {
				return nil, execErr
			}
			defer func() { _ = cursor.Close() }()
			results, collectErr := executor.CollectAll(ctx, cursor)
			if collectErr != nil {
				return nil, collectErr
			}
			for _, result := range results {
				row, ok := executor.RowValue(result).(map[string]any)
				if !ok {
					return nil, fmt.Errorf("row is %T, want map[string]any", executor.RowValue(result))
				}
				id, ok := row["ID"].(int64)
				if !ok {
					return nil, fmt.Errorf("ID is %T, want int64", row["ID"])
				}
				ids = append(ids, id)
			}
			return nil, nil
		})
		return ids, runErr
	}

	type testCase struct {
		name           string
		comparisonType predicates.ComparisonType
		parameter      any
		want           []int64
	}
	allThroughA := []int64{1, 2, 3, 4, 5, 6, 7}
	allFromB := []int64{8, 9, 10, 11}
	negativeThroughZero := []int64{1, 2, 3, 4, 5}
	positiveAfterZero := []int64{6, 7, 8, 9, 10, 11}
	negativeNonZero := []int64{1, 2, 3}
	zeroAndPositive := []int64{4, 5, 6, 7, 8, 9, 10, 11}
	allExceptPositiveInfinity := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	allExceptNegativeInfinity := []int64{2, 3, 4, 5, 6, 7, 8, 9, 10, 11}

	cases := []testCase{
		{"equals_integer_precision_cliff_is_empty", predicates.ComparisonEquals, int64(16_777_217), nil},
		{"not_distinct_precision_cliff_is_empty", predicates.ComparisonNotDistinctFrom, float64(16_777_217), nil},
		{"equals_positive_finite_overflow_is_empty", predicates.ComparisonEquals, math.MaxFloat64, nil},
		{"not_distinct_negative_finite_overflow_is_empty", predicates.ComparisonNotDistinctFrom, -math.MaxFloat64, nil},
		{"equals_exact_integer_control", predicates.ComparisonEquals, int64(16_777_216), []int64{7}},
		{"equals_negative_infinity_control", predicates.ComparisonEquals, math.Inf(-1), []int64{1}},
		{"equals_positive_infinity_control", predicates.ComparisonEquals, math.Inf(1), []int64{11}},

		// 16_777_217 is the tie-to-even midpoint between float32 16_777_216
		// and 16_777_218, so its wire projection rounds down to A.
		{"round_down_lt", predicates.ComparisonLessThan, float64(16_777_217), allThroughA},
		{"round_down_le", predicates.ComparisonLessThanOrEq, float64(16_777_217), allThroughA},
		{"round_down_gt", predicates.ComparisonGreaterThan, float64(16_777_217), allFromB},
		{"round_down_ge", predicates.ComparisonGreaterThanEq, float64(16_777_217), allFromB},

		// 16_777_217.5 projects up to B. The logically correct partition is
		// unchanged, but the endpoint adjustments are the opposite pair.
		{"round_up_lt", predicates.ComparisonLessThan, 16_777_217.5, allThroughA},
		{"round_up_le", predicates.ComparisonLessThanOrEq, 16_777_217.5, allThroughA},
		{"round_up_gt", predicates.ComparisonGreaterThan, 16_777_217.5, allFromB},
		{"round_up_ge", predicates.ComparisonGreaterThanEq, 16_777_217.5, allFromB},

		// Finite values outside float32's range project to infinities, but
		// remain strictly inside the actual infinity keys.
		{"positive_overflow_lt", predicates.ComparisonLessThan, math.MaxFloat64, allExceptPositiveInfinity},
		{"positive_overflow_le", predicates.ComparisonLessThanOrEq, math.MaxFloat64, allExceptPositiveInfinity},
		{"positive_overflow_gt", predicates.ComparisonGreaterThan, math.MaxFloat64, []int64{11}},
		{"positive_overflow_ge", predicates.ComparisonGreaterThanEq, math.MaxFloat64, []int64{11}},
		{"negative_overflow_lt", predicates.ComparisonLessThan, -math.MaxFloat64, []int64{1}},
		{"negative_overflow_le", predicates.ComparisonLessThanOrEq, -math.MaxFloat64, []int64{1}},
		{"negative_overflow_gt", predicates.ComparisonGreaterThan, -math.MaxFloat64, allExceptNegativeInfinity},
		{"negative_overflow_ge", predicates.ComparisonGreaterThanEq, -math.MaxFloat64, allExceptNegativeInfinity},

		// The smallest non-zero float64 values underflow to physical signed
		// zeros. Both zero encodings sit on the same logical side of each
		// non-zero threshold and must be included or excluded together.
		{"positive_underflow_eq", predicates.ComparisonEquals, math.SmallestNonzeroFloat64, nil},
		{"positive_underflow_not_distinct", predicates.ComparisonNotDistinctFrom, math.SmallestNonzeroFloat64, nil},
		{"positive_underflow_lt", predicates.ComparisonLessThan, math.SmallestNonzeroFloat64, negativeThroughZero},
		{"positive_underflow_le", predicates.ComparisonLessThanOrEq, math.SmallestNonzeroFloat64, negativeThroughZero},
		{"positive_underflow_gt", predicates.ComparisonGreaterThan, math.SmallestNonzeroFloat64, positiveAfterZero},
		{"positive_underflow_ge", predicates.ComparisonGreaterThanEq, math.SmallestNonzeroFloat64, positiveAfterZero},
		{"negative_underflow_eq", predicates.ComparisonEquals, -math.SmallestNonzeroFloat64, nil},
		{"negative_underflow_not_distinct", predicates.ComparisonNotDistinctFrom, -math.SmallestNonzeroFloat64, nil},
		{"negative_underflow_lt", predicates.ComparisonLessThan, -math.SmallestNonzeroFloat64, negativeNonZero},
		{"negative_underflow_le", predicates.ComparisonLessThanOrEq, -math.SmallestNonzeroFloat64, negativeNonZero},
		{"negative_underflow_gt", predicates.ComparisonGreaterThan, -math.SmallestNonzeroFloat64, zeroAndPositive},
		{"negative_underflow_ge", predicates.ComparisonGreaterThanEq, -math.SmallestNonzeroFloat64, zeroAndPositive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every case asserts ROWS, ordered comparisons included. They used
			// to short-circuit into an "unsupported ordering" error assertion
			// even though the expected row sets below were already written out
			// — the answer was known and the engine was refusing to compute it.
			got, err := run(t, tc.comparisonType, tc.parameter)
			if err != nil {
				t.Fatalf("execute %v %T(%v): %v", tc.comparisonType, tc.parameter, tc.parameter, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("FLOAT index %v %T(%v) = ids %v, want %v",
					tc.comparisonType, tc.parameter, tc.parameter, got, tc.want)
			}
		})
	}
}
