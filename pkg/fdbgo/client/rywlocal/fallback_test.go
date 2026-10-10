package rywlocal_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/client"
)

func TestPointReadFallbacks(t *testing.T) {
	t.Parallel()
	cf := rangeCluster(t)
	for _, mode := range []string{"ryw_disabled", "snapshot_disabled", "dependent_atomic", "independent_atomic", "missing", "clear"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			db, err := client.OpenDatabaseFromConfig(ctx, cf, client.WithAPIVersion(730))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			key := []byte(t.Name())
			_, err = db.Transact(ctx, func(tx *client.Transaction) (any, error) { tx.Set(key, []byte{1}); return nil, nil })
			if err != nil {
				t.Fatal(err)
			}
			tx := db.CreateTransaction()
			defer tx.Cancel()
			want, grvs := []byte{1}, int64(1)
			switch mode {
			case "ryw_disabled":
				tx.SetReadYourWritesDisable()
				tx.Set(key, []byte{9})
			case "snapshot_disabled":
				tx.Set(key, []byte{9})
				tx.SetSnapshotRYWDisable()
			case "dependent_atomic":
				tx.Atomic(client.MutAddValue, key, []byte{1})
				want = []byte{2}
			case "independent_atomic":
				tx.Clear(key)
				tx.Atomic(client.MutAddValue, key, []byte{1})
				grvs = 0
			case "missing":
				key = append(bytes.Clone(key), 0)
				want = nil
			case "clear":
				tx.Clear(key)
				want, grvs = nil, 0
			}
			before := db.Metrics()
			var got []byte
			if mode == "snapshot_disabled" {
				got, err = tx.Snapshot().Get(ctx, key)
			} else {
				got, err = tx.Get(ctx, key)
			}
			if err := pointValue(got, err, want); err != nil {
				t.Fatal(err)
			}
			after := db.Metrics()
			if n := after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted; n != grvs {
				t.Errorf("%s acquired %d read versions, want %d", mode, n, grvs)
			}
			if after.GRVCacheHits != before.GRVCacheHits {
				t.Error("read used a cached GRV")
			}
		})
	}
}
