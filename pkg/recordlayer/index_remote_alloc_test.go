package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

func remoteAllocationStore(t *testing.T) *FDBRecordStore {
	t.Helper()
	return &FDBRecordStore{
		metaData:        loadParityMetaData(t, true, false),
		recordsSubspace: subspace.FromBytes(tuple.Tuple{"remote-allocation"}.Pack()),
		context:         &FDBRecordContext{},
	}
}

func remoteAllocationRaw(t *testing.T, store *FDBRecordStore, id int64) FDBIndexedRawRecord {
	t.Helper()
	data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: &demopb.Order{
		OrderId: proto.Int64(id), Price: proto.Int32(25),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return FDBIndexedRawRecord{
		IndexEntry: &IndexEntry{Index: NewIndex("price", Field("price")), Key: tuple.Tuple{int64(25), id}},
		RawRecord: fdb.MappedKeyValue{Kind: fdb.MappedResultGetRange, GetRange: client.MappedGetRange{Rows: []client.KeyValue{{
			Key: store.recordsSubspace.Pack(tuple.Tuple{id, unsplitRecord}), Value: data,
		}}}},
	}
}

func TestRemoteRecordLoadRetainsOwnedResults(t *testing.T) {
	t.Parallel()
	store := remoteAllocationStore(t)
	loader := &remoteIndexRecordLoader{store: store, scanState: NewScanLimiterState(), orphan: IndexOrphanError}
	firstRaw, secondRaw := remoteAllocationRaw(t, store, 1001), remoteAllocationRaw(t, store, 2002)
	firstPending := loader.load(firstRaw)
	if !firstPending.IsReady() {
		t.Fatal("current-format record unexpectedly pending")
	}
	first, err := firstPending.Get()
	if err != nil {
		t.Fatal(err)
	}
	again, err := firstPending.Get()
	if err != nil || again != first {
		t.Fatal("ready result was not memoized")
	}
	second, err := loader.load(secondRaw).Get()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.Record == second.Record || first.Record.Record == second.Record.Record || first.IndexEntry == second.IndexEntry {
		t.Fatal("returned record, message, or entry was reused")
	}
	second.Record.PrimaryKey[0] = int64(3003)
	second.IndexEntry.Key[0] = int64(99)
	second.Record.Record.(*demopb.Order).OrderId = proto.Int64(3003)
	if first.Record.PrimaryKey[0] != int64(1001) || first.IndexEntry.Key[0] != int64(25) || first.Record.Record.(*demopb.Order).GetOrderId() != 1001 {
		t.Fatal("later result mutated the retained record")
	}
	wantBytes := int64(0)
	for _, raw := range []FDBIndexedRawRecord{firstRaw, secondRaw} {
		wantBytes += int64(len(raw.RawRecord.GetRange.Rows[0].Key) + len(raw.RawRecord.GetRange.Rows[0].Value))
	}
	if got := loader.scanState.BytesScanned(); got != wantBytes {
		t.Fatalf("raw byte charge = %d, want %d", got, wantBytes)
	}
	// An orphan following successful loads must clear their result, and its
	// memoized failure must not count the bad entry twice.
	store.context.SetTimer(NewStoreTimer())
	orphan := firstRaw
	orphan.RawRecord.GetRange.Rows = nil
	pending := loader.load(orphan)
	for range 2 {
		record, err := pending.Get()
		var storage *RecordCoreStorageError
		if record != nil || !errors.As(err, &storage) {
			t.Fatalf("orphan = %v, %v", record, err)
		}
	}
	if got := store.context.Timer().GetCount(CountBadIndexEntry); got != 1 {
		t.Fatalf("bad entries = %d, want 1", got)
	}
	// Errors must not leak into the next issued record.
	if record, err := loader.load(firstRaw).Get(); err != nil || record == nil {
		t.Fatalf("record after orphan = %v, %v", record, err)
	}
}

func TestRemoteRecordLoadLegacyVersionCompletion(t *testing.T) {
	t.Parallel()
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprint(fails), func(t *testing.T) {
			t.Parallel()
			store := remoteAllocationStore(t)
			raw := remoteAllocationRaw(t, store, 1001)
			record, err := store.BuildSingleRecord(&raw)
			if err != nil {
				t.Fatal(err)
			}
			version, err := IncompleteVersion(7)
			if err != nil {
				t.Fatal(err)
			}
			ready, gets := false, 0
			load := remoteRecordLoad{record: record, oldVersion: NewPendingRead(func() (*FDBRecordVersion, error) {
				gets++
				if fails {
					return nil, fdb.Error{Code: 1007}
				}
				return version, nil
			}, func() bool { return ready })}
			if load.IsReady() {
				t.Fatal("legacy version reported ready too early")
			}
			ready = true
			if !load.IsReady() {
				t.Fatal("completed legacy version not ready")
			}
			for range 2 {
				got, err := load.Get()
				if fails {
					var native fdb.Error
					if got != nil || !errors.As(err, &native) || native.Code != 1007 {
						t.Fatalf("legacy error = %v, %v", got, err)
					}
				} else if err != nil || got != record || got.Record.Version != version {
					t.Fatalf("legacy result = %v, %v", got, err)
				}
			}
			if gets != 1 || load.oldVersion != nil || !load.IsReady() {
				t.Fatalf("version completion: gets=%d, pending=%v", gets, load.oldVersion)
			}
		})
	}
}

func TestRemoteRecordReconstructionKeys(t *testing.T) {
	t.Parallel()
	store := remoteAllocationStore(t)
	for _, pk := range []tuple.Tuple{
		{int64(1001)},
		{int64(-20)},
		{"a\x00b", []byte{0, 0xff, 20}},
		{tuple.Tuple{int64(20), nil, "nested"}, int64(-1)},
	} {
		raw := remoteAllocationRaw(t, store, 1001).RawRecord
		raw.GetRange.Rows[0].Key = store.recordsSubspace.Pack(appendToTuple(pk, unsplitRecord))
		var sizes sizeInfo
		got, data, version, err := store.reconstructSingleRecord(raw, &sizes)
		if err != nil || !reflect.DeepEqual(got, pk) || version != nil || !bytes.Equal(data, raw.GetRange.Rows[0].Value) {
			t.Fatalf("primary key %v reconstructed as %v, version %v, err %v", pk, got, version, err)
		}
	}
	for i, key := range [][]byte{
		subspace.FromBytes(tuple.Tuple{"wrong-prefix"}.Pack()).Pack(tuple.Tuple{int64(1001), unsplitRecord}),
		store.recordsSubspace.Pack(tuple.Tuple{int64(20)}), // ends in 0x14, but 20 is not the unsplit suffix
		store.recordsSubspace.Pack(tuple.Tuple{int64(1001), "suffix"}),
		store.recordsSubspace.Bytes(),
	} {
		raw := remoteAllocationRaw(t, store, 1001).RawRecord
		raw.GetRange.Rows[0].Key = key
		var sizes sizeInfo
		_, _, _, err := store.reconstructSingleRecord(raw, &sizes)
		var argument *RecordCoreArgumentError
		var missingStart *FoundSplitWithoutStartError
		if i == 1 {
			if !errors.As(err, &missingStart) || missingStart.NextIndex != 20 {
				t.Fatalf("trailing 0x14 key error = %v, want missing start at 20", err)
			}
		} else if !errors.As(err, &argument) {
			t.Fatalf("invalid record key %x error = %v, want argument error", key, err)
		}
	}
	pk := tuple.Tuple{int64(1001)}
	versionKey := store.recordsSubspace.Pack(appendToTuple(pk, recordVersionSuffix))
	store.context.AddToLocalVersionCache(versionKey, 7)
	raw := remoteAllocationRaw(t, store, 1001).RawRecord
	var sizes sizeInfo
	_, _, version, err := store.reconstructSingleRecord(raw, &sizes)
	if err != nil || version == nil || version.IsComplete() || version.GetLocalVersion() != 7 || sizes.KeyCount != 2 ||
		sizes.KeySize != len(raw.GetRange.Rows[0].Key)+len(versionKey) || sizes.ValueSize != len(raw.GetRange.Rows[0].Value)+VersionBytes+1 || !sizes.VersionedInline {
		t.Fatalf("local version = %v, sizes=%+v, err=%v", version, sizes, err)
	}
}

var (
	remoteAllocationResult   *FDBIndexedRecord
	remoteAllocationOrdinary PendingRead[*FDBIndexedRecord]
)

// Isolate AllocsPerRun from parallel test workers. Prepared rows measure only
// reconstruction/materialization, not RPCs, goroutines, or store-open costs.
func TestRemoteFetchMaterializationAllocations(t *testing.T) {
	const child = "FDB_REMOTE_FETCH_ALLOCATION_CHILD"
	if os.Getenv(child) != "1" {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestRemoteFetchMaterializationAllocations$", "-test.v")
		cmd.Env = append(os.Environ(), child+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(output, []byte("remote materialization allocations:")) {
			t.Fatalf("isolated allocation test: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}
	store := remoteAllocationStore(t)
	raw := remoteAllocationRaw(t, store, 1001)
	pk := raw.IndexEntry.PrimaryKey()
	reader := &allocationRecordRange{row: fdb.KeyValue{
		Key: raw.RawRecord.GetRange.Rows[0].Key, Value: raw.RawRecord.GetRange.Rows[0].Value,
	}}
	input := pendingRecordRangeInput{primaryKey: pk, prefix: store.recordsSubspace.Pack(pk), rangeRead: reader}
	ordinary := &indexRecordLoader{store: store}
	remote := &remoteIndexRecordLoader{store: store, orphan: IndexOrphanError}
	const samples = 1000
	var err error
	scanAndFetch := testing.AllocsPerRun(samples, func() {
		remoteAllocationOrdinary = &indexRecordLoad{
			loader: ordinary, entry: raw.IndexEntry, primaryKey: pk,
			pending: &recordLoad{store: store, primaryKey: pk, first: pendingStoreRecordLoad{raw: pendingRecordLoad{pendingRecordRangeInput: input}}},
		}
		remoteAllocationResult, err = remoteAllocationOrdinary.Get()
		if err != nil {
			t.Fatal(err)
		}
	})
	check := func() {
		if remoteAllocationResult == nil || remoteAllocationResult.IndexEntry != raw.IndexEntry || remoteAllocationResult.Record.PrimaryKey[0] != int64(1001) ||
			remoteAllocationResult.Record.Record.(*demopb.Order).GetOrderId() != 1001 || remoteAllocationResult.Record.KeyCount != 1 {
			t.Fatal("allocation sample did not materialize the expected record")
		}
	}
	check()
	useRemoteFetch := testing.AllocsPerRun(samples, func() {
		remoteAllocationResult, err = remote.load(raw).Get()
		if err != nil {
			t.Fatal(err)
		}
	})
	check()
	// Building directly is the control: the size-one cursor's reusable pending
	// state must add no allocation to actual record reconstruction.
	direct := testing.AllocsPerRun(samples, func() {
		remoteAllocationResult, err = store.BuildSingleRecord(&raw)
		if err != nil {
			t.Fatal(err)
		}
	})
	check()
	if reader.iterators != samples+1 || reader.advances != 2*(samples+1) || reader.gets != 2*(samples+1) {
		t.Fatalf("ordinary population: iterators=%d advances=%d gets=%d", reader.iterators, reader.advances, reader.gets)
	}
	t.Logf("remote materialization allocations: samples=%d SCAN_AND_FETCH=%.0f USE_REMOTE_FETCH=%.0f direct=%.0f per row", samples, scanAndFetch, useRemoteFetch, direct)
	if scanAndFetch <= 0 || useRemoteFetch <= 0 || useRemoteFetch != direct || useRemoteFetch > scanAndFetch {
		t.Fatalf("allocation regression: scan=%g remote=%g direct=%g", scanAndFetch, useRemoteFetch, direct)
	}
	// Canonical tuple unpack is the previous reconstruction's control. Keep
	// the complete PK and suffix escaping, as the returned record's PK does.
	var decoded tuple.Tuple
	canonical := testing.AllocsPerRun(samples, func() {
		decoded, err = store.recordsSubspace.Unpack(fdb.Key(reader.row.Key))
		if err != nil {
			t.Fatal(err)
		}
	})
	if len(decoded) != 2 || decoded[0] != int64(1001) || decoded[1] != int64(0) {
		t.Fatal("canonical decode control returned a wrong key")
	}
	var sizes sizeInfo
	fast := testing.AllocsPerRun(samples, func() {
		decoded, _, _, err = store.reconstructSingleRecord(raw.RawRecord, &sizes)
		if err != nil {
			t.Fatal(err)
		}
	})
	if len(decoded) != 1 || decoded[0] != int64(1001) || sizes.KeyCount != 1 || fast >= canonical {
		t.Fatalf("key decode allocation regression: reconstruct=%g canonical=%g key=%v sizes=%+v", fast, canonical, decoded, sizes)
	}
	t.Logf("remote key allocations: reconstruct=%.0f canonical=%.0f; outputs independently owned", fast, canonical)
}

// Keep the index-entry-to-raw adapter's caller-visible state independently owned
// while its raw wrapper is passed by value to the size-one pipeline.
func TestRemotePrefetchValueCursor(t *testing.T) {
	t.Parallel()
	store := remoteAllocationStore(t)
	raw := remoteAllocationRaw(t, store, 1001)
	indexSS := subspace.FromBytes(tuple.Tuple{"prefetch"}.Pack())
	rows := &mappedIndexRows{rows: &remoteAllocationMappedRows{row: fdb.MappedKeyValue{
		Key: indexSS.Pack(raw.IndexEntry.Key), Kind: raw.RawRecord.Kind, GetRange: raw.RawRecord.GetRange,
	}}}
	entries := newIndexCursor(raw.IndexEntry.Index, indexSS, nil, TupleRangeAll, nil, NewScanProperties(DefaultExecuteProperties()))
	entries.iterator = rows
	cursor := &indexPrefetchCursor{entries: entries, rows: rows}
	first, err := cursor.OnNext(context.Background())
	if err != nil || !first.HasNext() || !reflect.DeepEqual(first.GetValue().IndexEntry.Key, raw.IndexEntry.Key) {
		t.Fatalf("prefetch result = %v, %v", first, err)
	}
	cont, err := first.GetContinuation().ToBytes()
	if err != nil || len(cont) == 0 {
		t.Fatalf("prefetch continuation = %x, %v", cont, err)
	}
	for range 2 {
		stop, err := cursor.OnNext(context.Background())
		if err != nil || stop.HasNext() || stop.GetNoNextReason() != SourceExhausted {
			t.Fatalf("prefetch terminal = %v, %v", stop, err)
		}
	}
	if err := cursor.Close(); err != nil || !cursor.IsClosed() {
		t.Fatalf("prefetch close = %v", err)
	}
}

type remoteAllocationMappedRows struct {
	row  fdb.MappedKeyValue
	done bool
}

func (r *remoteAllocationMappedRows) Advance() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}

func (r *remoteAllocationMappedRows) Get() (fdb.MappedKeyValue, error)           { return r.row, nil }
func (r *remoteAllocationMappedRows) MustGet() fdb.MappedKeyValue                { return r.row }
func (*remoteAllocationMappedRows) SetTraceLog(func(int, int, int, bool, error)) {}
