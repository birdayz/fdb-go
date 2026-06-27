package bench

import (
	"fmt"
	"os"
	"strings"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Explicit conflict-range API differential vs libfdb_c — RFC-064.
//
// AddReadConflictRange/Key + AddWriteConflictRange/Key feed the resolver and decide isolation. An
// empirical probe found NO divergence (edges + conflict outcome match go==cgo); this pins that.
// Reuses the RFC-058 version-pinning discipline (conflictOutcome, defined in
// differential_getkey_conflict_test.go): both A and B SetReadVersion(vSetup) so the outcome is a
// deterministic function of the scenario, not GRV timing; transient (1007) → retry the scenario
// with fresh prefixes/versions.
//
// Conflict-range key layout per prefix: r0 < r5 < r9 < zz (and q < r0). A read-conflict RANGE is
// [r0, r9) — half-open: r0 conflicts (inclusive begin), r9 does NOT (exclusive end). A
// read-conflict KEY r5 is [r5, r5\x00) — only r5 conflicts.

// crSetup commits a clear+seed in one txn and returns the committed version to pin A and B to.
func crSetupGo(t *testing.T, pfx string) (int64, bool) {
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
	setup.Set(gofdb.Key(pfx+"seed"), []byte("s"))
	if err := setup.Commit().Get(); err != nil {
		setup.Cancel()
		return 0, true // retry
	}
	v, err := setup.GetCommittedVersion()
	if err != nil {
		t.Fatalf("go setup committed version: %v", err)
	}
	return v, false
}

func crSetupCgo(t *testing.T, pfx string) (int64, bool) {
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
	setup.Set(cgofdb.Key(pfx+"seed"), []byte("s"))
	if err := setup.Commit().Get(); err != nil {
		setup.Cancel()
		return 0, true
	}
	v, err := setup.GetCommittedVersion()
	if err != nil {
		t.Fatalf("cgo setup committed version: %v", err)
	}
	return v, false
}

// addReadConflict adds either the Range or the Key variant on key r5 / range [r0,r9).
func addReadConflictGo(tx gofdb.Transaction, pfx string, keyVariant bool) {
	if keyVariant {
		_ = tx.AddReadConflictKey(gofdb.Key(pfx + "r5"))
		return
	}
	_ = tx.AddReadConflictRange(gofdb.KeyRange{Begin: gofdb.Key(pfx + "r0"), End: gofdb.Key(pfx + "r9")})
}

func addReadConflictCgo(tx cgofdb.Transaction, pfx string, keyVariant bool) {
	if keyVariant {
		_ = tx.AddReadConflictKey(cgofdb.Key(pfx + "r5"))
		return
	}
	_ = tx.AddReadConflictRange(cgofdb.KeyRange{Begin: cgofdb.Key(pfx + "r0"), End: cgofdb.Key(pfx + "r9")})
}

// goReadConflict: A pins vSetup, adds a read-conflict (range or key), sets a sentinel; B (pinned)
// writes the probe and commits; A commits → 1020 iff the probe is in A's read-conflict set.
func goReadConflict(t *testing.T, pfx string, keyVariant bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupGo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	addReadConflictGo(a, pfx, keyVariant)
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("x"))

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+probe), []byte("B"))
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

func cgoReadConflict(t *testing.T, pfx string, keyVariant bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupCgo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	addReadConflictCgo(a, pfx, keyVariant)
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("x"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+probe), []byte("B"))
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

// goWriteConflict: A pins vSetup, adds a WRITE-conflict (range/key), commits (succeeds, marking
// the range written at A's commit version); reader R (pinned to vSetup, before A's commit) adds a
// read-conflict on the probe and commits → 1020 iff the probe is in A's write-conflict set.
func goWriteConflict(t *testing.T, pfx string, keyVariant bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupGo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	if keyVariant {
		_ = a.AddWriteConflictKey(gofdb.Key(pfx + "r5"))
	} else {
		_ = a.AddWriteConflictRange(gofdb.KeyRange{Begin: gofdb.Key(pfx + "r0"), End: gofdb.Key(pfx + "r9")})
	}
	if code := fdbErrorCode(a.Commit().Get()); code != 0 {
		return conflictOutcome{retry: true} // A should commit; transient otherwise
	}

	r, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go R create: %v", err)
	}
	defer r.Cancel()
	r.SetReadVersion(vSetup)
	r.AddReadConflictKey(gofdb.Key(pfx + probe))
	r.Set(gofdb.Key(pfx+"~rsentinel"), []byte("x"))
	switch fdbErrorCode(r.Commit().Get()) {
	case 0:
		return conflictOutcome{conflicted: false}
	case 1020:
		return conflictOutcome{conflicted: true}
	default:
		return conflictOutcome{retry: true}
	}
}

func cgoWriteConflict(t *testing.T, pfx string, keyVariant bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupCgo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	if keyVariant {
		_ = a.AddWriteConflictKey(cgofdb.Key(pfx + "r5"))
	} else {
		_ = a.AddWriteConflictRange(cgofdb.KeyRange{Begin: cgofdb.Key(pfx + "r0"), End: cgofdb.Key(pfx + "r9")})
	}
	if code := fdbErrorCode(a.Commit().Get()); code != 0 {
		return conflictOutcome{retry: true}
	}

	r, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo R create: %v", err)
	}
	defer r.Cancel()
	r.SetReadVersion(vSetup)
	r.AddReadConflictKey(cgofdb.Key(pfx + probe))
	r.Set(cgofdb.Key(pfx+"~rsentinel"), []byte("x"))
	switch fdbErrorCode(r.Commit().Get()) {
	case 0:
		return conflictOutcome{conflicted: false}
	case 1020:
		return conflictOutcome{conflicted: true}
	default:
		return conflictOutcome{retry: true}
	}
}

func runConflictDifferential(t *testing.T, name string, goFn, cgoFn func(*testing.T, string, bool, string) conflictOutcome, keyVariant bool, probe string, wantConflict bool) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("%s: did not clear transient errors in %d attempts", name, maxAttempts)
		}
		goPfx := fmt.Sprintf("crconf_%d_%s_%d_go_", os.Getpid(), ns, attempt)
		cPfx := fmt.Sprintf("crconf_%d_%s_%d_c_", os.Getpid(), ns, attempt)
		goOut := goFn(t, goPfx, keyVariant, probe)
		cOut := cgoFn(t, cPfx, keyVariant, probe)
		if goOut.retry || cOut.retry {
			continue
		}
		if goOut.conflicted != cOut.conflicted {
			t.Fatalf("%s: conflict DIVERGES: go=%v cgo=%v (keyVariant=%v probe=%q)", name, goOut.conflicted, cOut.conflicted, keyVariant, probe)
		}
		if goOut.conflicted != wantConflict {
			t.Fatalf("%s: both conflicted=%v but expected %v (keyVariant=%v probe=%q)", name, goOut.conflicted, wantConflict, keyVariant, probe)
		}
		return
	}
}

func TestDifferential_ReadConflictRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		keyVariant   bool
		probe        string
		wantConflict bool
	}{
		// RANGE [r0, r9): half-open — begin inclusive, end exclusive.
		{"range_begin_r0", false, "r0", true},  // == begin → conflict
		{"range_mid_r5", false, "r5", true},    // middle → conflict
		{"range_end_r9", false, "r9", false},   // == end (exclusive) → NO conflict
		{"range_above_zz", false, "zz", false}, // > end → NO conflict
		{"range_below_q", false, "q", false},   // < begin → NO conflict
		// KEY r5 → [r5, r5\x00): only r5.
		{"key_exact_r5", true, "r5", true},
		{"key_other_r6", true, "r6", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runConflictDifferential(t, c.name, goReadConflict, cgoReadConflict, c.keyVariant, c.probe, c.wantConflict)
		})
	}
}

func TestDifferential_WriteConflictRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		keyVariant   bool
		probe        string
		wantConflict bool
	}{
		{"range_begin_r0", false, "r0", true},
		{"range_mid_r5", false, "r5", true},
		{"range_end_r9", false, "r9", false},
		{"range_above_zz", false, "zz", false},
		{"range_below_q", false, "q", false}, // < begin → NO conflict (symmetric with the read test)
		{"key_exact_r5", true, "r5", true},
		{"key_other_r6", true, "r6", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runConflictDifferential(t, c.name, goWriteConflict, cgoWriteConflict, c.keyVariant, c.probe, c.wantConflict)
		})
	}
}

// goSnapshotConflict: A reads probe (snapshot iff `snapshot`), B writes probe, A commits.
// A SNAPSHOT read adds NO read-conflict (C++ gates every conflictRange.send on !snapshot), so A
// commits cleanly; a regular read DOES conflict. The `snapshot` bool rides the keyVariant param.
func goSnapshotConflict(t *testing.T, pfx string, snapshot bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupGo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	if snapshot {
		if _, e := a.Snapshot().Get(gofdb.Key(pfx + probe)).Get(); e != nil {
			return conflictOutcome{retry: true}
		}
	} else {
		if _, e := a.Get(gofdb.Key(pfx + probe)).Get(); e != nil {
			return conflictOutcome{retry: true}
		}
	}
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("x"))

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+probe), []byte("B"))
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

func cgoSnapshotConflict(t *testing.T, pfx string, snapshot bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupCgo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	if snapshot {
		if _, e := a.Snapshot().Get(cgofdb.Key(pfx + probe)).Get(); e != nil {
			return conflictOutcome{retry: true}
		}
	} else {
		if _, e := a.Get(cgofdb.Key(pfx + probe)).Get(); e != nil {
			return conflictOutcome{retry: true}
		}
	}
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("x"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+probe), []byte("B"))
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

// TestDifferential_SnapshotReadNoConflict (FDB-C++ dev): a SNAPSHOT read adds no read-conflict, so
// a concurrent write to the read key does NOT conflict the reader; a regular read DOES. Both
// outcomes must match libfdb_c.
func TestDifferential_SnapshotReadNoConflict(t *testing.T) {
	t.Parallel()
	// keyVariant param carries `snapshot`; probe is the read/written key.
	t.Run("snapshot_read_no_conflict", func(t *testing.T) {
		t.Parallel()
		runConflictDifferential(t, "snapshot", goSnapshotConflict, cgoSnapshotConflict, true /*snapshot*/, "r5", false /*wantConflict*/)
	})
	t.Run("regular_read_conflicts", func(t *testing.T) {
		t.Parallel()
		runConflictDifferential(t, "regular", goSnapshotConflict, cgoSnapshotConflict, false /*snapshot*/, "r5", true /*wantConflict*/)
	})
}

// goSelfConflict: A read-conflicts probe AND writes it; B writes probe; A still conflicts (its own
// write does not suppress the read-conflict). keyVariant unused (always the Key variant).
func goSelfConflict(t *testing.T, pfx string, _ bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupGo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	a.AddReadConflictKey(gofdb.Key(pfx + probe))
	a.Set(gofdb.Key(pfx+probe), []byte("A")) // A writes the same key it read-conflicts

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(pfx+probe), []byte("B"))
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

func cgoSelfConflict(t *testing.T, pfx string, _ bool, probe string) conflictOutcome {
	t.Helper()
	vSetup, retry := crSetupCgo(t, pfx)
	if retry {
		return conflictOutcome{retry: true}
	}
	a, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo A create: %v", err)
	}
	defer a.Cancel()
	a.SetReadVersion(vSetup)
	a.AddReadConflictKey(cgofdb.Key(pfx + probe))
	a.Set(cgofdb.Key(pfx+probe), []byte("A"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(pfx+probe), []byte("B"))
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

// TestDifferential_SelfWriteReadConflict (FDB-C++ dev): a read-conflict on a key the txn also
// writes still conflicts with a concurrent write — the self-write does not suppress it. go==cgo.
func TestDifferential_SelfWriteReadConflict(t *testing.T) {
	t.Parallel()
	runConflictDifferential(t, "self_write_read_conflict", goSelfConflict, cgoSelfConflict, false, "r5", true)
}

// TestDifferential_ConflictRangeEdges pins the immediate error/accept behavior of the explicit
// conflict-range API for BOTH the read and the write range methods: inverted (begin>end → 2005),
// empty (begin==end → accept), normal (accept), and an oversized (>10KB) key in the range.
// go==cgo asserted. (Covers AddWriteConflictRange too — its validation could diverge from the
// read path independently; codex.)
func TestDifferential_ConflictRangeEdges(t *testing.T) {
	t.Parallel()
	big := make([]byte, 11000)
	for i := range big {
		big[i] = 'x'
	}
	bigEnd := append(append([]byte{}, big...), 0x00)

	// goCode/cgoCode run the chosen range method (read or write) with the given begin/end and
	// return the immediate error code.
	goCode := func(write bool, begin, end []byte) int {
		tr, _ := goClient.CreateTransaction()
		defer tr.Cancel()
		kr := gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}
		if write {
			return fdbErrorCode(tr.AddWriteConflictRange(kr))
		}
		return fdbErrorCode(tr.AddReadConflictRange(kr))
	}
	cgoCode := func(write bool, begin, end []byte) int {
		tr, _ := cgoClient.CreateTransaction()
		defer tr.Cancel()
		kr := cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)}
		if write {
			return fdbErrorCode(tr.AddWriteConflictRange(kr))
		}
		return fdbErrorCode(tr.AddReadConflictRange(kr))
	}

	shapes := []struct {
		name       string
		begin, end []byte
	}{
		{"inverted", []byte("cre_z"), []byte("cre_a")},
		{"empty", []byte("cre_a"), []byte("cre_a")},
		{"normal", []byte("cre_a"), []byte("cre_z")},
		{"oversized", big, bigEnd},
	}
	for _, write := range []bool{false, true} {
		write := write
		method := "read"
		if write {
			method = "write"
		}
		for _, s := range shapes {
			s := s
			t.Run(method+"_"+s.name, func(t *testing.T) {
				t.Parallel()
				g, cg := goCode(write, s.begin, s.end), cgoCode(write, s.begin, s.end)
				if g != cg {
					t.Fatalf("%s %s: AddConflictRange code differs: go=%d cgo=%d", method, s.name, g, cg)
				}
			})
		}
	}
}
