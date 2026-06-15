package fdb

import (
	"strings"
	"testing"
)

// TestFuturePanic_BecomesError pins RFC-110 Class C: a panic in a facade future's
// fn() (which runs on a detached goroutine) must become the future's error — the
// caller's Get sees a normal error — instead of aborting the whole host.
func TestFuturePanic_BecomesError(t *testing.T) {
	t.Parallel()
	f := newFutureByteSlice(func() ([]byte, error) { panic("boom in fn") })
	val, err := f.Get()
	if err == nil {
		t.Fatal("Get returned nil err for a panicking fn — the host would have crashed")
	}
	if val != nil {
		t.Fatalf("Get val = %v, want nil", val)
	}
	if !strings.Contains(err.Error(), "panic resolving future") {
		t.Fatalf("err = %v, want a panic-resolving-future error", err)
	}
}

// TestFutureNilPanic_BecomesError: same for the FutureNil variant (fn returns
// only an error).
func TestFutureNilPanic_BecomesError(t *testing.T) {
	t.Parallel()
	f := newFutureNil(func() error { panic("boom") })
	if err := f.Get(); err == nil {
		t.Fatal("FutureNil.Get returned nil err for a panicking fn")
	}
}

// TestFuturePanic_NoFalsePositive: the backstop is a no-op on the happy path — a
// clean fn resolves normally.
func TestFuturePanic_NoFalsePositive(t *testing.T) {
	t.Parallel()
	f := newFutureByteSlice(func() ([]byte, error) { return []byte("ok"), nil })
	val, err := f.Get()
	if err != nil || string(val) != "ok" {
		t.Fatalf("Get = %q, %v; want \"ok\", nil", val, err)
	}
}
