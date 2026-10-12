package recordlayer

import (
	"bytes"
	"fmt"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Metadata current/history reads and save's old-metadata read share the full
// record loader. They must protect absent suffixes just like ordinary records.
func TestFDB_MetadataLoadReadConflict(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	initial, err := loadParityMetaData(t, false, false).ToProto()
	if err != nil {
		t.Fatal(err)
	}
	initial.Version = proto.Int32(1)
	next := proto.Clone(initial).(*gen.MetaData)
	next.Version = proto.Int32(2)
	initialBytes, err := proto.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	nextBytes, err := proto.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"current", "history", "save"} {
		for _, exists := range []bool{false, true} {
			for _, change := range []string{"first-chunk", "inline-version", "actual-key", "unrelated-99"} {
				t.Run(fmt.Sprintf("%s/exists=%t/%s", api, exists, change), func(t *testing.T) {
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					mdSpace := root.Sub("metadata")
					store := NewFDBMetaDataStore(mdSpace)
					seed := loadParityContext(t, db)
					if exists {
						if err := store.SaveRecordMetaData(seed.Transaction(), initial); err != nil {
							t.Fatal(err)
						}
					}
					if api == "history" {
						// The missing-history case has current metadata but never had
						// version 1. The existing case really archives version 1.
						if err := store.SaveRecordMetaData(seed.Transaction(), next); err != nil {
							t.Fatal(err)
						}
					}
					assertLoadParityCommit(t, seed, false)
					reader := loadParityContext(t, db)
					key := tuple.Tuple{nil}
					var got *gen.MetaData
					var readErr error
					switch api {
					case "current":
						got, readErr = store.LoadRecordMetaDataProto(reader.Transaction())
					case "history":
						key = tuple.Tuple{"H", int64(1)}
						got, readErr = store.LoadRecordMetaDataProtoAtVersion(reader.Transaction(), 1)
					case "save":
						toSave, wantBytes := initial, initialBytes
						if exists {
							toSave, wantBytes = next, nextBytes
						}
						readErr = store.SaveRecordMetaData(reader.Transaction(), toSave)
						if readErr == nil {
							// Snapshot assertions prove the save ran without adding
							// read conflicts beyond the API being tested.
							value, err := reader.Transaction().Snapshot().Get(mdSpace.Pack(tuple.Tuple{nil, int64(0)})).Get()
							if err != nil || !bytes.Equal(value, wantBytes) {
								t.Fatalf("saved metadata does not match v%d (err=%v)", toSave.GetVersion(), err)
							}
							if exists {
								value, err := reader.Transaction().Snapshot().Get(mdSpace.Pack(tuple.Tuple{"H", int64(1), int64(0)})).Get()
								if err != nil || !bytes.Equal(value, initialBytes) {
									t.Fatalf("old metadata was not archived (err=%v)", err)
								}
							}
						}
					}
					if readErr != nil {
						t.Fatal(readErr)
					}
					if api != "save" {
						if exists && !proto.Equal(got, initial) {
							t.Fatalf("metadata = %v, want complete v1 proto", got)
						}
						if !exists && got != nil {
							t.Fatalf("missing metadata returned %v", got)
						}
					}
					var suffix int64
					conflict := true
					switch change {
					case "first-chunk":
						suffix = 1
					case "inline-version":
						suffix = -1
					case "unrelated-99":
						key = tuple.Tuple{int64(99)}
						conflict = false
					}
					writer := loadParityContext(t, db)
					writer.Transaction().Set(mdSpace.Sub(key...).Pack(tuple.Tuple{suffix}), []byte("concurrent metadata write"))
					assertLoadParityCommit(t, writer, false)
					reader.Transaction().Set(root.Sub("sentinel").Pack(nil), []byte("read completed"))
					assertLoadParityCommit(t, reader, conflict)
				})
			}
		}
	}
}
