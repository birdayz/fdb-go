package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	"google.golang.org/protobuf/proto"
)

func newLoadParityDatabase(t *testing.T) *FDBDatabase {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := foundationdbtc.Terminate(ctx, container); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(path, []byte(cluster), 0o600); err != nil {
		t.Fatal(err)
	}
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	return NewFDBDatabase(raw)
}

func loadParityContext(t *testing.T, db *FDBDatabase) *FDBRecordContext {
	t.Helper()
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	rc := NewFDBRecordContext(tx, nil)
	t.Cleanup(rc.Cancel)
	return rc
}

func loadParityMetaData(t *testing.T, split, versions bool) *RecordMetaData {
	t.Helper()
	b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
		SetSplitLongRecords(split).SetStoreRecordVersions(versions)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func assertLoadParityCommit(t *testing.T, rc *FDBRecordContext, conflict bool) {
	t.Helper()
	err := rc.Commit()
	if !conflict {
		if err != nil {
			t.Fatalf("commit = %v, want success", err)
		}
		return
	}
	var fdbErr fdb.Error
	if !errors.As(err, &fdbErr) || fdbErr.Code != 1020 {
		t.Fatalf("commit = %v, want not_committed (1020)", err)
	}
}

func loadParityRead(store *FDBRecordStore, async bool) (*FDBStoredRecord[proto.Message], error) {
	pk := tuple.Tuple{int64(1)}
	if async {
		return store.LoadRecordAsync(pk, nil).Get()
	}
	return store.LoadRecord(pk)
}

// Keep the process-wide allocation counter away from parallel tests. Issuance
// measurements use a real transaction's local write and the worker-free format-4
// layout, isolating the lifetime state added by each pending read wrapper.
func TestFDB_LoadRecordAsyncAllocations(t *testing.T) {
	const childEnv = "FDB_RECORD_LOAD_ALLOCATION_CHILD"
	if os.Getenv(childEnv) != "1" {
		t.Parallel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestFDB_LoadRecordAsyncAllocations$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(output, []byte("allocations per issue:")) {
			t.Fatalf("isolated allocation test: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	db := newLoadParityDatabase(t)
	ctx := loadParityContext(t, db)
	root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	store := legacyVersionStore(t, ctx, root, loadParityMetaData(t, false, false), 4, true)
	entry := &IndexEntry{
		Index: NewIndex("Order$order_id", Field("order_id")),
		Key:   tuple.Tuple{int64(1), int64(1)},
	}
	// Warm the lazy primary-key extraction outside the issuance measurements.
	pk := entry.PrimaryKey()
	if !bytes.Equal(pk.Pack(), (tuple.Tuple{int64(1)}).Pack()) {
		t.Fatalf("allocation entry primary key = %v, want [1]", pk)
	}
	scanState := NewScanLimiterState()
	order := &demopb.Order{OrderId: proto.Int64(1)}
	data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: order})
	if err != nil {
		t.Fatal(err)
	}
	recordKey, _ := legacyVersionKeys(root, 4)
	ctx.Transaction().Set(recordKey, data)
	var first pendingStoreRecordLoad
	issued := testing.AllocsPerRun(1000, func() {
		first = store.startRecordLoad(ctx.Transaction(), pk)
	})
	if first.raw.value == nil || first.raw.rangeRead != nil || first.raw.asyncRange != nil || !first.raw.ready() {
		t.Fatal("allocation control did not use the ready bare-key path")
	}
	var sizes sizeInfo
	value, _, err := first.raw.finish(&sizes)
	if err != nil || !bytes.Equal(value, data) || sizes.KeySize != len(recordKey) || sizes.ValueSize != len(data) {
		t.Fatalf("allocation control read = %x, sizes=%+v, error=%v", value, sizes, err)
	}
	var pending PendingRead[*FDBStoredRecord[proto.Message]]
	public := testing.AllocsPerRun(1000, func() {
		pending = store.LoadRecordAsync(pk, scanState)
	})
	if !pending.IsReady() {
		t.Fatal("public allocation sample was not immediately ready")
	}
	got, err := pending.Get()
	if err != nil || got == nil || !proto.Equal(got.Record, order) || !bytes.Equal(got.PrimaryKey.Pack(), pk.Pack()) {
		t.Fatalf("public allocation sample = %v, error=%v", got, err)
	}
	t.Logf("allocations per issue: public=%.0f raw=%.0f additional=%.0f", public, issued, public-issued)
	// One escaping recordLoad owns the PendingRead state. An extra pendingFunc
	// and its two bound method closures would add three more allocations.
	if issued <= 0 || public <= 0 || public-issued != 1 {
		t.Fatalf("LoadRecordAsync adds %.0f allocations to %.0f raw-read allocations, want 1", public-issued, issued)
	}

	loader := &indexRecordLoader{store: store, scanState: scanState}
	var indexedPending PendingRead[*FDBIndexedRecord]
	indexed := testing.AllocsPerRun(1000, func() {
		indexedPending = loader.load(entry)
	})
	if !indexedPending.IsReady() {
		t.Fatal("index allocation sample was not immediately ready")
	}
	indexedRecord, err := indexedPending.Get()
	if err != nil || indexedRecord == nil || indexedRecord.IndexEntry != entry || indexedRecord.Record == nil ||
		!proto.Equal(indexedRecord.Record.Record, order) || !bytes.Equal(indexedRecord.Record.PrimaryKey.Pack(), pk.Pack()) {
		t.Fatalf("index allocation sample = %v, error=%v", indexedRecord, err)
	}
	t.Logf("allocations per index issue: indexed=%.0f public=%.0f additional=%.0f", indexed, public, indexed-public)
	if indexed-public != 1 {
		t.Fatalf("indexRecordLoader.load adds %.0f allocations to %.0f public-load allocations, want 1", indexed-public, public)
	}
}

func TestFDB_RecordLoadParity(t *testing.T) {
	t.Parallel()
	db := newLoadParityDatabase(t)
	t.Run("range-boundaries", func(t *testing.T) { testLoadParityBoundaries(t, db) })
	t.Run("inline-versions", func(t *testing.T) { testLoadParityVersions(t, db) })
	t.Run("corruption", func(t *testing.T) { testLoadParityCorruption(t, db) })
	t.Run("stored-incomplete-version", func(t *testing.T) {
		root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
		md := loadParityMetaData(t, false, false)
		seed := loadParityContext(t, db)
		store, err := NewStoreBuilder().SetContext(seed).SetMetaDataProvider(md).SetSubspace(root).Create()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1)}); err != nil {
			t.Fatal(err)
		}
		packed, err := (tuple.Tuple{tuple.IncompleteVersionstamp(7)}).PackWithVersionstamp(nil)
		if err != nil {
			t.Fatal(err)
		}
		// Persist the raw 13-byte tuple, without the mutation-only offset.
		seed.Transaction().Set(root.Sub(RecordKey).Pack(tuple.Tuple{int64(1), int64(-1)}), packed[:len(packed)-4])
		assertLoadParityCommit(t, seed, false)
		for _, async := range []bool{false, true} {
			reader := loadParityContext(t, db)
			store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				t.Fatal(err)
			}
			got, err := loadParityRead(store, async)
			if err != nil || got == nil || got.Version == nil || got.Version.IsComplete() || got.Version.GetLocalVersion() != 7 || got.KeyCount != 2 {
				t.Fatalf("raw incomplete version load = %v (err=%v, async=%t)", got, err, async)
			}
		}
	})
}

func testLoadParityBoundaries(t *testing.T, db *FDBDatabase) {
	for _, split := range []bool{false, true} {
		md := loadParityMetaData(t, split, false)
		for _, exists := range []bool{false, true} {
			for _, boundary := range []struct {
				name     string
				suffix   []byte
				conflict bool
			}{
				{name: "bare-key"},
				{name: "inclusive-00", suffix: []byte{0x00}, conflict: true},
				{name: "exclusive-ff", suffix: []byte{0xff}},
				{name: "beyond-ff", suffix: []byte{0xff, 0x00}},
			} {
				t.Run(fmt.Sprintf("split=%t/exists=%t/%s", split, exists, boundary.name), func(t *testing.T) {
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					seed := loadParityContext(t, db)
					store, err := NewStoreBuilder().SetContext(seed).SetMetaDataProvider(md).SetSubspace(root).Create()
					if err != nil {
						t.Fatal(err)
					}
					order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
					if exists {
						if _, err := store.SaveRecord(order); err != nil {
							t.Fatal(err)
						}
					}
					assertLoadParityCommit(t, seed, false)
					reader := loadParityContext(t, db)
					store, err = NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(root).Open()
					if err != nil {
						t.Fatal(err)
					}
					got, err := store.LoadRecord(tuple.Tuple{int64(1)})
					if err != nil || (got != nil) != exists {
						t.Fatalf("load = %v, exists=%t (err=%v)", got, exists, err)
					}
					if exists && !proto.Equal(got.Record, order) {
						t.Fatalf("load = %v, want %v", got.Record, order)
					}
					// The raw boundary bytes need not be valid tuple suffixes. They are
					// written after the load, so this tests range bounds, not parsing.
					key := append(root.Sub(RecordKey).Pack(tuple.Tuple{int64(1)}), boundary.suffix...)
					writer := loadParityContext(t, db)
					writer.Transaction().Set(key, []byte("concurrent"))
					assertLoadParityCommit(t, writer, false)
					reader.Transaction().Set(root.Sub("sentinel").Pack(nil), []byte("loaded"))
					assertLoadParityCommit(t, reader, boundary.conflict)
				})
			}
		}
	}
}

func testLoadParityVersions(t *testing.T, db *FDBDatabase) {
	versionstamp := tuple.Versionstamp{TransactionVersion: [10]byte{0, 0, 0, 0, 0, 0, 0, 42, 0, 1}, UserVersion: 7}
	packedVersion := tuple.Tuple{versionstamp}.Pack()
	if len(packedVersion) != 13 {
		t.Fatalf("version fixture is %d bytes, want 13", len(packedVersion))
	}
	complete, err := NewCompleteVersion(versionstamp.TransactionVersion[:], 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, splitEnabled := range []bool{false, true} {
		for _, storeVersions := range []bool{false, true} {
			md := loadParityMetaData(t, splitEnabled, storeVersions)
			for _, splitRecord := range []bool{false, true} {
				for _, local := range []bool{false, true} {
					for _, async := range []bool{false, true} {
						t.Run(fmt.Sprintf("split-enabled=%t/versions-enabled=%t/split-record=%t/local=%t/async=%t", splitEnabled, storeVersions, splitRecord, local, async), func(t *testing.T) {
							root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
							seed := loadParityContext(t, db)
							if _, err := NewStoreBuilder().SetContext(seed).SetMetaDataProvider(md).SetSubspace(root).Create(); err != nil {
								t.Fatal(err)
							}
							order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
							if splitRecord {
								order.Tags = []string{strings.Repeat("x", 100_001)}
							}
							data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: order})
							if err != nil {
								t.Fatal(err)
							}
							rs := root.Sub(RecordKey)
							versionKey := rs.Pack(tuple.Tuple{int64(1), int64(-1)})
							wantKeys, wantKeyBytes := 2, len(versionKey)
							if splitRecord {
								first := rs.Pack(tuple.Tuple{int64(1), int64(1)})
								second := rs.Pack(tuple.Tuple{int64(1), int64(2)})
								seed.Transaction().Set(first, data[:100_000])
								seed.Transaction().Set(second, data[100_000:])
								wantKeys = 3
								wantKeyBytes += len(first) + len(second)
							} else {
								key := rs.Pack(tuple.Tuple{int64(1), int64(0)})
								seed.Transaction().Set(key, data)
								wantKeyBytes += len(key)
							}
							if !local {
								seed.Transaction().Set(versionKey, packedVersion)
							}
							assertLoadParityCommit(t, seed, false)
							reader := loadParityContext(t, db)
							store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(root).Open()
							if err != nil {
								t.Fatal(err)
							}
							wantVersion := complete
							if local {
								reader.AddToLocalVersionCache(versionKey, 7)
								wantVersion, err = IncompleteVersion(7)
								if err != nil {
									t.Fatal(err)
								}
							}
							got, err := loadParityRead(store, async)
							if err != nil || got == nil {
								t.Fatalf("load returned %v (err=%v)", got, err)
							}
							if !proto.Equal(got.Record, order) {
								t.Fatal("loaded record differs from the seeded Order")
							}
							if !got.Version.Equal(wantVersion) {
								t.Fatalf("version = %v, want %v", got.Version, wantVersion)
							}
							if got.KeyCount != wantKeys || got.KeySize != wantKeyBytes || got.ValueSize != len(data)+13 || got.Split != splitRecord {
								t.Fatalf("sizes = (%d,%d,%d,%t), want (%d,%d,%d,%t)", got.KeyCount, got.KeySize, got.ValueSize, got.Split, wantKeys, wantKeyBytes, len(data)+13, splitRecord)
							}
						})
					}
				}
			}
		}
	}
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-transaction-save/split=%t", split), func(t *testing.T) {
			root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			rc := loadParityContext(t, db)
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(loadParityMetaData(t, split, true)).SetSubspace(root).Create()
			if err != nil {
				t.Fatal(err)
			}
			order := &demopb.Order{OrderId: proto.Int64(1)}
			if split {
				order.Tags = []string{strings.Repeat("x", 100_001)}
			}
			if _, err := store.SaveRecord(order); err != nil {
				t.Fatal(err)
			}
			data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: order})
			if err != nil {
				t.Fatal(err)
			}
			rs := root.Sub(RecordKey)
			versionKey := rs.Pack(tuple.Tuple{int64(1), int64(-1)})
			physical, err := rc.Transaction().Snapshot().Get(versionKey).Get()
			if err != nil || physical != nil {
				t.Fatalf("pending version unexpectedly materialized: %x (err=%v)", physical, err)
			}
			got, err := store.LoadRecord(tuple.Tuple{int64(1)})
			if err != nil || got == nil || !proto.Equal(got.Record, order) {
				t.Fatalf("same-transaction load = %v (err=%v)", got, err)
			}
			if got.Version == nil || got.Version.IsComplete() || got.Version.GetLocalVersion() != 0 {
				t.Fatalf("version = %v, want local incomplete version 0", got.Version)
			}
			wantKeys, wantKeyBytes := 2, len(versionKey)+len(rs.Pack(tuple.Tuple{int64(1), int64(0)}))
			if split {
				wantKeys = 3
				wantKeyBytes = len(versionKey) + len(rs.Pack(tuple.Tuple{int64(1), int64(1)})) + len(rs.Pack(tuple.Tuple{int64(1), int64(2)}))
			}
			if got.KeyCount != wantKeys || got.KeySize != wantKeyBytes || got.ValueSize != len(data)+13 || got.Split != split {
				t.Fatalf("sizes = (%d,%d,%d,%t), want (%d,%d,%d,%t)", got.KeyCount, got.KeySize, got.ValueSize, got.Split, wantKeys, wantKeyBytes, len(data)+13, split)
			}
		})
	}
}

func testLoadParityCorruption(t *testing.T, db *FDBDatabase) {
	order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	data, err := proto.Marshal(&demopb.UnionDescriptor{XOrder: order})
	if err != nil {
		t.Fatal(err)
	}
	version := tuple.Tuple{tuple.Versionstamp{TransactionVersion: [10]byte{1}, UserVersion: 7}}.Pack()
	type row struct {
		suffix tuple.Tuple
		value  []byte
	}
	for _, split := range []bool{false, true} {
		md := loadParityMetaData(t, split, false)
		for _, async := range []bool{false, true} {
			for _, tc := range []struct {
				name       string
				rows       []row
				badPacked  bool
				localOnly  bool
				errorKind  string
				next       int64
				wantSplit  bool
				wantInline bool
			}{
				{name: "unsplit", rows: []row{{tuple.Tuple{int64(0)}, data}}},
				{name: "split", rows: []row{{tuple.Tuple{int64(1)}, data[:3]}, {tuple.Tuple{int64(2)}, data[3:]}}, wantSplit: true},
				{name: "version-unsplit", rows: []row{{tuple.Tuple{int64(-1)}, version}, {tuple.Tuple{int64(0)}, data}}, wantInline: true},
				{name: "version-split", rows: []row{{tuple.Tuple{int64(-1)}, version}, {tuple.Tuple{int64(1)}, data[:3]}, {tuple.Tuple{int64(2)}, data[3:]}}, wantSplit: true, wantInline: true},
				{name: "missing-first", rows: []row{{tuple.Tuple{int64(2)}, data}}, errorKind: "without-start", next: 2},
				{name: "gap", rows: []row{{tuple.Tuple{int64(1)}, data[:3]}, {tuple.Tuple{int64(3)}, data[3:]}}, errorKind: "out-of-order", next: 3},
				{name: "version-only", rows: []row{{tuple.Tuple{int64(-1)}, version}}, errorKind: "without-start", next: -1},
				{name: "local-version-only", localOnly: true, errorKind: "without-start", next: -1},
				{name: "unsplit-then-split", rows: []row{{tuple.Tuple{int64(0)}, data}, {tuple.Tuple{int64(1)}, data}}, errorKind: "core"},
				{name: "extra-extension", rows: []row{{tuple.Tuple{int64(0), int64(1)}, data}}, errorKind: "core"},
				{name: "nonnumeric-extension", rows: []row{{tuple.Tuple{"bad"}, data}}, errorKind: "argument"},
				{name: "malformed-extension", badPacked: true, errorKind: "argument"},
				{name: "invalid-version", rows: []row{{tuple.Tuple{int64(-1)}, []byte("not a version")}, {tuple.Tuple{int64(0)}, data}}, errorKind: "version"},
				{name: "invalid-negative-suffix", rows: []row{{tuple.Tuple{int64(-2)}, data}}, errorKind: "without-start", next: -2},
			} {
				t.Run(fmt.Sprintf("split=%t/async=%t/%s", split, async, tc.name), func(t *testing.T) {
					root := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
					seed := loadParityContext(t, db)
					if _, err := NewStoreBuilder().SetContext(seed).SetMetaDataProvider(md).SetSubspace(root).Create(); err != nil {
						t.Fatal(err)
					}
					pkSpace := root.Sub(RecordKey).Sub(int64(1))
					wantKeyBytes, wantValueBytes := 0, 0
					for _, row := range tc.rows {
						key := pkSpace.Pack(row.suffix)
						seed.Transaction().Set(key, row.value)
						wantKeyBytes += len(key)
						wantValueBytes += len(row.value)
					}
					if tc.badPacked {
						seed.Transaction().SetBytes(append(pkSpace.Bytes(), 0xfe), data)
					}
					assertLoadParityCommit(t, seed, false)
					reader := loadParityContext(t, db)
					store, err := NewStoreBuilder().SetContext(reader).SetMetaDataProvider(md).SetSubspace(root).Open()
					if err != nil {
						t.Fatal(err)
					}
					if tc.localOnly {
						reader.AddToLocalVersionCache(pkSpace.Pack(tuple.Tuple{int64(-1)}), 7)
					}
					got, err := loadParityRead(store, async)
					if tc.errorKind == "" {
						if err != nil || got == nil || !proto.Equal(got.Record, order) {
							t.Fatalf("valid record load = %v (err=%v)", got, err)
						}
						if !bytes.Equal(got.PrimaryKey.Pack(), tuple.Tuple{int64(1)}.Pack()) || got.Split != tc.wantSplit || (got.Version != nil) != tc.wantInline {
							t.Fatalf("wrong primary key, split, or version: %+v", got)
						}
						if got.KeyCount != len(tc.rows) || got.KeySize != wantKeyBytes || got.ValueSize != wantValueBytes {
							t.Fatalf("sizes = (%d,%d,%d), want (%d,%d,%d)", got.KeyCount, got.KeySize, got.ValueSize, len(tc.rows), wantKeyBytes, wantValueBytes)
						}
						return
					}
					if err == nil || got != nil {
						t.Fatalf("corrupt record returned %v (err=%v), want error and nil record", got, err)
					}
					switch tc.errorKind {
					case "without-start":
						var e *FoundSplitWithoutStartError
						if !errors.As(err, &e) || e.NextIndex != tc.next || e.Reverse || !bytes.Equal(e.KeyTuple.Pack(), tuple.Tuple{int64(1)}.Pack()) {
							t.Fatalf("error = %v, want forward without-start at %d", err, tc.next)
						}
					case "out-of-order":
						var e *FoundSplitOutOfOrderError
						if !errors.As(err, &e) || e.Expected != 2 || e.Found != tc.next || !bytes.Equal(e.KeyTuple.Pack(), tuple.Tuple{int64(1)}.Pack()) {
							t.Fatalf("error = %v, want out-of-order expected=2 found=%d", err, tc.next)
						}
					case "core":
						var e *RecordCoreError
						if !errors.As(err, &e) {
							t.Fatalf("error = %T %v, want RecordCoreError", err, err)
						}
					case "argument":
						var e *RecordCoreArgumentError
						if !errors.As(err, &e) {
							t.Fatalf("error = %T %v, want RecordCoreArgumentError", err, err)
						}
					case "version":
						if !strings.Contains(err.Error(), "version") {
							t.Fatalf("wrong error for invalid version: %v", err)
						}
					}
				})
			}
		}
	}
}
