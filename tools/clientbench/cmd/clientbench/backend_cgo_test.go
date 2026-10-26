//go:build libfdbc

package main

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/tools/clientbench/internal/bench"
)

// TestNativeBarrierFIFOOrdering verifies that backend.Barrier() waits for a
// predecessor queued on the native onMainThread FIFO before returning, whereas a
// synchronous GetCommittedVersion barrier returns before the predecessor completes.
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

	// Negative control: synchronous GetCommittedVersion returns before the parked
	// onMainThread predecessor runs, so runWithBarrierProbe must reject it.
	err = runWithBarrierProbe(b.cDB, func() error {
		tx, err := b.db.CreateTransaction()
		if err != nil {
			return err
		}
		_, err = tx.GetCommittedVersion()
		tx.Cancel()
		return err
	})
	if err == nil {
		t.Fatal("runWithBarrierProbe accepted synchronous GetCommittedVersion that bypassed the native FIFO")
	}

	// Positive control: the production b.Barrier() must wait until the queued
	// onMainThread predecessor has completed before returning.
	if err := runWithBarrierProbe(b.cDB, b.Barrier); err != nil {
		t.Fatalf("runWithBarrierProbe(b.Barrier) = %v, want nil", err)
	}
}
