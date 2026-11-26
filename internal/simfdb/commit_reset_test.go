package simfdb

// A handle that has committed begins a NEW logical transaction on its next use. The
// pure-Go client realizes that in Transaction.postCommitReset, which clears the read
// version (hasReadVersion=false, readVersion=0) along with the mutation buffer and
// BOTH conflict-range vectors, on the normal commit path AND on the read-only fast
// path (client/transaction.go:1810-1821, :3198-3204). SimFDB must match: it is the
// third fdb.BackendDatabase, and the record layer's DDL path really does commit twice
// on one handle (ddl.go commits inside a db.Run closure whose Transact commits again).
//
// Leaving the read version pinned across a commit breaks two things at once, so each
// is pinned separately below: reads on the reused handle are stuck in the past, and
// the reused handle conflicts against its OWN earlier commit.

import (
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestCommitReset_ReusedHandleReadsAtFreshVersion pins that a reused handle sees
// writes committed after its previous commit. With the read version left pinned, the
// second read is served from the pre-commit snapshot and silently returns stale data
// — the worst failure shape available, since nothing errors.
func TestCommitReset_ReusedHandleReadsAtFreshVersion(t *testing.T) {
	t.Parallel()
	db := New(nil)

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("shared"), []byte("v1"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	handle, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Establish a read version, then commit the handle.
	if got := string(handle.Get(k("shared")).MustGet()); got != "v1" {
		t.Fatalf("first read = %q, want v1", got)
	}
	handle.Set(k("other"), []byte("x"))
	if err := handle.Commit().Get(); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// A DIFFERENT transaction advances the database.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("shared"), []byte("v2"))
		return nil, nil
	}); err != nil {
		t.Fatalf("external write: %v", err)
	}

	// The reused handle is a new logical transaction: it must take a FRESH read
	// version and therefore see v2.
	if got := string(handle.Get(k("shared")).MustGet()); got != "v2" {
		t.Fatalf("reused handle read = %q, want v2 — the handle is still reading at its "+
			"pre-commit read version, so a committed handle silently serves stale data", got)
	}
}

// TestCommitReset_ReusedHandleDoesNotSelfConflict pins the conflict half: a handle
// that reads-then-writes a key, commits, and does it again must not be told 1020 by
// its own previous commit. With the read version pinned, the handle's own commit sits
// at a version greater than its (stale) read version and overlaps its read-conflict
// range, so SSI resolution reports a conflict against itself.
func TestCommitReset_ReusedHandleDoesNotSelfConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)

	handle, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for round := 1; round <= 3; round++ {
		// A real read (adds a read-conflict range) followed by a write to the same key.
		handle.Get(k("counter")).MustGet()
		handle.Set(k("counter"), []byte{byte(round)})
		if err := handle.Commit().Get(); err != nil {
			t.Fatalf("round %d commit: %v (code %d) — a reused handle must not conflict "+
				"against its own previous commit; the read version was not cleared on commit",
				round, err, errCode(t, err))
		}
	}

	final, err := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return tx.Get(k("counter")).MustGet(), nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := final.([]byte); len(got) != 1 || got[0] != 3 {
		t.Fatalf("counter = %v, want [3] — the last round's write did not land", got)
	}
}

// TestCommitReset_ReadOnlyFastPathResets covers the path that reset NOTHING: a
// read-only commit is completed client-side, but the client still runs the full
// postCommitReset there. A read-only transaction accumulates READ-conflict ranges, so
// skipping the reset leaves them attached to the handle — and the handle's next
// commit is then resolved against ranges it never read in this logical transaction.
func TestCommitReset_ReadOnlyFastPathResets(t *testing.T) {
	t.Parallel()
	db := New(nil)

	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("watched"), []byte("a"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	handle, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Read-only: builds a read-conflict range on "watched", then commits with no
	// mutations at all — the client-side fast path.
	handle.Get(k("watched")).MustGet()
	if err := handle.Commit().Get(); err != nil {
		t.Fatalf("read-only commit: %v", err)
	}

	// Someone else writes the key this handle read BEFORE its commit.
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(k("watched"), []byte("b"))
		return nil, nil
	}); err != nil {
		t.Fatalf("external write: %v", err)
	}

	// The reused handle writes an UNRELATED key. Its previous logical transaction's
	// read of "watched" must not be part of this one, so this must commit.
	handle.Set(k("unrelated"), []byte("z"))
	if err := handle.Commit().Get(); err != nil {
		t.Fatalf("post-read-only commit: %v (code %d) — the read-only fast path left the "+
			"previous transaction's read-conflict ranges and read version on the handle",
			err, errCode(t, err))
	}
}

// TestCommitReset_ClearsStateDirectly inspects the transaction state after each commit
// path. The behavioural tests above can only observe the reset through conflict and
// visibility outcomes; this one names the fields, so a partial reset (buffer cleared,
// read version kept — the original defect) is reported as exactly that.
func TestCommitReset_ClearsStateDirectly(t *testing.T) {
	t.Parallel()

	assertReset := func(t *testing.T, tx *simTxn, path string) {
		t.Helper()
		if tx.rvSet {
			t.Errorf("%s: rvSet still true after commit — the reused handle would read at the "+
				"pre-commit version (client postCommitReset sets hasReadVersion=false)", path)
		}
		if tx.readVersion != 0 {
			t.Errorf("%s: readVersion = %d after commit, want 0", path, tx.readVersion)
		}
		if len(tx.buffer) != 0 {
			t.Errorf("%s: %d buffered mutations survived the commit", path, len(tx.buffer))
		}
		if len(tx.readConflicts) != 0 {
			t.Errorf("%s: %d read-conflict ranges survived the commit", path, len(tx.readConflicts))
		}
		if len(tx.writeConflicts) != 0 {
			t.Errorf("%s: %d write-conflict ranges survived the commit", path, len(tx.writeConflicts))
		}
	}

	// Normal (mutating) commit path.
	dbW := New(nil)
	hw, err := dbW.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	hw.Get(k("r")).MustGet()
	hw.Set(k("w"), []byte("1"))
	if err := hw.Commit().Get(); err != nil {
		t.Fatalf("mutating commit: %v", err)
	}
	assertReset(t, hw.(*simTxn), "mutating commit")

	// Read-only client-side fast path.
	dbR := New(nil)
	hr, err := dbR.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	hr.Get(k("r")).MustGet()
	if err := hr.Commit().Get(); err != nil {
		t.Fatalf("read-only commit: %v", err)
	}
	assertReset(t, hr.(*simTxn), "read-only commit")

	// The post-commit metadata a caller may still read must SURVIVE the reset —
	// GetCommittedVersion / GetVersionstamp are defined after a successful commit.
	if got := hw.(*simTxn).committedVersion; got <= 0 {
		t.Errorf("mutating commit: committedVersion = %d, want > 0 (retained for GetCommittedVersion)", got)
	}
	if len(hw.(*simTxn).versionstamp) == 0 {
		t.Error("mutating commit: versionstamp cleared — GetVersionstamp must still resolve")
	}
	if got := hr.(*simTxn).committedVersion; got != -1 {
		t.Errorf("read-only commit: committedVersion = %d, want -1 (no commit version taken)", got)
	}
}
