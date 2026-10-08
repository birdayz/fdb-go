package rtree

// The R-tree node-ID half of the RFC-199 Tier-0 nonce-seam checks (the other
// sites are pinned in pkg/recordlayer/dst_nonce_seam_test.go).

import (
	"bytes"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
)

// TestRTreeNodeID_SeamedBothDirections covers the R-tree node IDs, which become
// key bytes for every split node.
func TestRTreeNodeID_SeamedBothDirections(t *testing.T) {
	t.Parallel()

	// The constructor must leave env nil: production is the default, and the
	// maintainer opts in by assigning store.Env().
	if NewStorageAdapter(subspace.Sub("t"), RTreeConfig{}).env != nil {
		t.Fatal("NewStorageAdapter installed a non-nil env — production must be the default")
	}

	prod := NewStorageAdapter(subspace.Sub("t"), RTreeConfig{})
	id1, err := prod.newRandomNodeID()
	if err != nil {
		t.Fatalf("nil-env node ID: %v", err)
	}
	id2, err := prod.newRandomNodeID()
	if err != nil {
		t.Fatalf("nil-env node ID: %v", err)
	}
	if len(id1) != 16 {
		t.Fatalf("node ID length = %d, want 16 (Java NodeHelpers.newRandomNodeId)", len(id1))
	}
	if bytes.Equal(id1, id2) {
		t.Fatal("nil-env node IDs repeated — the crypto/rand fallback is not live")
	}
	if bytes.Equal(id1, make([]byte, 16)) {
		t.Fatal("nil-env node ID is all zeros — it would collide with rootNodeID")
	}

	// Simulation: two independently built sims at one seed mint the same node ID
	// sequence, which is what makes a vector-index run replayable.
	simA := NewStorageAdapter(subspace.Sub("t"), RTreeConfig{})
	simA.env = dst.NewSim(5)
	simB := NewStorageAdapter(subspace.Sub("t"), RTreeConfig{})
	simB.env = dst.NewSim(5)
	for i := 0; i < 4; i++ {
		gotA, errA := simA.newRandomNodeID()
		gotB, errB := simB.newRandomNodeID()
		if errA != nil || errB != nil {
			t.Fatalf("sim node ID %d: %v / %v", i, errA, errB)
		}
		if !bytes.Equal(gotA, gotB) {
			t.Fatalf("sim node ID %d not reproducible: %x vs %x", i, gotA, gotB)
		}
	}
	// Different seed → different sequence.
	simC := NewStorageAdapter(subspace.Sub("t"), RTreeConfig{})
	simC.env = dst.NewSim(6)
	first, _ := simC.newRandomNodeID()
	simD := NewStorageAdapter(subspace.Sub("t"), RTreeConfig{})
	simD.env = dst.NewSim(5)
	other, _ := simD.newRandomNodeID()
	if bytes.Equal(first, other) {
		t.Fatal("sim node IDs are seed-independent — a constant would pass the reproducibility check")
	}
}
