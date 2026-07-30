package simfdb

import (
	"bytes"
	"encoding/binary"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
)

func seed(db *SimDB, kv ...string) {
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		for i := 0; i+1 < len(kv); i += 2 {
			tx.Set(k(kv[i]), []byte(kv[i+1]))
		}
		return nil, nil
	})
}

func TestGetKeySelectorResolution(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "b", "1", "d", "2", "f", "3") // keys: b, d, f

	get := func(sel fdb.Selectable) string {
		v, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
			return string(tx.GetKey(sel).MustGet()), nil
		})
		return v.(string)
	}
	cases := []struct {
		name string
		sel  fdb.KeySelector
		want string
	}{
		{"FGE(c)->d", fdb.FirstGreaterOrEqual(k("c")), "d"},
		{"FGE(d)->d", fdb.FirstGreaterOrEqual(k("d")), "d"},
		{"FGT(d)->f", fdb.FirstGreaterThan(k("d")), "f"},
		{"LLE(d)->d", fdb.LastLessOrEqual(k("d")), "d"},
		{"LLT(d)->b", fdb.LastLessThan(k("d")), "b"},
		{"FGE(a)->b", fdb.FirstGreaterOrEqual(k("a")), "b"},
	}
	for _, c := range cases {
		if got := get(c.sel); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSnapshotReadAddsNoConflict(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "s", "0")
	// txA snapshot-reads s (no read conflict), then writes a different key and commits after
	// txB writes s. A snapshot read must NOT cause a conflict.
	txA := db.newTxn()
	txA.Snapshot().Get(k("s")).MustGet() // snapshot read: no conflict
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Set(k("s"), []byte("B")); return nil, nil })
	txA.Set(k("other"), []byte("A"))
	if err := txA.Commit().Get(); err != nil {
		t.Fatalf("snapshot read should not conflict, got %v", err)
	}
}

func TestTransactionTooOldWindow(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Simulate a far-advanced database so an ancient read version falls outside the MVCC window.
	db.lastVersion = mvccWindow + 100
	tx := db.newTxn()
	tx.SetReadVersion(0) // ancient
	tx.Get(k("x")).MustGet()
	tx.Set(k("x"), []byte("v"))
	if code := errCode(t, tx.Commit().Get()); code != 1007 {
		t.Fatalf("ancient read version: got %d, want transaction_too_old(1007)", code)
	}

	// A write-only transaction (no read conflict ranges) at an ancient read version does NOT
	// get too_old — 1007 gates on having read conflict ranges.
	wtx := db.newTxn()
	wtx.SetReadVersion(0)
	wtx.Set(k("y"), []byte("v"))
	if err := wtx.Commit().Get(); err != nil {
		t.Fatalf("write-only ancient txn should commit, got %v", err)
	}
}

func TestVersionstampedValueStamped(t *testing.T) {
	t.Parallel()
	db := New(nil)
	// Value: 3-byte prefix + 10-byte placeholder + 4-byte LE offset (=3).
	val := append([]byte{1, 2, 3}, bytes.Repeat([]byte{0xFF}, 10)...)
	off := make([]byte, 4)
	binary.LittleEndian.PutUint32(off, 3)
	val = append(val, off...)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.SetVersionstampedValue(k("vk"), val)
		return nil, nil
	})
	got, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return tx.Get(k("vk")).MustGet(), nil
	})
	stamped := got.([]byte)
	if len(stamped) != 13 { // 3 prefix + 10 stamp, offset stripped
		t.Fatalf("stamped value len = %d, want 13", len(stamped))
	}
	if bytes.Equal(stamped[3:13], bytes.Repeat([]byte{0xFF}, 10)) {
		t.Fatal("value placeholder not overwritten")
	}
}

func TestClearRangeRemovesOnlyRange(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "a", "1", "b", "2", "c", "3", "d", "4")
	db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.ClearRange(fdb.KeyRange{Begin: k("b"), End: k("d")}) // clears b, c (not a, not d)
		return nil, nil
	})
	got, _ := db.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return keysOf(tx.GetRange(fdb.KeyRange{Begin: k("a"), End: k("z")}, fdb.RangeOptions{}).GetSliceOrPanic()), nil
	})
	if !eqStrs(got.([]string), []string{"a", "d"}) {
		t.Fatalf("after ClearRange [b,d): keys = %v, want [a d]", got)
	}
}

func TestReadOnlyCommitShortCircuits(t *testing.T) {
	t.Parallel()
	db := New(nil)
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Set(k("seed"), []byte("v")); return nil, nil })

	// A read-only transaction's commit is a client-side no-op in real FDB — it never conflicts,
	// never ages out, and never returns commit_unknown. So an injected fault must NOT fire on it.
	db.InjectOnce(1021)
	tx := db.newTxn()
	tx.Get(k("seed")).MustGet() // read only: adds a read conflict, no write
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("read-only commit should short-circuit to success, got %v", err)
	}
	// A read-only commit takes no commit version.
	if cv, _ := tx.GetCommittedVersion(); cv != -1 {
		t.Fatalf("read-only committedVersion = %d, want -1", cv)
	}
	// The injection was NOT consumed by the read-only commit — it is still pending for the next
	// real (write) commit.
	tx2 := db.newTxn()
	tx2.Set(k("w"), []byte("v"))
	if code := errCode(t, tx2.Commit().Get()); code != 1021 {
		t.Fatalf("injection should still fire on the next write commit, got %d", code)
	}
}

func TestInjectOnce(t *testing.T) {
	t.Parallel()
	db := New(nil)

	// 1020 (not_committed) fires BEFORE apply: the commit fails and the data is NOT written.
	db.InjectOnce(1020)
	tx := db.newTxn()
	tx.Set(k("x"), []byte("v"))
	if code := errCode(t, tx.Commit().Get()); code != 1020 {
		t.Fatalf("InjectOnce(1020): got %d, want 1020", code)
	}
	if got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.Get(k("x")).MustGet(), nil
	}); got.([]byte) != nil {
		t.Fatal("1020 fired before apply, but x was written")
	}

	// 1021 (commit_unknown_result) has two real branches. The APPLIED branch leaves the data
	// durable while the caller is told nothing — the non-idempotent-retry surface a real
	// workload must survive.
	db.InjectOnce(CommitUnknownApplied)
	tx2 := db.newTxn()
	tx2.Set(k("y"), []byte("v"))
	if code := errCode(t, tx2.Commit().Get()); code != 1021 {
		t.Fatalf("InjectOnce(CommitUnknownApplied): got %d, want 1021", code)
	}
	if got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return string(rtx.Get(k("y")).MustGet()), nil
	}); got.(string) != "v" {
		t.Fatal("the applied branch of 1021 left y NOT durable")
	}

	// The DISCARDED branch is the other half: the commit never reached the proxy, so nothing
	// is written even though the caller sees the same 1021.
	db.InjectOnce(CommitUnknownDiscarded)
	tx3 := db.newTxn()
	tx3.Set(k("y2"), []byte("v"))
	if code := errCode(t, tx3.Commit().Get()); code != 1021 {
		t.Fatalf("InjectOnce(CommitUnknownDiscarded): got %d, want 1021", code)
	}
	if got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.Get(k("y2")).MustGet(), nil
	}); got.([]byte) != nil {
		t.Fatal("the discarded branch of 1021 wrote y2 anyway")
	}

	// Injection is consumed after one commit — the next commit succeeds cleanly.
	if err := db.run(func(tx fdb.WritableTransaction) { tx.Set(k("z"), []byte("v")) }); err != nil {
		t.Fatalf("post-injection commit should succeed: %v", err)
	}
}

func TestIdempotentDoubleCommit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	tx := db.newTxn()
	tx.Add(k("cnt"), []byte{1, 0, 0, 0, 0, 0, 0, 0}) // +1 little-endian
	tx.Set(k("x"), []byte("v"))
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// A second commit with nothing new buffered is a no-op — matches real FDB's post-commit
	// reset (the SQL DDL path commits inside a Run closure, then Transact commits again). It
	// must NOT error and must NOT double-apply the atomic Add.
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("double commit should be a no-op, got %v", err)
	}
	// Post-commit reads still resolve.
	if cv, err := tx.GetCommittedVersion(); err != nil || cv <= 0 {
		t.Fatalf("GetCommittedVersion after double commit: cv=%d err=%v", cv, err)
	}
	if vs := tx.GetVersionstamp().MustGet(); len(vs) != 10 {
		t.Fatalf("versionstamp len = %d, want 10", len(vs))
	}
	// The Add applied exactly once (not doubled), and the Set persisted once.
	got, _ := db.ReadTransact(func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.Get(k("cnt")).MustGet(), nil
	})
	if binary.LittleEndian.Uint64(got.([]byte)) != 1 {
		t.Fatalf("Add double-applied under double commit: cnt = %d, want 1", binary.LittleEndian.Uint64(got.([]byte)))
	}
}

func TestNewSelectsAPIVersion(t *testing.T) {
	t.Parallel()
	// A SimDB stands in for an opened FDB database, which cannot exist without a selected API
	// version — the versionstamp/tuple layer reads fdb.GetAPIVersion(). New must enforce that
	// precondition so the first versionstamp write doesn't fail api_version_unset(2200). (The
	// DST hunt caught this on its first VERSION-index save.)
	_ = New(nil)
	if !fdb.IsAPIVersionSelected() {
		t.Fatal("New did not select an API version; versionstamp writes would fail 2200")
	}
	// A versionstamped-value write must now round-trip without an api_version_unset error.
	db := New(nil)
	val := append([]byte{9}, bytes.Repeat([]byte{0xFF}, 10)...)
	off := make([]byte, 4)
	binary.LittleEndian.PutUint32(off, 1)
	val = append(val, off...)
	if _, err := db.Transact(func(tx fdb.WritableTransaction) (any, error) {
		tx.SetVersionstampedValue(k("vsk"), val)
		return nil, nil
	}); err != nil {
		t.Fatalf("versionstamped write over SimFDB: %v", err)
	}
}

func TestAddReadConflictRangeExplicit(t *testing.T) {
	t.Parallel()
	db := New(nil)
	seed(db, "k", "0")
	// An explicit read conflict range conflicts with a later write, even without a Get.
	txA := db.newTxn()
	txA.ensureReadVersion()
	if err := txA.AddReadConflictRange(fdb.KeyRange{Begin: k("k"), End: k("l")}); err != nil {
		t.Fatal(err)
	}
	db.Transact(func(tx fdb.WritableTransaction) (any, error) { tx.Set(k("k"), []byte("B")); return nil, nil })
	txA.Set(k("other"), []byte("A"))
	if code := errCode(t, txA.Commit().Get()); code != 1020 {
		t.Fatalf("explicit read conflict range: got %d, want 1020", code)
	}
}

// TestSizeLimitBeatsTooOld pins the ORDER of the two terminal verdicts a commit can reach, for a
// transaction that qualifies for both.
//
// Size limits are enforced client-side, at the mutation that crosses them, long before a commit
// reaches a resolver; transaction_too_old comes back FROM the resolver. So a real client reports
// the size error, and the difference matters beyond the code: 1007 is retryable and 2103 is not,
// so answering 1007 first sends a retry loop spinning where a real client gives up immediately.
//
// SimFDB answered 1007 here until the checks were reordered. Nothing could have caught it
// incidentally — the disagreement needs a read version five million versions behind the latest
// commit, which only a test reaching into db.lastVersion can arrange.
func TestSizeLimitBeatsTooOld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mut  func(tx *simTxn)
		want int
	}{
		{"value_too_large", func(tx *simTxn) {
			tx.Set(k("x"), make([]byte, valueSizeLimit+1))
		}, 2103},
		{"key_too_large", func(tx *simTxn) {
			tx.Set(fdb.Key(make([]byte, keySizeLimit+1)), []byte("v"))
		}, 2102},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := New(nil)
			db.lastVersion = mvccWindow + 100
			tx := db.newTxn()
			tx.SetReadVersion(0)     // ancient: qualifies for 1007
			tx.Get(k("x")).MustGet() // ...but only with a read conflict range
			tc.mut(tx)               // ...and over-size: qualifies for the terminal code too
			if code := errCode(t, tx.Commit().Get()); code != tc.want {
				t.Fatalf("commit code = %d, want %d — a transaction that is BOTH over-size and "+
					"too old reports the size error on a real cluster, because the size check "+
					"is client-side and runs before any resolver sees the commit. Answering "+
					"the retryable 1007 instead makes a retry loop spin on a transaction that "+
					"can never succeed", code, tc.want)
			}
		})
	}
}
