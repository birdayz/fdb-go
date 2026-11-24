package dstwire

import (
	"bytes"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
)

func TestEnvNilReturnsNil(t *testing.T) {
	t.Parallel()
	if got := Env(nil); got != nil {
		t.Fatalf("Env(nil) = %#v, want nil", got)
	}
}

func TestEnvDelegatesDynamically(t *testing.T) {
	t.Parallel()
	sim := dst.NewSim(42)
	wrapped := Env(sim)
	if wrapped == nil {
		t.Fatal("Env(sim) = nil")
	}

	t0 := wrapped.Now()
	sim.Clock.(*dst.SimClock).Advance(5 * time.Second)
	if got := wrapped.Since(t0); got != 5*time.Second {
		t.Fatalf("Since = %v, want 5s", got)
	}

	// Replacing sim.Clock after wiring must be observed through wrapped.
	nextClock := dst.NewSimClock(t0.Add(time.Hour))
	sim.Clock = nextClock
	if got := wrapped.Now(); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("Now after clock swap = %v, want %v", got, t0.Add(time.Hour))
	}

	// Random stream produces identical bytes to a fresh dst.NewSim(42).
	fresh := dst.NewSim(42)
	b1, b2 := make([]byte, 32), make([]byte, 32)
	if _, err := wrapped.Read(b1); err != nil {
		t.Fatalf("wrapped.Read: %v", err)
	}
	if _, err := fresh.Read(b2); err != nil {
		t.Fatalf("fresh.Read: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("random bytes diverged: %x vs %x", b1, b2)
	}
}
