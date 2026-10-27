package otel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

type queryReplayState struct {
	entries          int
	injected         int
	successfulBodies int
	guardFailures    int
	work             []QueryResult
}

type queryReplayTransactor struct {
	fdb.Database
	backend      *FDBBackend
	state        *queryReplayState
	beforeReplay func() error
	guardKey     fdb.Key
	cause        error
}

type queryReplayTransaction struct {
	fdb.WritableTransaction
	state *queryReplayState
	key   fdb.Key
	cause error
}

func (tx queryReplayTransaction) AddReadConflictKey(key fdb.KeyConvertible) error {
	if string(key.FDBKey()) == string(tx.key) {
		tx.state.guardFailures++
		return tx.cause
	}
	return tx.WritableTransaction.AddReadConflictKey(key)
}

func (d queryReplayTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d queryReplayTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		d.state.entries++
		attempt := d.state.entries
		if attempt > 2 {
			return nil, fmt.Errorf("public query replay exceeded two closure entries: %d", attempt)
		}
		if attempt == 2 {
			if err := d.beforeReplay(); err != nil {
				return nil, err
			}
			if d.cause != nil {
				tx = queryReplayTransaction{WritableTransaction: tx, state: d.state, key: d.guardKey, cause: d.cause}
			}
		}
		result, err := fn(tx)
		work := d.backend.LastQueryWork()
		work.Hits = append([]Hit(nil), work.Hits...)
		d.state.work = append(d.state.work, work)
		if err != nil {
			return result, err
		}
		d.state.successfulBodies++
		if work.Candidates <= 0 || work.Bytes <= 0 {
			return nil, fmt.Errorf("public query closure %d succeeded without consumed candidates/bytes: %+v", attempt, work)
		}
		if attempt == 1 {
			d.state.injected++
			// The real query body consumed its index entries successfully. Abort
			// before commit so the native client re-enters that same public body.
			return nil, fdb.Error{Code: 1020}
		}
		return result, nil
	})
}

// The only custom-index rows in this fixture are its two literal source
// attributes. Count raw bytes without interpreting the backend's posting keys.
func queryReplayRawBytes(t *testing.T, ctx context.Context, b *FDBBackend, kind string) int64 {
	t.Helper()
	if kind == "span" {
		return intrinsicRawBytes(t, ctx, b, spanIndexName, recordlayer.TupleRangeAllOf(tuple.Tuple{int64(0), "0000000000000001"}))
	}
	if kind == "time" {
		return intrinsicRawBytes(t, ctx, b, timeIndexName, recordlayer.TupleRangeBetween(tuple.Tuple{int64(0), int64(0)}, tuple.Tuple{int64(0), int64(200)}))
	}
	var rows []fdb.KeyValue
	_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		var err error
		rows, err = tx.GetRange(otelSecondary(b), fdb.RangeOptions{Limit: 3}).GetSliceWithError()
		return nil, err
	})
	if err != nil || len(rows) != 2 {
		t.Fatalf("literal attribute raw census: rows=%d err=%v", len(rows), err)
	}
	var total int64
	for _, row := range rows {
		total += int64(len(row.Key) + len(row.Value))
	}
	return total
}

func testOtelPublicQueryReplay(t *testing.T, ctx context.Context, cluster, order string) {
	t.Helper()
	for _, kind := range []string{"query", "span", "time"} {
		for _, failGuard := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/state_error_%t", kind, failGuard), func(t *testing.T) {
				c := testConfig()
				c.Count, c.Tenants, c.K, c.Order = 2, 1, 2, order
				b := openOtelFixture(t, ctx, cluster, c, true)
				otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
					return store.DeleteAllRecords()
				})
				const traceA = "00000000000000000000000000000001"
				const traceB = "00000000000000000000000000000002"
				const sharedSpan = "0000000000000001"
				attribute := Attribute{Scope: "span", Key: "replay.shared", Type: "string", Value: "literal"}
				initial := []Span{
					{Ordinal: 0, Tenant: 0, Trace: traceA, ID: sharedSpan, Start: 100, Attributes: []Attribute{attribute}},
					{Ordinal: 1, Tenant: 0, Trace: traceB, ID: sharedSpan, Start: 90, Attributes: []Attribute{attribute}},
				}
				final := []Span{
					{Ordinal: 0, Tenant: 0, Trace: traceA, ID: sharedSpan, Start: 80, Attributes: []Attribute{attribute}},
					{Ordinal: 1, Tenant: 0, Trace: traceB, ID: sharedSpan, Start: 120, Attributes: []Attribute{attribute}},
				}
				for _, source := range initial {
					saveOtelSpan(t, ctx, b, source)
				}
				firstBytes := queryReplayRawBytes(t, ctx, b, kind)
				if firstBytes <= 0 {
					t.Fatal("first-attempt independent byte census is empty")
				}
				original := b.db
				defer func() { b.db = original }()
				state := &queryReplayState{}
				var secondBytes int64
				var cause error
				if failGuard {
					cause = errors.New("second public query attempt index-state conflict read failed")
				}
				index := b.idx.Name
				if kind == "span" {
					index = spanIndexName
				} else if kind == "time" {
					index = timeIndexName
				}
				b.db = recordlayer.NewFDBDatabaseWithTransactor(queryReplayTransactor{
					Database: b.raw, backend: b, state: state,
					guardKey: b.store.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{index}), cause: cause,
					beforeReplay: func() error {
						// Use the captured original database, never the replay wrapper.
						// Public SaveRecord updates the same PKs and all maintained indexes.
						_, err := original.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
							store, err := b.open(rc)
							if err != nil {
								return nil, err
							}
							for _, source := range final {
								message, err := toRecord(source)
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
							return err
						}
						secondBytes = queryReplayRawBytes(t, ctx, b, kind)
						return nil
					},
				}, b.raw)
				var got QueryResult
				var matches []SpanMatch
				var err error
				switch kind {
				case "query":
					got, err = b.Query(ctx, attribute)
				case "span":
					matches, err = b.Span(ctx, sharedSpan)
				case "time":
					got, err = b.Time(ctx, TimeRange{Start: 0, End: 200})
				}
				b.db = original
				if ctx.Err() != nil {
					t.Fatalf("query replay canceled its live parent: %v", ctx.Err())
				}
				wantSuccesses, wantFailures := 2, 0
				if failGuard {
					wantSuccesses, wantFailures = 1, 1
				}
				if state.entries != 2 || state.injected != 1 || state.successfulBodies != wantSuccesses || state.guardFailures != wantFailures || len(state.work) != 2 || secondBytes <= 0 {
					t.Fatalf("replay population: %+v secondBytes=%d err=%v", state, secondBytes, err)
				}
				if work := state.work[0]; work.Candidates != 2 || work.Bytes != firstBytes {
					t.Fatalf("first successful body did not consume exactly the literal source population: %+v want 2/%d", work, firstBytes)
				}
				if kind == "query" && !reflect.DeepEqual(state.work[0].Hits, []Hit{{Trace: traceA, Latest: 100}, {Trace: traceB, Latest: 90}}) {
					t.Fatalf("first equality body did not produce its original witnesses: %+v", state.work[0])
				}
				work := b.LastQueryWork()
				if failGuard {
					if !errors.Is(err, cause) || len(matches) != 0 || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
						t.Fatalf("second-attempt guard failure retained output or lost sentinel: matches=%+v result=%+v err=%v", matches, got, err)
					}
					if len(work.Hits) != 0 || work.Candidates != 0 || work.Bytes != 0 || len(state.work[1].Hits) != 0 || state.work[1].Candidates != 0 || state.work[1].Bytes != 0 {
						t.Fatalf("second-attempt guard failure retained first-attempt work: published=%+v attempt=%+v", work, state.work[1])
					}
				} else {
					if err != nil {
						t.Fatalf("public query replay failed: %v", err)
					}
					if kind == "span" {
						want := []SpanMatch{{Trace: traceA, ID: sharedSpan, Start: 80}, {Trace: traceB, ID: sharedSpan, Start: 120}}
						if !reflect.DeepEqual(matches, want) {
							t.Fatalf("Span retained failed-attempt matches/order: got=%+v want=%+v", matches, want)
						}
					} else {
						want := []Hit{{Trace: traceB, Latest: 120}, {Trace: traceA, Latest: 80}}
						if !reflect.DeepEqual(got.Hits, want) || got.Candidates != 2 || got.Bytes != secondBytes {
							t.Fatalf("replay retained failed-attempt hits/witnesses/work: got=%+v want=%+v 2/%d", got, want, secondBytes)
						}
						if kind == "query" && (!reflect.DeepEqual(work.Hits, want) || !reflect.DeepEqual(state.work[1].Hits, want)) {
							t.Fatalf("equality replay published stale work witnesses: published=%+v attempt=%+v want=%+v", work, state.work[1], want)
						}
					}
					if work.Candidates != 2 || work.Bytes != secondBytes || state.work[1].Candidates != 2 || state.work[1].Bytes != secondBytes {
						t.Fatalf("replay did not publish exact second-only work: published=%+v attempt=%+v want 2/%d", work, state.work[1], secondBytes)
					}
				}
				for _, source := range final {
					assertOtelSource(t, ctx, b, source)
				}
			})
		}
	}
}
