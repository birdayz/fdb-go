package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func TestFDB_RecordLoadChargeLayouts(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	for _, mode := range []string{"bare-ready", "bare-pending-legacy-error", "modern-missing", "modern-corrupt", "modern-cancelled", "modern-decode-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			t.Cleanup(cancel)
			root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			bare := strings.HasPrefix(mode, "bare-")
			legacyError := mode == "bare-pending-legacy-error"
			format := int32(14)
			if bare {
				format = 4
			}
			builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
				SetStoreRecordVersions(legacyError)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("id", Field("order_id")))
			metadata, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			open := func(rc *FDBRecordContext, create bool) *FDBRecordStore {
				t.Helper()
				if err := rc.Transaction().Options().SetTimeout((15 * time.Second).Milliseconds()); err != nil {
					t.Fatal(err)
				}
				return legacyVersionStore(t, rc, root, metadata, format, create)
			}
			pk := tuple.Tuple{int64(1)}
			order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(7)}
			data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: order})
			if err != nil {
				t.Fatal(err)
			}
			recordKey, versionKey := legacyVersionKeys(root, format)
			seed := loadParityContext(t, db)
			seedStore := open(seed, true)
			if bare {
				seed.Transaction().Set(recordKey, data)
			} else if _, err := seedStore.SaveRecord(order); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "bare-pending-legacy-error":
				seed.Transaction().Set(versionKey, []byte{1, 2, 3})
			case "modern-missing":
				seed.Transaction().Clear(recordKey)
			case "modern-corrupt":
				seed.Transaction().Clear(recordKey)
				seed.Transaction().Set(root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(2)}), data)
			case "modern-decode-error":
				seed.Transaction().Set(recordKey, []byte{0xff})
			}
			assertLoadParityCommit(t, seed, false)

			control := loadParityContext(t, db)
			controlStore := open(control, false)
			prefix := root.Sub(RecordKey).Pack(pk)
			begin, end := recordRangeBounds(prefix)
			var rawBytes int64
			if bare {
				value, err := control.Transaction().Get(recordKey).Get()
				if err != nil || !bytes.Equal(value, data) {
					t.Fatalf("bare positive control: value=%x error=%v", value, err)
				}
				rawBytes = int64(len(recordKey) + len(value))
			} else {
				rows, err := control.Transaction().GetRange(fdb.KeyRange{Begin: fdb.Key(begin), End: fdb.Key(end)}, fdb.RangeOptions{}).GetSliceWithError()
				wantRows := 1
				if mode == "modern-missing" {
					wantRows = 0
				}
				if err != nil || len(rows) != wantRows {
					t.Fatalf("raw positive control: rows=%d error=%v, want %d", len(rows), err, wantRows)
				}
				for _, row := range rows {
					rawBytes += int64(len(row.Key) + len(row.Value))
				}
			}
			if mode != "modern-missing" && rawBytes <= 0 {
				t.Fatal("nonempty raw positive control has no bytes")
			}

			reader := loadParityContext(t, db)
			probe := &layoutChargeTransaction{WritableTransaction: reader.Transaction(), recordKey: recordKey, versionKey: versionKey, begin: begin, end: end}
			loads := 1
			if mode == "modern-decode-error" {
				loads = 16
			}
			if legacyError || mode == "modern-decode-error" {
				probe.rawGate = newLayoutChargeGate(ctx, loads)
			}
			if legacyError {
				probe.versionGate = newLayoutChargeGate(ctx, 1)
			}
			store := open(NewFDBRecordContext(probe, nil), false)
			if mode == "bare-ready" {
				// A real local write gives the native point future an immediately ready result.
				reader.Transaction().Set(recordKey, data)
			}
			if mode == "modern-cancelled" {
				// The error comes from the native cancelled transaction, not an injected result.
				reader.Cancel()
			}
			state := NewScanLimiterState()
			var pending []*recordLoad
			t.Cleanup(func() {
				probe.rawGate.unblock()
				probe.versionGate.unblock()
				reader.Cancel()
				for _, load := range pending {
					if raw := load.first.raw.asyncRange; raw != nil {
						select {
						case <-raw.done:
						case <-time.After(5 * time.Second):
							t.Error("raw worker did not finish during cleanup")
						}
					}
				}
			})
			for range loads {
				read, ok := store.LoadRecordAsync(pk, state).(*recordLoad)
				if !ok {
					t.Fatal("LoadRecordAsync did not return its production recordLoad")
				}
				pending = append(pending, read)
			}
			if mode == "bare-ready" && (pending[0].first.raw.asyncRange != nil || !pending[0].first.raw.value.IsReady()) {
				t.Fatal("ready bare-key load acquired a worker or lost immediate readiness")
			}
			if gate := probe.rawGate; gate != nil {
				for range loads {
					select {
					case <-gate.entered:
					case <-ctx.Done():
						t.Fatalf("real raw read did not reach completion gate: %v", ctx.Err())
					}
				}
				if got := state.BytesScanned(); got != 0 {
					t.Fatalf("unreleased raw completions charged %d bytes", got)
				}
				if mode != "modern-decode-error" {
					gate.unblock()
				}
			}

			var indexBytes int64
			if mode == "modern-decode-error" {
				index := metadata.GetIndex("id")
				rows, err := control.Transaction().GetRange(controlStore.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
				if err != nil || len(rows) != 1 {
					t.Fatalf("index positive control: rows=%d error=%v", len(rows), err)
				}
				indexBytes = int64(len(rows[0].Key) + len(rows[0].Value))
				// Release the raw workers after the first real index charge, while
				// the remaining caller-side index scans use that same counter.
				for i := range loads {
					props := DefaultExecuteProperties()
					props.ScanState = state
					cursor := store.ScanIndex(index, TupleRangeAll, nil, NewScanProperties(props))
					row, err := cursor.OnNext(ctx)
					closeErr := cursor.Close()
					if err != nil || closeErr != nil || !row.HasNext() {
						t.Fatalf("real index charge: row=%+v error=%v close=%v", row, err, closeErr)
					}
					if i == 0 {
						probe.rawGate.unblock()
					}
				}
			}
			waitLayoutChargeQuiescence(t, ctx, state)
			wantBytes := int64(loads) * (rawBytes + indexBytes)
			if mode == "modern-corrupt" || mode == "modern-cancelled" {
				wantBytes = 0
			}
			if got := state.BytesScanned(); got != wantBytes {
				t.Fatalf("raw completion before Get charged %d bytes, want %d (loads=%d raw=%d index=%d)", got, wantBytes, loads, rawBytes, indexBytes)
			}
			for _, load := range pending {
				if load.loaded || load.done {
					t.Fatal("charge quiescence materialized a record")
				}
			}
			if legacyError {
				if probe.versionFuture == nil || probe.versionFuture.gets.Load() != 0 || pending[0].first.oldVersion.IsReady() {
					t.Fatal("raw quiescence consumed or awaited the separately gated legacy version")
				}
				probe.versionGate.unblock()
			}
			for _, load := range pending {
				for range 2 {
					record, err := load.Get()
					switch mode {
					case "bare-ready":
						if err != nil || record == nil || !proto.Equal(record.Record, order) {
							t.Fatalf("ready bare record=%v error=%v", record, err)
						}
					case "modern-missing":
						if err != nil || record != nil {
							t.Fatalf("missing record=%v error=%v", record, err)
						}
					case "modern-corrupt":
						var corrupt *FoundSplitWithoutStartError
						if !errors.As(err, &corrupt) || corrupt.NextIndex != 2 {
							t.Fatalf("corrupt raw error=%v, want missing split start at 2", err)
						}
					case "modern-cancelled":
						var cancelled fdb.Error
						if !errors.As(err, &cancelled) || cancelled.Code != 1025 {
							t.Fatalf("cancelled raw error=%v, want native transaction_cancelled", err)
						}
					case "modern-decode-error":
						var decode *RecordDeserializationError
						if !errors.As(err, &decode) {
							t.Fatalf("completed raw decode error=%v, want RecordDeserializationError", err)
						}
					case "bare-pending-legacy-error":
						if err == nil || !strings.Contains(err.Error(), "version must be 12 bytes, got 3") {
							t.Fatalf("legacy version error=%v, want the actual malformed-version failure", err)
						}
					}
					if got := state.BytesScanned(); got != wantBytes {
						t.Fatalf("Get changed raw-completion bytes to %d, want %d", got, wantBytes)
					}
				}
			}
			waitLayoutChargeQuiescence(t, ctx, state)
			if legacyError && (probe.recordFuture.gets.Load() != 1 || probe.versionFuture.gets.Load() != 1) {
				t.Fatalf("legacy future consumption: raw=%d version=%d, want one each", probe.recordFuture.gets.Load(), probe.versionFuture.gets.Load())
			}
			if got := probe.ranges.Load(); !bare && got != int32(loads) {
				t.Fatalf("raw range requests=%d, want %d; quiescence must not issue reads", got, loads)
			}
		})
	}
}

func waitLayoutChargeQuiescence(t *testing.T, ctx context.Context, state *ScanLimiterState) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		state.WaitForIssuedRecordLoadCharges()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("raw-charge quiescence did not finish: %v", ctx.Err())
	}
}

type layoutChargeGate struct {
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newLayoutChargeGate(ctx context.Context, count int) *layoutChargeGate {
	return &layoutChargeGate{ctx: ctx, entered: make(chan struct{}, count), release: make(chan struct{})}
}

func (g *layoutChargeGate) unblock() {
	if g != nil {
		g.once.Do(func() { close(g.release) })
	}
}

func (g *layoutChargeGate) hold() error {
	select {
	case g.entered <- struct{}{}:
	case <-g.ctx.Done():
		return g.ctx.Err()
	}
	select {
	case <-g.release:
		return nil
	case <-g.ctx.Done():
		return g.ctx.Err()
	}
}

type layoutChargeTransaction struct {
	fdb.WritableTransaction
	recordKey, versionKey []byte
	begin, end            []byte
	rawGate, versionGate  *layoutChargeGate
	recordFuture          *layoutChargeFuture
	versionFuture         *layoutChargeFuture
	ranges                atomic.Int32
}

func (p *layoutChargeTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	future := p.WritableTransaction.Get(key)
	if bytes.Equal(key.FDBKey(), p.recordKey) {
		p.recordFuture = &layoutChargeFuture{FutureByteSlice: future, gate: p.rawGate}
		return p.recordFuture
	}
	if bytes.Equal(key.FDBKey(), p.versionKey) {
		p.versionFuture = &layoutChargeFuture{FutureByteSlice: future, gate: p.versionGate}
		return p.versionFuture
	}
	return future
}

func (p *layoutChargeTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	result := p.WritableTransaction.GetRange(r, options)
	begin, end := r.FDBRangeKeySelectors()
	if bytes.Equal(begin.FDBKeySelector().Key.FDBKey(), p.begin) && bytes.Equal(end.FDBKeySelector().Key.FDBKey(), p.end) {
		p.ranges.Add(1)
		if p.rawGate != nil {
			return layoutChargeRange{RangeResult: result, gate: p.rawGate}
		}
	}
	return result
}

type layoutChargeFuture struct {
	fdb.FutureByteSlice
	gate *layoutChargeGate
	gets atomic.Int32
}

func (f *layoutChargeFuture) Get() ([]byte, error) {
	f.gets.Add(1)
	value, err := f.FutureByteSlice.Get()
	if f.gate != nil {
		if gateErr := f.gate.hold(); gateErr != nil {
			return nil, gateErr
		}
	}
	return value, err
}

func (f *layoutChargeFuture) IsReady() bool {
	if f.gate != nil {
		select {
		case <-f.gate.release:
		default:
			return false
		}
	}
	return f.FutureByteSlice.IsReady()
}

type layoutChargeRange struct {
	fdb.RangeResult
	gate *layoutChargeGate
}

func (r layoutChargeRange) Iterator() fdb.RangeIterator {
	return &layoutChargeIterator{RangeIterator: r.RangeResult.Iterator(), gate: r.gate}
}

type layoutChargeIterator struct {
	fdb.RangeIterator
	gate    *layoutChargeGate
	started bool
	err     error
}

func (i *layoutChargeIterator) Advance() bool {
	if i.err != nil {
		return false
	}
	more := i.RangeIterator.Advance()
	if !i.started {
		i.started = true
		if i.err = i.gate.hold(); i.err != nil {
			return false
		}
	}
	return more
}

func (i *layoutChargeIterator) Get() (fdb.KeyValue, error) {
	if i.err != nil {
		return fdb.KeyValue{}, i.err
	}
	return i.RangeIterator.Get()
}
