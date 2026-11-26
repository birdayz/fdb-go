package simfdb

import (
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestSnapshotSharesReadVersionWithParent pins the invariant that a Snapshot() view and its
// transaction read at ONE version.
//
// The shape is the record layer's: the transaction's FIRST read is a snapshot read, and the
// read-conflict on that data is registered separately (AddReadConflictKey), which is what
// BunchedMap does. A snapshot that pins its own GRV leaves the parent to pin a LATER one at
// commit time; the resolver then compares the concurrent write's commit version against that
// later read version, finds it is not newer, and BLESSES a lost update. Real FDB returns 1020.
func TestSnapshotSharesReadVersionWithParent(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "s", "0")

	tx := db.newTxn()
	// Snapshot read first — this is what pins the GRV.
	if got := string(tx.Snapshot().Get(k("s")).MustGet()); got != "0" {
		t.Fatalf("snapshot read = %q, want %q", got, "0")
	}
	// The read-conflict is declared explicitly, exactly as BunchedMap.entryForKey does.
	if err := tx.AddReadConflictKey(k("s")); err != nil {
		t.Fatalf("AddReadConflictKey: %v", err)
	}

	// A concurrent writer lands between the snapshot read and this transaction's commit.
	db.Transact(func(w fdb.WritableTransaction) (any, error) {
		w.Set(k("s"), []byte("concurrent"))
		return nil, nil
	})

	// Write back a value derived from what the snapshot read — the lost-update shape.
	tx.Set(k("s"), []byte("0+mine"))
	err := tx.Commit().Get()
	if code := errCode(t, err); code != 1020 {
		t.Fatalf("commit after concurrent write to a snapshot-read key: got err=%v (code %d), "+
			"want not_committed(1020). A Snapshot() that forks the read version blesses this "+
			"lost update.", err, code)
	}
}

// TestSnapshotReadSeesOwnWrites pins that snapshot reads are read-your-writes.
//
// FDB's snapshot suppresses CONFLICT RANGES, not the RYW merge (SNAPSHOT_RYW_ENABLE is the
// default). A Snapshot() that copies the write buffer freezes it at the length it had when
// Snapshot() was called, so writes issued afterwards on the transaction vanish from snapshot
// reads — including writes issued through a snapshot handle taken earlier in the same call
// stack, which is how the record layer's read-modify-write paths are written.
func TestSnapshotReadSeesOwnWrites(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "old")

	tx := db.newTxn()
	snap := tx.Snapshot() // taken BEFORE the write

	tx.Set(k("a"), []byte("new"))
	tx.Set(k("b"), []byte("fresh"))

	if got := string(snap.Get(k("a")).MustGet()); got != "new" {
		t.Errorf("snapshot Get of an overwritten key = %q, want %q", got, "new")
	}
	if got := string(snap.Get(k("b")).MustGet()); got != "fresh" {
		t.Errorf("snapshot Get of a newly written key = %q, want %q", got, "fresh")
	}

	kvs := snap.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).GetSliceOrPanic()
	got := map[string]string{}
	for _, kv := range kvs {
		got[string(kv.Key)] = string(kv.Value)
	}
	if got["a"] != "new" || got["b"] != "fresh" {
		t.Errorf("snapshot GetRange = %v, want a=new b=fresh", got)
	}
	if k := string(snap.GetKey(fdb.FirstGreaterOrEqual(k("b"))).MustGet()); k != "b" {
		t.Errorf("snapshot GetKey over a pending write = %q, want %q", k, "b")
	}
}

// TestSnapshotSharesOptionsWithParent pins that an option set through a snapshot handle takes
// effect on the transaction, and vice versa — one option set, not two. The real client's
// Snapshot.Options() returns a handle bound to the parent transaction; a value-copied snapshot
// gets a handle bound to the PARENT while its own flag fields stay stale, so the two views
// disagree about read-your-writes.
func TestSnapshotSharesOptionsWithParent(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "old")

	// The write is issued BEFORE the snapshot is taken, so a frozen-buffer fork would still see
	// it — this isolates the OPTION direction from the buffer direction.
	tx := db.newTxn()
	tx.Set(k("a"), []byte("new"))
	snap := tx.Snapshot()
	if err := snap.Options().SetReadYourWritesDisable(); err != nil {
		t.Fatalf("SetReadYourWritesDisable: %v", err)
	}

	if got := string(snap.Get(k("a")).MustGet()); got != "old" {
		t.Errorf("snapshot Get with RYW disabled via the snapshot handle = %q, want %q", got, "old")
	}
	if got := string(tx.Get(k("a")).MustGet()); got != "old" {
		t.Errorf("transaction Get after RYW disabled via the snapshot handle = %q, want %q", got, "old")
	}

	// And the other direction: an option set on the transaction governs the snapshot view.
	tx2 := db.newTxn()
	tx2.Set(k("a"), []byte("new2"))
	snap2 := tx2.Snapshot()
	if err := tx2.Options().SetReadYourWritesDisable(); err != nil {
		t.Fatalf("SetReadYourWritesDisable on tx: %v", err)
	}
	if got := string(snap2.Get(k("a")).MustGet()); got != "old" {
		t.Errorf("snapshot Get with RYW disabled via the transaction handle = %q, want %q", got, "old")
	}
}

// TestSnapshotOfSnapshotIsSameView pins fdb/snapshot.go's `func (sn Snapshot) Snapshot()
// ReadTransaction { return sn }` — snapshotting a snapshot is idempotent, and in particular does
// not produce a second, independently-versioned view.
func TestSnapshotOfSnapshotIsSameView(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "s", "0")
	tx := db.newTxn()
	snap := tx.Snapshot()
	snap2 := snap.Snapshot()
	snap2.Get(k("s")).MustGet()
	tx.Set(k("s"), []byte("mine"))
	if got := string(snap2.Get(k("s")).MustGet()); got != "mine" {
		t.Fatalf("Snapshot().Snapshot() Get = %q, want %q (same view)", got, "mine")
	}
	if len(tx.readConflicts) != 0 {
		t.Fatalf("Snapshot().Snapshot() reads took %d read conflict ranges, want 0", len(tx.readConflicts))
	}
}
