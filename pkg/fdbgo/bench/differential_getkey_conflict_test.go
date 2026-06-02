package bench

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// GetKey read-CONFLICT differential vs libfdb_c — RFC-058 sub-edge (2).
//
// getKey registers a read-conflict over the selector base ↔ resolved span, then C++
// updateConflictMap (ReadYourWrites.actor.cpp:335) SUBTRACTS the segments resolved locally
// with no DB read (INDEPENDENT writes + cleared ranges), keeping only UNMODIFIED gaps +
// DEPENDENT writes. The Go client used to keep the FULL span (safe over-conflict); this proves
// the ported filtering matches libfdb_c EXACTLY — neither over- nor under-conflicting.
//
// The Go client does not expose the \xff\xff/transaction/read_conflict_range/ special-key
// space, so we prove the conflict SET behaviorally with a deterministic commit-order race:
//
//	A.GetKey(selector)            // pins A's read version V_A, registers the read-conflict
//	A.Set(sentinel)               // a committable write (so A reaches the resolver)
//	B.Set(probeKey); B.Commit()   // commits at V_B > V_A
//	A.Commit()                    // fails not_committed(1020) IFF probeKey ∈ A's read-conflict
//
// B commits strictly between A's read version and A's commit, so the outcome is a pure
// function of whether probeKey is in A's (filtered) read-conflict range. Running the IDENTICAL
// scenario on each client and asserting go-A's outcome == cgo-A's outcome is the differential;
// we also assert the expected C++ outcome to catch a both-wrong regression. Before the fix the
// INDEPENDENT/CLEARED cases diverge (go over-conflicts → 1020 where cgo commits); after, they
// match. Write-write on probeKey does NOT conflict (last-writer-wins), so only A's READ
// conflict can trip 1020 — isolating exactly the read-conflict set under test.

// fdbErrorCode extracts the FDB error code from either client's error (0 if nil / non-FDB).
func fdbErrorCode(err error) int {
	if err == nil {
		return 0
	}
	var ge gofdb.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	var ce cgofdb.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

// conflictOutcome runs the commit-order race for one client and reports whether A's commit
// conflicted (1020). retry=true signals a transient retryable error (1007 etc., NOT 1020) —
// the caller re-runs with a fresh version.
type conflictOutcome struct {
	conflicted bool
	retry      bool
	resolved   string // DEBUG: prefix-stripped resolved getKey
}

func goConflictScenario(t *testing.T, pfx string, seed, pending []fuzzOp, sel selSpec, probeKey string) conflictOutcome {
	t.Helper()
	// Setup (clear + seed) in ONE committed txn; pin A's read version to its COMMIT version
	// so A reads CAUSALLY AFTER the setup. Otherwise A's own GRV can land BEFORE the setup
	// commit (FDB GRV is not strictly causal), and the setup's ClearRange — whose
	// write-conflict range [pfx, strinc(pfx)) spans the getKey conflict range — then falls in
	// A's commit window, tripping a spurious not_committed(1020). Each client's A gets an
	// independent GRV, so the two diverge nondeterministically under load — the CI flake this
	// fixes. Pinning to the exact setup version makes the conflict outcome a deterministic
	// function of the scenario (the getKey read-conflict vs B's write), not of GRV timing.
	r, err := gofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("go PrefixRange: %v", err)
	}
	setup, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go setup create: %v", err)
	}
	setup.ClearRange(r)
	applyGo(setup, seed, pfx)
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
	a.SetReadVersion(vSetup) // read exactly at the setup → the clear is NOT in A's window
	applyGo(a, pending, pfx)
	rk, gerr := a.GetKey(goSel(pfx, sel)).Get()
	if gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go A GetKey: %v", gerr)
	}
	resolved := "<offprefix>"
	if bytes.HasPrefix(rk, []byte(pfx)) {
		resolved = string(rk[len(pfx):])
	}
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("s")) // committable write, far from any probe
	// Concurrent B writes the probe and commits. Pin B to vSetup too (no fresh GRV) so B does
	// NOT ratchet the client's minAcceptableReadVersion past vSetup — otherwise A's commit at
	// vSetup is rejected client-side with transaction_too_old(1007) and the attempt is wasted,
	// a residual flake under load (codex). B's COMMIT version is still assigned fresh
	// (> vSetup), so the probe write remains in A's conflict window (vSetup, A_commit).
	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+probeKey), []byte("B"))
	if err := b.Commit().Get(); err != nil {
		if isFDBRetryable(err) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("go B commit: %v", err)
	}
	switch code := fdbErrorCode(a.Commit().Get()); code {
	case 0:
		return conflictOutcome{conflicted: false, resolved: resolved}
	case 1020:
		return conflictOutcome{conflicted: true, resolved: resolved}
	default:
		return conflictOutcome{retry: true} // 1007 etc. — transient
	}
}

func cgoConflictScenario(t *testing.T, pfx string, seed, pending []fuzzOp, sel selSpec, probeKey string) conflictOutcome {
	t.Helper()
	// Setup committed in one txn; pin A to its commit version (see goConflictScenario).
	r, err := cgofdb.PrefixRange([]byte(pfx))
	if err != nil {
		t.Fatalf("cgo PrefixRange: %v", err)
	}
	setup, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo setup create: %v", err)
	}
	setup.ClearRange(r)
	applyC(setup, seed, pfx)
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
	applyC(a, pending, pfx)
	rk, gerr := a.GetKey(cSel(pfx, sel)).Get()
	if gerr != nil {
		if isFDBRetryable(gerr) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("cgo A GetKey: %v", gerr)
	}
	resolved := "<offprefix>"
	if bytes.HasPrefix([]byte(rk), []byte(pfx)) {
		resolved = string(rk[len(pfx):])
	}
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("s"))
	// B pinned to vSetup too (no fresh GRV ratcheting the read-version floor — see goConflictScenario).
	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+probeKey), []byte("B"))
	if err := b.Commit().Get(); err != nil {
		if isFDBRetryable(err) {
			return conflictOutcome{retry: true}
		}
		t.Fatalf("cgo B commit: %v", err)
	}
	switch code := fdbErrorCode(a.Commit().Get()); code {
	case 0:
		return conflictOutcome{conflicted: false, resolved: resolved}
	case 1020:
		return conflictOutcome{conflicted: true, resolved: resolved}
	default:
		return conflictOutcome{retry: true}
	}
}

func runGetKeyConflictDifferential(t *testing.T, name string, seed, pending []fuzzOp, sel selSpec, probeKey string, wantConflict bool) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("%s: conflict differential did not clear transient errors in %d attempts", name, maxAttempts)
		}
		goPfx := fmt.Sprintf("gkconf_%d_%s_%d_go_", os.Getpid(), ns, attempt)
		cPfx := fmt.Sprintf("gkconf_%d_%s_%d_c_", os.Getpid(), ns, attempt)
		goOut := goConflictScenario(t, goPfx, seed, pending, sel, probeKey)
		cOut := cgoConflictScenario(t, cPfx, seed, pending, sel, probeKey)
		if goOut.retry || cOut.retry {
			continue // transient (1007) on either side — fresh prefixes + versions, retry
		}
		if goOut.conflicted != cOut.conflicted {
			t.Fatalf("%s: read-conflict DIVERGES: go-A conflicted=%v (resolved=%q) cgo-A conflicted=%v (probe=%q sel=%s)",
				name, goOut.conflicted, goOut.resolved, cOut.conflicted, probeKey, sel.name)
		}
		if goOut.conflicted != wantConflict {
			t.Fatalf("%s: both clients conflicted=%v but expected %v — conflict set wrong in BOTH (probe=%q sel=%s)",
				name, goOut.conflicted, wantConflict, probeKey, sel.name)
		}
		return
	}
}

// TestDifferential_GetKeyConflict pins the getKey read-conflict SET against libfdb_c. fuzzKeys
// = {a,b,c,d}; the selector is firstGreaterThan("a") (orEqual, offset 1) unless noted, so the
// conflict span runs from just after "a" to the resolved key. Probe keys land on each segment
// CLASS to exercise every arm of updateConflictMap.
func TestDifferential_GetKeyConflict(t *testing.T) {
	t.Parallel()
	b := func(s string) []byte { return []byte(s) }
	set := func(ki int, v string) fuzzOp { return fuzzOp{kind: fzSet, keyIdx: ki, operand: b(v)} }
	add := func(ki int, v string) fuzzOp { return fuzzOp{kind: fzAdd, keyIdx: ki, operand: b(v)} }
	crange := func(bi, ei int) fuzzOp { return fuzzOp{kind: fzClearRange, keyIdx: bi, key2Idx: ei} }
	fgt := func(ki int) selSpec { return selSpec{"FGT", ki, true, 1} } // firstGreaterThan(key)

	cases := []struct {
		name          string
		seed, pending []fuzzOp
		sel           selSpec
		probeKey      string // raw key suffix (within prefix) that B writes
		wantConflict  bool
	}{
		// INDEPENDENT pending write (Set c) — getKey(FGT a) resolves to c via the local write,
		// NO DB read at c, so C++ excludes c from the conflict. Probe ON c → must NOT conflict.
		// (Pre-fix go kept the full span → conflicted here. The distinguishing case.)
		{"independent_write_excluded", nil, []fuzzOp{set(2, "v")}, fgt(0), "c", false},
		// Same setup, probe in the UNMODIFIED gap (a,c) → DB read region → MUST conflict.
		{"unmodified_gap_conflicts", nil, []fuzzOp{set(2, "v")}, fgt(0), "b", true},
		// CLEARED range [b,d) then resolve to d (committed) — probe in the cleared part → no DB
		// read → must NOT conflict. (Pre-fix go over-conflicted here too.)
		{"cleared_range_excluded", []fuzzOp{set(3, "z")}, []fuzzOp{crange(1, 3)}, fgt(0), "b", false},
		// DEPENDENT atomic (Add onto a committed value) — getKey reads the DB base to resolve,
		// so C++ KEEPS the conflict. Probe ON c → MUST conflict. Proves the filter does not
		// UNDER-conflict on a dependent atomic (the codex #235 safety concern).
		{"dependent_atomic_conflicts", []fuzzOp{set(2, "\x05\x00\x00\x00")}, []fuzzOp{add(2, "\x01\x00\x00\x00")}, fgt(0), "c", true},
		// Probe OUTSIDE the span (d, beyond resolved c) → never in the conflict range.
		{"outside_span_no_conflict", nil, []fuzzOp{set(2, "v")}, fgt(0), "d", false},
	}
	// NOTE: the RYW-DISABLED conflict path (codex P2-2 — must use the full span, not the
	// filtered one) is NOT differential-tested here: the only scenario where the filter would
	// differ from the full span needs a local write INSIDE the read span, but libfdb_c rejects
	// reading a range overlapping a locally-written key under RYW-disabled with
	// client_invalid_operation (2000) — so the under-conflict is unreachable via the legal API.
	// The fix is pinned by a white-box unit test (TestAddGetKeyConflictRange_RYWDisabledFullSpan
	// in package client). The go-vs-cgo gap that go does NOT raise that 2000 is a distinct
	// option-semantics axis tracked under TODO item 3.
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runGetKeyConflictDifferential(t, tc.name, tc.seed, tc.pending, tc.sel, tc.probeKey, tc.wantConflict)
		})
	}
}
