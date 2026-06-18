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
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
				case workCtx.Err() != nil:
					return // workload window closed; not a failure
				default:
					// A non-context error with the window still open is a real failure
					// (badRead / invalid value / non-retryable error the Transact loop surfaced).
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

	if err := w.check(ctx, db); err == nil {
		t.Fatalf("check FAILED to detect a broken ring — the oracle has no teeth")
	} else {
		t.Logf("broken ring correctly detected: %v", err)
	}
}

// TestCycle_CheckData_FailureModes pins each cycleCheckData failure mode 1:1, deterministically and
// without FDB. This is the regression that survives even if the FDB revert-proof above is ever
// stressed. N=4 nodes; rings are described by their successor map.
func TestCycle_CheckData_FailureModes(t *testing.T) {
	t.Parallel()
	w := &cycleWorkload{nodeCount: 4, prefix: []byte("u_")}

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
