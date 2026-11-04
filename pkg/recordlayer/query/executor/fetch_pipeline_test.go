package executor

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

// A record range cannot complete until the caller has issued a whole window.
// Background waits are allowed; a serial fetch cannot pass this gate. The
// deadline makes that regression fail rather than hang the suite.
type fetchWindowProbe struct {
	fdb.WritableTransaction
	prefix []byte
	issued int
	gate   int
	window chan struct{}
	ctx    context.Context
}

func (p *fetchWindowProbe) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	result := p.WritableTransaction.GetRange(r, options)
	begin, _ := r.FDBRangeKeySelectors()
	if !bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), p.prefix) {
		return result
	}
	p.issued++
	gate := p.gate
	if gate == 0 {
		gate = recordlayer.DefaultPipelineSize
	}
	if p.issued == gate {
		close(p.window)
	}
	return fetchWindowRange{RangeResult: result, window: p.window, ctx: p.ctx}
}

type fetchWindowRange struct {
	fdb.RangeResult
	window <-chan struct{}
	ctx    context.Context
}

func (r fetchWindowRange) Iterator() fdb.RangeIterator {
	return &fetchWindowIterator{RangeIterator: r.RangeResult.Iterator(), window: r.window, ctx: r.ctx}
}

type fetchWindowIterator struct {
	fdb.RangeIterator
	window  <-chan struct{}
	ctx     context.Context
	started bool
	err     error
}

func (i *fetchWindowIterator) Advance() bool {
	if !i.started {
		i.started = true
		select {
		case <-i.window:
		case <-i.ctx.Done():
			i.err = i.ctx.Err()
		}
	}
	return i.err == nil && i.RangeIterator.Advance()
}

func (i *fetchWindowIterator) Get() (fdb.KeyValue, error) {
	if i.err != nil {
		return fdb.KeyValue{}, i.err
	}
	return i.RangeIterator.Get()
}

func TestIntegrationIndexAndPartialFetchesPipeline(t *testing.T) {
	t.Parallel()
	for _, depth := range []int{1, 4, recordlayer.DefaultPipelineSize} {
		for _, partial := range []bool{false, true} {
			name := fmt.Sprintf("depth%d/index", depth)
			if partial {
				name = fmt.Sprintf("depth%d/partial", depth)
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				base := setupStore(t)
				const count = 12
				for i := int64(1); i <= count; i++ {
					insertOrders(t, base, &demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					probe := &fetchWindowProbe{
						WritableTransaction: rtx.Transaction(), prefix: testSubspace(t).Sub(recordlayer.RecordKey).Bytes(),
						window: make(chan struct{}), ctx: ctx, gate: depth,
					}
					var sizeCalls int
					sizer := func(op recordlayer.PipelineOperation) int {
						if op == recordlayer.PipelineOperationIndexToRecord {
							sizeCalls++
							return depth
						}
						return recordlayer.DefaultPipelineSizer(op)
					}
					store, err := recordlayer.NewStoreBuilder().SetContext(testDB.NewRecordContext(probe)).SetMetaDataProvider(base.GetMetaData()).SetSubspace(testSubspace(t)).SetPipelineSizer(sizer).Open()
					if err != nil {
						return nil, err
					}
					entries := store.ScanIndex(base.GetMetaData().GetIndex("order_price_idx"), recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
					var cursor recordlayer.RecordCursor[QueryResult]
					if partial {
						rows := recordlayer.MapCursor(entries, func(e *recordlayer.IndexEntry) QueryResult { return QueryResult{PrimaryKey: e.PrimaryKey()} })
						cursor = newFetchFullRecordCursor(rows, store, nil)
					} else {
						cursor = newIndexFetchCursor(entries, store, nil)
					}
					defer cursor.Close()
					if sizeCalls != 1 {
						return nil, fmt.Errorf("INDEX_TO_RECORD sizing calls=%d, want1", sizeCalls)
					}
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
					if probe.issued != count {
						return nil, fmt.Errorf("record ranges issued = %d, want %d", probe.issued, count)
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
