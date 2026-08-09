package transport

import (
	"context"
	"testing"
	"time"
)

// The connection monitor must WAIT once per cycle on every path, including the
// idle one where it declines to PING.
//
// This is a regression pin for a real defect, not a hypothetical. C++'s first
// `delay(CONNECTION_MONITOR_LOOP_TIME)` (FlowTransport.actor.cpp:651) is inside
// a `!isClient()` block, so removing Go's port of it is correct — but Go's idle
// branch `continue`s where C++ either throws or falls through to the PING, so
// with the leading delay gone and the remaining delay on the ping path only,
// an idle connection spun with no wait at all.
//
// The failure mode is why this test asserts a RATE rather than an outcome: the
// spin did not fail an assertion anywhere, it hung `//pkg/fdbgo/transport` at
// the target's 300-second timeout. A timeout is not a regression test — it
// names no property, points at no line, and any future change that merely makes
// the spin slower would "fix" it.
func TestMonitor_IdleConnectionWaitsEachCycle(t *testing.T) {
	t.Parallel()

	s := newSimServer(t)
	go func() {
		if err := s.handshake(); err != nil {
			return
		}
		s.drainUntilClosed()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const (
		interval = 20 * time.Millisecond
		observe  = 300 * time.Millisecond
	)
	// No traffic is sent, so `pending` stays empty and the monitor takes the
	// idle branch on every cycle — the exact path that lacked a wait.
	c, err := dialWith(ctx, "sim", s.dialFunc(), nil, withMonitorCadence(interval, time.Hour))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	start := c.monitorCycles.Load()
	time.Sleep(observe)
	cycles := c.monitorCycles.Load() - start

	// Jitter is [0.9, 1.1) of the interval, so the expected count over the
	// window is observe/interval within about ±11%. The ceiling is deliberately
	// loose — this is a spin detector, and a spinning loop overshoots by orders
	// of magnitude, not by a few percent. A tight bound here would only buy
	// flakiness on a loaded machine.
	expected := int64(observe / interval)
	if cycles > expected*4 {
		t.Fatalf("monitor ran %d cycles in %v at a %v interval (expected ~%d): "+
			"the idle path is not waiting, so the loop is spinning. Every path "+
			"through connectionMonitor must reach exactly one delay per cycle.",
			cycles, observe, interval, expected)
	}
	// The floor guards the other direction, and it is the one that fails
	// silently: a monitor that never runs also never spins, so without this the
	// assertion above would pass on a loop that had stopped entirely.
	if cycles == 0 {
		t.Fatalf("monitor ran 0 cycles in %v at a %v interval — it is not "+
			"running at all, which would satisfy the spin check above while "+
			"leaving dead connections undetected", observe, interval)
	}
}

// The jitter port must match flow's delayJittered (flow.h:1481):
//
//	seconds * (DELAY_JITTER_OFFSET + DELAY_JITTER_RANGE * random01())
//
// with Knobs.cpp:54-55 giving offset 0.9 and range 0.2, i.e. uniform over
// [0.9d, 1.1d). Asserted directly because the monitor's timing is otherwise
// only observable as a rate, where a wrong constant hides inside the tolerance.
func TestJitteredDelayMatchesFlowRange(t *testing.T) {
	t.Parallel()

	const d = time.Second
	low := time.Duration(float64(d) * delayJitterOffset)
	high := time.Duration(float64(d) * (delayJitterOffset + delayJitterRange))

	var sawBelowNominal, sawAboveNominal bool
	for i := 0; i < 2000; i++ {
		got := jitteredDelay(d)
		if got < low || got >= high {
			t.Fatalf("jitteredDelay(%v) = %v, outside [%v, %v)", d, got, low, high)
		}
		if got < d {
			sawBelowNominal = true
		}
		if got > d {
			sawAboveNominal = true
		}
	}
	// Without this, a jitteredDelay that ignored its argument and returned a
	// constant inside the range would pass every check above.
	if !sawBelowNominal || !sawAboveNominal {
		t.Fatalf("jitter never landed on both sides of %v (below=%v above=%v); "+
			"the delay is not actually being randomized", d, sawBelowNominal, sawAboveNominal)
	}
}
