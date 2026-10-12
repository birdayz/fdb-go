package recordlayer

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

var legacyVersionAPIs = []string{"LoadRecord", "LoadRecordAsync", "SaveRecord", "SaveRecordsPipelined", "SaveRecordBatch", "DeleteRecord", "DryRunSaveRecord", "DryRunDeleteRecord"}

func legacyVersionStore(t *testing.T, rc *FDBRecordContext, root subspace.Subspace, md *RecordMetaData, format int32, create bool) *FDBRecordStore {
	t.Helper()
	b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
	if format != 0 {
		b.SetFormatVersion(format)
	}
	var store *FDBRecordStore
	var err error
	if create {
		store, err = b.Create()
	} else {
		store, err = b.Open()
	}
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func legacyVersionKeys(root subspace.Subspace, format int32) (fdb.Key, fdb.Key) {
	recordKey := root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(0)})
	if format == 4 {
		recordKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(1)})
	}
	versionKey := root.Sub(RecordVersionKey).Pack(tuple.Tuple{int64(1)})
	if format == 0 {
		versionKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(-1)})
	}
	return recordKey, versionKey
}

func runLegacyVersionAPI(t *testing.T, store *FDBRecordStore, api string, exists bool) error {
	t.Helper()
	pk := tuple.Tuple{int64(1)}
	original := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	replacement := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
	var got *FDBStoredRecord[proto.Message]
	var err error
	want := original
	switch api {
	case "LoadRecord":
		got, err = store.LoadRecord(pk)
	case "LoadRecordAsync":
		timer := NewStoreTimer()
		store.context.SetTimer(timer)
		scan := NewScanLimiterState()
		pending := store.LoadRecordAsync(pk, scan)
		got, err = pending.Get()
		charged := scan.BytesScanned()
		elapsed := timer.GetTimeNanos(EventLoadRecord)
		if elapsed <= 0 {
			t.Fatalf("load time = %d, want recorded duration", elapsed)
		}
		if got != nil && err == nil {
			if charged != int64(got.KeySize+got.ValueSize) {
				t.Fatalf("loaded byte charge = %d, want %d", charged, got.KeySize+got.ValueSize)
			}
		}
		for range 3 {
			if !pending.IsReady() {
				t.Fatal("completed public LoadRecordAsync is not ready")
			}
			if again, againErr := pending.Get(); again != got || againErr != err {
				t.Fatalf("public LoadRecordAsync changed result: (%p, %v), first (%p, %v)", again, againErr, got, err)
			}
		}
		if timer.GetCount(EventLoadRecord) != 1 || timer.GetTimeNanos(EventLoadRecord) != elapsed || scan.BytesScanned() != charged {
			t.Fatalf("repeated Get accounting: timer=%d time=%d bytes=%d; want 1,%d,%d", timer.GetCount(EventLoadRecord), timer.GetTimeNanos(EventLoadRecord), scan.BytesScanned(), elapsed, charged)
		}
		if err != nil && got != nil {
			t.Fatalf("failed public LoadRecordAsync returned record: %v", got)
		}
	case "DeleteRecord", "DryRunDeleteRecord":
		var deleted bool
		if api == "DeleteRecord" {
			deleted, err = store.DeleteRecord(pk)
		} else {
			deleted, err = store.DryRunDeleteRecord(pk)
		}
		if err == nil && deleted != exists {
			t.Fatalf("%s = %t, want %t", api, deleted, exists)
		}
		return err
	case "SaveRecord", "SaveRecordErrorIfExists":
		want, exists = replacement, true
		if api == "SaveRecord" {
			got, err = store.SaveRecord(replacement)
		} else {
			got, err = store.SaveRecordWithOptions(replacement, RecordExistenceCheckErrorIfExists)
		}
	case "DryRunSaveRecord", "DryRunSaveRecordErrorIfExists":
		want, exists = replacement, true
		check := RecordExistenceCheckNone
		if api == "DryRunSaveRecordErrorIfExists" {
			check = RecordExistenceCheckErrorIfExists
		}
		got, err = store.DryRunSaveRecord(replacement, check)
	case "SaveRecordsPipelined", "SaveRecordBatch":
		want, exists = replacement, true
		var records []*FDBStoredRecord[proto.Message]
		if api == "SaveRecordsPipelined" {
			records, err = store.SaveRecordsPipelined([]proto.Message{replacement}, RecordExistenceCheckNone)
		} else {
			records, err = store.SaveRecordBatch([]proto.Message{replacement})
		}
		if err == nil {
			if len(records) != 1 {
				t.Fatalf("%s returned %d records, want 1", api, len(records))
			}
			got = records[0]
		}
	default:
		t.Fatalf("unknown API %s", api)
	}
	if err == nil {
		if (got != nil) != exists {
			t.Fatalf("%s returned %v, want exists=%t", api, got, exists)
		}
		if exists && (!proto.Equal(got.Record, want) || !bytes.Equal(got.PrimaryKey.Pack(), pk.Pack())) {
			t.Fatalf("%s returned wrong record: %+v", api, got)
		}
	}
	return err
}

func TestFDB_LegacyRecordVersionReads(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	original := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: original})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := NewCompleteVersion([]byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 1}, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []int32{4, 5} {
		for _, enabled := range []bool{false, true} {
			md := loadParityMetaData(t, false, enabled)
			for _, exists := range []bool{false, true} {
				for _, api := range legacyVersionAPIs {
					t.Run(fmt.Sprintf("conflict/format=%d/versions=%t/exists=%t/%s", format, enabled, exists, api), func(t *testing.T) {
						root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
						seed := loadParityContext(t, db)
						legacyVersionStore(t, seed, root, md, format, true)
						recordKey, versionKey := legacyVersionKeys(root, format)
						if exists {
							seed.Transaction().Set(recordKey, data)
							seed.Transaction().Set(versionKey, complete.ToBytes())
						}
						assertLoadParityCommit(t, seed, false)
						reader := loadParityContext(t, db)
						store := legacyVersionStore(t, reader, root, md, format, false)
						if err := runLegacyVersionAPI(t, store, api, exists); err != nil {
							t.Fatal(err)
						}
						writer := loadParityContext(t, db)
						writer.Transaction().Set(versionKey, complete.ToBytes())
						assertLoadParityCommit(t, writer, false)
						reader.Transaction().Set(root.Sub("sentinel").Pack(nil), []byte("loaded"))
						assertLoadParityCommit(t, reader, enabled)
					})
				}
			}
			for _, local := range []bool{false, true} {
				t.Run(fmt.Sprintf("sizes/format=%d/versions=%t/local=%t", format, enabled, local), func(t *testing.T) {
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					seed := loadParityContext(t, db)
					legacyVersionStore(t, seed, root, md, format, true)
					recordKey, versionKey := legacyVersionKeys(root, format)
					seed.Transaction().Set(recordKey, data)
					seed.Transaction().Set(versionKey, complete.ToBytes())
					assertLoadParityCommit(t, seed, false)
					reader := loadParityContext(t, db)
					store := legacyVersionStore(t, reader, root, md, format, false)
					wantVersion := complete
					if local {
						reader.AddToLocalVersionCache(versionKey, 9)
						wantVersion, err = IncompleteVersion(9)
						if err != nil {
							t.Fatal(err)
						}
					}
					if !enabled {
						wantVersion = nil
					}
					got, err := store.LoadRecord(tuple.Tuple{int64(1)})
					if err != nil || got == nil || !proto.Equal(got.Record, original) {
						t.Fatalf("load = %v (err=%v)", got, err)
					}
					if !got.Version.Equal(wantVersion) || got.KeyCount != 1 || got.KeySize != len(recordKey) || got.ValueSize != len(data) || got.Split {
						t.Fatalf("legacy version or sizes incorrect: %+v, want version %v and sizes (1,%d,%d,false)", got, wantVersion, len(recordKey), len(data))
					}
				})
			}
		}
		md := loadParityMetaData(t, false, true)
		for _, state := range []string{"missing", "valid", "corrupt"} {
			for _, api := range append(append([]string{}, legacyVersionAPIs...), "SaveRecordErrorIfExists", "DryRunSaveRecordErrorIfExists") {
				t.Run(fmt.Sprintf("precedence/format=%d/%s/%s", format, state, api), func(t *testing.T) {
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					seed := loadParityContext(t, db)
					legacyVersionStore(t, seed, root, md, format, true)
					recordKey, versionKey := legacyVersionKeys(root, format)
					if state == "valid" {
						seed.Transaction().Set(recordKey, data)
					} else if state == "corrupt" {
						seed.Transaction().Set(recordKey, []byte{0xff})
					}
					seed.Transaction().Set(versionKey, []byte{1})
					assertLoadParityCommit(t, seed, false)
					reader := loadParityContext(t, db)
					store := legacyVersionStore(t, reader, root, md, format, false)
					err := runLegacyVersionAPI(t, store, api, state != "missing")
					var deser *RecordDeserializationError
					switch state {
					case "missing":
						if err != nil {
							t.Fatalf("missing record observed unused version error: %v", err)
						}
					case "corrupt":
						if !errors.As(err, &deser) {
							t.Fatalf("error = %v, want record deserialization before version failure", err)
						}
					case "valid":
						if err == nil || errors.As(err, &deser) || !strings.Contains(err.Error(), "version must be 12 bytes") {
							t.Fatalf("error = %v, want separate version decode failure", err)
						}
					}
				})
			}
		}
	}
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("inline-wins/async=%t", async), func(t *testing.T) {
			root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			md := loadParityMetaData(t, false, true)
			seed := loadParityContext(t, db)
			legacyVersionStore(t, seed, root, md, 5, true)
			recordKey, versionKey := legacyVersionKeys(root, 5)
			inlineKey := root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(-1)})
			inlineBytes := tuple.Tuple{tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 1}, UserVersion: 7}}.Pack()
			seed.Transaction().Set(recordKey, data)
			seed.Transaction().Set(inlineKey, inlineBytes)
			seed.Transaction().Set(versionKey, []byte{1})
			assertLoadParityCommit(t, seed, false)
			reader := loadParityContext(t, db)
			store := legacyVersionStore(t, reader, root, md, 5, false)
			got, err := loadParityRead(store, async)
			if err != nil || got == nil || !proto.Equal(got.Record, original) || !got.Version.Equal(complete) {
				t.Fatalf("inline version did not win: %v (err=%v)", got, err)
			}
			if got.KeyCount != 2 || got.KeySize != len(recordKey)+len(inlineKey) || got.ValueSize != len(data)+13 {
				t.Fatalf("inline sizes wrong: %+v", got)
			}
			writer := loadParityContext(t, db)
			writer.Transaction().Set(versionKey, complete.ToBytes())
			assertLoadParityCommit(t, writer, false)
			reader.Transaction().Set(root.Sub("sentinel").Pack(nil), []byte("loaded"))
			assertLoadParityCommit(t, reader, true)
		})
	}
}

func TestFDB_LoadRecordVersionConflictScope(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	complete, err := NewCompleteVersion([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []int32{4, 5, 0} {
		for _, mode := range []string{"serializable", "snapshot", "local-cache", "versions-disabled"} {
			md := loadParityMetaData(t, false, mode != "versions-disabled")
			for _, change := range []string{"version", "record", "chunk", "unrelated-99"} {
				t.Run(fmt.Sprintf("format=%d/%s/%s", format, mode, change), func(t *testing.T) {
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					seed := loadParityContext(t, db)
					legacyVersionStore(t, seed, root, md, format, true)
					recordKey, versionKey := legacyVersionKeys(root, format)
					versionBytes := complete.ToBytes()
					if format == 0 {
						versionBytes = tuple.Tuple{tuple.Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 7}}.Pack()
					}
					seed.Transaction().Set(versionKey, versionBytes)
					assertLoadParityCommit(t, seed, false)
					reader := loadParityContext(t, db)
					store := legacyVersionStore(t, reader, root, md, format, false)
					want := complete
					if mode == "local-cache" {
						reader.AddToLocalVersionCache(versionKey, 9)
						want, err = IncompleteVersion(9)
						if err != nil {
							t.Fatal(err)
						}
					} else if mode == "versions-disabled" && format != 0 {
						want = nil
					}
					got, err := store.LoadRecordVersion(tuple.Tuple{int64(1)}, mode == "snapshot")
					if err != nil || !got.Equal(want) {
						t.Fatalf("version = %v, want %v (err=%v)", got, want, err)
					}
					changedKey := versionKey
					switch change {
					case "record":
						changedKey = recordKey
					case "chunk":
						changedKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(1)})
					case "unrelated-99":
						changedKey = root.Sub(RecordVersionKey).Pack(tuple.Tuple{int64(99)})
					}
					writer := loadParityContext(t, db)
					writer.Transaction().Set(changedKey, []byte("concurrent"))
					assertLoadParityCommit(t, writer, false)
					reader.Transaction().Set(root.Sub("sentinel").Pack(nil), []byte("loaded"))
					wantConflict := change == "version" && (mode == "serializable" || (mode == "versions-disabled" && format == 0))
					assertLoadParityCommit(t, reader, wantConflict)
				})
			}
		}
	}
}

func TestFDB_DisabledVersionOverwriteCancelsPendingMutation(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	withVersions := loadParityMetaData(t, false, true)
	withoutVersionsBuilder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
		SetVersion(withVersions.Version() + 1).SetStoreRecordVersions(false)
	withoutVersionsBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	withoutVersionsBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	withoutVersionsBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	withoutVersions, err := withoutVersionsBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if withoutVersions.IsStoreRecordVersions() {
		t.Fatal("overwrite metadata must disable record versions")
	}
	for _, api := range []string{"SaveRecord", "SaveRecordBatch"} {
		t.Run(api, func(t *testing.T) {
			root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			rc := loadParityContext(t, db)
			store := legacyVersionStore(t, rc, root, withVersions, 0, true)
			original := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
			first, err := store.SaveRecord(original)
			if err != nil || first == nil || first.Version == nil || first.Version.IsComplete() {
				t.Fatalf("first save did not create a pending version: %v (err=%v)", first, err)
			}
			store = legacyVersionStore(t, rc, root, withoutVersions, 0, false)
			if err := runLegacyVersionAPI(t, store, api, true); err != nil {
				t.Fatal(err)
			}
			_, versionKey := legacyVersionKeys(root, 0)
			// Java removes the queued mutation, not the transaction's local
			// version cache entry. The next transaction must see no version.
			if local, ok := rc.GetLocalVersion(versionKey); !ok || local != first.Version.GetLocalVersion() {
				t.Fatalf("overwrite discarded the cached local version: local=%d present=%t", local, ok)
			}
			assertLoadParityCommit(t, rc, false)
			reader := loadParityContext(t, db)
			physical, err := reader.Transaction().Get(versionKey).Get()
			if err != nil || physical != nil {
				t.Fatalf("disabled overwrite resurrected an inline version at commit: %x (err=%v)", physical, err)
			}
			store = legacyVersionStore(t, reader, root, withoutVersions, 0, false)
			got, err := store.LoadRecord(tuple.Tuple{int64(1)})
			want := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			if err != nil || got == nil || !proto.Equal(got.Record, want) || got.Version != nil || got.KeyCount != 1 {
				t.Fatalf("committed overwrite = %v, want unversioned replacement (err=%v)", got, err)
			}
		})
	}
}
