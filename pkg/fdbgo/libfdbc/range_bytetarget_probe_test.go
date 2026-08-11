//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/libfdbc"
)

// TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget measures WHERE libfdb_c's per-mode byte
// target cuts a batch, and pins the answer because it contradicts the mechanism the
// GetRangeLimits port was briefed against.
//
// THE BRIEFED MECHANISM, which this refutes: "the request's byte limit is irrelevant; the
// division comes from the loop budget, so port GetRangeLimits{rows,bytes} into the scan loop
// and leave LimitBytes alone." The supporting measurement for the second half is real —
// lowering the request's LimitBytes ALONE changes nothing, because the Go scan loop absorbs a
// truncated reply and re-queries until its ROW budget is filled. But the conclusion drawn from
// it does not follow, and this test is the evidence.
//
// WHAT C++ ACTUALLY DOES — two cooperating halves, both required:
//
//  1. transformRangeLimits (NativeAPI.actor.cpp:4223) puts the budget ON THE REQUEST as
//     req.limitBytes = min(REPLY_BYTE_LIMIT, limits.bytes). It is called from BOTH range loops
//     (getExactRange:4299, getRange:4681). The STORAGE SERVER truncates the reply there.
//  2. The loop then STOPS instead of absorbing, via the soft-byte-limit early return at
//     getExactRange:4415 — `if (limits.hasSatisfiedMinRows() && output.size() > 0)`, where
//     hasSatisfiedMinRows() = hasByteLimit() && minRows == 0 (:2875) and minRows starts at 1
//     (FDBTypes.h GetRangeLimits ctor), so ANY non-empty reply satisfies it.
//
// So with a byte target set, one range call is exactly ONE server reply, and the batch boundary
// is wherever the SERVER truncated. The client-side budget's role is to end the call, not to
// choose the boundary.
//
// WHY THAT DISTINCTION IS LOAD-BEARING rather than pedantic: the two rules round opposite ways.
// A client-side budget over an untruncated reply stops at the first row that drives the budget
// to zero (an overshoot past the target); the server stops at its own accounting of what fits.
// They disagree by a row wherever the target is not a multiple of the row size. This test
// measures a target where they disagree, so a port that implements only the loop half is
// provably off by one there — and off by three at 8192 — rather than merely differently
// motivated.
//
// A pure-Go port therefore cannot reproduce C's boundary by modelling it. It must send the same
// per-mode LimitBytes and let the same storage server apply the same rule.
func TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget(t *testing.T) {
	t.Parallel()

	clusterFile := startCluster(t)

	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c database: %v", err)
	}
	defer cdb.Close()

	const (
		prefix   = "bytetgt_"
		n        = 60
		valueLen = 200
	)
	begin := []byte(prefix)
	end := []byte(prefix + "~")

	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("create seed transaction: %v", err)
	}
	keyLen := 0
	for i := 0; i < n; i++ {
		k := []byte(fmt.Sprintf("%s%04d", prefix, i))
		keyLen = len(k)
		tr.Set(k, bytes.Repeat([]byte{byte('a' + i%26)}, valueLen))
	}
	if err := tr.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	tr.Close()

	// C++'s per-row charge against a GetRangeLimits byte budget: 8 + key + value
	// (NativeAPI.actor.cpp:2823, the single-KeyValueRef decrement form).
	perRow := 8 + keyLen + valueLen

	// clientSideBudget is the batch a loop-only port would produce: fetch under the flat
	// REPLY_BYTE_LIMIT, then subtract per row until the budget hits zero. The row that drives
	// it to zero is included, so the batch overshoots the target.
	clientSideBudget := func(target int) int {
		remaining := target
		for i := 1; i <= n; i++ {
			if remaining -= perRow; remaining <= 0 {
				return i
			}
		}
		return n
	}

	// firstBatch drives ONE raw fdb_transaction_get_range with an explicit target_bytes. Mode
	// SERIAL's own mode_bytes is 80000, so min(target_bytes, 80000) = target_bytes and the
	// sweep value is the effective budget (fdb_c.cpp:1026-1029).
	firstBatch := func(targetBytes int) (int, bool) {
		ctr, err := cdb.CreateTransaction()
		if err != nil {
			t.Fatalf("create transaction: %v", err)
		}
		defer ctr.Close()
		batch, more, err := libfdbc.CGetRangeBatch(ctr, begin, end,
			0 /* limit: unlimited */, targetBytes,
			libfdbc.CStreamingModeSerial, 1 /* iteration */, false /* reverse */, false /* snapshot */)
		if err != nil {
			t.Fatalf("CGetRangeBatch(target_bytes=%d): %v", targetBytes, err)
		}
		return len(batch), more
	}

	type arm struct {
		target int
		// wantC is libfdb_c's measured first-batch row count. Literal values, not a second
		// derivation of the same rule: comparing C against a model that shares its inputs
		// would hold vacuously if both moved together.
		wantC int
	}
	arms := []arm{
		{256, 2},
		{1000, 5},
		{4096, 18},
		{8192, 35},
	}

	disagreements := 0
	for _, a := range arms {
		got, more, want := 0, false, a.wantC
		got, more = firstBatch(a.target)
		model := clientSideBudget(a.target)

		t.Logf("MEASURED target_bytes=%-5d libfdb_c first batch = %-3d (more=%v) | loop-only client-side model = %-3d  [perRow=%d]",
			a.target, got, more, model, perRow)

		if got != want {
			t.Errorf("target_bytes=%d: libfdb_c returned %d rows, want %d. This test pins C's "+
				"measured division; a change means either the storage server's reply-size "+
				"accounting moved or the harness stopped driving one raw get_range per call",
				a.target, got, want)
		}
		if got == 0 {
			t.Errorf("target_bytes=%d returned ZERO rows; minRows defaults to 1 "+
				"(FDBTypes.h GetRangeLimits ctor), so a byte-limited request must make progress", a.target)
		}
		// The whole range is 60 rows of ~212 bytes ~= 12.7 KB, so every swept target below
		// that must leave more data behind. A more=false here would mean the call drained the
		// range and the arm is not measuring a byte-driven cut at all.
		if !more {
			t.Errorf("target_bytes=%d reported more=false over a %d-row range — this arm is not "+
				"measuring a truncated batch", a.target, n)
		}
		if got != model {
			disagreements++
		}
	}

	// THE REFUTATION, asserted rather than logged. If a loop-only client-side budget predicted
	// C's cut everywhere, porting only the loop half would be sufficient and the brief would be
	// right. It does not: it is wrong at LARGE's own 4096 target (19 vs 18) and by three rows
	// at 8192. Requiring a NON-ZERO disagreement count is what keeps this from passing
	// vacuously if perRow or the row shape drifts into a degenerate alignment.
	if disagreements == 0 {
		t.Fatalf("the loop-only client-side budget model predicted libfdb_c's cut at EVERY swept "+
			"target (perRow=%d). That would mean a scan-loop byte budget alone reproduces C's "+
			"division and the request's LimitBytes is genuinely irrelevant — the opposite of what "+
			"this test exists to pin. Re-derive before trusting it: at these row sizes the two "+
			"rules must disagree at 4096 and 8192", perRow)
	}
	t.Logf("VERDICT: the loop-only client-side budget mispredicts libfdb_c at %d of %d swept targets. "+
		"C's boundary is set by the SERVER truncating at req.limitBytes (transformRangeLimits, "+
		"NativeAPI.actor.cpp:4223), and the loop's byte budget only ENDS the call (soft byte limit, "+
		":4415). A pure-Go port must send the per-mode LimitBytes on the request; a scan-loop "+
		"budget alone cannot reproduce this division.", disagreements, len(arms))
}
