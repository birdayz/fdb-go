package recordlayer

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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
			p := pendingRecordLoad{pendingRecordRangeInput: pendingRecordRangeInput{
				primaryKey: pk, prefix: ss.Pack(pk),
				rangeRead: gatedRecordRange{release: release, rows: rows, err: wantErr},
			}}
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

func TestRecordRangeReadinessInputSnapshot(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"rows", "corrupt-head", "range-error", "discard"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ss := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			pk := tuple.Tuple{int64(7)}
			version, err := IncompleteVersion(17)
			if err != nil {
				t.Fatal(err)
			}
			localSize := sizeInfo{KeyCount: 1, KeySize: 23, ValueSize: VersionBytes + 1, VersionedInline: true}
			rows := []fdb.KeyValue{
				{Key: ss.Pack(appendToTuple(pk, 1)), Value: []byte("first")},
				{Key: ss.Pack(appendToTuple(pk, 2)), Value: []byte("second")},
			}
			if mode == "corrupt-head" {
				rows = rows[1:]
			}
			var rangeErr error
			if mode == "range-error" {
				rangeErr = fdb.Error{Code: 1007}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			p := pendingRecordLoad{pendingRecordRangeInput: pendingRecordRangeInput{
				primaryKey: pk, prefix: ss.Pack(pk), version: version, localSize: localSize,
				rangeRead: gatedRecordRange{entered: entered, release: release, rows: rows, err: rangeErr},
			}}
			p.startReadiness()
			result := p.asyncRange
			t.Cleanup(func() {
				unblock()
				select {
				case <-result.done:
				case <-time.After(5 * time.Second):
					t.Error("range worker did not finish after release")
				}
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("range worker did not enter its iterator")
			}
			if p.ready() {
				t.Fatal("blocked worker reported ready")
			}
			// Replace field slots, not shared backing arrays: the snapshot is shallow.
			p.pendingRecordRangeInput = pendingRecordRangeInput{
				primaryKey: tuple.Tuple{int64(99)}, prefix: []byte("wrong-prefix"),
				localErr: errors.New("replaced caller input"),
			}
			if mode == "discard" {
				p = pendingRecordLoad{}
			}
			unblock()
			select {
			case <-result.done:
			case <-time.After(5 * time.Second):
				t.Fatal("range worker did not publish its result")
			}
			if mode != "discard" {
				if !p.ready() {
					t.Fatal("completed worker did not report ready")
				}
				var sizes sizeInfo
				value, gotVersion, err := p.finish(&sizes)
				if !bytes.Equal(value, result.value) || gotVersion != result.version || err != result.err || sizes != result.sizes {
					t.Fatal("finish did not return every published result field")
				}
			}
			wantSizes := localSize
			switch mode {
			case "corrupt-head":
				var got *FoundSplitWithoutStartError
				if !errors.As(result.err, &got) || got.NextIndex != 2 || len(got.KeyTuple) != 1 || got.KeyTuple[0] != pk[0] {
					t.Fatalf("captured corrupt-head error = %v", result.err)
				}
			case "range-error":
				if !errors.Is(result.err, rangeErr) {
					t.Fatalf("captured range error = %v, want %v", result.err, rangeErr)
				}
			default:
				if result.err != nil || result.version != version || !bytes.Equal(result.value, []byte("firstsecond")) {
					t.Fatalf("captured result = %q, version=%v, error=%v", result.value, result.version, result.err)
				}
				wantSizes.KeyCount += 2
				wantSizes.KeySize += len(rows[0].Key) + len(rows[1].Key)
				wantSizes.ValueSize += 11
				wantSizes.IsSplit = true
			}
			if result.err != nil && (result.value != nil || result.version != nil) {
				t.Fatalf("failed result leaked value/version: %q, %v", result.value, result.version)
			}
			if result.sizes != wantSizes {
				t.Fatalf("captured sizes = %+v, want %+v", result.sizes, wantSizes)
			}
		})
	}
}

func TestRecordRangeReadinessLocalInputs(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"local-error", "version-only"} {
		for _, async := range []bool{false, true} {
			name := mode + "/sync"
			if async {
				name = mode + "/async"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				pk := tuple.Tuple{int64(7)}
				version, err := IncompleteVersion(17)
				if err != nil {
					t.Fatal(err)
				}
				wantSizes := sizeInfo{KeyCount: 1, KeySize: 23, ValueSize: VersionBytes + 1, VersionedInline: true}
				reader := &allocationRecordRange{}
				p := pendingRecordLoad{pendingRecordRangeInput: pendingRecordRangeInput{
					primaryKey: pk, version: version, localSize: wantSizes, rangeRead: reader,
				}}
				var wantErr error
				if mode == "local-error" {
					wantErr = errors.New("captured local version error")
					p.localErr = wantErr
				} else {
					release := make(chan struct{})
					close(release)
					p.rangeRead = gatedRecordRange{release: release}
				}
				if async {
					p.startReadiness()
					select {
					case <-p.asyncRange.done:
					case <-time.After(5 * time.Second):
						t.Fatal("local-input worker did not finish")
					}
				}
				var sizes sizeInfo
				value, gotVersion, err := p.finish(&sizes)
				if value != nil || gotVersion != nil || sizes != wantSizes {
					t.Fatalf("local-input result = %q, version=%v, sizes=%+v; want nil, nil, %+v", value, gotVersion, sizes, wantSizes)
				}
				if mode == "local-error" {
					if err != wantErr || reader.iterators != 0 || reader.advances != 0 || reader.gets != 0 {
						t.Fatalf("local error = %v; range operations = %d/%d/%d; want captured error before iterator", err, reader.iterators, reader.advances, reader.gets)
					}
				} else {
					var got *FoundSplitWithoutStartError
					if !errors.As(err, &got) || got.NextIndex != recordVersionSuffix || len(got.KeyTuple) != 1 || got.KeyTuple[0] != pk[0] {
						t.Fatalf("version-only error = %v", err)
					}
				}
			})
		}
	}
}

// Measure the actual worker in a child so parallel tests cannot contribute to
// AllocsPerRun's process-wide counter. The preallocated iterator is reused only
// after each read has completed, and the synchronous read is the control.
func TestRecordRangeReadinessAllocations(t *testing.T) {
	const childEnv = "FDB_RECORD_RANGE_ALLOCATION_CHILD"
	if os.Getenv(childEnv) != "1" {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestRecordRangeReadinessAllocations$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(output, []byte("allocations per range read:")) {
			t.Fatalf("isolated range allocation test: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	ss := subspace.FromBytes([]byte("range-allocation"))
	pk := tuple.Tuple{int64(7)}
	version, err := IncompleteVersion(17)
	if err != nil {
		t.Fatal(err)
	}
	reader := &allocationRecordRange{row: fdb.KeyValue{
		Key: ss.Pack(appendToTuple(pk, unsplitRecord)), Value: []byte("record"),
	}}
	var p pendingRecordLoad
	p.primaryKey, p.prefix, p.rangeRead = pk, ss.Pack(pk), reader
	p.version = version
	p.localSize = sizeInfo{KeyCount: 1, KeySize: 23, ValueSize: VersionBytes + 1, VersionedInline: true}
	wantSizes := p.localSize
	wantSizes.KeyCount++
	wantSizes.KeySize += len(reader.row.Key)
	wantSizes.ValueSize += len(reader.row.Value)
	var sizes sizeInfo
	check := func(value []byte, gotVersion *FDBRecordVersion, err error) {
		if err != nil || !bytes.Equal(value, reader.row.Value) || gotVersion != version || sizes != wantSizes {
			t.Fatalf("allocation read = %q, version=%v, sizes=%+v, err=%v; want version=%v sizes=%+v", value, gotVersion, sizes, err, version, wantSizes)
		}
	}
	const runs = 1000
	synchronous := testing.AllocsPerRun(runs, func() {
		check(p.readRange(&sizes))
	})
	checkCounts := func(reads int) {
		if reader.iterators != reads || reader.advances != 2*reads || reader.gets != 2*reads {
			t.Fatalf("iterator population = %d iterators, %d advances, %d gets; want %d, %d, %d", reader.iterators, reader.advances, reader.gets, reads, 2*reads, 2*reads)
		}
	}
	checkCounts(runs + 1) // AllocsPerRun includes one warmup.
	asynchronous := testing.AllocsPerRun(runs, func() {
		p.startReadiness()
		check(p.finish(&sizes)) // Drain the worker before reusing its iterator.
	})
	checkCounts(2 * (runs + 1))
	t.Logf("allocations per range read: async=%.0f sync=%.0f additional=%.0f; reads=%d iterators=%d advances=%d gets=%d", asynchronous, synchronous, asynchronous-synchronous, 2*(runs+1), reader.iterators, reader.advances, reader.gets)
	if asynchronous <= 0 || asynchronous-synchronous != 3 {
		t.Fatalf("range readiness adds %.0f allocations to %.0f synchronous allocations, want 3", asynchronous-synchronous, synchronous)
	}
}

// This pure range/iterator fixture has no database behavior and allocates no
// per-read state; it isolates worker overhead from the same real range reader.
type allocationRecordRange struct {
	fdb.RangeResult
	fdb.RangeIterator
	row                       fdb.KeyValue
	index                     int
	iterators, advances, gets int
}

func (r *allocationRecordRange) Iterator() fdb.RangeIterator {
	r.iterators++
	r.index = -1
	return r
}

func (r *allocationRecordRange) Advance() bool {
	r.advances++
	r.index++
	return r.index == 0
}

func (r *allocationRecordRange) Get() (fdb.KeyValue, error) {
	r.gets++
	if r.index == 0 {
		return r.row, nil
	}
	return fdb.KeyValue{}, nil
}

func (r *allocationRecordRange) MustGet() fdb.KeyValue {
	kv, _ := r.Get()
	return kv
}

func (*allocationRecordRange) SetTraceLog(func(int, int, int, bool, error)) {}

type gatedRecordRange struct {
	fdb.RangeResult
	entered chan<- struct{}
	release <-chan struct{}
	rows    []fdb.KeyValue
	err     error
}

func (r gatedRecordRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	<-r.release
	return r.rows, r.err
}

func (r gatedRecordRange) Iterator() fdb.RangeIterator {
	if r.entered != nil {
		close(r.entered)
	}
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
				p := pendingRecordLoad{pendingRecordRangeInput: pendingRecordRangeInput{primaryKey: pk, prefix: ss.Pack(pk), rangeRead: rangeRead}}
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
			raw := &pendingRecordRange{done: done, value: data, sizes: sizeInfo{KeyCount: 1, KeySize: 11, ValueSize: len(data)}}
			legacy := &countedLegacyVersionRead{version: version}
			wantLegacyGets, wantRecord := 1, true
			wantBytes := int64(raw.sizes.KeySize + raw.sizes.ValueSize)
			var wantCause error
			switch outcome {
			case "nil-version":
				legacy.version = nil
			case "legacy-error":
				wantCause = fdb.Error{Code: 1007}
				legacy.err, wantRecord = wantCause, false
			case "missing":
				raw.value, raw.sizes = nil, sizeInfo{}
				wantLegacyGets, wantRecord, wantBytes = 0, false, 0
			case "range-error":
				wantCause = fdb.Error{Code: 1007}
				raw.err = wantCause
				wantLegacyGets, wantRecord, wantBytes = 0, false, 0
			case "deserialize-error":
				raw.value = []byte{0xff}
				raw.sizes.ValueSize = 1
				wantLegacyGets, wantRecord, wantBytes = 0, false, 12
			case "inline-version":
				raw.version, wantLegacyGets = version, 0
			}
			timer, scan := NewStoreTimer(), NewScanLimiterState()
			scan.startRecordLoadCharge()
			finishRawRecordCharge(raw, scan)
			ctx := NewFDBRecordContext(nil, nil)
			ctx.SetTimer(timer)
			load := &recordLoad{
				store:      &FDBRecordStore{context: ctx, metaData: loadParityMetaData(t, false, true)},
				primaryKey: tuple.Tuple{int64(1)},
				first:      pendingStoreRecordLoad{raw: pendingRecordLoad{asyncRange: raw}, oldVersion: legacy},
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
			elapsed := timer.GetTimeNanos(EventLoadRecord)
			if elapsed <= 0 {
				t.Fatalf("load time = %d, want recorded duration", elapsed)
			}
			if _, direct := any(load).(PendingRead[*FDBStoredRecord[proto.Message]]); direct {
				if load.store != nil || load.primaryKey != nil || load.first.raw.asyncRange != nil || load.first.oldVersion != nil {
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
			if (got != nil) != wantRecord {
				t.Fatalf("record = %v, error = %v, want record present = %t", got, gotErr, wantRecord)
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
			if timer.GetCount(EventLoadRecord) != 1 || timer.GetTimeNanos(EventLoadRecord) != elapsed || scan.BytesScanned() != wantBytes || legacy.gets != wantLegacyGets {
				t.Fatalf("final accounting: timer=%d time=%d bytes=%d legacy gets=%d; want 1,%d,%d,%d", timer.GetCount(EventLoadRecord), timer.GetTimeNanos(EventLoadRecord), scan.BytesScanned(), legacy.gets, elapsed, wantBytes, wantLegacyGets)
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
			load := recordLoad{store: &FDBRecordStore{context: NewFDBRecordContext(nil, nil)}, first: pendingStoreRecordLoad{
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
