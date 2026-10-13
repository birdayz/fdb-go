package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// A failed index advance discards its queued mapping, not the already-issued
// record read. Java charges that read when its raw result completes, before
// deserialization (FDBRecordStore.loadTypedRecord, lines 1070-1077).
func TestFDB_DiscardedIndexRecordLoadChargesOnRawCompletion(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
		SetStoreRecordVersions(true)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewVersionIndex("version", VersionKey()).SetSubspaceKey("version"))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	open := func(rc *FDBRecordContext) *FDBRecordStore {
		t.Helper()
		if err := rc.Transaction().Options().SetTimeout((15 * time.Second).Milliseconds()); err != nil {
			t.Fatal(err)
		}
		store, err := NewStoreBuilder().SetContext(rc).SetSubspace(root).
			SetMetaDataProvider(metadata).SetFormatVersion(14).CreateOrOpen()
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	seed := loadParityContext(t, db)
	seedStore := open(seed)
	for id := int64(1); id <= 2; id++ {
		if _, err := seedStore.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(100 - id))}); err != nil {
			t.Fatal(err)
		}
	}
	assertLoadParityCommit(t, seed, false)

	// Independent real reads establish the exact index and record charges.
	control := loadParityContext(t, db)
	controlStore := open(control)
	index := metadata.GetIndex("version")
	indexRows, err := control.Transaction().GetRange(controlStore.IndexSubspace(index), fdb.RangeOptions{}).GetSliceWithError()
	if err != nil || len(indexRows) != 2 {
		t.Fatalf("index positive control: rows=%d error=%v, want two entries", len(indexRows), err)
	}
	entryCursor := controlStore.ScanIndex(index, TupleRangeAll, nil, ForwardScan())
	entry, err := entryCursor.OnNext(t.Context())
	if err != nil || !entry.HasNext() {
		t.Fatalf("index positive control: result=%+v error=%v", entry, err)
	}
	if err := entryCursor.Close(); err != nil {
		t.Fatal(err)
	}
	pk := entry.GetValue().PrimaryKey()
	stored, err := controlStore.LoadRecord(pk)
	if err != nil || stored == nil || stored.Version == nil {
		t.Fatalf("record positive control: record=%v error=%v, want versioned record", stored, err)
	}
	indexBytes := int64(len(indexRows[0].Key) + len(indexRows[0].Value))
	recordBytes := int64(stored.KeySize + stored.ValueSize)
	if indexBytes <= 0 || recordBytes <= 0 {
		t.Fatalf("empty byte-count positive control: index=%d record=%d", indexBytes, recordBytes)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	read := loadParityContext(t, db)
	prefix := controlStore.recordsSubspace.Pack(pk)
	begin, end := recordRangeBounds(prefix)
	probe := &recordChargeTransaction{
		WritableTransaction: read.Transaction(), ctx: ctx, begin: begin, end: end,
		nativeReady: make(chan error, 1), release: make(chan struct{}),
	}
	store := open(NewFDBRecordContext(probe, nil))
	var pending *recordLoad
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(probe.release) }) }
	t.Cleanup(func() {
		release()
		probe.Cancel()
		if pending != nil && pending.first.raw.asyncRange != nil {
			select {
			case <-pending.first.raw.asyncRange.done:
			case <-time.After(5 * time.Second):
				t.Error("record worker did not finish after gate release and transaction cancellation")
			}
		}
	})

	properties := DefaultExecuteProperties()
	properties.ScannedRecordsLimit = 1
	properties.FailOnScanLimitReached = true
	state := properties.ScanState
	leaf := &recordChargeIndexCursor{
		RecordCursor: store.ScanIndex(index, TupleRangeAll, nil, NewScanProperties(properties)),
		nativeReady:  probe.nativeReady,
	}
	loader := &indexRecordLoader{store: store, scanState: state}
	issued := 0
	cursor := MapPipelined(leaf, func(entry *IndexEntry) PendingRead[*FDBIndexedRecord] {
		issued++
		load := loader.load(entry)
		indexed, ok := load.(*indexRecordLoad)
		if !ok {
			t.Fatalf("index loader returned %T, want observable production indexRecordLoad", load)
		}
		pending, ok = indexed.pending.(*recordLoad)
		if !ok {
			t.Fatalf("record loader returned %T, want production recordLoad", indexed.pending)
		}
		return load
	}, DefaultPipelineSize)
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})

	next, err := cursor.OnNext(ctx)
	var limit *ScanLimitReachedError
	if !errors.As(err, &limit) || next.HasNext() {
		t.Fatalf("first pipeline result=%+v error=%v, want no row and the actual scan-limit error", next, err)
	}
	if leaf.pulls != 2 || !leaf.nativeWasReady || issued != 1 || probe.ranges.Load() != 1 {
		t.Fatalf("gate population: pulls=%d nativeReady=%t issued=%d rawRanges=%d; want 2,true,1,1",
			leaf.pulls, leaf.nativeWasReady, issued, probe.ranges.Load())
	}
	if pending == nil || pending.first.raw.asyncRange == nil || pending.done || pending.loaded {
		t.Fatal("discarded pipeline did not retain an unconsumed asynchronous raw read")
	}
	raw := pending.first.raw.asyncRange
	select {
	case <-raw.done:
		t.Fatal("raw record completed before its gate was released")
	default:
	}
	if got := state.BytesScanned(); got != indexBytes {
		t.Fatalf("bytes before raw completion=%d, want index bytes %d", got, indexBytes)
	}

	release()
	select {
	case <-raw.done:
	case <-ctx.Done():
		t.Fatalf("raw record did not complete after gate release: %v", ctx.Err())
	}
	if raw.err != nil || raw.value == nil || raw.version == nil || int64(raw.sizes.KeySize+raw.sizes.ValueSize) != recordBytes {
		t.Fatalf("raw positive control: value=%x version=%v sizes=%+v error=%v, want %d record bytes",
			raw.value, raw.version, raw.sizes, raw.err, recordBytes)
	}
	if pending.done || pending.loaded {
		t.Fatal("raw completion materialized the discarded record on the caller's behalf")
	}
	wantBytes := indexBytes + recordBytes
	if got := state.BytesScanned(); got != wantBytes {
		t.Errorf("completed discarded record load charged %d bytes, want index %d + record %d = %d without Get",
			got, indexBytes, recordBytes, wantBytes)
	}

	// Consuming the retained handle afterward must neither lose nor re-charge it.
	for range 2 {
		got, err := pending.Get()
		if err != nil || got == nil || !proto.Equal(got.Record, stored.Record) || got.Version == nil || !got.Version.Equal(stored.Version) {
			t.Fatalf("retained load result=%v error=%v, want positive-control record", got, err)
		}
		if got := state.BytesScanned(); got != wantBytes {
			t.Errorf("bytes after retained Get=%d, want unchanged %d", got, wantBytes)
		}
	}
}

// Gate only the selected record range, after its native first Advance succeeds.
// The index and all record data still come from the real transaction.
type recordChargeTransaction struct {
	fdb.WritableTransaction
	ctx         context.Context
	begin, end  []byte
	nativeReady chan error
	release     chan struct{}
	ranges      atomic.Int32
}

func (p *recordChargeTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	result := p.WritableTransaction.GetRange(r, options)
	begin, end := r.FDBRangeKeySelectors()
	if !bytes.Equal(begin.FDBKeySelector().Key.FDBKey(), p.begin) || !bytes.Equal(end.FDBKeySelector().Key.FDBKey(), p.end) {
		return result
	}
	p.ranges.Add(1)
	return recordChargeRange{RangeResult: result, probe: p}
}

type recordChargeRange struct {
	fdb.RangeResult
	probe *recordChargeTransaction
}

func (r recordChargeRange) Iterator() fdb.RangeIterator {
	return &recordChargeIterator{RangeIterator: r.RangeResult.Iterator(), probe: r.probe}
}

type recordChargeIterator struct {
	fdb.RangeIterator
	probe   *recordChargeTransaction
	started bool
	err     error
}

func (i *recordChargeIterator) Advance() bool {
	if i.started {
		return i.err == nil && i.RangeIterator.Advance()
	}
	i.started = true
	if !i.RangeIterator.Advance() {
		_, i.err = i.RangeIterator.Get()
		if i.err == nil {
			i.err = fmt.Errorf("positive-control record range was empty")
		}
		select {
		case i.probe.nativeReady <- i.err:
		case <-i.probe.ctx.Done():
		}
		return false
	}
	select {
	case i.probe.nativeReady <- nil:
	case <-i.probe.ctx.Done():
	}
	select {
	case <-i.probe.release:
		return true
	case <-i.probe.ctx.Done():
		i.err = i.probe.ctx.Err()
		return false
	}
}

func (i *recordChargeIterator) Get() (fdb.KeyValue, error) {
	if i.err != nil {
		return fdb.KeyValue{}, i.err
	}
	return i.RangeIterator.Get()
}

// The second pull still executes the production index cursor's limit check.
type recordChargeIndexCursor struct {
	RecordCursor[*IndexEntry]
	nativeReady    <-chan error
	pulls          int
	nativeWasReady bool
}

func (c *recordChargeIndexCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	c.pulls++
	if c.pulls == 2 {
		select {
		case err := <-c.nativeReady:
			if err != nil {
				return RecordCursorResult[*IndexEntry]{}, err
			}
			c.nativeWasReady = true
		case <-ctx.Done():
			return RecordCursorResult[*IndexEntry]{}, ctx.Err()
		}
	}
	return c.RecordCursor.OnNext(ctx)
}
