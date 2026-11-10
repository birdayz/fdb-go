package executor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
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

func TestIntegration_PartitionUpdateDuplicatePhysicalInputs(t *testing.T) {
	t.Parallel()
	for _, sourceFirst := range []bool{false, true} {
		for _, format := range []int32{4, 14} {
			t.Run(fmt.Sprintf("sourceFirst=%t/format=%d", sourceFirst, format), func(t *testing.T) {
				ctx := t.Context()
				b := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).SetStoreRecordVersions(true).SetSplitLongRecords(true)
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
				open := func(rc *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
					return recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(root).SetMetaDataProvider(md).SetFormatVersion(format).CreateOrOpen()
				}
				defer func() {
					_, err := testDB.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
						rc.Transaction().ClearRange(root)
						return nil, nil
					})
					if err != nil {
						t.Error(err)
					}
				}()
				key := func(source, coordinate int64) tuple.Tuple {
					pk := tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), coordinate}
					if sourceFirst {
						return append(pk, source, int64(1))
					}
					return append(pk, int64(1), source)
				}
				moves := []struct {
					oldSource, oldP, newSource, newP int64
					price                            int32
				}{{-11, -2, 989, 98, 7}, {0, 0, 1000, 100, 8}}
				_, err = testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := open(rc)
					if err != nil {
						return nil, err
					}
					for _, move := range moves {
						if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), CoordX: proto.Int64(move.oldSource), Price: proto.Int32(move.price)}); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				oldVersions := make(map[string]*recordlayer.FDBRecordVersion)
				_, err = testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := open(rc)
					if err != nil {
						return nil, err
					}
					rowType := PositionalTypeForRecordLayout((&demopb.Order{}).ProtoReflect().Descriptor(), true)
					scan := mustExecutorConstruct(plans.NewRecordQueryScanPlan([]string{"Order"}, rowType, false))
					union := mustExecutorConstruct(plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{scan, scan}))
					control, err := ExecutePlan(ctx, union, store, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
					if err != nil {
						return nil, err
					}
					inputs, err := CollectAll(ctx, control)
					control.Close()
					if err != nil {
						return nil, err
					}
					counts := make(map[string]int)
					for _, input := range inputs {
						if input.Record == nil || input.Record.Version == nil || !input.Record.Version.IsComplete() || !reflect.DeepEqual(input.PrimaryKey, input.Record.PrimaryKey) {
							t.Fatalf("duplicate-input control lost a genuine stored row/key/version: %+v", input)
						}
						pk := string(input.PrimaryKey.Pack())
						counts[pk]++
						oldVersions[pk] = input.Record.Version
					}
					wantCounts := map[string]int{string(key(-11, -2).Pack()): 2, string(key(0, 0).Pack()): 2}
					if len(inputs) != 4 || !reflect.DeepEqual(counts, wantCounts) {
						t.Fatalf("raw union inputs=%d physical-key counts=%v, want four inputs/two entire PKs appearing twice", len(inputs), counts)
					}
					for _, move := range moves {
						pk := key(move.oldSource, move.oldP)
						versionKey := root.Sub(recordlayer.RecordVersionKey).Pack(pk)
						if format >= 6 {
							versionKey = store.RecordsSubspace().Sub(pk...).Pack(tuple.Tuple{int64(-1)})
						}
						versionBytes, err := rc.Transaction().Get(versionKey).Get()
						if err != nil {
							return nil, err
						}
						if len(versionBytes) == 0 {
							t.Fatalf("OLD physical version absent before move: %v", pk)
						}
					}

					// The real union repeats stored records, unlike two EXISTS witnesses.
					// Java's UPDATE wrapper dedups by the entire physical primary key.
					distinct := mustExecutorConstruct(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(union))
					target := mustTestQOV(t, values.NamedCorrelationIdentifier("Order"), rowType)
					sourceOrdinal := orderUpdate("coord_x", nil).FieldOrdinals[0]
					priceOrdinal := orderUpdate("price", nil).FieldOrdinals[0]
					calls := 0
					probe := &partitionUpdateValue{calls: &calls, run: func(int) (any, error) { return int64(1000), nil }}
					update := mustExecutorConstruct(plans.NewRecordQueryUpdatePlan(distinct, "Order", []expressions.UpdateTransform{
						orderUpdate("coord_x", mustExecutorConstruct(values.NewArithmeticValue(values.OpAdd, mustTestFieldOrdinal(t, target, sourceOrdinal), probe))),
						orderUpdate("price", mustExecutorConstruct(values.NewArithmeticValue(values.OpAdd, mustTestFieldOrdinal(t, target, priceOrdinal), &values.ConstantValue{Value: int64(1), Typ: values.NotNullInt}))),
					}))
					if shape := update.Explain(); !strings.Contains(shape, "UnorderedPrimaryKeyDistinct(UnorderedUnion(") {
						t.Fatalf("executed physical plan lost primary-key dedup above duplicate inputs: %s", shape)
					}
					cursor, err := ExecutePlan(ctx, update, store, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
					if err != nil {
						return nil, err
					}
					outputs, err := CollectAll(ctx, cursor)
					cursor.Close()
					if err != nil {
						return nil, err
					}
					if calls != 2 || len(outputs) != 2 || !rc.HasVersionMutations() {
						t.Fatalf("transformed OLD rows=%d outputs=%d pending versions=%t, want exactly two applied moves", calls, len(outputs), rc.HasVersionMutations())
					}
					seen := make(map[string]bool)
					localVersions := make(map[int]bool)
					for _, output := range outputs {
						for _, move := range moves {
							pk := string(key(move.newSource, move.newP).Pack())
							if string(output.PrimaryKey.Pack()) != pk {
								continue
							}
							if seen[pk] || output.Record == nil || output.Positional == nil || len(output.Positional.Slots) != 2 {
								t.Fatalf("duplicate or malformed move output: %+v", output)
							}
							seen[pk] = true
							old, oldOK := output.Positional.Slots[0].(*PositionalRow)
							newRow, newOK := output.Positional.Slots[1].(*PositionalRow)
							if !oldOK || !newOK || old.Slots[sourceOrdinal] != move.oldSource || old.Slots[priceOrdinal] != int64(move.price) || newRow.Slots[sourceOrdinal] != move.newSource || newRow.Slots[priceOrdinal] != int64(move.price+1) {
								t.Fatalf("wrong OLD/NEW images for move %+v: %+v", move, output.Positional.Slots)
							}
							record := output.Record.Record.(*demopb.Order)
							if record.GetOrderId() != 1 || record.GetCoordX() != move.newSource || record.GetPrice() != move.price+1 || !reflect.DeepEqual(output.Record.PrimaryKey, key(move.newSource, move.newP)) || output.Record.Version == nil || output.Record.Version.IsComplete() {
								t.Fatalf("wrong pending NEW physical row/version for %+v: %+v", move, output.Record)
							}
							localVersions[output.Record.Version.GetLocalVersion()] = true
						}
						if !seen[string(output.PrimaryKey.Pack())] {
							t.Fatalf("unexpected output physical key: %v", output.PrimaryKey)
						}
					}
					if len(seen) != 2 || len(localVersions) != 2 {
						t.Fatalf("distinct NEW keys=%d local versions=%d, want two of each", len(seen), len(localVersions))
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = testDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := open(rc)
					if err != nil {
						return nil, err
					}
					rows, err := recordlayer.AsList(ctx, store.ScanRecordsByType("Order", nil, recordlayer.ForwardScan()))
					if err != nil {
						return nil, err
					}
					if len(rows) != 2 {
						t.Fatalf("committed row count=%d, want 2", len(rows))
					}
					for _, move := range moves {
						oldKey, newKey := key(move.oldSource, move.oldP), key(move.newSource, move.newP)
						stored, err := store.LoadRecord(newKey)
						if err != nil {
							return nil, err
						}
						if stored == nil || stored.Version == nil || !stored.Version.IsComplete() || stored.Version.Equal(oldVersions[string(oldKey.Pack())]) || stored.VersionedInline != (format >= 6) || !reflect.DeepEqual(stored.PrimaryKey, newKey) {
							t.Fatalf("missing/wrong committed NEW key/version: %v: %+v", newKey, stored)
						}
						record := stored.Record.(*demopb.Order)
						if record.GetOrderId() != 1 || record.GetCoordX() != move.newSource || record.GetPrice() != move.price+1 {
							t.Fatalf("committed NEW row was not transformed once: %+v", record)
						}
						old, err := store.LoadRecord(oldKey)
						if err != nil {
							return nil, err
						}
						physicalOld, err := rc.Transaction().GetRange(store.RecordsSubspace().Sub(oldKey...), fdb.RangeOptions{}).GetSliceWithError()
						if err != nil {
							return nil, err
						}
						legacyOldVersion, err := rc.Transaction().Get(root.Sub(recordlayer.RecordVersionKey).Pack(oldKey)).Get()
						if err != nil {
							return nil, err
						}
						if old != nil || len(physicalOld) != 0 || len(legacyOldVersion) != 0 {
							t.Fatalf("committed OLD body/splits/inline or legacy version survived: %v", oldKey)
						}
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
