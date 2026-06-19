package client

// Cycle workload — a pure-client serializability oracle.
//
// Go port of FoundationDB's fdbserver/workloads/Cycle.actor.cpp @ tag 7.3.75. nodeCount keys form
// a single directed Hamiltonian cycle key(n) -> value((n+1) % nodeCount). Each transaction reads a
// 4-node chain r -> r2 -> r3 -> r4 and transposes r2/r3 by writing key(r)=r3, key(r2)=r4,
// key(r3)=r2 — a move whose serial composition preserves exactly one Hamiltonian cycle, and which
// under a non-serializable interleave corrupts the ring (splits it / orphans a node). The check
// walks the ring at a single read version and asserts it is still one cycle of length nodeCount.
// Only serializable isolation keeps the ring intact under concurrent swaps; a broken ring is a
// serializability violation. This drives the pure-Go client against a real FDB testcontainer — the
// real cluster's conflict detection is the chaos source, exactly as the C++ workload relies on it.
//
// Encoding note: Cycle.actor.cpp encodes keys/values as %016llx of IEEE-754 double bits
// (doubleToTestKey, tester.actor.cpp:82). key(n) uses the fraction n/nodeCount, value(n) the int n.
// Those keys are test-internal (own prefix, never shared with Java/C), so they are NOT wire-compat
// data. This port uses a clean order-preserving encoding instead — %016x of the node index for keys
// (dense + index-sorted, so the range read returns nodes in index order) and decimal for values —
// and ports the *invariant* (the check, cycleCheckData) 1:1. The bit-cast-double encoding carries
// no invariant semantics; only (dense, ordered range) + (value -> int round-trip) matter.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire/types"
)

type cycleWorkload struct {
	nodeCount int
	prefix    []byte
}

// key(n) — %016x of the node index, prefixed. Dense and index-sorted for n in [0, nodeCount).
func (w *cycleWorkload) key(n int) []byte {
	return []byte(fmt.Sprintf("%s%016x", w.prefix, n))
}

// value(n) — decimal node index; fromValue is its inverse.
func (w *cycleWorkload) value(n int) []byte {
	return []byte(strconv.Itoa(n))
}

// fromValue decodes a stored value back to a node index. A nil value is the badRead SevError analog
// (Cycle.actor.cpp:136-142, :173-182): a key that should exist read as absent. A malformed value is
// the non-integral arm of the "Invalid value" check (Cycle.actor.cpp:269, the C++ `i != d` test).
func (w *cycleWorkload) fromValue(v []byte) (int, error) {
	if v == nil {
		return 0, fmt.Errorf("cycle bad read: missing value")
	}
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return 0, fmt.Errorf("invalid value %q: %w", v, err)
	}
	return n, nil
}

// prefixEnd is the exclusive upper bound covering every key(n) (the full %016x key band). Hex
// digits are all < 0xff, so prefix+0xff sorts strictly after every key under the prefix.
func (w *cycleWorkload) prefixEnd() []byte {
	return append(append([]byte{}, w.prefix...), 0xff)
}

// setup writes the initial ring key(n) -> value((n+1) % nodeCount) for all n. Chunked into bounded
// transactions to stay well under the 10MB / 5s transaction limits.
func (w *cycleWorkload) setup(ctx context.Context, db *Database) error {
	const chunk = 200
	for start := 0; start < w.nodeCount; start += chunk {
		end := start + chunk
		if end > w.nodeCount {
			end = w.nodeCount
		}
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			for n := start; n < end; n++ {
				tx.Set(w.key(n), w.value((n+1)%w.nodeCount))
			}
			return nil, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// swapOnce runs one swap transaction starting at node r, via db.Transact's retry loop (the faithful
// analog of the C++ unconditional onError(e) at Cycle.actor.cpp:205 — retryable FDB errors are
// retried with backoff). A missing read inside the swap is a hard error (the badRead SevError), not
// swallowed as transient.
func (w *cycleWorkload) swapOnce(ctx context.Context, db *Database, r int) error {
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Reverse next and next^2 node: read the chain r -> r2 -> r3 -> r4.
		v, err := tx.Get(ctx, w.key(r))
		if err != nil {
			return nil, err
		}
		r2, err := w.fromValue(v)
		if err != nil {
			return nil, err
		}
		v2, err := tx.Get(ctx, w.key(r2))
		if err != nil {
			return nil, err
		}
		r3, err := w.fromValue(v2)
		if err != nil {
			return nil, err
		}
		v3, err := tx.Get(ctx, w.key(r3))
		if err != nil {
			return nil, err
		}
		r4, err := w.fromValue(v3)
		if err != nil {
			return nil, err
		}

		// Range clear over [key(r), key(r)+" end") — immediately overwritten by the set below.
		// Load-bearing mutation-ordering coverage (Cycle.actor.cpp:187-189: "Shouldn't have an
		// effect, but will break with wrong ordering"). ClearRange adds a [begin,end) write-conflict
		// range by default, matching the C++ AddConflictRange::True.
		kr := w.key(r)
		krEnd := append(append([]byte{}, kr...), []byte(" end")...)
		if err := tx.ClearRange(kr, krEnd); err != nil {
			return nil, err
		}

		// The swap: transpose r2 and r3. Before: r -> r2 -> r3 -> r4. After: r -> r3 -> r2 -> r4.
		tx.Set(kr, w.value(r3))
		tx.Set(w.key(r2), w.value(r4))
		tx.Set(w.key(r3), w.value(r2))
		return nil, nil
	})
	return err
}

// check reads the whole ring at a single read version (one db.Transact == one consistent snapshot,
// faithful to the C++ single getReadVersion read at Cycle.actor.cpp:316-319) and verifies it is one
// Hamiltonian cycle. The Go single-process port is the C++ clientId==0 checker (:309) by
// construction — call check once.
func (w *cycleWorkload) check(ctx context.Context, db *Database) error {
	var data []KeyValue
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// limit nodeCount+1 so an over-count (extra keys) is observable as len > nodeCount.
		kvs, _, err := tx.GetRange(ctx, w.prefix, w.prefixEnd(), w.nodeCount+1)
		if err != nil {
			return nil, err
		}
		data = kvs
		return nil, nil
	}); err != nil {
		return err
	}
	return w.checkData(data)
}

// checkData is a 1:1 port of cycleCheckData (Cycle.actor.cpp:230-293). It walks the in-memory range
// snapshot from index 0, following i = fromValue(data[i].value) exactly nodeCount times, and
// requires the data to be exactly one Hamiltonian cycle over all nodes. data must be the
// index-sorted dense range (data[i] is node i), as returned by the range read.
func (w *cycleWorkload) checkData(data []KeyValue) error {
	if len(data) != w.nodeCount {
		return fmt.Errorf("node count changed: before=%d after=%d", w.nodeCount, len(data))
	}
	i := 0
	for c := 0; c < w.nodeCount; c++ {
		if c != 0 && i == 0 {
			return fmt.Errorf("cycle got shorter: returned to 0 after %d of %d steps", c, w.nodeCount)
		}
		if !bytes.Equal(data[i].Key, w.key(i)) {
			return fmt.Errorf("key changed at index %d: got %q want %q", i, data[i].Key, w.key(i))
		}
		next, err := w.fromValue(data[i].Value)
		if err != nil {
			return fmt.Errorf("invalid value at index %d: %w", i, err)
		}
		if next < 0 || next >= w.nodeCount {
			return fmt.Errorf("invalid value at index %d: %d out of [0,%d)", i, next, w.nodeCount)
		}
		i = next
	}
	if i != 0 {
		return fmt.Errorf("cycle got longer: did not return to 0 after %d steps (i=%d)", w.nodeCount, i)
	}
	return nil
}

// TestCycle_SerializableUnderConcurrency is the happy-path oracle: concurrent swap transactions on a
// real FDB cluster must keep the ring a single Hamiltonian cycle. The whole concurrent phase is
// bounded by a context timeout so a conflict-livelock fails fast rather than hanging.
func TestCycle_SerializableUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	w := &cycleWorkload{nodeCount: 1000, prefix: []byte("cycle_serial_")}
	if err := w.setup(ctx, db); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := w.check(ctx, db); err != nil {
		t.Fatalf("ring invalid right after setup: %v", err)
	}

	const actors = 16
	workCtx, workCancel := context.WithTimeout(ctx, 20*time.Second)
	defer workCancel()

	var committed atomic.Int64
	var wg sync.WaitGroup
	for a := 0; a < actors; a++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for workCtx.Err() == nil {
				r := rng.Intn(w.nodeCount)
				err := w.swapOnce(workCtx, db, r)
				switch {
				case err == nil:
					committed.Add(1)
				case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
					return // workload window closed; not a failure
				default:
					// A real failure (badRead / invalid value / non-retryable error the Transact
					// loop surfaced). Keyed on error IDENTITY, not the clock, so a genuine error
					// is never masked by a simultaneously-firing deadline. (No per-tx timeout is
					// set here, so a window-close always surfaces as a raw context error, never a
					// mapTimeout-converted FDBError — see mapTimeout, transaction.go.)
					t.Errorf("swap failed: %v", err)
					return
				}
			}
		}(int64(a) + 1)
	}
	wg.Wait()

	// Anti-vacuity (non-timing-dependent): work actually happened. Not a rate assertion.
	if committed.Load() == 0 {
		t.Fatalf("no swaps committed — workload was vacuous")
	}
	t.Logf("committed %d swaps across %d actors", committed.Load(), actors)

	// The ring must still be exactly one Hamiltonian cycle. Check a few times (determinism).
	for i := 0; i < 3; i++ {
		if err := w.check(ctx, db); err != nil {
			t.Fatalf("serializability violated (check %d): %v", i, err)
		}
	}
}

// TestCycle_DetectsBrokenRing is the FDB revert-proof (the teeth): the oracle must catch a broken
// ring on real FDB, not merely pass the happy path. We deterministically install the kind of
// corrupted ring a lost-isolation interleave would produce (redirecting one pointer to skip a node,
// orphaning it) — avoiding a flaky concurrency-timing repro — and assert check goes red.
func TestCycle_DetectsBrokenRing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	w := &cycleWorkload{nodeCount: 50, prefix: []byte("cycle_broken_")}
	if err := w.setup(ctx, db); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := w.check(ctx, db); err != nil {
		t.Fatalf("ring should be valid after setup: %v", err)
	}

	// Redirect key(0): 0 -> 2 instead of 0 -> 1. Node 1 is orphaned and the walk returns to 0
	// before visiting all N nodes. A serializable swap would never leave the ring like this.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(w.key(0), w.value(2))
		return nil, nil
	}); err != nil {
		t.Fatalf("install corruption: %v", err)
	}

	// Assert the SPECIFIC failure (not just non-nil): redirecting key(0):0→2 orphans node 1, so
	// the walk returns to 0 after N-1 steps → "cycle got shorter". Pinning the message guards
	// against a future bug where check returns the WRONG failure (e.g. a dropped key reads as
	// "node count changed") yet still passes a bare non-nil assertion.
	err := w.check(ctx, db)
	if err == nil {
		t.Fatalf("check FAILED to detect a broken ring — the oracle has no teeth")
	}
	if !strings.Contains(err.Error(), "cycle got shorter") {
		t.Fatalf("expected 'cycle got shorter', got: %v", err)
	}
	t.Logf("broken ring correctly detected: %v", err)
}

// TestCycle_CheckData_FailureModes pins each cycleCheckData failure mode 1:1, deterministically and
// without FDB. This is the regression that survives even if the FDB revert-proof above is ever
// stressed. N=4 nodes; rings are described by their successor map.
func TestCycle_CheckData_FailureModes(t *testing.T) {
	t.Parallel()
	// In-memory only (checkData never touches FDB), but a descriptive prefix self-documents.
	w := &cycleWorkload{nodeCount: 4, prefix: []byte("cycle_unit_")}

	// ring builds a dense, index-sorted data slice from a successor list (succ[i] = node i's next).
	ring := func(succ ...int) []KeyValue {
		kvs := make([]KeyValue, len(succ))
		for i, s := range succ {
			kvs[i] = KeyValue{Key: w.key(i), Value: w.value(s)}
		}
		return kvs
	}

	tests := []struct {
		name    string
		data    []KeyValue
		wantErr string // "" = expect success
		mutate  func([]KeyValue)
	}{
		{name: "valid single cycle", data: ring(1, 2, 3, 0)},
		{name: "node count changed (fewer)", data: ring(1, 2, 0), wantErr: "node count changed"},
		{
			name:    "node count changed (more)",
			data:    append(ring(1, 2, 3, 0), KeyValue{Key: w.key(4), Value: w.value(0)}),
			wantErr: "node count changed",
		},
		{name: "cycle got shorter", data: ring(1, 0, 3, 2), wantErr: "cycle got shorter"},
		{name: "cycle got longer", data: ring(1, 2, 3, 1), wantErr: "cycle got longer"},
		{name: "invalid value out of range", data: ring(1, 2, 3, 7), wantErr: "out of [0,4)"},
		{
			name:    "key changed",
			data:    ring(1, 2, 3, 0),
			mutate:  func(d []KeyValue) { d[1].Key = w.key(99) },
			wantErr: "key changed",
		},
		{
			name:    "invalid value malformed (non-integral)",
			data:    ring(1, 2, 3, 0),
			mutate:  func(d []KeyValue) { d[0].Value = []byte("not-a-number") },
			wantErr: "invalid value",
		},
		{
			name:    "missing read mid-walk (badRead analog)",
			data:    ring(1, 2, 3, 0),
			mutate:  func(d []KeyValue) { d[1].Value = nil },
			wantErr: "missing value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := tc.data
			if tc.mutate != nil {
				tc.mutate(data)
			}
			err := w.checkData(data)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid ring, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.wantErr)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// --- Cycle under injected wire faults (RFC-120) -----------------------------------------------

// everyNthInlineReadError returns a frameIntercept (SimTransport, RFC-118) that rewrites every n-th
// storage READ reply into a retryable inline error, passing every other reply — commit (CommitID),
// GRV (GetReadVersionReply), locate, and any unrecognized frame — through verbatim.
//
// Content-discrimination is mandatory here, not address-scoping: the single-process testcontainer
// collocates storage / commit-proxy / GRV on ONE connection, so the fault must be scoped by reply
// CONTENT (the inner reply fileID at body[4:8]), or it would corrupt a CommitID/GetReadVersionReply
// reply and break the workload for a non-bug reason (RFC-120 §3.2; the FDB-C-dev NAK on v1). counter
// advances only on an actual injection — the anti-vacuity proof the fault fired.
func everyNthInlineReadError(n int, code uint16, counter *atomic.Int64) frameIntercept {
	if n <= 0 {
		panic("everyNthInlineReadError: n must be > 0")
	}
	var readSeen atomic.Int64 // read replies observed (shared across conns; aggregate ~1/n)
	// The per-conn idx is unused on purpose: readSeen is the GLOBAL read-reply count across the
	// collocated conns, so targeting doesn't depend on connection fan-out.
	return func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		if !isReadReplyBody(body) {
			return body, false // commit / GRV / locate / unknown → verbatim
		}
		if readSeen.Add(1)%int64(n) == 0 {
			counter.Add(1)
			return inlineErrorReply(code, 0), false // retryable inline error on this read reply
		}
		return body, false
	}
}

// composedErrorOr2 is the high bits FDB's ComposedIdentifier<T, 2> ORs into a fileID. A load-balanced
// storage read reply travels the wire as ErrorOr<T> and carries the COMPOSED envelope fileID
// (2<<24)|T_fileID — NOT T's own fileID (C++ flow.h:137 `class ErrorOr : ComposedIdentifier<T,2>`;
// FileIdentifier.h:79 `file_identifier = (B << 24) | FileIdentifierFor<T>`). The harness's
// MarshalErrorOr* stamps the INNER fileID as a placeholder (erroror.go:295: "NOT the per-RPC fileID
// the real server would send … ReadErrorOrInto does not validate the fileID"), so the discriminator
// must match the COMPOSED envelope the real server actually sends — verified empirically: a real
// GetValueReply read arrives as 0x2150A71 = (2<<24)|GetValueReplyFileID(0x150A71).
const composedErrorOr2 = uint32(2) << 24

const (
	getValueReplyEnvelopeFileID     = composedErrorOr2 | types.GetValueReplyFileID
	getKeyReplyEnvelopeFileID       = composedErrorOr2 | types.GetKeyReplyFileID
	getKeyValuesReplyEnvelopeFileID = composedErrorOr2 | types.GetKeyValuesReplyFileID

	// Control-plane reply envelopes that share the storage connection (single-process container) and
	// must pass through untouched — derived from the same ComposedIdentifier<T,2> rule, so they are
	// correct by construction (not magic) and provably disjoint from the read set above.
	commitReplyEnvelopeFileID = composedErrorOr2 | types.CommitIDFileID
	grvReplyEnvelopeFileID    = composedErrorOr2 | types.GetReadVersionReplyFileID
)

// isReadReplyBody reports whether a reply body is a storage READ reply (GetValue/GetKey/GetKeyValues),
// by the ErrorOr<T> envelope fileID at body[4:8] (writer_direct.go stamps the fileID at offset 4).
// Fail-safe: a runt / non-flatbuffer body (<8 bytes) is treated as not-a-read-reply (passed verbatim),
// never a panic.
func isReadReplyBody(body []byte) bool {
	if len(body) < 8 {
		return false
	}
	switch binary.LittleEndian.Uint32(body[4:8]) {
	case getValueReplyEnvelopeFileID, getKeyReplyEnvelopeFileID, getKeyValuesReplyEnvelopeFileID:
		return true
	}
	return false
}

// TestEveryNthInlineReadError is the determinism floor (no FDB) for the fault intercept. It pins:
// (a) every n-th READ reply is rewritten, the others pass verbatim; (b) commit (CommitID) and GRV
// (GetReadVersionReply) frames pass through UNTOUCHED and do NOT advance the read counter — the
// exact control-plane-passthrough dimension whose absence was the v1 NAK; (c) a runt (<8B) frame is
// passed verbatim with no panic; (d) the injection counter advances only on actual faults.
func TestEveryNthInlineReadError(t *testing.T) {
	t.Parallel()

	// frameWith builds a minimal body carrying fileID at body[4:8] — the only bytes the discriminator
	// inspects. Read frames carry the ErrorOr<T> COMPOSED envelope fileID (what the real server sends);
	// the control-plane frames carry the real commit/GRV composed envelopes (derived identically), so
	// the passthrough assertion is a faithful regression of the v1 NAK, not a synthetic stand-in. (A
	// frame-dump against a live container confirmed commit (47809359) and GRV (49263820) replies
	// arrive as these composed envelopes — both disjoint from the read set, so both pass verbatim.)
	frameWith := func(id uint32) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint32(b[4:8], id)
		return b
	}
	readA := frameWith(getValueReplyEnvelopeFileID)
	readB := frameWith(getKeyValuesReplyEnvelopeFileID)
	readC := frameWith(getKeyReplyEnvelopeFileID)
	commit := frameWith(commitReplyEnvelopeFileID)
	grv := frameWith(grvReplyEnvelopeFileID)
	runt := []byte{1, 2, 3} // < 8 bytes

	var injected atomic.Int64
	fn := everyNthInlineReadError(4, ErrFutureVersion, &injected)

	// Interleave control frames among the reads. Only reads advance the n-th counter, so the control
	// frames must NOT shift which read is the 4th. The 4th READ (readB at step idx) must fault.
	type step struct {
		body      []byte
		isRead    bool // a read reply (counts toward n)
		wantFault bool // this specific frame is the n-th read → rewritten
	}
	seq := []step{
		{readA, true, false},   // read 1 → pass
		{commit, false, false}, // commit → verbatim, must NOT count
		{readB, true, false},   // read 2 → pass
		{grv, false, false},    // grv → verbatim, must NOT count
		{readC, true, false},   // read 3 → pass
		{runt, false, false},   // runt → verbatim, no panic, no count
		{readA, true, true},    // read 4 → FAULT
		{commit, false, false}, // commit again → still verbatim
		{readB, true, false},   // read 5 → pass
	}

	for i, s := range seq {
		out, drop := fn(0, transport.UID{}, s.body)
		if drop {
			t.Fatalf("step %d: unexpected drop", i)
		}
		if s.wantFault {
			// Rewritten to exactly the injected inline-error body. (Note: that body carries the INNER
			// GetValueReplyFileID placeholder, not a read ENVELOPE fileID — the client tolerates this,
			// erroror.go:295 — so it is deliberately NOT an isReadReplyBody match.)
			if !bytes.Equal(out, inlineErrorReply(ErrFutureVersion, 0)) {
				t.Fatalf("step %d: expected the injected inline-error body, got %x", i, out)
			}
		} else {
			// Pass-through: the exact input bytes, untouched. This is the load-bearing assertion for
			// the commit/GRV/runt frames (the v1-NAK regression).
			if !bytes.Equal(out, s.body) {
				t.Fatalf("step %d: frame was altered but should pass verbatim (isRead=%v)", i, s.isRead)
			}
		}
	}

	if got := injected.Load(); got != 1 {
		t.Fatalf("expected exactly 1 injected fault over the sequence, got %d", got)
	}
}

// runCycleFaultPhase arms the intercept built by makeIntercept, runs `actors` swap actors for
// `window`, disarms, and asserts (all non-timing-dependent, with FRESH per-phase counters so phase N's
// anti-vacuity is never satisfied by phase N-1's injections): the fault fired (`injected > 0`), the
// workload progressed THROUGH it (`committed > 0`), and the ring is still exactly one Hamiltonian cycle
// (`check == nil`, 3× for determinism). Generic over the fault shape (makeIntercept binds the fresh
// per-phase `injected` counter) — used for inline read errors (1009/1037/1001) AND dropped commit
// replies (RFC-123). The recovery path under test is what differs; this harness only drives + asserts.
func runCycleFaultPhase(t *testing.T, ctx context.Context, db *Database, sd *simDialer, w *cycleWorkload, faultName string, makeIntercept func(*atomic.Int64) frameIntercept, actors int, window time.Duration) {
	t.Helper()
	var injected, committed atomic.Int64
	// armAll (not armAddr) because content-discrimination — not address — scopes the fault (RFC-120 §3.2).
	sd.setIntercept(makeIntercept(&injected))
	sd.armAll()

	workCtx, workCancel := context.WithTimeout(ctx, window)
	defer workCancel()
	var wg sync.WaitGroup
	for a := 0; a < actors; a++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for workCtx.Err() == nil {
				err := w.swapOnce(workCtx, db, rng.Intn(w.nodeCount))
				switch {
				case err == nil:
					committed.Add(1)
				case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
					return // window closed; not a failure
				default:
					// A retryable injected fault must be absorbed by db.Transact. A non-context error
					// surfacing means the client failed to recover it — a real bug. Keyed on error
					// IDENTITY, never the clock: no per-tx timeout is set, so a window close is always a
					// raw context error, never a mapTimeout-synthesized FDBError (transaction.go).
					t.Errorf("[%s] swap failed under injected fault: %v", faultName, err)
					return
				}
			}
		}(int64(a) + 1)
	}
	wg.Wait()

	sd.setIntercept(nil) // disarm: the final check reads fault-free

	if injected.Load() == 0 {
		t.Fatalf("[%s] no faults injected — test is vacuous", faultName)
	}
	if committed.Load() == 0 {
		t.Fatalf("[%s] no swaps committed under fault — client failed to recover", faultName)
	}
	t.Logf("[%s] injected %d faults; committed %d swaps under fault", faultName, injected.Load(), committed.Load())

	// The ring must still be exactly one Hamiltonian cycle despite the injected faults + retries.
	for i := 0; i < 3; i++ {
		if err := w.check(ctx, db); err != nil {
			t.Fatalf("[%s] serializability violated under injected faults (check %d): %v", faultName, i, err)
		}
	}
}

// TestCycle_SurvivesInjectedReadFaults runs the RFC-119 Cycle workload through SimTransport while a
// retryable inline read error is injected on every 4th storage READ reply — future_version (1009)
// then process_behind (1037), sequentially on one ring. Both bubble out of the read path and are
// absorbed by db.Transact's onError loop; injecting either is the same one-line change (a different
// retryable code into the same intercept), so 1037 is RFC-120 §7's "one-line table row". They share
// the inline LoadBalancedReply.error read channel + the QueueModel futureVersion classification
// (isFutureVersionOrProcessBehind), differing only in onError backoff (1009 fixed futureVersionDelay;
// 1037 growing-capped nextBackoff) — which does not threaten progress (both capped at ~1s). Each phase
// starts from the valid ring the previous phase's disarmed check confirmed. The faithful analog of FDB
// running Cycle under Sim2 network faults.
func TestCycle_SurvivesInjectedReadFaults(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	w := &cycleWorkload{nodeCount: 1000, prefix: []byte("cycle_faults_")}
	if err := w.setup(ctx, db); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := w.check(ctx, db); err != nil {
		t.Fatalf("ring invalid right after setup: %v", err)
	}

	for _, phase := range []struct {
		code uint16
		name string
	}{
		{ErrFutureVersion, "future_version/1009"},
		{ErrProcessBehind, "process_behind/1037"},
	} {
		code := phase.code
		runCycleFaultPhase(t, ctx, db, sd, w, phase.name,
			func(c *atomic.Int64) frameIntercept { return everyNthInlineReadError(4, code, c) },
			16, 20*time.Second)
	}
}

// TestCycle_SurvivesInjectedWrongShard runs the Cycle workload through SimTransport while wrong_shard
// (1001) is injected on every 4th storage READ reply. Unlike 1009/1037 (which bubble to onError for a
// re-read), 1001 drives a DISTINCT in-read-path recovery: classify→invalidate the location cache→
// re-locate→re-read (readpath.go:434-439; onErrorRetryable(1001) is FALSE, so 1001 must be absorbed in
// the read path and never reach onError raw). On budget exhaustion getValueImpl surfaces a RETRYABLE
// transaction_too_old (1007) — never terminal — so sustained injection cannot fail a swap spuriously
// (matching libfdb_c, which never propagates wrong_shard/all_alternatives_failed to the app). Cycle
// reads are single-key Gets, so relocate re-reads the same key at the same read version → the correct
// value, with no continuation/drop/dup surface (that hazard is range-mid-scan, pinned by C4). The
// ring-survival assertion is the proof the relocate+invalidate path stays serializable under load
// (RFC-120 §7: wrong_shard is "its own increment ... its own ring-survival assertion").
func TestCycle_SurvivesInjectedWrongShard(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	w := &cycleWorkload{nodeCount: 1000, prefix: []byte("cycle_wrongshard_")}
	if err := w.setup(ctx, db); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := w.check(ctx, db); err != nil {
		t.Fatalf("ring invalid right after setup: %v", err)
	}

	runCycleFaultPhase(t, ctx, db, sd, w, "wrong_shard/1001",
		func(c *atomic.Int64) frameIntercept { return everyNthInlineReadError(4, ErrWrongShardServer, c) },
		16, 20*time.Second)
}

// --- Cycle under a dropped commit reply (RFC-123) ---------------------------------------------

// isCommitReplyBody reports whether a reply body is a commit reply (ErrorOr<CommitID>), by the composed
// envelope fileID at body[4:8] — the SAME content-discrimination isReadReplyBody uses, but for the
// commit envelope. Fail-safe: a runt (<8B) body is treated as not-a-commit-reply (passed verbatim).
func isCommitReplyBody(body []byte) bool {
	if len(body) < 8 {
		return false
	}
	return binary.LittleEndian.Uint32(body[4:8]) == commitReplyEnvelopeFileID
}

// everyNthCommitReplyDrop returns a frameIntercept that DROPS every n-th commit (CommitID) reply,
// passing every read / GRV / locate / unknown frame verbatim. A dropped commit reply makes the client's
// waitReplyOrProxiesChanged time out (DefaultRPCTimeout) → commit_unknown_result (1021) — the faithful
// wire model of a lost commit reply where the commit MAY have applied at the proxy (1021 is
// client-minted from an ambiguous RPC, never proxy-sent; RFC-123 §2). counter advances only on an
// actual drop — the only witness the fault fired, since a dropped frame leaves no other artifact.
//
// Content-discrimination is mandatory (RFC-120 §3.2): the single-process container collocates commit /
// GRV / storage on ONE connection, so this MUST drop only commit replies. In particular GRV replies
// pass verbatim — commitDummyTransaction's own GRV must complete, or the 1021-recovery barrier would
// wedge on a GRV stall (RFC-123 §4.1; pinned by TestEveryNthCommitReplyDrop's GRV-passthrough case).
func everyNthCommitReplyDrop(n int, counter *atomic.Int64) frameIntercept {
	if n <= 0 {
		panic("everyNthCommitReplyDrop: n must be > 0")
	}
	var commitSeen atomic.Int64 // commit replies observed (shared across conns; aggregate ~1/n)
	return func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		if !isCommitReplyBody(body) {
			return body, false // read / GRV / locate / unknown → verbatim
		}
		if commitSeen.Add(1)%int64(n) == 0 {
			counter.Add(1)
			return nil, true // DROP → client commit RPC times out → commit_unknown_result (1021)
		}
		return body, false
	}
}

// TestCycle_SurvivesDroppedCommitReply runs the Cycle workload through SimTransport while every 8th
// commit reply is DROPPED — the faithful commit_unknown_result (1021) fault: the commit MAY have applied
// at the proxy, but the client's commit RPC times out and learns nothing. The client's recovery
// (commitDummyTransaction synchronization barrier + onError(1021) self-conflicting retry) must absorb
// every such drop; the retry re-runs the swap fn from scratch (a FRESH valid transposition of the
// current ring, never a replay), so the ring stays exactly one Hamiltonian cycle whether or not the
// dropped-reply commit applied (committed merely under-counts the applied-but-unknown ones).
//
// Flake-free despite the 5s-per-drop cost: 1021 is onErrorRetryable, the swap-actor loop is workCtx-
// bounded, and the dummy barrier — which runs under context.WithoutCancel (transaction.go:1616), NOT
// workCtx — terminates by a clean reply ((n-1)/n per attempt) + the outer ctx. n=8 (fewer drops than
// the read tests' 1/4, matching the higher per-drop cost) keeps committed orders above the >0 floor.
// The outer ctx (180s) must exceed window (30s) + the worst-case commit/dummy tail with comfortable
// margin (RFC-123 §4.1) — do NOT shrink it toward window+ε or the tail wedges into a real ctx deadline.
func TestCycle_SurvivesDroppedCommitReply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	w := &cycleWorkload{nodeCount: 1000, prefix: []byte("cycle_commitdrop_")}
	if err := w.setup(ctx, db); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := w.check(ctx, db); err != nil {
		t.Fatalf("ring invalid right after setup: %v", err)
	}

	runCycleFaultPhase(t, ctx, db, sd, w, "commit_unknown_result/dropped-reply",
		func(c *atomic.Int64) frameIntercept { return everyNthCommitReplyDrop(8, c) },
		16, 30*time.Second)
}

// TestEveryNthCommitReplyDrop is the determinism floor (no FDB) for the commit-drop intercept. It pins:
// (a) every n-th COMMIT reply is dropped, the others pass verbatim; (b) READ and GRV replies pass
// through UNTOUCHED and do NOT advance the commit counter — the anti-wedge regression (a misfilter that
// dropped the dummy barrier's GRV, or storage reads, would wedge the workload, RFC-123 §4.1); (c) a runt
// (<8B) frame is passed verbatim with no panic; (d) the injection counter advances only on actual drops.
func TestEveryNthCommitReplyDrop(t *testing.T) {
	t.Parallel()
	frameWith := func(id uint32) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint32(b[4:8], id)
		return b
	}
	commit := frameWith(commitReplyEnvelopeFileID)
	read := frameWith(getValueReplyEnvelopeFileID)
	grv := frameWith(grvReplyEnvelopeFileID)
	runt := []byte{1, 2, 3} // < 8 bytes

	var injected atomic.Int64
	fn := everyNthCommitReplyDrop(3, &injected)

	// Interleave reads + GRV among the commits. Only commits advance the n-th counter, so the
	// read/GRV frames must NOT shift which commit is the 3rd. The 3rd COMMIT (at the marked step) drops.
	type step struct {
		body     []byte
		wantDrop bool
		verbatim bool // expect the exact input bytes back (not dropped, not rewritten)
	}
	seq := []step{
		{commit, false, true}, // commit 1 → pass
		{read, false, true},   // read → verbatim, must NOT count
		{commit, false, true}, // commit 2 → pass
		{grv, false, true},    // GRV → verbatim, must NOT count (anti-wedge)
		{runt, false, true},   // runt → verbatim, no panic, no count
		{commit, true, false}, // commit 3 → DROP
		{read, false, true},   // read → verbatim
		{commit, false, true}, // commit 4 → pass
	}
	for i, s := range seq {
		out, drop := fn(0, transport.UID{}, s.body)
		if drop != s.wantDrop {
			t.Fatalf("step %d: drop=%v want %v", i, drop, s.wantDrop)
		}
		if s.wantDrop {
			continue // dropped frame's body is irrelevant
		}
		if s.verbatim && !bytes.Equal(out, s.body) {
			t.Fatalf("step %d: frame altered but should pass verbatim", i)
		}
	}
	if got := injected.Load(); got != 1 {
		t.Fatalf("expected exactly 1 dropped commit over the sequence, got %d", got)
	}
}
