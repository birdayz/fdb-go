package fdb

import "testing"

// RFC-133 / codex #331: the database-level SetSnapshotRywDisable and SetTransactionBypassUnreadable
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
	Database{d: idb}.applyTxDefaults(tr.t)
	if inner.SnapshotRYWDisableCount() <= 0 {
		t.Errorf("DB-level SetSnapshotRywDisable must disable snapshot RYW on the new tx, got count %d", inner.SnapshotRYWDisableCount())
	}
}

// TestDatabaseDefault_SnapshotRYW_IsCounter pins libfdb_c's cumulative-counter semantics
// (NativeAPI.actor.cpp:2156/2160 snapshotRywEnabled++/--; ReadYourWrites.actor.cpp:2082 seeds each
// new tx): SetSnapshotRywEnable() then SetSnapshotRywDisable() nets to ZERO — the new tx stays
// ENABLED — not last-wins-disabled (codex #331; a bool would get this wrong).
func TestDatabaseDefault_SnapshotRYW_IsCounter(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	opts := DatabaseOptions{db: idb}
	_ = opts.SetSnapshotRywEnable()  // net -1
	_ = opts.SetSnapshotRywDisable() // net  0
	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t)
	if inner.SnapshotRYWDisableCount() > 0 {
		t.Errorf("enable+disable must net to enabled (count 0), not last-wins disabled — got count %d", inner.SnapshotRYWDisableCount())
	}

	idb2 := &internalDB{}
	o2 := DatabaseOptions{db: idb2}
	_ = o2.SetSnapshotRywDisable()
	_ = o2.SetSnapshotRywDisable()
	_ = o2.SetSnapshotRywEnable()
	tr2, inner2 := newBareOptionsTx()
	Database{d: idb2}.applyTxDefaults(tr2.t)
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
	if !idb.txDefaults.bypassUnreadable {
		t.Fatal("SetTransactionBypassUnreadable must store the DB default (codex #331)")
	}

	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t)
	if !inner.BypassUnreadable() {
		t.Error("DB-level SetTransactionBypassUnreadable must set bypass_unreadable on the new tx")
	}
}

// TestDatabaseDefault_SnapshotRYW_RetryIdempotent pins codex #331: applyTxDefaults re-runs on EVERY
// retry attempt against the same inner tx (client.Transact re-invokes the closure) and reset()
// preserves snapshotRYWDisableCount, so the counter must be SET (idempotent), not incremented — or
// it drifts (-1,-2,… per attempt) and changes snapshot-read semantics on retries.
func TestDatabaseDefault_SnapshotRYW_RetryIdempotent(t *testing.T) {
	t.Parallel()
	idb := &internalDB{}
	_ = (DatabaseOptions{db: idb}).SetSnapshotRywDisable() // net 1
	tr, inner := newBareOptionsTx()
	db := Database{d: idb}
	db.applyTxDefaults(tr.t) // attempt 1
	db.applyTxDefaults(tr.t) // attempt 2 (retry)
	db.applyTxDefaults(tr.t) // attempt 3 (retry)
	if got := inner.SnapshotRYWDisableCount(); got != 1 {
		t.Errorf("snapshot-RYW DB default must not drift across retries (SET, not ++): want 1, got %d", got)
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
	if !idb.txDefaults.causalReadRisky {
		t.Fatal("SetTransactionCausalReadRisky must store the DB default")
	}

	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t)
	if !inner.CausalReadRisky() {
		t.Error("DB-level SetTransactionCausalReadRisky must set the GRV causal-read-risky flag on the new tx")
	}
}
