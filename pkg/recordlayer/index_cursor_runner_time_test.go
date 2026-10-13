package recordlayer

import (
	"context"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func TestFDB_IndexCursorRunnerCreationTime(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewIndex("price", Field("price")))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	seed, err := NewStoreBuilder().SetContext(loadParityContext(t, db)).SetSubspace(root).SetMetaDataProvider(metadata).CreateOrOpen()
	if err != nil {
		t.Fatal(err)
	}
	for id := int64(0); id < 3; id++ {
		if _, err := seed.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))}); err != nil {
			t.Fatal(err)
		}
	}
	assertLoadParityCommit(t, seed.context, false)
	for _, route := range []string{"RunWithRetry", "OpenContext"} {
		t.Run(route, func(t *testing.T) {
			clock := dst.NewSimClock(dst.Epoch)
			env := &dst.Env{Clock: clock}
			db.SetEnv(env)
			runner := NewFDBDatabaseRunner(db)
			scan := func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetSubspace(root).SetMetaDataProvider(metadata).Open()
				if err != nil {
					return nil, err
				}
				// Age the context, then freeze time so fresh-cursor anchoring cannot
				// accidentally pass because a slow real network consumed its budget.
				clock.Advance(time.Second)
				props := NewScanProperties(DefaultExecutePropertiesIn(env).WithTimeLimit(100 * time.Millisecond))
				cursor := store.ScanIndex(metadata.GetIndex("price"), TupleRangeAll, nil, props)
				defer cursor.Close()
				first, err := cursor.OnNext(context.Background())
				if err != nil || !first.HasNext() {
					t.Fatalf("free initial pass: %+v %v", first, err)
				}
				last, err := cursor.OnNext(context.Background())
				if err != nil || last.HasNext() || last.GetNoNextReason() != TimeLimitReached {
					t.Fatalf("runner-created context budget restarted: next=%v reason=%v error=%v create=%v", last.HasNext(), last.GetNoNextReason(), err, rc.createTime)
				}
				return nil, nil
			}
			if route == "RunWithRetry" {
				_, err = runner.RunWithRetry(context.Background(), scan)
			} else {
				var rc *FDBRecordContext
				rc, err = runner.OpenContext(context.Background())
				if err == nil {
					defer rc.Cancel()
					_, err = scan(rc)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
