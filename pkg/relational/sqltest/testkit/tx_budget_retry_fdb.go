package testkit

// The whole-transaction MVCC budget, forced deterministically, and the retry an
// explicit transaction needs because of it.
//
// THE ASSUMPTION THIS IS ABOUT. An explicit SQL transaction pins one FDB read
// version, and that version dies five seconds after it was obtained — in WALL
// CLOCK, whether the client was working or merely queued behind other load. The
// driver pre-empts at four (txPageTimeLimit) so the caller gets a clean 40001
// naming the remedy instead of FDB's raw 1007 mid-scan. Every test that opens a
// transaction and issues more than one statement therefore carries an unstated
// assumption: that its statements complete inside four seconds of wall time.
//
// That assumption is REACHABLE, not theoretical. TestFDB_RFC198_ReadYourWrites-
// ThroughIndex takes 0.08s standalone and was MEASURED at 5.34s in a captured
// full-parallel suite run — the suite runs up to GOMAXPROCS tests at once
// against one containerised FDB, and the reported failure was `read version
// 4.321s old, budget 4s`, the same order.
//
// WHY NO PRODUCTION KNOB WAS ADDED. The budget is already injectable: the
// elapsed comparison is `env.Since(instant)` on the record layer's dst.Env
// clock, and RegisterBackend is an exported seam for binding a database built
// with a chosen Env to a cluster_file key. So the condition is forced by moving
// the CLOCK, exactly as sim_tx_budget_midpage_test.go does — the house
// mechanism — rather than by making a production constant mutable. A knob added
// only so a test can turn it is a knob production has to carry forever.
//
// The clock here is a wall clock plus an offset, NOT a sim clock, and that is
// load-bearing: on a real backend the read-version instant comes from the pure-Go
// client, which stamps it with time.Now() (pkg/fdbgo/client/grv.go — no dst usage
// in grv.go or transaction.go at all). Pinning an Epoch-based sim clock against a
// real backend would measure the gap between two unrelated epochs, which is the
// hazard dst/env.go:57 warns about. An offset preserves the epoch and moves only
// the distance.

import (
	"sync/atomic"
	"time"
)

// lateClock is a wall clock that can be made to report a fixed amount of extra
// elapsed time — a load spike, modelled as a clock that has run ahead.
//
// ONE-SHOT by default, which is the point rather than a convenience: a spike
// that never ends is a different scenario (covered separately below) and cannot
// distinguish "the retry works" from "the retry is missing", because both end
// red. Disarm is called by the retry's observer hook, so the injected fault
// lasts exactly one attempt — the same lifetime as the chaos harness's
// InjectOnce.
type lateClock struct {
	lateBy time.Duration
	armed  atomic.Bool
}

func NewLateClock(lateBy time.Duration) *lateClock {
	c := &lateClock{lateBy: lateBy}
	c.armed.Store(true)
	return c
}

func (c *lateClock) Now() time.Time {
	if c.armed.Load() {
		return time.Now().Add(c.lateBy)
	}
	return time.Now()
}

// Disarm ends the spike. Safe to call repeatedly.
func (c *lateClock) Disarm() { c.armed.Store(false) }

// Rearm re-injects the spike for the NEXT transaction. A test with several
// independent explicit transactions — subtests, typically — needs each of them
// to meet the condition; without this the first one consumes the one-shot and
// every later transaction runs clean, so its retry would be a permanently
// untested arm while the test reported green.
func (c *lateClock) Rearm() { c.armed.Store(true) }
