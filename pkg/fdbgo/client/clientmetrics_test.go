package client

import (
	"context"
	"fmt"
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
	dbWarn.countRetryAndLog(context.Background(), ErrCommitUnknownResult, 1)
	dbWarn.countRetryAndLog(context.Background(), ErrNotCommitted, 1)
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
	dbDebug.countRetryAndLog(context.Background(), ErrNotCommitted, 1)
	dbDebug.countRetryAndLog(context.Background(), ErrCommitUnknownResult, 2)
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

// TestFDB_Metrics_DummyRetriesCounted: the commit_unknown_result barrier's
// retry loop must tick the per-code counters like C++, whose dummy routes
// errors through tr.onError (NativeAPI.actor.cpp:6341). A spoiler goroutine
// hammers the dummy's conflict key so its conflict-only commit keeps hitting
// not_committed; poll until the conflict counter advances (a missing count
// site never converges — red pre-fix; bounded, so no flake).
func TestFDB_Metrics_DummyRetriesCounted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// No local `defer db.Close()`: openTestDB registers Close via t.Cleanup,
	// and the spoiler-join cleanup below is registered AFTER it — cleanups
	// run LIFO, so the spoiler is joined BEFORE the handle closes on every
	// exit path, including t.Fatal (Torvalds nit: a dying spoiler Transact
	// must not race Close).
	db := openTestDB(t, ctx)

	key := []byte(t.Name() + "_key")
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v0"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Spoiler: continuously commit writes to key so the dummy's read+write
	// conflict range keeps conflicting.
	spoilCtx, spoilCancel := context.WithCancel(ctx)
	spoilDone := make(chan struct{})
	t.Cleanup(func() {
		spoilCancel()
		<-spoilDone
	})
	go func() {
		defer close(spoilDone)
		for spoilCtx.Err() == nil {
			_, _ = db.Transact(spoilCtx, func(tx *Transaction) (any, error) {
				tx.Set(key, []byte("spoil"))
				return nil, nil
			})
		}
	}()

	base := db.Metrics()
	deadline := time.Now().Add(30 * time.Second)
	for {
		// Drive the barrier directly, exactly as the 1021 path does: a txn
		// with key in both conflict sets. Each call commits a fresh
		// conflict-only dummy; under the spoiler it conflicts and retries.
		runCtx, runCancel := context.WithTimeout(ctx, 5*time.Second)
		dummy := db.CreateTransaction()
		dummy.Set(key, []byte("never-committed"))
		dummy.addReadConflictForKey(key)
		dummy.commitDummyTransaction(runCtx)
		runCancel()

		if db.Metrics().TransactionsNotCommitted > base.TransactionsNotCommitted {
			break // a dummy retry was counted
		}
		if time.Now().After(deadline) {
			t.Fatal("dummy barrier retries never advanced TransactionsNotCommitted (count site missing?)")
		}
	}
}

// TestFDB_Metrics_OversizedCommitCountsStarted: C++ counts CommitStarted
// BEFORE its size check (NativeAPI.actor.cpp:6808 vs ~:6835), so a
// persistently oversized commit is visible as Started-without-Completed.
// Torvalds impl-review condition. Each mutation stays under the per-value
// limit (100KB) so the DEFERRED 10MB size check is what fires (RFC-067
// eager-vs-deferred ordering).
func TestFDB_Metrics_OversizedCommitCountsStarted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	base := db.Metrics()
	tx := db.CreateTransaction()
	val := make([]byte, 90_000) // under VALUE_SIZE_LIMIT
	for i := 0; i < 150; i++ {  // ~13.5MB total > 10MB TRANSACTION_SIZE_LIMIT
		tx.Set([]byte(t.Name()+"_k"+string(rune('a'+i%26))+string(rune('a'+i/26))), val)
	}
	err := tx.Commit(ctx)
	if err == nil {
		t.Fatal("oversized commit succeeded, want transaction_too_large (2101)")
	}
	assertFDBErrorCode(t, err, 2101)

	s := db.Metrics()
	if d := s.TransactionsCommitStarted - base.TransactionsCommitStarted; d != 1 {
		t.Errorf("CommitStarted delta = %d, want 1 (counted before the size check)", d)
	}
	if d := s.TransactionsCommitCompleted - base.TransactionsCommitCompleted; d != 0 {
		t.Errorf("CommitCompleted delta = %d, want 0", d)
	}
}

// TestClientMetrics_ConnectionFailureVisible pins RFC-114 R2-CRITICAL #2: the
// single failure sink (handleConnError) increments the counter, emits a Warn
// carrying the address, AND still marks the endpoint failed (the pre-existing
// behavior must be preserved). Unit-level via the per-handle logger — never
// slog.SetDefault. Revert-proof: removing the counter or the Warn reddens this.
func TestClientMetrics_ConnectionFailureVisible(t *testing.T) {
	t.Parallel()
	h := &capturingHandler{level: slog.LevelWarn}
	db := &database{logger: slog.New(h), failMon: newFailureMonitor()}

	const addr = "10.0.0.7:4500"
	db.handleConnError(addr)

	if c := db.metrics.Snapshot().ClientConnectionFailures; c != 1 {
		t.Errorf("ClientConnectionFailures = %d, want 1", c)
	}
	if got := h.count(); got != 1 {
		t.Fatalf("expected 1 Warn record, got %d", got)
	}
	h.mu.Lock()
	rec := h.records[0]
	h.mu.Unlock()
	if rec.Level != slog.LevelWarn {
		t.Errorf("connection-failure event level = %v, want WARN", rec.Level)
	}
	var sawAddr bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "address" && a.Value.String() == addr {
			sawAddr = true
		}
		return true
	})
	if !sawAddr {
		t.Error("connection-failure event missing address attr")
	}
	if !db.failMon.isFailed(addr) {
		t.Error("handleConnError must still mark the endpoint failed (behavior preserved)")
	}

	// Storm hygiene: a repeat failure on the same (still-failed) endpoint ticks the
	// COUNTER (rate signal) but does NOT re-Warn (edge-triggered, no log melt).
	db.handleConnError(addr)
	if c := db.metrics.Snapshot().ClientConnectionFailures; c != 2 {
		t.Errorf("ClientConnectionFailures = %d after a repeat, want 2 (counter ticks every event)", c)
	}
	if got := h.count(); got != 1 {
		t.Errorf("Warn count = %d after a repeat failure, want 1 (edge-triggered, not per-event)", got)
	}
	// Recovery re-arms the edge: a failure after markAlive Warns again.
	db.failMon.markAlive(addr)
	db.handleConnError(addr)
	if got := h.count(); got != 2 {
		t.Errorf("Warn count = %d after recover→fail, want 2 (re-armed)", got)
	}
}

// TestFollowForward_CountsCoordinatorChange pins RFC-114: a followed coordinator
// forward increments CoordinatorChanges; a refused (degenerate self) forward does
// not. Mirrors the TestFollowForward_Adopts harness.
func TestFollowForward_CountsCoordinatorChange(t *testing.T) {
	t.Parallel()
	start := &ClusterFile{Description: "old", ID: "a", Coordinators: []string{"1.1.1.1:4500"}}
	db := newFollowTestDB(t, start, "") // memory-only record; no disk needed

	if c := db.metrics.Snapshot().CoordinatorChanges; c != 0 {
		t.Fatalf("CoordinatorChanges = %d before any forward, want 0", c)
	}
	// A self-forward is refused and must NOT count.
	if db.followForward(start, start.String()) {
		t.Fatal("self-forward should be refused")
	}
	if c := db.metrics.Snapshot().CoordinatorChanges; c != 0 {
		t.Errorf("CoordinatorChanges = %d after a refused self-forward, want 0", c)
	}
	// A distinct, valid forward is adopted and counts once.
	if !db.followForward(start, "new:b@2.2.2.2:4500") {
		t.Fatal("valid distinct forward refused")
	}
	if c := db.metrics.Snapshot().CoordinatorChanges; c != 1 {
		t.Errorf("CoordinatorChanges = %d after a followed forward, want 1", c)
	}
}

// TestFDB_Metrics_LatencyRecorded pins RFC-114 R2-CRITICAL #1: read/commit/GRV/
// total latency distributions are populated by real transactions. Counts are
// tied to the commit-completed delta (exact regardless of retries) and the read
// count, so removing any sample site zeroes its category and reddens the test.
func TestFDB_Metrics_LatencyRecorded(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	const n = 5
	base := db.Metrics()
	for i := 0; i < n; i++ {
		k := []byte(fmt.Sprintf("%s_k%d", t.Name(), i))
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			if _, err := tx.Get(ctx, k); err != nil { // one read → one read-latency sample
				return nil, err
			}
			tx.Set(k, []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("transact %d: %v", i, err)
		}
	}
	s := db.Metrics()

	// Each successful commit records one commit-latency AND one total-latency
	// sample; tie to the commit-completed delta so the invariant is exact whether
	// or not any attempt retried (a retried commit still completes exactly once).
	commits := s.TransactionsCommitCompleted - base.TransactionsCommitCompleted
	if commits != n {
		t.Fatalf("commit-completed delta = %d, want %d", commits, n)
	}
	if d := s.CommitLatency.Count - base.CommitLatency.Count; d != commits {
		t.Errorf("CommitLatency.Count delta = %d, want %d (== commits)", d, commits)
	}
	if d := s.TransactionLatency.Count - base.TransactionLatency.Count; d != commits {
		t.Errorf("TransactionLatency.Count delta = %d, want %d (== commits)", d, commits)
	}
	if d := s.ReadLatency.Count - base.ReadLatency.Count; d < n {
		t.Errorf("ReadLatency.Count delta = %d, want >= %d", d, n)
	}
	if d := s.GRVLatency.Count - base.GRVLatency.Count; d < 1 {
		t.Errorf("GRVLatency.Count delta = %d, want >= 1", d)
	}

	for name, st := range map[string]LatencyStats{
		"read": s.ReadLatency, "commit": s.CommitLatency, "grv": s.GRVLatency, "transaction": s.TransactionLatency,
	} {
		if st.Count == 0 {
			t.Errorf("%s latency: count 0 — sample site missing?", name)
			continue
		}
		if !(st.Max > 0 && st.Median > 0 && st.P99 > 0 && st.Mean > 0 && st.Min > 0) {
			t.Errorf("%s latency: non-positive stat (min=%g max=%g median=%g p99=%g mean=%g)", name, st.Min, st.Max, st.Median, st.P99, st.Mean)
		}
		// Monotone: Min ≤ median ≤ p90 ≤ p99 ≤ Max. Min and Max are EXACT (LatencyStats doc: "C++
		// DDSketchBase::min/max") while the quantiles are DDSketch estimates (±0.5% relative), so BOTH
		// exact-vs-quantile edges need a bucket-width of slack — they are mirror images: a DDSketch
		// median can sit up to a bucket-width BELOW the exact Min exactly as the p99 estimate can sit a
		// bucket-width ABOVE the exact Max, when the sample is tiny and near-degenerate (all latencies
		// clustered in ~one bucket). The median-vs-median edges (median≤p90≤p99) are sketch-vs-sketch and
		// stay strict (DDSketch quantiles are monotone in q).
		if !(st.Min <= st.Median*1.0201 && st.Median <= st.P90 && st.P90 <= st.P99 && st.P99 <= st.Max*1.0201) {
			t.Errorf("%s latency: not ordered (min=%g median=%g p90=%g p99=%g max=%g)", name, st.Min, st.Median, st.P90, st.P99, st.Max)
		}
		if st.Sum <= 0 {
			t.Errorf("%s latency: sum=%g, want > 0", name, st.Sum)
		}
	}
}

// TestFDB_Metrics_PipelinedReadLatencySampled pins RFC-114's pipelined read-latency
// sample (the Resolve happy path) INDEPENDENTLY of the sync getValue sample: it
// drives reads only through GetPipelined→Resolve, so deleting the pipelined observe
// site zeroes the delta and reddens this. (Torvalds revert-proof catch — the main
// latency test exercises only the sync path.)
func TestFDB_Metrics_PipelinedReadLatencySampled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	const n = 4
	keys := make([][]byte, n)
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := range keys {
			keys[i] = []byte(fmt.Sprintf("%s_k%d", t.Name(), i))
			tx.Set(keys[i], []byte("v"))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	base := db.Metrics()
	// A fresh transaction so the seeded keys are NOT in this tx's RYW writes — each
	// GetPipelined sends a real request and Resolve takes the happy reply path.
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, k := range keys {
			_, p, err := tx.GetPipelined(ctx, k)
			if err != nil {
				return nil, err
			}
			if p == nil {
				return nil, fmt.Errorf("expected a pipelined read for committed key %q, got cache hit", k)
			}
			if _, err := p.Resolve(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("pipelined reads: %v", err)
	}
	s := db.Metrics()
	if d := s.ReadLatency.Count - base.ReadLatency.Count; d < n {
		t.Errorf("ReadLatency.Count delta = %d, want >= %d (pipelined Resolve happy path unmeasured?)", d, n)
	}
	if s.ReadLatency.Max <= 0 {
		t.Errorf("ReadLatency.Max = %g, want > 0", s.ReadLatency.Max)
	}
}

// TestFDB_Metrics_TransactionLatencyResetsOnReuse pins RFC-114's metricStart reuse
// boundary (codex catch): a Transaction reused after a successful commit measures the
// NEXT transaction fresh, excluding the idle gap before it begins. Revert-proof: drop
// the postCommitReset clear (or the first-GRV re-stamp) and commit2 folds in the sleep.
func TestFDB_Metrics_TransactionLatencyResetsOnReuse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	const idle = 400 * time.Millisecond
	tx := db.CreateTransaction()
	tx.Set([]byte(t.Name()+"_a"), []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit1: %v", err)
	}
	base := db.Metrics()
	time.Sleep(idle) // idle gap that must NOT be charged to the reused transaction

	// Measure the reused transaction's own wall time. The total-latency sample is a
	// sub-interval of it (first GRV → commit success), so with the reuse boundary
	// working the sample is ≤ wall; if broken it folds in the idle sleep and exceeds
	// wall by ≈ idle. Comparing the sample to the MEASURED wall (not a fixed threshold)
	// is robust to testcontainer jitter — a slow commit grows both sides together.
	t0 := time.Now()
	tx.Set([]byte(t.Name()+"_b"), []byte("v")) // reuse the same (auto-reset) handle
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit2 (reuse): %v", err)
	}
	wall := time.Since(t0)
	s := db.Metrics()

	if d := s.TransactionLatency.Count - base.TransactionLatency.Count; d != 1 {
		t.Fatalf("TransactionLatency.Count delta = %d, want 1 (the reused commit)", d)
	}
	assertReusedCommitLatency(t, s, base, wall, idle)
}

// assertReusedCommitLatency pins the post-base commit's total-latency sample (isolated
// via the Sum delta, since the sketch's Max is cumulative) ≤ its measured wall time +
// a small slack. The sample is genuinely a sub-interval of wall, so this holds for any
// commit speed (jitter-robust); a broken reuse boundary folds in the idle gap, pushing
// the sample to ≈ idle + wall ≫ wall + slack. Revert-proof against idle ≫ slack.
func assertReusedCommitLatency(t *testing.T, s, base ClientMetricsSnapshot, wall, idle time.Duration) {
	t.Helper()
	const slack = 100 * time.Millisecond
	if slack >= idle {
		t.Fatalf("test bug: slack %v must be < idle %v for a valid discriminator", slack, idle)
	}
	sample := s.TransactionLatency.Sum - base.TransactionLatency.Sum // the one new commit's latency
	if sample > (wall + slack).Seconds() {
		t.Errorf("reused-commit total latency = %gs, want ≤ wall %gs + slack %gs (idle gap folded in?)",
			sample, wall.Seconds(), slack.Seconds())
	}
}

// TestFDB_Metrics_TransactionLatencyResetsOnUserReset pins the user-Reset() metric
// boundary (codex + Torvalds catch): a handle that reads then Reset()s without
// committing must measure the NEXT transaction fresh. The clear lives in Reset(), NOT
// the OnError-shared reset(). Revert-proof: drop the Reset() metricStart clear and the
// post-Reset commit folds in the idle gap below.
func TestFDB_Metrics_TransactionLatencyResetsOnUserReset(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	const idle = 400 * time.Millisecond
	tx := db.CreateTransaction()
	if _, err := tx.Get(ctx, []byte(t.Name()+"_probe")); err != nil { // first GRV stamps metricStart
		t.Fatalf("probe read: %v", err)
	}
	base := db.Metrics()
	time.Sleep(idle)
	t0 := time.Now()
	tx.Reset() // new logical transaction — must re-anchor metricStart (cleared here)
	tx.Set([]byte(t.Name()+"_k"), []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit after reset: %v", err)
	}
	wall := time.Since(t0)
	s := db.Metrics()
	if d := s.TransactionLatency.Count - base.TransactionLatency.Count; d != 1 {
		t.Fatalf("TransactionLatency.Count delta = %d, want 1", d)
	}
	assertReusedCommitLatency(t, s, base, wall, idle)
}
