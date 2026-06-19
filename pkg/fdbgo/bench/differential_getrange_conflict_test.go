package bench

import (
	"fmt"
	"os"
	"strings"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// RFC-121 conflict-range differential probes vs libfdb_c — two documented divergences (this file
// covers BOTH: D1 GetRange conflict-clamp below, and D2 read-own-write further down).
//
// ── RFC-121 D1: GetRange read-CONFLICT-clamp ──────────────────────────────────────────────────
//
// On a LIMITED / more=true GetRange, libfdb_c clamps the read-conflict to the data actually
// returned (`rangeEnd = keyAfter(lastReturnedKey)` — ReadYourWrites.actor.cpp:271-274 /
// NativeAPI.actor.cpp:4576-4579). The pure-Go client used to add the FULL requested [begin,end)
// eagerly and never clamp, so a concurrent write in the UNREAD tail aborted a Go transaction that
// libfdb_c committed. RFC-121 D1 (rangeConflictExtent, transaction.go) ported the clamp, so this
// now asserts agreement (go==cgo): both COMMIT.
//
// Deterministic commit-order race (same version-pinning as the GetKey conflict differential, so a
// non-causal GRV can't trip a spurious 1020):
//
//	setup: seed k00..k19, commit; pin A and B to the setup's COMMIT version
//	A.GetRange([k00, kzz), limit=10)  // reads k00..k09; registers the read-conflict
//	A.Set(sentinel)                   // committable write so A reaches the resolver
//	B.Set(k15); B.Commit()            // k15 is in the UNREAD tail (k09, kzz), commits > vSetup
//	A.Commit()                        // 1020 IFF k15 ∈ A's read-conflict range
//
// Go's unclamped [k00, kzz) contains k15 → abort; libfdb_c's [k00, k09\x00) does not → commit.

const (
	rangeConflictLimit = 10
	rangeConflictProbe = "k15" // in the unread tail (k09, kzz) for limit=10 over k00..k19
)

func seedRangeKeysGo(tx gofdb.Transaction, pfx string) {
	for i := range 20 {
		tx.Set(gofdb.Key(fmt.Sprintf("%sk%02d", pfx, i)), []byte("v"))
	}
}

func seedRangeKeysCgo(tx cgofdb.Transaction, pfx string) {
	for i := range 20 {
		tx.Set(cgofdb.Key(fmt.Sprintf("%sk%02d", pfx, i)), []byte("v"))
	}
}

func goRangeConflictScenario(t *testing.T, pfx string) conflictOutcome {
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
	kvs, gerr := a.GetRange(
		gofdb.KeyRange{Begin: gofdb.Key(pfx + "k00"), End: gofdb.Key(pfx + "kzz")},
		gofdb.RangeOptions{Limit: rangeConflictLimit},
	).GetSliceWithError()
	if gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go A GetRange: %v", gerr)
	}
	if len(kvs) != rangeConflictLimit {
		t.Fatalf("go A GetRange returned %d rows, want %d (scenario assumption)", len(kvs), rangeConflictLimit)
	}
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+rangeConflictProbe), []byte("B"))
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

func cgoRangeConflictScenario(t *testing.T, pfx string) conflictOutcome {
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
	kvs, gerr := a.GetRange(
		cgofdb.KeyRange{Begin: cgofdb.Key(pfx + "k00"), End: cgofdb.Key(pfx + "kzz")},
		cgofdb.RangeOptions{Limit: rangeConflictLimit},
	).GetSliceWithError()
	if gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("cgo A GetRange: %v", gerr)
	}
	if len(kvs) != rangeConflictLimit {
		t.Fatalf("cgo A GetRange returned %d rows, want %d (scenario assumption)", len(kvs), rangeConflictLimit)
	}
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+rangeConflictProbe), []byte("B"))
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

// TestDifferential_GetRangeConflictClamp_RFC121 pins RFC-121 D1: Go over-conflicts on a limited
// GetRange (a concurrent write in the unread tail aborts Go but commits in libfdb_c). When RFC-121
// clamps the Go conflict to the returned extent, BOTH will commit — and the `goOut.conflicted` /
// `cOut.conflicted` assertions below will fail, forcing this probe to flip to assert agreement.
func TestDifferential_GetRangeConflictClamp_RFC121(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("conflict differential did not clear transient errors in %d attempts", maxAttempts)
		}
		goPfx := fmt.Sprintf("grconf_%d_%s_%d_go_", os.Getpid(), ns, attempt)
		cPfx := fmt.Sprintf("grconf_%d_%s_%d_c_", os.Getpid(), ns, attempt)
		goOut := goRangeConflictScenario(t, goPfx)
		cOut := cgoRangeConflictScenario(t, cPfx)
		if goOut.retry || cOut.retry {
			continue
		}
		// RFC-121 D1 FIXED: Go now clamps the GetRange read-conflict to the returned extent
		// ([k00, k09\x00)), so the unread-tail write to k15 conflicts neither client — both COMMIT,
		// matching libfdb_c. Assert agreement (the clamp is wire-faithful, not merely "Go also
		// commits"). Reverting the clamp makes Go over-conflict → goOut.conflicted=true → red.
		if goOut.conflicted != cOut.conflicted {
			t.Errorf("RFC-121 D1: GetRange conflict-clamp diverges — go conflicted=%v, cgo conflicted=%v "+
				"(both should COMMIT: the unread-tail write k15 is outside [k00, keyAfter(k09)))",
				goOut.conflicted, cOut.conflicted)
		}
		if cOut.conflicted {
			t.Errorf("unexpected: libfdb_c aborted on the unread-tail write — scenario assumption wrong " +
				"(it clamps the conflict to the returned extent and COMMITs)")
		}
		return
	}
}

// ── RFC-121 D2: Get/GetRange read-own-write conflict ──────────────────────────────────────────
//
// When a Get is served entirely by a local INDEPENDENT write (a prior Set in the same txn),
// libfdb_c adds NO read-conflict for that key (updateConflictMap skips independent-write segments —
// ReadYourWrites.actor.cpp:328/342). RFC-058 wired this filter into GetKey; RFC-121 D2
// (addReadConflictForKeyRYW, transaction.go) wired it into Get/GetPipelined too, so `Set(K);Get(K)`
// no longer registers a spurious read-conflict on K — a concurrent write to K commits in both
// clients. Asserts agreement. Same deterministic commit-order race + version pinning as D1.
//
//	A.Set(rk); A.Get(rk)   // Go registers a read-conflict on rk; libfdb_c does not (local write)
//	A.Set(sentinel)        // committable write
//	B.Set(rk); B.Commit()  // write-write on rk does NOT conflict — only A's READ-conflict can trip 1020
//	A.Commit()             // 1020 IFF rk ∈ A's read-conflict (Go: yes; libfdb_c: no)

const readOwnWriteKey = "rk"

func goReadOwnWriteScenario(t *testing.T, pfx string) conflictOutcome {
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
	a.Set(gofdb.Key(pfx+readOwnWriteKey), []byte("A")) // local independent write
	if _, gerr := a.Get(gofdb.Key(pfx + readOwnWriteKey)).Get(); gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go A Get: %v", gerr)
	}
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+readOwnWriteKey), []byte("B"))
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

func cgoReadOwnWriteScenario(t *testing.T, pfx string) conflictOutcome {
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
	a.Set(cgofdb.Key(pfx+readOwnWriteKey), []byte("A"))
	if _, gerr := a.Get(cgofdb.Key(pfx + readOwnWriteKey)).Get(); gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("cgo A Get: %v", gerr)
	}
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+readOwnWriteKey), []byte("B"))
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

// TestDifferential_ReadOwnWriteConflict_RFC121 pins RFC-121 D2: a Get served by a local independent
// write still registers a read-conflict in Go (not in libfdb_c), so a concurrent write to that key
// aborts Go but commits in libfdb_c. Flip to assert agreement when the RYW filter is wired into Get.
func TestDifferential_ReadOwnWriteConflict_RFC121(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("conflict differential did not clear transient errors in %d attempts", maxAttempts)
		}
		goPfx := fmt.Sprintf("rowconf_%d_%s_%d_go_", os.Getpid(), ns, attempt)
		cPfx := fmt.Sprintf("rowconf_%d_%s_%d_c_", os.Getpid(), ns, attempt)
		goOut := goReadOwnWriteScenario(t, goPfx)
		cOut := cgoReadOwnWriteScenario(t, cPfx)
		if goOut.retry || cOut.retry {
			continue
		}
		// RFC-121 D2 FIXED: a Get served by a local independent Set adds no read-conflict in Go
		// (the RYW filter is now wired into Get/GetPipelined), so the concurrent write to rk
		// conflicts neither client — both COMMIT, matching libfdb_c. Reverting the filter makes Go
		// register the spurious read-conflict → goOut.conflicted=true → red.
		if goOut.conflicted != cOut.conflicted {
			t.Errorf("RFC-121 D2: read-own-write conflict diverges — go conflicted=%v, cgo conflicted=%v "+
				"(both should COMMIT: the read on rk is served by a local Set, so no read-conflict)",
				goOut.conflicted, cOut.conflicted)
		}
		if cOut.conflicted {
			t.Errorf("unexpected: libfdb_c aborted on a read-own-write — it skips the read-conflict and COMMITs")
		}
		return
	}
}
