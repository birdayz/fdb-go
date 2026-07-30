package simfdb

import (
	"errors"
	"testing"

	"fdb.dev/pkg/dst"
	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestCommitUnknownDoesNotMutateTheHandle pins that an error return leaves the transaction
// handle exactly as it was.
//
// The commit path used to run committed=true + postCommitReset BEFORE returning 1021. That is
// the success path's bookkeeping applied to a failure, and it makes SimFDB certify three wrong
// things at once: a committed version and a versionstamp exist for a commit whose outcome is
// by definition unknown; the write buffer is gone; and — worst — a re-Commit() then takes the
// "already committed, nothing buffered" idempotent-no-op arm and returns SUCCESS having
// written nothing at all. An explicit BeginTx→COMMIT that retries its COMMIT lands exactly
// there and silently loses the whole transaction.
func TestCommitUnknownDoesNotMutateTheHandle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		inject  int
		applied bool
	}{
		{"applied branch", CommitUnknownApplied, true},
		{"discarded branch", CommitUnknownDiscarded, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			db.InjectOnce(tc.inject)

			tx := db.newTxn()
			tx.Set(k("a"), []byte("v"))
			if code := errCode(t, tx.Commit().Get()); code != 1021 {
				t.Fatalf("commit: got %d, want 1021", code)
			}

			// The handle is NOT committed: the client only sets hasCommitted on the success
			// path, so these two are used_during_commit(2017).
			if cv, err := tx.GetCommittedVersion(); err == nil {
				t.Errorf("GetCommittedVersion after 1021 succeeded with %d, want error 2017", cv)
			} else if code := errCode(t, err); code != 2017 {
				t.Errorf("GetCommittedVersion after 1021: code %d, want 2017", code)
			}
			if _, err := tx.GetVersionstamp().Get(); err == nil {
				t.Error("GetVersionstamp after 1021 succeeded, want error 2017")
			} else if code := errCode(t, err); code != 2017 {
				t.Errorf("GetVersionstamp after 1021: code %d, want 2017", code)
			}

			// The buffer survives, so a re-Commit re-sends the mutations instead of being a
			// silent no-op.
			if len(tx.buffer) != 1 {
				t.Fatalf("buffer after 1021 has %d mutations, want 1 (a re-Commit must re-send)", len(tx.buffer))
			}

			// The store reflects the branch that actually happened.
			got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
				return rtx.Get(k("a")).MustGet(), nil
			})
			if tc.applied != (got.([]byte) != nil) {
				t.Fatalf("store after 1021 (%s): value=%q, applied=%v", tc.name, got.([]byte), tc.applied)
			}
		})
	}
}

// TestReCommitAfterCommitUnknownResendsTheBuffer is the RFC-198 explicit-COMMIT-retry pin: the
// path where a SQL BeginTx→COMMIT sees 1021 and issues COMMIT again on the same handle.
//
// On the DISCARDED branch nothing landed, so the retry must actually write. Under the old
// model the retry returned success and the row never existed — a silently lost transaction
// reported as committed.
func TestReCommitAfterCommitUnknownResendsTheBuffer(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.InjectOnce(CommitUnknownDiscarded)

	tx := db.newTxn()
	tx.Set(k("row"), []byte("payload"))
	if code := errCode(t, tx.Commit().Get()); code != 1021 {
		t.Fatalf("first commit: got %d, want 1021", code)
	}
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("re-Commit after 1021: %v", err)
	}
	got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return string(rtx.Get(k("row")).MustGet()), nil
	})
	if got.(string) != "payload" {
		t.Fatalf("after re-Commit the row is %q, want %q — the retry reported success but "+
			"wrote nothing", got.(string), "payload")
	}
	if cv, err := tx.GetCommittedVersion(); err != nil || cv <= 0 {
		t.Fatalf("GetCommittedVersion after the successful re-Commit: cv=%d err=%v", cv, err)
	}
}

// TestReCommitAfterAppliedCommitUnknownSelfConflicts is the other branch: the first commit DID
// land, so re-sending the same buffer on a handle that still holds its read conflicts must
// abort 1020 rather than apply the mutations twice.
func TestReCommitAfterAppliedCommitUnknownSelfConflicts(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "cnt", "start")
	db.InjectOnce(CommitUnknownApplied)

	tx := db.newTxn()
	tx.Get(k("cnt")).MustGet() // a read-modify-write: the read conflict is what detects the landing
	tx.Set(k("cnt"), []byte("mine"))
	if code := errCode(t, tx.Commit().Get()); code != 1021 {
		t.Fatalf("first commit: got %d, want 1021", code)
	}
	if code := errCode(t, tx.Commit().Get()); code != 1020 {
		t.Fatalf("re-Commit after an APPLIED 1021: got %d, want not_committed(1020) — the "+
			"transaction's own landed write must conflict with its retained read conflict", code)
	}
}

// TestOnErrorPromotesWriteConflictsOnMaybeCommitted pins the client's MAYBE_COMMITTED
// promotion (client/transaction.go:2382-2400, C++ FDB_ERROR_PREDICATE_MAYBE_COMMITTED): after
// 1021/1039 the retry carries the previous attempt's WRITE conflict ranges as READ conflict
// ranges, so a commit that did land is detected as a conflict instead of being applied twice.
// A plain Reset() drops them and the retry re-applies blind.
func TestOnErrorPromotesWriteConflictsOnMaybeCommitted(t *testing.T) {
	t.Parallel()
	for _, code := range []int{1021, 1039} {
		code := code
		t.Run(fdbCodeName(code), func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			tx := db.newTxn()
			tx.Set(k("w1"), []byte("v"))
			tx.ClearRange(fdb.KeyRange{Begin: k("r0"), End: k("r9")})
			want := len(tx.writeConflicts)
			if want != 2 {
				t.Fatalf("setup: %d write conflict ranges, want 2", want)
			}
			if err := tx.OnError(fdb.Error{Code: code}).Get(); err != nil {
				t.Fatalf("OnError(%d): %v", code, err)
			}
			if got := len(tx.readConflicts); got != want {
				t.Fatalf("after OnError(%d): %d read conflict ranges, want %d promoted from "+
					"the write conflicts", code, got, want)
			}
			if got := len(tx.writeConflicts); got != 0 {
				t.Fatalf("after OnError(%d): %d write conflict ranges survived the reset", code, got)
			}
			if !hasRange(tx.readConflicts, "w1", "w1\x00") || !hasRange(tx.readConflicts, "r0", "r9") {
				t.Fatalf("promoted ranges are wrong: %v", tx.readConflicts)
			}
		})
	}

	// A definitely-not-committed retryable error promotes nothing — the transaction is known
	// not to have landed, so a self-conflict would abort every retry forever.
	t.Run("1020 promotes nothing", func(t *testing.T) {
		t.Parallel()
		db := New(nil)
		tx := db.newTxn()
		tx.Set(k("w1"), []byte("v"))
		if err := tx.OnError(fdb.Error{Code: 1020}).Get(); err != nil {
			t.Fatalf("OnError(1020): %v", err)
		}
		if got := len(tx.readConflicts); got != 0 {
			t.Fatalf("OnError(1020) promoted %d ranges, want 0", got)
		}
	})
}

// TestTransactRetriesThroughOnError pins that the retry loop routes through OnError — that the
// promotion is reachable from the path the record layer actually uses, not just from a direct
// OnError call — and, in the same shape, shows what the promotion BUYS.
//
// The transaction is write-only (an atomic Add), so it takes no read conflict of its own. After
// the applied 1021 the promoted range on the counter key is the ONLY thing standing between the
// retry and a lost update: a concurrent writer that lands after the retry's read version must
// abort it. Without the promotion the retry has an empty read-conflict set and blesses the
// overwrite.
func TestTransactRetriesThroughOnError(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.InjectOnce(CommitUnknownApplied)

	attempts, raced := 0, false
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		attempts++
		// Pin the read version before the concurrent write below, so that write lands at a
		// version STRICTLY GREATER than this attempt's read version — the only arrangement in
		// which any read conflict, promoted or not, can fire.
		tx.GetReadVersion().MustGet()
		if attempts == 2 && !raced {
			raced = true
			if _, err := db.Transact(func(w fdb.WritableTransaction) (any, error) {
				w.Set(k("cnt"), []byte("concurrent"))
				return nil, nil
			}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}
		}
		tx.Add(k("cnt"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
		return nil, nil
	}); err != nil {
		t.Fatalf("Transact: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("Transact ran the closure %d times, want 3: attempt 1 gets the applied 1021, "+
			"attempt 2 must be aborted by the PROMOTED read conflict against the concurrent "+
			"write, attempt 3 succeeds. A retry loop that Resets instead of calling OnError "+
			"stops at 2 and silently loses the concurrent write.", attempts)
	}
}

// TestCommitUnknownBranchIsSeededAndReachesBothWays pins that a plain (un-named) 1021 resolves
// its branch from the run's seed: reproducible for one seed, and both branches reachable
// across seeds. A one-branch model would make one of these impossible to observe.
func TestCommitUnknownBranchIsSeededAndReachesBothWays(t *testing.T) {
	t.Parallel()

	// applied reports whether seed's first plain-1021 commit left the write durable.
	applied := func(seed uint64) bool {
		env := dst.NewSim(seed)
		env.Buggify = dst.NewBuggifier(seed, false) // faults off; the coin still flips
		db := New(env)
		db.InjectOnce(1021)
		tx := db.newTxn()
		tx.Set(k("a"), []byte("v"))
		if code := errCode(t, tx.Commit().Get()); code != 1021 {
			t.Fatalf("seed %d: commit got %d, want 1021", seed, code)
		}
		got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
			return rtx.Get(k("a")).MustGet(), nil
		})
		return got.([]byte) != nil
	}

	sawApplied, sawDiscarded := false, false
	for s := uint64(1); s <= 32; s++ {
		if applied(s) {
			sawApplied = true
		} else {
			sawDiscarded = true
		}
		if got := applied(s); got != applied(s) {
			t.Fatalf("seed %d is not reproducible", s)
		}
	}
	if !sawApplied || !sawDiscarded {
		t.Fatalf("across 32 seeds a plain 1021 reached applied=%v discarded=%v — the branch "+
			"is not actually a coin", sawApplied, sawDiscarded)
	}
}

// TestInjectOnceDoesNotPerturbTheBuggifySchedule pins the other half of the per-site-stream
// work: the commit path draws every Buggify site unconditionally, so a run with targeted
// injection sees the same seeded fault schedule as one without. A `cond || env.Fault(site)`
// short-circuit skips the draw and silently re-phases the site.
func TestInjectOnceDoesNotPerturbTheBuggifySchedule(t *testing.T) {
	t.Parallel()

	// commitOutcomes runs n commits and records the error code of each; withInject fires a
	// targeted 1020 on the first commit, which must not change the codes of the rest.
	outcomes := func(withInject bool) []int {
		env := dst.NewSim(9)
		env.Buggify.SetProbabilities(1.0, 0.5) // every site active, coin-flip firing
		db := New(env)
		var out []int
		for i := 0; i < 40; i++ {
			if i == 0 && withInject {
				db.InjectOnce(1020)
			}
			tx := db.newTxn()
			tx.Set(fdb.Key([]byte{byte(i)}), []byte("v"))
			err := tx.Commit().Get()
			var fe fdb.Error
			if errors.As(err, &fe) {
				out = append(out, fe.Code)
			} else if err != nil {
				t.Fatalf("commit %d: unexpected error %v", i, err)
			} else {
				out = append(out, 0)
			}
		}
		return out
	}
	base, withInject := outcomes(false), outcomes(true)
	if len(base) != len(withInject) {
		t.Fatalf("length mismatch: %d vs %d", len(base), len(withInject))
	}
	for i := 1; i < len(base); i++ { // commit 0 differs by construction (it is the injected one)
		if base[i] != withInject[i] {
			t.Fatalf("InjectOnce shifted the seeded fault schedule at commit %d: %d vs %d\n"+
				"base=%v\nwith=%v", i, base[i], withInject[i], base, withInject)
		}
	}
}

func fdbCodeName(code int) string {
	switch code {
	case 1021:
		return "commit_unknown_result"
	case 1039:
		return "cluster_version_changed"
	}
	return "unknown"
}

func hasRange(rs []keyRange, begin, end string) bool {
	for _, r := range rs {
		if string(r.begin) == begin && string(r.end) == end {
			return true
		}
	}
	return false
}
