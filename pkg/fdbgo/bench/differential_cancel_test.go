package bench

import (
	"fmt"
	"os"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// Cancelled-transaction error-CODE differential vs libfdb_c — RFC-068 (RFC-010 C3 hunt).
//
// libfdb_c's cancel() does resetPromise.sendError(transaction_cancelled) (ReadYourWrites.actor.cpp
// :2730); every op racing that promise resolves with transaction_cancelled (1025). The pure-Go
// client returned a bare string error (no code) for ops on a cancelled txn — an app branching on
// err.Code == 1025 (the Go analogue of catch(transaction_cancelled)) matched C/Java but not Go.
//
// Each op is run on a freshly-CREATED-then-CANCELLED transaction (direct CreateTransaction, not
// Transact, so the cancel is the only state) and the returned code is compared go-vs-cgo.
// AddReadConflictRange is included as the negative: it buffers without the read/commit gate, so a
// cancelled txn (which never commits) returns 0 on both. The five read/commit ops were red
// (go=-1 string, cgo=1025) before the fix and green (both 1025) after.
//
// NOT covered (separate, entangled divergence, see RFC-068): commit→cancel→get returns
// used_during_commit (2017) in libfdb_c but a cancelled error in Go, because Go auto-resets a txn
// to active after a successful commit (a deliberate documented extension to match the binding
// tester's handle reuse) whereas C++ leaves it committed. That corner is tied to the
// auto-reset extension, not this clean cancelled-error-code surface.
func TestDifferential_CancelLifecycle(t *testing.T) {
	t.Parallel()
	k := fmt.Sprintf("differ_cancel_%d_k", os.Getpid())

	goCancelledOp := func(op func(tr gofdb.Transaction) error) int {
		tr, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatalf("go CreateTransaction: %v", err)
		}
		tr.Cancel()
		return fdbErrorCode(op(tr))
	}
	cgoCancelledOp := func(op func(tr cgofdb.Transaction) error) int {
		tr, err := cgoClient.CreateTransaction()
		if err != nil {
			t.Fatalf("cgo CreateTransaction: %v", err)
		}
		tr.Cancel()
		return fdbErrorCode(op(tr))
	}

	cases := []struct {
		name     string
		wantCode int
		g        func(tr gofdb.Transaction) error
		c        func(tr cgofdb.Transaction) error
	}{
		{
			name: "get", wantCode: 1025,
			g: func(tr gofdb.Transaction) error { _, e := tr.Get(gofdb.Key(k)).Get(); return e },
			c: func(tr cgofdb.Transaction) error { _, e := tr.Get(cgofdb.Key(k)).Get(); return e },
		},
		{
			name: "get_read_version", wantCode: 1025,
			g: func(tr gofdb.Transaction) error { _, e := tr.GetReadVersion().Get(); return e },
			c: func(tr cgofdb.Transaction) error { _, e := tr.GetReadVersion().Get(); return e },
		},
		{
			name: "get_key", wantCode: 1025,
			g: func(tr gofdb.Transaction) error {
				_, e := tr.GetKey(gofdb.KeySelector{Key: gofdb.Key(k), OrEqual: false, Offset: 1}).Get()
				return e
			},
			c: func(tr cgofdb.Transaction) error {
				_, e := tr.GetKey(cgofdb.KeySelector{Key: cgofdb.Key(k), OrEqual: false, Offset: 1}).Get()
				return e
			},
		},
		{
			name: "get_range", wantCode: 1025,
			g: func(tr gofdb.Transaction) error {
				_, e := tr.GetRange(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(k + "\xff")}, gofdb.RangeOptions{}).GetSliceWithError()
				return e
			},
			c: func(tr cgofdb.Transaction) error {
				_, e := tr.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(k), End: cgofdb.Key(k + "\xff")}, cgofdb.RangeOptions{}).GetSliceWithError()
				return e
			},
		},
		{
			name: "commit", wantCode: 1025,
			g: func(tr gofdb.Transaction) error { tr.Set(gofdb.Key(k), []byte("v")); return tr.Commit().Get() },
			c: func(tr cgofdb.Transaction) error { tr.Set(cgofdb.Key(k), []byte("v")); return tr.Commit().Get() },
		},
		{
			// Negative: buffers without the read/commit gate; a cancelled txn never commits → 0.
			name: "add_read_conflict_range", wantCode: 0,
			g: func(tr gofdb.Transaction) error {
				return tr.AddReadConflictRange(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(k + "\xff")})
			},
			c: func(tr cgofdb.Transaction) error {
				return tr.AddReadConflictRange(cgofdb.KeyRange{Begin: cgofdb.Key(k), End: cgofdb.Key(k + "\xff")})
			},
		},
		// Ops that BYPASS ensureReadVersion and so needed their own checkCancelled gate
		// (FDB-C++ reviewer caught these — all silently diverged before: go=0/2015, cgo=1025).
		{
			name: "get_estimated_range_size", wantCode: 1025,
			g: func(tr gofdb.Transaction) error {
				_, e := tr.GetEstimatedRangeSizeBytes(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(k + "\xff")}).Get()
				return e
			},
			c: func(tr cgofdb.Transaction) error {
				_, e := tr.GetEstimatedRangeSizeBytes(cgofdb.KeyRange{Begin: cgofdb.Key(k), End: cgofdb.Key(k + "\xff")}).Get()
				return e
			},
		},
		{
			name: "get_range_split_points", wantCode: 1025,
			g: func(tr gofdb.Transaction) error {
				_, e := tr.GetRangeSplitPoints(gofdb.KeyRange{Begin: gofdb.Key(k), End: gofdb.Key(k + "\xff")}, 1000).Get()
				return e
			},
			c: func(tr cgofdb.Transaction) error {
				_, e := tr.GetRangeSplitPoints(cgofdb.KeyRange{Begin: cgofdb.Key(k), End: cgofdb.Key(k + "\xff")}, 1000).Get()
				return e
			},
		},
		{
			// OnError on a cancelled txn must NOT reset-and-retry; it returns 1025 (a retryable
			// input like 1020 returned nil before the fix — would reuse a cancelled handle).
			name: "on_error", wantCode: 1025,
			g: func(tr gofdb.Transaction) error { return tr.OnError(gofdb.Error{Code: 1020}).Get() },
			c: func(tr cgofdb.Transaction) error { return tr.OnError(cgofdb.Error{Code: 1020}).Get() },
		},
		{
			name: "get_versionstamp", wantCode: 1025, // 1025 out-ranks the not-yet-committed 2015
			g: func(tr gofdb.Transaction) error { _, e := tr.GetVersionstamp().Get(); return e },
			c: func(tr cgofdb.Transaction) error { _, e := tr.GetVersionstamp().Get(); return e },
		},
		{
			// LocalityGetAddressesForKey also bypasses ensureReadVersion (go=0 before the fix).
			name: "locality_get_addresses", wantCode: 1025,
			g: func(tr gofdb.Transaction) error { _, e := tr.LocalityGetAddressesForKey(gofdb.Key(k)).Get(); return e },
			c: func(tr cgofdb.Transaction) error {
				_, e := tr.LocalityGetAddressesForKey(cgofdb.Key(k)).Get()
				return e
			},
		},
		{
			// Watch is gated via WatchSetup→ensureReadVersion: resolves 1025 synchronously (no poll).
			name: "watch", wantCode: 1025,
			g: func(tr gofdb.Transaction) error { return tr.Watch(gofdb.Key(k)).Get() },
			c: func(tr cgofdb.Transaction) error { return tr.Watch(cgofdb.Key(k)).Get() },
		},
		{
			// Control: GetApproximateSize is a pure size getter — C++ returns the raw size with NO
			// resetPromise check, code 0. Proves checkCancelled is NOT over-applied to it.
			name: "get_approximate_size", wantCode: 0,
			g: func(tr gofdb.Transaction) error { _, e := tr.GetApproximateSize().Get(); return e },
			c: func(tr cgofdb.Transaction) error { _, e := tr.GetApproximateSize().Get(); return e },
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goCode := goCancelledOp(tc.g)
			cCode := cgoCancelledOp(tc.c)
			if goCode != cCode {
				t.Fatalf("%s on cancelled txn: error code differs — go=%d cgo=%d (C++ spec: %d)", tc.name, goCode, cCode, tc.wantCode)
			}
			if goCode != tc.wantCode {
				t.Fatalf("%s on cancelled txn: both returned %d but C++ spec is %d", tc.name, goCode, tc.wantCode)
			}
		})
	}
}
