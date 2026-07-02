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

// GetPipelined ↔ full-RYW boundary differential vs libfdb_c — RFC-175 E3.
//
// The facade's Get routes every point read through Transaction.GetPipelined
// (fdb/transaction.go), which answers from the RYW write map when it can and falls
// back to the full ryw.get() server-read+merge path via ErrNeedFullRYW when the key
// carries pending atomics (client/transaction.go GetPipelined). That fast path is a
// second RYW read implementation that must agree with the full path — and with
// libfdb_c — at every boundary. libfdb_c has ONE implementation (RYWIterator over
// the write map, ReadYourWrites.actor.cpp), so any fast-vs-full drift shows up here
// as a go-vs-cgo mismatch. These NAMED cases pin the boundary mechanically:
// identical seeded storage, identical pending ops, the same in-txn reads through
// goClient AND cgoClient, results compared byte-for-byte including error codes
// (runRYWReadDifferential fails on any Get/GetRange error-presence mismatch).
//
// Boundary arms exercised (client/transaction.go GetPipelined):
//   - pending-atomic fallback: entry.hasAtomics → ErrNeedFullRYW → server read +
//     merge (the arm RFC-175 E3 names "pending atomic op on the read key");
//   - phantom-absent: a matched CompareAndClear is an is_kv slot for getKey but
//     ABSENT for a point read (RFC-058);
//   - plain-entry / cleared-range hits crossed by a straddling range read (the arm
//     E3 names "a range read straddling a pipelined write");
//   - the unreadable gate: pending versionstamp under a pending atomic → 1036
//     (error-code parity, RFC-098).

// TestDifferential_PipelinedRYWBoundary_PendingAtomicOnReadKey pins the
// ErrNeedFullRYW fallback: a point read of a key with pending atomics must merge
// exactly like libfdb_c across base-source (committed / same-txn Set / same-txn
// Clear / absent) and op semantics (width interplay, present-gated Min, stacked
// chains, CompareAndClear phantoms, AppendIfFits).
func TestDifferential_PipelinedRYWBoundary_PendingAtomicOnReadKey(t *testing.T) {
	t.Parallel()
	b := func(s string) []byte { return []byte(s) }
	set := func(ki int, v string) fuzzOp { return fuzzOp{kind: fzSet, keyIdx: ki, operand: b(v)} }
	clr := func(ki int) fuzzOp { return fuzzOp{kind: fzClear, keyIdx: ki} }
	op := func(kind, ki int, operand []byte) fuzzOp { return fuzzOp{kind: kind, keyIdx: ki, operand: operand} }

	cases := []struct {
		name          string
		seed, pending []fuzzOp
	}{
		// The fallback arm proper: hasAtomics + a COMMITTED server base. The atomic-fold
		// widths differential (differential_atomic_test.go) folds only over same-txn Sets or
		// absent storage; here the merge input is the SERVER value fetched by the fallback.
		{
			"add_on_committed_base_widths",
			[]fuzzOp{set(0, "\x05\x00\x00\x00")},
			[]fuzzOp{op(fzAdd, 0, b("\x03\x00"))},
		}, // operand narrower than base: result truncates to operand width
		// doMinV2/doAndV2 gate on Optional::present() (Atomic.h) — server-present vs
		// server-absent take different branches of the SAME pending op.
		{
			"min_on_committed_base",
			[]fuzzOp{set(0, "\x07\x00\x00\x00")},
			[]fuzzOp{op(fzMin, 0, b("\x03\x00\x00\x00"))},
		},
		{
			"min_on_absent_base",
			nil,
			[]fuzzOp{op(fzMin, 0, b("\x03\x00\x00\x00"))},
		},
		// An unresolved multi-op chain folded over the server base in one merge.
		{
			"atomic_chain_on_committed_base",
			[]fuzzOp{set(0, "\x01\x00\x00\x00")},
			[]fuzzOp{op(fzAdd, 0, b("\x02\x00\x00\x00")), op(fzXor, 0, b("\x0f\x00\x00\x00")), op(fzByteMax, 0, b("\x04\x00\x00\x00"))},
		},
		// hasAtomics with a LOCAL base: the fallback must resolve from the write map
		// without letting the server value bleed through the same-txn Set/Clear.
		{
			"add_after_set_same_txn",
			[]fuzzOp{set(0, "\x09\x00\x00\x00")}, // committed value must NOT contribute
			[]fuzzOp{set(0, "\x01\x00\x00\x00"), op(fzAdd, 0, b("\x02\x00\x00\x00"))},
		},
		{
			"add_after_clear_same_txn",
			[]fuzzOp{set(0, "\x09\x00\x00\x00")},
			[]fuzzOp{clr(0), op(fzAdd, 0, b("\x02\x00\x00\x00"))},
		},
		// Matched CompareAndClear against the COMMITTED value: phantom slot — point read
		// ABSENT, range omits the key (RFC-058); mismatch leaves the committed value.
		{
			"cac_match_committed_phantom",
			[]fuzzOp{set(0, "v"), set(1, "w")},
			[]fuzzOp{op(fzCompareAndClear, 0, b("v"))},
		},
		{
			"cac_mismatch_committed",
			[]fuzzOp{set(0, "v")},
			[]fuzzOp{op(fzCompareAndClear, 0, b("x"))},
		},
		{
			"appendiffits_on_committed_base",
			[]fuzzOp{set(0, "ab")},
			[]fuzzOp{op(fzAppendIfFits, 0, b("cd"))},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // unique per-test prefix (t.Name()) makes this collision-safe
			runRYWReadDifferential(t, tc.name, tc.seed, tc.pending)
		})
	}
}

// TestDifferential_PipelinedRYWBoundary_RangeStraddlesPipelinedWrite pins the
// merged-view arm: a range read whose span crosses keys written through the
// pipelined path (plain Sets, empty-value Sets, pending atomics, clears) must
// interleave pending and committed keys exactly like libfdb_c.
func TestDifferential_PipelinedRYWBoundary_RangeStraddlesPipelinedWrite(t *testing.T) {
	t.Parallel()
	b := func(s string) []byte { return []byte(s) }
	set := func(ki int, v string) fuzzOp { return fuzzOp{kind: fzSet, keyIdx: ki, operand: b(v)} }
	crange := func(bi, ei int) fuzzOp { return fuzzOp{kind: fzClearRange, keyIdx: bi, key2Idx: ei} }
	op := func(kind, ki int, operand []byte) fuzzOp { return fuzzOp{kind: kind, keyIdx: ki, operand: operand} }

	cases := []struct {
		name          string
		seed, pending []fuzzOp
	}{
		// Committed a,c; pending Set(b), Set(d): the scan alternates server keys and
		// pipelined writes on both sides of each boundary.
		{
			"range_straddles_pipelined_set",
			[]fuzzOp{set(0, "A"), set(2, "C")},
			[]fuzzOp{set(1, "B"), set(3, "D")},
		},
		// A pending EMPTY-value Set must still appear in the merged view (the
		// dropped-empty-pending-key getRange bug class, RFC-055).
		{
			"range_straddles_empty_value_set",
			[]fuzzOp{set(0, "A"), set(2, "C")},
			[]fuzzOp{set(1, "")},
		},
		// Pending atomics inside the span: one over a committed base (server merge),
		// one over absent (operand-as-value) — both must materialize in range order.
		{
			"range_straddles_pending_atomic",
			[]fuzzOp{set(0, "\x05\x00\x00\x00"), set(2, "C")},
			[]fuzzOp{op(fzAdd, 0, b("\x01\x00\x00\x00")), op(fzAdd, 1, b("\x02\x00\x00\x00"))},
		},
		// Clear-then-Set inside the cleared span: the Set wins over the clear, the
		// remaining cleared committed keys vanish.
		{
			"range_straddles_clear_then_set",
			[]fuzzOp{set(0, "A"), set(1, "B"), set(2, "C"), set(3, "D")},
			[]fuzzOp{crange(0, 3), set(1, "B2")},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runRYWReadDifferential(t, tc.name, tc.seed, tc.pending)
		})
	}
}

// TestDifferential_PipelinedRYWBoundary_RangeOptionsAcrossPipelinedWrite pins the
// straddle under limit/reverse and with range endpoints landing EXACTLY on
// pipelined-written keys: a limit that cuts the scan at a pending key, a reverse
// scan entering the merged view from the top, and the exclusive end sitting on a
// pending key must all agree with libfdb_c.
func TestDifferential_PipelinedRYWBoundary_RangeOptionsAcrossPipelinedWrite(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	goPfx := fmt.Sprintf("pipestraddle_%d_%s_go_", os.Getpid(), ns)
	cPfx := fmt.Sprintf("pipestraddle_%d_%s_c_", os.Getpid(), ns)
	clearPrefix(t, goPfx)
	clearPrefix(t, cPfx)

	// Committed k000,k002,k004; pending Set(k001) + Add(k003, over absent).
	seedOne := func(pfx string) []struct{ k, v string } {
		return []struct{ k, v string }{
			{pfx + "k000", "S0"}, {pfx + "k002", "S2"}, {pfx + "k004", "S4"},
		}
	}
	seedKeys(t, func(tx cgofdb.Transaction) {
		for _, kv := range seedOne(goPfx) {
			tx.Set(cgofdb.Key(kv.k), []byte(kv.v))
		}
		for _, kv := range seedOne(cPfx) {
			tx.Set(cgofdb.Key(kv.k), []byte(kv.v))
		}
	})

	applyPending := func(goTxn gofdb.Transaction, cTxn cgofdb.Transaction) {
		goTxn.Set(gofdb.Key(goPfx+"k001"), []byte("P1"))
		goTxn.Add(gofdb.Key(goPfx+"k003"), []byte("\x03\x00"))
		cTxn.Set(cgofdb.Key(cPfx+"k001"), []byte("P1"))
		cTxn.Add(cgofdb.Key(cPfx+"k003"), []byte("\x03\x00"))
	}

	cases := []struct {
		name       string
		begin, end string // appended to the per-client prefix; end is EXCLUSIVE
		limit      int
		reverse    bool
	}{
		{"all", "k000", "k005", 0, false},
		{"limit_cuts_at_pending", "k000", "k005", 2, false},   // second key IS the pipelined Set
		{"limit_cuts_after_atomic", "k000", "k005", 4, false}, // fourth key IS the pending atomic
		{"reverse_all", "k000", "k005", 0, true},
		{"reverse_limit_cuts_at_atomic", "k000", "k005", 2, true}, // reverse scan hits k004 then pending k003
		{"begin_exactly_on_pending_set", "k001", "k005", 0, false},
		{"end_exactly_on_pending_atomic", "k000", "k003", 0, false}, // exclusive end ON the pending key → excluded
		{"begin_and_end_on_pending", "k001", "k003", 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const maxAttempts = 12
			for attempt := 0; ; attempt++ {
				if attempt >= maxAttempts {
					t.Fatalf("%s: retryable errors did not clear in %d attempts", tc.name, maxAttempts)
				}
				v := freshSharedVersion(t)
				goTxn, err := goClient.CreateTransaction()
				if err != nil {
					t.Fatalf("go CreateTransaction: %v", err)
				}
				cTxn, err := cgoClient.CreateTransaction()
				if err != nil {
					goTxn.Cancel()
					t.Fatalf("cgo CreateTransaction: %v", err)
				}
				goTxn.SetReadVersion(v)
				cTxn.SetReadVersion(v)
				applyPending(goTxn, cTxn)

				if retry := func() bool {
					defer goTxn.Cancel()
					defer cTxn.Cancel()
					goKVs, goErr := goTxn.GetRange(gofdb.KeyRange{
						Begin: gofdb.Key(goPfx + tc.begin), End: gofdb.Key(goPfx + tc.end),
					}, gofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse}).GetSliceWithError()
					cKVs, cErr := cTxn.GetRange(cgofdb.KeyRange{
						Begin: cgofdb.Key(cPfx + tc.begin), End: cgofdb.Key(cPfx + tc.end),
					}, cgofdb.RangeOptions{Limit: tc.limit, Reverse: tc.reverse}).GetSliceWithError()
					if isFDBRetryable(goErr) || isFDBRetryable(cErr) {
						return true
					}
					if (goErr == nil) != (cErr == nil) {
						t.Fatalf("%s: error mismatch: go=%v cgo=%v", tc.name, goErr, cErr)
					}
					if goErr != nil {
						t.Fatalf("%s: both errored (non-retryable): go=%v cgo=%v", tc.name, goErr, cErr)
					}
					if len(goKVs) != len(cKVs) {
						t.Fatalf("%s: count differs: go=%d cgo=%d\ngo=%v\ncgo=%v", tc.name, len(goKVs), len(cKVs), goKVs, cKVs)
					}
					for i := range goKVs {
						gk := goKVs[i].Key[len(goPfx):]
						ck := cKVs[i].Key[len(cPfx):]
						if !bytes.Equal(gk, ck) || !bytes.Equal(goKVs[i].Value, cKVs[i].Value) {
							t.Fatalf("%s: pair %d differs: go=(%q,%x) cgo=(%q,%x)", tc.name, i, gk, goKVs[i].Value, ck, cKVs[i].Value)
						}
					}
					return false
				}(); !retry {
					return
				}
			}
		})
	}
}

// TestDifferential_PipelinedRYWBoundary_AtomicThenVersionstampSameKey pins
// error-code parity where the pending-atomic fallback and the unreadable gate
// stack on ONE key: an Add followed by a SetVersionstampedValue leaves the key
// with pending atomics AND sticky-unreadable — the read must throw
// accessed_unreadable (1036) on both clients, not fall back into a merge
// (C++ WriteMap is_unreadable stickiness, WriteMap.cpp:97; RFC-098).
func TestDifferential_PipelinedRYWBoundary_AtomicThenVersionstampSameKey(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("pipeboundary_1036_%d_", os.Getpid())
	clearPrefix(t, pfx)
	k := pfx + "atomic_then_svv"
	goCode := goErrCode(func(tx gofdb.Transaction) error {
		tx.Add(gofdb.Key(k), []byte("\x01\x00\x00\x00"))
		tx.SetVersionstampedValue(gofdb.Key(k), unreadableSVVOperand())
		_, err := tx.Get(gofdb.Key(k)).Get()
		return err
	})
	cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
		tx.Add(cgofdb.Key(k), []byte("\x01\x00\x00\x00"))
		tx.SetVersionstampedValue(cgofdb.Key(k), unreadableSVVOperand())
		_, err := tx.Get(cgofdb.Key(k)).Get()
		return err
	})
	if goCode != cCode || goCode != errAccessedUnreadable {
		t.Fatalf("Get of pending atomic+SVV key: go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
	}
}
