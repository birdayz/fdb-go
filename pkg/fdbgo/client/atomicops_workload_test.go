package client

// AtomicOps workload — atomic-op + companion-log transactional-consistency oracle.
//
// Go port of FoundationDB's fdbserver/workloads/AtomicOps.actor.cpp @ tag 7.3.75 (AddValue arm).
// Each committed transaction writes, ATOMICALLY together: a UNIQUE log key set to val, and an atomic
// Add of val to an ops key. Because the set + the atomicOp commit as one unit (both-or-neither), the
// per-keyspace aggregates must match exactly: sum(log) == sum(ops). A torn commit (log without op, or
// op without log), a lost mutation, or a mis-applied atomic op breaks the equality. This is a stronger
// oracle than TestConcurrentAtomicAdd (a bare single-key sum, which a non-atomic read-modify-write
// could satisfy and which has no fault stress) — it is the AtomicOps "distributed idempotency-under-
// retry stress" gap (RFC-119 §7), run healthy AND under the RFC-123 commit-reply-drop fault.
//
// THE LOAD-BEARING DESIGN DETAIL (RFC-124): the op is generated INSIDE the db.Transact fn (fresh per
// retry) and the log key is UNIQUE PER ATTEMPT (a global monotonic counter bumped inside fn), NOT per
// logical op. Rationale: with no idempotency IDs (neither Go nor libfdb_c-without-IDs has them; the
// commitDummyTransaction barrier handles the in-flight race, not an already-applied op), retrying the
// SAME atomic Add under commit_unknown_result DOUBLE-APPLIES it. The C++ worker avoids self-double-
// counting by re-randomizing per attempt; we do the same. If the logKey were keyed on (actor,opNum), a
// 1021-retry would reuse it and overwrite the maybe-applied original's log entry while its Add already
// landed → ops > log → a spurious (flaky) mismatch. A fresh unique logKey per attempt keeps the
// maybe-applied original and the retry disjoint, so sum(log)==sum(ops) holds exactly even under fault.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// atomicOpsLogSeq mints a globally-unique suffix for every log key — once PER ATTEMPT (incremented
// inside the db.Transact fn), so a commit_unknown_result retry never reuses a prior attempt's log key.
var atomicOpsLogSeq atomic.Int64

type atomicOpsWorkload struct {
	prefix        []byte
	groups        int
	nodesPerGroup int
}

func (w *atomicOpsWorkload) opsKey(group, node int) []byte {
	return []byte(fmt.Sprintf("%sops_%02x_%02x", w.prefix, group, node))
}

func (w *atomicOpsWorkload) logKey(group int, attempt int64) []byte {
	return []byte(fmt.Sprintf("%slog_%02x_%016x", w.prefix, group, attempt))
}

// opsGroupRange / logGroupRange cover every ops_/log_ key for ONE group ('_' < any hex digit; the
// trailing 0xff sorts strictly after every suffix). The check iterates per group (not a single global
// scan) so it catches cross-group MISROUTING — an Add applied to the wrong ops_<group> while its log
// is written under the original group keeps the GLOBAL sums equal but breaks the per-group equality
// (the C++ AtomicOps check is per-group, _check loops g∈[0,100)).
func (w *atomicOpsWorkload) opsGroupRange(group int) (begin, end []byte) {
	begin = []byte(fmt.Sprintf("%sops_%02x_", w.prefix, group))
	return begin, append(append([]byte{}, begin...), 0xff)
}

func (w *atomicOpsWorkload) logGroupRange(group int) (begin, end []byte) {
	begin = []byte(fmt.Sprintf("%slog_%02x_", w.prefix, group))
	return begin, append(append([]byte{}, begin...), 0xff)
}

// runOnce commits ONE atomic-op+log transaction. The op (group/node/val) and the unique log key are
// drawn INSIDE the fn, so db.Transact's retry loop re-randomizes a FRESH op on every attempt (the
// 1021-replay hazard, see file header). Returns the committed val (the successful attempt's), for the
// lbsum lower bound. db.Transact hides the intermediate commit_unknown_result retries, so this counts
// only the definitively-committed value — exactly the lower bound for sum(ops).
func (w *atomicOpsWorkload) runOnce(ctx context.Context, db *Database, rng *rand.Rand) (uint64, error) {
	res, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		group := rng.Intn(w.groups)
		node := rng.Intn(w.nodesPerGroup)
		val := uint64(rng.Intn(1_000_000) + 1) // nonzero so a dropped op is observable in the sum
		attempt := atomicOpsLogSeq.Add(1)      // UNIQUE PER ATTEMPT — see file header
		tx.Set(w.logKey(group, attempt), le64(val))
		tx.Atomic(MutAddValue, w.opsKey(group, node), le64(val))
		return val, nil
	})
	if err != nil {
		return 0, err
	}
	return res.(uint64), nil
}

// scanSumLE64 sums the little-endian uint64 values over [begin, end), paginating on `more` (the log
// keyspace grows one key per committed attempt). One read version (one db.Transact) — a consistent
// snapshot. Returns (sum, key count). Resets accumulators inside fn for retry safety.
func scanSumLE64(ctx context.Context, db *Database, begin, end []byte) (uint64, int, error) {
	var sum uint64
	var count int
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		sum, count = 0, 0
		b := begin
		for {
			kvs, more, err := tx.GetRange(ctx, b, end, 10000)
			if err != nil {
				return nil, err
			}
			for _, kv := range kvs {
				sum += binary.LittleEndian.Uint64(kv.Value)
				count++
			}
			if !more || len(kvs) == 0 {
				break
			}
			b = append(append([]byte{}, kvs[len(kvs)-1].Key...), 0) // keyAfter(lastKey)
		}
		return nil, nil
	})
	return sum, count, err
}

// check is the AtomicOps oracle, evaluated PER GROUP: for each group, sum(log) == sum(ops)
// (server-vs-server, exact — the two move together or not at all because each op's set+atomicOp commit
// as one unit; per-group catches cross-group misrouting a global sum would mask). Returns the
// aggregate (sumLog, sumOps, logKeys) for the caller's bound/anti-vacuity assertions, and a non-nil
// err naming the first group whose per-group equality fails.
func (w *atomicOpsWorkload) check(ctx context.Context, db *Database) (sumLog, sumOps uint64, logKeys int, err error) {
	for g := 0; g < w.groups; g++ {
		lb, le := w.logGroupRange(g)
		ob, oe := w.opsGroupRange(g)
		gLog, gLogKeys, err := scanSumLE64(ctx, db, lb, le)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("scan log group %02x: %w", g, err)
		}
		gOps, _, err := scanSumLE64(ctx, db, ob, oe)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("scan ops group %02x: %w", g, err)
		}
		if gLog != gOps {
			return 0, 0, 0, fmt.Errorf("group %02x: sum(log)=%d != sum(ops)=%d (torn/lost/double/misrouted commit)", g, gLog, gOps)
		}
		sumLog += gLog
		sumOps += gOps
		logKeys += gLogKeys
	}
	return sumLog, sumOps, logKeys, nil
}

// runAtomicOpsPhase runs `actors` actors committing atomic-op+log txns for `window` (optionally under
// the fault built by makeIntercept), disarms, then asserts the AtomicOps oracle: sum(log)==sum(ops)
// (exact), sum(ops) >= lbsum (the definitely-committed lower bound), and anti-vacuity (lbsum>0, and
// injected>0 when a fault is armed). All non-timing-dependent.
func runAtomicOpsPhase(t *testing.T, ctx context.Context, db *Database, sd *simDialer, w *atomicOpsWorkload, faultName string, makeIntercept func(*atomic.Int64) frameIntercept, actors int, window time.Duration) {
	t.Helper()
	var injected atomic.Int64
	var committed atomic.Int64
	var lbsum atomic.Uint64
	if makeIntercept != nil {
		sd.setIntercept(makeIntercept(&injected))
		sd.armAll()
	}

	workCtx, workCancel := context.WithTimeout(ctx, window)
	defer workCancel()
	var wg sync.WaitGroup
	for a := 0; a < actors; a++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for workCtx.Err() == nil {
				val, err := w.runOnce(workCtx, db, rng)
				switch {
				case err == nil:
					committed.Add(1)
					lbsum.Add(val)
				case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
					return // window closed; not a failure
				default:
					// A retryable injected fault must be absorbed by db.Transact. A non-context error
					// surfacing means the client failed to recover it — a real bug. Keyed on error
					// identity, never the clock (no per-tx timeout → a window close is a raw ctx error).
					t.Errorf("[%s] atomic op failed under fault: %v", faultName, err)
					return
				}
			}
		}(int64(a) + 1)
	}
	wg.Wait()
	if makeIntercept != nil {
		sd.setIntercept(nil) // disarm: the consistency check reads fault-free
	}

	if committed.Load() == 0 {
		t.Fatalf("[%s] no atomic ops committed — workload vacuous", faultName)
	}
	if makeIntercept != nil && injected.Load() == 0 {
		t.Fatalf("[%s] no faults injected — test is vacuous", faultName)
	}

	// Primary oracle (per group, inside check): the atomic op and its companion log entry commit as one
	// unit, so each group's keyspace aggregates are exactly equal — even across concurrent commits and
	// 1021 retries. check returns a non-nil err naming the first group that mismatches.
	sumLog, sumOps, logKeys, err := w.check(ctx, db)
	if err != nil {
		t.Fatalf("[%s] AtomicOps consistency violated: %v", faultName, err)
	}
	t.Logf("[%s] committed=%d injected=%d lbsum=%d sumLog=%d sumOps=%d logKeys=%d",
		faultName, committed.Load(), injected.Load(), lbsum.Load(), sumLog, sumOps, logKeys)

	// sum(ops) must be at least the definitely-committed total; a 1021-maybe-applied op only adds more.
	if sumOps < lbsum.Load() {
		t.Fatalf("[%s] sum(ops)=%d < lbsum=%d — a definitely-committed atomic op was lost",
			faultName, sumOps, lbsum.Load())
	}
	if logKeys == 0 || lbsum.Load() == 0 {
		t.Fatalf("[%s] vacuous: logKeys=%d lbsum=%d", faultName, logKeys, lbsum.Load())
	}
}

// TestAtomicOps_LogConsistentUnderConcurrency is the healthy baseline: concurrent atomic-op+log txns
// keep sum(log)==sum(ops). The companion-log cross-check is the teeth TestConcurrentAtomicAdd lacks
// (a bare single-key sum can't distinguish a real atomic op from a lost/torn one).
func TestAtomicOps_LogConsistentUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	w := &atomicOpsWorkload{prefix: []byte("atomicops_healthy_"), groups: 8, nodesPerGroup: 8}
	runAtomicOpsPhase(t, ctx, db, nil, w, "healthy", nil, 16, 15*time.Second)
}

// TestAtomicOps_LogConsistentUnderDroppedCommit is the gap: the same workload under the RFC-123
// commit-reply-drop fault (commit_unknown_result). sum(log)==sum(ops) must STILL hold — proving the
// atomic op and its companion log commit atomically (both-or-neither) even when the commit outcome is
// ambiguous and the client retries (with a FRESH op, via commitDummyTransaction + onError(1021)). A
// torn commit (log without op / op without log) or a lost op under the fault → mismatch, caught here.
func TestAtomicOps_LogConsistentUnderDroppedCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)

	w := &atomicOpsWorkload{prefix: []byte("atomicops_drop_"), groups: 8, nodesPerGroup: 8}
	runAtomicOpsPhase(t, ctx, db, sd, w, "commit_unknown_result/dropped-reply",
		func(c *atomic.Int64) frameIntercept { return everyNthCommitReplyDrop(8, c) },
		16, 30*time.Second)
}
