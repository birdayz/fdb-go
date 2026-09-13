package recordlayer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"google.golang.org/protobuf/proto"
)

// Page a Chained cursor driven by real record scans, reopening the transaction
// for every row. ConcatCursors serializes its child's position through Java's
// ConcatContinuation proto. The first record's key value encodes to non-nil
// empty bytes, so losing that field's presence or treating an empty decoded
// continuation as a fresh start replays the record.
func TestFDB_ChainedCursorEmptyContinuation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := container.Terminate(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clusterPath := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(clusterPath, []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rawDB.Close)
	db := NewFDBDatabase(rawDB)

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("name"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	want := []string{"", "a", "b"}
	_, err = db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for i, name := range want {
			if _, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(int64(i)), Name: proto.String(name)}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var continuation []byte
	for page := 0; page <= len(want); page++ {
		out, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			newChained := func(raw []byte) RecordCursor[string] {
				return Chained(
					func(prev *string) (*string, error) {
						var low tuple.Tuple
						endpoint := EndpointTypeTreeStart
						if prev != nil {
							low = tuple.Tuple{*prev}
							endpoint = EndpointTypeRangeExclusive
						}
						props := ForwardScan()
						props.ExecuteProperties.ReturnedRowLimit = 1
						scan := store.ScanRecordsInRange(low, nil, endpoint, EndpointTypeTreeEnd, nil, props)
						defer func() { _ = scan.Close() }()
						result, err := scan.OnNext(ctx)
						if err != nil || !result.HasNext() {
							return nil, err
						}
						name := result.GetValue().Record.(*gen.Customer).GetName()
						return &name, nil
					},
					func(name string) []byte { return []byte(name) },
					func(raw []byte) (string, bool) { return string(raw), true },
					raw,
				)
			}
			cursor := ConcatCursors(func([]byte) RecordCursor[string] { return Empty[string]() }, newChained, continuation)
			defer func() { _ = cursor.Close() }()
			return cursor.OnNext(ctx)
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		result := out.(RecordCursorResult[string])
		if page == len(want) {
			if result.HasNext() || result.GetNoNextReason() != SourceExhausted || !result.GetContinuation().IsEnd() {
				t.Fatalf("page %d: want source exhaustion after all records, got %+v", page, result)
			}
			break
		}
		if !result.HasNext() || result.GetValue() != want[page] {
			t.Fatalf("page %d: want %q, got %+v; an empty continuation must resume, not replay the first record", page, want[page], result)
		}
		continuation, err = result.GetContinuation().ToBytes()
		if err != nil || continuation == nil || result.GetContinuation().IsEnd() {
			t.Fatalf("page %d: want non-terminal continuation, got %v (err=%v)", page, continuation, err)
		}
		var wrapped gen.ConcatContinuation
		if err := wrapped.UnmarshalVT(continuation); err != nil {
			t.Fatalf("page %d: decode Concat continuation: %v", page, err)
		}
		if !wrapped.GetSecond() || wrapped.Continuation == nil || string(wrapped.Continuation) != want[page] {
			t.Fatalf("page %d: Concat must preserve the child's position %q and field presence, got %v", page, want[page], &wrapped)
		}
		// Java sets both fields, including the empty child's bytes: second=true
		// (field 1) and a present zero-length continuation (field 3).
		if page == 0 && !bytes.Equal(continuation, []byte{0x08, 0x01, 0x1a, 0x00}) {
			t.Fatalf("empty child position did not retain Java's wire encoding: %x", continuation)
		}
	}
}
