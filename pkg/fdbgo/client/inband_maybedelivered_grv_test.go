package client

import (
	"context"
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
		// CONTENT discrimination, not "first armed frame": the armed connection
		// carries more than GRV replies, and replacing an arbitrary frame injects
		// nothing the GRV path will ever parse. Measured -- an index-keyed
		// intercept fired (counter 1) while the arm under test never ran, and the
		// mutation test passed as a result. Replace only a frame that currently
		// parses AS a GRV reply, and replace it with the ErrorOr<> envelope a
		// dying proxy actually sends.
		if _, _, _, _, _, _, err := parseGetReadVersionReply(body); err != nil {
			return body, false
		}
		if injected.CompareAndSwap(0, 1) {
			return grvErrBody, false
		}
		return body, false
	})
	sd.armAddr(addr)

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
