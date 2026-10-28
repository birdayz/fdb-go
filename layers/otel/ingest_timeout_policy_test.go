package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

func TestSourceTimeoutClassification(t *testing.T) {
	t.Parallel()
	timeout := fdb.Error{Code: 1031}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"value", timeout, true},
		{"pointer", &timeout, true},
		{"wrapped value", fmt.Errorf("source: %w", timeout), true},
		{"operation wrapper", wrap("operation", "source", timeout), true},
		{"timed out", fdb.Error{Code: 1004}, false},
		{"commit unknown", fdb.Error{Code: 1021}, false},
		{"canceled transaction", fdb.Error{Code: 1025}, false},
		{"canceled context", context.Canceled, false},
		{"text only", errors.New("transaction_timed_out (1031)"), false},
		{"budget cause", wrap("budget", "source", timeout), false},
		{"nested budget cause", fmt.Errorf("source: %w", wrap("budget", "guard", &timeout)), false},
		{"admission cause", wrap("admission", "source", timeout), false},
		{"config cause", wrap("config", "source", timeout), false},
		{"ownership cause", wrap("ownership", "source", timeout), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSourceTimeout(tc.err); got != tc.want {
				t.Fatalf("isSourceTimeout(%v)=%t want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestSourceTimeoutRecorderCountersAndDisabledSafety(t *testing.T) {
	t.Parallel()
	var disabled *ingestRecorder
	disabled.timeout()
	disabled.freshRetry()
	if got := disabled.snapshot(0, 0); got != nil {
		t.Fatalf("disabled timeout instrumentation produced a receipt: %+v", got)
	}
	enabled := newIngestRecorder(true)
	enabled.timeout()
	enabled.timeout()
	enabled.freshRetry()
	got := enabled.snapshot(0, 0)
	if got == nil || got.Scope != "source_batches_only" || got.SourceTimeouts != 2 || got.SourceFreshRetries != 1 || got.SourceRuns != 0 || got.SourceAttempts != 0 || got.SourceReplays != 0 || got.CommittedBatches != 0 || got.CommittedRows != 0 || got.ApproximateMutationBytes != 0 || len(got.PhaseNanos) != 0 || len(got.Timer) != 0 {
		t.Fatalf("timeout counters fabricated unrelated source work: %+v", got)
	}
	enabled.timeout()
	enabled.freshRetry()
	got = enabled.snapshot(0, 0)
	if got.SourceTimeouts != 3 || got.SourceFreshRetries != 2 {
		t.Fatalf("timeout counters did not accumulate: %+v", got)
	}
}

func TestSourceRunBackoffBounds(t *testing.T) {
	t.Parallel()
	if maxSourceRuns != 8 {
		t.Fatalf("fresh source Run cap=%d want 8", maxSourceRuns)
	}
	for _, tc := range []struct {
		completed int
		want      time.Duration
	}{
		{-int(^uint(0) >> 1), 25 * time.Millisecond},
		{-1, 25 * time.Millisecond},
		{0, 25 * time.Millisecond},
		{1, 25 * time.Millisecond},
		{2, 50 * time.Millisecond},
		{3, 100 * time.Millisecond},
		{4, 200 * time.Millisecond},
		{5, 250 * time.Millisecond},
		{6, 250 * time.Millisecond},
		{7, 250 * time.Millisecond},
		{8, 250 * time.Millisecond},
		{63, 250 * time.Millisecond},
		{int(^uint(0) >> 1), 250 * time.Millisecond},
	} {
		if got := sourceRunBackoff(tc.completed); got != tc.want {
			t.Fatalf("completed Runs=%d backoff=%s want %s", tc.completed, got, tc.want)
		}
	}
}

type sourceBackoffEntryContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *sourceBackoffEntryContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestWaitSourceBackoffCancellation(t *testing.T) {
	t.Parallel()
	if err := waitSourceBackoff(t.Context(), 0); err != nil {
		t.Fatalf("live zero-duration backoff: %v", err)
	}
	guard, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(guard)
	defer cancel()
	entered := &sourceBackoffEntryContext{Context: ctx, entered: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { finished <- waitSourceBackoff(entered, time.Hour) }()
	select {
	case <-entered.entered:
		if ctx.Err() != nil {
			t.Fatal("backoff did not enter under a live context")
		}
		cancel()
	case <-guard.Done():
		t.Fatal("backoff never entered its cancelable wait")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("backoff cancellation: %v", err)
		}
	case <-guard.Done():
		t.Fatal("backoff did not join after cancellation")
	}
	if err := waitSourceBackoff(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled backoff: %v", err)
	}
}

type sourceTimeoutSizeSample struct {
	native, returned int64
	err              error
}

type sourceTimeoutPolicyState struct {
	mu                                                   sync.Mutex
	prepared                                             []ingestReplayBatchSnapshot
	attempts                                             []ingestReplayBatchSnapshot
	calls                                                []recordlayer.AttemptCall
	sizes                                                []sourceTimeoutSizeSample
	waits                                                []time.Duration
	events                                               map[string]int
	parentCalls, sourceClosures, activeRuns, activeWaits int
	returned, late                                       bool
	problems                                             []string
	original                                             *recordlayer.FDBDatabase
}

func (s *sourceTimeoutPolicyState) probe(event ingestEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.returned {
		s.late = true
	}
	s.events[event.kind]++
	if event.kind != "prepared" && event.kind != "attempt" {
		return
	}
	snapshot, err := snapshotIngestReplayBatch(event.batch)
	if err != nil {
		s.problems = append(s.problems, err.Error())
		return
	}
	if event.kind == "prepared" {
		s.prepared = append(s.prepared, snapshot)
	} else {
		s.attempts = append(s.attempts, snapshot)
	}
}

type sourceTimeoutPolicyTransactor struct {
	fdb.Database
	parent                             context.Context
	state                              *sourceTimeoutPolicyState
	sourceError, initError, readyError error
	sizeOverride                       int64
}

type sourceTimeoutPolicyTransaction struct {
	fdb.WritableTransaction
	state        *sourceTimeoutPolicyState
	sizeOverride int64
}

type sourceTimeoutPolicySizeFuture struct {
	fdb.FutureInt64
	state        *sourceTimeoutPolicyState
	sizeOverride int64
}

func (f sourceTimeoutPolicySizeFuture) Get() (int64, error) {
	n, err := f.FutureInt64.Get()
	returned := n
	if err == nil && f.sizeOverride != 0 {
		returned = f.sizeOverride
	}
	f.state.mu.Lock()
	f.state.sizes = append(f.state.sizes, sourceTimeoutSizeSample{native: n, returned: returned, err: err})
	f.state.mu.Unlock()
	return returned, err
}

func (tx sourceTimeoutPolicyTransaction) GetApproximateSize() fdb.FutureInt64 {
	return sourceTimeoutPolicySizeFuture{FutureInt64: tx.WritableTransaction.GetApproximateSize(), state: tx.state, sizeOverride: tx.sizeOverride}
}

func (d sourceTimeoutPolicyTransactor) TransactAttempt(ctx context.Context, call recordlayer.AttemptCall, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	source := ctx != d.parent
	d.state.mu.Lock()
	stage := "source"
	if source {
		d.state.calls = append(d.state.calls, call)
		d.state.activeRuns++
	} else {
		stage = "ready"
		if d.state.parentCalls == 0 {
			stage = "init"
		}
		d.state.parentCalls++
	}
	d.state.mu.Unlock()
	if source {
		defer func() { d.state.mu.Lock(); d.state.activeRuns--; d.state.mu.Unlock() }()
	}
	result, err := d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		if source {
			d.state.mu.Lock()
			d.state.sourceClosures++
			d.state.mu.Unlock()
			tx = sourceTimeoutPolicyTransaction{WritableTransaction: tx, state: d.state, sizeOverride: d.sizeOverride}
		}
		result, err := fn(tx)
		// Bound the native retry loop as well as the Record Layer loop, so this
		// regression measures only the new fresh-source-Run policy.
		if optionErr := tx.Options().SetRetryLimit(0); optionErr != nil {
			return nil, optionErr
		}
		if err != nil {
			return result, err
		}
		if source && d.sourceError != nil {
			return nil, d.sourceError
		}
		return result, nil
	})
	if err == nil && stage == "init" && d.initError != nil {
		return nil, d.initError
	}
	if err == nil && stage == "ready" && d.readyError != nil {
		return nil, d.readyError
	}
	return result, err
}

func (d sourceTimeoutPolicyTransactor) ReadTransactAttempt(ctx context.Context, _ recordlayer.AttemptCall, fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return d.Database.ReadTransactCtx(ctx, fn)
}

func newSourceTimeoutPolicyFixture(t *testing.T, ctx context.Context, cluster, order string, sourceError, initError, readyError error, sizeOverride int64) (*FDBBackend, *sourceTimeoutPolicyState) {
	t.Helper()
	c := testConfig()
	c.Count, c.Tenants, c.Batch, c.Workers, c.SaveMode, c.Diagnostics, c.Order = 2, 1, 2, 1, "batch", true, order
	b := openOtelFixture(t, ctx, cluster, c, false)
	seedOtelProfileClientMetrics(t, ctx, b)
	state := &sourceTimeoutPolicyState{events: map[string]int{}}
	original := b.db
	state.original = original
	b.db = recordlayer.NewFDBDatabaseWithTransactor(sourceTimeoutPolicyTransactor{Database: b.raw, parent: ctx, state: state, sourceError: sourceError, initError: initError, readyError: readyError, sizeOverride: sizeOverride}, b.raw)
	if err := b.db.SetMaxAttempts(1); err != nil {
		t.Fatal(err)
	}
	b.ingestProbe = state.probe
	b.ingestWait = func(ctx context.Context, delay time.Duration) error {
		state.mu.Lock()
		state.waits = append(state.waits, delay)
		state.mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { b.ingestProbe = nil; b.ingestWait = nil; b.db = original })
	return b, state
}

func assertSourceTimeoutPreparedAndJoined(t *testing.T, state *sourceTimeoutPolicyState, attempts int) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.returned = true
	if state.late || state.activeRuns != 0 || state.activeWaits != 0 || len(state.problems) != 0 || len(state.prepared) != 1 || len(state.attempts) != attempts || state.sourceClosures != attempts || len(state.calls) != attempts {
		t.Fatalf("source timeout population/join: prepared=%d attempts=%d closures=%d calls=%d active=%d/%d late=%t problems=%v", len(state.prepared), len(state.attempts), state.sourceClosures, len(state.calls), state.activeRuns, state.activeWaits, state.late, state.problems)
	}
	for _, event := range []string{"prepared", "admitted", "producer_done", "before_join"} {
		if state.events[event] != 1 {
			t.Fatalf("event %s=%d want 1: %+v", event, state.events[event], state.events)
		}
	}
	prepared := state.prepared[0]
	if prepared.first != 0 || prepared.next != 2 || len(prepared.messages) != 2 || prepared.estimate <= 0 || prepared.estimate > 300000 || prepared.logicalBytes <= 0 {
		t.Fatalf("prepared immutable source bounds: %+v", prepared)
	}
	ids := map[uint64]bool{}
	for i, attempt := range state.attempts {
		if !sameIngestReplayBatch(prepared, attempt) {
			t.Fatalf("fresh Run %d rebuilt or mutated prepared messages", i+1)
		}
		call := state.calls[i]
		if call.CallID == 0 || ids[call.CallID] || call.Attempt != 0 || call.Execution != 0 || call.Owner != "run" || call.Route != recordlayer.RouteAttempt {
			t.Fatalf("fresh Run %d reused/replayed a Record Layer Run identity: %+v", i+1, call)
		}
		ids[call.CallID] = true
	}
	final, err := snapshotIngestReplayBatch(preparedBatch{first: prepared.first, next: prepared.next, messages: prepared.messages, estimate: prepared.estimate, logicalBytes: prepared.logicalBytes})
	if err != nil || !sameIngestReplayBatch(prepared, final) {
		t.Fatalf("source saves mutated prepared wire bytes: %v", err)
	}
}

func assertSourceTimeoutRawPopulation(t *testing.T, ctx context.Context, b *FDBBackend, owner string, rows int) {
	t.Helper()
	_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		wire, err := tx.Get(b.root.Sub("owner").FDBKey()).Get()
		if err != nil {
			return nil, err
		}
		var got Manifest
		if err := json.Unmarshal(wire, &got); err != nil {
			return nil, err
		}
		if got != manifest(b.cfg, owner) {
			return nil, fmt.Errorf("owner=%+v want %s", got, owner)
		}
		prefixes := []struct {
			name   string
			prefix subspace.Subspace
			count  int
		}{
			{"sources", b.store.Sub(recordlayer.RecordKey), rows},
			{"custom primary", b.store.Sub(recordlayer.IndexKey, b.idx.SubspaceTupleKey()), 0},
			{"custom secondary", otelSecondary(b), rows * b.cfg.Attributes},
		}
		for _, name := range []string{spanIndexName, timeIndexName} {
			index := b.md.GetIndex(name)
			if index == nil {
				return nil, fmt.Errorf("missing maintained index %s", name)
			}
			prefixes = append(prefixes, struct {
				name   string
				prefix subspace.Subspace
				count  int
			}{name, b.store.Sub(recordlayer.IndexKey, index.SubspaceTupleKey()), rows})
		}
		for _, check := range prefixes {
			entries, err := tx.GetRange(check.prefix, fdb.RangeOptions{Limit: check.count + 1}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(entries) != check.count {
				return nil, fmt.Errorf("%s durable entries=%d want %d", check.name, len(entries), check.count)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertSourceTimeoutDiagnostics(t *testing.T, stats Statistics, state *sourceTimeoutPolicyState, runs, timeouts, retries, committed int64) {
	t.Helper()
	d := stats.Diagnostics
	if d == nil || d.Scope != "source_batches_only" || d.SourceRuns != runs || d.SourceAttempts != runs || d.SourceReplays != 0 || d.SourceTimeouts != timeouts || d.SourceFreshRetries != retries || d.CommittedBatches != committed || d.CommittedRows != 2*committed || stats.Rows != 2*committed || stats.Attributes != 2*committed*8 {
		t.Fatalf("source timeout diagnostic scope/accounting: %+v stats=%+v", d, stats)
	}
	if stats.LogicalPayloadBytes != state.prepared[0].logicalBytes*committed || stats.Details["committed_batches"] != fmt.Sprint(committed) || stats.Details["save_mode"] != "batch" || stats.Details["workers"] != "1" {
		t.Fatalf("fresh Run policy changed acknowledged progress/batch boundary: %+v", stats)
	}
	var bytes int64
	if int64(len(state.sizes)) != runs {
		t.Fatalf("successful size sample population=%d want %d", len(state.sizes), runs)
	}
	for _, sample := range state.sizes {
		if sample.err != nil || sample.native <= 0 || sample.returned <= 0 {
			t.Fatalf("source size sample was not obtained from a successful real future: %+v", sample)
		}
		bytes += sample.returned
	}
	if d.ApproximateMutationBytes != bytes {
		t.Fatalf("mutation bytes=%d want sum of all actual guard samples %d", d.ApproximateMutationBytes, bytes)
	}
	wantPhases := 6
	if len(state.waits) > 0 {
		wantPhases++
		if d.PhaseNanos["timeout_backoff"] <= 0 {
			t.Fatalf("missing positive timeout backoff phase: %+v", d.PhaseNanos)
		}
	}
	if len(d.PhaseNanos) != wantPhases {
		t.Fatalf("unexpected source timing phases: %+v", d.PhaseNanos)
	}
	for _, phase := range []string{"preparation", "manifest", "open", "save", "guard", "run"} {
		if d.PhaseNanos[phase] <= 0 {
			t.Fatalf("nonpositive source phase %s: %+v", phase, d.PhaseNanos)
		}
	}
	opened := d.Timer[recordlayer.EventOpenStore.Name]
	if opened == nil || opened.Event != recordlayer.EventOpenStore || opened.Count != runs || opened.CumulativeValue <= 0 {
		t.Fatalf("source-only open timer: %+v want %d", opened, runs)
	}
	commit := d.Timer[recordlayer.EventCommit.Name]
	if committed == 0 {
		if commit != nil && (commit.Count != 0 || commit.CumulativeValue != 0) {
			t.Fatalf("failed source Runs reported successful commits: %+v", commit)
		}
	} else if commit == nil || commit.Event != recordlayer.EventCommit || commit.Count != committed || commit.CumulativeValue <= 0 {
		t.Fatalf("source-only commit timer: %+v want %d", commit, committed)
	}
	for _, event := range []recordlayer.Event{recordlayer.EventSaveRecord, recordlayer.CountSaveRecordKey, recordlayer.CountSaveRecordKeyBytes, recordlayer.CountSaveRecordValueBytes} {
		if counter := d.Timer[event.Name]; counter != nil && (counter.Count != 0 || counter.CumulativeValue != 0) {
			t.Fatalf("batch invented scalar save work %s: %+v", event.Name, counter)
		}
	}
	attempts := int64(len(state.attempts))
	assertOtelProfileMoments(t, stats, map[string]int64{
		"preparation": int64(len(state.prepared)), "manifest": attempts,
		"open": attempts, "save": attempts, "guard": attempts,
		"run": int64(len(state.calls)), "timeout_backoff": int64(len(state.waits)), "commit": committed,
	})
}

func testOtelTimeoutPolicy(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	t.Run("exhausts_eight_fresh_runs", func(t *testing.T) {
		timeout := fdb.Error{Code: 1031}
		b, state := newSourceTimeoutPolicyFixture(t, ctx, cluster, order, timeout, nil, nil, 0)
		before := b.raw.Metrics()
		stats, err := b.Ingest(ctx)
		after := b.raw.Metrics()
		assertOtelProfileClientMetrics(t, stats, before, after)
		if !errors.Is(err, timeout) {
			t.Fatalf("timeout exhaustion lost exact cause: %v", err)
		}
		assertSourceTimeoutPreparedAndJoined(t, state, 8)
		wantWaits := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond}
		if !reflect.DeepEqual(state.waits, wantWaits) {
			t.Fatalf("fresh-Run waits=%v want %v", state.waits, wantWaits)
		}
		assertSourceTimeoutDiagnostics(t, stats, state, 8, 8, 7, 0)
		assertSourceTimeoutRawPopulation(t, ctx, b, "loading", 0)
	})
	for _, cancelParent := range []bool{true, false} {
		name := "cancel_child_wait"
		if cancelParent {
			name = "cancel_parent_wait"
		}
		t.Run(name, func(t *testing.T) {
			ingestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			b, state := newSourceTimeoutPolicyFixture(t, ingestCtx, cluster, order, fdb.Error{Code: 1031}, nil, nil, 0)
			b.ingestWait = func(waitCtx context.Context, delay time.Duration) error {
				state.mu.Lock()
				state.waits = append(state.waits, delay)
				state.activeWaits++
				state.mu.Unlock()
				defer func() { state.mu.Lock(); state.activeWaits--; state.mu.Unlock() }()
				if waitCtx.Err() != nil {
					return fmt.Errorf("wait entered with canceled worker context: %w", waitCtx.Err())
				}
				child, cancelChild := context.WithCancel(waitCtx)
				defer cancelChild()
				entry := &sourceBackoffEntryContext{Context: child, entered: make(chan struct{})}
				canceled := make(chan struct{})
				go func() {
					select {
					case <-entry.entered:
						if cancelParent {
							cancel()
						} else {
							cancelChild()
						}
					case <-child.Done():
					}
					close(canceled)
				}()
				err := waitSourceBackoff(entry, time.Hour)
				select {
				case <-entry.entered:
				default:
					cancelChild()
					<-canceled
					return errors.New("source backoff skipped its live-context wait")
				}
				<-canceled
				return err
			}
			before := b.raw.Metrics()
			stats, err := b.Ingest(ingestCtx)
			after := b.raw.Metrics()
			assertOtelProfileClientMetrics(t, stats, before, after)
			if !errors.Is(err, context.Canceled) || (cancelParent && !errors.Is(ingestCtx.Err(), context.Canceled)) || (!cancelParent && ingestCtx.Err() != nil) {
				t.Fatalf("wait cancellation: parent=%t context=%v error=%v", cancelParent, ingestCtx.Err(), err)
			}
			assertSourceTimeoutPreparedAndJoined(t, state, 1)
			if !reflect.DeepEqual(state.waits, []time.Duration{25 * time.Millisecond}) {
				t.Fatalf("cancel wait population: %v", state.waits)
			}
			assertSourceTimeoutDiagnostics(t, stats, state, 1, 1, 0, 0)
			assertSourceTimeoutRawPopulation(t, ctx, b, "loading", 0)
		})
	}
	for _, tc := range []struct {
		name   string
		cause  error
		size   int64
		budget bool
	}{
		{"fdb1004", fdb.Error{Code: 1004}, 0, false},
		{"fdb1021", fdb.Error{Code: 1021}, 0, false},
		{"fdb1025", fdb.Error{Code: 1025}, 0, false},
		{"actual_size_guard", nil, 500001, true},
		{"admission", fail("admission", "injected source", "source rejected"), 0, false},
		{"config", fail("config", "injected source", "invalid source configuration"), 0, false},
		{"ownership", fail("ownership", "injected source", "source ownership rejected"), 0, false},
		{"budget_with_timeout_cause", wrap("budget", "injected guard", fdb.Error{Code: 1031}), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, state := newSourceTimeoutPolicyFixture(t, ctx, cluster, order, tc.cause, nil, nil, tc.size)
			before := b.raw.Metrics()
			stats, err := b.Ingest(ctx)
			after := b.raw.Metrics()
			assertOtelProfileClientMetrics(t, stats, before, after)
			if tc.budget {
				var budget *QueryBudgetError
				if !errors.As(err, &budget) || !strings.Contains(err.Error(), "bytes=500001") {
					t.Fatalf("500001-byte guard did not remain a typed terminal budget error: %v", err)
				}
			} else if !errors.Is(err, tc.cause) {
				t.Fatalf("non-timeout source failure lost exact cause %v: %v", tc.cause, err)
			}
			assertSourceTimeoutPreparedAndJoined(t, state, 1)
			if len(state.waits) != 0 {
				t.Fatalf("non-timeout source error invoked fresh-Run backoff: %v", state.waits)
			}
			assertSourceTimeoutDiagnostics(t, stats, state, 1, 0, 0, 0)
			assertSourceTimeoutRawPopulation(t, ctx, b, "loading", 0)
		})
	}
	for _, stage := range []string{"init", "ready"} {
		t.Run(stage+"_timeout_is_not_source_recovery", func(t *testing.T) {
			timeout := fdb.Error{Code: 1031}
			var initError, readyError error
			if stage == "init" {
				initError = timeout
			} else {
				readyError = timeout
			}
			b, state := newSourceTimeoutPolicyFixture(t, ctx, cluster, order, nil, initError, readyError, 0)
			before := b.raw.Metrics()
			stats, err := b.Ingest(ctx)
			after := b.raw.Metrics()
			if stage != "init" {
				assertOtelProfileClientMetrics(t, stats, before, after)
			}
			if !errors.Is(err, timeout) || len(state.waits) != 0 {
				t.Fatalf("%s timeout was retried/adopted: waits=%v error=%v", stage, state.waits, err)
			}
			if stage == "init" {
				if state.parentCalls != 1 || len(state.calls) != 0 || len(state.prepared) != 0 || len(state.attempts) != 0 || stats.Rows != 0 || stats.Diagnostics != nil {
					t.Fatalf("reported initialization timeout started source work: state=%+v stats=%+v", state, stats)
				}
				assertSourceTimeoutRawPopulation(t, ctx, b, "loading", 0)
			} else {
				if state.parentCalls != 2 {
					t.Fatalf("ready timeout called initialization/publication %d times", state.parentCalls)
				}
				assertSourceTimeoutPreparedAndJoined(t, state, 1)
				assertSourceTimeoutDiagnostics(t, stats, state, 1, 0, 0, 1)
				assertSourceTimeoutRawPopulation(t, ctx, b, "ready", 2)
			}
			priorRuns, priorPrepared := len(state.calls), len(state.prepared)
			repeated, repeatedErr := b.Ingest(ctx)
			var ownership *OwnershipError
			if !errors.As(repeatedErr, &ownership) || repeated.Rows != 0 || repeated.Diagnostics != nil || len(state.calls) != priorRuns || len(state.prepared) != priorPrepared || len(state.waits) != 0 {
				t.Fatalf("%s committed-but-reported-timeout manifest was adopted on repeat: error=%v stats=%+v sourceRuns=%d prepared=%d waits=%v", stage, repeatedErr, repeated, len(state.calls), len(state.prepared), state.waits)
			}
			if stage == "ready" {
				b.db = state.original
				sources := []Span{Generate(b.cfg, 0), Generate(b.cfg, 1)}
				for _, source := range sources {
					assertOtelSource(t, ctx, b, source)
				}
				assertOtelCommittedPostings(t, ctx, b, sources)
			}
		})
	}
}
