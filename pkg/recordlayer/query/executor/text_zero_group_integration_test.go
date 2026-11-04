package executor

import (
	"bytes"
	"context"
	"math"
	"slices"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

func TestIntegrationTextSignedZeroGroups(t *testing.T) {
	t.Parallel()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("TypedRecord", recordlayer.NewTextIndex("text_zero", recordlayer.GroupBy(recordlayer.Concat(recordlayer.Field("val_string"), recordlayer.Field("price")), recordlayer.Field("val_double"))))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	space := testSubspace(t)
	negative := math.Copysign(0, -1)
	records := []*demopb.TypedRecord{
		{Id: proto.Int64(9), ValDouble: proto.Float64(negative), ValString: proto.String("red"), Price: proto.Int32(1)},
		{Id: proto.Int64(8), ValDouble: proto.Float64(negative), ValString: proto.String("blue"), Price: proto.Int32(1)},
		{Id: proto.Int64(1), ValDouble: proto.Float64(0), ValString: proto.String("red blue"), Price: proto.Int32(1)},
		{Id: proto.Int64(2), ValDouble: proto.Float64(negative), ValString: proto.String("red blue"), Price: proto.Int32(1)},
		{Id: proto.Int64(3), ValDouble: proto.Float64(0), ValString: proto.String("prefix prelude"), Price: proto.Int32(1)},
		{Id: proto.Int64(4), ValDouble: proto.Float64(negative), ValString: proto.String("prefix prelude"), Price: proto.Int32(1)},
		{Id: proto.Int64(5), ValDouble: proto.Float64(negative), ValString: proto.String("prefix alone"), Price: proto.Int32(1)},
	}
	_, err = testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(space).CreateOrOpen()
		if e != nil {
			return nil, e
		}
		for _, r := range records {
			if _, e = store.SaveRecord(r); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		typ    predicates.ComparisonType
		query  string
		ids    []int64
		suffix bool
	}{
		{"all", predicates.ComparisonTextContainsAll, "red blue", []int64{2, 1}, false},
		{"phrase", predicates.ComparisonTextContainsPhrase, "red blue", []int64{2, 1}, false},
		{"within", predicates.ComparisonTextContainsAllWithin, "red blue", []int64{2, 1}, false},
		{"suffix", predicates.ComparisonTextContainsAll, "red blue", []int64{2, 1}, true},
		{"any", predicates.ComparisonTextContainsAny, "red blue", []int64{2, 8, 9, 1}, false},
		{"prefixes", predicates.ComparisonTextContainsAllPrefixes, "pr prel", []int64{4, 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				for _, pageSize := range []int{0, 1, 2} {
					scan := plans.TextScan{Predicate: &predicates.Comparison{Type: tc.typ, Operand: values.LiteralValue(tc.query), TextMaxDistance: 2}, Grouping: []values.Value{values.LiteralValue(float64(0))}, KeyTypes: []values.Type{values.NullableDouble, values.NullableString, values.NullableInt}}
					if tc.suffix {
						scan.Suffix = []*predicates.ComparisonRange{predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int32(1))}).Range}
					}
					plan, e := plans.NewRecordQueryTextIndexPlan("text_zero", scan, integrationTypedRecordType(), reverse)
					if e != nil {
						t.Fatal(e)
					}
					var continuation []byte
					var actual []int64
					exhausted := false
					for page := 0; page < 30 && !exhausted; page++ {
						result, e := readTextTestPage(context.Background(), continuation, testDB.Run, func(rtx *recordlayer.FDBRecordContext, input []byte) (recordlayer.RecordCursor[QueryResult], error) {
							store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(space).Open()
							if e != nil {
								return nil, e
							}
							props := recordlayer.DefaultExecuteProperties()
							props.ReturnedRowLimit = pageSize
							return ExecutePlan(context.Background(), plan, store, EmptyEvaluationContext(), input, props)
						})
						if e != nil {
							t.Fatal(e)
						}
						continuation = result.continuation
						exhausted = result.reason.IsSourceExhausted()
						for _, row := range result.rows {
							r := row.Record.Record.(*demopb.TypedRecord)
							// Verify original record identity, including the physically distinct zero sign.
							if !bytes.Equal(row.PrimaryKey.Pack(), tuple.Tuple{r.GetId()}.Pack()) {
								t.Fatalf("wrong fetched primary key: %v", row.PrimaryKey)
							}
							wantNegative := r.GetId() == 2 || r.GetId() == 4 || r.GetId() == 5 || r.GetId() == 8 || r.GetId() == 9
							if math.Signbit(r.GetValDouble()) != wantNegative {
								t.Fatalf("wrong fetched group for id %d", r.GetId())
							}
							actual = append(actual, r.GetId())
						}
					}
					if !exhausted {
						t.Fatal("pagination did not terminate")
					}
					want := slices.Clone(tc.ids)
					if reverse {
						slices.Reverse(want)
					}
					// Prefix scans may produce one posting per matching token; the planner's
					// PK distinct removes them. Check their identity and coverage independently.
					if tc.typ == predicates.ComparisonTextContainsAllPrefixes {
						actual = slices.Compact(actual)
					}
					if !slices.Equal(actual, want) {
						t.Fatalf("reverse=%v page=%d got %v want %v", reverse, pageSize, actual, want)
					}
				}
			}
		})
	}
}
