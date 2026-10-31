package otel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

type otelTimeoutOrderState struct {
	mu                sync.Mutex
	role              string
	transaction       fdb.WritableTransaction
	timeouts, retries []int64
	armed, firstArmed int64
	firstRead         string
	reads             int
	setError          error
	readTimeout       bool
}

func (s *otelTimeoutOrderState) read(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reads == 0 {
		s.firstRead, s.firstArmed = method, s.armed
	}
	s.reads++
}

type otelTimeoutOrderOptions struct {
	fdb.TransactionOptions
	state *otelTimeoutOrderState
}

func (o otelTimeoutOrderOptions) SetTimeout(milliseconds int64) error {
	o.state.mu.Lock()
	o.state.timeouts = append(o.state.timeouts, milliseconds)
	injected := o.state.setError
	o.state.mu.Unlock()
	if injected != nil {
		return injected
	}
	if err := o.TransactionOptions.SetTimeout(milliseconds); err != nil {
		return err
	}
	o.state.mu.Lock()
	o.state.armed = milliseconds
	o.state.mu.Unlock()
	return nil
}

func (o otelTimeoutOrderOptions) SetRetryLimit(limit int64) error {
	o.state.mu.Lock()
	o.state.retries = append(o.state.retries, limit)
	o.state.mu.Unlock()
	return o.TransactionOptions.SetRetryLimit(limit)
}

type otelTimeoutOrderFuture struct {
	fdb.FutureByteSlice
	err error
}

func (f otelTimeoutOrderFuture) Get() ([]byte, error) { return nil, f.err }

type otelTimeoutOrderTransaction struct {
	fdb.WritableTransaction
	state *otelTimeoutOrderState
}

func (tx otelTimeoutOrderTransaction) Options() fdb.TransactionOptions {
	return otelTimeoutOrderOptions{TransactionOptions: tx.WritableTransaction.Options(), state: tx.state}
}

func (tx otelTimeoutOrderTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	tx.state.read("Get")
	future := tx.WritableTransaction.Get(key)
	if tx.state.readTimeout {
		return otelTimeoutOrderFuture{FutureByteSlice: future, err: fdb.Error{Code: 1031}}
	}
	return future
}

func (tx otelTimeoutOrderTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.state.read("GetRange")
	return tx.WritableTransaction.GetRange(r, options)
}

func (tx otelTimeoutOrderTransaction) GetReadVersion() fdb.FutureInt64 {
	tx.state.read("GetReadVersion")
	return tx.WritableTransaction.GetReadVersion()
}

type otelTimeoutOrderTransactor struct {
	fdb.Database
	parent           context.Context
	initSetError     error
	sourceTimeouts   int64
	parents, sources atomic.Int64
	mu               sync.Mutex
	states           []*otelTimeoutOrderState
}

func (d *otelTimeoutOrderTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *otelTimeoutOrderTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	role := "source"
	var source int64
	if ctx == d.parent {
		role = "ready"
		if d.parents.Add(1) == 1 {
			role = "initialization"
		}
	} else {
		source = d.sources.Add(1)
	}
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		state := &otelTimeoutOrderState{role: role, transaction: tx, readTimeout: role == "source" && source <= d.sourceTimeouts}
		if role == "initialization" {
			state.setError = d.initSetError
		}
		d.mu.Lock()
		d.states = append(d.states, state)
		d.mu.Unlock()
		return fn(otelTimeoutOrderTransaction{WritableTransaction: tx, state: state})
	})
}

func testOtelIngestTimeoutOrdering(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, scenario := range []string{"all_roles", "fresh_source_runs", "timeout_option_error"} {
		t.Run(scenario, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Attributes, c.Batch, c.Workers, c.Diagnostics, c.Order = 1, 1, 4, 1, 1, true, order
			b := openOtelFixture(t, ctx, cluster, c, false)
			cause := errors.New("initial transaction timeout option rejected")
			wrapped := &otelTimeoutOrderTransactor{Database: b.raw, parent: ctx}
			if scenario == "fresh_source_runs" {
				wrapped.sourceTimeouts = 2
			}
			if scenario == "timeout_option_error" {
				wrapped.initSetError = cause
			}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(wrapped, b.raw)
			defer func() { b.db, b.ingestWait = original, nil }()
			var waits []time.Duration
			b.ingestWait = func(waitCtx context.Context, delay time.Duration) error {
				if waitCtx.Err() != nil {
					return waitCtx.Err()
				}
				waits = append(waits, delay)
				return nil
			}
			var observerMu sync.Mutex
			var calls []recordlayer.AttemptCall
			var observedErrors []error
			b.db.SetAttemptObserver(func(call recordlayer.AttemptCall, err error) {
				observerMu.Lock()
				calls, observedErrors = append(calls, call), append(observedErrors, err)
				observerMu.Unlock()
			})
			stats, err := b.Ingest(ctx)
			b.db.SetAttemptObserver(nil)
			wrapped.mu.Lock()
			states := append([]*otelTimeoutOrderState(nil), wrapped.states...)
			wrapped.mu.Unlock()
			if scenario == "timeout_option_error" {
				if !errors.Is(err, cause) || len(states) != 1 || wrapped.sources.Load() != 0 || len(waits) != 0 || stats.Rows != 0 {
					t.Fatalf("initial timeout option failure reached source/retry: states=%d source=%d waits=%v stats=%+v err=%v", len(states), wrapped.sources.Load(), waits, stats, err)
				}
				state := states[0]
				state.mu.Lock()
				reads, armed, values := state.reads, state.armed, append([]int64(nil), state.timeouts...)
				state.mu.Unlock()
				if reads != 0 || armed != 0 || len(values) != 1 || values[0] != 1500 {
					t.Fatalf("failed timeout option did not precede first read: reads=%d armed=%d values=%v", reads, armed, values)
				}
				b.db = original
				if snapshot := otelRawRootSnapshot(t, ctx, b); len(snapshot) != 0 {
					t.Fatalf("failed initial option left owner/source/index state: %v", snapshot)
				}
				return
			}
			if err != nil || stats.Rows != 1 || stats.Attributes != 4 {
				t.Fatalf("timeout-order ingest: stats=%+v err=%v", stats, err)
			}
			wantSources := int64(1)
			if scenario == "fresh_source_runs" {
				wantSources = 3
			}
			if wrapped.parents.Load() != 2 || wrapped.sources.Load() != wantSources || len(states) != int(wantSources)+2 {
				t.Fatalf("actual initialization/source/ready population parents=%d sources=%d states=%d", wrapped.parents.Load(), wrapped.sources.Load(), len(states))
			}
			roles := map[string]int{}
			transactions := map[fdb.WritableTransaction]bool{}
			for _, state := range states {
				state.mu.Lock()
				role, first, firstArmed, reads := state.role, state.firstRead, state.firstArmed, state.reads
				timeouts, retries := append([]int64(nil), state.timeouts...), append([]int64(nil), state.retries...)
				state.mu.Unlock()
				roles[role]++
				if transactions[state.transaction] {
					t.Fatalf("fresh source Run reused an actual transaction: role=%s", role)
				}
				transactions[state.transaction] = true
				if reads < 1 || first == "" || firstArmed != 1500 || len(timeouts) == 0 || timeouts[0] != 1500 {
					t.Fatalf("%s first read %s was not armed1500: reads=%d armed=%d options=%v", role, first, reads, firstArmed, timeouts)
				}
				if role == "initialization" || role == "source" && !state.readTimeout {
					foundLegacyLimit := false
					for _, limit := range retries {
						if limit == 10 {
							foundLegacyLimit = true
						}
					}
					minimumTimeouts := 1
					if role == "initialization" {
						minimumTimeouts = 2
					}
					if !foundLegacyLimit || len(timeouts) < minimumTimeouts {
						t.Fatalf("%s lost existing open timeout/retry guard: timeouts=%v retries=%v", role, timeouts, retries)
					}
				}
			}
			if roles["initialization"] != 1 || roles["ready"] != 1 || roles["source"] != int(wantSources) {
				t.Fatalf("missing positive timeout-order role: %v", roles)
			}
			observerMu.Lock()
			observedCalls := append([]recordlayer.AttemptCall(nil), calls...)
			observed := append([]error(nil), observedErrors...)
			observerMu.Unlock()
			if len(observedCalls) != len(states) {
				t.Fatalf("external attempt observer replaced/bypassed: calls=%d actual=%d", len(observedCalls), len(states))
			}
			ids := map[uint64]bool{}
			timeouts := 0
			for i, call := range observedCalls {
				if call.CallID == 0 || ids[call.CallID] || call.Owner != "run" || call.Route != recordlayer.RouteAttempt {
					t.Fatalf("source fresh retry reused/lost external attempt identity: %+v", observedCalls)
				}
				ids[call.CallID] = true
				var code fdb.Error
				if errors.As(observed[i], &code) && code.Code == 1031 {
					timeouts++
				}
			}
			wantTimeouts := int64(0)
			if scenario == "fresh_source_runs" {
				wantTimeouts = 2
				if len(waits) != 2 || waits[0] != 25*time.Millisecond || waits[1] != 50*time.Millisecond {
					t.Fatalf("fresh source timeout backoff=%v", waits)
				}
			} else if len(waits) != 0 {
				t.Fatalf("successful source unexpectedly backed off: %v", waits)
			}
			if int64(timeouts) != wantTimeouts {
				t.Fatalf("external observer timeout population=%d want=%d", timeouts, wantTimeouts)
			}
			if d := stats.Diagnostics; d == nil || d.SourceRuns != wantSources || d.SourceAttempts != wantSources || d.SourceReplays != 0 || d.SourceTimeouts != wantTimeouts || d.SourceFreshRetries != wantTimeouts {
				t.Fatalf("fresh-run timeout diagnostics=%+v", d)
			}
			b.db, b.ingestWait = original, nil
			if audited, err := b.Audit(ctx); err != nil || audited.Rows != 1 {
				t.Fatalf("timeout-order source audit: %+v %v", audited, err)
			}
		})
	}
}
