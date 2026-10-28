package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

type ingestTimeoutCommitObservation struct {
	call recordlayer.AttemptCall
	err  error
}

type ingestTimeoutCommitState struct {
	mu                 sync.Mutex
	replay             ingestReplayState
	order              string
	sourceCalls        int
	firstBodyFinished  bool
	timeoutReported    bool
	freshImageObserved bool
	pendingStarted     bool
	pendingErr         error
	pendingRelease     chan struct{}
	pendingDone        chan struct{}
	releaseOnce        sync.Once
	waits              []time.Duration
	observations       []ingestTimeoutCommitObservation
}

func (s *ingestTimeoutCommitState) releasePending() {
	s.releaseOnce.Do(func() { close(s.pendingRelease) })
}

func (s *ingestTimeoutCommitState) joinPending() {
	s.releasePending()
	s.mu.Lock()
	started := s.pendingStarted
	s.mu.Unlock()
	if started {
		<-s.pendingDone
	}
}

func (s *ingestTimeoutCommitState) pendingResult(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.pendingDone:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingErr
}

type ingestTimeoutCommitTransactor struct {
	fdb.Database
	parent      context.Context
	state       *ingestTimeoutCommitState
	beforeFresh func() error
}

func (d ingestTimeoutCommitTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d ingestTimeoutCommitTransactor) body(tx fdb.WritableTransaction, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	d.state.replay.mu.Lock()
	d.state.replay.closureEntries++
	attempt := d.state.replay.closureEntries
	d.state.replay.mu.Unlock()
	result, bodyErr := fn(ingestReplayTransaction{WritableTransaction: tx, state: &d.state.replay, attempt: attempt})
	// The application opens stores with retry limit 10. Restore zero after its
	// entire closure/precommit returns, so the native client cannot replay it.
	if err := tx.Options().SetRetryLimit(0); err != nil {
		return nil, err
	}
	if bodyErr != nil {
		return result, bodyErr
	}
	d.state.replay.mu.Lock()
	guards := 0
	for _, sample := range d.state.replay.sizes {
		if sample.attempt == attempt && sample.err == nil && sample.bytes > 0 && sample.bytes <= 500000 {
			guards++
		}
	}
	d.state.replay.mu.Unlock()
	if guards != 1 {
		return nil, fmt.Errorf("source body %d did not finish exactly one real size guard: %d", attempt, guards)
	}
	return result, nil
}

func (d ingestTimeoutCommitTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ctx == d.parent {
		return d.Database.TransactCtx(ctx, fn)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.state.mu.Lock()
	d.state.sourceCalls++
	call := d.state.sourceCalls
	d.state.mu.Unlock()
	if call == 1 && d.state.order == "already_committed" {
		result, err := d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) { return d.body(tx, fn) })
		if err != nil {
			return result, err
		}
		d.state.mu.Lock()
		d.state.firstBodyFinished, d.state.timeoutReported = true, true
		d.state.mu.Unlock()
		return result, fdb.Error{Code: 1031}
	}
	if call == 1 {
		tx, err := d.Database.CreateTransaction()
		if err != nil {
			return nil, err
		}
		retained := false
		defer func() {
			if !retained {
				tx.Cancel()
			}
		}()
		if err := tx.Options().SetRetryLimit(0); err != nil {
			return nil, err
		}
		result, err := d.body(tx, fn)
		if err != nil {
			return result, err
		}
		d.state.mu.Lock()
		d.state.firstBodyFinished, d.state.timeoutReported, d.state.pendingStarted = true, true, true
		d.state.mu.Unlock()
		retained = true
		// Only Commit outlives the returned Run. No source closure, precommit,
		// probe, size guard or record context is used by this goroutine.
		go func() {
			defer close(d.state.pendingDone)
			defer tx.Cancel()
			<-d.state.pendingRelease
			err := tx.Commit().Get()
			d.state.mu.Lock()
			d.state.pendingErr = err
			d.state.mu.Unlock()
		}()
		return result, fdb.Error{Code: 1031}
	}
	if call == 2 {
		d.state.mu.Lock()
		finished, timedOut := d.state.firstBodyFinished, d.state.timeoutReported
		d.state.mu.Unlock()
		if !finished || !timedOut {
			return nil, errors.New("fresh Run began before first source body/timeout completed")
		}
		if err := d.beforeFresh(); err != nil {
			return nil, err
		}
		d.state.mu.Lock()
		d.state.freshImageObserved = true
		d.state.mu.Unlock()
	}
	result, err := d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		result, err := d.body(tx, fn)
		if err != nil {
			return result, err
		}
		if call == 2 && d.state.order == "pending_first_wins" {
			d.state.releasePending()
			if err := d.state.pendingResult(ctx); err != nil {
				return nil, fmt.Errorf("first pending commit failed before fresh commit: %w", err)
			}
		}
		return result, nil
	})
	if err == nil && call == 2 && d.state.order == "fresh_second_wins" {
		d.state.releasePending()
		pendingErr := d.state.pendingResult(ctx)
		var conflict fdb.Error
		if !errors.As(pendingErr, &conflict) || conflict.Code != 1020 {
			return nil, fmt.Errorf("older pending commit must conflict after fresh commit: %v", pendingErr)
		}
	}
	return result, err
}

func testOtelTimeoutCommitRecovery(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, mode := range []string{"serial", "pipelined", "batch"} {
		for _, commitOrder := range []string{"already_committed", "pending_first_wins", "fresh_second_wins"} {
			t.Run(mode+"_"+commitOrder, func(t *testing.T) {
				c := testConfig()
				c.Count, c.Tenants, c.Batch, c.Workers = 2, 1, 2, 1
				c.Order, c.SaveMode, c.Diagnostics = order, mode, true
				b := openOtelFixture(t, ctx, cluster, c, false)
				c = b.cfg
				sources := []Span{Generate(c, 0), Generate(c, 1)}
				old := []Span{Generate(c, 0), Generate(c, 1)}
				for i := range old {
					old[i].Start += 1000000
					old[i].Attributes[0] = Attribute{"span", "timeout.old_image", "string", fmt.Sprint(i)}
					saveOtelSpan(t, ctx, b, old[i])
					assertOtelSource(t, ctx, b, old[i])
				}
				assertOtelCommittedPostings(t, ctx, b, old)
				state := &ingestTimeoutCommitState{order: commitOrder, pendingRelease: make(chan struct{}), pendingDone: make(chan struct{})}
				// Registered after opening the fixture: release and join must finish
				// before namespace cleanup or closing the shared database handle.
				t.Cleanup(state.joinPending)
				original := b.db
				inspectFreshImage := func() error {
					want := old
					if commitOrder == "already_committed" {
						want = sources
					}
					_, err := original.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
						store, err := b.open(rc)
						if err != nil {
							return nil, err
						}
						if err := b.readManifest(rc, false); err != nil {
							return nil, err
						}
						for _, source := range want {
							record, err := store.LoadRecord(otelPK(source))
							if err != nil {
								return nil, err
							}
							if record == nil {
								return nil, fmt.Errorf("fresh Run inspection missing source %s", source.ID)
							}
							got, err := decodeRecord(record.Record)
							if err != nil {
								return nil, err
							}
							expected := source
							expected.Attributes = append([]Attribute(nil), source.Attributes...)
							if !equalSpan(got, expected) {
								return nil, fmt.Errorf("fresh Run saw wrong durable source image for %s", source.ID)
							}
						}
						return nil, nil
					})
					return err
				}
				wrapped := recordlayer.NewFDBDatabaseWithTransactor(ingestTimeoutCommitTransactor{Database: b.raw, parent: ctx, state: state, beforeFresh: inspectFreshImage}, b.raw)
				b.db = wrapped
				b.ingestProbe = state.replay.probe
				b.ingestWait = func(waitCtx context.Context, delay time.Duration) error {
					if err := waitCtx.Err(); err != nil {
						return err
					}
					state.mu.Lock()
					state.waits = append(state.waits, delay)
					state.mu.Unlock()
					return nil
				}
				wrapped.SetAttemptObserver(func(call recordlayer.AttemptCall, err error) {
					state.mu.Lock()
					state.observations = append(state.observations, ingestTimeoutCommitObservation{call, err})
					state.mu.Unlock()
				})
				defer func() { b.ingestProbe = nil; b.ingestWait = nil; wrapped.SetAttemptObserver(nil); b.db = original }()
				metricsBefore := b.raw.Metrics()
				stats, err := b.Ingest(ctx)
				metricsAfter := b.raw.Metrics()
				state.joinPending()
				b.ingestProbe, b.ingestWait = nil, nil
				wrapped.SetAttemptObserver(nil)
				b.db = original
				assertOtelProfileClientMetrics(t, stats, metricsBefore, metricsAfter)
				if err != nil {
					t.Fatalf("public ambiguous commit recovery: %v", err)
				}
				wantAttempts, wantInternal := int64(2), int64(0)
				if commitOrder == "pending_first_wins" {
					wantAttempts, wantInternal = 3, 1
				}
				state.mu.Lock()
				calls, bodyFinished, reported, observed := state.sourceCalls, state.firstBodyFinished, state.timeoutReported, state.freshImageObserved
				waits := append([]time.Duration(nil), state.waits...)
				observations := append([]ingestTimeoutCommitObservation(nil), state.observations...)
				pendingErr := state.pendingErr
				state.mu.Unlock()
				if int64(calls) != wantAttempts || !bodyFinished || !reported || !observed || len(waits) != 1 || waits[0] != 25*time.Millisecond {
					t.Fatalf("ambiguous commit barriers/runs: calls=%d firstBody=%t timeout=%t freshImage=%t waits=%v", calls, bodyFinished, reported, observed, waits)
				}
				if commitOrder == "pending_first_wins" && pendingErr != nil {
					t.Fatalf("first pending commit should win: %v", pendingErr)
				}
				if commitOrder == "fresh_second_wins" {
					var conflict fdb.Error
					if !errors.As(pendingErr, &conflict) || conflict.Code != 1020 {
						t.Fatalf("older pending commit did not conflict: %v", pendingErr)
					}
				}
				d := stats.Diagnostics
				if d == nil || d.SourceRuns != 2 || d.SourceAttempts != wantAttempts || d.SourceReplays != wantInternal || d.SourceTimeouts != 1 || d.SourceFreshRetries != 1 || d.CommittedBatches != 1 || d.CommittedRows != 2 {
					t.Fatalf("ambiguous timeout/once-only ACK diagnostics: %+v", d)
				}
				if counter := d.Timer[recordlayer.EventCommit.Name]; counter == nil || counter.Count != 1 {
					t.Fatalf("only successful acknowledged source Run gets a commit timer: %+v", counter)
				}
				state.replay.mu.Lock()
				prepared := append([]ingestReplayBatchSnapshot(nil), state.replay.prepared...)
				attempts := append([]ingestReplayBatchSnapshot(nil), state.replay.attempts...)
				sizes := append([]ingestReplaySizeSample(nil), state.replay.sizes...)
				problems := append([]string(nil), state.replay.problems...)
				entries := state.replay.closureEntries
				state.replay.mu.Unlock()
				if len(problems) != 0 || len(prepared) != 1 || int64(len(attempts)) != wantAttempts || int64(entries) != wantAttempts || int64(len(sizes)) != wantAttempts {
					t.Fatalf("prepared/replayed message population: prepared=%d attempts=%d entries=%d sizes=%v problems=%v", len(prepared), len(attempts), entries, sizes, problems)
				}
				assertOtelProfileMoments(t, stats, map[string]int64{
					"preparation": int64(len(prepared)),
					"manifest":    wantAttempts, "open": wantAttempts, "save": wantAttempts, "guard": wantAttempts,
					"run": 2, "timeout_backoff": 1, "commit": 1,
				})
				for i, attempt := range attempts {
					if !sameIngestReplayBatch(prepared[0], attempt) {
						t.Fatalf("source attempt %d changed once-prepared messages/wire", i+1)
					}
				}
				final, err := snapshotIngestReplayBatch(preparedBatch{first: prepared[0].first, next: prepared[0].next, estimate: prepared[0].estimate, logicalBytes: prepared[0].logicalBytes, messages: prepared[0].messages})
				if err != nil || !sameIngestReplayBatch(prepared[0], final) {
					t.Fatalf("save/replay mutated prepared messages: %v", err)
				}
				var mutationBytes int64
				for i, size := range sizes {
					if size.attempt != i+1 || size.err != nil || size.bytes <= 0 {
						t.Fatalf("real size guard %d: %+v", i, size)
					}
					mutationBytes += size.bytes
				}
				if d.ApproximateMutationBytes != mutationBytes {
					t.Fatalf("attempt-size diagnostics=%d want %d", d.ApproximateMutationBytes, mutationBytes)
				}
				callIDs := map[uint64]bool{}
				timeouts, conflicts, successes := 0, 0, 0
				for _, observation := range observations {
					if observation.call.Owner != "run" || observation.call.Route != recordlayer.RouteAttempt || observation.call.CallID == 0 {
						t.Fatalf("external observer invalid call: %+v", observation)
					}
					callIDs[observation.call.CallID] = true
					if observation.err == nil {
						successes++
						continue
					}
					var code fdb.Error
					if !errors.As(observation.err, &code) {
						t.Fatalf("external observer unexpected error: %v", observation.err)
					}
					switch code.Code {
					case 1031:
						timeouts++
					case 1020:
						conflicts++
					default:
						t.Fatalf("external observer unexpected FDB code: %v", observation.err)
					}
				}
				if len(callIDs) != 4 || successes != 3 || timeouts != 1 || int64(conflicts) != wantInternal {
					t.Fatalf("external observer lost Run/retry outcomes: calls=%d successes=%d timeouts=%d conflicts=%d", len(callIDs), successes, timeouts, conflicts)
				}
				var logical int64
				for _, source := range sources {
					wire, err := json.Marshal(source)
					if err != nil {
						t.Fatal(err)
					}
					logical += int64(len(wire))
					assertOtelSource(t, ctx, b, source)
				}
				if stats.Rows != 2 || stats.Attributes != int64(2*c.Attributes) || stats.LogicalPayloadBytes != logical || stats.Details["committed_batches"] != "1" {
					t.Fatalf("ambiguous commits duplicated acknowledgments: %+v want logical=%d", stats, logical)
				}
				assertOtelCommittedPostings(t, ctx, b, sources)
				audit, err := b.Audit(ctx)
				if err != nil || audit.Rows != 2 || audit.Structures["eq_secondary"].Keys != int64(2*c.Attributes) || audit.Structures[spanIndexName].Keys != 2 || audit.Structures[timeIndexName].Keys != 2 {
					t.Fatalf("ambiguous commit primary/custom/native census: %+v %v", audit, err)
				}
				for i, source := range sources {
					matches, err := b.Span(ctx, source.ID)
					if err != nil || !sameJSON(matches, []SpanMatch{{source.Trace, source.ID, source.Start}}) {
						t.Fatalf("recovered intrinsic identity: %+v %v", matches, err)
					}
					oldTime, err := b.Time(ctx, TimeRange{Start: old[i].Start, End: old[i].Start + 1})
					if err != nil || len(oldTime.Hits) != 0 {
						t.Fatalf("recovered source retains old native timestamp: %+v %v", oldTime, err)
					}
					oldTerm, err := b.Query(ctx, Attribute{"span", "timeout.old_image", "string", fmt.Sprint(i)})
					if err != nil || len(oldTerm.Hits) != 0 {
						t.Fatalf("recovered source retains old custom term: %+v %v", oldTerm, err)
					}
				}
			})
		}
	}
}
