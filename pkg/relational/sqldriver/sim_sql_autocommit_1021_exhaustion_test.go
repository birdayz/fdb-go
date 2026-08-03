package sqldriver

// RFC-198 criterion 13's ONE sanctioned expectation edit, and the measurement
// of how far it reaches.
//
// Criterion 13 holds auto-commit untouched with a single stated exception: an
// auto-commit caller whose retry loop EXHAUSTS on 1021 now sees 40003 where it
// saw a raw FDB error. Everything else about auto-commit is unchanged, which
// is why the exception gets its own assertion instead of being absorbed into a
// suite-wide "still green".
//
// MEASURED — how reachable the exception actually is, which the RFC did not
// establish:
//
//   - 1021 is onError-retryable in every backend's retry loop
//     (fdb.IsOnErrorRetryable; pure-Go client commitpath.go:239, SimFDB
//     simfdb.go:194), so an auto-commit statement that hits it does NOT
//     normally surface anything at all — it retries and succeeds. Pinned by
//     the second test below, which is what makes "exception" the right word.
//   - It escapes only when the retry budget runs out first. SimFDB bounds its
//     loop at maxRetries (simfdb.go:14), so the exhaustion is reachable there
//     by injecting past it — that is this test. On the pure-Go client the
//     equivalent bound is per-transaction and OPT-IN
//     (SetTransactionRetryLimit / SetTransactionTimeout; database.go
//     applyTxDefaults), and NOTHING in pkg/recordlayer or pkg/relational sets
//     either. So on a default production configuration the retry loop is
//     bounded by the caller's context, and a context that expires surfaces as
//     the context error, not as 1021.
//
// The consequence worth stating: the sanctioned edit is real but narrow. It is
// reachable by a caller who arms a retry limit and by any backend with a fixed
// bound; it is not something a default-configured auto-commit workload will
// meet. That is a claim about reachability, so it is tested rather than
// asserted in prose.

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/simfdb"
)

// simMaxRetries mirrors pkg/simfdb's unexported maxRetries. If the sim ever
// raises its bound, the injection count below stops exhausting the loop and
// the test fails loudly on the "want 40003, got success" arm rather than
// quietly measuring nothing.
const simMaxRetries = 100

// TestSQLFault_AutoCommit1021ExhaustionSurfacesAs40003 is criterion 13's
// sanctioned exception: an auto-commit statement whose retry loop exhausts on
// commit_unknown_result surfaces SQLSTATE 40003 — not a raw FDB error, and not
// 40001, because 40001 promises the transaction did NOT commit and 1021
// promises nothing at all.
//
// The DISCARDED branch is injected throughout: every attempt leaves nothing
// durable, so the row's absence at the end is a fact about this schedule, not
// a coin flip. (The applied branch would make the retry a double-apply, which
// is a different criterion — 10's — and is pinned there.)
//
// Mutation direction (verified at introduction): drop the 1021 lane from
// translateFDBCode and the error escapes as a raw *fdb.Error, reddening the
// *api.Error assert.
func TestSQLFault_AutoCommit1021ExhaustionSurfacesAs40003(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 13,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")

	// One fault per attempt, one more than the loop will take.
	faults := make([]int, simMaxRetries+1)
	for i := range faults {
		faults[i] = simfdb.CommitUnknownDiscarded
	}
	sim.InjectSequence(faults...)

	_, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 100)")
	if err == nil {
		t.Fatalf("the INSERT succeeded after %d injected commit_unknown_result faults: "+
			"the retry loop is no longer bounded at %d attempts, so this test is not "+
			"exercising retry exhaustion at all", len(faults), simMaxRetries)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("auto-commit INSERT failed with %v (%T), want *api.Error 40003: the raw FDB "+
			"error escaped the driver — criterion 13's sanctioned lane (1021 → 40003) is "+
			"not wired on the auto-commit path", err, err)
	}
	if apiErr.Code != api.ErrCodeStatementCompletionUnknown {
		t.Fatalf("auto-commit INSERT: SQLSTATE %s, want %s (40003). 40001 would be a LIE here — "+
			"it promises the transaction did not commit, and commit_unknown_result promises "+
			"nothing: %v", apiErr.Code, api.ErrCodeStatementCompletionUnknown, err)
	}

	// The discarded branch wrote nothing, on every attempt.
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t WHERE id = 1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("the row exists after %d DISCARDED commit_unknown_result faults: nothing was "+
			"supposed to be durable on this branch, so either the injection resolved the "+
			"other way or a retry applied after the error was reported", len(faults))
	}
}

// TestSQLFault_AutoCommit1021BelowTheLimitIsInvisible is the negative result
// that makes the test above an EXCEPTION rather than a rule: a 1021 the retry
// loop can absorb surfaces nothing at all. The statement succeeds, the row is
// there, and no auto-commit expectation changed.
//
// This is what pins criterion 13's scope. If a change ever makes 1021 escape
// the retry loop on the first occurrence — by dropping it from the
// onError-retryable set, or by arming a retry limit somewhere in the record
// layer or the SQL layer — every auto-commit caller starts seeing 40003 where
// it used to see success, and the "one sanctioned expectation edit" is no
// longer one.
func TestSQLFault_AutoCommit1021BelowTheLimitIsInvisible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, sim := openSimSchema(t, 17,
		"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))")

	sim.InjectOnce(simfdb.CommitUnknownDiscarded)
	if _, err := db.ExecContext(ctx, "INSERT INTO t (id, a) VALUES (1, 100)"); err != nil {
		t.Fatalf("auto-commit INSERT failed with %v after ONE injected commit_unknown_result: "+
			"the retry loop absorbed it before this change and must still — a 1021 that "+
			"escapes on its first occurrence turns criterion 13's single sanctioned "+
			"expectation edit into a change every auto-commit caller sees", err)
	}
	var a int64
	if err := db.QueryRowContext(ctx, "SELECT a FROM t WHERE id = 1").Scan(&a); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if a != 100 {
		t.Fatalf("a = %d, want 100: the retried attempt did not land the row", a)
	}
}
