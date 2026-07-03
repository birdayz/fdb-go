package dst

import (
	"bytes"
	"testing"
	"time"
)

func TestRealClock_AdvancesWithWallTime(t *testing.T) {
	t.Parallel()
	c := RealClock{}
	t0 := c.Now()
	time.Sleep(2 * time.Millisecond)
	if !c.Now().After(t0) {
		t.Fatalf("RealClock.Now did not advance: %v then %v", t0, c.Now())
	}
}

func TestSimClock_DeterministicAndDriverControlled(t *testing.T) {
	t.Parallel()
	start := Epoch
	c := NewSimClock(start)

	// Now is fixed until advanced.
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v, want %v", got, start)
	}
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now moved without Advance: %v", got)
	}

	// Advance moves it forward deterministically.
	if got := c.Advance(90 * time.Second); !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("Advance returned %v, want %v", got, start.Add(90*time.Second))
	}
	if got := Since(c, start); got != 90*time.Second {
		t.Fatalf("Since = %v, want 90s", got)
	}

	// Negative advance is a no-op (timestamps must not go backwards).
	before := c.Now()
	if got := c.Advance(-time.Hour); !got.Equal(before) {
		t.Fatalf("negative Advance moved the clock: %v, want %v", got, before)
	}

	// Set jumps to an exact instant.
	target := Epoch.Add(365 * 24 * time.Hour)
	c.Set(target)
	if got := c.Now(); !got.Equal(target) {
		t.Fatalf("after Set, Now = %v, want %v", got, target)
	}
}

func TestSimClock_TwoClocksSameSeedSameTimeline(t *testing.T) {
	t.Parallel()
	a, b := NewSimClock(Epoch), NewSimClock(Epoch)
	for _, d := range []time.Duration{time.Second, 5 * time.Second, 100 * time.Millisecond} {
		a.Advance(d)
		b.Advance(d)
	}
	if !a.Now().Equal(b.Now()) {
		t.Fatalf("identical advance sequence diverged: %v vs %v", a.Now(), b.Now())
	}
}

func TestSeededRandomness_DeterministicSameSeed(t *testing.T) {
	t.Parallel()
	r1 := NewSeededRandomness(42)
	r2 := NewSeededRandomness(42)
	b1, b2 := make([]byte, 64), make([]byte, 64)
	mustRead(t, r1, b1)
	mustRead(t, r2, b2)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("same seed produced different bytes:\n%x\n%x", b1, b2)
	}
}

func TestSeededRandomness_DifferentSeedDiffers(t *testing.T) {
	t.Parallel()
	b1, b2 := make([]byte, 64), make([]byte, 64)
	mustRead(t, NewSeededRandomness(1), b1)
	mustRead(t, NewSeededRandomness(2), b2)
	if bytes.Equal(b1, b2) {
		t.Fatal("different seeds produced identical bytes")
	}
}

func TestSeededRandomness_FillsExactLengthIncludingNonMultipleOf8(t *testing.T) {
	t.Parallel()
	// 13 is not a multiple of 8 — the tail must still be filled, and reproducibly.
	for _, n := range []int{0, 1, 7, 8, 13, 100} {
		p := make([]byte, n)
		got, err := NewSeededRandomness(7).Read(p)
		if err != nil {
			t.Fatalf("Read(%d) error: %v", n, err)
		}
		if got != n {
			t.Fatalf("Read(%d) = %d, want %d", n, got, n)
		}
		// Same seed, same length → same bytes (tail determinism).
		q := make([]byte, n)
		mustRead(t, NewSeededRandomness(7), q)
		if !bytes.Equal(p, q) {
			t.Fatalf("Read(%d) not reproducible:\n%x\n%x", n, p, q)
		}
	}
}

func TestCryptoRandomness_FillsAndVaries(t *testing.T) {
	t.Parallel()
	b1, b2 := make([]byte, 32), make([]byte, 32)
	mustRead(t, CryptoRandomness{}, b1)
	mustRead(t, CryptoRandomness{}, b2)
	if bytes.Equal(b1, b2) {
		t.Fatal("crypto/rand produced identical 32-byte reads (astronomically unlikely)")
	}
	if bytes.Equal(b1, make([]byte, 32)) {
		t.Fatal("crypto/rand left the buffer zeroed")
	}
}

func TestBuggifier_DisabledNeverFires(t *testing.T) {
	t.Parallel()
	for _, b := range []*Buggifier{DisabledBuggifier(), NewBuggifier(1, false), nil} {
		for i := 0; i < 1000; i++ {
			if b.Buggify("site") {
				t.Fatal("disabled/nil Buggifier fired")
			}
			if b.BuggifyWithProb("site", 1.0) {
				t.Fatal("disabled/nil Buggifier fired at prob 1.0")
			}
		}
	}
}

func TestBuggifier_ProbZeroNeverProbOneAlwaysWhenActive(t *testing.T) {
	t.Parallel()
	b := NewBuggifier(99, true)
	// Force the site active so we isolate the firing gate from activation.
	b.activated["s"] = true
	for i := 0; i < 500; i++ {
		if b.BuggifyWithProb("s", 0.0) {
			t.Fatal("fired at prob 0.0")
		}
	}
	for i := 0; i < 500; i++ {
		if !b.BuggifyWithProb("s", 1.0) {
			t.Fatal("did not fire at prob 1.0 on an active site")
		}
	}
}

func TestBuggifier_InactiveSiteNeverFires(t *testing.T) {
	t.Parallel()
	b := NewBuggifier(3, true)
	b.activated["dead"] = false // pin inactive
	for i := 0; i < 500; i++ {
		if b.BuggifyWithProb("dead", 1.0) {
			t.Fatal("inactive site fired even at prob 1.0")
		}
	}
}

func TestBuggifier_ActivationCachedPerSite(t *testing.T) {
	t.Parallel()
	// With activation prob 1.0 the first hit activates and caches; a site is stable.
	b := NewBuggifier(5, true)
	b.SetProbabilities(1.0, 1.0)
	if !b.Buggify("x") {
		t.Fatal("prob 1.0/1.0 site did not fire")
	}
	// Same site keeps firing; a fresh site also activates (prob 1.0).
	if !b.Buggify("x") || !b.Buggify("y") {
		t.Fatal("cached activation regressed")
	}

	// With activation prob 0.0 no site ever activates → never fires even at fire prob 1.0.
	b2 := NewBuggifier(5, true)
	b2.SetProbabilities(0.0, 1.0)
	for _, s := range []string{"a", "b", "c"} {
		if b2.Buggify(s) {
			t.Fatalf("site %q fired with activation prob 0", s)
		}
	}
}

func TestBuggifier_DeterministicSameSeed(t *testing.T) {
	t.Parallel()
	seq := func() []bool {
		b := NewBuggifier(1234, true)
		var out []bool
		// Interleave several sites so both activation and firing rolls are exercised.
		for i := 0; i < 200; i++ {
			out = append(out, b.Buggify("commit"))
			out = append(out, b.Buggify("read"))
			out = append(out, b.BuggifyWithProb("size", 0.5))
		}
		return out
	}
	s1, s2 := seq(), seq()
	for i := range s1 {
		if s1[i] != s2[i] {
			t.Fatalf("Buggify sequence diverged at %d for the same seed", i)
		}
	}
	// Sanity: at 0.25/0.25 the sequence is not all-false and not all-true.
	anyTrue, anyFalse := false, false
	for _, v := range s1 {
		anyTrue = anyTrue || v
		anyFalse = anyFalse || !v
	}
	if !anyTrue || !anyFalse {
		t.Fatalf("degenerate fire sequence (anyTrue=%v anyFalse=%v)", anyTrue, anyFalse)
	}
}

func TestBuggifier_SetProbabilitiesClamps(t *testing.T) {
	t.Parallel()
	b := NewBuggifier(1, true)
	b.SetProbabilities(-5, 42)
	if b.activationProb != 0 || b.fireProb != 1 {
		t.Fatalf("clamp failed: activation=%v fire=%v", b.activationProb, b.fireProb)
	}
}

func TestEnv_ProductionDefaults(t *testing.T) {
	t.Parallel()
	e := Production()
	if _, ok := e.Clock.(RealClock); !ok {
		t.Fatalf("Production clock = %T, want RealClock", e.Clock)
	}
	if _, ok := e.Random.(CryptoRandomness); !ok {
		t.Fatalf("Production random = %T, want CryptoRandomness", e.Random)
	}
	if e.Buggify.Enabled() {
		t.Fatal("Production buggify should be disabled")
	}
	if e.Fault("anything") {
		t.Fatal("Production Env.Fault fired")
	}
}

func TestEnv_NilSafeAccessors(t *testing.T) {
	t.Parallel()
	var e *Env // nil
	if e.Now().IsZero() {
		t.Fatal("nil Env.Now returned zero time (should be wall clock)")
	}
	p := make([]byte, 16)
	if _, err := e.Read(p); err != nil {
		t.Fatalf("nil Env.Read error: %v", err)
	}
	if bytes.Equal(p, make([]byte, 16)) {
		t.Fatal("nil Env.Read left buffer zeroed (should be crypto/rand)")
	}
	if e.Fault("s") {
		t.Fatal("nil Env.Fault fired")
	}

	// Env with nil fields also falls back to production.
	empty := &Env{}
	if empty.Now().IsZero() {
		t.Fatal("empty Env.Now returned zero")
	}
	if _, err := empty.Read(make([]byte, 8)); err != nil {
		t.Fatalf("empty Env.Read error: %v", err)
	}
	if empty.Fault("s") {
		t.Fatal("empty Env.Fault fired")
	}
}

func TestEnv_SimIsDeterministicAndReproducible(t *testing.T) {
	t.Parallel()
	run := func() (time.Time, []byte, []bool) {
		e := NewSim(2024)
		sc := e.Clock.(*SimClock)
		sc.Advance(30 * time.Second)
		b := make([]byte, 32)
		if _, err := e.Read(b); err != nil {
			t.Fatalf("sim Read error: %v", err)
		}
		var faults []bool
		for i := 0; i < 50; i++ {
			faults = append(faults, e.Fault("commit"))
		}
		return e.Now(), b, faults
	}
	t1, b1, f1 := run()
	t2, b2, f2 := run()
	if !t1.Equal(t2) {
		t.Fatalf("sim clock diverged: %v vs %v", t1, t2)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("sim randomness diverged:\n%x\n%x", b1, b2)
	}
	for i := range f1 {
		if f1[i] != f2[i] {
			t.Fatalf("sim faults diverged at %d", i)
		}
	}
	// The sim clock starts at Epoch, not the wall clock.
	if !t1.Equal(Epoch.Add(30 * time.Second)) {
		t.Fatalf("sim clock = %v, want Epoch+30s", t1)
	}
}

func mustRead(t *testing.T, r Randomness, p []byte) {
	t.Helper()
	n, err := r.Read(p)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if n != len(p) {
		t.Fatalf("Read = %d, want %d", n, len(p))
	}
}
