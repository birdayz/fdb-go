package bench

import (
	"fmt"
	"os"
	"strings"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// READ_YOUR_WRITES_DISABLE-after-an-operation differential vs libfdb_c — RFC-059.
//
// libfdb_c forbids setting READ_YOUR_WRITES_DISABLE once any read or write has happened: its
// ReadYourWritesTransaction throws client_invalid_operation on the network thread, captured
// into deferredError, so the option call itself succeeds but EVERY subsequent read and the
// commit surface error 2000. A clean (pre-op) disable is fine. Characterized differentially
// against cgo: the poison covers regular reads, snapshot reads, GetKey, GetRange,
// GetReadVersion, GetEstimatedRangeSizeBytes, and Commit — all uniformly. The Go client used
// to silently disable RYW mid-transaction (no error). This pins the option-set-after-op poison:
// for each sequence the FINAL-op error code must match between the two clients.

func goRYWSeq(t *testing.T, run func(tx gofdb.Transaction) int) int {
	t.Helper()
	tr, err := goClient.CreateTransaction()
	if err != nil {
		t.Fatalf("go create: %v", err)
	}
	defer tr.Cancel()
	return run(tr)
}

func cgoRYWSeq(t *testing.T, run func(tx cgofdb.Transaction) int) int {
	t.Helper()
	tr, err := cgoClient.CreateTransaction()
	if err != nil {
		t.Fatalf("cgo create: %v", err)
	}
	defer tr.Cancel()
	return run(tr)
}

func TestDifferential_RYWDisableAfterOp(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	pfx := fmt.Sprintf("rywdis_%d_%s_", os.Getpid(), ns)
	gk := gofdb.Key(pfx + "k")
	ck := cgofdb.Key(pfx + "k")
	gr := gofdb.KeyRange{Begin: gofdb.Key(pfx), End: gofdb.Key(pfx + "\xff")}
	cr := cgofdb.KeyRange{Begin: cgofdb.Key(pfx), End: cgofdb.Key(pfx + "\xff")}
	gd := func(tx gofdb.Transaction) { _ = tx.Options().SetReadYourWritesDisable() }
	cd := func(tx cgofdb.Transaction) { _ = tx.Options().SetReadYourWritesDisable() }

	cases := []struct {
		name string
		goFn func(tx gofdb.Transaction) int
		cFn  func(tx cgofdb.Transaction) int
	}{
		// Clean disable BEFORE any op → a subsequent storage read succeeds (code 0) on both.
		{
			"clean_disable_then_get",
			func(tx gofdb.Transaction) int { gd(tx); _, e := tx.Get(gk).Get(); return fdbErrorCode(e) },
			func(tx cgofdb.Transaction) int { cd(tx); _, e := tx.Get(ck).Get(); return fdbErrorCode(e) },
		},
		// Set → disable → Get : poisoned (2000) on both.
		{
			"set_disable_get",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.Get(gk).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.Get(ck).Get()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → GetKey.
		{
			"set_disable_getkey",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.GetKey(gofdb.FirstGreaterOrEqual(gk)).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.GetKey(cgofdb.FirstGreaterOrEqual(ck)).Get()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → GetRange.
		{
			"set_disable_getrange",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.GetRange(gr, gofdb.RangeOptions{}).GetSliceWithError()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.GetRange(cr, cgofdb.RangeOptions{}).GetSliceWithError()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → SNAPSHOT Get (snapshot reads poison too).
		{
			"set_disable_snapshot_get",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.Snapshot().Get(gk).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.Snapshot().Get(ck).Get()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → SNAPSHOT GetKey (snapshot getKey goes through ensureReadVersion, so
		// it poisons exactly like snapshot Get — pins the axis @claude flagged as structurally
		// argued but un-probed).
		{
			"set_disable_snapshot_getkey",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.Snapshot().GetKey(gofdb.FirstGreaterOrEqual(gk)).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.Snapshot().GetKey(cgofdb.FirstGreaterOrEqual(ck)).Get()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → GetReadVersion (empirically poisons too).
		{
			"set_disable_get_read_version",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.GetReadVersion().Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.GetReadVersion().Get()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → GetEstimatedRangeSizeBytes (metrics path poisons too).
		{
			"set_disable_estimated_size",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.GetEstimatedRangeSizeBytes(gr).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.GetEstimatedRangeSizeBytes(cr).Get()
				return fdbErrorCode(e)
			},
		},
		// (Completed) Get → disable → Get : the prior read populated the cache → poisoned.
		{
			"get_disable_get",
			func(tx gofdb.Transaction) int {
				tx.Get(gk).MustGet()
				gd(tx)
				_, e := tx.Get(gk).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Get(ck).MustGet()
				cd(tx)
				_, e := tx.Get(ck).Get()
				return fdbErrorCode(e)
			},
		},
		// Set → disable → Commit : the poison kills the commit too.
		{
			"set_disable_commit",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				return fdbErrorCode(tx.Commit().Get())
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				return fdbErrorCode(tx.Commit().Get())
			},
		},
		// Set → disable → GetRangeSplitPoints : the metrics sibling poisons too.
		{
			"set_disable_split_points",
			func(tx gofdb.Transaction) int {
				tx.Set(gk, []byte("v"))
				gd(tx)
				_, e := tx.GetRangeSplitPoints(gr, 1<<20).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				tx.Set(ck, []byte("v"))
				cd(tx)
				_, e := tx.GetRangeSplitPoints(cr, 1<<20).Get()
				return fdbErrorCode(e)
			},
		},
		// READ-ONLY poisoned commit: Get → disable → Commit (no writes) — the read-only commit
		// fast path skips ensureReadVersion, so this exercises the top-of-Commit gate.
		{
			"get_disable_commit_readonly",
			func(tx gofdb.Transaction) int { tx.Get(gk).MustGet(); gd(tx); return fdbErrorCode(tx.Commit().Get()) },
			func(tx cgofdb.Transaction) int { tx.Get(ck).MustGet(); cd(tx); return fdbErrorCode(tx.Commit().Get()) },
		},
		// Clean disable → GetKey (reads storage via the rywDisabled getKey choke, setting the
		// read signal) → re-disable (now poisons) → Get : pins the getKey read choke.
		{
			"getkey_rywdisabled_redisable",
			func(tx gofdb.Transaction) int {
				gd(tx)
				_, _ = tx.GetKey(gofdb.FirstGreaterOrEqual(gk)).Get()
				gd(tx)
				_, e := tx.Get(gk).Get()
				return fdbErrorCode(e)
			},
			func(tx cgofdb.Transaction) int {
				cd(tx)
				_, _ = tx.GetKey(cgofdb.FirstGreaterOrEqual(ck)).Get()
				cd(tx)
				_, e := tx.Get(ck).Get()
				return fdbErrorCode(e)
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goCode := goRYWSeq(t, tc.goFn)
			cCode := cgoRYWSeq(t, tc.cFn)
			if goCode != cCode {
				t.Fatalf("%s: final-op error code differs: go=%d cgo=%d", tc.name, goCode, cCode)
			}
		})
	}
}
