package otel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Ports of the umbrella subtests (integration_test.go) that §2.1 keeps as
// Exporter tests. The umbrella ran each twice, for order trace and time; D2
// removes the order key, and the relational reader always stops at the K-th
// distinct trace (§1.3), so each runs once, as order time. D2 also removes
// hash_bytes: the indexes hold attribute values, not digests.

// relFixtureSpan is s's single-span envelope, as every backend stores it.
func relFixtureSpan(t *testing.T, s Span) *tracepb.ResourceSpans {
	t.Helper()
	rs, _, err := FixtureOTLP(s)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// relExportCorpus exports the spans Generate assigns to c.Tenant through e,
// whose tenant store holds that half alone, and returns them.
func relExportCorpus(t *testing.T, ctx context.Context, e *Exporter, c Config) []Span {
	t.Helper()
	var spans []Span
	var rss []*tracepb.ResourceSpans
	for i := 0; i < c.Count; i++ {
		if s := Generate(c, i); s.Tenant == c.Tenant {
			spans = append(spans, s)
			rss = append(rss, relFixtureSpan(t, s))
		}
	}
	if stats, err := e.ExportResourceSpans(ctx, rss); err != nil || stats.Spans != int64(len(rss)) || len(rss) == 0 {
		t.Fatalf("export corpus of %d spans: %+v %v", len(rss), stats, err)
	}
	return spans
}

// Port of testOtelScopedQueries (integration_test.go:1115) on the umbrella
// corpus (Count 260, K 3). D1: equality is typed, so the int-typed spellings
// are answered by SPANS_INT_ATTR and the string-typed ones never match them.
func testRelTypedScopedControls(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	c := testConfig()
	c.Count, c.K = 260, 3
	e := openRelExporter(t, ctx, cluster, func(x *ExporterConfig) { x.K = c.K })
	relExportCorpus(t, ctx, e, c)
	for _, a := range []Attribute{
		{"span", QueryKey, "string", QueryValue},
		{"span", QueryKey, "int", QueryValue},
		{"span", QueryKey + ".int", "int", QueryValue},
		{"resource", QueryKey, "string", QueryValue},
		{"scope", QueryKey, "string", QueryValue},
	} {
		want := Expected(c, a.Scope, a.Key, a.Type, a.Value)
		if len(want) == 0 {
			t.Fatalf("vacuous positive control: %+v", a)
		}
		got, err := e.Query(ctx, a)
		if err != nil || !sameJSON(got.Hits, want) {
			t.Fatalf("typed/scoped positive %+v: %+v want %+v: %v", a, got, want, err)
		}
	}
	for _, a := range []Attribute{
		{"span", QueryKey + ".int", "string", QueryValue},
		{"resource", QueryKey, "int", QueryValue},
		{"scope", QueryKey, "int", QueryValue},
		{"scope", QueryKey, "string", "definitely_absent"},
	} {
		got, err := e.Query(ctx, a)
		if err != nil || len(got.Hits) != 0 {
			t.Fatalf("typed/scoped negative %+v: %+v %v", a, got, err)
		}
	}
}

// Port of testOtelSelectiveV2 (selectivity_test.go:148). Its trace-order
// branch, which exhausted the two-candidate budget on a common term, has no
// counterpart (D2): every common term returns the K=1 frontier after one
// index entry.
func testRelSelectiveV2(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.MatchEvery = 64, 1, 8, 1
	c.MaxCandidates, c.K, c.Iterations = 2, 1, 2
	e := openRelExporter(t, ctx, cluster, func(x *ExporterConfig) { x.K, x.MaxCandidates = c.K, c.MaxCandidates })
	maxBytes := e.Config().MaxBytes
	relExportCorpus(t, ctx, e, c)
	selective, err := queryCases(ctx, c, "selective")
	if err != nil {
		t.Fatal(err)
	}
	q := selective[1]
	source := Generate(c, q.sourceOrdinal)
	if q.kind != "attribute" || q.class != "custom_varying" || q.attribute != (Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + source.ID}) {
		t.Fatalf("real selective predicate is not the unique source response ID: %+v", q)
	}
	got, err := e.Query(ctx, q.attribute)
	want := []Hit{{Trace: source.Trace, Latest: source.Start}}
	if err != nil || !reflect.DeepEqual(got.Hits, want) || got.Candidates != 1 || got.Bytes <= 0 || got.Bytes > maxBytes {
		t.Fatalf("unique response query did not fit bounded work: got=%+v want=%+v error=%v", got, want, err)
	}
	mixed, err := queryCases(ctx, c, "mixed")
	if err != nil {
		t.Fatal(err)
	}
	genaiConfig := c
	genaiConfig.Iterations = 7
	genai, err := queryCases(ctx, genaiConfig, "genai")
	if err != nil {
		t.Fatal(err)
	}
	for _, common := range []queryCase{mixed[1], genai[6]} {
		if population := selectivitySourcePopulation(c, []Attribute{common.attribute}); population[common.attribute] != c.Count {
			t.Fatalf("%s common source population=%d want %d", common.class, population[common.attribute], c.Count)
		}
		got, err := e.Query(ctx, common.attribute)
		want := bruteAttributeOracle(c, common.attribute)
		if err != nil || len(want) != 1 || !reflect.DeepEqual(got.Hits, want) || got.Candidates != 1 || got.Bytes <= 0 || got.Bytes > maxBytes {
			t.Fatalf("%s must return the sound K=1 frontier: got=%+v want=%+v error=%v", common.class, got, want, err)
		}
	}
}

// relIndexCensus reads the raw key-values of e's index name in bounds,
// bypassing the reader, and returns their count and key plus value bytes:
// what a scan of the whole of bounds charges.
func relIndexCensus(t *testing.T, ctx context.Context, e *Exporter, name string, bounds recordlayer.TupleRange) (int, int64) {
	t.Helper()
	idx := e.store.ts.MetaData.GetIndex(name)
	if idx == nil {
		t.Fatalf("store has no index %s", name)
	}
	ss := e.store.ts.Subspace.Sub(recordlayer.IndexKey, idx.SubspaceTupleKey())
	var rows []fdb.KeyValue
	if _, err := e.raw.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		var err error
		rows, err = tx.GetRange(bounds.ToFDBRange(ss), fdb.RangeOptions{Limit: 257}).GetSliceWithError()
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) > 256 {
		t.Fatalf("index %s census exceeded the fixture bound", name)
	}
	var total int64
	for _, row := range rows {
		total += int64(len(row.Key) + len(row.Value))
	}
	return len(rows), total
}

// relIndexStateKey is the key of name's state, whose serializable read
// (ReadIndexState) every index scan of the reader adds.
func relIndexStateKey(e *Exporter, name string) fdb.Key {
	return e.store.ts.Subspace.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{name})
}

// relReplayState counts the closure entries of one public read.
type relReplayState struct {
	entries, injected, successfulBodies, guardFailures int
	// stateReads counts each attempt's conflict read of the index state key:
	// the attempt's body reached its index scan.
	stateReads [2]int
}

// relReplayTransactor fails a read's first successful attempt as
// not_committed (1020), so the client re-enters the same body, and runs
// beforeReplay first. With cause set, the second attempt's index-state
// conflict read fails with it.
type relReplayTransactor struct {
	fdb.Database
	state        *relReplayState
	beforeReplay func() error
	guardKey     fdb.Key
	cause        error
}

type relReplayTransaction struct {
	fdb.WritableTransaction
	state   *relReplayState
	attempt int
	key     fdb.Key
	cause   error
}

func (tx relReplayTransaction) AddReadConflictKey(key fdb.KeyConvertible) error {
	if string(key.FDBKey()) == string(tx.key) {
		tx.state.stateReads[tx.attempt-1]++
		if tx.cause != nil {
			tx.state.guardFailures++
			return tx.cause
		}
	}
	return tx.WritableTransaction.AddReadConflictKey(key)
}

func (d relReplayTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d relReplayTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.Database.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		d.state.entries++
		attempt := d.state.entries
		if attempt > 2 {
			return nil, fmt.Errorf("public query replay exceeded two closure entries: %d", attempt)
		}
		wrapped := relReplayTransaction{WritableTransaction: tx, state: d.state, attempt: attempt, key: d.guardKey}
		if attempt == 2 {
			if err := d.beforeReplay(); err != nil {
				return nil, err
			}
			wrapped.cause = d.cause
		}
		result, err := fn(wrapped)
		if err != nil {
			return result, err
		}
		d.state.successfulBodies++
		if d.state.stateReads[attempt-1] != 1 {
			return nil, fmt.Errorf("public query closure %d succeeded without reading its index: %+v", attempt, *d.state)
		}
		if attempt == 1 {
			d.state.injected++
			// The body read its index entries; abort before commit so the
			// client re-enters it.
			return nil, fdb.Error{Code: 1020}
		}
		return result, nil
	})
}

// Port of testOtelPublicQueryReplay (query_replay_test.go:113): a read whose
// successful first attempt is retried after both sources moved answers from
// the second attempt alone. §1.3: work is per call, returned with the
// result, and there is no LastQueryWork; so the v1 per-attempt work and
// witness census has no counterpart. Each attempt's index-state read shows
// its body scanned the index, and the returned work is the second attempt's
// exact census (two entries), not a sum of both.
func testRelPublicQueryReplay(t *testing.T, cluster string) {
	for _, kind := range []string{"query", "span", "time"} {
		for _, failGuard := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/state_error_%t", kind, failGuard), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.K = 2 })
				const traceA = "00000000000000000000000000000001"
				const traceB = "00000000000000000000000000000002"
				const sharedSpan = "0000000000000001"
				attribute := Attribute{Scope: "span", Key: "replay.shared", Type: "string", Value: "literal"}
				initial := []Span{
					{Ordinal: 0, Trace: traceA, ID: sharedSpan, Start: 100, Attributes: []Attribute{attribute}},
					{Ordinal: 1, Trace: traceB, ID: sharedSpan, Start: 90, Attributes: []Attribute{attribute}},
				}
				final := []Span{
					{Ordinal: 0, Trace: traceA, ID: sharedSpan, Start: 80, Attributes: []Attribute{attribute}},
					{Ordinal: 1, Trace: traceB, ID: sharedSpan, Start: 120, Attributes: []Attribute{attribute}},
				}
				export := func(spans []Span) error {
					rss := []*tracepb.ResourceSpans{relFixtureSpan(t, spans[0]), relFixtureSpan(t, spans[1])}
					stats, err := e.ExportResourceSpans(ctx, rss)
					if err == nil && stats.Spans != 2 {
						err = fmt.Errorf("export stats %+v", stats)
					}
					return err
				}
				if err := export(initial); err != nil {
					t.Fatal(err)
				}
				index, bounds := spansStrAttrIndex, recordlayer.TupleRangeAllOf(tuple.Tuple{attribute.Scope, attribute.Key, attribute.Value})
				switch kind {
				case "span":
					index, bounds = spansSpanIDIndex, recordlayer.TupleRangeAllOf(tuple.Tuple{qaBytes(t, sharedSpan)})
				case "time":
					index, bounds = spansTimeIndex, recordlayer.TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(200)})
				}
				if rows, firstBytes := relIndexCensus(t, ctx, e, index, bounds); rows != 2 || firstBytes <= 0 {
					t.Fatalf("first-attempt census: %d rows, %d bytes", rows, firstBytes)
				}
				state := &relReplayState{}
				var secondBytes int64
				var cause error
				if failGuard {
					cause = errors.New("second public query attempt index-state conflict read failed")
				}
				// Only the reads are replayed: the writer keeps the original database.
				replayed := *e.store
				replayed.db = recordlayer.NewFDBDatabaseWithTransactor(relReplayTransactor{
					Database: e.raw, state: state, guardKey: relIndexStateKey(e, index), cause: cause,
					beforeReplay: func() error {
						// Re-exports update the same primary keys and every index.
						if err := export(final); err != nil {
							return err
						}
						var rows int
						if rows, secondBytes = relIndexCensus(t, ctx, e, index, bounds); rows != 2 {
							return fmt.Errorf("second-attempt census: %d rows", rows)
						}
						return nil
					},
				}, e.raw)
				reader := *e.reader
				reader.store = &replayed
				original := e.reader
				e.reader = &reader
				defer func() { e.reader = original }()
				var got QueryResult
				var matches []SpanMatch
				var err error
				switch kind {
				case "query":
					got, err = e.Query(ctx, attribute)
				case "span":
					matches, got, err = e.reader.Span(e.txnContext(ctx), sharedSpan)
				case "time":
					got, err = e.Time(ctx, TimeRange{Start: 0, End: 200})
				}
				e.reader = original
				if ctx.Err() != nil {
					t.Fatalf("query replay canceled its live parent: %v", ctx.Err())
				}
				wantSuccesses, wantFailures := 2, 0
				if failGuard {
					wantSuccesses, wantFailures = 1, 1
				}
				if state.entries != 2 || state.injected != 1 || state.successfulBodies != wantSuccesses || state.guardFailures != wantFailures || state.stateReads != [2]int{1, 1} || secondBytes <= 0 {
					t.Fatalf("replay population: %+v secondBytes=%d err=%v", state, secondBytes, err)
				}
				if failGuard {
					if !errors.Is(err, cause) || len(matches) != 0 || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
						t.Fatalf("second-attempt guard failure retained output or lost sentinel: matches=%+v result=%+v err=%v", matches, got, err)
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
					} else if want := []Hit{{Trace: traceB, Latest: 120}, {Trace: traceA, Latest: 80}}; !reflect.DeepEqual(got.Hits, want) {
						t.Fatalf("replay retained failed-attempt hits: got=%+v want=%+v", got, want)
					}
					if got.Candidates != 2 || got.Bytes != secondBytes {
						t.Fatalf("replay did not return exact second-only work: %+v want 2/%d", got, secondBytes)
					}
				}
				for _, s := range final {
					rs, err := e.LoadSpanOTLP(ctx, s.Trace, s.ID)
					if err != nil {
						t.Fatal(err)
					}
					requireProtoEqual(t, "final source "+s.Trace, rs, relFixtureSpan(t, s))
				}
			})
		}
	}
}

// Port of testOtelIntrinsicV2 (intrinsic_test.go:104). The corpus tenants
// are two schema stores of one run (§0.1), so the built-in census is per
// store and the other tenant's lookup reads the other store. §1.3: work is
// returned per call, so the v1 LastQueryWork checks read the returned work.
func testRelIntrinsicV2(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c := testConfig()
	c.Count, c.Tenants, c.Attributes, c.K = 8, 2, 8, 1
	e := openRelExporter(t, ctx, cluster, func(x *ExporterConfig) { x.K = c.K })
	otherConfig := e.Config()
	otherConfig.Tenant++
	other := startRelExporter(t, ctx, otherConfig, relParts{})
	otherHalf := c
	otherHalf.Tenant = 1
	if n := len(relExportCorpus(t, ctx, e, c)) + len(relExportCorpus(t, ctx, other, otherHalf)); n != 8 {
		t.Fatalf("V2 corpus exported %d spans, want 8", n)
	}
	for _, x := range []*Exporter{e, other} {
		for _, name := range []string{spansSpanIDIndex, spansTimeIndex} {
			if rows, _ := relIndexCensus(t, ctx, x, name, recordlayer.TupleRangeAll); rows != 4 {
				t.Fatalf("V2 built-in index census %s of tenant %d: %d entries, want 4", name, x.Config().Tenant, rows)
			}
		}
	}
	first := Generate(c, 0)
	response := Attribute{"span", "gen_ai.response.id", "string", "chatcmpl-fixture-v2-" + first.ID}
	reHits(t, ctx, e, response, Hit{first.Trace, first.Start})
	for _, a := range []Attribute{
		{"resource", response.Key, "string", response.Value},
		{"scope", response.Key, "string", response.Value},
		{"span", response.Key, "int", "42"},
		{"span", response.Key, "string", "missing_response_id"},
	} {
		got, err := e.Query(ctx, a)
		if err != nil || len(got.Hits) != 0 {
			t.Fatalf("GenAI scope/type/missing control %+v: %+v %v", a, got, err)
		}
	}
	t.Run("attribute_state_read_error", func(t *testing.T) {
		testRelAttributeStateReadError(t, ctx, e, response, []Hit{{first.Trace, first.Start}})
	})
	for _, x := range []*Exporter{e, other} {
		if err := x.store.run(ctx, "test", func(store *recordlayer.FDBRecordStore) error { return store.DeleteAllRecords() }); err != nil {
			t.Fatal(err)
		}
	}
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
	byTenant := [2][]*tracepb.ResourceSpans{}
	for i := range sources {
		sources[i].Ordinal = i
		byTenant[sources[i].Tenant] = append(byTenant[sources[i].Tenant], relFixtureSpan(t, sources[i]))
	}
	for i, x := range []*Exporter{e, other} {
		if stats, err := x.ExportResourceSpans(ctx, byTenant[i]); err != nil || stats.Spans != int64(len(byTenant[i])) {
			t.Fatalf("explicit sources of tenant %d: %+v %v", i, stats, err)
		}
		for _, name := range []string{spansSpanIDIndex, spansTimeIndex} {
			if rows, _ := relIndexCensus(t, ctx, x, name, recordlayer.TupleRangeAll); rows != len(byTenant[i]) {
				t.Fatalf("explicit source built-in census %s of tenant %d: %d entries, want %d", name, i, rows, len(byTenant[i]))
			}
		}
	}
	reader := func(mutate func(*Config)) *SpanReader {
		r := *e.reader
		mutate(&r.cfg)
		return &r
	}
	wantSpan := []SpanMatch{{trace(3), common, 90}, {trace(1), common, 100}, {trace(2), common, 100}}
	reSpan(t, ctx, e, common, wantSpan...)
	_, spanBytes := relIndexCensus(t, ctx, e, spansSpanIDIndex, recordlayer.TupleRangeAllOf(tuple.Tuple{qaBytes(t, common)}))
	if matches, work, err := e.reader.Span(ctx, common); err != nil || !reflect.DeepEqual(matches, wantSpan) || work.Candidates != 3 || work.Bytes != spanBytes || spanBytes <= 0 {
		t.Fatalf("standalone Span must return all matches despite K=1, with their work: %+v %+v want 3/%d: %v", matches, work, spanBytes, err)
	}
	if matches, work, err := e.reader.Span(ctx, id(999)); err != nil || len(matches) != 0 || work.Candidates != 0 || work.Bytes != 0 {
		t.Fatalf("standalone span miss: %+v %+v %v", matches, work, err)
	}
	reSpan(t, ctx, other, common, SpanMatch{trace(7), common, 120})
	_, _, err := reader(func(x *Config) { x.MaxCandidates = 2 }).Span(ctx, common)
	requireOtelBudget(t, err)
	if matches, _, err := reader(func(x *Config) { x.MaxCandidates, x.MaxBytes = 3, spanBytes }).Span(ctx, common); err != nil || !reflect.DeepEqual(matches, wantSpan) {
		t.Fatalf("exact span candidate/byte budget: %+v %v", matches, err)
	}
	_, work, err := reader(func(x *Config) { x.MaxCandidates, x.MaxBytes = 3, spanBytes-1 }).Span(ctx, common)
	requireOtelBudget(t, err)
	if work.Candidates != 3 || work.Bytes != spanBytes {
		t.Fatalf("final span index entry crossing: %+v want 3/%d", work, spanBytes)
	}

	k := func(n int) func(*Config) { return func(x *Config) { x.K = n } }
	interval := TimeRange{Start: 100, End: 200}
	wantTime := []Hit{{trace(2), 199}, {trace(8), 150}, {trace(6), 150}, {trace(1), 100}}
	timeRows, timeBytes := relIndexCensus(t, ctx, e, spansTimeIndex, recordlayer.TupleRangeBetween(tuple.Tuple{int64(100)}, tuple.Tuple{int64(200)}))
	got, err := reader(k(10)).Time(ctx, interval)
	if err != nil || !reflect.DeepEqual(got.Hits, wantTime) || got.Candidates != 6 || got.Bytes != timeBytes || timeRows != 6 || timeBytes <= 0 {
		t.Fatalf("time half-open distinct witnesses: %+v want %+v 6/%d: %v", got, wantTime, timeBytes, err)
	}
	got, err = reader(k(3)).Time(ctx, interval)
	if err != nil || !reflect.DeepEqual(got.Hits, wantTime[:3]) || got.Candidates != 3 || got.Bytes >= timeBytes {
		t.Fatalf("time distinct frontier: %+v want %+v: %v", got, wantTime[:3], err)
	}
	if got, err := reader(k(1)).Time(ctx, TimeRange{Start: 100, End: 101}); err != nil || !reflect.DeepEqual(got.Hits, []Hit{{trace(2), 100}}) {
		t.Fatalf("literal time tie boundary: %+v %v", got, err)
	}
	if got, err := reader(k(10)).Time(ctx, TimeRange{Start: 200, End: 201}); err != nil || !reflect.DeepEqual(got.Hits, []Hit{{trace(4), 200}}) {
		t.Fatalf("inclusive lower boundary: %+v %v", got, err)
	}
	for _, empty := range []TimeRange{{Start: 100, End: 100}, {Start: 400, End: 500}} {
		got, err := reader(k(10)).Time(ctx, empty)
		if err != nil || got.Hits == nil || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
			t.Fatalf("empty/missing time interval %+v: %+v %v", empty, got, err)
		}
	}
	got, err = reader(k(10)).Time(ctx, TimeRange{Start: 200, End: 100})
	var invalid *ConfigError
	if !errors.As(err, &invalid) || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("reversed time range: %+v %T %v", got, err, err)
	}
	_, err = reader(func(x *Config) { x.K, x.MaxCandidates = 10, 5 }).Time(ctx, interval)
	requireOtelBudget(t, err)
	got, err = reader(func(x *Config) { x.K, x.MaxCandidates, x.MaxBytes = 10, 6, timeBytes }).Time(ctx, interval)
	if err != nil || !reflect.DeepEqual(got.Hits, wantTime) || got.Candidates != 6 || got.Bytes != timeBytes {
		t.Fatalf("exact time consumed budgets: %+v %v", got, err)
	}
	got, err = reader(func(x *Config) { x.K, x.MaxCandidates, x.MaxBytes = 10, 6, timeBytes-1 }).Time(ctx, interval)
	requireOtelBudget(t, err)
	if got.Candidates != 6 || got.Bytes != timeBytes {
		t.Fatalf("final time index entry crossing: %+v want 6/%d", got, timeBytes)
	}

	for _, kind := range []string{"span", "time"} {
		lookup := func(r *SpanReader) (QueryResult, error) {
			if kind == "span" {
				matches, work, err := r.Span(ctx, id(2))
				if err == nil && !reflect.DeepEqual(matches, []SpanMatch{{trace(1), id(2), 300}}) {
					return work, fmt.Errorf("singleton span result %+v", matches)
				}
				return work, err
			}
			got, err := r.Time(ctx, TimeRange{Start: 299, End: 301})
			if err == nil && !reflect.DeepEqual(got.Hits, []Hit{{trace(1), 300}}) {
				return got, fmt.Errorf("singleton time result %+v", got)
			}
			return got, err
		}
		work, err := lookup(e.reader)
		if err != nil {
			t.Fatal(err)
		}
		if work.Candidates != 1 || work.Bytes <= 1 {
			t.Fatalf("vacuous singleton %s work: %+v", kind, work)
		}
		for _, insufficient := range []int64{1, work.Bytes - 1} {
			_, err := lookup(reader(func(x *Config) { x.MaxBytes = insufficient }))
			requireOtelBudget(t, err)
		}
		if _, err := lookup(reader(func(x *Config) { x.MaxBytes = work.Bytes })); err != nil {
			t.Fatalf("exact singleton %s bytes: %v", kind, err)
		}
	}
	testRelIntrinsicStateReadError(t, ctx, e)
	for _, name := range []string{spansSpanIDIndex, spansTimeIndex} {
		for _, state := range []string{"write_only", "disabled"} {
			t.Run(name+"_"+state, func(t *testing.T) { testRelIntrinsicUnreadable(t, ctx, cluster, c, name, state) })
		}
	}
}

// relFailStateReads makes e's transactions fail the conflict read of name's
// index state with cause, counting the failures, until the returned restore.
func relFailStateReads(e *Exporter, name string, cause error, failures *atomic.Int64) (restore func()) {
	original := e.store.db
	e.store.db = recordlayer.NewFDBDatabaseWithTransactor(stateReadFailureTransactor{e.raw, relIndexStateKey(e, name), cause, failures}, e.raw)
	return func() { e.store.db = original }
}

// Port of testOtelAttributeStateReadError (intrinsic_test.go:390). §1.3: the
// work is the result's, so the v1 LastQueryWork checks are the returned
// results' and a failed read cannot leave earlier work behind.
func testRelAttributeStateReadError(t *testing.T, ctx context.Context, e *Exporter, attribute Attribute, want []Hit) {
	t.Helper()
	prior, err := e.Query(ctx, attribute)
	if err != nil || !reflect.DeepEqual(prior.Hits, want) || prior.Candidates != 1 || prior.Bytes <= 0 {
		t.Fatalf("attribute state read error needs positive prior result/work: got=%+v want=%+v err=%v", prior, want, err)
	}
	cause := errors.New("attribute index state conflict read failed")
	var failures atomic.Int64
	restore := relFailStateReads(e, spansStrAttrIndex, cause, &failures)
	defer restore()
	got, err := e.Query(ctx, attribute)
	if !errors.Is(err, cause) || failures.Load() != 1 || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("attribute Query swallowed state read failure or consumed entries: failures=%d got=%+v err=%v", failures.Load(), got, err)
	}
	restore()
	if restored, err := e.Query(ctx, attribute); err != nil || !reflect.DeepEqual(restored, prior) {
		t.Fatalf("attribute restored state positive: got=%+v prior=%+v err=%v", restored, prior, err)
	}
}

// Port of testOtelIntrinsicStateReadError (intrinsic_test.go:427). §1.3: an
// empty range reads nothing, so the empty Time on which v1 showed the
// failure no longer reads the index state. The failure is shown on the
// singleton range, and the empty range is shown not to read the state.
func testRelIntrinsicStateReadError(t *testing.T, ctx context.Context, e *Exporter) {
	singleton, empty := TimeRange{Start: 299, End: 301}, TimeRange{Start: 100, End: 100}
	prior, err := e.Time(ctx, singleton)
	if err != nil || prior.Candidates != 1 || prior.Bytes <= 0 {
		t.Fatalf("state read error needs successful prior work: %+v %v", prior, err)
	}
	cause := errors.New("intrinsic index state conflict read failed")
	var failures atomic.Int64
	restore := relFailStateReads(e, spansTimeIndex, cause, &failures)
	defer restore()
	got, err := e.Time(ctx, empty)
	if err != nil || failures.Load() != 0 || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("empty Time read the index state: failures=%d got=%+v err=%v", failures.Load(), got, err)
	}
	got, err = e.Time(ctx, singleton)
	if !errors.Is(err, cause) || failures.Load() != 1 || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("Time swallowed state read failure: failures=%d got=%+v err=%v", failures.Load(), got, err)
	}
	restore()
	if got, err := e.Time(ctx, singleton); err != nil || !reflect.DeepEqual(got, prior) {
		t.Fatalf("Time restored state positive: %+v prior %+v: %v", got, prior, err)
	}
	if got, err := e.Time(ctx, empty); err != nil || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
		t.Fatalf("empty Time restored state positive: %+v %v", got, err)
	}
}

// Port of testOtelIntrinsicUnreadable (intrinsic_test.go:312). §1.3: an empty
// Time reads nothing, so on an unreadable SPANS_TIME it returns no hits where
// v1 returned ownership.
func testRelIntrinsicUnreadable(t *testing.T, ctx context.Context, cluster string, c Config, name, state string) {
	c.Count = 4
	e := openRelExporter(t, ctx, cluster, func(x *ExporterConfig) { x.K = c.K })
	s := relExportCorpus(t, ctx, e, c)[0]
	lookup := func() (QueryResult, error) {
		if name == spansSpanIDIndex {
			_, work, err := e.reader.Span(e.txnContext(ctx), s.ID)
			return work, err
		}
		return e.Time(ctx, TimeRange{Start: s.Start, End: s.Start + 1})
	}
	if work, err := lookup(); err != nil || work.Candidates == 0 || work.Bytes == 0 {
		t.Fatalf("unreadable-index positive control empty: %+v %v", work, err)
	}
	if err := e.store.run(ctx, "test", func(store *recordlayer.FDBRecordStore) error {
		if state == "write_only" {
			_, err := store.MarkIndexWriteOnly(name)
			return err
		}
		_, err := store.MarkIndexDisabled(name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ownership *OwnershipError
	work, err := lookup()
	if !errors.As(err, &ownership) {
		t.Fatalf("%s %s public lookup: %T %v", name, state, err, err)
	}
	if work.Candidates != 0 || work.Bytes != 0 {
		t.Fatalf("unreadable-index lookup consumed work: %+v", work)
	}
	if name == spansTimeIndex {
		if got, err := e.Time(ctx, TimeRange{Start: s.Start, End: s.Start}); err != nil || got.Hits == nil || len(got.Hits) != 0 || got.Candidates != 0 || got.Bytes != 0 {
			t.Fatalf("empty Time on an unreadable index: %+v %v", got, err)
		}
	}
}
