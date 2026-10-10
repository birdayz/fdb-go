package rywlocal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
	"fdb.dev/pkg/fdbgo/wire"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
)

type localRead struct {
	name string
	read func(context.Context, *client.Transaction, []byte, []byte, []byte, []byte) error
}

func pointValue(got []byte, err error, want []byte) error {
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("value %q, want %q", got, want)
	}
	return nil
}

func localReaders() []localRead {
	readers := []localRead{
		{"get", func(ctx context.Context, tx *client.Transaction, k1, _, _, _ []byte) error {
			v, err := tx.Get(ctx, k1)
			return pointValue(v, err, []byte("one"))
		}},
		{"snapshot_get", func(ctx context.Context, tx *client.Transaction, k1, _, _, _ []byte) error {
			v, err := tx.Snapshot().Get(ctx, k1)
			return pointValue(v, err, []byte("one"))
		}},
		{"pipelined_get", func(ctx context.Context, tx *client.Transaction, k1, _, _, _ []byte) error {
			v, pending, err := tx.GetPipelined(ctx, k1)
			if err != nil {
				return err
			}
			if pending != nil {
				return fmt.Errorf("local write produced a remote PendingGet")
			}
			return pointValue(v, nil, []byte("one"))
		}},
		{"get_cleared", func(ctx context.Context, tx *client.Transaction, _, _, begin, _ []byte) error {
			v, err := tx.Get(ctx, begin)
			return pointValue(v, err, nil)
		}},
		{"get_key", func(ctx context.Context, tx *client.Transaction, k1, _, _, _ []byte) error {
			v, err := tx.GetKey(ctx, k1, false, 1)
			return pointValue(v, err, k1)
		}},
		{"snapshot_get_key", func(ctx context.Context, tx *client.Transaction, k1, _, _, _ []byte) error {
			v, err := tx.Snapshot().GetKey(ctx, k1, false, 1)
			return pointValue(v, err, k1)
		}},
	}
	for _, snapshot := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, limit := range []int{0, 1} {
				readers = append(readers, localRead{
					name: fmt.Sprintf("range/snapshot=%t/reverse=%t/limit=%d", snapshot, reverse, limit),
					read: func(ctx context.Context, tx *client.Transaction, k1, k2, begin, end []byte) error {
						var rows []client.KeyValue
						var more bool
						var err error
						if snapshot {
							rows, more, err = tx.Snapshot().GetRangeWithByteTarget(ctx, begin, end, limit, client.ByteLimitUnlimited, reverse)
						} else {
							rows, more, err = tx.GetRangeWithByteTarget(ctx, begin, end, limit, client.ByteLimitUnlimited, reverse)
						}
						if err != nil {
							return err
						}
						want := []client.KeyValue{{Key: k1, Value: []byte("one")}, {Key: k2, Value: []byte("two")}}
						if reverse {
							want[0], want[1] = want[1], want[0]
						}
						if limit == 1 {
							want = want[:1]
						}
						if len(rows) != len(want) || more != (limit == 1) {
							return fmt.Errorf("range rows=%d more=%t, want rows=%d more=%t", len(rows), more, len(want), limit == 1)
						}
						for i := range want {
							if !bytes.Equal(rows[i].Key, want[i].Key) || !bytes.Equal(rows[i].Value, want[i].Value) {
								return fmt.Errorf("range row %d: %q=%q, want %q=%q", i, rows[i].Key, rows[i].Value, want[i].Key, want[i].Value)
							}
						}
						return nil
					},
				})
			}
		}
	}
	return readers
}

// C++ ReadYourWrites.actor.cpp read() serves known write-map segments without
// entering NativeAPI's GRV path, including when the explicit version is ancient.
func TestLocalReadsDoNotAcquireReadVersion(t *testing.T) {
	t.Parallel()
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setup, "", foundationdbtc.WithAPIVersion(730), foundationdbtc.WithDirectIP())
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
	cluster, err := container.ClusterFile(setup)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := client.ParseClusterString(cluster)
	if err != nil {
		t.Fatal(err)
	}
	for _, reader := range localReaders() {
		for _, mode := range []string{"fresh", "ancient", "cancelled", "caller_cancelled", "timed_out", "deferred"} {
			t.Run(reader.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				db, err := client.OpenDatabaseFromConfig(ctx, cf, client.WithAPIVersion(730))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				prefix := []byte(t.Name() + "/")
				begin := append(bytes.Clone(prefix), 'a', '/')
				end := append(bytes.Clone(prefix), 'b')
				k1, k2 := append(bytes.Clone(begin), '1'), append(bytes.Clone(begin), '2')
				remote := append(bytes.Clone(prefix), 'r')
				_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Set(remote, []byte("remote")); return nil, nil })
				if err != nil {
					t.Fatal(err)
				}
				tx := db.CreateTransaction()
				defer tx.Cancel()
				if err := tx.ClearRange(begin, end); err != nil {
					t.Fatal(err)
				}
				tx.Set(k1, []byte("one"))
				tx.Set(k2, []byte("two"))
				readCtx := ctx
				switch mode {
				case "ancient":
					tx.SetReadVersion(1)
				case "cancelled":
					tx.Cancel()
				case "timed_out":
					tx.SetTimeout(1)
					// Start the read after expiry; no timer races a server reply.
					time.Sleep(2 * time.Millisecond)
				case "deferred":
					tx.SetReadYourWritesDisable()
				case "caller_cancelled":
					var stop context.CancelFunc
					readCtx, stop = context.WithCancel(ctx)
					stop()
				}
				before := db.Metrics()
				err = reader.read(readCtx, tx, k1, k2, begin, end)
				switch mode {
				case "cancelled":
					var fdbErr *wire.FDBError
					if !errors.As(err, &fdbErr) || fdbErr.Code != 1025 {
						t.Fatalf("cancelled local read: %v, want FDB1025", err)
					}
				case "timed_out", "deferred":
					want := 1031
					if mode == "deferred" {
						want = 2000
					}
					var fdbErr *wire.FDBError
					if !errors.As(err, &fdbErr) || fdbErr.Code != want {
						t.Fatalf("%s local read: %v, want FDB%d", mode, err, want)
					}
				case "caller_cancelled":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled caller: %v", err)
					}
				default:
					if err != nil {
						t.Fatal(err)
					}
				}
				after := db.Metrics()
				if n := after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted; n != 0 {
					t.Errorf("LOCAL_READ_GRV: acquired %d read versions for fully local %s", n, reader.name)
				}
				if n := after.GRVCacheHits - before.GRVCacheHits; n != 0 {
					t.Errorf("LOCAL_READ_GRV: used %d cached versions", n)
				}
				if n := after.GRVLatency.Count - before.GRVLatency.Count; n != 0 {
					t.Errorf("LOCAL_READ_GRV: received %d GRV batch replies", n)
				}
				if after.TransactionsCommitStarted != before.TransactionsCommitStarted {
					t.Error("local read committed")
				}
				if mode == "fresh" {
					if _, ok := tx.ReadVersionInstant(); ok {
						t.Error("local read established an MVCC age anchor")
					}
					got, err := tx.Get(ctx, remote)
					if err := pointValue(got, err, []byte("remote")); err != nil {
						t.Fatal(err)
					}
					if n := db.Metrics().TransactionReadVersionsCompleted - after.TransactionReadVersionsCompleted; n != 1 {
						t.Errorf("remote positive control acquired %d versions, want 1", n)
					}
				} else if mode == "ancient" {
					_, err := tx.Get(ctx, remote)
					var fdbErr *wire.FDBError
					if !errors.As(err, &fdbErr) || fdbErr.Code != 1007 {
						t.Fatalf("ancient version remote read: %v, want FDB1007", err)
					}
				}
			})
		}
	}
}
