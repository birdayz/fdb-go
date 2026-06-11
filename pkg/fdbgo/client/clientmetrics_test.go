package client

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestClientMetrics_RetryMapping pins the per-code counter mapping against
// the C++ onError arms (NativeAPI.actor.cpp:7749-7772). Pure unit test.
func TestClientMetrics_RetryMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		get  func(s ClientMetricsSnapshot) int64
		name string
	}{
		{ErrNotCommitted, func(s ClientMetricsSnapshot) int64 { return s.TransactionsNotCommitted }, "not_committed"},
		{ErrCommitUnknownResult, func(s ClientMetricsSnapshot) int64 { return s.TransactionsMaybeCommitted }, "maybe_committed"},
		{ErrProxyMemoryLimitExceeded, func(s ClientMetricsSnapshot) int64 { return s.TransactionsResourceConstrained }, "resource_constrained_1042"},
		{ErrGrvProxyMemoryLimit, func(s ClientMetricsSnapshot) int64 { return s.TransactionsResourceConstrained }, "resource_constrained_1078"},
		{ErrProcessBehind, func(s ClientMetricsSnapshot) int64 { return s.TransactionsProcessBehind }, "process_behind"},
		{ErrBatchTransactionThrottled, func(s ClientMetricsSnapshot) int64 { return s.TransactionsThrottled }, "throttled_1051"},
		{ErrTagThrottled, func(s ClientMetricsSnapshot) int64 { return s.TransactionsThrottled }, "throttled_1213"},
		{ErrProxyTagThrottled, func(s ClientMetricsSnapshot) int64 { return s.TransactionsThrottled }, "throttled_1223"},
		{ErrTransactionTooOld, func(s ClientMetricsSnapshot) int64 { return s.TransactionsTooOld }, "too_old"},
		{ErrFutureVersion, func(s ClientMetricsSnapshot) int64 { return s.TransactionsFutureVersions }, "future_version"},
	}
	for _, tc := range cases {
		var m ClientMetrics
		m.countRetry(tc.code)
		s := m.Snapshot()
		if got := tc.get(s); got != 1 {
			t.Errorf("%s: counter = %d, want 1", tc.name, got)
		}
		if s.TransactionRetries != 1 {
			t.Errorf("%s: TransactionRetries = %d, want 1", tc.name, s.TransactionRetries)
		}
	}

	// Codes C++ retries WITHOUT a per-code counter (:7743-7747) still count
	// toward the aggregate only.
	var m ClientMetrics
	m.countRetry(ErrDatabaseLocked)
	m.countRetry(ErrBlobGranuleRequestFailed)
	s := m.Snapshot()
	if s.TransactionRetries != 2 {
		t.Errorf("TransactionRetries = %d, want 2", s.TransactionRetries)
	}
	if s.TransactionsNotCommitted+s.TransactionsMaybeCommitted+s.TransactionsResourceConstrained+
		s.TransactionsProcessBehind+s.TransactionsThrottled+s.TransactionsTooOld+s.TransactionsFutureVersions != 0 {
		t.Error("counterless-retryable codes must not touch per-code counters")
	}
}

// capturingHandler is a minimal concurrency-safe slog.Handler for tests.
type capturingHandler struct {
	mu      sync.Mutex
	level   slog.Level
	records []slog.Record
}

func (h *capturingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// TestClientMetrics_SlogEvents pins the event levels: 1021 at Warn, 1020 at
// Debug, and the Enabled guard (Debug events never reach an Info handler).
// Unit-level via the per-handle hook — never slog.SetDefault (process-global
// mutation would race parallel tests).
func TestClientMetrics_SlogEvents(t *testing.T) {
	t.Parallel()

	// Warn handler: sees 1021, not 1020.
	warnH := &capturingHandler{level: slog.LevelWarn}
	dbWarn := &database{logger: slog.New(warnH)}
	dbWarn.countRetryAndLog(ErrCommitUnknownResult, 1)
	dbWarn.countRetryAndLog(ErrNotCommitted, 1)
	if got := warnH.count(); got != 1 {
		t.Fatalf("warn-level handler captured %d records, want 1 (only the 1021 Warn)", got)
	}
	warnH.mu.Lock()
	rec := warnH.records[0]
	warnH.mu.Unlock()
	if rec.Level != slog.LevelWarn {
		t.Errorf("1021 event level = %v, want WARN", rec.Level)
	}
	var sawCode bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "fdb_error_code" && a.Value.Int64() == int64(ErrCommitUnknownResult) {
			sawCode = true
		}
		return true
	})
	if !sawCode {
		t.Error("1021 event missing fdb_error_code attr")
	}

	// Debug handler: sees both.
	debugH := &capturingHandler{level: slog.LevelDebug}
	dbDebug := &database{logger: slog.New(debugH)}
	dbDebug.countRetryAndLog(ErrNotCommitted, 1)
	dbDebug.countRetryAndLog(ErrCommitUnknownResult, 2)
	if got := debugH.count(); got != 2 {
		t.Fatalf("debug-level handler captured %d records, want 2", got)
	}

	// Counters advance regardless of log level.
	if s := dbWarn.metrics.Snapshot(); s.TransactionRetries != 2 || s.TransactionsNotCommitted != 1 || s.TransactionsMaybeCommitted != 1 {
		t.Errorf("counters with warn logger = %+v, want retries=2 notCommitted=1 maybeCommitted=1", s)
	}
}

// TestFDB_Metrics_CleanCommit: one read-write Transact on a fresh handle
// advances commit started/completed and the (real-fetch) GRV counter, and no
// error counters. Fresh handle ⇒ fresh counters ⇒ exact deltas are safe under
// t.Parallel.
func TestFDB_Metrics_CleanCommit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	base := db.Metrics()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(t.Name()+"_key"), []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("Transact: %v", err)
	}
	s := db.Metrics()

	if d := s.TransactionsCommitStarted - base.TransactionsCommitStarted; d != 1 {
		t.Errorf("CommitStarted delta = %d, want 1", d)
	}
	if d := s.TransactionsCommitCompleted - base.TransactionsCommitCompleted; d != 1 {
		t.Errorf("CommitCompleted delta = %d, want 1", d)
	}
	// The commit's GRV on a fresh handle is a real fetch (cache empty), so the
	// completed counter advances. ≥1 (not ==1): another in-flight bootstrap
	// fetch may batch.
	if d := s.TransactionReadVersionsCompleted - base.TransactionReadVersionsCompleted; d < 1 {
		t.Errorf("ReadVersionsCompleted delta = %d, want >= 1", d)
	}
	if s.TransactionRetries != base.TransactionRetries {
		t.Errorf("TransactionRetries advanced on a clean commit: %d -> %d", base.TransactionRetries, s.TransactionRetries)
	}
	if s.TransactionsNotCommitted != base.TransactionsNotCommitted {
		t.Errorf("NotCommitted advanced on a clean commit")
	}
}

// TestFDB_Metrics_ReadOnlyCommitNotCounted: the read-only fast path must not
// count commits — C++'s empty-commit fast path returns before the counter
// (NativeAPI.actor.cpp:6800-6808). Torvalds RFC-097 condition.
func TestFDB_Metrics_ReadOnlyCommitNotCounted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	base := db.Metrics()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		_, err := tx.Get(ctx, []byte(t.Name()+"_absent"))
		return nil, err
	}); err != nil {
		t.Fatalf("read-only Transact: %v", err)
	}
	s := db.Metrics()
	if s.TransactionsCommitStarted != base.TransactionsCommitStarted ||
		s.TransactionsCommitCompleted != base.TransactionsCommitCompleted {
		t.Errorf("read-only Transact moved commit counters: started %d->%d completed %d->%d",
			base.TransactionsCommitStarted, s.TransactionsCommitStarted,
			base.TransactionsCommitCompleted, s.TransactionsCommitCompleted)
	}
	if d := s.TransactionReadVersionsCompleted - base.TransactionReadVersionsCompleted; d < 1 {
		t.Errorf("read-only Transact should still complete a GRV, delta = %d", d)
	}
}

// TestFDB_Metrics_ConflictCounts: a forced commit conflict advances the
// conflict counter and the retry aggregate, and the Transact loop's eventual
// success completes a commit.
func TestFDB_Metrics_ConflictCounts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	base := db.Metrics()
	first := true
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		// Read to establish a read conflict range.
		if _, err := tx.Get(ctx, key); err != nil {
			return nil, err
		}
		if first {
			first = false
			// Conflicting write from another transaction between our read
			// and our commit — forces not_committed (1020) on this attempt.
			if _, err := db.Transact(ctx, func(spoiler *Transaction) (any, error) {
				spoiler.Set(key, []byte("spoiler"))
				return nil, nil
			}); err != nil {
				return nil, err
			}
		}
		tx.Set(key, []byte("v1"))
		return nil, nil
	}); err != nil {
		t.Fatalf("conflicting Transact: %v", err)
	}
	s := db.Metrics()

	if d := s.TransactionsNotCommitted - base.TransactionsNotCommitted; d < 1 {
		t.Errorf("NotCommitted delta = %d, want >= 1 (the forced conflict)", d)
	}
	if d := s.TransactionRetries - base.TransactionRetries; d < 1 {
		t.Errorf("TransactionRetries delta = %d, want >= 1", d)
	}
	// First attempt + spoiler + successful retry: at least 3 started, and the
	// spoiler + retry completed.
	if d := s.TransactionsCommitStarted - base.TransactionsCommitStarted; d < 3 {
		t.Errorf("CommitStarted delta = %d, want >= 3", d)
	}
	if d := s.TransactionsCommitCompleted - base.TransactionsCommitCompleted; d < 2 {
		t.Errorf("CommitCompleted delta = %d, want >= 2", d)
	}
}

// TestFDB_Metrics_DummyCommitCounted: commitDummyTransaction's barrier commit
// goes through the same Commit path and is counted — matching C++, whose
// dummy runs commitMutations/tryCommit (NativeAPI.actor.cpp:6306-6344).
// Pinned explicitly so the choice is deliberate (RFC-097).
func TestFDB_Metrics_DummyCommitCounted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte(t.Name() + "_key")
	base := db.Metrics()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("uncommitted"))
		tx.addReadConflictForKey(key)
		tx.commitDummyTransaction(ctx) // the barrier commits a real (conflict-only) txn
		return nil, errAbortRegression // abort the outer txn
	}); err != errAbortRegression {
		t.Fatalf("expected abort sentinel, got %v", err)
	}
	s := db.Metrics()
	if d := s.TransactionsCommitStarted - base.TransactionsCommitStarted; d < 1 {
		t.Errorf("CommitStarted delta = %d, want >= 1 (the dummy barrier commit)", d)
	}
	if d := s.TransactionsCommitCompleted - base.TransactionsCommitCompleted; d < 1 {
		t.Errorf("CommitCompleted delta = %d, want >= 1 (the dummy barrier commit)", d)
	}
}
