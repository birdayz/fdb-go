//go:build libfdbc

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/tools/clientbench/internal/bench"
)

// TestNativeBarrierFIFOOrdering proves backend.Barrier() blocks behind a parked
// native FIFO predecessor, whereas non-waiting alternatives are rejected.
func TestNativeBarrierFIFOOrdering(t *testing.T) {
	clusterFile := filepath.Join(t.TempDir(), "fdb.cluster")
	if err := os.WriteFile(clusterFile, []byte("test:test@127.0.0.1:4500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, _, err := openBackend(bench.Config{
		ClusterFile: clusterFile,
		TimeoutMS:   30000,
		RetryLimit:  10,
	})
	if err != nil {
		t.Fatalf("openBackend: %v", err)
	}
	defer db.Close()
	b := db.(*backend)

	probeDB, err := openNativeProbeDB(clusterFile)
	if err != nil {
		t.Fatalf("openNativeProbeDB: %v", err)
	}
	defer probeDB.Close()

	// Negative control 1: synchronous option returns while the native network
	// thread is still parked, so verifySynchronousBypassRejected rejects it.
	err = probeDB.verifySynchronousBypassRejected(func() error {
		tx, err := b.db.CreateTransaction()
		if err != nil {
			return err
		}
		err = tx.Options().SetReadYourWritesDisable()
		tx.Cancel()
		return err
	})
	if !errors.Is(err, errSynchronousBypass) {
		t.Fatalf("verifySynchronousBypassRejected (option) = %v, want %v", err, errSynchronousBypass)
	}

	// Negative control 2: queuing GetApproximateSize and invoking onBarrierWait
	// without awaiting fut.Get() returns while the gate is parked and is rejected.
	err = probeDB.verifyBarrierWaitsForPredecessor(b, func() error {
		tx, err := b.db.CreateTransaction()
		if err != nil {
			return err
		}
		fut := tx.GetApproximateSize()
		if b.onBarrierWait != nil {
			fut = b.onBarrierWait(fut)
		}
		fut.Cancel()
		tx.Cancel()
		return nil
	})
	if !errors.Is(err, errSynchronousBypass) {
		t.Fatalf("verifyBarrierWaitsForPredecessor (no-wait future) = %v, want %v", err, errSynchronousBypass)
	}

	// Negative control 3: ignoring the wrapper returned by onBarrierWait and
	// blocking on raw fut.Get() times out the parked gate and is rejected.
	err = probeDB.verifyBarrierWaitsForPredecessorBounded(b, 100, func() error {
		tx, err := b.db.CreateTransaction()
		if err != nil {
			return err
		}
		fut := tx.GetApproximateSize()
		if b.onBarrierWait != nil {
			_ = b.onBarrierWait(fut)
		}
		_, err = fut.Get()
		tx.Cancel()
		return err
	})
	if !errors.Is(err, errSynchronousBypass) {
		t.Fatalf("verifyBarrierWaitsForPredecessorBounded (unwrapped Get) = %v, want %v", err, errSynchronousBypass)
	}

	// Positive control: production b.Barrier() queues an unready future while the
	// network thread is parked and returns only after the predecessor completes.
	if err := probeDB.verifyBarrierWaitsForPredecessor(b, b.Barrier); err != nil {
		t.Fatalf("verifyBarrierWaitsForPredecessor(b.Barrier) = %v, want nil", err)
	}
}
