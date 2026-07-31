package bench

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Self-conflict-range canary — a pinned NEGATIVE result.
//
// A commit whose write and read conflict ranges do not already intersect gets an ephemeral
// \xff/SC/<128-bit UID> range added to BOTH sets (C++ commitMutations → makeSelfConflicting,
// NativeAPI.actor.cpp:5952-5959; Go's maybeMakeSelfConflicting / makeSelfConflictingLocked). The
// key is the transaction's idempotency anchor: it is what commitDummyTransaction barriers on
// after commit_unknown_result.
//
// Each iteration here commits a transaction that does NO reads and writes ONE private key. Its
// read-conflict set is therefore EXACTLY the \xff/SC/<uid> range and nothing else, and its write
// key is touched by nothing else in the suite. A not_committed(1020) can then mean only one
// thing: some other transaction committed a write-conflict range covering \xff/SC/<uid> — either
// a UID collision, or a system-keyspace range wide enough to swallow every in-flight
// transaction's self-conflict key. Either would spuriously conflict unrelated transactions
// cluster-wide and would be indistinguishable, from the outside, from a conflict-range bug.
//
// This is load-bearing rather than decorative: it is the fact that lets a spurious 1020 elsewhere
// be attributed to the transaction's USER conflict ranges instead of to the client-injected
// self-conflict range. Measured clean across ~134_000 commits (both clients) while the rest of
// this package ran at volume. If it ever fires, that attribution is void and every conclusion
// resting on it has to be re-derived.
//
// The cgo arm carries REPORT_CONFLICTING_KEYS so a hit names the offending range instead of
// merely asserting one exists; the pure-Go client does not implement that option
// (fdb.UnsupportedOptionError), which is why only one arm reports.
func TestDifferential_SelfConflictRangeCanary(t *testing.T) {
	t.Parallel()
	const workers = 4
	iters := 150
	if testing.Short() {
		iters = 15
	}

	var goHits, cgoHits, goCommits, cgoCommits int64
	var firstCK atomic.Value
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := fmt.Sprintf("scanary_%d_w%d_%d", os.Getpid(), id, i)

				if tx, err := goClient.CreateTransaction(); err == nil {
					tx.Set(gofdb.Key(k+"_go"), []byte("c"))
					code := fdbErrorCode(tx.Commit().Get())
					tx.Cancel()
					atomic.AddInt64(&goCommits, 1)
					if code == 1020 {
						atomic.AddInt64(&goHits, 1)
					}
				}

				if ct, err := cgoClient.CreateTransaction(); err == nil {
					_ = ct.Options().SetReportConflictingKeys()
					ct.Set(cgofdb.Key(k+"_c"), []byte("c"))
					code := fdbErrorCode(ct.Commit().Get())
					atomic.AddInt64(&cgoCommits, 1)
					if code == 1020 {
						atomic.AddInt64(&cgoHits, 1)
						firstCK.CompareAndSwap(nil, selfConflictConflictingKeys(ct))
					}
					ct.Cancel()
				}
			}
		}(w)
	}
	wg.Wait()

	if goHits > 0 || cgoHits > 0 {
		ck, _ := firstCK.Load().(string)
		t.Fatalf("a transaction whose only read-conflict range is its own \\xff/SC/<uid> took not_committed: "+
			"go=%d/%d cgo=%d/%d conflicting_keys=%s — the self-conflict range is no longer private, so "+
			"attributing spurious conflicts to USER conflict ranges is no longer sound",
			goHits, goCommits, cgoHits, cgoCommits, ck)
	}
}

// selfConflictConflictingKeys reads \xff\xff/transaction/conflicting_keys/ off a cgo transaction
// that just took not_committed with REPORT_CONFLICTING_KEYS set. The module reports the
// transaction's own read-conflict ranges annotated "1" (begin of a range that conflicted) / "0"
// (its end).
func selfConflictConflictingKeys(tr cgofdb.Transaction) string {
	const mod = "\xff\xff/transaction/conflicting_keys/"
	rr := tr.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(mod), End: cgofdb.Key(mod + "\xff\xff")}, cgofdb.RangeOptions{})
	kvs, err := rr.GetSliceWithError()
	if err != nil {
		return "<read-back failed: " + err.Error() + ">"
	}
	if len(kvs) == 0 {
		return "<none reported>"
	}
	out := ""
	for _, kv := range kvs {
		out += fmt.Sprintf(" %q=%s", string(kv.Key)[len(mod):], kv.Value)
	}
	return out
}
