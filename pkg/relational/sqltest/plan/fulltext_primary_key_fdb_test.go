package sqltest

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func TestFDBFullTextOverlappingPrimaryKeyUsesRecordFilter(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	database := recordlayer.NewFDBDatabase(raw)
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Concat(recordlayer.Field("name"), recordlayer.Field("customer_id")))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Customer", recordlayer.NewTextIndex("customer_text", recordlayer.Field("name")))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if positions := metadata.GetIndex("customer_text").PrimaryKeyComponentPositions(); !reflect.DeepEqual(positions, []int{0, -1}) {
		t.Fatalf("overlap precondition=%v", positions)
	}
	plan, err := embedded.PlanRecordQueryWithMetadata(`SELECT "customer_id" FROM "Customer" WHERE TEXT_CONTAINS_ALL("name",'blue')`, metadata, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Explain(), "TextIndexScan") {
		t.Fatalf("unsafe text index selected: %s", plan.Explain())
	}
	space := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	_, err = database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(space).CreateOrOpen()
		if e != nil {
			return nil, e
		}
		for _, record := range []*demopb.Customer{{CustomerId: proto.Int64(1), Name: proto.String("red blue")}, {CustomerId: proto.Int64(2), Name: proto.String("red")}} {
			if _, e = store.SaveRecord(record); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, e := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(metadata).SetSubspace(space).Open()
		if e != nil {
			return nil, e
		}
		cursor, e := executor.ExecutePlan(context.Background(), plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if e != nil {
			return nil, e
		}
		defer cursor.Close()
		rows, e := executor.CollectAll(context.Background(), cursor)
		if e != nil {
			return nil, e
		}
		if len(rows) != 1 || !bytes.Equal(rows[0].PrimaryKey.Pack(), tuple.Tuple{"red blue", int64(1)}.Pack()) {
			t.Fatalf("fallback rows=%v", rows)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
