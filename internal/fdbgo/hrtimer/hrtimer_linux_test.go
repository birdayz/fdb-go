//go:build linux

package hrtimer

import (
	"errors"
	"testing"
	"time"
)

func newTimer(t *testing.T) *Timer {
	t.Helper()
	tm, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tm.Close() })
	return tm
}

// waitAsync runs Wait on its own goroutine, as the timer's owner would.
func waitAsync(tm *Timer) <-chan error {
	done := make(chan error, 1)
	go func() { done <- tm.Wait() }()
	return done
}

func TestWaitNeverReturnsBeforeTheArmedTime(t *testing.T) {
	t.Parallel()
	tm := newTimer(t)
	for _, d := range []time.Duration{50 * time.Microsecond, time.Millisecond, 3 * time.Millisecond} {
		start := time.Now()
		if err := tm.Reset(d); err != nil {
			t.Fatal(err)
		}
		if err := tm.Wait(); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < d {
			t.Fatalf("Reset(%v): Wait returned after %v", d, elapsed)
		}
	}
}

func TestNonPositiveDurationStillFires(t *testing.T) {
	t.Parallel()
	tm := newTimer(t)
	for _, d := range []time.Duration{0, -time.Second} {
		if err := tm.Reset(d); err != nil {
			t.Fatal(err)
		}
		if err := tm.Wait(); err != nil {
			t.Fatalf("Reset(%v): %v", d, err)
		}
	}
}

func TestResetWhileParkedRearmsTheWaiter(t *testing.T) {
	t.Parallel()
	tm := newTimer(t)
	if err := tm.Reset(time.Hour); err != nil {
		t.Fatal(err)
	}
	done := waitAsync(tm)
	if err := tm.Reset(time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Wait still parked after a Reset to 1ms")
	}
}

func TestStopKeepsTheWaiterParkedAndCloseWakesIt(t *testing.T) {
	t.Parallel()
	tm := newTimer(t)
	if err := tm.Reset(time.Hour); err != nil {
		t.Fatal(err)
	}
	done := waitAsync(tm)
	if err := tm.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("Wait returned %v after Stop", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := tm.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Wait after Close = %v, want ErrClosed", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Close did not wake a parked Wait")
	}
}

func TestClosedTimerReportsErrClosed(t *testing.T) {
	t.Parallel()
	tm, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := tm.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tm.Reset(time.Millisecond); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reset after Close = %v, want ErrClosed", err)
	}
	if err := tm.Stop(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Stop after Close = %v, want ErrClosed", err)
	}
	if err := tm.Wait(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Wait after Close = %v, want ErrClosed", err)
	}
}
