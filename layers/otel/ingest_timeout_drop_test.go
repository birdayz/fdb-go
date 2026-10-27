package otel

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

type otelTimeoutDropResult struct {
	stats Statistics
	err   error
}

type otelTimeoutDropTransactor struct {
	fdb.Database
	parent                        context.Context
	root                          fdb.Key
	pending                       bool
	releaseCommit                 <-chan struct{}
	prepared, actorDone           chan struct{}
	preparedOnce                  sync.Once
	parents, sources, firstBodies atomic.Int64
	initialCommitted              atomic.Bool
	mu                            sync.Mutex
	completionError               error
}

func (d *otelTimeoutDropTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *otelTimeoutDropTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ctx == d.parent {
		role := d.parents.Add(1)
		result, err := d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
			result, err := fn(tx)
			if err == nil && role == 1 {
				tx.Set(d.root, []byte("owned-timeout-root-boundary"))
			}
			return result, err
		})
		if role == 1 && err == nil {
			d.initialCommitted.Store(true)
		}
		return result, err
	}
	if d.sources.Add(1) != 1 {
		return d.Database.TransactCtx(ctx, fn)
	}
	run := func() (any, error) {
		return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
			d.firstBodies.Add(1)
			result, err := fn(tx)
			if err != nil {
				return nil, err
			}
			// The real source closure, record-layer precommit, and size guard
			// have finished. Never rerun it inside this late commit actor.
			if err := tx.Options().SetRetryLimit(0); err != nil {
				return nil, err
			}
			d.preparedOnce.Do(func() { close(d.prepared) })
			if d.pending {
				select {
				case <-d.releaseCommit:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return result, nil
		})
	}
	finish := func(err error) {
		d.mu.Lock()
		d.completionError = err
		d.mu.Unlock()
		close(d.actorDone)
	}
	if !d.pending {
		_, err := run()
		finish(err)
		if err != nil {
			return nil, err
		}
		return nil, fdb.Error{Code: 1031}
	}
	go func() { _, err := run(); finish(err) }()
	select {
	case <-d.prepared:
		// Report uncertainty while a real transaction retains its ownership
		// read conflict and fully prepared mutations, but has not committed.
		return nil, fdb.Error{Code: 1031}
	case <-d.actorDone:
		d.mu.Lock()
		err := d.completionError
		d.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("late source transaction finished before its commit barrier")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func awaitOtelTimeoutDrop(t *testing.T, ctx context.Context, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", name, ctx.Err())
	case <-time.After(10 * time.Second):
		t.Fatalf("missing positive %s handshake", name)
	}
}

func otelTimeoutDropRoot(t *testing.T, ctx context.Context, b *FDBBackend) map[string]string {
	t.Helper()
	var rows []fdb.KeyValue
	_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		root, err := fdb.PrefixRange(b.root.Bytes())
		if err != nil {
			return nil, err
		}
		rows, err = tx.GetRange(root, fdb.RangeOptions{Limit: 513}).GetSliceWithError()
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 512 {
		t.Fatal("timeout/drop fixture exceeded bounded root population")
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[string(row.Key)] = string(row.Value)
	}
	return out
}

func testOtelTimeoutDropSafety(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, pending := range []bool{false, true} {
		name := "committed_but_reported_timeout"
		if pending {
			name = "drop_precedes_pending_commit"
		}
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Attributes, c.Batch, c.Workers, c.Diagnostics, c.Order = 1, 1, 4, 1, 1, true, order
			b := openOtelFixture(t, ctx, cluster, c, false)
			c = b.cfg
			siblingConfig := c
			siblingConfig.Count = 3
			sibling := openOtelFixture(t, ctx, cluster, siblingConfig, true)
			_, err := sibling.raw.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
				tx.Set(sibling.root.FDBKey(), []byte("independent-timeout-sibling-boundary"))
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			siblingBefore := otelTimeoutDropRoot(t, ctx, sibling)
			if siblingBefore[string(sibling.root.FDBKey())] != "independent-timeout-sibling-boundary" || len(siblingBefore) <= 1 {
				t.Fatal("sibling sentinel lacks a populated independent namespace")
			}
			// Open the independent public drop handle before holding a source
			// commit, keeping that transaction's 1500ms deadline out of setup.
			dropper, err := OpenFDBContext(ctx, c, cluster)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(dropper.Close)
			runCtx, cancel := context.WithCancel(ctx)
			commitRelease, waitRelease := make(chan struct{}), make(chan struct{})
			releaseCommit := sync.OnceFunc(func() { close(commitRelease) })
			releaseWait := sync.OnceFunc(func() { close(waitRelease) })
			waitEntered := make(chan struct{})
			var enteredOnce sync.Once
			var waitCalls atomic.Int64
			var waitDelay atomic.Int64
			wrapped := &otelTimeoutDropTransactor{Database: b.raw, parent: runCtx, root: b.root.FDBKey(), pending: pending, releaseCommit: commitRelease, prepared: make(chan struct{}), actorDone: make(chan struct{})}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(wrapped, b.raw)
			b.ingestWait = func(waitCtx context.Context, delay time.Duration) error {
				waitCalls.Add(1)
				waitDelay.Store(int64(delay))
				enteredOnce.Do(func() { close(waitEntered) })
				select {
				case <-waitRelease:
					return waitCtx.Err()
				case <-waitCtx.Done():
					return waitCtx.Err()
				}
			}
			var observerMu sync.Mutex
			var observedCalls []recordlayer.AttemptCall
			var observedErrors []error
			b.db.SetAttemptObserver(func(call recordlayer.AttemptCall, err error) {
				observerMu.Lock()
				observedCalls, observedErrors = append(observedCalls, call), append(observedErrors, err)
				observerMu.Unlock()
			})
			result := make(chan otelTimeoutDropResult, 1)
			done := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				releaseCommit()
				releaseWait()
				joined := true
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("timeout/drop Ingest did not exit cleanup barriers")
					joined = false
				}
				if wrapped.sources.Load() > 0 {
					select {
					case <-wrapped.actorDone:
					case <-time.After(10 * time.Second):
						t.Error("late timeout/drop commit actor was not joined")
						joined = false
					}
				}
				if joined {
					b.db.SetAttemptObserver(nil)
					b.db, b.ingestWait = original, nil
				}
			})
			go func() {
				defer close(done)
				stats, err := b.Ingest(runCtx)
				result <- otelTimeoutDropResult{stats: stats, err: err}
			}()
			awaitOtelTimeoutDrop(t, ctx, wrapped.prepared, "actual first source closure/precommit completed")
			awaitOtelTimeoutDrop(t, ctx, waitEntered, "source 1031 paused before fresh Run")
			if !wrapped.initialCommitted.Load() || wrapped.firstBodies.Load() != 1 || wrapped.sources.Load() != 1 || waitCalls.Load() != 1 || time.Duration(waitDelay.Load()) != 25*time.Millisecond || ctx.Err() != nil || runCtx.Err() != nil {
				t.Fatalf("drop did not reach a live, actual first-source timeout: initialized=%t bodies=%d sources=%d waits=%d delay=%s parent=%v ingest=%v", wrapped.initialCommitted.Load(), wrapped.firstBodies.Load(), wrapped.sources.Load(), waitCalls.Load(), time.Duration(waitDelay.Load()), ctx.Err(), runCtx.Err())
			}
			if !pending {
				awaitOtelTimeoutDrop(t, ctx, wrapped.actorDone, "first source actually committed before reported timeout")
				wrapped.mu.Lock()
				commitErr := wrapped.completionError
				wrapped.mu.Unlock()
				if commitErr != nil {
					t.Fatalf("committed timeout source failed its real transaction: %v", commitErr)
				}
				commit, err := readOtelIngestCommit(ctx, b, recordlayer.AttemptCall{})
				if err != nil || commit.owner != manifest(c, "loading") || commit.recordKeys != 1 || commit.postingKeys != 4 {
					t.Fatalf("maybe-committed positive population: %+v %v", commit, err)
				}
				if before := otelTimeoutDropRoot(t, ctx, b); before[string(b.root.FDBKey())] != "owned-timeout-root-boundary" {
					t.Fatal("owned bare boundary was not committed")
				}
			} else {
				select {
				case <-wrapped.actorDone:
					t.Fatal("pending source commit escaped its independent barrier")
				default:
				}
			}
			dropCtx, dropCancel := context.WithTimeout(ctx, 5*time.Second)
			err = dropper.Drop(dropCtx)
			dropCancel()
			if err != nil {
				t.Fatalf("public owned Drop failed while source was paused: %v", err)
			}
			if pending {
				releaseCommit()
				awaitOtelTimeoutDrop(t, ctx, wrapped.actorDone, "late source commit rejected after Drop")
				wrapped.mu.Lock()
				lateError := wrapped.completionError
				wrapped.mu.Unlock()
				var conflict fdb.Error
				if !errors.As(lateError, &conflict) || conflict.Code != 1020 || wrapped.firstBodies.Load() != 1 {
					t.Fatalf("Drop must conflict the original ownership read, without rerunning source fn: bodies=%d error=%v", wrapped.firstBodies.Load(), lateError)
				}
			}
			if ctx.Err() != nil || runCtx.Err() != nil {
				t.Fatalf("Drop canceled the ingestion parent instead of rejecting ownership: parent=%v ingest=%v", ctx.Err(), runCtx.Err())
			}
			if after := otelTimeoutDropRoot(t, ctx, b); len(after) != 0 {
				t.Fatalf("Drop left owned root/bare boundary/index state: %v", after)
			}
			if after := otelTimeoutDropRoot(t, ctx, sibling); !reflect.DeepEqual(after, siblingBefore) {
				t.Fatal("owned Drop modified populated sibling namespace")
			}
			releaseWait()
			awaitOtelTimeoutDrop(t, ctx, done, "fresh source Run rejected missing ownership")
			out := <-result
			var ownership *OwnershipError
			if !errors.As(out.err, &ownership) || errors.Is(out.err, context.Canceled) || ctx.Err() != nil || runCtx.Err() != nil {
				t.Fatalf("fresh source replay did not fail on missing manifest with live parent: err=%v parent=%v ingest=%v", out.err, ctx.Err(), runCtx.Err())
			}
			if wrapped.sources.Load() != 2 || wrapped.parents.Load() != 1 || wrapped.firstBodies.Load() != 1 || waitCalls.Load() != 1 {
				t.Fatalf("post-Drop replay recreated initialization/ready or repeated source: parent=%d source=%d firstBodies=%d waits=%d", wrapped.parents.Load(), wrapped.sources.Load(), wrapped.firstBodies.Load(), waitCalls.Load())
			}
			if out.stats.Rows != 0 || out.stats.Attributes != 0 || out.stats.LogicalPayloadBytes != 0 || out.stats.Details["committed_batches"] != "0" {
				t.Fatalf("dropped maybe-committed source was acknowledged/double-counted: %+v", out.stats)
			}
			if d := out.stats.Diagnostics; d == nil || d.SourceRuns != 2 || d.SourceAttempts != 2 || d.SourceReplays != 0 || d.SourceTimeouts != 1 || d.SourceFreshRetries != 1 || d.CommittedRows != 0 || d.CommittedBatches != 0 {
				t.Fatalf("timeout/drop diagnostic population: %+v", d)
			}
			observerMu.Lock()
			calls, observed := append([]recordlayer.AttemptCall(nil), observedCalls...), append([]error(nil), observedErrors...)
			observerMu.Unlock()
			if len(calls) != 3 || len(observed) != 3 {
				t.Fatalf("timeout replay replaced external attempt observer: calls=%+v errors=%v", calls, observed)
			}
			ids := map[uint64]bool{}
			for _, call := range calls {
				if call.CallID == 0 || ids[call.CallID] || call.Route != recordlayer.RouteAttempt || call.Owner != "run" {
					t.Fatalf("timeout replay attempt identity lost: %+v", calls)
				}
				ids[call.CallID] = true
			}
			var reported fdb.Error
			if observed[0] != nil || !errors.As(observed[1], &reported) || reported.Code != 1031 || !errors.As(observed[2], &ownership) {
				t.Fatalf("external observer did not retain init/timeout/ownership causes: %v", observed)
			}
			if after := otelTimeoutDropRoot(t, ctx, b); len(after) != 0 {
				t.Fatalf("fresh replay resurrected dropped owner/source/index state: %v", after)
			}
			if after := otelTimeoutDropRoot(t, ctx, sibling); !reflect.DeepEqual(after, siblingBefore) {
				t.Fatal("failed fresh replay modified sibling namespace")
			}
		})
	}
}
