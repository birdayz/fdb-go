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
	"bytes"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
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

// TestRTreeNodeID_SeamedBothDirections covers the R-tree node IDs, which become
// key bytes for every split node.
func TestRTreeNodeID_SeamedBothDirections(t *testing.T) {
	t.Parallel()

	// The constructor must leave env nil: production is the default, and the
	// maintainer opts in by assigning store.Env().
	if newRTreeStorage(subspace.Sub("t"), RTreeConfig{}).env != nil {
		t.Fatal("newRTreeStorage installed a non-nil env — production must be the default")
	}

	prod := newRTreeStorage(subspace.Sub("t"), RTreeConfig{})
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
	simA := newRTreeStorage(subspace.Sub("t"), RTreeConfig{})
	simA.env = dst.NewSim(5)
	simB := newRTreeStorage(subspace.Sub("t"), RTreeConfig{})
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
	simC := newRTreeStorage(subspace.Sub("t"), RTreeConfig{})
	simC.env = dst.NewSim(6)
	first, _ := simC.newRandomNodeID()
	simD := newRTreeStorage(subspace.Sub("t"), RTreeConfig{})
	simD.env = dst.NewSim(5)
	other, _ := simD.newRandomNodeID()
	if bytes.Equal(first, other) {
		t.Fatal("sim node IDs are seed-independent — a constant would pass the reproducibility check")
	}
}

// TestHNSWStorageEnv_DefaultsToProduction pins the HNSW sample-key seam's default.
// The nonce itself is drawn inside appendSampledVector against a live transaction;
// what is checkable without one — and what the rebase had to re-apply — is that the
// constructor leaves the seam at production and the field exists to be assigned.
func TestHNSWStorageEnv_DefaultsToProduction(t *testing.T) {
	t.Parallel()
	s := newHNSWStorage(subspace.Sub("h"), HNSWConfig{})
	if s.env != nil {
		t.Fatal("newHNSWStorage installed a non-nil env — production must be the default")
	}
	// The seam reads through the nil-safe accessor, so a production storage draws
	// real entropy rather than panicking on the nil env.
	var n1, n2 [16]byte
	if _, err := s.env.Read(n1[:]); err != nil {
		t.Fatalf("nil-env sample nonce: %v", err)
	}
	if _, err := s.env.Read(n2[:]); err != nil {
		t.Fatalf("nil-env sample nonce: %v", err)
	}
	if n1 == n2 {
		t.Fatal("nil-env sample nonces repeated — the crypto/rand fallback is not live")
	}

	s.env = dst.NewSim(9)
	var m1, m2 [16]byte
	if _, err := s.env.Read(m1[:]); err != nil {
		t.Fatalf("sim sample nonce: %v", err)
	}
	fresh := newHNSWStorage(subspace.Sub("h"), HNSWConfig{})
	fresh.env = dst.NewSim(9)
	if _, err := fresh.env.Read(m2[:]); err != nil {
		t.Fatalf("sim sample nonce: %v", err)
	}
	if m1 != m2 {
		t.Fatalf("sim sample nonce not reproducible: %x vs %x", m1, m2)
	}
}

// TestSPFreshProcessNonce_SeamedBothDirections covers the SPFresh rebalancer's
// owner nonce. Note this site is deliberately drawn PER RUN rather than once at
// package init: a package-global minted at import time cannot be replayed from a
// seed, which is the whole point of the seam. Per-run is no less unique — the
// nonce only has to separate processes, and spfreshOwnerSeq already separates runs
// within one.
func TestSPFreshProcessNonce_SeamedBothDirections(t *testing.T) {
	t.Parallel()

	// PRODUCTION IS A PROCESS NONCE, not a per-call draw. Before the seam this was a
	// package-level var minted once at import; the seam's claim is that a nil env is
	// byte-identical to that. Routing it through the nil-default accessor would draw fresh
	// bytes on every call — no less unique, but a DIFFERENT PROGRAM, which is precisely what
	// "byte-identical to pre-seam" says does not happen.
	p1, p2 := spfreshProcessNonce(nil), spfreshProcessNonce(nil)
	if p1 == "" {
		t.Fatal("nil-env process nonce is empty")
	}
	if p1 != p2 {
		t.Fatalf("nil-env process nonce changed between calls (%q vs %q): production must keep "+
			"the once-per-process nonce it had before the seam, or the byte-identity claim the "+
			"seam is sold on is false at this site", p1, p2)
	}
	// It still separates processes: the value is drawn (not a constant), just drawn once.
	if p1 == newSPFreshProcessNonce(nil) {
		t.Fatal("the process nonce is a constant — two processes would mint the same lease owner")
	}

	s1 := spfreshProcessNonce(dst.NewSim(21))
	s2 := spfreshProcessNonce(dst.NewSim(21))
	if s1 != s2 {
		t.Fatalf("sim process nonce not reproducible: %q vs %q", s1, s2)
	}
	if other := spfreshProcessNonce(dst.NewSim(22)); other == s1 {
		t.Fatal("sim process nonce is seed-independent — a constant would pass the reproducibility check")
	}

	// The owner string composes the nonce, so a reproducible nonce yields a
	// reproducible owner for a given sequence position.
	o1 := spfreshRebalanceOwner("idx", s1)
	o2 := spfreshRebalanceOwner("idx", s1)
	if o1 == o2 {
		t.Fatal("two rebalance owners at the same nonce collided — spfreshOwnerSeq must separate them")
	}
	if !bytes.Contains([]byte(o1), []byte(s1)) {
		t.Fatalf("owner %q does not carry the process nonce %q", o1, s1)
	}
}
