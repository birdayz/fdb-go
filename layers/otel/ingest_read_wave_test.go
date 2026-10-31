package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

type ingestReadWaveAttempt struct {
	timeout, retries                            int64
	reads, owners, headers, states, resolutions int
	recordWrites, headerWrites                  int
	prefetches, resolved, normalRanges          int
	expectedPrefetches                          int
	storageStart, storageBeforeSave, storageEnd int64
	problems                                    []string
}

func (s *ingestReadWaveAttempt) read() {
	if s.timeout != 1500 || s.retries != 10 {
		s.problems = append(s.problems, "read before timeout1500/retry10")
	}
	s.reads++
}

type ingestReadWaveOptions struct {
	fdb.TransactionOptions
	state *ingestReadWaveAttempt
}

func (o ingestReadWaveOptions) SetTimeout(n int64) error {
	if o.state.reads != 0 {
		o.state.problems = append(o.state.problems, "timeout option after read issuance")
	}
	o.state.timeout = n
	return o.TransactionOptions.SetTimeout(n)
}

func (o ingestReadWaveOptions) SetRetryLimit(n int64) error {
	if o.state.reads != 0 {
		o.state.problems = append(o.state.problems, "retry option after read issuance")
	}
	o.state.retries = n
	return o.TransactionOptions.SetRetryLimit(n)
}

type ingestReadWaveFuture struct {
	fdb.FutureByteSlice
	state *ingestReadWaveAttempt
	owner bool
	err   error
}

func (f ingestReadWaveFuture) Get() ([]byte, error) {
	if f.owner {
		f.state.resolutions++
		// A causal barrier, not a timing assertion: the serial implementation
		// cannot resolve this owner future before issuing both metadata reads.
		if f.state.headers != 1 || f.state.states != 1 || f.state.prefetches != f.state.expectedPrefetches {
			f.state.problems = append(f.state.problems, "owner awaited before store metadata reads")
			return nil, errors.New("owner future crossed metadata issuance barrier")
		}
	}
	wire, err := f.FutureByteSlice.Get()
	if f.err != nil {
		return nil, f.err
	}
	return wire, err
}

type ingestReadWaveTransaction struct {
	fdb.WritableTransaction
	driver *ingestReadWaveTransactor
	state  *ingestReadWaveAttempt
}

func (tx ingestReadWaveTransaction) Options() fdb.TransactionOptions {
	return ingestReadWaveOptions{TransactionOptions: tx.WritableTransaction.Options(), state: tx.state}
}

func (tx ingestReadWaveTransaction) Get(key fdb.KeyConvertible) fdb.FutureByteSlice {
	tx.state.read()
	future := tx.WritableTransaction.Get(key)
	if bytes.Equal(key.FDBKey(), tx.driver.owner) {
		tx.state.owners++
		return ingestReadWaveFuture{FutureByteSlice: future, state: tx.state, owner: true, err: tx.driver.ownerError}
	}
	if bytes.Equal(key.FDBKey(), tx.driver.header) {
		tx.state.headers++
		return ingestReadWaveFuture{FutureByteSlice: future, state: tx.state, err: tx.driver.openError}
	}
	return future
}

func (tx ingestReadWaveTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.state.read()
	if exact, ok := r.(fdb.ExactRange); ok {
		begin, _ := exact.FDBRangeKeys()
		if bytes.HasPrefix(begin.FDBKey(), tx.driver.records) {
			if tx.state.resolved != tx.state.expectedPrefetches {
				tx.state.problems = append(tx.state.problems, "normal save before all prefetch resolutions")
			}
			if tx.state.normalRanges == 0 {
				tx.state.storageBeforeSave = tx.driver.Database.Metrics().TransactionGetRangeRequests
			}
			tx.state.normalRanges++
		}
	}
	return tx.WritableTransaction.GetRange(r, options)
}

func (tx ingestReadWaveTransaction) Snapshot() fdb.ReadTransaction {
	return ingestReadWaveSnapshot{ReadTransaction: tx.WritableTransaction.Snapshot(), driver: tx.driver, state: tx.state}
}

func (tx ingestReadWaveTransaction) Set(key fdb.KeyConvertible, value []byte) {
	if bytes.Equal(key.FDBKey(), tx.driver.header) {
		tx.state.headerWrites++
	}
	if bytes.HasPrefix(key.FDBKey(), tx.driver.records) {
		tx.state.recordWrites++
	}
	tx.WritableTransaction.Set(key, value)
}

func (tx ingestReadWaveTransaction) SetBytes(key, value []byte) {
	if bytes.HasPrefix(key, tx.driver.records) {
		tx.state.recordWrites++
	}
	tx.WritableTransaction.SetBytes(key, value)
}

type ingestReadWaveSnapshot struct {
	fdb.ReadTransaction
	driver *ingestReadWaveTransactor
	state  *ingestReadWaveAttempt
}

type ingestReadWaveRange struct {
	fdb.RangeResult
	state  *ingestReadWaveAttempt
	driver *ingestReadWaveTransactor
}

func (r ingestReadWaveRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	rows, err := r.RangeResult.GetSliceWithError()
	r.state.resolved++
	if r.driver.afterPrefetch != nil {
		r.driver.afterPrefetch(r.state)
	}
	if r.driver.prefetchError != nil {
		return nil, r.driver.prefetchError
	}
	return rows, err
}

func (tx ingestReadWaveSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	tx.state.read()
	isRecord := false
	if exact, ok := r.(fdb.ExactRange); ok {
		begin, end := exact.FDBRangeKeys()
		if bytes.Equal(begin.FDBKey(), tx.driver.stateBegin) && bytes.Equal(end.FDBKey(), tx.driver.stateEnd) {
			tx.state.states++
		}
		isRecord = bytes.HasPrefix(begin.FDBKey(), tx.driver.records)
		if isRecord {
			if tx.state.headers != 0 || tx.state.states != 0 {
				tx.state.problems = append(tx.state.problems, "prefetch issued after store-open metadata wave")
			}
			tx.state.prefetches++
		}
	}
	result := tx.ReadTransaction.GetRange(r, options)
	if isRecord {
		return ingestReadWaveRange{RangeResult: result, state: tx.state, driver: tx.driver}
	}
	return result
}

type ingestReadWaveTransactor struct {
	fdb.Database
	parent                               context.Context
	owner, header, records               []byte
	stateBegin, stateEnd                 []byte
	ownerError, openError, prefetchError error
	afterPrefetch                        func(*ingestReadWaveAttempt)
	beforeAttempt                        func(int) error
	injectCode, count                    int
	attempts                             []*ingestReadWaveAttempt
}

func newIngestReadWaveTransactor(b *FDBBackend, parent context.Context) *ingestReadWaveTransactor {
	begin, end := b.store.Sub(recordlayer.IndexStateSpaceKey).FDBRangeKeys()
	return &ingestReadWaveTransactor{
		Database: b.raw, parent: parent, count: min(b.cfg.Batch, b.cfg.Count),
		owner: b.root.Sub("owner").FDBKey(), header: b.store.Pack(tuple.Tuple{recordlayer.StoreInfoKey}),
		records: b.store.Sub(recordlayer.RecordKey).Bytes(), stateBegin: begin.FDBKey(), stateEnd: end.FDBKey(),
	}
}

func (d *ingestReadWaveTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *ingestReadWaveTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	if ctx == d.parent {
		return d.Database.TransactCtx(ctx, fn)
	}
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		if d.beforeAttempt != nil {
			if err := d.beforeAttempt(len(d.attempts) + 1); err != nil {
				return nil, err
			}
		}
		state := &ingestReadWaveAttempt{expectedPrefetches: d.count, storageStart: d.Database.Metrics().TransactionGetRangeRequests}
		d.attempts = append(d.attempts, state)
		result, err := fn(ingestReadWaveTransaction{WritableTransaction: tx, driver: d, state: state})
		state.storageEnd = d.Database.Metrics().TransactionGetRangeRequests
		if len(state.problems) != 0 {
			return nil, fmt.Errorf("source read wave: %v", state.problems)
		}
		if err == nil && len(d.attempts) == 1 && d.injectCode != 0 {
			return nil, fdb.Error{Code: d.injectCode}
		}
		return result, err
	})
}

func assertIngestReadWave(t *testing.T, attempts []*ingestReadWaveAttempt, count int) {
	t.Helper()
	if len(attempts) != count {
		t.Fatalf("read-wave attempts=%d want=%d", len(attempts), count)
	}
	for i, state := range attempts {
		if len(state.problems) != 0 || state.owners != 1 || state.headers != 1 || state.states != 1 || state.resolutions != 1 || state.timeout != 1500 || state.retries != 10 {
			t.Fatalf("attempt %d lost authoritative pipelined reads/options: %+v", i, state)
		}
	}
}

func testOtelIngestReadWave(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, code := range []int{0, 1020, 1031} {
		t.Run(fmt.Sprintf("retry_%d", code), func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.SaveMode, c.Order = 2, 1, 2, 1, true, "batch", order
			b := openOtelFixture(t, ctx, cluster, c, false)
			wrapped := newIngestReadWaveTransactor(b, ctx)
			wrapped.injectCode = code
			original := b.db
			if code != 0 {
				_, err := original.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := b.open(rc)
					if err != nil {
						return nil, err
					}
					for ordinal := 0; ordinal < 2; ordinal++ {
						old := Generate(b.cfg, ordinal)
						old.Start += 1000000
						old.Attributes[0] = Attribute{"span", "replay.old", "string", "initial"}
						message, err := toRecord(old)
						if err != nil {
							return nil, err
						}
						if _, err := store.SaveRecord(message); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				wrapped.beforeAttempt = func(attempt int) error {
					if attempt != 2 {
						return nil
					}
					_, err := original.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
						store, err := b.open(rc)
						if err != nil {
							return nil, err
						}
						old := Generate(b.cfg, 0)
						old.Start += 2000000
						old.Attributes[0] = Attribute{"span", "replay.old", "string", "changed-between-attempts"}
						message, err := toRecord(old)
						if err != nil {
							return nil, err
						}
						if _, err := store.SaveRecord(message); err != nil {
							return nil, err
						}
						_, err = store.DeleteRecord(otelPK(Generate(b.cfg, 1)))
						return nil, err
					})
					return err
				}
			}
			b.db = recordlayer.NewFDBDatabaseWithTransactor(wrapped, b.raw)
			defer func() { b.db = original }()
			stats, err := b.Ingest(ctx)
			if err != nil || stats.Rows != 2 || stats.Attributes != 16 {
				t.Fatalf("pipelined ingest: stats=%+v err=%v", stats, err)
			}
			want := 1
			if code != 0 {
				want = 2
			}
			assertIngestReadWave(t, wrapped.attempts, want)
			for _, state := range wrapped.attempts {
				if state.recordWrites != 2 || state.reads != 7 || state.prefetches != 2 || state.resolved != 2 || state.normalRanges != 2 {
					t.Fatalf("attempt changed authoritative prefetch/normal-save population: %+v", state)
				}
				if state.storageBeforeSave-state.storageStart < 3 || state.storageEnd != state.storageBeforeSave {
					t.Fatalf("same-transaction cache did not eliminate normal save storage reads: %+v", state)
				}
			}
			d := stats.Diagnostics
			if d == nil || d.SourceAttempts != int64(want) || d.CommittedBatches != 1 || d.PhaseLatencies["manifest"].Count != int64(want) || d.PhaseLatencies["open"].Count != int64(want) {
				t.Fatalf("pipelined phase populations: %+v", d)
			}
			if code == 1020 && (d.SourceRuns != 1 || d.SourceReplays != 1 || d.SourceFreshRetries != 0) {
				t.Fatalf("internal replay changed: %+v", d)
			}
			if code == 1031 && (d.SourceRuns != 2 || d.SourceReplays != 0 || d.SourceTimeouts != 1 || d.SourceFreshRetries != 1) {
				t.Fatalf("fresh1031 recovery changed: %+v", d)
			}
			b.db = original
			assertOtelCommittedPostings(t, ctx, b, []Span{Generate(b.cfg, 0), Generate(b.cfg, 1)})
			if audited, err := b.Audit(ctx); err != nil || audited.Rows != 2 {
				t.Fatalf("pipelined durable census: %+v %v", audited, err)
			}
		})
	}
	for _, scenario := range []string{"missing_owner", "malformed_owner", "mismatched_owner", "owner_error_and_open_error", "valid_owner_open_error"} {
		t.Run(scenario, func(t *testing.T) {
			c := testConfig()
			c.Count, c.Tenants, c.Batch, c.Workers, c.Diagnostics, c.SaveMode, c.Order = 1, 1, 1, 1, true, "batch", order
			b := openOtelFixture(t, ctx, cluster, c, false)
			ownerError, openError := errors.New("owner read failed"), errors.New("header read failed")
			wrapped := newIngestReadWaveTransactor(b, ctx)
			if scenario == "owner_error_and_open_error" {
				wrapped.ownerError, wrapped.openError = ownerError, openError
			}
			if scenario == "malformed_owner" || scenario == "mismatched_owner" || scenario == "valid_owner_open_error" {
				wire := []byte("invalid manifest JSON")
				if scenario != "malformed_owner" {
					m := manifest(b.cfg, "loading")
					if scenario == "mismatched_owner" {
						m.Corpus = "not-the-approved-source"
					}
					var err error
					wire, err = json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "valid_owner_open_error" {
						wrapped.openError = openError
					}
				}
				if _, err := b.raw.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
					tx.Set(b.root.Sub("owner").FDBKey(), wire)
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := otelRawRootSnapshot(t, ctx, b)
			batch, err := prepareIngestBatch(ctx, b.cfg, 0)
			if err != nil {
				t.Fatal(err)
			}
			original := b.db
			b.db = recordlayer.NewFDBDatabaseWithTransactor(wrapped, b.raw)
			defer func() { b.db = original }()
			sourceCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			d := newIngestRecorder(true)
			err = b.saveIngestBatch(sourceCtx, batch, d)
			if err == nil {
				t.Fatal("failed ownership/open was accepted")
			}
			if scenario == "owner_error_and_open_error" && (!errors.Is(err, ownerError) || errors.Is(err, openError)) {
				t.Fatalf("owner error lost precedence: %v", err)
			}
			if scenario == "valid_owner_open_error" && !errors.Is(err, openError) {
				t.Fatalf("valid owner masked store-open failure: %v", err)
			}
			assertIngestReadWave(t, wrapped.attempts, 1)
			if (scenario == "missing_owner" || scenario == "malformed_owner" || scenario == "mismatched_owner") && wrapped.attempts[0].headerWrites != 1 {
				t.Fatalf("owner rejection did not exercise transaction-local store creation: %+v", wrapped.attempts[0])
			}
			if wrapped.attempts[0].recordWrites != 0 || d.snapshot(0, 0).PhaseLatencies["save"].Count != 0 || d.out.PhaseLatencies["guard"].Count != 0 || d.out.PhaseLatencies["commit"].Count != 0 {
				t.Fatalf("failed ownership/open reached source save/guard/commit: attempt=%+v profile=%+v", wrapped.attempts[0], d.out)
			}
			b.db = original
			if after := otelRawRootSnapshot(t, ctx, b); !reflect.DeepEqual(before, after) {
				t.Fatalf("failed ownership/open persisted create/rebuild/source changes: before=%v after=%v", before, after)
			}
		})
	}
}
