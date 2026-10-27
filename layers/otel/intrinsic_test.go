package otel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

func TestIntrinsicMatchMetadataAndMalformedEntries(t *testing.T) {
	t.Parallel()
	c := testConfig()
	md, _, err := metadata(c)
	if err != nil {
		t.Fatal(err)
	}
	trace, span := strings.Repeat("a", 32), strings.Repeat("b", 16)
	for _, name := range []string{spanIndexName, timeIndexName} {
		t.Run(name, func(t *testing.T) {
			idx := md.GetIndex(name)
			if idx == nil {
				t.Fatal("intrinsic index metadata missing")
			}
			key := tuple.Tuple{int64(0), span, int64(123), trace}
			wantPositions := []int{0, -1, 1}
			tracePosition, spanPosition, timePosition := 3, 1, 2
			if name == timeIndexName {
				key = tuple.Tuple{int64(0), int64(123), trace, span}
				wantPositions = []int{0, 2, -1}
				tracePosition, spanPosition, timePosition = 2, 3, 1
			}
			if !reflect.DeepEqual(idx.PrimaryKeyComponentPositions(), wantPositions) {
				t.Fatalf("metadata PK overlap: %v want %v", idx.PrimaryKeyComponentPositions(), wantPositions)
			}
			entry := &recordlayer.IndexEntry{Index: idx, Key: key}
			got, err := intrinsicMatch(entry, name, 0)
			if err != nil || got != (SpanMatch{trace, span, 123}) || !reflect.DeepEqual(entry.PrimaryKey(), tuple.Tuple{int64(0), trace, span}) {
				t.Fatalf("covering metadata extraction: %+v %v pk=%v", got, err, entry.PrimaryKey())
			}
			cases := []struct {
				name      string
				entry     *recordlayer.IndexEntry
				requested string
				tenant    int
			}{
				{"nil_entry", nil, name, 0},
				{"nil_index", &recordlayer.IndexEntry{Key: key}, name, 0},
				{"wrong_index_name", &recordlayer.IndexEntry{Index: recordlayer.NewIndex("other", idx.RootExpression), Key: key}, name, 0},
				{"short_key", &recordlayer.IndexEntry{Index: idx, Key: key[:3]}, name, 0},
				{"long_key", &recordlayer.IndexEntry{Index: idx, Key: append(append(tuple.Tuple(nil), key...), int64(7))}, name, 0},
				{"nonempty_value", &recordlayer.IndexEntry{Index: idx, Key: key, Value: tuple.Tuple{int64(7)}}, name, 0},
				{"missing_pk_mapping", &recordlayer.IndexEntry{Index: recordlayer.NewIndex(name, idx.RootExpression), Key: key}, name, 0},
				{"wrong_column_count", &recordlayer.IndexEntry{Index: recordlayer.NewIndex(name, recordlayer.Field("tenant")), Key: key}, name, 0},
				{"other_tenant", &recordlayer.IndexEntry{Index: idx, Key: key}, name, 1},
				{"unknown_shape", &recordlayer.IndexEntry{Index: idx, Key: key}, "unknown_index", 0},
			}
			for _, mutation := range []struct {
				name     string
				position int
				value    any
			}{
				{"tenant_type", 0, "0"},
				{"trace_type", tracePosition, []byte(trace)},
				{"trace_length", tracePosition, "aa"},
				{"trace_hex", tracePosition, strings.Repeat("z", 32)},
				{"trace_zero", tracePosition, strings.Repeat("0", 32)},
				{"trace_uppercase", tracePosition, strings.Repeat("A", 32)},
				{"span_type", spanPosition, []byte(span)},
				{"span_length", spanPosition, "bb"},
				{"span_hex", spanPosition, strings.Repeat("z", 16)},
				{"span_zero", spanPosition, strings.Repeat("0", 16)},
				{"span_uppercase", spanPosition, strings.Repeat("B", 16)},
				{"timestamp_type", timePosition, "123"},
			} {
				changed := append(tuple.Tuple(nil), key...)
				changed[mutation.position] = mutation.value
				cases = append(cases, struct {
					name      string
					entry     *recordlayer.IndexEntry
					requested string
					tenant    int
				}{mutation.name, &recordlayer.IndexEntry{Index: idx, Key: changed}, name, 0})
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					_, err := intrinsicMatch(test.entry, test.requested, test.tenant)
					var corruption *CorruptionError
					if !errors.As(err, &corruption) {
						t.Fatalf("malformed covering entry: %T %v", err, err)
					}
				})
			}
		})
	}
}

func testOtelIntrinsicV2(t *testing.T, ctx context.Context, cluster, order string) {
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.K, c.Order = 8, 2, 8, 1, order
	b := openOtelFixture(t, ctx, cluster, c, true)
	c = b.cfg
	stats, err := b.Audit(ctx)
	if err != nil || stats.Rows != 8 || stats.Structures[spanIndexName].Keys != 8 || stats.Structures[timeIndexName].Keys != 8 {
		t.Fatalf("V2 built-in index census: %+v %v", stats, err)
	}
	first := Generate(c, 0)
	response := Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + first.ID}
	got, err := b.Query(ctx, response)
	if err != nil || !reflect.DeepEqual(got.Hits, []Hit{{first.Trace, first.Start}}) {
		t.Fatalf("GenAI response ID positive: %+v %v", got, err)
	}
	for _, a := range []Attribute{
		{"resource", response.Key, "string", response.Value},
		{"scope", response.Key, "string", response.Value},
		{"span", response.Key, "int", "42"},
		{"span", response.Key, "string", "missing_response_id"},
	} {
		got, err := b.Query(ctx, a)
		if err != nil || len(got.Hits) != 0 {
			t.Fatalf("GenAI scope/type/missing control %+v: %+v %v", a, got, err)
		}
	}
	t.Run("attribute_state_read_error", func(t *testing.T) {
		testOtelAttributeStateReadError(t, ctx, b, response, []Hit{{first.Trace, first.Start}})
	})
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		return store.DeleteAllRecords()
	})
	trace := func(n int) string { return fmt.Sprintf("%032x", n) }
	id := func(n int) string { return fmt.Sprintf("%016x", n) }
	common := id(1)
	sources := []Span{
		{Tenant: 0, Trace: trace(1), ID: common, Start: 100},
		{Tenant: 0, Trace: trace(2), ID: common, Start: 100},
		{Tenant: 0, Trace: trace(3), ID: common, Start: 90},
		{Tenant: 0, Trace: trace(1), ID: id(2), Start: 300},
		{Tenant: 0, Trace: trace(2), ID: id(3), Start: 199},
		{Tenant: 0, Trace: trace(4), ID: id(4), Start: 200},
		{Tenant: 0, Trace: trace(5), ID: id(5), Start: 99},
		{Tenant: 0, Trace: trace(6), ID: id(6), Start: 150},
		{Tenant: 0, Trace: trace(6), ID: id(7), Start: 150},
		{Tenant: 0, Trace: trace(8), ID: id(8), Start: 150},
		{Tenant: 1, Trace: trace(7), ID: common, Start: 120},
	}
	for i := range sources {
		sources[i].Ordinal = i
		saveOtelSpan(t, ctx, b, sources[i])
	}
	for _, name := range []string{spanIndexName, timeIndexName} {
		idx := b.md.GetIndex(name)
		size, err := b.size(ctx, b.store.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey()))
		if err != nil || size.Keys != int64(len(sources)) {
			t.Fatalf("explicit source built-in census %s: %+v %v", name, size, err)
		}
	}
	wantSpan := []SpanMatch{{trace(3), common, 90}, {trace(1), common, 100}, {trace(2), common, 100}}
	matches, err := b.Span(ctx, common)
	if err != nil || !reflect.DeepEqual(matches, wantSpan) {
		t.Fatalf("standalone Span must return all matches despite K=1: %+v want %+v: %v", matches, wantSpan, err)
	}
	spanWork := b.LastQueryWork()
	spanBytes := intrinsicRawBytes(t, ctx, b, spanIndexName, recordlayer.TupleRangeAllOf(tuple.Tuple{int64(0), common}))
	if spanWork.Candidates != 3 || spanWork.Bytes != spanBytes || spanBytes <= 0 {
		t.Fatalf("standalone span consumed work: %+v want 3/%d", spanWork, spanBytes)
	}
	matches, err = b.Span(ctx, id(999))
	if err != nil || len(matches) != 0 || b.LastQueryWork().Candidates != 0 || b.LastQueryWork().Bytes != 0 {
		t.Fatalf("standalone span miss: %+v %+v %v", matches, b.LastQueryWork(), err)
	}
	b.cfg.Tenant = 1
	matches, err = b.Span(ctx, common)
	if err != nil || !reflect.DeepEqual(matches, []SpanMatch{{trace(7), common, 120}}) {
		t.Fatalf("standalone other-tenant lookup: %+v %v", matches, err)
	}
	b.cfg.Tenant = 0
	b.cfg.MaxCandidates = 2
	_, err = b.Span(ctx, common)
	requireOtelBudget(t, err)
	b.cfg.MaxCandidates, b.cfg.MaxBytes = 3, spanBytes
	matches, err = b.Span(ctx, common)
	if err != nil || !reflect.DeepEqual(matches, wantSpan) {
		t.Fatalf("exact span candidate/byte budget: %+v %v", matches, err)
	}
	b.cfg.MaxBytes = spanBytes - 1
	_, err = b.Span(ctx, common)
	requireOtelBudget(t, err)
	if work := b.LastQueryWork(); work.Candidates != 3 || work.Bytes != spanBytes {
		t.Fatalf("final span index entry crossing: %+v want 3/%d", work, spanBytes)
	}
	b.cfg = c
	b.cfg.K = 10
	interval := TimeRange{Start: 100, End: 200}
	wantTime := []Hit{{trace(2), 199}, {trace(8), 150}, {trace(6), 150}, {trace(1), 100}}
	got, err = b.Time(ctx, interval)
	timeBytes := intrinsicRawBytes(t, ctx, b, timeIndexName, recordlayer.TupleRangeBetween(tuple.Tuple{int64(0), int64(100)}, tuple.Tuple{int64(0), int64(200)}))
	if err != nil || !reflect.DeepEqual(got.Hits, wantTime) || got.Candidates != 6 || got.Bytes != timeBytes || timeBytes <= 0 {
		t.Fatalf("time half-open distinct witnesses: %+v want %+v 6/%d: %v", got, wantTime, timeBytes, err)
	}
	b.cfg.K = 3
	got, err = b.Time(ctx, interval)
	if err != nil || !reflect.DeepEqual(got.Hits, wantTime[:3]) || got.Candidates != 3 || got.Bytes >= timeBytes {
		t.Fatalf("time distinct frontier: %+v want %+v: %v", got, wantTime[:3], err)
	}
	b.cfg.K = 1
	got, err = b.Time(ctx, TimeRange{Start: 100, End: 101})
	if err != nil || !reflect.DeepEqual(got.Hits, []Hit{{trace(2), 100}}) {
		t.Fatalf("literal time tie boundary: %+v %v", got, err)
	}
	b.cfg.K = 10
	got, err = b.Time(ctx, TimeRange{Start: 200, End: 201})
	if err != nil || !reflect.DeepEqual(got.Hits, []Hit{{trace(4), 200}}) {
		t.Fatalf("inclusive lower boundary: %+v %v", got, err)
	}
	for _, empty := range []TimeRange{{Start: 100, End: 100}, {Start: 400, End: 500}} {
		got, err := b.Time(ctx, empty)
		if err != nil || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 || b.LastQueryWork().Candidates != 0 || b.LastQueryWork().Bytes != 0 {
			t.Fatalf("empty/missing time interval %+v: %+v %v", empty, got, err)
		}
	}
	if _, err := b.Time(ctx, interval); err != nil {
		t.Fatal(err)
	}
	_, err = b.Time(ctx, TimeRange{Start: 200, End: 100})
	var invalid *ConfigError
	if !errors.As(err, &invalid) || b.LastQueryWork().Candidates != 0 || b.LastQueryWork().Bytes != 0 {
		t.Fatalf("reversed time range/reset: %+v %T %v", b.LastQueryWork(), err, err)
	}
	b.cfg.MaxCandidates = 5
	_, err = b.Time(ctx, interval)
	requireOtelBudget(t, err)
	b.cfg.MaxCandidates, b.cfg.MaxBytes = 6, timeBytes
	got, err = b.Time(ctx, interval)
	if err != nil || !reflect.DeepEqual(got.Hits, wantTime) || got.Candidates != 6 || got.Bytes != timeBytes {
		t.Fatalf("exact time consumed budgets: %+v %v", got, err)
	}
	b.cfg.MaxBytes = timeBytes - 1
	_, err = b.Time(ctx, interval)
	requireOtelBudget(t, err)
	if work := b.LastQueryWork(); work.Candidates != 6 || work.Bytes != timeBytes {
		t.Fatalf("final time index entry crossing: %+v want 6/%d", work, timeBytes)
	}
	b.cfg = c
	for _, kind := range []string{"span", "time"} {
		lookup := func() error {
			if kind == "span" {
				matches, err := b.Span(ctx, id(2))
				if err == nil && !reflect.DeepEqual(matches, []SpanMatch{{trace(1), id(2), 300}}) {
					return fmt.Errorf("singleton span result %+v", matches)
				}
				return err
			}
			got, err := b.Time(ctx, TimeRange{Start: 299, End: 301})
			if err == nil && !reflect.DeepEqual(got.Hits, []Hit{{trace(1), 300}}) {
				return fmt.Errorf("singleton time result %+v", got)
			}
			return err
		}
		if err := lookup(); err != nil {
			t.Fatal(err)
		}
		work := b.LastQueryWork()
		if work.Candidates != 1 || work.Bytes <= 1 {
			t.Fatalf("vacuous singleton %s work: %+v", kind, work)
		}
		for _, insufficient := range []int64{1, work.Bytes - 1} {
			b.cfg.MaxBytes = insufficient
			requireOtelBudget(t, lookup())
		}
		b.cfg.MaxBytes = work.Bytes
		if err := lookup(); err != nil {
			t.Fatalf("exact singleton %s bytes: %v", kind, err)
		}
		b.cfg = c
	}
	testOtelIntrinsicStateReadError(t, ctx, b)
	for _, name := range []string{spanIndexName, timeIndexName} {
		for _, state := range []string{"write_only", "disabled"} {
			t.Run(name+"_"+state, func(t *testing.T) { testOtelIntrinsicUnreadable(t, ctx, cluster, c, name, state) })
		}
	}
}

func intrinsicRawBytes(t *testing.T, ctx context.Context, b *FDBBackend, name string, bounds recordlayer.TupleRange) int64 {
	t.Helper()
	ss := b.store.Sub(recordlayer.IndexKey, b.md.GetIndex(name).SubspaceTupleKey())
	var rows []fdb.KeyValue
	_, err := b.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		var err error
		rows, err = tx.GetRange(bounds.ToFDBRange(ss), fdb.RangeOptions{Limit: 257}).GetSliceWithError()
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 256 {
		t.Fatal("intrinsic raw byte census exceeded fixture bound")
	}
	var total int64
	for _, row := range rows {
		total += int64(len(row.Key) + len(row.Value))
	}
	return total
}

func testOtelIntrinsicUnreadable(t *testing.T, ctx context.Context, cluster string, c Config, name, state string) {
	c.Count = 4
	b := openOtelFixture(t, ctx, cluster, c, true)
	s := Generate(b.cfg, 0)
	lookup := func() error {
		if name == spanIndexName {
			_, err := b.Span(ctx, s.ID)
			return err
		}
		_, err := b.Time(ctx, TimeRange{Start: s.Start, End: s.Start + 1})
		return err
	}
	if err := lookup(); err != nil {
		t.Fatal(err)
	}
	if work := b.LastQueryWork(); work.Candidates == 0 || work.Bytes == 0 {
		t.Fatalf("unreadable-index positive control empty: %+v", work)
	}
	otelTransaction(t, ctx, b, func(store *recordlayer.FDBRecordStore, _ *recordlayer.FDBRecordContext) error {
		if state == "write_only" {
			_, err := store.MarkIndexWriteOnly(name)
			return err
		}
		_, err := store.MarkIndexDisabled(name)
		return err
	})
	var ownership *OwnershipError
	if err := lookup(); !errors.As(err, &ownership) {
		t.Fatalf("%s %s public lookup: %T %v", name, state, err, err)
	}
	if work := b.LastQueryWork(); work.Candidates != 0 || work.Bytes != 0 {
		t.Fatalf("unreadable-index lookup retained prior work: %+v", work)
	}
	if name == timeIndexName {
		_, err := b.Time(ctx, TimeRange{Start: s.Start, End: s.Start})
		if !errors.As(err, &ownership) {
			t.Fatalf("empty Time bypassed unreadable state: %T %v", err, err)
		}
		if work := b.LastQueryWork(); work.Candidates != 0 || work.Bytes != 0 {
			t.Fatalf("empty unreadable Time retained work: %+v", work)
		}
	}
}

type (
	stateReadFailureTransactor struct {
		fdb.Database
		key      fdb.Key
		cause    error
		failures *atomic.Int64
	}
	stateReadFailureTransaction struct {
		fdb.WritableTransaction
		key      fdb.Key
		cause    error
		failures *atomic.Int64
	}
)

func (tx stateReadFailureTransaction) AddReadConflictKey(key fdb.KeyConvertible) error {
	if string(key.FDBKey()) == string(tx.key) {
		tx.failures.Add(1)
		return tx.cause
	}
	return tx.WritableTransaction.AddReadConflictKey(key)
}

func (d stateReadFailureTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d stateReadFailureTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		return fn(stateReadFailureTransaction{tx, d.key, d.cause, d.failures})
	})
}

func testOtelAttributeStateReadError(t *testing.T, ctx context.Context, b *FDBBackend, attribute Attribute, want []Hit) {
	t.Helper()
	prior, err := b.Query(ctx, attribute)
	if err != nil || !reflect.DeepEqual(prior.Hits, want) || prior.Candidates != 1 || prior.Bytes <= 0 {
		t.Fatalf("attribute state read error needs positive prior result/work: got=%+v want=%+v err=%v", prior, want, err)
	}
	if work := b.LastQueryWork(); work.Candidates != prior.Candidates || work.Bytes != prior.Bytes {
		t.Fatalf("attribute positive control did not publish prior work: got=%+v result=%+v", work, prior)
	}
	cause := errors.New("attribute index state conflict read failed")
	var failures atomic.Int64
	original := b.db
	defer func() { b.db = original }()
	key := b.store.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{b.idx.Name})
	b.db = recordlayer.NewFDBDatabaseWithTransactor(stateReadFailureTransactor{b.raw, key, cause, &failures}, b.raw)
	got, err := b.Query(ctx, attribute)
	if !errors.Is(err, cause) || failures.Load() != 1 || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("attribute Query swallowed state read failure or consumed postings: failures=%d got=%+v err=%v", failures.Load(), got, err)
	}
	if work := b.LastQueryWork(); work.Candidates != 0 || work.Bytes != 0 || len(work.Hits) != 0 {
		t.Fatalf("failed attribute Query retained previous work: %+v", work)
	}
	b.db = original
	restored, err := b.Query(ctx, attribute)
	if err != nil || !reflect.DeepEqual(restored.Hits, want) || restored.Candidates != prior.Candidates || restored.Bytes != prior.Bytes {
		t.Fatalf("attribute restored state positive: got=%+v want=%+v prior=%+v err=%v", restored, want, prior, err)
	}
	if work := b.LastQueryWork(); work.Candidates != restored.Candidates || work.Bytes != restored.Bytes || !reflect.DeepEqual(work.Hits, want) {
		t.Fatalf("restored attribute Query work: %+v result=%+v", work, restored)
	}
}

func testOtelIntrinsicStateReadError(t *testing.T, ctx context.Context, b *FDBBackend) {
	if _, err := b.Time(ctx, TimeRange{Start: 299, End: 301}); err != nil {
		t.Fatal(err)
	}
	if work := b.LastQueryWork(); work.Candidates != 1 || work.Bytes <= 0 {
		t.Fatalf("state read error needs successful prior work: %+v", work)
	}
	cause := errors.New("intrinsic index state conflict read failed")
	var failures atomic.Int64
	original := b.db
	defer func() { b.db = original }()
	key := b.store.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{timeIndexName})
	b.db = recordlayer.NewFDBDatabaseWithTransactor(stateReadFailureTransactor{b.raw, key, cause, &failures}, b.raw)
	got, err := b.Time(ctx, TimeRange{Start: 100, End: 100})
	if !errors.Is(err, cause) || failures.Load() != 1 || len(got.Hits) != 0 {
		t.Fatalf("empty Time swallowed state read failure: failures=%d got=%+v err=%v", failures.Load(), got, err)
	}
	if work := b.LastQueryWork(); work.Candidates != 0 || work.Bytes != 0 {
		t.Fatalf("failed empty Time retained previous work: %+v", work)
	}
	b.db = original
	got, err = b.Time(ctx, TimeRange{Start: 100, End: 100})
	if err != nil || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("empty Time restored state positive: %+v %v", got, err)
	}
}

type (
	approximateFailureTransactor struct {
		fdb.Database
		cause error
		gets  *atomic.Int64
	}
	approximateFailureTransaction struct {
		fdb.WritableTransaction
		cause error
		gets  *atomic.Int64
	}
	approximateFailureFuture struct {
		fdb.FutureInt64
		cause error
		gets  *atomic.Int64
	}
)

func (f approximateFailureFuture) Get() (int64, error) { f.gets.Add(1); return 0, f.cause }
func (tx approximateFailureTransaction) GetApproximateSize() fdb.FutureInt64 {
	return approximateFailureFuture{tx.WritableTransaction.GetApproximateSize(), tx.cause, tx.gets}
}

func (d approximateFailureTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d approximateFailureTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		return fn(approximateFailureTransaction{tx, d.cause, d.gets})
	})
}

func testOtelApproximateSizeError(t *testing.T, ctx context.Context, cluster, order string) {
	c := testConfig()
	c.Count, c.Tenants, c.Batch, c.Workers, c.Order = 4, 1, 2, 1, order
	b := openOtelFixture(t, ctx, cluster, c, false)
	cause := errors.New("approximate mutation size future failed")
	var gets atomic.Int64
	original := b.db
	b.db = recordlayer.NewFDBDatabaseWithTransactor(approximateFailureTransactor{b.raw, cause, &gets}, b.raw)
	stats, err := b.Ingest(ctx)
	b.db = original
	if !errors.Is(err, cause) || gets.Load() != 1 || stats.Rows != 0 {
		t.Fatalf("approximate-size future failure must abort source transaction: gets=%d stats=%+v err=%v", gets.Load(), stats, err)
	}
	commit, err := readOtelIngestCommit(ctx, b, recordlayer.AttemptCall{})
	if err != nil || commit.owner != manifest(b.cfg, "loading") || commit.recordKeys != 0 || commit.postingKeys != 0 {
		t.Fatalf("failed source guard published data: %+v %v", commit, err)
	}
	for _, name := range []string{spanIndexName, timeIndexName} {
		size, err := b.size(ctx, b.store.Sub(recordlayer.IndexKey, b.md.GetIndex(name).SubspaceTupleKey()))
		if err != nil || size.Keys != 0 {
			t.Fatalf("failed source guard left built-in %s entries: %+v %v", name, size, err)
		}
	}
}
