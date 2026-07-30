package recordlayer

import (
	"testing"
	"time"

	"fdb.dev/pkg/dst"
)

// TestStoreHeader_LastUpdateTimeSeamed proves the RFC-199 Tier-0 Clock seam on the
// primary persisted-byte site: the store header's LastUpdateTime. Under a sim env it is a
// deterministic function of the sim clock (Epoch), reproducible across runs; under
// production (nil env) it falls back to the wall clock — byte-identical to pre-seam
// behavior. This is the site every store open writes (createStoreHeader).
func TestStoreHeader_LastUpdateTimeSeamed(t *testing.T) {
	t.Parallel()
	epochMillis := uint64(dst.Epoch.UnixMilli())

	// Sim env → the persisted timestamp is the sim clock (Epoch), not the wall clock.
	h1 := createStoreHeader(1, nil, dst.NewSim(7))
	if h1.LastUpdateTime == nil {
		t.Fatal("LastUpdateTime unset")
	}
	if *h1.LastUpdateTime != epochMillis {
		t.Fatalf("sim LastUpdateTime = %d, want Epoch %d", *h1.LastUpdateTime, epochMillis)
	}

	// Reproducible: a fresh sim at the same seed yields the identical persisted timestamp.
	h2 := createStoreHeader(1, nil, dst.NewSim(7))
	if *h2.LastUpdateTime != *h1.LastUpdateTime {
		t.Fatalf("sim header not reproducible: %d vs %d", *h1.LastUpdateTime, *h2.LastUpdateTime)
	}

	// Production (nil env) uses the wall clock — must NOT be the fixed Epoch, and must be
	// a recent time (proves the fallback path is live, i.e. we didn't hard-wire Epoch).
	before := uint64(time.Now().UnixMilli())
	h3 := createStoreHeader(1, nil, nil)
	after := uint64(time.Now().UnixMilli())
	if *h3.LastUpdateTime == epochMillis {
		t.Fatal("production LastUpdateTime is the sim Epoch — the wall-clock fallback is broken")
	}
	if *h3.LastUpdateTime < before || *h3.LastUpdateTime > after {
		t.Fatalf("production LastUpdateTime %d not within [%d,%d]", *h3.LastUpdateTime, before, after)
	}
}

// TestContextEnv_NilSafeAndInherited proves the Env threads DB→context and that a nil
// database env (production) yields a context whose Env() accessors are safe and wall-clock.
func TestContextEnv_NilSafeAndInherited(t *testing.T) {
	t.Parallel()

	// Production database: contexts inherit a nil env, which Env() returns and the *dst.Env
	// accessors treat as wall clock.
	prod := &FDBDatabase{}
	if prod.Env() != nil {
		t.Fatal("unset database env should be nil (production)")
	}
	prodCtx := &FDBRecordContext{env: prod.env}
	if prodCtx.Env().Now().IsZero() {
		t.Fatal("nil-env context Now() returned zero (should be wall clock)")
	}

	// Simulation database: SetEnv installs a seeded env that contexts inherit.
	sim := (&FDBDatabase{}).SetEnv(dst.NewSim(3))
	simCtx := &FDBRecordContext{env: sim.env}
	if !simCtx.Env().Now().Equal(dst.Epoch) {
		t.Fatalf("sim context Now() = %v, want Epoch %v", simCtx.Env().Now(), dst.Epoch)
	}

	// A nil context is safe too.
	var nilCtx *FDBRecordContext
	if nilCtx.Env() != nil {
		t.Fatal("nil context Env() should be nil")
	}
	if nilCtx.Env().Now().IsZero() {
		t.Fatal("nil context Env().Now() returned zero (should be wall clock)")
	}
}
