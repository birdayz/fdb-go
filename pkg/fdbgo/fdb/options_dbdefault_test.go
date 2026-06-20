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
	if !idb.txDefaults.snapshotRywDisabled {
		t.Fatal("SetSnapshotRywDisable must store the DB default (codex #331), not silently drop it")
	}

	tr, inner := newBareOptionsTx()
	Database{d: idb}.applyTxDefaults(tr.t)
	if inner.SnapshotRYWDisableCount() <= 0 {
		t.Errorf("DB-level SetSnapshotRywDisable must disable snapshot RYW on the new tx, got count %d", inner.SnapshotRYWDisableCount())
	}

	// SetSnapshotRywEnable toggles the default back off (last-call-wins at the DB level).
	if err := (DatabaseOptions{db: idb}).SetSnapshotRywEnable(); err != nil {
		t.Fatalf("SetSnapshotRywEnable: %v", err)
	}
	if idb.txDefaults.snapshotRywDisabled {
		t.Error("SetSnapshotRywEnable must clear the disable default")
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
