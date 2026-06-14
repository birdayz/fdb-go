//go:build cgo

package libfdbc_test

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/directory"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/libfdbc"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// This is RFC-109's gold gate: the libfdb_c escape hatch (pkg/fdbgo/libfdbc) and
// the from-scratch pure-Go client MUST read/write byte-identical records, index
// entries, and split records against the SAME cluster — that wire compatibility
// is the whole point (a Go app may flip the backend and still share data with Java
// or C apps, and with its own prior writes).
//
// Two proofs run against one real FDB (no mocks):
//
//   - cross_backend_roundtrip: save through one backend, read through the OTHER on
//     the SAME subspace, and assert the records come back equal. This is the actual
//     operator scenario ("flip the flag; existing data still reads"). It is the
//     strongest, header-agnostic proof — if the bytes were not compatible, the
//     reader's record/version/split decoders would choke or mismatch.
//   - wire_bytes_identical: save the SAME records through each backend on DISJOINT
//     subspaces, then byte-compare the record and index keyspaces (relative to each
//     subspace) read back through a single neutral reader. Identical bytes prove the
//     cgo backend writes exactly what the pure-Go client writes — record split
//     points, index entry layout, inline record versions, all of it.
//
// The test is its own (non-Ginkgo) container so the cgo build tag keeps it out of
// the pure-Go suite; it always calls t.Parallel and uses unique subspaces.

func TestLibFDBC_RecordLayerDifferential(t *testing.T) {
	t.Parallel()

	clusterFile := startCluster(t)

	// Both clients use API 730 (the cgofdb binding's header version, matching the
	// 7.3.75 server). The two API-version registrations are independent in-process
	// bookkeeping; only the cgo backend touches the libfdb_c network thread.
	fdb.MustAPIVersion(730)
	goRaw, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open pure-Go database: %v", err)
	}
	defer goRaw.Close()

	cgoBackend, err := libfdbc.Open(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c backend: %v", err)
	}
	defer cgoBackend.Close()

	goDB := recordlayer.NewFDBDatabase(goRaw)
	cgoDB := recordlayer.NewFDBDatabaseWithBackend(cgoBackend)

	md := orderMetaData(t)
	orders := []*gen.Order{
		makeOrder(1, 100, "Rose"),
		makeOrder(2, 250, "Tulip"),
		makeOrder(3, 50, "Daisy"),
		makeOrder(42, 999, "Orchid"),
	}

	t.Run("cross_backend_roundtrip", func(t *testing.T) {
		// Write with cgo, read with pure-Go on the same subspace.
		ss := uniqueSubspace("xback_cgo_to_go")
		saveOrders(t, cgoDB, md, ss, orders)
		for _, want := range orders {
			got := loadOrder(t, goDB, md, ss, tuple.Tuple{want.GetOrderId()})
			if !proto.Equal(got, want) {
				t.Fatalf("cgo-write/go-read mismatch for order %d:\n got=%v\nwant=%v", want.GetOrderId(), got, want)
			}
		}

		// Write with pure-Go, read with cgo on the same subspace (the reverse flip).
		ss2 := uniqueSubspace("xback_go_to_cgo")
		saveOrders(t, goDB, md, ss2, orders)
		for _, want := range orders {
			got := loadOrder(t, cgoDB, md, ss2, tuple.Tuple{want.GetOrderId()})
			if !proto.Equal(got, want) {
				t.Fatalf("go-write/cgo-read mismatch for order %d:\n got=%v\nwant=%v", want.GetOrderId(), got, want)
			}
		}
	})

	t.Run("wire_bytes_identical", func(t *testing.T) {
		ssGo := uniqueSubspace("wire_go")
		ssCgo := uniqueSubspace("wire_cgo")
		saveOrders(t, goDB, md, ssGo, orders)
		saveOrders(t, cgoDB, md, ssCgo, orders)

		// Record subspace (RecordKey=1) and index subspace (IndexKey=2) are the
		// wire-critical bytes; compare each, relative to its store subspace, read
		// through one neutral reader (the pure-Go client).
		for _, part := range []struct {
			name string
			key  int
		}{{"records", recordlayer.RecordKey}, {"indexes", recordlayer.IndexKey}} {
			goKVs := readSubspaceRelative(t, goRaw, ssGo.Sub(int64(part.key)))
			cgoKVs := readSubspaceRelative(t, goRaw, ssCgo.Sub(int64(part.key)))
			assertSameKeyspace(t, part.name, goKVs, cgoKVs)
		}
	})

	t.Run("split_record_wire_compat", func(t *testing.T) {
		// A record > 100KB is split across keys (suffixes 1+). Prove the cgo backend
		// writes the same split layout: save a big record with cgo, read it back with
		// pure-Go, and byte-compare its record subspace against the pure-Go-written one.
		big := makeOrder(7, 7, string(bigBlob(250_000)))
		ssGo := uniqueSubspace("split_go")
		ssCgo := uniqueSubspace("split_cgo")
		saveOrders(t, goDB, md, ssGo, []*gen.Order{big})
		saveOrders(t, cgoDB, md, ssCgo, []*gen.Order{big})

		got := loadOrder(t, goDB, md, ssCgo, tuple.Tuple{int64(7)})
		if !proto.Equal(got, big) {
			t.Fatalf("split record cgo-write/go-read mismatch (len got=%d want=%d)",
				len(got.GetFlower().GetType()), len(big.GetFlower().GetType()))
		}
		goKVs := readSubspaceRelative(t, goRaw, ssGo.Sub(int64(recordlayer.RecordKey)))
		cgoKVs := readSubspaceRelative(t, goRaw, ssCgo.Sub(int64(recordlayer.RecordKey)))
		if len(goKVs) < 2 {
			t.Fatalf("expected a split record (>=2 chunks), got %d keys — test data too small", len(goKVs))
		}
		assertSameKeyspace(t, "split-records", goKVs, cgoKVs)
	})

	// The record-store subtests above drive the WritableTransaction adapter through
	// the record layer; these exercise the wire-critical primitives the record path
	// may not hit, directly on the raw fdb.WritableTransaction (FDB C++ reviewer's
	// requested follow-ups): atomic mutations, conflict ranges, versionstamps, and
	// snapshot reads.

	t.Run("raw_atomic_and_conflict_range_wire_compat", func(t *testing.T) {
		// Apply the same atomic-mutation + plain-set program through each backend's
		// WritableTransaction over disjoint prefixes; the resulting bytes must be
		// identical (atomic ADD/MAX/MIN/BYTE_MAX are little-endian/byte-wise ops the
		// cluster performs — the client just forwards the opcode+operand). Conflict
		// ranges are smoke-tested: they must round-trip through the adapter (libfdb_c
		// returns an error on a bad range; nil here proves the forward is correct).
		apply := func(db fdb.BackendDatabase, p string) {
			t.Helper()
			_, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
				tr.Set(fdb.Key(p+"set"), []byte("hello"))
				tr.Add(fdb.Key(p+"cnt"), le64(5))
				tr.Add(fdb.Key(p+"cnt"), le64(3)) // → 8
				tr.Max(fdb.Key(p+"max"), le64(10))
				tr.Max(fdb.Key(p+"max"), le64(7)) // stays 10
				tr.Min(fdb.Key(p+"min"), le64(4))
				tr.ByteMax(fdb.Key(p+"bmax"), []byte("aaa"))
				tr.ByteMax(fdb.Key(p+"bmax"), []byte("zzz")) // → "zzz"
				if err := tr.AddReadConflictRange(fdb.KeyRange{Begin: fdb.Key(p), End: fdb.Key(p + "\xff")}); err != nil {
					return nil, err
				}
				if err := tr.AddWriteConflictKey(fdb.Key(p + "set")); err != nil {
					return nil, err
				}
				return nil, nil
			})
			if err != nil {
				t.Fatalf("apply(%q): %v", p, err)
			}
		}
		apply(goRaw, "libfdbc_diff/atom_go/")
		apply(cgoBackend, "libfdbc_diff/atom_cgo/")
		goKVs := readSubspaceRelative(t, goRaw, subspace.FromBytes([]byte("libfdbc_diff/atom_go/")))
		cgoKVs := readSubspaceRelative(t, goRaw, subspace.FromBytes([]byte("libfdbc_diff/atom_cgo/")))
		assertSameKeyspace(t, "raw-atomic", goKVs, cgoKVs)
	})

	t.Run("versionstamp_value_roundtrip", func(t *testing.T) {
		// SetVersionstampedValue + GetVersionstamp: the 10-byte stamp is assigned by
		// the cluster at commit (differs per txn), so we assert STRUCTURE — the value
		// read back equals exactly the committed stamp GetVersionstamp returns. Run
		// both directions (write cgo/read pure-Go and vice versa) to prove the opcode,
		// the trailing 4-byte LE position suffix, and the post-commit stamp all agree.
		check := func(writer, reader fdb.BackendDatabase, key fdb.Key) {
			t.Helper()
			var stampFut fdb.FutureKey
			_, err := writer.Transact(func(tr fdb.WritableTransaction) (any, error) {
				param := make([]byte, 14) // 10-byte stamp placeholder + 4-byte LE offset=0
				tr.SetVersionstampedValue(key, param)
				stampFut = tr.GetVersionstamp()
				return nil, nil
			})
			if err != nil {
				t.Fatalf("versionstamp write: %v", err)
			}
			stamp, err := stampFut.Get()
			if err != nil {
				t.Fatalf("GetVersionstamp: %v", err)
			}
			if len(stamp) != 10 {
				t.Fatalf("versionstamp len = %d, want 10", len(stamp))
			}
			val := readKeyVia(t, reader, key)
			if string(val) != string(stamp) {
				t.Fatalf("versionstamp value mismatch: read=%x want(stamp)=%x", val, stamp)
			}
		}
		check(cgoBackend, goRaw, fdb.Key("libfdbc_diff/vs_cgo"))
		check(goRaw, cgoBackend, fdb.Key("libfdbc_diff/vs_go"))
	})

	t.Run("snapshot_read", func(t *testing.T) {
		// Exercise reader.Snapshot() on the cgo backend: a snapshot read of a committed
		// key returns its value (the adapter forwards a snapshot, not a serializable read).
		key := fdb.Key("libfdbc_diff/snap_k")
		if _, err := cgoBackend.Transact(func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(key, []byte("snapval"))
			return nil, nil
		}); err != nil {
			t.Fatalf("snapshot seed: %v", err)
		}
		got, err := cgoBackend.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
			return rtx.Snapshot().Get(key).Get()
		})
		if err != nil {
			t.Fatalf("snapshot read: %v", err)
		}
		if v, _ := got.([]byte); string(v) != "snapval" {
			t.Fatalf("snapshot read = %q, want %q", v, "snapval")
		}
	})

	// The following pin the three codex-review P2 findings — capability detection in
	// the backend constructor, ctx honoring on the cgo backend, and the directory
	// layer not panicking on a non-pure-Go transactor.

	t.Run("pure_go_backend_keeps_direct_paths", func(t *testing.T) {
		// NewFDBDatabaseWithBackend on the pure-Go backend (what OpenDatabaseWithBackend
		// (BackendGo, …) returns) must KEEP CreateTransaction — the constructor detects
		// the concrete fdb.Database and populates its db slot. (codex P2 #1)
		rlGo := recordlayer.NewFDBDatabaseWithBackend(goRaw)
		tx, err := rlGo.CreateTransaction()
		if err != nil {
			t.Fatalf("pure-Go backend must support CreateTransaction, got %v", err)
		}
		tx.Cancel()

		// The cgo backend genuinely lacks it → fail-fast BackendCapabilityError, not nil-panic.
		if _, err := cgoDB.CreateTransaction(); err == nil {
			t.Fatal("cgo backend CreateTransaction must fail, got nil")
		} else {
			var be *recordlayer.BackendCapabilityError
			if !errors.As(err, &be) {
				t.Fatalf("cgo backend CreateTransaction must return *BackendCapabilityError, got %v", err)
			}
		}
	})

	t.Run("cgo_backend_honors_canceled_ctx", func(t *testing.T) {
		// A canceled ctx must abort BEFORE the callback runs/commits on the cgo backend
		// (it implements CtxTransactor now), matching the pure-Go backend. (codex P2 #2)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		_, err := cgoDB.Run(ctx, func(*recordlayer.FDBRecordContext) (any, error) {
			called = true
			return nil, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cgo Run with a canceled ctx must return context.Canceled, got %v", err)
		}
		if called {
			t.Fatal("cgo Run must NOT execute the callback when ctx is already canceled")
		}
	})

	t.Run("directory_layer_rejects_cgo_backend", func(t *testing.T) {
		// Directory writes need concrete pure-Go transaction features (out of escape-hatch
		// scope); with the cgo backend they must return UnsupportedBackendError, NOT panic
		// on the concrete-type assertion. (codex P2 #3)
		_, err := directory.CreateOrOpen(cgoBackend, []string{"libfdbc_diff_dir_cgo"}, nil)
		var ue *directory.UnsupportedBackendError
		if !errors.As(err, &ue) {
			t.Fatalf("directory.CreateOrOpen on cgo backend must return *UnsupportedBackendError, got %v", err)
		}
		// Still works on the pure-Go backend.
		if _, err := directory.CreateOrOpen(goRaw, []string{"libfdbc_diff_dir_go"}, nil); err != nil {
			t.Fatalf("directory.CreateOrOpen on pure-Go backend must succeed, got %v", err)
		}
	})

	t.Run("cgo_backend_keeps_committed_result_on_late_cancel", func(t *testing.T) {
		// A ctx canceled AFTER the write is queued (so the commit still proceeds and
		// succeeds) must NOT be reported as a failure — reporting a ctx error for a
		// committed transaction would be a lie that invites a double-write retry. The
		// ctx cause is surfaced only when the transaction actually failed.
		ct, ok := cgoBackend.(fdb.CtxTransactor)
		if !ok {
			t.Fatal("cgo backend must implement fdb.CtxTransactor")
		}
		ctx, cancel := context.WithCancel(context.Background())
		key := fdb.Key("libfdbc_diff/late_cancel")
		res, err := ct.TransactCtx(ctx, func(tr fdb.WritableTransaction) (any, error) {
			tr.Set(key, []byte("committed"))
			cancel() // cancel after the write is queued; the commit still goes through
			return "ok", nil
		})
		if err != nil {
			t.Fatalf("a committed tx must not be reported failed on a late cancel, got %v", err)
		}
		if res != "ok" {
			t.Fatalf("lost the committed result: %v", res)
		}
		if got := readKeyVia(t, goRaw, key); string(got) != "committed" {
			t.Fatalf("write must have committed despite the late cancel, got %q", got)
		}
	})
}

// ---- helpers ----

func startCluster(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// WithDirectIP gives the container a direct bridge IP whose advertised
	// public_address matches what clients dial — required for the libfdb_c client,
	// whose FlowTransport asserts canonicalRemotePort == peerAddress.port (the
	// pure-Go client tolerates port-mapping, libfdb_c does not). ClusterFile then
	// returns that internal bridge-IP file, usable by BOTH clients.
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithDirectIP())
	if err != nil {
		t.Skipf("FDB not available (no Docker): %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = container.Terminate(stopCtx)
	})
	content, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "fdb_cluster_*.txt")
	if err != nil {
		t.Fatalf("temp cluster file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write cluster file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close cluster file: %v", err)
	}
	return f.Name()
}

func orderMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.SetSplitLongRecords(true) // exercise the split-record wire format (>100KB → multiple keys)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("price_idx", recordlayer.Field("price")))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

func makeOrder(id int64, price int32, flowerType string) *gen.Order {
	return &gen.Order{
		OrderId: proto.Int64(id),
		Price:   proto.Int32(price),
		Flower:  &gen.Flower{Type: proto.String(flowerType), Color: gen.Color_RED.Enum()},
	}
}

func bigBlob(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('A' + (i % 26))
	}
	return b
}

func uniqueSubspace(name string) subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{"libfdbc_diff", name}.Pack())
}

func saveOrders(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, ss subspace.Subspace, orders []*gen.Order) {
	t.Helper()
	ctx := context.Background()
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for _, o := range orders {
			if _, err := store.SaveRecord(o); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("saveOrders: %v", err)
	}
}

func loadOrder(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, ss subspace.Subspace, pk tuple.Tuple) *gen.Order {
	t.Helper()
	ctx := context.Background()
	got, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			Open()
		if err != nil {
			return nil, err
		}
		rec, err := store.LoadRecord(pk)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, nil
		}
		return rec.Record, nil
	})
	if err != nil {
		t.Fatalf("loadOrder %v: %v", pk, err)
	}
	if got == nil {
		t.Fatalf("loadOrder %v: record not found", pk)
	}
	return got.(proto.Message).(*gen.Order)
}

// readSubspaceRelative reads every KV under sub through the neutral pure-Go reader,
// keyed by the suffix after the subspace prefix (so two stores on different
// subspaces are directly comparable).
func readSubspaceRelative(t *testing.T, raw fdb.Database, sub subspace.Subspace) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_, err := raw.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		kvs, err := rtx.GetRange(sub, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		prefix := sub.Bytes()
		for _, kv := range kvs {
			rel := string([]byte(kv.Key)[len(prefix):])
			out[rel] = append([]byte(nil), kv.Value...)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("readSubspaceRelative: %v", err)
	}
	return out
}

// le64 is an 8-byte little-endian operand for FDB atomic ADD/MAX/MIN.
func le64(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

func readKeyVia(t *testing.T, db fdb.BackendDatabase, key fdb.Key) []byte {
	t.Helper()
	v, err := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.Get(key).Get()
	})
	if err != nil {
		t.Fatalf("readKeyVia %x: %v", []byte(key), err)
	}
	if v == nil {
		return nil
	}
	return v.([]byte)
}

func assertSameKeyspace(t *testing.T, name string, a, b map[string][]byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: key count differs: pure-Go=%d cgo=%d", name, len(a), len(b))
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			t.Fatalf("%s: key %x present in pure-Go but missing in cgo", name, k)
		}
		if string(av) != string(bv) {
			t.Fatalf("%s: value differs for key %x:\n pure-Go=%x\n     cgo=%x", name, k, av, bv)
		}
	}
}
