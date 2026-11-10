package executor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

// A typed runtime transform probe, not a planner rule: count exactly how many
// OLD rows were transformed, or fail/cancel on the second, before any save.
type partitionUpdateValue struct {
	calls *int
	run   func(int) (any, error)
}

func (*partitionUpdateValue) Children() []values.Value { return nil }
func (*partitionUpdateValue) Type() values.Type        { return values.NullableLong }
func (*partitionUpdateValue) Name() string             { return "partition-update-test-probe" }
func (value *partitionUpdateValue) Evaluate(any) (any, error) {
	*value.calls++
	return value.run(*value.calls)
}

func TestIntegration_PartitionUpdatePreEffectBounds(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{false, true} {
		for _, refusal := range []string{"materialization", "retained memory", "second transform", "second deadline"} {
			t.Run(fmt.Sprintf("sourceFirst=%t/%s", sourceFirst, refusal), func(t *testing.T) {
				ctx := context.Background()
				b := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).SetStoreRecordVersions(true)
				fields := []recordlayer.KeyExpression{recordlayer.Field("order_id"), recordlayer.Field("coord_x")}
				if sourceFirst {
					fields[0], fields[1] = fields[1], fields[0]
				}
				b.GetRecordType("Order").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.PartitionExpr(recordlayer.Field("coord_x"), 10), fields[0], fields[1]))
				b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("customer_id")))
				b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("id")))
				md, err := b.Build()
				if err != nil {
					t.Fatal(err)
				}
				root := testSubspace(t)
				_, err = testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(root).SetMetaDataProvider(md).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					for _, row := range []*demopb.Order{{OrderId: proto.Int64(1), CoordX: proto.Int64(-11), Price: proto.Int32(7)}, {OrderId: proto.Int64(1), CoordX: proto.Int64(0), Price: proto.Int32(8)}} {
						if _, err := store.SaveRecord(row); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					_, err := testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
						rc.Transaction().ClearRange(root)
						return nil, nil
					})
					if err != nil {
						t.Error(err)
					}
				}()
				var before []fdb.KeyValue
				_, err = testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(root).SetMetaDataProvider(md).Open()
					if err != nil {
						return nil, err
					}
					before, err = rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					statementCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					calls := 0
					transformFailure := errors.New("second OLD transform refused")
					value := &partitionUpdateValue{calls: &calls, run: func(call int) (any, error) {
						if call == 2 && refusal == "second transform" {
							return nil, transformFailure
						}
						if call == 2 && refusal == "second deadline" {
							cancel()
						}
						// Different NEW keys avoid duplicate-final refusal: the
						// memory/deadline/transform branch must be authoritative.
						return int64(100000 + call), nil
					}}
					rowType := PositionalTypeForRecordLayout((&demopb.Order{}).ProtoReflect().Descriptor(), true)
					scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false))
					update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(scan, "Order", []expressions.UpdateTransform{orderUpdate("coord_x", value)}))
					props := recordlayer.DefaultExecuteProperties()
					switch refusal {
					case "materialization":
						props = props.WithMaterializationLimit(1)
					case "retained memory":
						// A real versioned scan independently pins the bytes before
						// group preparation: OLD messages/PKs 35, NEW messages 16.
						control, err := ExecutePlan(ctx, scan, store, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
						if err != nil {
							return nil, err
						}
						rows, err := CollectAll(ctx, control)
						control.Close()
						if err != nil {
							return nil, err
						}
						if len(rows) != 2 {
							t.Fatalf("memory fixture rows=%d, want 2", len(rows))
						}
						var oldBytes, newBytes int
						for i, row := range rows {
							if row.Record.Version == nil || !row.Record.Version.IsComplete() {
								t.Fatal("memory fixture lost committed row versions")
							}
							oldBytes += proto.Size(row.Record.Record) + len(row.PrimaryKey.Pack())
							newRow := proto.Clone(row.Record.Record).(*demopb.Order)
							newRow.CoordX = proto.Int64(int64(100001 + i))
							newBytes += proto.Size(newRow)
						}
						if oldBytes != 35 || newBytes != 16 {
							t.Fatalf("memory fixture charges OLD=%d NEW=%d, want 35+16 before retained-key reservation", oldBytes, newBytes)
						}
						props.State = recordlayer.NewExecuteState(512)
					}
					cursor, executionErr := ExecutePlan(statementCtx, update, store, EmptyEvaluationContext(), nil, props)
					if cursor != nil {
						cursor.Close()
					}
					if executionErr == nil {
						t.Fatal("partition UPDATE did not refuse")
					}
					switch refusal {
					case "materialization":
						var limit *MaterializationLimitExceededError
						if !errors.As(executionErr, &limit) || calls != 0 {
							t.Fatalf("materialization refusal=%v transforms=%d", executionErr, calls)
						}
					case "retained memory":
						var limit *recordlayer.MemoryLimitExceededError
						if !errors.As(executionErr, &limit) || calls != 2 {
							t.Fatalf("memory refusal=%v transforms=%d (want after both NEW messages)", executionErr, calls)
						}
						// The two private row/key/collision reservations are 620
						// and 612 bytes: 35+1232=1267, versus 51 before the group.
						if limit.Used != 1267 || limit.Limit != 512 || props.State.MemUsed() != 1267 {
							t.Fatalf("retained-key charge used=%d limit=%d state=%d, want 1267/512/1267", limit.Used, limit.Limit, props.State.MemUsed())
						}
					case "second transform":
						if !errors.Is(executionErr, transformFailure) || calls != 2 {
							t.Fatalf("transform refusal=%v transforms=%d", executionErr, calls)
						}
					case "second deadline":
						if !errors.Is(executionErr, context.Canceled) || calls != 2 {
							t.Fatalf("deadline refusal=%v transforms=%d", executionErr, calls)
						}
					}
					visible, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					if !reflect.DeepEqual(visible, before) || rc.HasVersionMutations() {
						t.Fatal("pre-effect refusal changed body/index/protocol or pending versions")
					}
					return nil, nil // Deliberate Commit must remain legal here.
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					committed, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					if len(committed) == 0 || !reflect.DeepEqual(committed, before) {
						t.Fatal("deliberate Commit changed the whole store")
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
