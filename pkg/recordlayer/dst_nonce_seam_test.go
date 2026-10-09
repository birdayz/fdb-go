package recordlayer

// Re-verification of the RFC-199 Tier-0 claim that every persisted-nonce site is
// routed through the Env seam AND that a nil env (production) still draws from the
// production primitive. The RFC lists the indexer UUID, the R-tree node IDs, the
// HNSW sample keys and the SPFresh owner nonce as seamed; before these tests only
// the store-header timestamp was pinned, so "seamed" was prose for four of them.
//
// Each site is checked on both axes, because a seam can fail in two independent
// directions: it can stop being reproducible under a sim env (the seam is dead and
// the site reads the real source), or it can stop reading the real source under a
// nil env (the seam is hard-wired to the sim and production bytes changed). A test
// that only checks one direction passes with the other direction broken.

import (
	"testing"

	"fdb.dev/pkg/dst"
	"github.com/google/uuid"
)

// TestIndexerID_SeamedBothDirections covers the indexer UUID that every heartbeat
// key carries. Production must stay byte-shape-identical to uuid.New(): a random
// (v4) UUID with the RFC 4122 variant bits, freshly drawn per call.
func TestIndexerID_SeamedBothDirections(t *testing.T) {
	t.Parallel()

	// Production (nil env): a real v4 UUID, and distinct per call.
	a, b := newIndexerID(nil), newIndexerID(nil)
	if a.Version() != 4 {
		t.Fatalf("nil-env indexer ID version = %d, want 4 — production must match uuid.New()", a.Version())
	}
	if a.Variant() != uuid.RFC4122 {
		t.Fatalf("nil-env indexer ID variant = %v, want RFC4122 — production must match uuid.New()", a.Variant())
	}
	if a == b {
		t.Fatal("nil-env indexer IDs repeated — the crypto/rand fallback is not live")
	}
	if a == (uuid.UUID{}) {
		t.Fatal("nil-env indexer ID is the nil UUID — entropy never reached it")
	}

	// Simulation: reproducible from the seed across independently built envs.
	s1 := newIndexerID(dst.NewSim(11))
	s2 := newIndexerID(dst.NewSim(11))
	if s1 != s2 {
		t.Fatalf("sim indexer ID not reproducible at the same seed: %v vs %v", s1, s2)
	}
	if s1.Version() != 4 || s1.Variant() != uuid.RFC4122 {
		t.Fatalf("sim indexer ID is not a well-formed v4 UUID: version=%d variant=%v", s1.Version(), s1.Variant())
	}
	// A different seed must give a different ID, or "reproducible" would be
	// satisfied by a constant.
	if other := newIndexerID(dst.NewSim(12)); other == s1 {
		t.Fatal("sim indexer ID is seed-independent — a constant would pass the reproducibility check")
	}
}
