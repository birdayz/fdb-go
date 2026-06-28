package bench

import (
	"fmt"
	"os"
	"strings"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// RFC-121 iterator under-conflict guard (codex #319 P1).
//
// The streaming RangeResult.Iterator() fetches in batches. It used to read the FIRST batch as a
// serializable read and ALL LATER batches as snapshot reads (no conflict), on the premise that the
// first read recorded the full [begin,end) conflict. RFC-121's clamp broke that premise: the first
// batch's conflict is now clamped to its returned prefix (2 rows in StreamingModeIterator), so rows
// consumed in later (snapshot) batches carried NO read-conflict. A concurrent write to such a row
// would let the Go transaction COMMIT with stale data where libfdb_c ABORTS — a real under-conflict
// (lost serializability). The fix makes every batch a serializable read (each adds its own clamped
// conflict; the union covers the consumed range — the C-client behavior).
//
// This differential consumes the ENTIRE iterator over k00..k19 (so it reads k15 in a LATER batch),
// then a concurrent committed write to k15 must abort BOTH clients. Deterministic commit-order race
// + version pinning, identical to the GetSlice conflict differentials in the sibling file.
//
//	A: iterate GetRange([k00,kzz), StreamingModeIterator) consuming all 20 rows (reads k15 late)
//	A.Set(sentinel)        // committable write so A reaches the resolver
//	B.Set(k15); B.Commit() // k15 was READ by A in a later (formerly snapshot) batch
//	A.Commit()             // 1020 IFF k15 ∈ A's read-conflict — must match cgo

const iterConflictProbe = "k15" // read in iterator batch 4 (rows 14-19), a formerly-snapshot batch

func goRangeIteratorConflictScenario(t *testing.T, pfx string) conflictOutcome {
	t.Helper()
	r, err := gofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("go PrefixRange: %v", err)
	}
	setup, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go setup create: %v", err)
	}
	setup.ClearRange(r)
	seedRangeKeysGo(setup, pfx)
	if err := setup.Commit().Get(); err != nil {
		setup.Cancel()
		if isFDBRetryable(err) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go setup commit: %v", err)
	}
	vSetup, err := setup.GetCommittedVersion()
	if err != nil {
		t.Fatalf("go setup committed version: %v", err)
	}

	a, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	iter := a.GetRange(
		gofdb.KeyRange{Begin: gofdb.Key(pfx + "k00"), End: gofdb.Key(pfx + "kzz")},
		gofdb.RangeOptions{Mode: gofdb.StreamingModeIterator},
	).Iterator()
	n := 0
	for iter.Advance() {
		iter.MustGet() // Advance()==true guarantees a valid element with no error
		n++
	}
	// Advance() returns false on exhaustion OR a batch-fetch error; surface the latter via Get()
	// so a transient read (e.g. 1007 under heavy parallel-container load) RETRIES instead of
	// fataling the row-count assert (matching the sibling GetSlice scenarios' transient handling).
	if _, gerr := iter.Get(); gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go A iterate: %v", gerr)
	}
	if n != 20 {
		t.Fatalf("go A iterated %d rows, want 20 (scenario assumption — must read k15 in a later batch)", n)
	}
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+iterConflictProbe), []byte("B"))
	if err := b.Commit().Get(); err != nil {
		if isFDBRetryable(err) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go B commit: %v", err)
	}
	switch fdbErrorCode(a.Commit().Get()) {
	case 0:
		return conflictOutcome{conflicted: false}
	case 1020:
		return conflictOutcome{conflicted: true}
	default:
		return conflictOutcome{retry: true}
	}
}

func cgoRangeIteratorConflictScenario(t *testing.T, pfx string) conflictOutcome {
	t.Helper()
	r, err := cgofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("cgo PrefixRange: %v", err)
	}
	setup, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo setup create: %v", err)
	}
	setup.ClearRange(r)
	seedRangeKeysCgo(setup, pfx)
	if err := setup.Commit().Get(); err != nil {
		setup.Cancel()
		if isFDBRetryable(err) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("cgo setup commit: %v", err)
	}
	vSetup, err := setup.GetCommittedVersion()
	if err != nil {
		t.Fatalf("cgo setup committed version: %v", err)
	}

	a, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	iter := a.GetRange(
		cgofdb.KeyRange{Begin: cgofdb.Key(pfx + "k00"), End: cgofdb.Key(pfx + "kzz")},
		cgofdb.RangeOptions{Mode: cgofdb.StreamingModeIterator},
	).Iterator()
	n := 0
	for iter.Advance() {
		// Apple binding contract: Advance() returns TRUE on a batch-fetch error (so the next
		// Get surfaces it), so check Get's error IN the loop. Use Get (not MustGet, which would
		// panic the FDB error) so a transient read RETRIES; a post-loop Get() would index past
		// the end on clean exhaustion and panic.
		if _, gerr := iter.Get(); gerr != nil {
			if isFDBRetryable(gerr) {
				return conflictOutcome{retry: true}
			}
			t.Fatalf("cgo A iterate: %v", gerr)
		}
		n++
	}
	if n != 20 {
		t.Fatalf("cgo A iterated %d rows, want 20 (scenario assumption)", n)
	}
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+iterConflictProbe), []byte("B"))
	if err := b.Commit().Get(); err != nil {
		if isFDBRetryable(err) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("cgo B commit: %v", err)
	}
	switch fdbErrorCode(a.Commit().Get()) {
	case 0:
		return conflictOutcome{conflicted: false}
	case 1020:
		return conflictOutcome{conflicted: true}
	default:
		return conflictOutcome{retry: true}
	}
}

// TestDifferential_GetRangeIteratorConflict_RFC121 pins the codex #319 P1 fix: a key read through a
// LATER iterator batch must register a read-conflict in Go, just as in libfdb_c. Both clients fully
// iterate [k00,kzz) and then a concurrent write to k15 must abort BOTH. Reverting the iterator fix
// (later batches → snapshot, no conflict) makes Go commit (goOut.conflicted=false) while cgo aborts
// → the agreement assert below fires.
func TestDifferential_GetRangeIteratorConflict_RFC121(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("conflict differential did not clear transient errors in %d attempts", maxAttempts)
		}
		goPfx := fmt.Sprintf("itconf_%d_%s_%d_go_", os.Getpid(), ns, attempt)
		cPfx := fmt.Sprintf("itconf_%d_%s_%d_c_", os.Getpid(), ns, attempt)
		goOut := goRangeIteratorConflictScenario(t, goPfx)
		cOut := cgoRangeIteratorConflictScenario(t, cPfx)
		if goOut.retry || cOut.retry {
			continue
		}
		if goOut.conflicted != cOut.conflicted {
			t.Errorf("RFC-121 iterator conflict diverges — go conflicted=%v, cgo conflicted=%v "+
				"(both should ABORT: k15 was read through a later iterator batch, so it must conflict the "+
				"concurrent write)", goOut.conflicted, cOut.conflicted)
		}
		if !cOut.conflicted {
			t.Errorf("unexpected: libfdb_c did NOT conflict on a key it read via the iterator — scenario assumption wrong")
		}
		return
	}
}
