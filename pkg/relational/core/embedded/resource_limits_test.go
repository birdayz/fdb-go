package embedded

// RFC-106a per-statement resource governance — white-box unit tests that
// reach the unexported wiring (executeProps, translateExecError) directly.
// The FDB end-to-end coverage lives in
// pkg/relational/sqldriver/resource_limits_fdb_test.go.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/session"
)

// TestRFC106a_DefaultSafety is the default-safety gate: a
// connection with NO options/config yields per-page ExecuteProperties with
// ScannedRecordsLimit==0 (the recordlayer "no limit" value),
// ScannedBytesLimit==0, and FailOnScanLimitReached==false. The only
// non-default is the per-page time floor (txPageTimeLimit), which always
// applies and is NOT a user-set limit. This is a DIRECT assertion that the
// wiring is inert with no options set.
func TestRFC106a_DefaultSafety(t *testing.T) {
	t.Parallel()
	pr := &paginatingRows{conn: &EmbeddedConnection{}}
	props := pr.executeProps()
	if props.ScannedRecordsLimit != 0 {
		t.Fatalf("default ScannedRecordsLimit = %d, want 0 (no limit)", props.ScannedRecordsLimit)
	}
	if props.ScannedBytesLimit != 0 {
		t.Fatalf("default ScannedBytesLimit = %d, want 0 (no limit)", props.ScannedBytesLimit)
	}
	if props.FailOnScanLimitReached {
		t.Fatalf("default FailOnScanLimitReached = true, want false")
	}
	if props.TimeLimit != txPageTimeLimit {
		t.Fatalf("default TimeLimit = %v, want tx page floor %v", props.TimeLimit, txPageTimeLimit)
	}
}

// TestRFC106a_ExecutePropsWiring proves the scan-limit options actually
// flow through to ExecuteProperties when set (per-page, Java semantics).
func TestRFC106a_ExecutePropsWiring(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	conn.SetOptions(api.NewOptionsBuilder().
		Set(api.OptExecutionScannedRowsLimit, 7).
		Set(api.OptExecutionScannedBytesLimit, int64(4096)).
		Set(api.OptExecutionTimeLimit, int64(50)). // 50ms — below the tx floor
		Build())
	conn.SetFailOnScanLimitReached(true)
	props := (&paginatingRows{conn: conn}).executeProps()

	if props.ScannedRecordsLimit != 7 {
		t.Fatalf("ScannedRecordsLimit = %d, want 7", props.ScannedRecordsLimit)
	}
	if props.ScannedBytesLimit != 4096 {
		t.Fatalf("ScannedBytesLimit = %d, want 4096", props.ScannedBytesLimit)
	}
	if props.TimeLimit != 50*time.Millisecond {
		t.Fatalf("TimeLimit = %v, want 50ms (user limit < tx floor)", props.TimeLimit)
	}
	if !props.FailOnScanLimitReached {
		t.Fatalf("FailOnScanLimitReached = false, want true")
	}

	// A user time limit ABOVE the tx floor must NOT raise the per-page
	// limit beyond the floor (the FDB 5s wall must hold).
	conn2 := &EmbeddedConnection{}
	conn2.SetOptions(api.NewOptionsBuilder().
		Set(api.OptExecutionTimeLimit, int64(60_000)). // 60s
		Build())
	if got := (&paginatingRows{conn: conn2}).executeProps().TimeLimit; got != txPageTimeLimit {
		t.Fatalf("TimeLimit with 60s user limit = %v, want clamped to tx floor %v", got, txPageTimeLimit)
	}

	// MaxInt32 sentinel for scanned-rows must remain "no limit" (0).
	conn3 := &EmbeddedConnection{}
	conn3.SetOptions(api.NewOptionsBuilder().
		Set(api.OptExecutionScannedRowsLimit, math.MaxInt32).Build())
	if got := (&paginatingRows{conn: conn3}).executeProps().ScannedRecordsLimit; got != 0 {
		t.Fatalf("ScannedRecordsLimit with MaxInt32 sentinel = %d, want 0 (no limit)", got)
	}
}

// TestRFC106a_SQLSTATEMap proves the translateExecError arms added for the
// eager-buffer caps + the scan-limit fail surface 54F01 — and revert-proves the
// arms are load-bearing (an unmapped error passes through untranslated).
func TestRFC106a_SQLSTATEMap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{"materialization", &executor.MaterializationLimitExceededError{Limit: 100_000, Context: "buffered union"}},
		{"sortBuffer", &executor.SortBufferExceededError{Rows: 6_000_000, Limit: 5_000_000}},
		{"scanLimitFail", &recordlayer.ScanLimitReachedError{Reason: recordlayer.ScanLimitReached}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := translateExecError(tc.err)
			var apiErr *api.Error
			if !errors.As(got, &apiErr) {
				t.Fatalf("%s: want *api.Error, got %T (%v)", tc.name, got, got)
			}
			if apiErr.Code != api.ErrCodeExecutionLimitReached {
				t.Fatalf("%s: code = %q, want %q", tc.name, apiErr.Code, api.ErrCodeExecutionLimitReached)
			}
		})
	}

	// RFC-180 WS-A: a resume attempt on a cursor shape with no continuation
	// support declines typed — 0A000, not 54F01 and never a generic error.
	{
		got := translateExecError(&executor.UnsupportedContinuationError{Shape: "buffered union"})
		var apiErr *api.Error
		if !errors.As(got, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
			t.Fatalf("UnsupportedContinuationError → %v, want *api.Error 0A000", got)
		}
	}

	// A bare deadline is NO LONGER mapped by translateExecError on its own — the
	// statement-timeout→54F01 mapping is gated on the internal-timeout context cause
	// (translateExecErrorCtx) so a caller deadline can survive (see below).
	if out := translateExecError(context.DeadlineExceeded); !errors.Is(out, context.DeadlineExceeded) {
		t.Fatalf("translateExecError(deadline) = %v, want the bare deadline to pass through", out)
	}

	// Revert-prove: an UNMAPPED error passes through untranslated, proving
	// the arms (not a catch-all) did the work.
	plain := errors.New("some other internal failure")
	if out := translateExecError(plain); out != plain {
		t.Fatalf("unmapped error rewritten to %v, want pass-through", out)
	}
}

// TestRFC106a_StatementTimeoutVsCallerDeadline pins the PR #291 fix: the
// INTERNAL RFC-106a statement-timeout deadline maps to 54F01 "statement timeout", while
// a CALLER's own QueryContext/ExecContext deadline propagates as context.DeadlineExceeded
// (never rewritten to 54F01) so errors.Is(err, context.DeadlineExceeded) keeps working
// and a client cancellation isn't misreported as a Go-local statement timeout.
func TestRFC106a_StatementTimeoutVsCallerDeadline(t *testing.T) {
	t.Parallel()

	// Internal statement timeout: cause == errStatementTimeout → 54F01.
	stmtCtx, stmtCancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), errStatementTimeout)
	defer stmtCancel()
	for _, in := range []error{context.DeadlineExceeded, fmt.Errorf("cursor gave up: %w", context.DeadlineExceeded)} {
		got := translateExecErrorCtx(stmtCtx, in)
		var apiErr *api.Error
		if !errors.As(got, &apiErr) || apiErr.Code != api.ErrCodeExecutionLimitReached {
			t.Fatalf("statement-timeout deadline %v → %v, want 54F01 ExecutionLimitReached", in, got)
		}
		if apiErr.Message != "statement timeout" {
			t.Fatalf("statement-timeout message = %q, want 'statement timeout'", apiErr.Message)
		}
	}

	// Caller's own deadline (no statement-timeout cause) must PROPAGATE unchanged.
	callerCtx, callerCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer callerCancel()
	out := translateExecErrorCtx(callerCtx, context.DeadlineExceeded)
	if !errors.Is(out, context.DeadlineExceeded) {
		t.Fatalf("caller deadline = %v, want propagated context.DeadlineExceeded (errors.Is must hold)", out)
	}
	var apiErr *api.Error
	if errors.As(out, &apiErr) {
		t.Fatalf("caller deadline mapped to api.Error %q, want the raw context.DeadlineExceeded", apiErr.Code)
	}

	// No statement timeout set at all (Execute didn't wrap ctx): a deadline is the
	// caller's by construction → propagate.
	if out := translateExecErrorCtx(context.Background(), context.DeadlineExceeded); !errors.Is(out, context.DeadlineExceeded) {
		t.Fatalf("unwrapped ctx deadline = %v, want propagated context.DeadlineExceeded", out)
	}
}

// TestPageBudgetAnchorsOnTheDatabaseEnvClock pins that the per-page scan/time budget is anchored
// on the DATABASE's DST clock, not the wall clock.
//
// This path always arms a time limit — txPageTimeLimit, clamped so the FDB 5s wall is never
// crossed — and that limit decides where a page ends and what continuation the caller gets. So a
// wall-clock anchor makes a simulated run page differently depending on how fast the host was:
// nondeterminism in the layer RFC-199 exists to make reproducible, on every single statement.
//
// The seam gate cannot see this one. There is no raw time.Now here to flag; the whole mistake
// would be calling the env-less constructor, which looks perfectly ordinary. It needs its own
// assertion.
func TestPageBudgetAnchorsOnTheDatabaseEnvClock(t *testing.T) {
	t.Parallel()

	clock := dst.NewSimClock(dst.Epoch)
	env := &dst.Env{Clock: clock}
	conn := &EmbeddedConnection{sess: &session.Session{
		DB: (&recordlayer.FDBDatabase{}).SetEnv(env),
	}}
	state := (&paginatingRows{conn: conn}).executeProps().ScanState
	if state == nil {
		t.Fatal("executeProps produced no ScanState; the page budget has nothing to charge against")
	}
	if !state.StartTime().Equal(dst.Epoch) {
		t.Fatalf("page budget anchored at %v, want the database's sim Epoch %v — the anchor "+
			"must come off the env clock or a simulated run pages by wall-clock luck",
			state.StartTime(), dst.Epoch)
	}
	clock.Advance(2 * time.Second)
	if got := state.Elapsed(); got != 2*time.Second {
		t.Fatalf("elapsed after advancing the sim clock 2s = %v, want 2s", got)
	}

	// A connection with no session/database is production: wall clock, unchanged. The unit
	// tests above construct exactly that shape, so this arm also pins that they keep working.
	prod := (&paginatingRows{conn: &EmbeddedConnection{}}).executeProps().ScanState
	if prod == nil {
		t.Fatal("env-less connection produced no ScanState")
	}
	if prod.StartTime().Equal(dst.Epoch) {
		t.Fatal("env-less connection anchored at the sim Epoch — a nil env must be the wall clock")
	}
}
