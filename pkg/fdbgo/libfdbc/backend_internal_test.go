//go:build cgo

package libfdbc

import (
	"errors"
	"fmt"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// TestErrorShim_RetryRecognizedAndContextPreserved pins the two-way error bridge
// (Torvalds review): an fdb.Error the record layer propagated up — wrapped with
// %w context — must (1) still be recognized as a cgofdb.Error by cgofdb's
// retryable() loop so the retryable code is delegated to libfdb_c's OnError, and
// (2) on a terminal failure, round-trip back through convErr with its raw code AND
// its wrapped context intact (the pure-Go backend keeps that context; this backend
// must too).
func TestErrorShim_RetryRecognizedAndContextPreserved(t *testing.T) {
	t.Parallel()

	// Record layer returns a wrapped retryable fdb error (1020 = not_committed).
	orig := fmt.Errorf("save record xyz: %w", fdb.Error{Code: 1020})

	// On the way OUT of the Transact callback.
	shimmed := toCgoErr(orig)

	// cgofdb's retryable() does exactly this to decide retry + call OnError.
	var ce cgofdb.Error
	if !errors.As(shimmed, &ce) {
		t.Fatal("cgofdb.retryable() must still see a cgofdb.Error (else no retry delegation)")
	}
	if ce.Code != 1020 {
		t.Fatalf("cgofdb.Error.Code = %d, want 1020", ce.Code)
	}

	// Terminal failure: the libfdbc database boundary maps it back. Context AND code
	// must both survive.
	back := convErr(shimmed)
	if back.Error() != orig.Error() {
		t.Fatalf("context lost across round-trip:\n got=%q\nwant=%q", back.Error(), orig.Error())
	}
	var fe fdb.Error
	if !errors.As(back, &fe) || fe.Code != 1020 {
		t.Fatalf("fdb.Error code lost across round-trip: %v", back)
	}
}

// TestErrorShim_NonFDBPassThrough confirms a record-layer semantic error (not an
// fdb.Error) is NOT shimmed — it passes through both directions unchanged, so
// cgofdb treats it as terminal (no spurious retry) and the caller sees it verbatim.
func TestErrorShim_NonFDBPassThrough(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("record already exists")
	out := toCgoErr(sentinel)
	if out != sentinel {
		t.Fatalf("non-fdb error must pass through toCgoErr unchanged, got %v", out)
	}
	var ce cgofdb.Error
	if errors.As(out, &ce) {
		t.Fatal("a non-fdb error must NOT look like a cgofdb.Error (would trigger a bogus retry)")
	}
	if got := convErr(out); got != sentinel {
		t.Fatalf("non-fdb error must pass through convErr unchanged, got %v", got)
	}
}

// TestConvErr_PlainCgoError maps a genuine cgofdb.Error (as produced inside the
// callback by a future's Get) to an fdb.Error with the same raw code.
func TestConvErr_PlainCgoError(t *testing.T) {
	t.Parallel()

	out := convErr(cgofdb.Error{Code: 1007})
	var fe fdb.Error
	if !errors.As(out, &fe) || fe.Code != 1007 {
		t.Fatalf("convErr(cgofdb.Error{1007}) = %v, want fdb.Error{1007}", out)
	}
}
