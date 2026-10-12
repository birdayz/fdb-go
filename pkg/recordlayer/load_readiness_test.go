package recordlayer

import (
	"bytes"
	"errors"
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

// The backend-neutral range adapter must publish errors as well as rows: the
// index pipeline keeps only a completed prefix when its source hits a deadline.
func TestRecordRangeReadiness(t *testing.T) {
	t.Parallel()
	ss := subspace.FromBytes([]byte("record-readiness"))
	pk := tuple.Tuple{int64(7)}
	for _, fails := range []bool{false, true} {
		name := "rows"
		if fails {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			rows := []fdb.KeyValue{
				{Key: ss.Pack(appendToTuple(pk, 1)), Value: []byte("first")},
				{Key: ss.Pack(appendToTuple(pk, 2)), Value: []byte("second")},
			}
			var wantErr error
			if fails {
				wantErr = fdb.Error{Code: 1007}
			}
			p := pendingRecordLoad{
				primaryKey: pk, prefix: ss.Pack(pk),
				rangeRead: gatedRecordRange{release: release, rows: rows, err: wantErr},
			}
			p.startReadiness()
			if p.ready() {
				t.Fatal("range reported ready before its rows/error were available")
			}
			close(release)
			<-p.asyncRange.done
			if !p.ready() {
				t.Fatal("completed range did not report ready")
			}
			var sizes sizeInfo
			value, version, err := p.finish(&sizes)
			if fails {
				var got fdb.Error
				if !errors.As(err, &got) || got.Code != 1007 {
					t.Fatalf("range error = %v, want FDB 1007", err)
				}
				return
			}
			if err != nil || version != nil || !bytes.Equal(value, []byte("firstsecond")) {
				t.Fatalf("range result = %q, %v, %v", value, version, err)
			}
			wantSizes := sizeInfo{KeyCount: 2, KeySize: len(rows[0].Key) + len(rows[1].Key), ValueSize: 11, IsSplit: true}
			if sizes != wantSizes {
				t.Fatalf("sizes = %+v, want %+v", sizes, wantSizes)
			}
		})
	}
}

type gatedRecordRange struct {
	fdb.RangeResult
	release <-chan struct{}
	rows    []fdb.KeyValue
	err     error
}

func (r gatedRecordRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	<-r.release
	return r.rows, r.err
}

func (r gatedRecordRange) Iterator() fdb.RangeIterator {
	return &gatedRecordIterator{rangeRead: r, index: -1}
}

type gatedRecordIterator struct {
	rangeRead gatedRecordRange
	index     int
}

func (i *gatedRecordIterator) Advance() bool {
	if i.index == -1 {
		<-i.rangeRead.release
	}
	if i.rangeRead.err != nil || i.index+1 >= len(i.rangeRead.rows) {
		return false
	}
	i.index++
	return true
}

func (i *gatedRecordIterator) Get() (fdb.KeyValue, error) {
	if i.rangeRead.err != nil {
		return fdb.KeyValue{}, i.rangeRead.err
	}
	if i.index < 0 || i.index >= len(i.rangeRead.rows) {
		return fdb.KeyValue{}, nil
	}
	return i.rangeRead.rows[i.index], nil
}

func (i *gatedRecordIterator) MustGet() fdb.KeyValue {
	kv, err := i.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

func (*gatedRecordIterator) SetTraceLog(func(int, int, int, bool, error)) {}

// A corrupt first row must win before the next iterator advance can block or
// fail. Materializing the range first changes both readiness and error ordering.
func TestRecordRangeRejectsCorruptHeadBeforeTail(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"sync", "async"} {
		for _, tail := range []string{"blocked", "fdb-error"} {
			t.Run(mode+"/"+tail, func(t *testing.T) {
				t.Parallel()
				ss := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
				pk := tuple.Tuple{int64(7)}
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				iter := &corruptHeadIterator{
					head:          fdb.KeyValue{Key: ss.Pack(appendToTuple(pk, -2)), Value: []byte("corrupt")},
					tailAttempted: make(chan struct{}), release: release,
				}
				if tail == "fdb-error" {
					iter.tailErr = fdb.Error{Code: 1007}
					unblock()
				}
				rangeRead := &corruptHeadRange{iter: iter}
				p := pendingRecordLoad{primaryKey: pk, prefix: ss.Pack(pk), rangeRead: rangeRead}
				var value []byte
				var version *FDBRecordVersion
				var sizes sizeInfo
				var err error
				var done <-chan struct{}
				if mode == "async" {
					p.startReadiness()
					done = p.asyncRange.done
				} else {
					finished := make(chan struct{})
					done = finished
					go func() {
						value, version, err = p.finish(&sizes)
						close(finished)
					}()
				}
				t.Cleanup(func() {
					unblock()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("record loader did not stop after releasing the test iterator")
					}
				})
				select {
				case <-done:
				case <-iter.tailAttempted:
					t.Fatal("record loader advanced into the tail before rejecting suffix -2")
				case <-time.After(5 * time.Second):
					t.Fatal("available corrupt first row did not complete the record load")
				}
				if mode == "async" {
					if !p.ready() {
						t.Fatal("completed corrupt row did not publish asynchronous readiness")
					}
					value, version, err = p.finish(&sizes)
				}
				var corrupt *FoundSplitWithoutStartError
				if !errors.As(err, &corrupt) || corrupt.NextIndex != -2 || corrupt.Reverse || !bytes.Equal(corrupt.KeyTuple.Pack(), pk.Pack()) {
					t.Fatalf("load error = %v, want forward FoundSplitWithoutStart at -2 for %v", err, pk)
				}
				if value != nil || version != nil {
					t.Fatalf("corrupt load returned value/version: %x/%v", value, version)
				}
				if rangeRead.sliceCalls.Load() != 0 || rangeRead.iteratorCalls.Load() != 1 || iter.advances.Load() != 1 || iter.gets.Load() != 1 || iter.tailReads.Load() != 0 {
					t.Fatalf("read calls: slices=%d iterators=%d advances=%d gets=%d tail=%d; want 0,1,1,1,0", rangeRead.sliceCalls.Load(), rangeRead.iteratorCalls.Load(), iter.advances.Load(), iter.gets.Load(), iter.tailReads.Load())
				}
			})
		}
	}
}

type corruptHeadRange struct {
	iter          *corruptHeadIterator
	sliceCalls    atomic.Int32
	iteratorCalls atomic.Int32
}

func (r *corruptHeadRange) Iterator() fdb.RangeIterator {
	r.iteratorCalls.Add(1)
	return r.iter
}

func (r *corruptHeadRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	r.sliceCalls.Add(1)
	iter := r.Iterator()
	var rows []fdb.KeyValue
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		rows = append(rows, kv)
	}
	_, err := iter.Get()
	return rows, err
}

func (r *corruptHeadRange) GetSliceOrPanic() []fdb.KeyValue {
	rows, err := r.GetSliceWithError()
	if err != nil {
		panic(err)
	}
	return rows
}

type corruptHeadIterator struct {
	head          fdb.KeyValue
	release       <-chan struct{}
	tailErr       error
	tailAttempted chan struct{}
	tailOnce      sync.Once
	advances      atomic.Int32
	gets          atomic.Int32
	tailReads     atomic.Int32
}

func (i *corruptHeadIterator) Advance() bool {
	if i.advances.Add(1) == 1 {
		return true
	}
	i.tailReads.Add(1)
	i.tailOnce.Do(func() { close(i.tailAttempted) })
	<-i.release
	return false
}

func (i *corruptHeadIterator) Get() (fdb.KeyValue, error) {
	i.gets.Add(1)
	if i.advances.Load() == 1 {
		return i.head, nil
	}
	return fdb.KeyValue{}, i.tailErr
}

func (i *corruptHeadIterator) MustGet() fdb.KeyValue {
	kv, err := i.Get()
	if err != nil {
		panic(err)
	}
	return kv
}

func (*corruptHeadIterator) SetTraceLog(func(int, int, int, bool, error)) {}

// Use the load itself when it implements PendingRead; the fallback keeps these
// state-machine regressions runnable against the original wrapper implementation.
func recordLoadPendingRead(load *recordLoad) PendingRead[*FDBStoredRecord[proto.Message]] {
	if pending, ok := any(load).(PendingRead[*FDBStoredRecord[proto.Message]]); ok {
		return pending
	}
	return NewPendingRead(load.finish, load.ready)
}

type countedLegacyVersionRead struct {
	version *FDBRecordVersion
	err     error
	gets    int
}

func (p *countedLegacyVersionRead) Get() (*FDBRecordVersion, error) {
	p.gets++
	return p.version, p.err
}

func (*countedLegacyVersionRead) IsReady() bool { return false }

func TestRecordLoadPendingReadMemoizesResult(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"version", "nil-version", "legacy-error", "missing", "range-error", "deserialize-error", "inline-version"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			order := &demopb.Order{OrderId: proto.Int64(1)}
			data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: order})
			if err != nil {
				t.Fatal(err)
			}
			version, err := IncompleteVersion(7)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			close(done)
			raw := &pendingRecordRange{done: done, value: data, sizes: sizeInfo{KeyCount: 1, KeySize: 11, ValueSize: len(data)}}
			legacy := &countedLegacyVersionRead{version: version}
			wantLegacyGets, wantTimer := 1, int64(1)
			wantBytes := int64(raw.sizes.KeySize + raw.sizes.ValueSize)
			var wantCause error
			switch outcome {
			case "nil-version":
				legacy.version = nil
			case "legacy-error":
				wantCause = fdb.Error{Code: 1007}
				legacy.err, wantTimer = wantCause, 0
			case "missing":
				raw.value, raw.sizes = nil, sizeInfo{}
				wantLegacyGets, wantTimer, wantBytes = 0, 0, 0
			case "range-error":
				wantCause = fdb.Error{Code: 1007}
				raw.err = wantCause
				wantLegacyGets, wantTimer, wantBytes = 0, 0, 0
			case "deserialize-error":
				raw.value = []byte{0xff}
				raw.sizes.ValueSize = 1
				wantLegacyGets, wantTimer, wantBytes = 0, 0, 12
			case "inline-version":
				raw.version, wantLegacyGets = version, 0
			}
			timer, scan := NewStoreTimer(), NewScanLimiterState()
			ctx := NewFDBRecordContext(nil, nil)
			ctx.SetTimer(timer)
			load := &recordLoad{
				store:      &FDBRecordStore{context: ctx, metaData: loadParityMetaData(t, false, true)},
				primaryKey: tuple.Tuple{int64(1)}, scanState: scan,
				first: pendingStoreRecordLoad{raw: pendingRecordLoad{asyncRange: raw}, oldVersion: legacy},
			}
			pending := recordLoadPendingRead(load)
			for range 3 {
				if got := pending.IsReady(); got != (wantLegacyGets == 0) {
					t.Fatalf("IsReady = %t, want unused legacy read = %t", got, wantLegacyGets == 0)
				}
			}
			if timer.GetCount(EventLoadRecord) != 0 || scan.BytesScanned() != wantBytes || legacy.gets != 0 {
				t.Fatalf("readiness accounting: timer=%d bytes=%d legacy gets=%d", timer.GetCount(EventLoadRecord), scan.BytesScanned(), legacy.gets)
			}
			got, gotErr := pending.Get()
			if _, direct := any(load).(PendingRead[*FDBStoredRecord[proto.Message]]); direct {
				if load.store != nil || load.primaryKey != nil || load.scanState != nil || load.first.raw.asyncRange != nil || load.first.oldVersion != nil {
					t.Fatal("completed load retained intermediate store/read state")
				}
			}
			if wantCause != nil && !errors.Is(gotErr, wantCause) {
				t.Fatalf("error = %v, want cause %v", gotErr, wantCause)
			}
			if outcome == "deserialize-error" {
				var deser *RecordDeserializationError
				if !errors.As(gotErr, &deser) {
					t.Fatalf("error = %v, want RecordDeserializationError", gotErr)
				}
			} else if wantCause == nil && gotErr != nil {
				t.Fatal(gotErr)
			}
			if (got != nil) != (wantTimer == 1) {
				t.Fatalf("record = %v, error = %v, want record present = %t", got, gotErr, wantTimer == 1)
			}
			if got != nil && (!proto.Equal(got.Record, order) || !got.Version.Equal(legacy.version)) {
				t.Fatalf("wrong record/version: %+v", got)
			}
			// Terminal readiness must not reconsult even a non-monotonic source.
			raw.done = make(chan struct{})
			for range 3 {
				if !pending.IsReady() {
					t.Fatal("Get did not make readiness terminal")
				}
				if again, againErr := pending.Get(); again != got || againErr != gotErr {
					t.Fatalf("Get changed result: (%p, %v), first (%p, %v)", again, againErr, got, gotErr)
				}
			}
			if timer.GetCount(EventLoadRecord) != wantTimer || scan.BytesScanned() != wantBytes || legacy.gets != wantLegacyGets {
				t.Fatalf("final accounting: timer=%d bytes=%d legacy gets=%d; want %d,%d,%d", timer.GetCount(EventLoadRecord), scan.BytesScanned(), legacy.gets, wantTimer, wantBytes, wantLegacyGets)
			}
		})
	}
}

func TestRecordLoadReadinessIncludesLegacyVersion(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	var versionReady bool
	load := recordLoad{loaded: true, record: &FDBStoredRecord[proto.Message]{}, first: pendingStoreRecordLoad{
		raw: pendingRecordLoad{asyncRange: &pendingRecordRange{done: done}},
		oldVersion: NewPendingRead(func() (*FDBRecordVersion, error) { return nil, nil },
			func() bool { return versionReady }),
	}}
	if load.ready() {
		t.Fatal("neither range nor version is ready")
	}
	versionReady = true
	if load.ready() {
		t.Fatal("version ready must not hide a pending range")
	}
	versionReady = false
	close(done)
	if load.ready() {
		t.Fatal("range ready must not hide a pending legacy version")
	}
	versionReady = true
	if !load.ready() {
		t.Fatal("both range and version are ready")
	}
	load.first.oldVersion = nil
	if !load.ready() {
		t.Fatal("modern range needs no separate version read")
	}
}

func TestRecordLoadReadinessDoesNotWaitForUnusedLegacyVersion(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"missing", "range-error", "deserialize-error", "inline-version"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			close(done)
			load := recordLoad{first: pendingStoreRecordLoad{
				raw: pendingRecordLoad{asyncRange: &pendingRecordRange{done: done}},
				oldVersion: NewPendingRead(func() (*FDBRecordVersion, error) {
					t.Fatal("unused legacy version was awaited")
					return nil, nil
				}, func() bool { return false }),
			}}
			switch outcome {
			case "range-error":
				load.first.raw.asyncRange.err = fdb.Error{Code: 1007}
			case "deserialize-error":
				load.loaded = true
				load.loadErr = &RecordDeserializationError{PrimaryKey: tuple.Tuple{int64(1)}}
			case "inline-version":
				version, err := IncompleteVersion(7)
				if err != nil {
					t.Fatal(err)
				}
				load.loaded = true
				load.record = &FDBStoredRecord[proto.Message]{Version: version}
			}
			if !load.ready() {
				t.Fatal("completed record outcome was hidden by an unused legacy version")
			}
			if outcome != "inline-version" {
				_, _ = load.finish()
			} else if _, err := load.first.finishVersion(load.record.Version); err != nil {
				t.Fatal(err)
			}
		})
	}
}
