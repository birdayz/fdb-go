package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"google.golang.org/protobuf/proto"
)

// A full record load protects the entire primary-key subspace, including absent
// chunks and inline versions. The legacy bare-key layout protects only its key.
func TestFDB_RecordLoadConflicts(t *testing.T) {
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
		if err := foundationdbtc.Terminate(cleanupCtx, container); err != nil {
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

	primaryKey := tuple.Tuple{int64(1)}
	original := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	replacement := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
	assertRecord := func(t *testing.T, got *FDBStoredRecord[proto.Message], want proto.Message) {
		t.Helper()
		if got == nil {
			t.Fatal("expected one populated record, got nil")
		}
		if !bytes.Equal(got.PrimaryKey.Pack(), primaryKey.Pack()) || !proto.Equal(got.Record, want) {
			t.Fatalf("record = %v at %v, want %v at %v", got.Record, got.PrimaryKey, want, primaryKey)
		}
		if got.RecordType == nil || got.RecordType.Name != "Order" || got.Split || got.KeyCount != 1 || got.Version != nil {
			t.Fatalf("expected an unversioned, unsplit Order with one key, got %+v", got)
		}
	}
	assertLoad := func(t *testing.T, got *FDBStoredRecord[proto.Message], exists bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			assertRecord(t, got, original)
		} else if got != nil {
			t.Fatalf("missing record load returned %+v", got)
		}
	}
	assertSave := func(t *testing.T, got *FDBStoredRecord[proto.Message], err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		assertRecord(t, got, replacement)
	}
	assertDelete := func(t *testing.T, got, exists bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if got != exists {
			t.Fatalf("delete result = %t, want %t", got, exists)
		}
	}
	apis := []struct {
		name         string
		existingOnly bool
		modernOnly   bool
		conflict     func(bool, string) bool
		run          func(*testing.T, *FDBRecordStore, bool)
	}{
		{name: "LoadRecord", run: func(t *testing.T, store *FDBRecordStore, exists bool) {
			got, err := store.LoadRecord(primaryKey)
			assertLoad(t, got, exists, err)
		}},
		{name: "LoadRecordAsync", run: func(t *testing.T, store *FDBRecordStore, exists bool) {
			got, err := store.LoadRecordAsync(primaryKey, nil).Get()
			assertLoad(t, got, exists, err)
		}},
		{name: "SaveRecord", run: func(t *testing.T, store *FDBRecordStore, _ bool) {
			got, err := store.SaveRecord(replacement)
			assertSave(t, got, err)
		}},
		{name: "SaveRecordsPipelined", run: func(t *testing.T, store *FDBRecordStore, _ bool) {
			got, err := store.SaveRecordsPipelined([]proto.Message{replacement}, RecordExistenceCheckNone)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("pipelined save returned %d records, want 1", len(got))
			}
			assertRecord(t, got[0], replacement)
		}},
		{name: "SaveRecordBatch", run: func(t *testing.T, store *FDBRecordStore, _ bool) {
			got, err := store.SaveRecordBatch([]proto.Message{replacement})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("batch save returned %d records, want 1", len(got))
			}
			assertRecord(t, got[0], replacement)
		}},
		{name: "DeleteRecord", run: func(t *testing.T, store *FDBRecordStore, exists bool) {
			got, err := store.DeleteRecord(primaryKey)
			assertDelete(t, got, exists, err)
		}},
		{name: "DryRunSaveRecord", run: func(t *testing.T, store *FDBRecordStore, _ bool) {
			got, err := store.DryRunSaveRecord(replacement, RecordExistenceCheckNone)
			assertSave(t, got, err)
		}},
		{name: "DryRunDeleteRecord", run: func(t *testing.T, store *FDBRecordStore, exists bool) {
			got, err := store.DryRunDeleteRecord(primaryKey)
			assertDelete(t, got, exists, err)
		}},
		{
			name: "RecordExists", modernOnly: true,
			// Java's keyExists reads one row, not the whole record: an existing
			// unsplit key ends the read before suffix 1, but includes absent -1.
			conflict: func(exists bool, change string) bool {
				return change != "unrelated-99" && (!exists || change != "first-chunk")
			},
			run: func(t *testing.T, store *FDBRecordStore, exists bool) {
				got, err := store.RecordExists(primaryKey, IsolationLevelSerializable)
				if err != nil || got != exists {
					t.Fatalf("RecordExists = %t, want %t (err=%v)", got, exists, err)
				}
			},
		},
		{
			name: "RecordExistsSnapshot", modernOnly: true,
			conflict: func(bool, string) bool { return false },
			run: func(t *testing.T, store *FDBRecordStore, exists bool) {
				got, err := store.RecordExists(primaryKey, IsolationLevelSnapshot)
				if err != nil || got != exists {
					t.Fatalf("snapshot RecordExists = %t, want %t (err=%v)", got, exists, err)
				}
			},
		},
		// An empty index has no record to fetch, so it cannot exercise a missing
		// record's load. Do not mistake that empty population for a conflict test.
		{name: "ScanIndexRecords", existingOnly: true, run: func(t *testing.T, store *FDBRecordStore, _ bool) {
			cursor := store.ScanIndexRecords("order_price", TupleRangeAll, nil, ForwardScan())
			defer func() { _ = cursor.Close() }()
			got, err := AsList(context.Background(), cursor)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] == nil {
				t.Fatalf("index scan returned %v, want one populated record", got)
			}
			assertRecord(t, got[0].Record, original)
		}},
	}

	for _, layout := range []struct {
		name   string
		split  bool
		legacy bool
	}{
		{name: "unsplit"},
		{name: "split-enabled", split: true},
		{name: "legacy-bare-key", legacy: true},
	} {
		builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
			SetSplitLongRecords(layout.split).SetStoreRecordVersions(false)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", NewIndex("order_price", Field("price")))
		md, err := builder.Build()
		if err != nil {
			t.Fatal(err)
		}
		for _, api := range apis {
			if api.modernOnly && layout.legacy {
				continue
			}
			for _, exists := range []bool{true, false} {
				if api.existingOnly && !exists {
					continue
				}
				for _, change := range []string{"first-chunk", "inline-version", "actual-key", "unrelated-99"} {
					t.Run(fmt.Sprintf("%s/%s/exists=%t/%s", layout.name, api.name, exists, change), func(t *testing.T) {
						root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
						storeBuilder := func(rc *FDBRecordContext) *StoreBuilder {
							b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
							if layout.legacy {
								b.SetFormatVersion(4)
							}
							return b
						}
						newContext := func() *FDBRecordContext {
							tx, err := db.CreateWritableTransaction()
							if err != nil {
								t.Fatal(err)
							}
							rc := NewFDBRecordContext(tx, nil)
							t.Cleanup(rc.Cancel)
							return rc
						}
						seed := newContext()
						seedStore, err := storeBuilder(seed).Create()
						if err != nil {
							t.Fatal(err)
						}
						if seedStore.omitUnsplitRecordSuffix() != layout.legacy || seedStore.metaData.IsStoreRecordVersions() {
							t.Fatal("store layout or disabled record versions do not match the fixture")
						}
						if exists {
							got, err := seedStore.SaveRecord(original)
							if err != nil {
								t.Fatal(err)
							}
							assertRecord(t, got, original)
						}
						actualKey := root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(0)})
						if layout.legacy {
							actualKey = root.Sub(RecordKey).Pack(primaryKey)
						}
						value, err := seed.Transaction().Get(actualKey).Get()
						if err != nil || (value != nil) != exists {
							t.Fatalf("seed physical record exists = %t, want %t (err=%v)", value != nil, exists, err)
						}
						if err := seed.Commit(); err != nil {
							t.Fatal(err)
						}

						reader := newContext()
						store, err := storeBuilder(reader).Open()
						if err != nil {
							t.Fatal(err)
						}
						api.run(t, store, exists)

						changedKey := actualKey
						wantConflict := true
						switch change {
						case "first-chunk":
							changedKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(1)})
							wantConflict = !layout.legacy
						case "inline-version":
							changedKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(-1)})
							wantConflict = !layout.legacy
						case "unrelated-99":
							changedKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(99), int64(0)})
							if layout.legacy {
								changedKey = root.Sub(RecordKey).Pack(tuple.Tuple{int64(99)})
							}
							wantConflict = false
						}
						if api.conflict != nil {
							wantConflict = api.conflict(exists, change)
						}
						writer := newContext()
						writer.Transaction().Set(changedKey, []byte("concurrent raw write"))
						if err := writer.Commit(); err != nil {
							t.Fatalf("concurrent writer commit: %v", err)
						}
						// Force even load-only and dry-run transactions to commit their
						// conflict ranges. Neither transaction is retried.
						reader.Transaction().Set(root.Sub("commit-sentinel").Pack(nil), []byte("read completed"))
						err = reader.Commit()
						if wantConflict {
							var conflict fdb.Error
							if !errors.As(err, &conflict) || conflict.Code != 1020 {
								t.Fatalf("reader commit = %v, want not_committed (1020)", err)
							}
						} else if err != nil {
							t.Fatalf("reader commit = %v, want no conflict", err)
						}
					})
				}
			}
		}
	}
}
