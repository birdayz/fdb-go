package fdb

import (
	"context"
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// RFC-133: the database-level SetSnapshotRywDisable and SetTransactionBypassUnreadable
// options change READ semantics on every new transaction in libfdb_c (snapshot-read-after-own-write;
// accessed_unreadable→read). The pure-Go client must propagate them as txDefaults like the other
// honored DB defaults, not silently drop the caller's intent. These pin both the storage of the
// default and its application to a new transaction (via applyTxDefaults).

func TestDatabaseDefault_SnapshotRYWDisable_Propagates(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	if err := (DatabaseOptions{db: idb}).SetSnapshotRywDisable(); err != nil {
		t.Fatalf("SetSnapshotRywDisable: %v", err)
	}
	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t, true)
	if inner.SnapshotRYWDisableCount() <= 0 {
		t.Errorf("DB-level SetSnapshotRywDisable must disable snapshot RYW on the new tx, got count %d", inner.SnapshotRYWDisableCount())
	}
}

// TestDatabaseDefault_SnapshotRYW_IsCounter pins libfdb_c's cumulative-counter semantics
// (NativeAPI.actor.cpp:2156/2160 snapshotRywEnabled++/--; ReadYourWrites.actor.cpp:2082 seeds each
// new tx): SetSnapshotRywEnable() then SetSnapshotRywDisable() nets to ZERO — the new tx stays
// ENABLED — not last-wins-disabled (a bool would get this wrong).
func TestDatabaseDefault_SnapshotRYW_IsCounter(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	opts := DatabaseOptions{db: idb}
	_ = opts.SetSnapshotRywEnable()  // net -1
	_ = opts.SetSnapshotRywDisable() // net  0
	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t, true)
	if inner.SnapshotRYWDisableCount() > 0 {
		t.Errorf("enable+disable must net to enabled (count 0), not last-wins disabled — got count %d", inner.SnapshotRYWDisableCount())
	}

	idb2 := &internalDB{}
	o2 := DatabaseOptions{db: idb2}
	_ = o2.SetSnapshotRywDisable()
	_ = o2.SetSnapshotRywDisable()
	_ = o2.SetSnapshotRywEnable()
	tr2, inner2 := newBareOptionsTx()
	Database{d: idb2}.applyTxDefaults(tr2.t, true)
	if inner2.SnapshotRYWDisableCount() != 1 {
		t.Errorf("disable+disable+enable must net to 1 disable, got count %d", inner2.SnapshotRYWDisableCount())
	}
}

func TestDatabaseDefault_BypassUnreadable_Propagates(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	if err := (DatabaseOptions{db: idb}).SetTransactionBypassUnreadable(); err != nil {
		t.Fatalf("SetTransactionBypassUnreadable: %v", err)
	}
	if !idb.defaults().bypassUnreadable {
		t.Fatal("SetTransactionBypassUnreadable must store the DB default")
	}

	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t, true)
	if !inner.BypassUnreadable() {
		t.Error("DB-level SetTransactionBypassUnreadable must set bypass_unreadable on the new tx")
	}
}

// Undo the inherited count, retry through native OnError, then require replay to restore it.
// Applying again without another reset must remain idempotent.
func TestDatabaseDefault_SnapshotRYW_RetryIdempotent(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	_ = (DatabaseOptions{db: idb}).SetSnapshotRywDisable() // net 1
	tr, inner := newBareOptionsTx()
	db := Database{d: idb}
	defer inner.Cancel()
	db.applyTxDefaults(tr.t, true)
	if got := inner.SnapshotRYWDisableCount(); got != 1 {
		t.Fatalf("initial snapshot-RYW default=%d, want 1", got)
	}
	for attempt := 2; attempt <= 3; attempt++ {
		inner.SetSnapshotRYWEnable()
		if err := inner.OnError(context.Background(), &wire.FDBError{Code: 1020}); err != nil {
			t.Fatalf("native OnError before attempt %d: %v", attempt, err)
		}
		if got := inner.SnapshotRYWDisableCount(); got != 0 {
			t.Fatalf("pre-replay control at attempt %d=%d, want 0", attempt, got)
		}
		db.applyTxDefaults(tr.t, false)
		if got := inner.SnapshotRYWDisableCount(); got != 1 {
			t.Fatalf("replayed snapshot-RYW default at attempt %d=%d, want 1", attempt, got)
		}
		db.applyTxDefaults(tr.t, false)
		if got := inner.SnapshotRYWDisableCount(); got != 1 {
			t.Fatalf("idempotent snapshot-RYW replay at attempt %d=%d, want 1", attempt, got)
		}
	}
}

// FDB C++ dev review #331 (third silent drop): causal_read_risky's per-tx form IS honored (sets the
// GRV flag), so the DB default must propagate too — unlike causal_write_risky, whose per-tx form is
// a fail-safe no-op.
func TestDatabaseDefault_CausalReadRisky_Propagates(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	if err := (DatabaseOptions{db: idb}).SetTransactionCausalReadRisky(); err != nil {
		t.Fatalf("SetTransactionCausalReadRisky: %v", err)
	}
	if !idb.defaults().causalReadRisky {
		t.Fatal("SetTransactionCausalReadRisky must store the DB default")
	}

	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t, true)
	if !inner.CausalReadRisky() {
		t.Error("DB-level SetTransactionCausalReadRisky must set the GRV causal-read-risky flag on the new tx")
	}
}

func TestRetryDefaults_ValidationCaptureIsLocalAndNonconsuming(t *testing.T) {
	t.Parallel()
	for _, timeout := range []int64{0, 10000, int64(math.MaxInt32) + 1} {
		db := Database{d: &internalDB{}}
		if err := db.Options().SetTransactionTimeout(timeout); err != nil {
			t.Fatal(err)
		}
		tr, inner := newBareOptionsTx()
		defer inner.Cancel()
		// A nil native database makes any unintended GRV/RPC fail; nonempty buffers
		// also detect a verdict query that mutates rather than reads local accounting.
		inner.Set([]byte("key"), []byte("value"))
		before, err := inner.GetApproximateSize()
		if err != nil || before == 0 {
			t.Fatalf("nonempty accounting control: size=%d err=%v", before, err)
		}
		var defaults retryTxDefaults
		if err := defaults.apply(db, tr.t); err != nil {
			t.Fatalf("initial application must keep validation deferred: %v", err)
		}
		after, err := inner.GetApproximateSize()
		if timeout <= math.MaxInt32 {
			if err != nil || after != before || defaults.err != nil {
				t.Fatalf("local capture changed accounting: before=%d after=%d err=%v captured=%v", before, after, err, defaults.err)
			}
			continue
		}
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != 2006 {
			t.Fatalf("validation query consumed the native error: %v", err)
		}
		if err := inner.OnError(context.Background(), &wire.FDBError{Code: 1020}); err != nil {
			t.Fatalf("native retry behavior changed: %v", err)
		}
		if _, err := inner.GetApproximateSize(); err != nil {
			t.Fatalf("native OnError must still clear deferred errors: %v", err)
		}
		var public Error
		if err := defaults.apply(db, tr.t); !errors.As(err, &public) || public.Code != 2006 {
			t.Fatalf("reentry lost captured persistent validation failure: %v", err)
		}
	}
}

func TestRetryDefaults_DistinguishesNativePoisonFromTimeoutFailure(t *testing.T) {
	t.Parallel()
	for _, timeout := range []int64{10000, int64(math.MaxInt32) + 1} {
		db := Database{d: &internalDB{}}
		if err := db.Options().SetTransactionTimeout(timeout); err != nil {
			t.Fatal(err)
		}
		tr, inner := newBareOptionsTx()
		defer inner.Cancel()
		inner.SetSizeLimit(1)
		var defaults retryTxDefaults
		if err := defaults.apply(db, tr.t); err != nil || defaults.err != nil || defaults.pendingTimeout != timeout {
			t.Fatalf("unrelated native poison became persistent: err=%v captured=%v pending=%d", err, defaults.err, defaults.pendingTimeout)
		}
		// A database update cannot replace the snapshot whose timeout was skipped.
		if err := db.Options().SetTransactionTimeout(20000); err != nil {
			t.Fatal(err)
		}
		if err := inner.OnError(context.Background(), &wire.FDBError{Code: 1020}); err != nil {
			t.Fatal(err)
		}
		err := defaults.apply(db, tr.t)
		if timeout <= math.MaxInt32 {
			if err != nil || defaults.err != nil || defaults.pendingTimeout != 0 {
				t.Fatalf("repaired native poison prevented timeout application: err=%v captured=%v pending=%d", err, defaults.err, defaults.pendingTimeout)
			}
			if _, err := inner.GetApproximateSize(); err != nil {
				t.Fatalf("valid recovered timeout poisoned the transaction: %v", err)
			}
		} else {
			var public Error
			if !errors.As(err, &public) || public.Code != 2006 || defaults.pendingTimeout != 0 {
				t.Fatalf("captured invalid timeout was replaced: err=%v pending=%d", err, defaults.pendingTimeout)
			}
		}
	}
}

func TestPendingTimeout_OnlySuccessfulOverridesReplaceInheritedValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		blocked bool
		timeout int64
		replace bool
	}{
		{"positive", false, 60000, true},
		{"disable", false, 0, true},
		{"invalid_negative", false, -1, false},
		{"invalid_large", false, int64(math.MaxInt32) + 1, false},
		{"poisoned_positive", true, 60000, false},
		{"poisoned_disable", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := Database{d: &internalDB{}}
			if err := db.Options().SetTransactionTimeout(10000); err != nil {
				t.Fatal(err)
			}
			tr, inner := newBareOptionsTx()
			defer inner.Cancel()
			inner.SetSizeLimit(1)
			var defaults retryTxDefaults
			tr.t.defaults = &defaults
			if err := defaults.apply(db, tr.t); err != nil || defaults.pendingTimeout != 10000 {
				t.Fatalf("pending setup: err=%v pending=%d", err, defaults.pendingTimeout)
			}
			if !tc.blocked {
				if err := inner.OnError(context.Background(), &wire.FDBError{Code: 1020}); err != nil {
					t.Fatal(err)
				}
			}
			if err := tr.Options().SetTimeout(tc.timeout); err != nil {
				t.Fatal(err)
			}
			want := int64(10000)
			if tc.replace {
				want = 0
			}
			if defaults.pendingTimeout != want || defaults.err != nil {
				t.Fatalf("override capture: pending=%d err=%v, want pending=%d and no inherited failure", defaults.pendingTimeout, defaults.err, want)
			}
			_, err := inner.GetApproximateSize()
			if tc.replace {
				if err != nil {
					t.Fatalf("successful override is poisoned: %v", err)
				}
			} else {
				var fe *wire.FDBError
				if !errors.As(err, &fe) || fe.Code != 2006 {
					t.Fatalf("skipped/invalid override lost native poison: %v", err)
				}
			}
		})
	}
}
