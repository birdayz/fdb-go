package executor

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

// fetchWindowProbe counts record reads issued before the first is waited on.
type fetchWindowProbe struct {
	fdb.WritableTransaction
	prefix       []byte
	issued       int
	issuedAtWait int
	waited       bool
}

func (p *fetchWindowProbe) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	future := p.WritableTransaction.Get(key)
	if !bytes.HasPrefix(key.FDBKey(), p.prefix) {
		return future
	}
	p.issued++
	return &fetchWindowFuture{FutureByteSlice: future, probe: p}
}

type fetchWindowFuture struct {
	fdb.FutureByteSlice
	probe *fetchWindowProbe
}

func (f *fetchWindowFuture) Get() ([]byte, error) {
	if !f.probe.waited {
		f.probe.waited = true
		f.probe.issuedAtWait = f.probe.issued
	}
	return f.FutureByteSlice.Get()
}

func TestIntegrationIndexAndPartialFetchesPipeline(t *testing.T) {
	t.Parallel()
	for _, partial := range []bool{false, true} {
		name := "index"
		if partial {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := setupStore(t)
			const count = 12
			for i := int64(1); i <= count; i++ {
				insertOrders(t, base, &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
			}
			_, err := testDB.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				probe := &fetchWindowProbe{WritableTransaction: rtx.Transaction(), prefix: testSubspace(t).Sub(recordlayer.RecordKey).Bytes()}
				store, err := recordlayer.NewStoreBuilder().SetContext(testDB.NewRecordContext(probe)).SetMetaDataProvider(base.GetMetaData()).SetSubspace(testSubspace(t)).Open()
				if err != nil {
					return nil, err
				}
				entries := store.ScanIndex(base.GetMetaData().GetIndex("order_price_idx"), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
				var cursor recordlayer.RecordCursor[QueryResult]
				if partial {
					rows := recordlayer.MapCursor(entries, func(e *recordlayer.IndexEntry) QueryResult { return QueryResult{PrimaryKey: e.PrimaryKey()} })
					cursor = &fetchFullRecordCursor{inner: rows, store: store}
				} else {
					cursor = &indexFetchCursor{inner: entries, store: store}
				}
				defer cursor.Close()
				for want := int64(1); want <= count; want++ {
					row, err := cursor.OnNext(context.Background())
					if err != nil {
						return nil, err
					}
					if !row.HasNext() || row.GetValue().PrimaryKey[0] != want {
						return nil, fmt.Errorf("row %d lost or reordered", want)
					}
				}
				end, err := cursor.OnNext(context.Background())
				if err != nil || end.HasNext() {
					return nil, fmt.Errorf("end: %v, %v", end, err)
				}
				// A serial fetch waits after its single read: one round trip per row.
				if probe.issuedAtWait != recordlayer.DefaultPipelineSize {
					return nil, fmt.Errorf("record reads in flight at first wait = %d, want %d", probe.issuedAtWait, recordlayer.DefaultPipelineSize)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
