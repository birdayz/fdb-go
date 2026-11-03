package executor

import (
	"bytes"
	"context"
	"math"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestIntegrationTextAnyPhysicalFloatingSuffixOrder(t *testing.T) {
	t.Parallel()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("TypedRecord", recordlayer.NewTextIndex("text_float", recordlayer.Concat(recordlayer.Field("val_string"), recordlayer.Field("val_double"))))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	records := []*demopb.TypedRecord{
		{Id: proto.Int64(9), ValString: proto.String("red"), ValDouble: proto.Float64(math.Float64frombits(0x7ff8000000000001))},
		{Id: proto.Int64(1), ValString: proto.String("blue"), ValDouble: proto.Float64(math.Float64frombits(0x7ff8000000000002))},
		{Id: proto.Int64(3), ValString: proto.String("red blue"), ValDouble: proto.Float64(math.Copysign(0, -1))},
		{Id: proto.Int64(2), ValString: proto.String("blue"), ValDouble: proto.Float64(0)},
	}
	expected := make([][]byte, len(records))
	for i, record := range records {
		expected[i] = tuple.Tuple{record.GetValDouble(), record.GetId()}.Pack()
	}
	slices.SortFunc(expected, bytes.Compare)
	_, err = testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(testSubspace(t)).CreateOrOpen()
		if e != nil {
			return nil, e
		}
		for _, record := range records {
			if _, e = store.SaveRecord(record); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, reverse := range []bool{false, true} {
		for _, pageSize := range []int{0, 1, 2} {
			plan, err := plans.NewRecordQueryTextIndexPlan("text_float", plans.TextScan{Predicate: &predicates.Comparison{Type: predicates.ComparisonTextContainsAny, Operand: values.LiteralValue("red blue")}, KeyTypes: []values.Type{values.NullableString, values.NullableDouble}}, integrationTypedRecordType(), reverse)
			if err != nil {
				t.Fatal(err)
			}
			var actual [][]byte
			var continuation []byte
			for page := 0; page < 20; page++ {
				exhausted := false
				_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(testSubspace(t)).Open()
					if e != nil {
						return nil, e
					}
					properties := recordlayer.DefaultExecuteProperties()
					properties.ReturnedRowLimit = pageSize
					cursor, e := ExecutePlan(context.Background(), plan, store, EmptyEvaluationContext(), continuation, properties)
					if e != nil {
						return nil, e
					}
					defer cursor.Close()
					for {
						result, e := cursor.OnNext(context.Background())
						if e != nil {
							return nil, e
						}
						if !result.HasNext() {
							exhausted = result.GetNoNextReason().IsSourceExhausted()
							continuation, e = result.GetContinuation().ToBytes()
							return nil, e
						}
						record := result.GetValue().Record.Record.(*demopb.TypedRecord)
						actual = append(actual, tuple.Tuple{record.GetValDouble(), record.GetId()}.Pack())
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				if exhausted {
					break
				}
				if page == 19 {
					t.Fatal("floating suffix union did not terminate")
				}
			}
			want := append([][]byte(nil), expected...)
			if reverse {
				slices.Reverse(want)
			}
			if len(actual) != len(want) {
				t.Fatalf("reverse=%v page=%d rows=%d want=%d", reverse, pageSize, len(actual), len(want))
			}
			for i, key := range actual {
				if !bytes.Equal(key, want[i]) {
					t.Fatalf("reverse=%v page=%d row=%d packed suffix=%x want=%x", reverse, pageSize, i, key, want[i])
				}
			}
		}
	}
}
