package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Java MapPipelinedCursor returns a completed head immediately after issuing a
// mapping, before another source pull can fail (tryToFillPipeline, lines 163-164).
func TestFDB_IndexCursorReadyHeadBeforeSourceFailure(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
		SetStoreRecordVersions(true)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewIndex("price", Field("price")))
	builder.AddIndex("Order", NewVersionIndex("version", VersionKey()).SetSubspaceKey("version"))
	metadata, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}

	for _, indexName := range []string{"price", "version"} {
		for _, reverse := range []bool{false, true} {
			for _, ready := range []bool{false, true} {
				t.Run(fmt.Sprintf("index=%s/reverse=%v/ready=%v", indexName, reverse, ready), func(t *testing.T) {
					t.Parallel()
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
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
						if _, err := seedStore.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(300 - 100*id))}); err != nil {
							t.Fatal(err)
						}
					}
					assertLoadParityCommit(t, seed, false)

					control := loadParityContext(t, db)
					controlStore := open(control)
					index := metadata.GetIndex(indexName)
					indexSpace := controlStore.IndexSubspace(index)
					indexRows, err := control.Transaction().GetRange(indexSpace, fdb.RangeOptions{Reverse: reverse}).GetSliceWithError()
					if err != nil || len(indexRows) != 2 {
						t.Fatalf("index positive control: rows=%d error=%v, want two entries", len(indexRows), err)
					}
					entryCursor := controlStore.ScanIndex(index, TupleRangeAll, nil, ForwardScan().WithReverse(reverse))
					entryResult, err := entryCursor.OnNext(t.Context())
					if err != nil || !entryResult.HasNext() {
						t.Fatalf("index positive control: result=%+v error=%v", entryResult, err)
					}
					if err := entryCursor.Close(); err != nil {
						t.Fatal(err)
					}
					entry := entryResult.GetValue()
					firstID := int64(1)
					if (indexName == "price" && !reverse) || (indexName == "version" && reverse) {
						firstID = 2
					}
					pk := tuple.Tuple{firstID}
					if !bytes.Equal(entry.PrimaryKey().Pack(), pk.Pack()) || !bytes.Equal(indexSpace.Pack(entry.Key), indexRows[0].Key) {
						t.Fatalf("first index entry key=%v primary=%v, want primary=%v and native key=%x", entry.Key, entry.PrimaryKey(), pk, indexRows[0].Key)
					}
					wantToken, err := entryResult.GetContinuation().ToBytes()
					if err != nil || len(wantToken) == 0 {
						t.Fatalf("index continuation positive control: token=%x error=%v", wantToken, err)
					}
					stored, err := controlStore.LoadRecord(pk)
					wantRecord := &demopb.Order{OrderId: proto.Int64(firstID), Price: proto.Int32(int32(300 - 100*firstID))}
					if err != nil || stored == nil || stored.Version == nil || !proto.Equal(stored.Record, wantRecord) {
						t.Fatalf("record positive control: record=%v error=%v, want %v with version", stored, err, wantRecord)
					}
					indexBytes := int64(len(indexRows[0].Key) + len(indexRows[0].Value))
					recordBytes := int64(stored.KeySize + stored.ValueSize)
					if indexBytes <= 0 || recordBytes <= 0 {
						t.Fatalf("empty byte-count positive control: index=%d record=%d", indexBytes, recordBytes)
					}

					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
					t.Cleanup(cancel)
					read := loadParityContext(t, db)
					begin, end := recordRangeBounds(controlStore.recordsSubspace.Pack(pk))
					probe := &recordChargeTransaction{
						WritableTransaction: read.Transaction(), ctx: ctx, begin: begin, end: end,
						nativeReady: make(chan error, 1), release: make(chan struct{}),
					}
					store := open(NewFDBRecordContext(probe, nil))
					var pending *recordLoad
					var raw *pendingRecordRange
					var releaseOnce sync.Once
					release := func() { releaseOnce.Do(func() { close(probe.release) }) }
					t.Cleanup(func() {
						release()
						probe.Cancel()
						if raw != nil {
							select {
							case <-raw.done:
							case <-time.After(5 * time.Second):
								t.Error("record worker did not finish after gate release and transaction cancellation")
							}
						}
					})

					properties := DefaultExecuteProperties()
					properties.ScannedRecordsLimit = 1
					properties.FailOnScanLimitReached = true
					properties.ScanState = NewScanLimiterState()
					state := properties.ScanState
					cursor := store.ScanIndexRecords(indexName, TupleRangeAll, nil, NewScanProperties(properties).WithReverse(reverse))
					t.Cleanup(func() {
						if err := cursor.Close(); err != nil {
							t.Error(err)
						}
					})
					pipeline, ok := cursor.(*mapPipelinedCursor[*IndexEntry, *FDBIndexedRecord])
					if !ok {
						t.Fatalf("ScanIndexRecords returned %T, want production mapPipelinedCursor", cursor)
					}
					issue := pipeline.issue
					issued := 0
					pipeline.issue = func(entry *IndexEntry) PendingRead[*FDBIndexedRecord] {
						issued++
						load := issue(entry)
						indexed, ok := load.(*indexRecordLoad)
						if !ok {
							t.Fatalf("index loader returned %T, want production indexRecordLoad", load)
						}
						pending, ok = indexed.pending.(*recordLoad)
						if !ok || pending.first.raw.asyncRange == nil {
							t.Fatalf("record loader returned %T, want production asynchronous recordLoad", indexed.pending)
						}
						raw = pending.first.raw.asyncRange
						select {
						case err := <-probe.nativeReady:
							if err != nil {
								t.Fatalf("native record did not reach the gate: %v", err)
							}
						case <-ctx.Done():
							t.Fatalf("waiting for native record gate: %v", ctx.Err())
						}
						if ready {
							release()
							select {
							case <-raw.done:
							case <-ctx.Done():
								t.Fatalf("waiting for raw record completion: %v", ctx.Err())
							}
							if raw.err != nil || raw.value == nil || raw.version == nil {
								t.Fatalf("completed raw load: value=%x version=%v error=%v", raw.value, raw.version, raw.err)
							}
						}
						// Return the real mapping, not a synthetic readiness wrapper.
						if load.IsReady() != ready || pending.done || pending.loaded {
							t.Fatalf("mapping gate: ready=%v done=%v loaded=%v, want ready=%v and no materialization", load.IsReady(), pending.done, pending.loaded, ready)
						}
						return load
					}

					next, err := cursor.OnNext(ctx)
					if issued != 1 || probe.ranges.Load() != 1 || raw == nil {
						t.Fatalf("gate population: mappings=%d rawRanges=%d raw=%p, want 1,1,non-nil", issued, probe.ranges.Load(), raw)
					}
					if ready {
						if err != nil || !next.HasNext() {
							t.Fatalf("completed head lost to a later source failure: result=%+v error=%v, want first row", next, err)
						}
						got := next.GetValue()
						if !bytes.Equal(got.IndexEntry.Key.Pack(), entry.Key.Pack()) || !bytes.Equal(got.Record.PrimaryKey.Pack(), pk.Pack()) || !proto.Equal(got.Record.Record, stored.Record) || got.Record.Version == nil || !got.Record.Version.Equal(stored.Version) {
							t.Fatalf("ready row=%+v, want key=%v primary=%v record=%v version=%v", got, entry.Key, pk, stored.Record, stored.Version)
						}
						token, tokenErr := next.GetContinuation().ToBytes()
						if tokenErr != nil || next.GetContinuation().IsEnd() || !bytes.Equal(token, wantToken) {
							t.Fatalf("ready row continuation: token=%x end=%v error=%v, want %x", token, next.GetContinuation().IsEnd(), tokenErr, wantToken)
						}
						if state.RecordsScanned() != 1 || state.BytesScanned() != indexBytes+recordBytes {
							t.Fatalf("ready row already pulled later source: attempts=%d bytes=%d, want 1/%d", state.RecordsScanned(), state.BytesScanned(), indexBytes+recordBytes)
						}
						next, err = cursor.OnNext(ctx)
					} else {
						select {
						case <-raw.done:
							t.Fatal("pending head completed before its gate was released")
						default:
						}
						if state.BytesScanned() != indexBytes || pending.done || pending.loaded {
							t.Fatalf("pending head was consumed: bytes=%d done=%v loaded=%v, want %d,false,false", state.BytesScanned(), pending.done, pending.loaded, indexBytes)
						}
					}
					var limit *ScanLimitReachedError
					if !errors.As(err, &limit) || next.HasNext() || state.RecordsScanned() != 2 {
						t.Fatalf("source failure: result=%+v error=%v attempts=%d, want no row, ScanLimitReachedError, and two attempts", next, err, state.RecordsScanned())
					}
					failure := err
					release()
					select {
					case <-raw.done:
					case <-ctx.Done():
						t.Fatalf("record worker did not finish after gate release: %v", ctx.Err())
					}
					state.WaitForIssuedRecordLoadCharges()
					if raw.err != nil || raw.value == nil || raw.version == nil || int64(raw.sizes.KeySize+raw.sizes.ValueSize) != recordBytes || state.BytesScanned() != indexBytes+recordBytes {
						t.Fatalf("quiescent record charge: rawError=%v sizes=%+v bytes=%d, want record=%d total=%d", raw.err, raw.sizes, state.BytesScanned(), recordBytes, indexBytes+recordBytes)
					}
					again, err := cursor.OnNext(ctx)
					if err != failure || again.HasNext() || state.RecordsScanned() != 2 || state.BytesScanned() != indexBytes+recordBytes || issued != 1 || probe.ranges.Load() != 1 {
						t.Fatalf("sticky failure changed after completion: result=%+v error=%v attempts=%d bytes=%d mappings=%d rawRanges=%d", again, err, state.RecordsScanned(), state.BytesScanned(), issued, probe.ranges.Load())
					}
					retained, err := pending.Get()
					if err != nil || retained == nil || !proto.Equal(retained.Record, stored.Record) || state.BytesScanned() != indexBytes+recordBytes {
						t.Fatalf("retained real load: record=%v error=%v bytes=%d, want %v and unchanged %d bytes", retained, err, state.BytesScanned(), stored.Record, indexBytes+recordBytes)
					}
				})
			}
		}
	}
}
