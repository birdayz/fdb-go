package vectorindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// The SPFresh half of recordlayer's TestFDB_AggregateScanModes: the batch
// record scan SPFresh's build and assignment drive reads with the requested
// isolation and the iterator streaming mode. The observers forward every
// operation to a real FDB transaction; recording at GetRange pins the boundary
// production calls, and prefix filtering excludes store-header reads.
type scanModeObservation struct {
	options  fdb.RangeOptions
	snapshot bool
}

type scanModeRecorder struct {
	mu       sync.Mutex
	prefixes [][]byte
	reads    []scanModeObservation
}

func (r *scanModeRecorder) observe(rng fdb.Range, options fdb.RangeOptions, snapshot bool) {
	begin, _ := rng.FDBRangeKeySelectors()
	matched := false
	for _, prefix := range r.prefixes {
		matched = matched || bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), prefix)
	}
	if !matched {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, scanModeObservation{options, snapshot})
}

type scanModeTransaction struct {
	fdb.WritableTransaction
	recorder *scanModeRecorder
}

func (tx *scanModeTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.recorder.observe(r, options, false)
	return tx.WritableTransaction.GetRange(r, options)
}

func (tx *scanModeTransaction) Snapshot() fdb.ReadTransaction {
	return &scanModeReadTransaction{ReadTransaction: tx.WritableTransaction.Snapshot(), recorder: tx.recorder}
}

type scanModeReadTransaction struct {
	fdb.ReadTransaction
	recorder *scanModeRecorder
}

func (tx *scanModeReadTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.recorder.observe(r, options, true)
	return tx.ReadTransaction.GetRange(r, options)
}

func (tx *scanModeReadTransaction) Snapshot() fdb.ReadTransaction {
	return &scanModeReadTransaction{ReadTransaction: tx.ReadTransaction.Snapshot(), recorder: tx.recorder}
}

func TestFDB_SPFreshScanModes(t *testing.T) {
	t.Parallel()
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setupCtx, "", foundationdbtc.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			t.Error(err)
		}
	})
	cluster, err := container.ClusterFile(setupCtx)
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
	db := recordlayer.NewFDBDatabase(rawDB)

	for _, kind := range []string{"spfresh_sample", "spfresh_assignment"} {
		isolation := recordlayer.SnapshotIsolation
		if kind == "spfresh_assignment" {
			isolation = recordlayer.SerializableIsolation
		}
		t.Run(fmt.Sprintf("%s/isolation=%d", kind, isolation), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			idx := recordlayer.NewIndex("scan_mode", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")))
			idx.Type = recordlayer.IndexTypeVectorSPFresh
			idx.Options = map[string]string{recordlayer.IndexOptionSPFreshNumDimensions: "2"}
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			openStore := func(rctx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
				return recordlayer.NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			}
			_, err = db.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
				store, err := openStore(rctx)
				if err != nil {
					return nil, err
				}
				if _, err := store.MarkIndexDisabled(idx.Name); err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)), Quantity: proto.Int32(int32(i))}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			recorder := &scanModeRecorder{prefixes: [][]byte{ks.Sub(recordlayer.RecordKey).Bytes()}}
			observeStore := func(rctx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
				observed := recordlayer.NewFDBRecordContext(&scanModeTransaction{WritableTransaction: rctx.Transaction(), recorder: recorder}, nil)
				return openStore(observed)
			}
			var got []spfreshBuildInput
			var inTx func(*recordlayer.FDBRecordContext, []spfreshBuildInput) error
			var post func([]spfreshBuildInput) error
			if kind == "spfresh_assignment" {
				inTx = func(_ *recordlayer.FDBRecordContext, batch []spfreshBuildInput) error {
					// Assignment callbacks are idempotent across transaction retries.
					for _, entry := range batch {
						found := false
						for _, prior := range got {
							found = found || bytes.Equal(prior.fullPK.Pack(), entry.fullPK.Pack())
						}
						if !found {
							got = append(got, entry)
						}
					}
					return nil
				}
			} else {
				post = func(batch []spfreshBuildInput) error { got = append(got, batch...); return nil }
			}
			err = spfreshScanRecordBatches(ctx, db, observeStore, idx, ks.Sub(recordlayer.IndexKey).Sub(idx.SubspaceTupleKey()), 2, inTx, post)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 3 {
				t.Fatalf("SPFresh batch scan returned %d vectors, want 3", len(got))
			}
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			if len(recorder.reads) == 0 {
				t.Fatal("no production range read reached the observer")
			}
			for _, read := range recorder.reads {
				if read.options.Mode != fdb.StreamingModeIterator || read.snapshot != (isolation == recordlayer.SnapshotIsolation) {
					t.Fatalf("range mode/snapshot = %v/%v, want %v/%v", read.options.Mode, read.snapshot, fdb.StreamingModeIterator, isolation == recordlayer.SnapshotIsolation)
				}
			}
			t.Logf("observed %d real range reads with mode %v", len(recorder.reads), fdb.StreamingModeIterator)
		})
	}
}
