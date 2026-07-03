package dst

import (
	crand "crypto/rand"
	"encoding/binary"
	mrand "math/rand/v2"
	"sync"
)

// Randomness abstracts a source of random bytes. Production uses CryptoRandomness
// (crypto/rand); simulation uses SeededRandomness so nonces, UUIDs, and other bytes that
// end up persisted (R-tree node IDs, HNSW sample keys, indexer UUIDs, spfresh tokens) come
// from a seeded pool instead of the OS CSPRNG — the single worst byte-determinism offender
// the RFC calls out. Mirrors FDB's deterministicRandom() seam.
//
// Determinism caveat (RFC-179 §1a coverage ledger): the record layer generates some of
// these nonces inside concurrent goroutine fan-out (indexer/spfresh). A seeded source makes
// the pool of drawn bytes deterministic, but which goroutine draws which value is still
// scheduler-dependent, so per-node assignment is not bit-exact under fan-out. Full
// bit-exactness comes from driving those paths single-goroutine (Tier 2). The seam still
// removes the crypto/rand nondeterminism that defeats replay outright.
type Randomness interface {
	// Read fills p with random bytes and returns len(p). Matches the crypto/rand.Read and
	// io.Reader contract; the sim implementation never returns an error.
	Read(p []byte) (int, error)
}

// CryptoRandomness is the production source: crypto/rand.
type CryptoRandomness struct{}

// Read fills p from crypto/rand.
func (CryptoRandomness) Read(p []byte) (int, error) { return crand.Read(p) }

// SeededRandomness is a deterministic byte source backed by a seeded math/rand/v2 PCG
// generator. NOT cryptographically secure — simulation only. Guarded by a mutex so the
// record layer's concurrent fan-out cannot data-race the generator (the draw order under
// fan-out is still scheduler-dependent — see the Randomness doc).
type SeededRandomness struct {
	mu  sync.Mutex
	rng *mrand.Rand
}

// randStream is the PCG stream selector for the randomness source, distinct from the
// Buggifier's stream so the two consumers draw from independent sequences of one seed
// (adding a fault point does not shift nonce bytes, and vice versa).
const randStream uint64 = 1

// NewSeededRandomness returns a deterministic Randomness seeded by seed.
func NewSeededRandomness(seed uint64) *SeededRandomness {
	return &SeededRandomness{rng: mrand.New(mrand.NewPCG(seed, randStream))}
}

// Read fills p with deterministic pseudo-random bytes. math/rand/v2's *Rand has no Read
// method (unlike v1), so bytes are drawn 8 at a time from Uint64 in little-endian order.
func (s *SeededRandomness) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf [8]byte
	for i := 0; i < len(p); i += 8 {
		binary.LittleEndian.PutUint64(buf[:], s.rng.Uint64())
		copy(p[i:], buf[:])
	}
	return len(p), nil
}
