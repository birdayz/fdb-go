package client

import (
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// EVERY ARM OF THE basicLoadBalance RULE, DRIVEN DIRECTLY.
//
// The arms differ only by AtMostOnce, and the two production callers each pass
// a constant, so a corpus run exercises one path per site and never the matrix.
// The dangerous cell in particular -- commit + maybeDelivered -- is the one
// that must NOT become a retry, and nothing in a healthy suite reaches it.
func TestDispositionForReplyErrorMatchesBasicLoadBalance(t *testing.T) {
	t.Parallel()

	fdbErr := func(code int) error { return &wire.FDBError{Code: code} }

	for _, tc := range []struct {
		name       string
		err        error
		atMostOnce bool
		want       replyDisposition
		why        string
	}{
		// The maybeDelivered class: LoadBalance.actor.h:344 is exactly
		// {broken_promise, request_maybe_delivered}.
		{
			"1100 at AtMostOnce::False", fdbErr(ErrBrokenPromise), false, replyTryNextAlternative,
			"GRV must ask the next proxy, not fail the transaction",
		},
		{
			"1030 at AtMostOnce::False", fdbErr(ErrRequestMaybeDelivered), false, replyTryNextAlternative,
			"1030 is the same class as 1100",
		},
		{
			"1100 at AtMostOnce::True", fdbErr(ErrBrokenPromise), true, replyConvertToMaybeDelivered,
			"a commit may already have applied; converting is the ONLY safe answer",
		},
		{
			"1030 at AtMostOnce::True", fdbErr(ErrRequestMaybeDelivered), true, replyConvertToMaybeDelivered,
			"same class, same conversion",
		},

		// Everything else is rethrown unchanged at BOTH settings. If these
		// leaked into a retry arm, a genuine error would be silently re-sent.
		{
			"1020 at AtMostOnce::False", fdbErr(1020), false, replySurfaceError,
			"not_committed is a real answer from a live proxy",
		},
		{"1020 at AtMostOnce::True", fdbErr(1020), true, replySurfaceError, ""},
		{
			"1021 at AtMostOnce::True", fdbErr(ErrCommitUnknownResult), true, replySurfaceError,
			"already the converted form; converting again would loop the meaning",
		},
		{"1007 at AtMostOnce::False", fdbErr(1007), false, replySurfaceError, ""},

		// Non-FDB and nil: a caller that asks about a nil error must not get a
		// retry answer, or a success path could be re-sent.
		{
			"non-FDB error", errors.New("dial tcp: refused"), false, replySurfaceError,
			"a Go-level error is not an in-band reply code",
		},
		{"nil at AtMostOnce::False", nil, false, replySurfaceError, ""},
		{"nil at AtMostOnce::True", nil, true, replySurfaceError, ""},

		// Wrapped, because production errors arrive under %w.
		{
			"wrapped 1100", fmt.Errorf("commit: %w", fdbErr(ErrBrokenPromise)), true, replyConvertToMaybeDelivered,
			"errors.As must see through the wrap the reply parsers add",
		},
	} {
		if got := dispositionForReplyError(tc.err, tc.atMostOnce); got != tc.want {
			t.Errorf("%s: got %v, want %v%s", tc.name, got, tc.want,
				map[bool]string{true: " — " + tc.why}[tc.why != ""])
		}
	}
}

// THE RULE IS REACHED THROUGH THE REAL PARSE PATH, NOT ONLY BY DIRECT CALL.
//
// dispositionForReplyError is only useful if an in-band 1100 actually arrives
// at it wearing the shape the parser produces. This drives the genuine wire
// encoding a dying proxy sends -- ErrorOr<CommitID> with tag=Error -- through
// parseCommitReply and asserts the classification on ITS output, so a change to
// how the parser wraps errors cannot silently disconnect the fix.
func TestInBandBrokenPromiseSurvivesTheCommitParser(t *testing.T) {
	t.Parallel()

	body := (&types.ErrorOrError{ErrorCode: ErrBrokenPromise}).MarshalFDB()
	tx := newTestTx()
	err := tx.parseCommitReply(body)
	if err == nil {
		t.Fatal("parseCommitReply returned nil for an in-band error reply; the " +
			"rest of this test would be vacuous")
	}
	// Vacuity: the parser must still be handing back a 1100, or the assertion
	// below would pass for the wrong reason.
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrBrokenPromise {
		t.Fatalf("parser produced %v, want an FDBError 1100", err)
	}
	if got := dispositionForReplyError(err, true); got != replyConvertToMaybeDelivered {
		t.Errorf("a parsed in-band 1100 classified as %v; the commit path would "+
			"return a terminal broken_promise to the application, which is the "+
			"divergence this exists to close", got)
	}
}
