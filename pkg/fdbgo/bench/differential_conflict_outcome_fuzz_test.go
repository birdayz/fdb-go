package bench

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// FuzzDifferential_ConflictOutcome is RFC-121's under-conflict guard. It applies the SAME random
// op mix (Set/Clear/Get/GetRange fwd+rev with varied limits — including empty-range, limited, and
// full-drain reads) to transaction A through BOTH clients, runs a concurrent committed write to a
// probe key, and asserts the commit/abort outcome is IDENTICAL (go == cgo). The whole point of the
// RFC-121 clamp + RYW-filter is to make Go's read-conflict set match libfdb_c's exactly; this fuzz
// proves the fix never goes too far the OTHER way.
//
// The catastrophic direction is `go=COMMIT, cgo=ABORT` — Go UNDER-conflicts, i.e. it dropped a
// read-conflict libfdb_c kept, losing serializability (strictly worse than the old over-conflict).
// That direction is a hard t.Fatalf with a screaming message; it is NEVER folded into the transient
// `retry` bucket (only genuine non-{commit,1020} errors are skipped, and only when neither client
// produced a decisive outcome). The over-conflict direction (`go=ABORT, cgo=COMMIT`) also fatals —
// any divergence from libfdb_c is a bug.
//
// Determinism: A and B are both pinned to the setup's COMMIT version (so B commits causally after
// setup but concurrently with A's read snapshot), exactly like the GetKey/GetRange conflict
// differentials — a non-causal GRV can't trip a spurious 1020. Given the fixed op mix and seed
// data, whether the probe key falls in A's read-conflict set is fully determined by the conflict-
// range computation, so the two clients MUST agree unless their conflict logic diverges.

const (
	cfSet = iota
	cfClear
	cfGet
	cfGetRange
	cfNumKinds
)

// cfNumSeeded keys k00..k09 are seeded; GetRange end indices run 0..cfNumSeeded so "k10" (> "k09",
// 2-digit padding keeps lexicographic == numeric order) is a valid "past the last seeded key"
// exclusive bound. The probe B writes is always a seeded key (0..cfNumSeeded-1).
const cfNumSeeded = 10

type cfOp struct {
	kind    int
	a, b    int // key indices; b = GetRange exclusive-end index (b > a)
	limit   int
	reverse bool
}

func cfKey(pfx string, i int) string { return fmt.Sprintf("%sk%02d", pfx, i) }

// decodeConflictOps walks the byte stream left-to-right (deterministic, seed-reproducible). Byte 0
// picks the probe key; the rest decode into A's op list (capped so a txn stays well under the FDB
// limits).
func decodeConflictOps(data []byte) (ops []cfOp, probeIdx int) {
	i := 0
	rd := func() byte {
		if i < len(data) {
			b := data[i]
			i++
			return b
		}
		return 0
	}
	probeIdx = int(rd()) % cfNumSeeded
	const maxOps = 12
	for i < len(data) && len(ops) < maxOps {
		op := cfOp{kind: int(rd()) % cfNumKinds}
		switch op.kind {
		case cfSet, cfClear, cfGet:
			op.a = int(rd()) % cfNumSeeded
		case cfGetRange:
			x := int(rd()) % (cfNumSeeded + 1) // 0..cfNumSeeded
			y := int(rd()) % (cfNumSeeded + 1)
			if x > y {
				x, y = y, x
			}
			op.a, op.b = x, y // [k0x, k0y); x==y → empty range
			op.limit = int(rd()) % (cfNumSeeded + 2)
			op.reverse = rd()&1 == 1
		}
		ops = append(ops, op)
	}
	return ops, probeIdx
}

var cfSeq atomic.Int64

func goConflictOutcomeRun(t *testing.T, pfx string, ops []cfOp, probeIdx int) conflictOutcome {
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
	for i := range cfNumSeeded {
		setup.Set(gofdb.Key(cfKey(pfx, i)), []byte("v"))
	}
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
	for _, op := range ops {
		switch op.kind {
		case cfSet:
			a.Set(gofdb.Key(cfKey(pfx, op.a)), []byte("A"))
		case cfClear:
			a.Clear(gofdb.Key(cfKey(pfx, op.a)))
		case cfGet:
			if _, gerr := a.Get(gofdb.Key(cfKey(pfx, op.a))).Get(); gerr != nil {
				if isFDBRetryable(gerr) {
					return conflictOutcome{retry: true}
				}
				t.Fatalf("go A Get: %v", gerr)
			}
		case cfGetRange:
			_, gerr := a.GetRange(
				gofdb.KeyRange{Begin: gofdb.Key(cfKey(pfx, op.a)), End: gofdb.Key(cfKey(pfx, op.b))},
				gofdb.RangeOptions{Limit: op.limit, Reverse: op.reverse},
			).GetSliceWithError()
			if gerr != nil {
				if isFDBRetryable(gerr) {
					return conflictOutcome{retry: true}
				}
				t.Fatalf("go A GetRange: %v", gerr)
			}
		}
	}
	a.Set(gofdb.Key(pfx+"~sentinel"), []byte("s")) // ensure A reaches the resolver

	b, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(gofdb.Key(cfKey(pfx, probeIdx)), []byte("B"))
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

func cgoConflictOutcomeRun(t *testing.T, pfx string, ops []cfOp, probeIdx int) conflictOutcome {
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
	for i := range cfNumSeeded {
		setup.Set(cgofdb.Key(cfKey(pfx, i)), []byte("v"))
	}
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
	for _, op := range ops {
		switch op.kind {
		case cfSet:
			a.Set(cgofdb.Key(cfKey(pfx, op.a)), []byte("A"))
		case cfClear:
			a.Clear(cgofdb.Key(cfKey(pfx, op.a)))
		case cfGet:
			if _, gerr := a.Get(cgofdb.Key(cfKey(pfx, op.a))).Get(); gerr != nil {
				if isFDBRetryable(gerr) {
					return conflictOutcome{retry: true}
				}
				t.Fatalf("cgo A Get: %v", gerr)
			}
		case cfGetRange:
			_, gerr := a.GetRange(
				cgofdb.KeyRange{Begin: cgofdb.Key(cfKey(pfx, op.a)), End: cgofdb.Key(cfKey(pfx, op.b))},
				cgofdb.RangeOptions{Limit: op.limit, Reverse: op.reverse},
			).GetSliceWithError()
			if gerr != nil {
				if isFDBRetryable(gerr) {
					return conflictOutcome{retry: true}
				}
				t.Fatalf("cgo A GetRange: %v", gerr)
			}
		}
	}
	a.Set(cgofdb.Key(pfx+"~sentinel"), []byte("s"))

	b, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo B create: %v", err)
	}
	defer b.Cancel()
	b.SetReadVersion(vSetup)
	b.Set(cgofdb.Key(cfKey(pfx, probeIdx)), []byte("B"))
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

func FuzzDifferential_ConflictOutcome(f *testing.F) {
	// D1 (limited fwd read, probe in unread tail → both COMMIT): probe k05, GetRange([k00,k10),limit=3).
	f.Add([]byte{5, cfGetRange, 0, cfNumSeeded, 3, 0})
	// D2 (read-own-write, probe set+read → both COMMIT): probe k02, Set(k02), Get(k02).
	f.Add([]byte{2, cfSet, 2, cfGet, 2})
	// Phantom (full unlimited drain, probe inside → both ABORT): probe k04, GetRange([k00,k10),limit=0).
	f.Add([]byte{4, cfGetRange, 0, cfNumSeeded, 0, 0})
	// Reverse clamp (limited reverse read, probe in scanned suffix → both ABORT): probe k08, reverse limit=3.
	f.Add([]byte{8, cfGetRange, 0, cfNumSeeded, 3, 1})
	// Mixed: Set + Get + a couple of ranges.
	f.Add([]byte{1, cfSet, 1, cfGet, 0, cfGetRange, 2, 7, 4, 0, cfGetRange, 3, 9, 2, 1})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13})

	f.Fuzz(func(t *testing.T, data []byte) {
		ops, probeIdx := decodeConflictOps(data)
		if len(ops) == 0 {
			return
		}
		ns := strings.ReplaceAll(t.Name(), "/", "_")
		const maxAttempts = 12
		for attempt := 0; ; attempt++ {
			if attempt >= maxAttempts {
				return // could not clear transients in budget; skip this input (no false failure)
			}
			seq := cfSeq.Add(1)
			goPfx := fmt.Sprintf("cfout_%d_%s_%d_go_", os.Getpid(), ns, seq)
			cPfx := fmt.Sprintf("cfout_%d_%s_%d_c_", os.Getpid(), ns, seq)
			goOut := goConflictOutcomeRun(t, goPfx, ops, probeIdx)
			cOut := cgoConflictOutcomeRun(t, cPfx, ops, probeIdx)
			if goOut.retry || cOut.retry {
				continue
			}
			if goOut.conflicted == cOut.conflicted {
				return // agreement — the fix holds for this op mix
			}
			if !goOut.conflicted && cOut.conflicted {
				t.Fatalf("RFC-121 UNDER-CONFLICT (lost serializability): go COMMITTED but libfdb_c ABORTED "+
					"(1020) — Go dropped a read-conflict the C client kept. ops=%+v probe=k%02d", ops, probeIdx)
			}
			t.Fatalf("RFC-121 conflict-outcome divergence (over-conflict): go aborted, libfdb_c committed — "+
				"Go added a read-conflict libfdb_c did not. ops=%+v probe=k%02d", ops, probeIdx)
		}
	})
}
