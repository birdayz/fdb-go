package client

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// AN IN-BAND maybeDelivered MUST NOT TOUCH THE FAILURE MONITOR OR THE POOL.
//
// This regression was found by reading, not by the suite: the first version of
// the in-band arm called handleConnError, and restoring it left 874 tests
// passing. Nothing drove sendGRVRequest with an in-band 1100 at all, so a defect
// that manufactures commit_unknown_result for an UNRELATED transaction reddened
// nothing.
//
// Why that defect is worse than it sounds: the connection is HEALTHY -- a
// well-formed reply just arrived on it -- and connPool is keyed by ADDRESS. A
// single-process FDB collocates GrvProxy and CommitProxy on one connection, so
// evicting it for a dying GrvProxy pushes an in-flight COMMIT into resp.Err,
// which the commit path maps to commit_unknown_result. C++ does not do this:
// basicLoadBalance's maybeDelivered arm touches neither IFailureMonitor nor the
// connection (LoadBalance.actor.h:823-833).
//
// NOT covered here, stated first because the positive claim is narrow:
//   - the commit arm. Its equivalent conversion is pinned only at the
//     classifier and parse level (inband_reply_error_test.go); driving a real
//     in-band 1100 through commitAttempt needs a commit-proxy intercept this
//     harness does not have.
//   - the next-alternative BEHAVIOUR. With one proxy in the sim there is no
//     other alternative to observe, so what is pinned is that the request
//     survives and the address is untouched -- not that a second proxy was
//     tried.
//   - kickTopology, which neither arm calls.
func TestInBandMaybeDeliveredLeavesGRVProxyUntouched(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	b := db.db.grvBatchers[grvBatcherDefault]
	// Warm: learn the proxies and establish the connection the intercept arms.
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after a successful GRV")
	}
	addr := proxies[0].Address

	// Vacuity: the address must be healthy and pooled BEFORE the injection, or
	// the assertions afterwards hold for the wrong reason.
	if db.db.failMon.isFailed(addr) {
		t.Fatalf("%s already failed before injection; the assertion below would be vacuous", addr)
	}
	db.db.connMu.RLock()
	_, pooledBefore := db.db.connPool[addr]
	db.db.connMu.RUnlock()
	if !pooledBefore {
		t.Fatalf("%s not in connPool before injection; nothing to evict, so the "+
			"eviction assertion would pass for the wrong reason", addr)
	}
	// The DIAL COUNT is the observable that survives the retry. Asserting on
	// connPool membership afterwards does not work and was tried: the retry
	// re-dials and re-pools the address, and markAlive clears the failure, so
	// the end state is identical either way. Verified by mutation -- with
	// handleConnError restored, the membership assertions PASSED. A dial the
	// address did not need is the durable trace of a teardown that should not
	// have happened.
	sd.mu.Lock()
	dialsBefore := len(sd.conns[addr])
	sd.mu.Unlock()

	// Inject an in-band broken_promise on the NEXT GRV reply only: this is the
	// shape a dying proxy sends -- an error inside the ErrorOr, on a live
	// connection -- which reaches parseGetReadVersionReply and never resp.Err.
	// Inject ONCE on the next armed reply, with a counter -- replaceFirst keys
	// on per-conn frame index 0, which the warm-up above has already consumed,
	// so it would silently never fire. The counter is the anti-vacuity proof
	// that the fault actually landed.
	var injected atomic.Int64
	grvErrBody := (&types.ErrorOrError{ErrorCode: ErrBrokenPromise}).MarshalFDB()
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		// Discriminate by the ErrorOr<T> ENVELOPE fileID at body[4:8], not by
		// "does it parse as a GRV reply". Two weaker versions were tried and both
		// injected into the wrong frame:
		//
		//   - keying on per-conn frame index 0, which the warm-up above has
		//     already consumed, so nothing was replaced at all;
		//   - keying on parseGetReadVersionReply returning nil, which is NOT a GRV
		//     discriminator: ReadErrorOrInto deliberately ignores fileIDs, so a
		//     GetValue or GetKeyValues success body parses cleanly too. On a
		//     single-process container these share this connection, so the
		//     intercept could replace an unrelated reply, tick the counter, and
		//     let the real GRV succeed untouched -- green without ever reaching
		//     the arm under test.
		//
		// The fileID is stamped at offset 4 by writer_direct.go and is the same
		// discriminator cycle_workload_test.go uses for the same reason.
		//
		// It measurably rejects: instrumented, this run sees 3 frames and rejects
		// 1 -- the co-located coordinator reply. The parse-based predicate it
		// replaced rejected 0 of the same 3, and was saved only by the injection
		// happening to be consumed before that frame arrived.
		if len(body) < 8 || binary.LittleEndian.Uint32(body[4:8]) != grvReplyEnvelopeFileID {
			return body, false
		}
		if injected.CompareAndSwap(0, 1) {
			return grvErrBody, false
		}
		return body, false
	})
	sd.armAddr(addr)

	// The arm-hit counter is what proves the ARM ran. The injection counter
	// above proves only that a FRAME was replaced, which is a weaker claim and
	// fails open: a well-formed ErrorOr<T> from any co-located role decodes as a
	// GRV reply, so an intercept keyed on decodability can replace the wrong
	// frame, tick its counter, and let the real GRV succeed untouched.
	armBefore := inBandMaybeDeliveredGRV.Load()
	db.db.grvCache.invalidate() // force a real round trip, not a cache hit
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false); err != nil {
		t.Fatalf("GRV returned %v; an in-band 1100 must be absorbed and retried, "+
			"not surfaced -- 1100 is retryable under none of the three predicates, "+
			"so surfacing it fails the caller's transaction terminally", err)
	}

	if injected.Load() == 0 {
		t.Fatal("the intercept never fired: no in-band 1100 was injected, so every " +
			"assertion below would hold for a run that exercised nothing")
	}
	if inBandMaybeDeliveredGRV.Load() == armBefore {
		t.Fatal("a frame was replaced but the in-band arm never ran: the injection " +
			"landed on something other than a GRV reply, so nothing below tests the " +
			"code path this exists for")
	}

	// The two things the removed handleConnError did, asserted separately so a
	// failure says which one came back.
	if db.db.failMon.isFailed(addr) {
		t.Errorf("%s was marked failed by an in-band maybeDelivered; "+
			"basicLoadBalance touches no failure monitor here, and marking it "+
			"re-arms the Warn latch and closes the shared recovered channel that "+
			"all grvBatchers wait on", addr)
	}
	sd.mu.Lock()
	dialsAfter := len(sd.conns[addr])
	sd.mu.Unlock()
	if dialsAfter != dialsBefore {
		t.Errorf("the connection to %s was re-dialled (%d -> %d): an in-band "+
			"maybeDelivered tore down a HEALTHY connection -- a well-formed reply "+
			"had just arrived on it. connPool is keyed by ADDRESS, so that fans "+
			"1030 out to every unrelated in-flight RPC on the same connection, "+
			"turning a concurrent commit into commit_unknown_result",
			addr, dialsBefore, dialsAfter)
	}
}

// AN IN-BAND maybeDelivered MUST STILL CLEAR A PRE-EXISTING ADDRESS FAILURE.
//
// WHAT THIS DOES NOT PIN, first, because the obvious reading is wrong: it does
// NOT pin WHERE markAlive sits. Moving it below the disposition check leaves
// this test green -- measured. The address is shared with the coordinator in
// this sim, and a topology reply on the same address marks it alive too, so no
// assertion on failure-monitor state can attribute the alive edge to the GRV
// arm specifically. The argument for keeping markAlive above the check is
// therefore reasoning, recorded at the call site, not something this harness
// can distinguish.
//
// What it DOES pin: the GRV path clears a pre-existing address failure at all.
// Deleting markAlive from that path reddens this test -- also measured. That is
// the property worth a sentinel, because the failure monitor is keyed by
// ADDRESS and a stuck failure excludes every co-located role.
//
// It starts from a FAILED address, the state the GRV-timeout arm leaves behind
// (it marks the address failed and deliberately keeps the pooled connection).
//
// The failure monitor is keyed by ADDRESS, not by role. A well-formed frame
// therefore proves the ADDRESS is reachable whatever the reply says, and the
// alive edge is what wakes recovery waiters and lifts storage-server exclusion
// for every role co-located there. If the in-band arm continues before reaching
// markAlive, a healthy co-located role stays excluded and the waiters are never
// signalled -- which is why markAlive belongs ABOVE the disposition check.
func TestInBandMaybeDeliveredClearsAPreExistingAddressFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	b := db.db.grvBatchers[grvBatcherDefault]
	if _, _, _, err := b.getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after a successful GRV")
	}
	addr := proxies[0].Address

	// The precondition this test exists for, and the one the sibling cannot
	// create: the address is ALREADY failed when the in-band 1100 arrives.
	db.db.failMon.markFailed(addr)
	if !db.db.failMon.isFailed(addr) {
		t.Fatalf("could not put %s into the failed state; the assertion below "+
			"would pass without ever testing a recovery", addr)
	}

	var injected atomic.Int64
	grvErrBody := (&types.ErrorOrError{ErrorCode: ErrBrokenPromise}).MarshalFDB()
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		if len(body) < 8 || binary.LittleEndian.Uint32(body[4:8]) != grvReplyEnvelopeFileID {
			return body, false
		}
		// EVERY GRV reply, not just the first. The position of markAlive is only
		// observable while the in-band arm is the ONLY thing happening: let the
		// GRV succeed and the successful reply marks the address alive from
		// either position, so the end states are identical -- measured, this
		// test passed with markAlive moved below the check until it injected
		// persistently.
		injected.Add(1)
		return grvErrBody, false
	})
	sd.armAddr(addr)

	// The arm-hit counter is what proves the ARM ran. The injection counter
	// above proves only that a FRAME was replaced, which is a weaker claim and
	// fails open: a well-formed ErrorOr<T> from any co-located role decodes as a
	// GRV reply, so an intercept keyed on decodability can replace the wrong
	// frame, tick its counter, and let the real GRV succeed untouched.
	armBefore := inBandMaybeDeliveredGRV.Load()
	db.db.grvCache.invalidate()
	// Bounded, and expected to EXPIRE: with every reply carrying an in-band
	// 1100 the GRV can never complete, which is exactly the window in which the
	// two markAlive positions differ.
	grvCtx, grvCancel := context.WithTimeout(ctx, 3*time.Second)
	defer grvCancel()
	if _, _, _, err := b.getReadVersion(db.db, grvCtx, grvPriorityDefault, types.SpanContext{}, nil, false, false); err == nil {
		t.Fatal("GRV succeeded although every reply carried an in-band 1100; the " +
			"injection is not reaching the path under test")
	}
	if injected.Load() == 0 {
		t.Fatal("the intercept never replaced a GRV reply; the assertion below " +
			"would hold for a run that injected nothing")
	}
	if inBandMaybeDeliveredGRV.Load() == armBefore {
		t.Fatal("a frame was replaced but the in-band arm never ran; the assertion " +
			"below would hold for a run that never reached the code under test")
	}

	if db.db.failMon.isFailed(addr) {
		t.Errorf("%s is still marked failed after well-formed frames arrived from "+
			"it. The GRV path must clear an address failure on frame receipt: the "+
			"monitor is keyed by ADDRESS, so a stuck failure excludes every role "+
			"co-located there and leaves recovery waiters asleep", addr)
	}
}
