package recordlayer

import (
	"context"
	"strings"
	"testing"
)

// TestSPFreshCoarsePassChangelogLimit pins that coarsePass writes
// one changelog delta per coarse cell in a SINGLE transaction, and the
// changelog's 2-byte user-version caps a tx at spfreshMaxDeltasPerTx (65536)
// deltas. At defaults k0 crosses that at ~267M records — BELOW the K0>sample
// cliff (~1.0B) — so without the explicit guard the build failed deep in
// spfreshAppendDeltas with a generic "too many deltas". The guard fires first
// with a clear message. coarsePass computes k0 from totalN and returns before
// any FDB access, so this needs no cluster.
func TestSPFreshCoarsePassChangelogLimit(t *testing.T) {
	t.Parallel()
	b := &spfreshBuilder{config: DefaultSPFreshConfig(2)} // Lmax=256, r=2, cellTarget=48 ⇒ avgFill=170
	// totalN large enough that k0 = ceil(totalN·2 / (170·48)) > 65536 ⇒ totalN > ~267M.
	const totalN = 300_000_000
	sample := [][]float64{{0, 0}, {1, 1}} // tiny; the changelog guard precedes the K0>sample check
	err := b.coarsePass(context.Background(), sample, totalN, 7)
	if err == nil {
		t.Fatalf("expected the single-tx changelog-limit error at totalN=%d, got nil", totalN)
	}
	if !strings.Contains(err.Error(), "single-tx changelog limit") {
		t.Fatalf("expected the changelog-limit error, got: %v", err)
	}
}
