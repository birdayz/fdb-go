//go:build libfdbc

package main

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/tools/clientbench/internal/bench"
)

// TestNativeBarrierFIFOOrdering proves backend.Barrier() blocks behind a parked
// native FIFO predecessor, whereas synchronous GetCommittedVersion is rejected.
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

	// Negative control: synchronous GetCommittedVersion returns while the native
	// network thread is still parked before the predecessor, so the probe rejects it.
	err = probeDB.verifyBarrierWaitsForPredecessor(func() error {
		tx, err := b.db.CreateTransaction()
		if err != nil {
			return err
		}
		_, err = tx.GetCommittedVersion()
		tx.Cancel()
		return err
	})
	if err == nil {
		t.Fatal("verifyBarrierWaitsForPredecessor accepted synchronous GetCommittedVersion that bypassed the native FIFO")
	}

	// Positive control: production b.Barrier() remains blocked while the native
	// network thread is parked and returns only after the predecessor completes.
	if err := probeDB.verifyBarrierWaitsForPredecessor(b.Barrier); err != nil {
		t.Fatalf("verifyBarrierWaitsForPredecessor(b.Barrier) = %v, want nil", err)
	}
}
