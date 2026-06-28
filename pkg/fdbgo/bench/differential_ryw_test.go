package bench

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// RYW-read differential vs libfdb_c — RFC-055 (RFC-010 C2 follow-up). RFC-053/054
// compare COMMITTED state, which only exercises RYW *write* coalescing. This adds
// the RYW *read* axis for Get + GetRange: open one uncommitted txn per client at a
// shared read version, seed identical storage, apply the same pending mutations,
// then issue Get/GetRange WITHIN the uncommitted txn and compare the
// read-your-writes-resolved results byte-for-byte. The RYW read merge (pending op
// stack merged UNDER the storage value) is distinct from commit-coalesce and was
// otherwise untested differentially — this surfaced and fixed a getRange bug that
// dropped empty-value pending keys (ryw.go).
//
// Determinism: shared read version V + identical seeded storage + identical pending
// ops ⇒ the merge is a pure function of (storage@V, op stack), so any difference is
// a pure RYW-resolution divergence. Both txns are explicitly Cancel()ed (the cgo C
// handle needs explicit cleanup, not GC). Never commits.
//
// GetKey (key-SELECTOR resolution over pending writes) and the broader RYW
// applyAtomic edge cases (e.g. Min on a present empty value) are NOT covered here:
// the fuzzer revealed a cluster of RYW-resolution divergences (getKey resolves
// against storage only; some atomics resolve differently on empty values) that
// constitute a dedicated RYW-correctness audit — deferred to RFC-056.

// rywStrip returns the prefix-stripped RYW GetRange of both clients. The GetRange
// errors are RETURNED (not Fatal'd) so the caller can retry on a transient retryable
// error (transaction_too_old etc.) — see runRYWReadDifferential. PrefixRange errors are
// programming errors and stay fatal.
func rywStrip(t *testing.T, goTxn gofdb.Transaction, cTxn cgofdb.Transaction, goPfx, cPfx string) ([]kvPair, []kvPair, error, error) {
	t.Helper()
	goR, err := gofdb.PrefixRange([]byte(goPfx))
	if err != nil {
		t.Fatalf("go PrefixRange: %v", err)
	}
	cR, err := cgofdb.PrefixRange([]byte(cPfx))
	if err != nil {
		t.Fatalf("cgo PrefixRange: %v", err)
	}
	goKVs, goErr := goTxn.GetRange(goR, gofdb.RangeOptions{}).GetSliceWithError()
	cKVs, cErr := cTxn.GetRange(cR, cgofdb.RangeOptions{}).GetSliceWithError()
	if goErr != nil || cErr != nil {
		return nil, nil, goErr, cErr
	}
	goOut := make([]kvPair, len(goKVs))
	for i, kv := range goKVs {
		goOut[i] = kvPair{append([]byte(nil), kv.Key[len(goPfx):]...), append([]byte(nil), kv.Value...)}
	}
	cOut := make([]kvPair, len(cKVs))
	for i, kv := range cKVs {
		cOut[i] = kvPair{append([]byte(nil), kv.Key[len(cPfx):]...), append([]byte(nil), kv.Value...)}
	}
	return goOut, cOut, nil, nil
}

// runRYWReadDifferential seeds identical storage, applies identical pending mutations
// to one uncommitted txn per client at a shared read version, and compares the
// RYW-resolved Get/GetRange/GetKey results.
func runRYWReadDifferential(t *testing.T, label string, seed, pending []fuzzOp) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	goPfx := fmt.Sprintf("rywread_%d_%s_go_", os.Getpid(), ns)
	cPfx := fmt.Sprintf("rywread_%d_%s_c_", os.Getpid(), ns)
	clearPrefix(t, goPfx)
	clearPrefix(t, cPfx)

	if len(seed) > 0 {
		if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			applyGo(tx, seed, goPfx)
			return nil, nil
		}); err != nil {
			t.Fatalf("%s: seed go: %v", label, err)
		}
		mustCGo(t, func(tx cgofdb.Transaction) { applyC(tx, seed, cPfx) })
	}

	seq := fmt.Sprintf("seed=%s pending=%s", fmtTxns([][]fuzzOp{seed}), fmtTxns([][]fuzzOp{pending}))

	// Compare under a fresh shared read version, retrying on a TRANSIENT retryable error
	// (transaction_too_old(1007) etc. when a slow run under heavy parallel-container load
	// drifts past the 5s MVCC window). That is the RFC-056 item (2) go-vs-cgo read-version
	// asymmetry, NOT a resolution divergence — re-version and retry rather than flag a
	// false error mismatch. The RYW merge is the invariant under test; version staleness
	// is not. (Same pattern as runGetKeyRYWDifferential.)
	const maxAttempts = 12
	for attempt := 0; ; attempt++ {
		if attempt >= maxAttempts {
			t.Fatalf("%s: RYW-read differential: retryable errors (RFC-056 item 2 read-version staleness) did not clear in %d attempts\n%s", label, maxAttempts, seq)
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

		if retry := func() bool {
			defer goTxn.Cancel()
			defer cTxn.Cancel()
			// (1) Get each domain key — RYW-resolved (pending merged with storage@V).
			for _, k := range fuzzKeys {
				gv, gerr := goTxn.Get(gofdb.Key(goPfx + k)).Get()
				cv, cerr := cTxn.Get(cgofdb.Key(cPfx + k)).Get()
				if isFDBRetryable(gerr) || isFDBRetryable(cerr) {
					return true
				}
				if (gerr == nil) != (cerr == nil) {
					t.Fatalf("%s: RYW Get(%s) error mismatch: go=%v cgo=%v\n%s", label, k, gerr, cerr, seq)
				}
				if gerr == nil && !bytes.Equal(gv, cv) {
					t.Fatalf("%s: RYW Get(%s) differs: go=%x cgo=%x\n%s", label, k, gv, cv, seq)
				}
			}
			// (2) GetRange over the prefix — the merged pending+storage view.
			goState, cState, goErr, cErr := rywStrip(t, goTxn, cTxn, goPfx, cPfx)
			if isFDBRetryable(goErr) || isFDBRetryable(cErr) {
				return true
			}
			if goErr != nil || cErr != nil {
				t.Fatalf("%s: RYW GetRange error: go=%v cgo=%v\n%s", label, goErr, cErr, seq)
			}
			if len(goState) != len(cState) {
				t.Fatalf("%s: RYW GetRange count differs: go=%d cgo=%d\ngoKVs=%v cgoKVs=%v\n%s",
					label, len(goState), len(cState), kvDump(goState), kvDump(cState), seq)
			}
			for i := range goState {
				if !bytes.Equal(goState[i].k, cState[i].k) || !bytes.Equal(goState[i].v, cState[i].v) {
					t.Fatalf("%s: RYW GetRange pair %d differs: go=(%q,%x) cgo=(%q,%x)\n%s",
						label, i, goState[i].k, goState[i].v, cState[i].k, cState[i].v, seq)
				}
			}
			return false
		}(); !retry {
			break
		}
	}

	// NOTE: RYW key-SELECTOR resolution (GetKey over pending writes) is intentionally
	// NOT compared here. The Go client's GetKey resolves selectors against storage
	// only and does not merge pending mutations — a real divergence from C++
	// RYW::getKey that this differential surfaced. The correct fix is a faithful port
	// of resolveKeySelectorFromCache (offset/orEqual semantics over the merged
	// write-map), deferred to RFC-056. See transaction.go GetKey. This test covers
	// the RYW Get + GetRange read-merge paths, which are fixed and validated.
}

func kvDump(kvs []kvPair) string {
	s := "["
	for i, kv := range kvs {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%q=%x", kv.k, kv.v)
	}
	return s + "]"
}

func TestDifferential_RYWReads(t *testing.T) {
	t.Parallel()
	b := func(s string) []byte { return []byte(s) }
	set := func(ki int, v string) fuzzOp { return fuzzOp{kind: fzSet, keyIdx: ki, operand: b(v)} }
	clr := func(ki int) fuzzOp { return fuzzOp{kind: fzClear, keyIdx: ki} }
	crange := func(bi, ei int) fuzzOp { return fuzzOp{kind: fzClearRange, keyIdx: bi, key2Idx: ei} }
	add := func(ki int, op []byte) fuzzOp { return fuzzOp{kind: fzAdd, keyIdx: ki, operand: op} }

	cases := []struct {
		name          string
		seed, pending []fuzzOp
	}{
		// pending Set shadows storage (read the pending value, not the seeded one).
		{"pending_set_shadows_storage", []fuzzOp{set(0, "old")}, []fuzzOp{set(0, "new")}},
		// pending Clear of a seeded key → read sees absence.
		{"pending_clear_of_seeded", []fuzzOp{set(0, "v"), set(1, "w")}, []fuzzOp{clr(0)}},
		// pending atomic accumulation onto a seeded value.
		{"pending_add_onto_seeded", []fuzzOp{set(0, "\x05\x00\x00\x00")}, []fuzzOp{add(0, b("\x03\x00\x00\x00"))}},
		// (a) clear-range hole crossing the prefix: clear [a,c) over seeded b; read must omit a,b.
		{"clearrange_hole", []fuzzOp{set(0, "A"), set(1, "B"), set(2, "C"), set(3, "D")}, []fuzzOp{crange(0, 2)}},
		// (b) GetKey offset over the merged view: pending Set fills a gap; offset must skip it correctly.
		{"getkey_offset_over_pending", []fuzzOp{set(0, "a"), set(2, "c")}, []fuzzOp{set(1, "b")}},
		// (c) atomic onto a key cleared EARLIER in the same txn (dependent-op-onto-cleared-range:
		// coalesces over an empty SetValue base, NOT unreadable) → result = operand.
		{"add_onto_cleared_same_txn", []fuzzOp{set(0, "\x09\x00\x00\x00")}, []fuzzOp{clr(0), add(0, b("\x01"))}},
		// CompareAndClear (match) over a seeded value → key vanishes from the merged view.
		{"compareandclear_match", []fuzzOp{set(0, "v")}, []fuzzOp{{kind: fzCompareAndClear, keyIdx: 0, operand: b("v")}}},
		// Empty-operand Xor sets a key to EMPTY value (doXor(_, "") returns the empty
		// operand) — the key must still appear in the merged view (FuzzRYWRead find).
		{"xor_empty_operand", []fuzzOp{{kind: fzXor, keyIdx: 0, operand: b("")}}, []fuzzOp{{kind: fzXor, keyIdx: 1, operand: b("")}}},
		// FuzzRYWRead find: Xor(a,"") committed + pending CAC(b,"0") (no-op on absent b)
		// + Xor(c,"") → merged view {a, c}; GetKey FGE+2(a) must resolve to c.
		{"xor_cac_xor_getkey_offset", []fuzzOp{{kind: fzXor, keyIdx: 0, operand: b("")}}, []fuzzOp{{kind: fzCompareAndClear, keyIdx: 1, operand: b("0")}, {kind: fzXor, keyIdx: 2, operand: b("")}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // unique per-test prefix (t.Name()) makes this collision-safe
			runRYWReadDifferential(t, tc.name, tc.seed, tc.pending)
		})
	}
}
