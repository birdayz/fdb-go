package sqldriver_test

// Aggregate indexes are physical scans too: an equality on a signed-zero group
// key must read both physical groups while retaining later grouping-key bounds.
// The executor leaf is constructed directly so this physical consumer is pinned
// independently of cost-based planner selection.

import (
	"context"
	"fmt"
	"math"
	"reflect"
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

func TestFDB_AggregateIndexSignedZeroRangeSet(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	for _, width := range []string{"DOUBLE", "FLOAT"} {
		t.Run(width, func(t *testing.T) {
			runAggregateSignedZeroRangeSet(t, width)
		})
	}
}

func runAggregateSignedZeroRangeSet(t *testing.T, width string) {
	t.Helper()
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name(), "aggregate", width}.Pack())

	var groupDataType api.DataType
	var groupValueType values.Type
	if width == "FLOAT" {
		groupDataType = api.NewFloatType(false)
		groupValueType = values.NotNullFloat
	} else {
		groupDataType = api.NewDoubleType(false)
		groupValueType = values.NotNullDouble
	}
	b := metadata.NewSchemaTemplateBuilder().SetName("aggregate_signed_zero_" + width)
	b.AddTable("GA", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("G", groupDataType, 2),
		metadata.NewColumnSpec("W", api.NewLongType(false), 3),
		metadata.NewColumnSpec("VAL", api.NewLongType(false), 4),
	}, []string{"ID"})
	b.AddAggregateIndex("GA", "SUM_GW", []string{"G", "W"}, "SUM", "VAL")
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("GA").Descriptor

	makeRec := func(id int64, group float64, w, val int64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		if width == "FLOAT" {
			m.Set(desc.Fields().ByName("G"), protoreflect.ValueOfFloat32(float32(group)))
		} else {
			m.Set(desc.Fields().ByName("G"), protoreflect.ValueOfFloat64(group))
		}
		m.Set(desc.Fields().ByName("W"), protoreflect.ValueOfInt64(w))
		m.Set(desc.Fields().ByName("VAL"), protoreflect.ValueOfInt64(val))
		return m
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, openErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if openErr != nil {
			return nil, openErr
		}
		for _, rec := range []proto.Message{
			makeRec(1, math.Copysign(0, -1), 5, 10),
			makeRec(2, math.Copysign(0, -1), 5, 20),
			makeRec(3, 0, 5, 70),
			makeRec(4, math.Copysign(0, -1), 9, 900),
			makeRec(5, 0, 1, 100),
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

	comparisonRange := func(t *testing.T, comparison predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatalf("comparison range merge failed: %v", comparison)
		}
		return merged.Range
	}
	groupRange := comparisonRange(t, predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewParameterValue(1),
	})
	wRange := comparisonRange(t, predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(5)))

	type aggregateRow struct {
		negative bool
		sum      int64
	}
	run := func(t *testing.T, parameter float64, reverse bool) []aggregateRow {
		t.Helper()
		indexPlan := plans.NewRecordQueryIndexPlan(
			"SUM_GW",
			[]*predicates.ComparisonRange{groupRange, wRange},
			[]string{"GA"},
			values.UnknownType,
			reverse,
		).WithKeyComponentTypes([]values.Type{groupValueType, values.NotNullLong})
		aggregatePlan := plans.NewRecordQueryAggregateIndexPlan(
			indexPlan,
			"GA",
			values.UnknownType,
			"SUM",
		).WithGroupColumns([]string{"G", "W"}, "VAL")

		var out []aggregateRow
		_, runErr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, openErr := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if openErr != nil {
				return nil, openErr
			}
			cursor, execErr := executor.ExecutePlan(
				ctx,
				aggregatePlan,
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
				// Slots by POSITION, matching the projection (G, W, SUM(VAL)).
				// Reading them by name would resolve each one regardless of which
				// slot it actually occupies, so a permuted row — a mis-bound window
				// — would type-assert cleanly and answer correctly here.
				slots := positionalSlots(result)
				if len(slots) != 3 {
					return nil, fmt.Errorf("aggregate row has %d slots, want 3 (G, W, SUM(VAL))", len(slots))
				}
				group, ok := slots[0].(float64)
				if !ok {
					return nil, fmt.Errorf("aggregate G is %T, want float64", slots[0])
				}
				w, ok := slots[1].(int64)
				if !ok || w != 5 {
					return nil, fmt.Errorf("aggregate suffix W is %T(%v), want int64(5)", slots[1], slots[1])
				}
				sum, ok := slots[2].(int64)
				if !ok {
					return nil, fmt.Errorf("aggregate SUM(VAL) is %T, want int64", slots[2])
				}
				out = append(out, aggregateRow{negative: math.Signbit(group), sum: sum})
			}
			return nil, nil
		})
		if runErr != nil {
			t.Fatalf("execute aggregate index plan: %v", runErr)
		}
		return out
	}

	for _, parameter := range []float64{0, math.Copysign(0, -1)} {
		forward := run(t, parameter, false)
		wantForward := []aggregateRow{{negative: true, sum: 30}, {negative: false, sum: 70}}
		if !reflect.DeepEqual(forward, wantForward) {
			t.Fatalf("%s forward aggregate parameter signbit=%v = %v, want %v",
				width, math.Signbit(parameter), forward, wantForward)
		}
		reverse := run(t, parameter, true)
		wantReverse := []aggregateRow{{negative: false, sum: 70}, {negative: true, sum: 30}}
		if !reflect.DeepEqual(reverse, wantReverse) {
			t.Fatalf("%s reverse aggregate parameter signbit=%v = %v, want %v",
				width, math.Signbit(parameter), reverse, wantReverse)
		}
	}
}
