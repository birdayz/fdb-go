package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Regression for a harness flake: the pinned-version range read (goRangeAt/cgoRangeAt, used by
// stripNormGo/stripNormC and TestDifferential_RangeRead) used to call t.Fatalf on ANY error.
// Under heavy parallel-container load the pinned read version can age past FDB's 5s MVCC window
// between the GRV and the read, yielding transaction_too_old (1007) — a RETRYABLE error — which
// then failed the test instead of being retried with a fresh version (the getkey path already
// returned-and-retried; the range path did not). This pins both halves of the fix:
//
//	(a) goRangeAt/cgoRangeAt RETURN a retryable 1007 on a stale pin (no Fatalf), and
//	(b) the caller's re-pin-and-retry loop recovers and both clients agree on the snapshot.
//
// An ancient pinned version (1) deterministically reproduces the 1007 on both clients.
func TestDifferential_PinnedRangeRetriesStaleVersion(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("stalepin_%d_", os.Getpid())
	seedKeys(t, func(tx cgofdb.Transaction) {
		tx.Set(cgofdb.Key(pfx+"k1"), []byte("a"))
		tx.Set(cgofdb.Key(pfx+"k2"), []byte("b"))
	})
	goPR, err := gofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("go PrefixRange: %v", err)
	}
	cPR, err := cgofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("cgo PrefixRange: %v", err)
	}

	// (a) An ancient pin surfaces a RETURNED retryable 1007 on both clients (not a Fatalf).
	if _, err := goRangeAt(t, 1, goPR, gofdb.RangeOptions{}); !isFDBRetryable(err) {
		t.Fatalf("go: ancient pin must return a retryable error, got %v (code %d)", err, fdbErrorCode(err))
	}
	if _, err := cgoRangeAt(t, 1, cPR, cgofdb.RangeOptions{}); !isFDBRetryable(err) {
		t.Fatalf("cgo: ancient pin must return a retryable error, got %v (code %d)", err, fdbErrorCode(err))
	}

	// (b) The re-pin-and-retry loop recovers: attempt 0 uses the stale version (both clients
	// return retryable → continue), then a fresh shared version succeeds and the two agree.
	var goKVs []gofdb.KeyValue
	var cKVs []cgofdb.KeyValue
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("did not recover from the stale pin in %d attempts", maxAttempts)
		}
		v := int64(1) // stale on the first attempt
		if attempt > 0 {
			v = freshSharedVersion(t)
		}
		gRaw, gErr := goRangeAt(t, v, goPR, gofdb.RangeOptions{})
		cRaw, cErr := cgoRangeAt(t, v, cPR, cgofdb.RangeOptions{})
		if (gErr != nil && isFDBRetryable(gErr)) || (cErr != nil && isFDBRetryable(cErr)) {
			continue // attempt 0: the stale pin retries; later attempts: any transient
		}
		if gErr != nil || cErr != nil {
			t.Fatalf("unexpected non-retryable error: go=%v cgo=%v", gErr, cErr)
		}
		goKVs, cKVs = gRaw, cRaw
		break
	}
	if len(goKVs) != 2 || len(cKVs) != 2 {
		t.Fatalf("after recovery expected 2 KV pairs each, got go=%d cgo=%d", len(goKVs), len(cKVs))
	}
}
