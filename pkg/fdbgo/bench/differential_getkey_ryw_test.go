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

// GetKey read-your-writes differential vs libfdb_c — RFC-056 sub-item (1).
//
// RFC-055's RYW-read differential (differential_ryw_test.go) compares Get + GetRange
// over pending writes but DELIBERATELY EXCLUDES GetKey (see its NOTE at :132): the
// Go client's GetKey resolves selectors against storage ONLY and never merges the
// transaction's pending writes, unlike C++ resolveKeySelectorFromCache. This file is
// that missing axis: open one uncommitted txn per client at a shared read version,
// seed identical storage, apply identical pending writes, then resolve key SELECTORS
// (all four kinds + offset>1 + orEqual) WITHIN the uncommitted txns and compare the
// resolved keys byte-for-byte.
//
// On master this is EXPECTED TO FAIL on any case where a pending Set/Clear shifts
// where a selector lands — that failure is the proof of the divergence, with the
// fuzzer producing a minimized seed. Once the faithful resolveKeySelectorFromCache
// port lands, this becomes the green regression net.
//
// Determinism: shared read version V + identical seeded storage + identical pending
// ops ⇒ resolution is a pure function of (storage@V, op stack), so any difference is
// a pure RYW key-selector-resolution divergence. Both txns are explicitly Cancel()ed;
// never commits, so getKey's read-conflict range is irrelevant here.

// selSpec is one key selector parameterization, built for both clients in parallel.
type selSpec struct {
	name    string
	keyIdx  int
	orEqual bool
	offset  int
}

// getKeySelectors covers the resolution dimensions that distinguish RYW from
// storage-only: the four canonical selectors (offset ±1 baked in via orEqual), plus
// offset>1 with orEqual true/false (the case a merged-GetRange shortcut got WRONG),
// plus a backward offset<−1.
func getKeySelectors() []selSpec {
	var out []selSpec
	for ki := range fuzzKeys {
		out = append(out,
			selSpec{"FGE", ki, false, 1}, // firstGreaterOrEqual
			selSpec{"FGT", ki, true, 1},  // firstGreaterThan
			selSpec{"LLE", ki, true, 0},  // lastLessOrEqual
			selSpec{"LLT", ki, false, 0}, // lastLessThan
			selSpec{"OE_OFF2", ki, true, 2},
			selSpec{"NE_OFF2", ki, false, 2},
			selSpec{"OE_BACK2", ki, true, -2},
		)
	}
	return out
}

func goSel(pfx string, s selSpec) gofdb.KeySelector {
	return gofdb.KeySelector{Key: gofdb.Key(pfx + fuzzKeys[s.keyIdx]), OrEqual: s.orEqual, Offset: s.offset}
}

func cSel(pfx string, s selSpec) cgofdb.KeySelector {
	return cgofdb.KeySelector{Key: cgofdb.Key(pfx + fuzzKeys[s.keyIdx]), OrEqual: s.orEqual, Offset: s.offset}
}

// runGetKeyRYWDifferential seeds identical storage, applies identical pending writes
// to one uncommitted txn per client at a shared read version, and compares the
// RYW-resolved GetKey results for every selector in getKeySelectors().
func runGetKeyRYWDifferential(t *testing.T, label string, seed, pending []fuzzOp) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	goPfx := fmt.Sprintf("gkryw_%d_%s_go_", os.Getpid(), ns)
	cPfx := fmt.Sprintf("gkryw_%d_%s_c_", os.Getpid(), ns)
	clearPrefix(t, goPfx)
	clearPrefix(t, cPfx)

	// SEAL the prefix with sentinel keys: 2 below "a" and 2 above "d" (the fuzzKeys
	// range). A key selector with |offset| up to 2 over the probe keys {a,b,c,d} then
	// always resolves WITHIN [prefix+\x00, prefix+\xef] — it never walks off the prefix
	// into the concurrently-shared keyspace where the two clients' prefixes have
	// different neighbours (which would make a deep-offset/backward selector resolve
	// differently per client even though the prefix-local key set is identical). The
	// sentinels are identical in both prefixes, so they compare equal. Without them, an
	// offset like OE_BACK2(d) escapes the prefix and the result-in-prefix clamp is not
	// enough to keep the comparison sound.
	sentinels := []string{"\x00", "\x01", "\xee", "\xef"}
	if _, err := goClient.Transact(func(tx gofdb.Transaction) (any, error) {
		for _, s := range sentinels {
			tx.Set(gofdb.Key(goPfx+s), []byte("s"))
		}
		applyGo(tx, seed, goPfx)
		return nil, nil
	}); err != nil {
		t.Fatalf("%s: seed go: %v", label, err)
	}
	mustCGo(t, func(tx cgofdb.Transaction) {
		for _, s := range sentinels {
			tx.Set(cgofdb.Key(cPfx+s), []byte("s"))
		}
		applyC(tx, seed, cPfx)
	})

	seq := fmt.Sprintf("seed=%s pending=%s", fmtTxns([][]fuzzOp{seed}), fmtTxns([][]fuzzOp{pending}))

	// Compare under a fresh shared read version, retrying on a TRANSIENT retryable error
	// (e.g. transaction_too_old(1007) when a slow run under heavy parallel-container load
	// drifts past the 5s MVCC window). Such an error is the RFC-056 item (2) go-vs-cgo
	// read-version asymmetry, NOT a resolution divergence — re-capture a fresh version
	// and retry rather than flag a false error mismatch. The RESOLUTION is the invariant
	// under test; transient version-staleness is not.
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("%s: GetKey differential: retryable errors (RFC-056 item 2 read-version staleness) did not clear in %d attempts\n%s", label, maxAttempts, seq)
		}
		v := freshSharedVersion(t)
		goTxn, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatalf("%s: go CreateTransaction: %v", label, err)
		}
		cTxn, err := cgoClient.CreateTransaction()
		if err != nil {
			goTxn.Cancel()
			t.Fatalf("%s: cgo CreateTransaction: %v", label, err)
		}
		goTxn.SetReadVersion(v)
		cTxn.SetReadVersion(v)
		applyGo(goTxn, pending, goPfx)
		applyC(cTxn, pending, cPfx)

		retry := false
		for _, s := range getKeySelectors() {
			goK, goErr := goTxn.GetKey(goSel(goPfx, s)).Get()
			cK, cErr := cTxn.GetKey(cSel(cPfx, s)).Get()
			if isFDBRetryable(goErr) || isFDBRetryable(cErr) {
				retry = true
				break
			}
			if (goErr == nil) != (cErr == nil) {
				goTxn.Cancel()
				cTxn.Cancel()
				t.Fatalf("%s: GetKey %s(%q) error mismatch: go=%v cgo=%v\n%s",
					label, s.name, fuzzKeys[s.keyIdx], goErr, cErr, seq)
			}
			if goErr != nil {
				continue
			}
			// Clamp to the per-test prefix: a selector that resolves off-prefix lands in
			// the concurrently-shared keyspace. Both clients read at the SAME version v so
			// they'd still agree, but an off-prefix result is not a meaningful RYW probe —
			// only compare when both land inside [prefix, prefix+\xff).
			goIn := bytes.HasPrefix(goK, []byte(goPfx))
			cIn := bytes.HasPrefix(cK, []byte(cPfx))
			if !goIn || !cIn {
				continue
			}
			goRel := goK[len(goPfx):]
			cRel := cK[len(cPfx):]
			if !bytes.Equal(goRel, cRel) {
				goTxn.Cancel()
				cTxn.Cancel()
				t.Fatalf("%s: GetKey %s(%q) RYW-resolved differs: go=%q cgo=%q\n%s",
					label, s.name, fuzzKeys[s.keyIdx], goRel, cRel, seq)
			}
		}
		goTxn.Cancel()
		cTxn.Cancel()
		if !retry {
			return
		}
	}
}

// isFDBRetryable reports whether err is a transient, retryable FDB error from EITHER
// client — the gofdb facade Error (returned by GetKey().Get() via convertError) or the
// libfdb_c cgofdb.Error. The differential retries on these (with a fresh read version)
// instead of treating them as a divergence.
func isFDBRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ge gofdb.Error
	if errors.As(err, &ge) {
		return ge.Retryable()
	}
	var ce cgofdb.Error
	if errors.As(err, &ce) {
		switch ce.Code {
		case 1004, 1007, 1009, 1020, 1021, 1037, 1038, 1042, 1051, 1078, 1213: // too_old / future / not_committed / process_behind / throttled / mem-limits
			return true
		}
	}
	return false
}

func TestDifferential_GetKeyRYW(t *testing.T) {
	t.Parallel()
	b := func(s string) []byte { return []byte(s) }
	set := func(ki int, v string) fuzzOp { return fuzzOp{kind: fzSet, keyIdx: ki, operand: b(v)} }
	clr := func(ki int) fuzzOp { return fuzzOp{kind: fzClear, keyIdx: ki} }
	crange := func(bi, ei int) fuzzOp { return fuzzOp{kind: fzClearRange, keyIdx: bi, key2Idx: ei} }

	cases := []struct {
		name          string
		seed, pending []fuzzOp
	}{
		// pending Set fills a gap between seeded keys → a forward selector landing in
		// the gap must resolve to the pending key, not skip to the next seeded key.
		{"pending_set_fills_gap", []fuzzOp{set(0, "x"), set(2, "x")}, []fuzzOp{set(1, "x")}},
		// pending Clear of a seeded key → a selector that would land on it must skip.
		{"pending_clear_shifts", []fuzzOp{set(0, "x"), set(1, "x"), set(2, "x")}, []fuzzOp{clr(1)}},
		// pending ClearRange hole → selectors must skip the whole hole.
		{"pending_clearrange_hole", []fuzzOp{set(0, "x"), set(1, "x"), set(2, "x"), set(3, "x")}, []fuzzOp{crange(1, 3)}},
		// pending Set BEFORE all seeded keys → backward/offset selectors shift.
		{"pending_set_new_first", []fuzzOp{set(2, "x"), set(3, "x")}, []fuzzOp{set(0, "x")}},
		// pure pending (no seed) → resolution is entirely from the write map.
		{"pending_only", nil, []fuzzOp{set(0, "x"), set(2, "x")}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runGetKeyRYWDifferential(t, tc.name, tc.seed, tc.pending)
		})
	}
}

// FuzzDifferential_GetKeyRYW drives random (seed, pending) op sequences and compares
// GetKey resolution over the full selector matrix vs libfdb_c. The first decoded txn
// is the committed seed; the second is the uncommitted pending set.
//
// Robust under load: `runGetKeyRYWDifferential` RETRIES on a transient retryable error
// (transaction_too_old(1007) etc.) with a fresh shared read version — so the RFC-056
// item (2) go-vs-cgo read-version-staleness asymmetry (under a `-test.fuzz` burst the
// single shared FDB container is hammered by many worker processes, slow executions
// drift past the 5 s MVCC window, and go vs cgo momentarily disagree on a pinned
// version) does NOT flake this test: it's a transient version-staleness, not a
// resolution divergence, so we re-version and retry. Verified: a 90 s burst runs ~170k
// executions clean. (The asymmetry itself — why go is more prone to 1007 under
// starvation — is still tracked for root-cause under RFC-056 item (2); it's not a
// getKey-resolution bug, which is the invariant this differential pins.)
func FuzzDifferential_GetKeyRYW(f *testing.F) {
	// Seeds that exercise the known-divergent shapes.
	f.Add([]byte{fzSet, 0, 1, 'x', fzSet, 2, 1, 'x', fzCommit, fzSet, 1, 1, 'x'})
	f.Add([]byte{fzSet, 0, 1, 'x', fzSet, 1, 1, 'x', fzCommit, fzClear, 1})
	f.Add([]byte{fzCommit, fzSet, 0, 1, 'x', fzSet, 2, 1, 'x'})
	f.Fuzz(func(t *testing.T, data []byte) {
		txns := decodeFuzzOps(data)
		var seed, pending []fuzzOp
		if len(txns) > 0 {
			seed = txns[0]
		}
		if len(txns) > 1 {
			pending = txns[1]
		}
		// Scope: PENDING is restricted to write-SHAPING ops (Set/Clear/ClearRange) — the
		// keyspace shape is what a key selector resolves over, and that is the primary
		// getKey-RYW divergence this RFC closes. PENDING atomics are intentionally
		// excluded: libfdb_c keeps a pending atomic that resolves to no value (e.g.
		// CompareAndClear) as a "phantom" is_kv slot COUNTED in the offset walk, whereas
		// the Go rywCache eagerly collapses it — a deeper write-map-slot-preservation
		// gap deferred under the RFC-056 audit (see ryw_getkey.go DEFERRED note). The
		// committed SEED may still contain atomics (it just shapes the storage keyset).
		pending = filterWriteShaping(pending)
		runGetKeyRYWDifferential(t, "fuzz", seed, pending)
	})
}

// filterWriteShaping keeps only the ops that shape the keyspace (Set/Clear/ClearRange).
func filterWriteShaping(ops []fuzzOp) []fuzzOp {
	out := ops[:0]
	for _, op := range ops {
		switch op.kind {
		case fzSet, fzClear, fzClearRange:
			out = append(out, op)
		}
	}
	return out
}
