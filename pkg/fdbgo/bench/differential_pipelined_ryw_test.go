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
// goClient AND cgoClient. Values are compared byte-for-byte; Get errors are
// compared by CODE when both clients error (runRYWReadDifferential); the
// RangeOptions runner requires success, so any error there is loud; the 1036
// cases compare codes explicitly.
//
// Boundary arms exercised (client/transaction.go GetPipelined):
//   - pending-atomic fallback: entry.hasAtomics → ErrNeedFullRYW → server read +
//     merge (the arm RFC-175 E3 names "pending atomic op on the read key");
//   - the eager-fold FAST path: an atomic over a same-txn Set/cleared base folds
//     into a PLAIN entry (ryw.go sites B/C, hasAtomics stays false) — the fast
//     path must answer identically to what the full path would;
//   - phantom-absent: a matched CompareAndClear is an is_kv slot for getKey but
//     ABSENT for a point read (RFC-058);
//   - plain-entry / cleared-range hits crossed by a straddling range read (the arm
//     E3 names "a range read straddling a pipelined write");
//   - snapshot reads: snapshot RYW is ON by default since API 300, so they route
//     through the SAME RYWIterator merge without conflict ranges
//     (ReadYourWrites.actor.cpp:402-405);
//   - the unreadable gate: pending versionstamp on/under a pending-atomic key →
//     1036 (error-code parity, RFC-098).

// TestDifferential_PipelinedRYWBoundary_PendingAtomicOnReadKey pins the point-read
// boundary: the ErrNeedFullRYW fallback (pending atomics over a COMMITTED server
// base — width interplay, present-gated Min, stacked chains, CompareAndClear
// phantoms, AppendIfFits) plus the eager-fold FAST-path cases (atomic over a
// same-txn Set/cleared base), which must answer identically without the fallback.
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
		// (Min over an ABSENT base — the same fallback arm with no server value — is
		// already pinned by differential_atomic_test.go's min_missing_o8; not repeated.)
		// An unresolved multi-op chain folded over the server base in one merge.
		{
			"atomic_chain_on_committed_base",
			[]fuzzOp{set(0, "\x01\x00\x00\x00")},
			[]fuzzOp{op(fzAdd, 0, b("\x02\x00\x00\x00")), op(fzXor, 0, b("\x0f\x00\x00\x00")), op(fzByteMax, 0, b("\x04\x00\x00\x00"))},
		},
		// FAST-PATH arm, not the fallback: the eager fold (ryw.go sites B/C) resolves an
		// atomic over a same-txn Set / locally-cleared base into a PLAIN entry
		// (hasAtomics stays false), so GetPipelined answers from the write map without
		// ErrNeedFullRYW. Pinned here because the fast path must give the same answer
		// the full path would — the dimension these add over the atomic-fold widths
		// differential is the STALE COMMITTED VALUE beneath, which must not bleed
		// through the local base on either the point read or the range merge.
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
// error-code parity where a pending atomic and a versionstamped op land on ONE key,
// in BOTH orders. C++ resolves the two orders through DIFFERENT WriteMap paths:
//   - add_then_svv: a versionstamped op landing on a non-unreadable entry REPLACES
//     the stack (WriteMap.cpp:124-137, gated `!it.is_unreadable()`), dropping the
//     Add; the entry becomes unreadable;
//   - svv_then_add: the later Add is pushed UNCOALESCED onto the already-unreadable
//     entry (WriteMap.cpp:143-147), whose unreadable flag is STICKY (WriteMap.cpp:97).
//
// Either way a read must throw accessed_unreadable (1036) on both clients, never
// fall back into a merge (RFC-098).
func TestDifferential_PipelinedRYWBoundary_AtomicThenVersionstampSameKey(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("pipeboundary_1036_%d_", os.Getpid())
	clearPrefix(t, pfx)

	t.Run("add_then_svv", func(t *testing.T) {
		t.Parallel()
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
			t.Fatalf("Get of Add→SVV key: go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})

	t.Run("svv_then_add", func(t *testing.T) {
		t.Parallel()
		k := pfx + "svv_then_atomic"
		goCode := goErrCode(func(tx gofdb.Transaction) error {
			tx.SetVersionstampedValue(gofdb.Key(k), unreadableSVVOperand())
			tx.Add(gofdb.Key(k), []byte("\x01\x00\x00\x00"))
			_, err := tx.Get(gofdb.Key(k)).Get()
			return err
		})
		cCode := cgoErrCode(func(tx cgofdb.Transaction) error {
			tx.SetVersionstampedValue(cgofdb.Key(k), unreadableSVVOperand())
			tx.Add(cgofdb.Key(k), []byte("\x01\x00\x00\x00"))
			_, err := tx.Get(cgofdb.Key(k)).Get()
			return err
		})
		if goCode != cCode || goCode != errAccessedUnreadable {
			t.Fatalf("Get of SVV→Add key (sticky unreadable): go=%d cgo=%d, want both %d", goCode, cCode, errAccessedUnreadable)
		}
	})
}

// TestDifferential_PipelinedRYWBoundary_SnapshotGetPendingAtomic pins the THIRD point-read
// path at the boundary: snapshot reads. Snapshot RYW is enabled by default since API 300
// (NativeAPI.actor.cpp:1603), so a snapshot Get routes through the SAME RYWIterator merge
// as a regular read — just without conflict ranges (ReadYourWrites.actor.cpp:402-405). A
// snapshot Get of a key with a pending atomic must produce the identical merged value on
// both clients, over a committed base and over absent storage.
func TestDifferential_PipelinedRYWBoundary_SnapshotGetPendingAtomic(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("pipesnap_%d_", os.Getpid())
	clearPrefix(t, pfx)
	goCommitted, goAbsent := gofdb.Key(pfx+"go_c"), gofdb.Key(pfx+"go_a")
	cCommitted, cAbsent := cgofdb.Key(pfx+"c_c"), cgofdb.Key(pfx+"c_a")
	seedKeys(t, func(tx cgofdb.Transaction) {
		tx.Set(cgofdb.Key(pfx+"go_c"), []byte("\x05\x00\x00\x00"))
		tx.Set(cCommitted, []byte("\x05\x00\x00\x00"))
	})

	type snapRes struct{ committed, absent string }
	var g, c snapRes
	if _, err := goClient.Transact(func(txw gofdb.WritableTransaction) (any, error) {
		tx := txw.(gofdb.Transaction)
		tx.Add(goCommitted, []byte("\x03\x00\x00\x00"))
		tx.Add(goAbsent, []byte("\x03\x00\x00\x00"))
		vc, err := tx.Snapshot().Get(goCommitted).Get()
		if err != nil {
			return nil, err
		}
		va, err := tx.Snapshot().Get(goAbsent).Get()
		if err != nil {
			return nil, err
		}
		g = snapRes{hexOrAbsent(vc), hexOrAbsent(va)}
		return nil, nil
	}); err != nil {
		t.Fatalf("go txn: %v", err)
	}
	if _, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Add(cCommitted, []byte("\x03\x00\x00\x00"))
		tx.Add(cAbsent, []byte("\x03\x00\x00\x00"))
		vc, err := tx.Snapshot().Get(cCommitted).Get()
		if err != nil {
			return nil, err
		}
		va, err := tx.Snapshot().Get(cAbsent).Get()
		if err != nil {
			return nil, err
		}
		c = snapRes{hexOrAbsent(vc), hexOrAbsent(va)}
		return nil, nil
	}); err != nil {
		t.Fatalf("cgo txn: %v", err)
	}
	if g != c {
		t.Fatalf("snapshot Get over pending atomic differs: go=%+v cgo=%+v", g, c)
	}
	// Anti-self-confirming sanity: the merge really happened (5 + 3 = 8 at operand width).
	if g.committed != "08000000" {
		t.Fatalf("sanity: snapshot merge over committed 5 + Add 3 should be 08000000, got %s", g.committed)
	}
}

// TestDifferential_PipelinedRYWBoundary_RangeStraddlesPendingVersionstamp pins the
// mid-scan unreadable reach with committed keys on BOTH sides of the pending SVV key
// (differential_unreadable_test.go's getrange_reach places the SVV at the range END):
// a limited scan that stops before the SVV key succeeds; an unlimited scan REACHES it
// mid-span and throws 1036 (RYWIterator.cpp:44-46, :74-76) even though committed data
// exists beyond; a reverse scan hits it from above. Both clients must agree on rows
// AND codes.
func TestDifferential_PipelinedRYWBoundary_RangeStraddlesPendingVersionstamp(t *testing.T) {
	t.Parallel()
	pfx := fmt.Sprintf("pipestraddle_svv_%d_", os.Getpid())
	clearPrefix(t, pfx)
	// Committed a, c; pending SVV on b — the unreadable key sits MID-SPAN.
	seedKeys(t, func(tx cgofdb.Transaction) {
		for _, cl := range []string{"go_", "c_"} {
			tx.Set(cgofdb.Key(pfx+cl+"a"), []byte("va"))
			tx.Set(cgofdb.Key(pfx+cl+"c"), []byte("vc"))
		}
	})

	type res struct {
		limitedRows   int
		limitedCode   int
		unlimitedCode int
		reverseCode   int
	}
	run := func(limited func(limit int, reverse bool) (int, int)) res {
		nRows, limCode := limited(1, false)
		_, unlimCode := limited(0, false)
		_, revCode := limited(0, true)
		return res{nRows, limCode, unlimCode, revCode}
	}

	// Uncommitted txns at a shared read version, explicitly Cancel()ed — the 1036
	// reads are TRACKED read errors and would poison a commit (RFC-098), so a
	// Transact-based shape cannot commit cleanly; this matches runRYWReadDifferential.
	v := freshSharedVersion(t)
	goTxn, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go CreateTransaction: %v", err)
	}
	defer goTxn.Cancel()
	cTxn, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo CreateTransaction: %v", err)
	}
	defer cTxn.Cancel()
	goTxn.SetReadVersion(v)
	cTxn.SetReadVersion(v)

	var g, c res
	goTxn.SetVersionstampedValue(gofdb.Key(pfx+"go_b"), unreadableSVVOperand())
	goR := gofdb.KeyRange{Begin: gofdb.Key(pfx + "go_"), End: gofdb.Key(pfx + "go_\xff")}
	g = run(func(limit int, reverse bool) (int, int) {
		kvs, err := goTxn.GetRange(goR, gofdb.RangeOptions{Limit: limit, Reverse: reverse}).GetSliceWithError()
		return len(kvs), fdbErrorCode(err)
	})
	cTxn.SetVersionstampedValue(cgofdb.Key(pfx+"c_b"), unreadableSVVOperand())
	cR := cgofdb.KeyRange{Begin: cgofdb.Key(pfx + "c_"), End: cgofdb.Key(pfx + "c_\xff")}
	c = run(func(limit int, reverse bool) (int, int) {
		kvs, err := cTxn.GetRange(cR, cgofdb.RangeOptions{Limit: limit, Reverse: reverse, Mode: cgofdb.StreamingModeWantAll}).GetSliceWithError()
		return len(kvs), fdbErrorCode(err)
	})
	if g != c {
		t.Fatalf("SVV mid-scan straddle differs: go=%+v cgo=%+v", g, c)
	}
	if g.unlimitedCode != errAccessedUnreadable || g.reverseCode != errAccessedUnreadable {
		t.Fatalf("unlimited/reverse scans must REACH the mid-span SVV key → 1036, got %+v", g)
	}
	if g.limitedCode != 0 || g.limitedRows != 1 {
		t.Fatalf("limit-1 scan must stop BEFORE the SVV key with one row, got %+v", g)
	}
}
