package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/bits"
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
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

// These observers forward every operation to a real FDB transaction. Recording
// the options at GetRange pins the maintainer boundary, not a properties helper
// that production might never call. Prefix filtering excludes store-header reads.
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

func TestFDB_AggregateScanModes(t *testing.T) {
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
	db := NewFDBDatabase(rawDB)
	for _, maximum := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_extremum/max=%v", maximum), func(t *testing.T) {
			t.Parallel()
			testPermutedExtremumConcurrentInsert(t, db, maximum)
		})
	}

	for _, kind := range []string{"count", "sum", "min", "max", "bitmap", "permuted_min", "permuted_max", "extremum", "repair_iterator", "repair_small", "repair_want_all", "spfresh_sample", "spfresh_assignment"} {
		for _, isolation := range []IsolationLevel{SerializableIsolation, SnapshotIsolation} {
			if (kind == "extremum" || kind == "spfresh_assignment") && isolation != SerializableIsolation || kind == "spfresh_sample" && isolation != SnapshotIsolation {
				continue
			}
			t.Run(fmt.Sprintf("%s/isolation=%d", kind, isolation), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
				builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
				var idx *Index
				var fn *IndexAggregateFunction
				want := tuple.Tuple{int64(1)}
				switch kind {
				case "count":
					idx = NewCountIndex("scan_mode", GroupAll(Field("price")))
					fn = NewCountAggregateFunction(Ungrouped(EmptyKey()))
					want = tuple.Tuple{int64(3)}
				case "sum":
					idx = NewSumIndex("scan_mode", GroupBy(Field("quantity"), Field("price")))
					fn = NewSumAggregateFunction(Ungrouped(Field("quantity")))
					want = tuple.Tuple{int64(6)}
				case "min", "max":
					idx = NewIndex("scan_mode", Field("price"))
					fn = NewMinAggregateFunction(Field("price"))
					want = tuple.Tuple{int64(10)}
					if kind == "max" {
						fn = NewMaxAggregateFunction(Field("price"))
						want = tuple.Tuple{int64(30)}
					}
				case "bitmap":
					idx = NewBitmapValueIndex("scan_mode", GroupBy(Field("order_id")))
					fn = &IndexAggregateFunction{Name: FunctionNameBitmapValue, Operand: GroupBy(Field("order_id"))}
				case "spfresh_sample", "spfresh_assignment":
					idx = NewIndex("scan_mode", Concat(Field("price"), Field("quantity")))
					idx.Type = IndexTypeVectorSPFresh
					idx.Options = map[string]string{IndexOptionSPFreshNumDimensions: "2"}
				default:
					idx = NewPermutedMinIndex("scan_mode", GroupBy(Field("quantity"), Field("price")), 1)
					fn = NewMinAggregateFunction(Ungrouped(Field("quantity")))
					if kind == "permuted_max" {
						idx = NewPermutedMaxIndex("scan_mode", GroupBy(Field("quantity"), Field("price")), 1)
						fn = NewMaxAggregateFunction(Ungrouped(Field("quantity")))
						want = tuple.Tuple{int64(3)}
					}
				}
				builder.AddIndex("Order", idx)
				md, err := builder.Build()
				if err != nil {
					t.Fatal(err)
				}
				openStore := func(rctx *FDBRecordContext) (*FDBRecordStore, error) {
					return NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				}
				_, err = db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
					store, err := openStore(rctx)
					if err != nil {
						return nil, err
					}
					if idx.Type == IndexTypeVectorSPFresh {
						if _, err := store.MarkIndexDisabled(idx.Name); err != nil {
							return nil, err
						}
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
				recorder := &scanModeRecorder{prefixes: [][]byte{ks.Sub(IndexKey).Bytes(), ks.Sub(IndexSecondarySpaceKey).Bytes()}}
				observeStore := func(rctx *FDBRecordContext) (*FDBRecordStore, error) {
					rctx.tx = &scanModeTransaction{WritableTransaction: rctx.Transaction(), recorder: recorder}
					return openStore(rctx)
				}
				wantMode := fdb.StreamingModeIterator
				if idx.Type == IndexTypeVectorSPFresh {
					recorder.prefixes = [][]byte{ks.Sub(RecordKey).Bytes()}
					var got []spfreshBuildInput
					var inTx func(*FDBRecordContext, []spfreshBuildInput) error
					var post func([]spfreshBuildInput) error
					if kind == "spfresh_assignment" {
						inTx = func(_ *FDBRecordContext, batch []spfreshBuildInput) error {
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
					err = spfreshScanRecordBatches(ctx, db, observeStore, idx, ks.Sub(IndexKey).Sub(idx.SubspaceTupleKey()), 2, inTx, post)
					if err == nil && len(got) != 3 {
						t.Fatalf("SPFresh batch scan returned %d vectors, want 3", len(got))
					}
				} else {
					_, err = db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
						store, err := observeStore(rctx)
						if err != nil {
							return nil, err
						}
						var got tuple.Tuple
						if kind == "extremum" || kind == "repair_iterator" || kind == "repair_small" || kind == "repair_want_all" {
							maintainer, maintErr := store.getIndexMaintainer(idx)
							if maintErr != nil {
								return nil, maintErr
							}
							m := maintainer.(*permutedMinMaxIndexMaintainer)
							if kind == "extremum" {
								got, err = m.getExtremum(tuple.Tuple{int64(10)})
								if err == nil && (len(got) < 2 || got[0] != int64(10) || got[1] != int64(1)) {
									t.Fatalf("extremum key = %v, want prefix [10 1]", got)
								}
								return nil, err
							}
							props := DefaultExecuteProperties().WithIsolationLevel(isolation).WithSkip(7).WithReturnedRowLimit(17)
							if kind == "repair_small" {
								props.DefaultCursorStreamingMode = StreamingModeSmall
								wantMode = fdb.StreamingModeSmall
							} else if kind == "repair_want_all" {
								props.DefaultCursorStreamingMode = StreamingModeWantAll
								wantMode = fdb.StreamingModeWantAll
							}
							got, err = PermutedMinIgnoringNulls(ctx, func(r TupleRange, p ScanProperties) RecordCursor[*IndexEntry] {
								return m.standardIndexMaintainer.Scan(r, nil, p)
							}, idx.Name, tuple.Tuple{int64(10)}, 1, 2, props)
						} else {
							fn.Index = idx.Name
							got, err = store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn, TupleRangeAll, isolation)
						}
						if err != nil {
							return nil, err
						}
						if kind == "bitmap" {
							if len(got) != 1 {
								t.Fatalf("bitmap result = %v, want one bitmap", got)
							}
							bitmap, ok := got[0].([]byte)
							set := 0
							for _, b := range bitmap {
								set += bits.OnesCount8(b)
							}
							if !ok || len(bitmap) == 0 || bitmap[0] != 14 || set != 3 {
								t.Fatalf("bitmap = %x, want exactly bits 1,2,3", bitmap)
							}
						} else if !bytes.Equal(got.Pack(), want.Pack()) {
							t.Fatalf("aggregate = %v, want %v", got, want)
						}
						return nil, nil
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				recorder.mu.Lock()
				defer recorder.mu.Unlock()
				if len(recorder.reads) == 0 {
					t.Fatal("no production range read reached the observer")
				}
				for _, read := range recorder.reads {
					if read.options.Mode != wantMode || read.snapshot != (isolation == SnapshotIsolation) {
						t.Fatalf("range mode/snapshot = %v/%v, want %v/%v", read.options.Mode, read.snapshot, wantMode, isolation == SnapshotIsolation)
					}
					if kind == "max" && !read.options.Reverse || kind == "min" && read.options.Reverse {
						t.Fatalf("VALUE %s used reverse=%v", kind, read.options.Reverse)
					}
				}
				t.Logf("observed %d real range reads with mode %v", len(recorder.reads), wantMode)
			})
		}
	}
}

func testPermutedExtremumConcurrentInsert(t *testing.T, db *FDBDatabase, maximum bool) {
	t.Helper()
	ctx := context.Background()
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	root := GroupBy(Field("quantity"), Field("price"))
	idx := NewPermutedMinIndex("concurrent_extremum", root, 1)
	fn := NewMinAggregateFunction(Ungrouped(Field("quantity")))
	first, second := int32(5), int32(9)
	if maximum {
		idx = NewPermutedMaxIndex("concurrent_extremum", root, 1)
		fn = NewMaxAggregateFunction(Ungrouped(Field("quantity")))
		first, second = second, first
	}
	fn.Index = idx.Name
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	open := func(rctx *FDBRecordContext) (*FDBRecordStore, error) {
		return NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
	}
	_, err = db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
		store, err := open(rctx)
		if err != nil {
			return nil, err
		}
		for i, quantity := range []int32{first, second} {
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(10), Quantity: proto.Int32(quantity)}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatal(err)
	}
	deleting := db.NewRecordContext(tx)
	defer deleting.Cancel()
	store, err := open(deleting)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
	if err != nil || !deleted {
		t.Fatalf("delete original extremum: deleted=%v err=%v", deleted, err)
	}
	// This writer sees the original extremum, so 7 does not replace it. The
	// deleting transaction must conflict and re-read the ordinary group before
	// publishing its replacement; otherwise it installs the stale 9 (or 5).
	_, err = db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
		store, err := open(rctx)
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(10), Quantity: proto.Int32(7)})
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	commitErr := deleting.Commit()
	if commitErr == nil {
		got, readErr := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
			store, err := open(rctx)
			if err != nil {
				return nil, err
			}
			return store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn, TupleRangeAll, SerializableIsolation)
		})
		t.Fatalf("stale extremum replacement committed: aggregate=%v err=%v, want conflict then 7", got, readErr)
	}
	var conflict fdb.Error
	if !errors.As(commitErr, &conflict) || conflict.Code != 1020 {
		t.Fatalf("extremum replacement commit = %v, want not_committed (1020)", commitErr)
	}
	got, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
		store, err := open(rctx)
		if err != nil {
			return nil, err
		}
		if _, err := store.DeleteRecord(tuple.Tuple{int64(1)}); err != nil {
			return nil, err
		}
		return store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn, TupleRangeAll, SerializableIsolation)
	})
	result, ok := got.(tuple.Tuple)
	if err != nil || !ok || len(result) != 1 || result[0] != int64(7) {
		t.Fatalf("retried extremum = %v err=%v, want 7", got, err)
	}
}
