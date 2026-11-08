package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/keyspace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// The Reader against stores the Exporter wrote. Two subtests audit every
// user key of the cluster before and after, so they run alone: the parallel
// subtests wait until the test function returns.
func TestReaderRealFDB(t *testing.T) {
	t.Parallel()
	cluster := startExporterCluster(t)
	t.Run("open_writes_nothing", func(t *testing.T) { testReaderWritesNothing(t, cluster) })
	t.Run("older_store_read_as_stored", func(t *testing.T) { testReaderOlderStore(t, cluster) })
	for _, layout := range []struct {
		name   string
		mutate func(*ExporterConfig)
		want   string
	}{
		{"none", nil, StoreLayoutNone},
		{"deltas", func(c *ExporterConfig) { c.EnableMessageDeltas = true }, StoreLayoutDeltas},
		{"fulltext", func(c *ExporterConfig) { c.EnableMessageDeltas, c.EnableMessageFullText = true, true }, StoreLayoutFullText},
	} {
		t.Run("trace_otlp_"+layout.name, func(t *testing.T) {
			t.Parallel()
			testReaderTraceOTLP(t, cluster, layout.mutate, layout.want)
		})
	}
	t.Run("trace_budgets", func(t *testing.T) { t.Parallel(); testReaderTraceBudgets(t, cluster) })
	t.Run("searches_and_logs", func(t *testing.T) { t.Parallel(); testReaderSearches(t, cluster) })
	t.Run("attribute_skip_scans", func(t *testing.T) { t.Parallel(); testReaderAttributes(t, cluster) })
	t.Run("empty_store", func(t *testing.T) { t.Parallel(); testReaderEmptyStore(t, cluster) })
	t.Run("open_refusals", func(t *testing.T) { t.Parallel(); testReaderOpenRefusals(t, cluster) })
	t.Run("transactions_never_commit", func(t *testing.T) { t.Parallel(); testReaderTransactions(t, cluster) })
	t.Run("stale_metadata", func(t *testing.T) { t.Parallel(); testReaderStaleMetadata(t, cluster) })
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// rdDB is a database handle of its own on cluster, as a dashboard process
// has: no client state shared with the Exporter that wrote the store.
func rdDB(t *testing.T, ctx context.Context, cluster string) *recordlayer.FDBDatabase {
	t.Helper()
	raw, err := openRelDatabase(ctx, cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	return recordlayer.NewFDBDatabase(raw)
}

// rdOpen opens a Reader on the store of the Exporter configuration cfg.
func rdOpen(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, cfg ExporterConfig, rc ReaderConfig) *Reader {
	t.Helper()
	path, schema := relTenantPaths(cfg.Run, cfg.Tenant)
	r, err := OpenReader(ctx, db, path, schema, rc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// rdKeys is every user key and value of the cluster, read at one version.
func rdKeys(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase) []fdb.KeyValue {
	t.Helper()
	out, err := db.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) {
		return rtx.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		t.Fatal(err)
	}
	kvs := out.([]fdb.KeyValue)
	if len(kvs) == 0 {
		t.Fatal("the key audit read no keys")
	}
	return kvs
}

func rdSameKeys(a, b []fdb.KeyValue) bool {
	return slices.EqualFunc(a, b, func(x, y fdb.KeyValue) bool {
		return bytes.Equal(x.Key, y.Key) && bytes.Equal(x.Value, y.Value)
	})
}

// rdEventually polls ok until it holds: a Reader's reads may use a read
// version up to 100ms older than another handle's commit (GRV cache).
func rdEventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never held", what)
		}
	}
}

const rdStart = int64(1_780_000_000_000_000_000)

// rdSpanID is span i's identity; the identities sort opposite to i, so span
// ID order is not creation order.
func rdSpanID(i int) []byte {
	return []byte{byte(0xf0 - 0x10*i), byte(i), 0x5a, 0x5a, 0x5a, 0x5a, 0x5a, 0x01}
}

// rdTrace is a GenAI trace of n spans: a root and its children, each with
// typed attributes of every scope, an event, a link to the previous span,
// and chat messages, the array form on even spans and the JSON form on odd
// ones, whose history grows. big > 0 adds a user message of big bytes to the
// last span, stored in parts.
func rdTrace(trace []byte, n, big int) []*tracepb.ResourceSpans {
	out := make([]*tracepb.ResourceSpans, n)
	var history []deltaMsg
	for i := range n {
		history = append(history, deltaMsg{"user", deltaText("user", i, 300)})
		if big > 0 && i == n-1 {
			history = append(history, deltaMsg{"user", deltaText("user", 99, big)})
		}
		form := "array"
		if i%2 == 1 {
			form = "json"
		}
		sp := &tracepb.Span{
			TraceId: trace, SpanId: rdSpanID(i), Name: "chat " + strconv.Itoa(i), Kind: tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(rdStart + int64(i)*1000), EndTimeUnixNano: uint64(rdStart + int64(i)*1000 + 500),
			Attributes: []*commonpb.KeyValue{
				exKV("gen_ai.operation.name", exStr("chat")),
				exKV("turn", exInt(int64(i))),
				exKV("ratio", exDouble(float64(i)/4)),
				exKV("ok", exBool(i%2 == 0)),
				exKV("gen_ai.input.messages", deltaMessages(form, history)),
				exKV("gen_ai.output.messages", deltaMessages(form, []deltaMsg{{"assistant", deltaText("assistant", i, 300)}})),
			},
			Events: []*tracepb.Span_Event{{TimeUnixNano: uint64(rdStart + int64(i)*1000 + 10), Name: "gen_ai.choice", Attributes: []*commonpb.KeyValue{exKV("index", exInt(int64(i)))}}},
		}
		if i > 0 {
			sp.ParentSpanId = rdSpanID(0)
			sp.Links = []*tracepb.Span_Link{{TraceId: trace, SpanId: rdSpanID(i - 1), Attributes: []*commonpb.KeyValue{exKV("link.kind", exStr("previous"))}}}
		}
		if i == 1 {
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "tool failed"}
		}
		out[i] = &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc-reader")), exKV("region", exStr("eu"))}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "reader-test", Version: "1", Attributes: []*commonpb.KeyValue{exKV("lib.flag", exBool(true))}},
				Spans: []*tracepb.Span{sp},
			}},
		}
	}
	return out
}

// rdLogs are three logs: two correlated to the trace's first two spans, one
// uncorrelated.
func rdLogs(trace []byte) []LogRecord {
	attrs := func(level string, count int64) []*commonpb.KeyValue {
		return []*commonpb.KeyValue{exKV("log.level", exStr(level)), exKV("log.count", exInt(count))}
	}
	res := []*commonpb.KeyValue{exKV("service.name", exStr("svc-reader"))}
	tr := hex.EncodeToString(trace)
	return []LogRecord{
		{EventID: "rd-1", Timestamp: rdStart + 10, TraceID: tr, SpanID: hex.EncodeToString(rdSpanID(0)), SeverityText: "INFO", Body: exStr("first"), Attributes: attrs("info", 1), ResourceAttributes: res, ScopeName: "reader-log"},
		{EventID: "rd-2", Timestamp: rdStart + 1010, TraceID: tr, SpanID: hex.EncodeToString(rdSpanID(1)), SeverityText: "WARN", Body: exStr("second"), Attributes: attrs("warn", 2), ResourceAttributes: res, ScopeName: "reader-log"},
		{EventID: "rd-3", Timestamp: rdStart + 20, SeverityText: "INFO", Body: exStr("uncorrelated"), Attributes: attrs("info", 3), ResourceAttributes: res, ScopeName: "reader-log"},
	}
}

func rdExport(t *testing.T, ctx context.Context, e *Exporter, spans []*tracepb.ResourceSpans, logs []LogRecord) {
	t.Helper()
	if _, err := e.ExportResourceSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	if len(logs) > 0 {
		if _, err := e.ExportLogs(ctx, logs); err != nil {
			t.Fatal(err)
		}
	}
}

// rdBySpanID is spans in span ID order, TraceOTLP's.
func rdBySpanID(spans []*tracepb.ResourceSpans) []*tracepb.ResourceSpans {
	out := slices.Clone(spans)
	slices.SortFunc(out, func(a, b *tracepb.ResourceSpans) int {
		return bytes.Compare(a.ScopeSpans[0].Spans[0].SpanId, b.ScopeSpans[0].Spans[0].SpanId)
	})
	return out
}

func rdRequireSpans(t *testing.T, label string, got, want []*tracepb.ResourceSpans) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d spans, want %d", label, len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Fatalf("%s: span %d (%x) differs", label, i, want[i].ScopeSpans[0].Spans[0].SpanId)
		}
	}
}

// ---------------------------------------------------------------------------
// Writes nothing
// ---------------------------------------------------------------------------

// testReaderWritesNothing runs every Reader read on a store that has no
// continuation signing key, as the Exporter leaves one it never paged, and
// audits the cluster's keys; the Exporter's own full page then publishes the
// key, which the audit sees, and the Reader signs with it.
func testReaderWritesNothing(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas, c.EnableMessageFullText = true, true })
	trace := exRandomID(t, 16)
	rdExport(t, ctx, e, rdTrace(trace, 4, 0), rdLogs(trace))
	db := rdDB(t, ctx, cluster)
	before := rdKeys(t, ctx, db)
	r := rdOpen(t, ctx, db, e.Config(), ReaderConfig{})
	same := SameSpanQuery{Predicates: []Attribute{{"span", "gen_ai.operation.name", "string", "chat"}}, Limit: 1}
	tr := hex.EncodeToString(trace)
	reads := map[string]func() (int, error){
		"trace": func() (int, error) { got, _, err := r.TraceOTLP(ctx, tr, TraceBudget{}); return len(got), err },
		"query": func() (int, error) {
			got, err := r.Query(ctx, Attribute{"span", "turn", "int", "1"})
			return len(got.Hits), err
		},
		"time": func() (int, error) {
			got, err := r.Time(ctx, TimeRange{Start: rdStart, End: rdStart + 10_000})
			return len(got.Hits), err
		},
		"same span full page": func() (int, error) {
			page, err := r.QuerySameSpan(ctx, same)
			if len(page.Continuation) != 0 {
				t.Errorf("a Reader signed a continuation on a store without a key")
			}
			return len(page.Spans), err
		},
		"existential": func() (int, error) {
			got, err := r.QueryTraceExistential(ctx, TraceExistentialQuery{
				Clauses:        []Attribute{{"span", "ok", "bool", "true"}},
				LogClauses:     []Attribute{{LogScopeRecord, "log.level", "string", "warn"}},
				MessageClauses: []MessageFullTextQuery{{Query: "inspects"}},
			})
			return len(got.Hits), err
		},
		"logs by trace": func() (int, error) { got, err := r.QueryLogsByTrace(ctx, tr, ""); return len(got), err },
		"logs by attribute": func() (int, error) {
			got, err := r.QueryLogsByAttribute(ctx, Attribute{LogScopeRecord, "log.level", "string", "info"}, 0)
			return len(got), err
		},
		"span keys": func() (int, error) {
			got, _, err := r.AttributeKeys(ctx, "span", "span", "", "", 0)
			return len(got), err
		},
		"log keys": func() (int, error) {
			got, _, err := r.AttributeKeys(ctx, "log", LogScopeRecord, "", "", 0)
			return len(got), err
		},
		"values": func() (int, error) {
			got, _, err := r.AttributeValues(ctx, "span", Attribute{Scope: "span", Key: "turn", Type: "int"}, "", 0)
			return len(got), err
		},
	}
	for name, read := range reads {
		if n, err := read(); err != nil || n == 0 {
			t.Fatalf("%s: %d results (%v)", name, n, err)
		}
	}
	if after := rdKeys(t, ctx, db); !rdSameKeys(before, after) {
		t.Fatalf("the Reader changed the cluster's keys: %d before, %d after", len(before), len(after))
	}
	// Positive control: the audit sees the key the Exporter's full page publishes.
	page, err := e.QuerySameSpan(ctx, same)
	if err != nil || len(page.Continuation) == 0 {
		t.Fatalf("Exporter full page: continuation %x (%v)", page.Continuation, err)
	}
	published := rdKeys(t, ctx, db)
	if rdSameKeys(before, published) {
		t.Fatal("the audit missed the signing key the Exporter published")
	}
	// The Reader signs with the stored key, and each accepts the other's token.
	var rp SpanQueryPage
	rdEventually(t, "a Reader continuation from the stored key", func() bool {
		rp, err = r.QuerySameSpan(ctx, same)
		return err == nil && len(rp.Continuation) > 0
	})
	next := same
	next.Continuation = rp.Continuation
	ep2, err := e.QuerySameSpan(ctx, next)
	if err != nil {
		t.Fatalf("the Exporter refused the Reader's continuation: %v", err)
	}
	next.Continuation = page.Continuation
	rp2, err := r.QuerySameSpan(ctx, next)
	if err != nil || len(rp2.Spans) != 1 || !reflect.DeepEqual(rp2.Spans, ep2.Spans) {
		t.Fatalf("Reader page 2 %+v (%v), Exporter page 2 %+v", rp2.Spans, err, ep2.Spans)
	}
	if after := rdKeys(t, ctx, db); !rdSameKeys(published, after) {
		t.Fatal("the Reader changed the cluster's keys while signing")
	}
}

// testReaderOlderStore reads a store whose header names an older metadata
// version, as one last opened by an older build: the Reader neither upgrades
// it nor rebuilds or disables its indexes (a store open would), reads it as
// stored, and leaves its keys unchanged.
func testReaderOlderStore(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	trace := exRandomID(t, 16)
	spans := rdTrace(trace, 3, 0)
	rdExport(t, ctx, e, spans, rdLogs(trace))
	tr := hex.EncodeToString(trace)
	attr := Attribute{"span", "gen_ai.operation.name", "string", "chat"}
	wantHits, err := e.Query(ctx, attr)
	if err != nil || len(wantHits.Hits) == 0 {
		t.Fatalf("Exporter query: %+v (%v)", wantHits, err)
	}
	wantLogs, err := e.QueryLogsByTrace(ctx, tr, "")
	if err != nil || len(wantLogs) != 2 {
		t.Fatalf("Exporter logs: %d (%v)", len(wantLogs), err)
	}
	db := rdDB(t, ctx, cluster)
	// The Exporter is not used from here: its opens would upgrade the store.
	if was := rdSetHeaderVersion(t, ctx, db, e.store.ts.Subspace, func(int32) int32 { return 0 }); was <= 0 {
		t.Fatalf("the store header's metadata version was %d", was)
	}
	before := rdKeys(t, ctx, db)
	r := rdOpen(t, ctx, db, e.Config(), ReaderConfig{K: e.Config().K, MaxCandidates: e.Config().MaxCandidates, MaxBytes: e.Config().MaxBytes})
	got, err := r.Query(ctx, attr)
	if err != nil || !reflect.DeepEqual(got.Hits, wantHits.Hits) {
		t.Fatalf("query on the older store: %+v (%v), want %+v", got.Hits, err, wantHits.Hits)
	}
	otlp, _, err := r.TraceOTLP(ctx, tr, TraceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	rdRequireSpans(t, "older store", otlp, rdBySpanID(spans))
	logs, err := r.QueryLogsByTrace(ctx, tr, "")
	if err != nil || len(logs) != len(wantLogs) {
		t.Fatalf("logs on the older store: %d (%v)", len(logs), err)
	}
	for i := range logs {
		lgRequireLog(t, "older store log", logs[i], wantLogs[i])
	}
	if after := rdKeys(t, ctx, db); !rdSameKeys(before, after) {
		t.Fatal("the Reader upgraded the older store")
	}
}

// rdSetHeaderVersion rewrites the store header's metadata version as set
// maps it and returns the version it had.
func rdSetHeaderVersion(t *testing.T, ctx context.Context, db *recordlayer.FDBDatabase, store subspace.Subspace, set func(int32) int32) int32 {
	t.Helper()
	var was int32
	_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		key := store.Pack(tuple.Tuple{recordlayer.StoreInfoKey})
		raw, err := rc.Transaction().Get(key).Get()
		if err != nil {
			return nil, err
		}
		var info gen.DataStoreInfo
		if err := info.UnmarshalVT(raw); err != nil {
			return nil, err
		}
		was = info.GetMetaDataversion()
		version := set(was)
		info.MetaDataversion = &version
		b, err := info.MarshalVT()
		if err != nil {
			return nil, err
		}
		rc.Transaction().Set(key, b)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return was
}

// testReaderStaleMetadata: a store whose header names a newer metadata
// version than a Reader loaded is never read with the old metadata (the
// rebuild check that would refuse it is skipped): every read, and an open,
// fail stale.
func testReaderStaleMetadata(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	trace := exRandomID(t, 16)
	rdExport(t, ctx, e, rdTrace(trace, 2, 0), rdLogs(trace))
	db := rdDB(t, ctx, cluster)
	r := rdOpen(t, ctx, db, e.Config(), ReaderConfig{})
	attr := Attribute{"span", "turn", "int", "1"}
	if got, err := r.Query(ctx, attr); err != nil || len(got.Hits) != 1 {
		t.Fatalf("before: %+v (%v)", got, err)
	}
	was := rdSetHeaderVersion(t, ctx, db, e.store.ts.Subspace, func(v int32) int32 { return v + 1 })
	requireStale := func(t *testing.T, err error) bool {
		t.Helper()
		var stale *StaleMetadataError
		var versions *recordlayer.StaleMetaDataVersionError
		if !errors.As(err, &stale) {
			return false
		}
		if stale.Kind != "stale" || !errors.As(err, &versions) || versions.StoredVersion != int(was)+1 || versions.LocalVersion != int(was) {
			t.Fatalf("stale error %v", err)
		}
		return true
	}
	rdEventually(t, "the stale store refused", func() bool {
		_, err := r.Query(ctx, attr)
		return requireStale(t, err)
	})
	tr := hex.EncodeToString(trace)
	for name, read := range map[string]func() error{
		"trace": func() error { _, _, err := r.TraceOTLP(ctx, tr, TraceBudget{}); return err },
		"logs":  func() error { _, err := r.QueryLogsByTrace(ctx, tr, ""); return err },
		"keys":  func() error { _, _, err := r.AttributeKeys(ctx, "span", "span", "", "", 0); return err },
	} {
		if err := read(); !requireStale(t, err) {
			t.Fatalf("%s on a stale store: %v", name, err)
		}
	}
	path, schema := relTenantPaths(e.Config().Run, e.Config().Tenant)
	if _, err := OpenReader(ctx, db, path, schema, ReaderConfig{}); !requireStale(t, err) {
		t.Fatalf("open of a stale store: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TraceOTLP
// ---------------------------------------------------------------------------

// testReaderTraceOTLP: on each layout, TraceOTLP returns every span of the
// trace, and only those, as LoadSpanOTLP and the export had it.
func testReaderTraceOTLP(t *testing.T, cluster string, mutate func(*ExporterConfig), layout string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, mutate)
	trace := exRandomID(t, 16)
	spans := rdTrace(trace, 5, 0)
	rdExport(t, ctx, e, spans, nil)
	rdExport(t, ctx, e, rdTrace(exRandomID(t, 16), 2, 0), nil)
	r := rdOpen(t, ctx, rdDB(t, ctx, cluster), e.Config(), ReaderConfig{})
	if r.Layout() != layout {
		t.Fatalf("layout %q, want %q", r.Layout(), layout)
	}
	tr := hex.EncodeToString(trace)
	got, stats, err := r.TraceOTLP(ctx, tr, TraceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	want := rdBySpanID(spans)
	rdRequireSpans(t, "exported", got, want)
	loaded := make([]*tracepb.ResourceSpans, len(want))
	for i, rs := range want {
		if loaded[i], err = e.LoadSpanOTLP(ctx, tr, hex.EncodeToString(rs.ScopeSpans[0].Spans[0].SpanId)); err != nil {
			t.Fatal(err)
		}
	}
	rdRequireSpans(t, "LoadSpanOTLP", got, loaded)
	if stats.Spans != len(want) || stats.Truncated || stats.Reason != "" || stats.Bytes <= 0 || stats.DecodedBytes != rdDecoded(want) {
		t.Fatalf("stats %+v", stats)
	}
	got, stats, err = r.TraceOTLP(ctx, hex.EncodeToString(exRandomID(t, 16)), TraceBudget{})
	if err != nil || got == nil || len(got) != 0 || stats != (TraceReadStats{}) {
		t.Fatalf("unknown trace: %d spans %+v (%v)", len(got), stats, err)
	}
}

// rdStored is what the store holds for a trace: its rows' spans (message
// manifests unrestored) and bytes in span ID order, and its message rows'
// bytes, heads and later parts.
type rdStored struct {
	spans        []*tracepb.ResourceSpans
	rowBytes     []int64
	heads, parts int64
}

// rdStoredTrace reads e's store directly; its message rows must all belong
// to trace.
func rdStoredTrace(t *testing.T, ctx context.Context, e *Exporter, trace []byte) rdStored {
	t.Helper()
	var s rdStored
	err := e.store.read(withExporterTxns(ctx, 10*time.Second), "stored trace", func(store *recordlayer.FDBRecordStore) error {
		s = rdStored{}
		cur := store.ScanRecords(nil, recordlayer.ForwardScan())
		defer cur.Close()
		for {
			res, err := cur.OnNext(ctx)
			if err != nil {
				return err
			}
			if !res.HasNext() {
				return nil
			}
			rec := res.GetValue()
			n := int64(rec.KeySize + rec.ValueSize)
			switch row := rec.Record.(type) {
			case *otelv1pb.SPANS:
				if !bytes.Equal(row.GetTRACE_ID(), trace) {
					continue
				}
				rs, err := rowOTLP("stored trace", row.ProtoReflect(), trace, row.GetSPAN_ID())
				if err != nil {
					return err
				}
				s.spans, s.rowBytes = append(s.spans, rs), append(s.rowBytes, n)
			case *otelv1pb.MESSAGES:
				if row.GetPART() > 0 {
					s.parts += n
				} else {
					s.heads += n
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rdSum(xs []int64) int64 {
	var n int64
	for _, x := range xs {
		n += x
	}
	return n
}

// testReaderTraceBudgets drives each limit of TraceOTLP at its boundary on a
// trace with a message stored in parts: the span count; the stored bytes of
// the span rows and of the message rows, after their heads (the parts are
// then not read) and after their parts; and the decoded bytes of the span
// rows, of the messages' raw elements (the parts are then not read) and of
// the restored spans. Bytes and DecodedBytes are exact.
func testReaderTraceBudgets(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	trace := exRandomID(t, 16)
	spans := rdBySpanID(rdTrace(trace, 4, 3<<19))
	rdExport(t, ctx, e, spans, nil)
	stored := rdStoredTrace(t, ctx, e, trace)
	if len(stored.spans) != 4 || stored.heads == 0 || stored.parts == 0 {
		t.Fatalf("fixture: %d spans, message heads %d bytes, parts %d", len(stored.spans), stored.heads, stored.parts)
	}
	spanBytes := rdSum(stored.rowBytes)
	total := spanBytes + stored.heads + stored.parts
	restored, manifests := rdDecoded(spans), rdDecoded(stored.spans)
	if restored <= 3*manifests {
		t.Fatalf("fixture: restored spans %d bytes, manifests %d: the messages must dominate", restored, manifests)
	}
	r := rdOpen(t, ctx, rdDB(t, ctx, cluster), e.Config(), ReaderConfig{})
	tr := hex.EncodeToString(trace)
	byBytes, byDecoded := TraceReasonMaxBytes+","+TraceReasonMessages, TraceReasonMaxDecodedBytes+","+TraceReasonMessages
	for _, c := range []struct {
		name    string
		budget  TraceBudget
		want    []*tracepb.ResourceSpans
		reason  string
		bytes   int64 // -1: not checked
		decoded int64
	}{
		{"default budget", TraceBudget{}, spans, "", total, restored},
		{"exact span count", TraceBudget{MaxSpans: 4}, spans, "", total, restored},
		{"one span fewer", TraceBudget{MaxSpans: 3}, spans[:3], TraceReasonMaxSpans, -1, rdDecoded(spans[:3])},
		{"exact bytes", TraceBudget{MaxBytes: total}, spans, "", total, restored},
		{"parts read, one byte over", TraceBudget{MaxBytes: total - 1}, stored.spans, byBytes, total, manifests},
		{"heads one byte over", TraceBudget{MaxBytes: spanBytes + stored.heads - 1}, stored.spans, byBytes, spanBytes + stored.heads, manifests},
		{"heads exactly, parts over", TraceBudget{MaxBytes: spanBytes + stored.heads}, stored.spans, byBytes, total, manifests},
		{"span rows exactly", TraceBudget{MaxBytes: spanBytes}, stored.spans, byBytes, spanBytes + stored.heads, manifests},
		{"last span row over", TraceBudget{MaxBytes: spanBytes - 1}, stored.spans[:3], byBytes, rdSum(stored.rowBytes[:3]), rdDecoded(stored.spans[:3])},
		{"exact decoded bytes", TraceBudget{MaxDecodedBytes: restored}, spans, "", total, restored},
		{"restored one byte over", TraceBudget{MaxDecodedBytes: restored - 1}, stored.spans, byDecoded, total, manifests},
		{"raw elements over", TraceBudget{MaxDecodedBytes: manifests}, stored.spans, byDecoded, spanBytes + stored.heads, manifests},
		{"last span decoded over", TraceBudget{MaxDecodedBytes: manifests - 1}, stored.spans[:3], byDecoded, rdSum(stored.rowBytes[:3]), rdDecoded(stored.spans[:3])},
	} {
		got, stats, err := r.TraceOTLP(ctx, tr, c.budget)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		rdRequireSpans(t, c.name, got, c.want)
		if stats.Spans != len(c.want) || stats.Truncated != (c.reason != "") || stats.Reason != c.reason || c.bytes >= 0 && stats.Bytes != c.bytes || stats.DecodedBytes != c.decoded {
			t.Fatalf("%s: stats %+v, want reason %q, %d bytes, %d decoded", c.name, stats, c.reason, c.bytes, c.decoded)
		}
	}
}

// rdDecoded is the spans' protobuf size.
func rdDecoded(spans []*tracepb.ResourceSpans) int64 {
	var n int64
	for _, rs := range spans {
		n += int64(proto.Size(rs))
	}
	return n
}

// ---------------------------------------------------------------------------
// Searches and logs
// ---------------------------------------------------------------------------

// testReaderSearches: the Reader's searches and log reads answer as the
// Exporter's on the same store, within the Reader's own K and budgets.
func testReaderSearches(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas, c.EnableMessageFullText = true, true })
	trace := exRandomID(t, 16)
	rdExport(t, ctx, e, rdTrace(trace, 5, 0), rdLogs(trace))
	rdExport(t, ctx, e, rdTrace(exRandomID(t, 16), 2, 0), nil)
	tr := hex.EncodeToString(trace)
	cfg := e.Config()
	db := rdDB(t, ctx, cluster)
	r := rdOpen(t, ctx, db, cfg, ReaderConfig{K: cfg.K, MaxCandidates: cfg.MaxCandidates, MaxBytes: cfg.MaxBytes})
	chat := Attribute{"span", "gen_ai.operation.name", "string", "chat"}

	rq, err := r.Query(ctx, chat)
	eq, eerr := e.Query(ctx, chat)
	if err != nil || eerr != nil || len(rq.Hits) != 2 || !reflect.DeepEqual(rq, eq) {
		t.Fatalf("query: %+v (%v), Exporter %+v (%v)", rq, err, eq, eerr)
	}
	window := TimeRange{Start: rdStart, End: rdStart + 10_000}
	rt, err := r.Time(ctx, window)
	et, eerr := e.Time(ctx, window)
	if err != nil || eerr != nil || len(rt.Hits) != 2 || !reflect.DeepEqual(rt, et) {
		t.Fatalf("time: %+v (%v), Exporter %+v (%v)", rt, err, et, eerr)
	}
	same := SameSpanQuery{Predicates: []Attribute{chat, {"span", "ok", "bool", "true"}}, Limit: 2}
	rs, err := r.QuerySameSpan(ctx, same)
	es, eerr := e.QuerySameSpan(ctx, same)
	if err != nil || eerr != nil || len(rs.Spans) != 2 || !reflect.DeepEqual(rs.Spans, es.Spans) || rs.Candidates != es.Candidates || rs.Bytes != es.Bytes {
		t.Fatalf("same span: %+v (%v), Exporter %+v (%v)", rs, err, es, eerr)
	}
	ex := TraceExistentialQuery{
		Clauses:        []Attribute{{"span", "turn", "int", "3"}},
		LogClauses:     []Attribute{{LogScopeRecord, "log.level", "string", "warn"}},
		MessageClauses: []MessageFullTextQuery{{Query: "inspects"}},
	}
	rx, err := r.QueryTraceExistential(ctx, ex)
	exe, eerr := e.QueryTraceExistential(ctx, ex)
	rx.ReadVersion, exe.ReadVersion = 0, 0
	if err != nil || eerr != nil || len(rx.Hits) != 1 || rx.Hits[0].Trace != tr || !reflect.DeepEqual(rx, exe) {
		t.Fatalf("existential: %+v (%v), Exporter %+v (%v)", rx, err, exe, eerr)
	}
	for _, span := range []string{"", hex.EncodeToString(rdSpanID(1))} {
		rl, err := r.QueryLogsByTrace(ctx, tr, span)
		el, eerr := e.QueryLogsByTrace(ctx, tr, span)
		if err != nil || eerr != nil || len(rl) == 0 || len(rl) != len(el) {
			t.Fatalf("logs of %q: %d (%v), Exporter %d (%v)", span, len(rl), err, len(el), eerr)
		}
		for i := range rl {
			lgRequireLog(t, "log by trace", rl[i], el[i])
		}
	}
	info := Attribute{LogScopeRecord, "log.level", "string", "info"}
	rl, err := r.QueryLogsByAttribute(ctx, info, 0)
	el, eerr := e.QueryLogsByAttribute(ctx, info, 0)
	if err != nil || eerr != nil || len(rl) != 2 || len(el) != 2 {
		t.Fatalf("logs by attribute: %d (%v), Exporter %d (%v)", len(rl), err, len(el), eerr)
	}
	for i := range rl {
		lgRequireLog(t, "log by attribute", rl[i], el[i])
	}

	// The Reader's own K and budget.
	one := rdOpen(t, ctx, db, cfg, ReaderConfig{K: 1})
	if got, err := one.Time(ctx, window); err != nil || len(got.Hits) != 1 || got.Hits[0] != rt.Hits[0] {
		t.Fatalf("K 1: %+v (%v)", got, err)
	}
	tight := rdOpen(t, ctx, db, cfg, ReaderConfig{MaxCandidates: 1})
	_, err = tight.Query(ctx, chat)
	requireOtelBudget(t, err)
}

// ---------------------------------------------------------------------------
// Attribute autocomplete
// ---------------------------------------------------------------------------

// rdAttrSpans and rdAttrLogs hold indexed attributes of every scope and type
// with keys sharing prefixes and a key holding NUL, and attributes no index
// holds: a key with only a long (digest-indexed) string, a key over the cap,
// and bytes and array values.
func rdAttrSpans(trace []byte) []*tracepb.ResourceSpans {
	long := strings.Repeat("l", otelsql.MaxIndexedStringBytes+1)
	overKey := strings.Repeat("k", otelsql.MaxAttributeKeyBytes+1)
	mk := func(i int, res, scope, span []*commonpb.KeyValue) *tracepb.ResourceSpans {
		return &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: res},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "attrs", Attributes: scope},
				Spans: []*tracepb.Span{{TraceId: trace, SpanId: rdSpanID(i), Name: "s", StartTimeUnixNano: uint64(rdStart + int64(i)), EndTimeUnixNano: uint64(rdStart + int64(i) + 1), Attributes: span}},
			}},
		}
	}
	return []*tracepb.ResourceSpans{
		mk(0,
			[]*commonpb.KeyValue{exKV("service.name", exStr("svc-a")), exKV("host.cpu", exInt(8))},
			[]*commonpb.KeyValue{exKV("lib.flag", exBool(true))},
			[]*commonpb.KeyValue{
				exKV("http.method", exStr("GET")), exKV("http.status", exInt(200)), exKV("ratio", exDouble(0.5)), exKV("ok", exBool(true)), exKV("nul", exStr("")),
				exKV("nul\x00key", exStr("x")), exKV("big", exStr(long)), exKV("blob", exBytes([]byte{1})), exKV("list", exArray(exInt(1))), exKV(overKey, exStr("v")),
			}),
		mk(1,
			[]*commonpb.KeyValue{exKV("service.name", exStr("svc-b")), exKV("host.cpu", exInt(16))},
			[]*commonpb.KeyValue{exKV("lib.flag", exBool(false))},
			[]*commonpb.KeyValue{exKV("http.method", exStr("POST")), exKV("http.status", exInt(404)), exKV("ratio", exDouble(-2.25)), exKV("ok", exBool(false)), exKV("http", exStr("h")), exKV("nul", exStr("\x00x"))}),
		mk(2,
			[]*commonpb.KeyValue{exKV("service.name", exStr("svc-a"))},
			nil,
			[]*commonpb.KeyValue{exKV("http.method", exStr("GET")), exKV("http.status", exInt(-1)), exKV("ratio", exDouble(math.NaN())), exKV("ok", exBool(true)), exKV("nul", exStr("v"))}),
		mk(3,
			[]*commonpb.KeyValue{exKV("service.name", exStr("svc-a"))},
			nil,
			[]*commonpb.KeyValue{exKV("nul", exStr("v\x00x"))}),
	}
}

func rdAttrLogs() []LogRecord {
	return []LogRecord{
		{
			EventID: "at-1", Timestamp: rdStart, ResourceAttributes: []*commonpb.KeyValue{exKV("service.name", exStr("svc-log"))}, ScopeName: "l", ScopeAttributes: []*commonpb.KeyValue{exKV("log.lib", exStr("zap"))},
			Attributes: []*commonpb.KeyValue{exKV("log.level", exStr("info")), exKV("log.count", exInt(1)), exKV("log.ok", exBool(true)), exKV("log.ratio", exDouble(1.5))},
		},
		{EventID: "at-2", Timestamp: rdStart + 1, Attributes: []*commonpb.KeyValue{exKV("log.level", exStr("warn")), exKV("log.count", exInt(2))}},
	}
}

// rdIndexed is the seeds' indexed attributes, derived from the OTLP: per
// signal, scope, type and key, the stored values (NaN as otelsql.CanonicalNaN).
type rdIndexed map[[4]string][]tuple.TupleElement

func (x rdIndexed) add(signal, scope string, kvs []*commonpb.KeyValue) {
	for _, kv := range kvs {
		if len(kv.Key) > otelsql.MaxAttributeKeyBytes {
			continue
		}
		var typ string
		var v tuple.TupleElement
		switch val := kv.Value.GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			if len(val.StringValue) > otelsql.MaxIndexedStringBytes {
				continue
			}
			typ, v = "string", val.StringValue
		case *commonpb.AnyValue_IntValue:
			typ, v = "int", val.IntValue
		case *commonpb.AnyValue_DoubleValue:
			f := val.DoubleValue
			if math.IsNaN(f) {
				f = otelsql.CanonicalNaN
			}
			typ, v = "double", f
		case *commonpb.AnyValue_BoolValue:
			typ, v = "bool", val.BoolValue
		default:
			continue
		}
		k := [4]string{signal, scope, typ, kv.Key}
		if !slices.ContainsFunc(x[k], func(o tuple.TupleElement) bool { return bytes.Equal(tuple.Tuple{o}.Pack(), tuple.Tuple{v}.Pack()) }) {
			x[k] = append(x[k], v)
		}
	}
}

// keys is the sorted distinct keys of signal and scope with a value of typ
// ("" any).
func (x rdIndexed) keys(signal, scope, typ string) []string {
	out := []string{}
	for k := range x {
		if k[0] == signal && k[1] == scope && (typ == "" || k[2] == typ) && !slices.Contains(out, k[3]) {
			out = append(out, k[3])
		}
	}
	sort.Strings(out)
	return out
}

// values is the key's values as canonical predicate text, in index order.
func (x rdIndexed) values(k [4]string) []string {
	vs := slices.Clone(x[k])
	slices.SortFunc(vs, func(a, b tuple.TupleElement) int { return bytes.Compare(tuple.Tuple{a}.Pack(), tuple.Tuple{b}.Pack()) })
	out := make([]string, len(vs))
	for i, v := range vs {
		switch v := v.(type) {
		case string:
			out[i] = v
		case int64:
			out[i] = strconv.FormatInt(v, 10)
		case float64:
			out[i] = otelsql.FormatDouble(v)
		case bool:
			out[i] = strconv.FormatBool(v)
		}
	}
	return out
}

// testReaderAttributes checks every signal, scope and type against the
// seeds, every listed value as a working predicate, prefixes, limits at
// their boundary, and what is never offered.
func testReaderAttributes(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	trace := exRandomID(t, 16)
	spans, logs := rdAttrSpans(trace), rdAttrLogs()
	rdExport(t, ctx, e, spans, logs)
	want := rdIndexed{}
	for _, rs := range spans {
		want.add("span", "resource", rs.Resource.Attributes)
		want.add("span", "scope", rs.ScopeSpans[0].Scope.Attributes)
		want.add("span", "span", rs.ScopeSpans[0].Spans[0].Attributes)
	}
	for _, l := range logs {
		want.add("log", "resource", l.ResourceAttributes)
		want.add("log", "scope", l.ScopeAttributes)
		want.add("log", LogScopeRecord, l.Attributes)
	}
	r := rdOpen(t, ctx, rdDB(t, ctx, cluster), e.Config(), ReaderConfig{})
	populated := 0
	for signal, s := range attributeIndexes {
		for _, scope := range s.scopes {
			for _, typ := range append([]string{""}, attributeTypes...) {
				keys, truncated, err := r.AttributeKeys(ctx, signal, scope, typ, "", 0)
				if w := want.keys(signal, scope, typ); err != nil || truncated || !slices.Equal(keys, w) {
					t.Fatalf("%s %s %q keys %q %v (%v), want %q", signal, scope, typ, keys, truncated, err, w)
				}
				populated += len(keys)
			}
		}
	}
	if populated == 0 {
		t.Fatal("no keys listed")
	}
	for k := range want {
		signal, scope, typ, key := k[0], k[1], k[2], k[3]
		values, truncated, err := r.AttributeValues(ctx, signal, Attribute{Scope: scope, Key: key, Type: typ}, "", 0)
		if w := want.values(k); err != nil || truncated || !slices.Equal(values, w) {
			t.Fatalf("%q values %q %v (%v), want %q", k, values, truncated, err, w)
		}
		for _, v := range values {
			a := Attribute{scope, key, typ, v}
			n := 0
			if signal == "span" {
				got, err := r.Query(ctx, a)
				if err != nil {
					t.Fatalf("%+v as a predicate: %v", a, err)
				}
				n = len(got.Hits)
			} else {
				got, err := r.QueryLogsByAttribute(ctx, a, 0)
				if err != nil {
					t.Fatalf("%+v as a predicate: %v", a, err)
				}
				n = len(got)
			}
			if n == 0 {
				t.Fatalf("listed value %+v matches nothing", a)
			}
		}
	}
	for _, c := range []struct {
		name, prefix string
		limit        int
		want         []string
		truncated    bool
	}{
		{"prefix", "http.", 0, []string{"http.method", "http.status"}, false},
		{"prefix is a key", "http", 0, []string{"http", "http.method", "http.status"}, false},
		{"prefix is a key before NUL", "nul", 0, []string{"nul", "nul\x00key"}, false},
		{"prefix with NUL", "nul\x00", 0, []string{"nul\x00key"}, false},
		{"no key", "zzz", 0, []string{}, false},
		{"limit at the count", "http", 3, []string{"http", "http.method", "http.status"}, false},
		{"limit under the count", "http", 2, []string{"http", "http.method"}, true},
	} {
		keys, truncated, err := r.AttributeKeys(ctx, "span", "span", "", c.prefix, c.limit)
		if err != nil || truncated != c.truncated || !slices.Equal(keys, c.want) {
			t.Fatalf("%s: %q %v (%v), want %q %v", c.name, keys, truncated, err, c.want, c.truncated)
		}
	}
	// One type's scan truncates past the limit too.
	if keys, truncated, err := r.AttributeKeys(ctx, "span", "span", "string", "", 1); err != nil || !truncated || !slices.Equal(keys, want.keys("span", "span", "string")[:1]) {
		t.Fatalf("string keys, limit 1: %q %v (%v)", keys, truncated, err)
	}
	// The read and time caps truncate a scan.
	for _, c := range []struct {
		name     string
		maxReads int
		maxTime  time.Duration
		want     []string
	}{
		{"one read", 1, time.Minute, want.keys("span", "span", "string")[:1]},
		{"past the time", MaxAttributeKeys, -time.Second, nil},
	} {
		s := skipScan{index: spansStrAttrIndex, fixed: tuple.Tuple{"span"}, text: stringText, limit: MaxAttributeKeys, maxReads: c.maxReads, maxTime: c.maxTime}
		s.run(withExporterTxns(ctx, 5*time.Second), r.store, "capped scan")
		if s.err != nil || !s.truncated || !slices.Equal(s.out, c.want) {
			t.Fatalf("%s: %q %v (%v), want %q truncated", c.name, s.out, s.truncated, s.err, c.want)
		}
	}
	for _, c := range []struct {
		a         Attribute
		prefix    string
		limit     int
		want      []string
		truncated bool
	}{
		{Attribute{Scope: "span", Key: "http.method", Type: "string"}, "PO", 0, []string{"POST"}, false},
		{Attribute{Scope: "span", Key: "http.method", Type: "string"}, "", 1, []string{"GET"}, true},
		{Attribute{Scope: "span", Key: "http.method", Type: "string"}, "", 2, []string{"GET", "POST"}, false},
		{Attribute{Scope: "span", Key: "http.status", Type: "int"}, "4", 0, []string{"404"}, false},
		{Attribute{Scope: "span", Key: "http.status", Type: "int"}, "", 2, []string{"-1", "200"}, true},
		{Attribute{Scope: "span", Key: "ratio", Type: "double"}, "-", 0, []string{"-2.25"}, false},
		{Attribute{Scope: "span", Key: "ok", Type: "bool"}, "t", 0, []string{"true"}, false},
		{Attribute{Scope: "span", Key: "big", Type: "string"}, "", 0, []string{}, false},
		{Attribute{Scope: "span", Key: "http.method", Type: "int"}, "", 0, []string{}, false},
		{Attribute{Scope: "log", Key: "log.level", Type: "string"}, "w", 0, []string{"warn"}, false},
	} {
		signal := "span"
		if c.a.Scope == LogScopeRecord {
			signal = "log"
		}
		values, truncated, err := r.AttributeValues(ctx, signal, c.a, c.prefix, c.limit)
		if err != nil || truncated != c.truncated || !slices.Equal(values, c.want) {
			t.Fatalf("%+v %q limit %d: %q %v (%v), want %q %v", c.a, c.prefix, c.limit, values, truncated, err, c.want, c.truncated)
		}
	}
}

// testReaderEmptyStore: a store with nothing exported lists no key or value
// of any signal, scope and type, untruncated; a span exported afterwards is
// listed (positive control).
func testReaderEmptyStore(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	e := openRelExporter(t, ctx, cluster, nil)
	r := rdOpen(t, ctx, rdDB(t, ctx, cluster), e.Config(), ReaderConfig{})
	calls := 0
	for signal, s := range attributeIndexes {
		for _, scope := range s.scopes {
			for _, typ := range append([]string{""}, attributeTypes...) {
				keys, truncated, err := r.AttributeKeys(ctx, signal, scope, typ, "", 0)
				if err != nil || truncated || keys == nil || len(keys) != 0 {
					t.Fatalf("%s %s %q: %q %v (%v)", signal, scope, typ, keys, truncated, err)
				}
				if typ == "" {
					continue
				}
				values, truncated, err := r.AttributeValues(ctx, signal, Attribute{Scope: scope, Key: "service.name", Type: typ}, "", 0)
				if err != nil || truncated || values == nil || len(values) != 0 {
					t.Fatalf("%s %s %s values: %q %v (%v)", signal, scope, typ, values, truncated, err)
				}
				calls++
			}
		}
	}
	if calls != 2*3*4 {
		t.Fatalf("%d value calls", calls)
	}
	got, stats, err := r.TraceOTLP(ctx, hex.EncodeToString(exRandomID(t, 16)), TraceBudget{})
	if err != nil || len(got) != 0 || stats != (TraceReadStats{}) {
		t.Fatalf("trace in an empty store: %d %+v (%v)", len(got), stats, err)
	}
	rdExport(t, ctx, e, rdAttrSpans(exRandomID(t, 16))[:1], nil)
	rdEventually(t, "the exported span's keys listed", func() bool {
		keys, _, err := r.AttributeKeys(ctx, "span", "span", "string", "http.", 0)
		return err == nil && slices.Equal(keys, []string{"http.method"})
	})
	// An index that is not readable is ownership, never an empty answer.
	err = e.store.run(withExporterTxns(ctx, 10*time.Second), "disable index", func(s *recordlayer.FDBRecordStore) error {
		_, err := s.MarkIndexDisabled(logsIntAttrIndex)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rdEventually(t, "the disabled index refused", func() bool {
		_, _, err := r.AttributeKeys(ctx, "log", LogScopeRecord, "int", "", 0)
		var owned *OwnershipError
		return errors.As(err, &owned)
	})
}

// ---------------------------------------------------------------------------
// Open and transactions
// ---------------------------------------------------------------------------

// testReaderOpenRefusals: a schema no Exporter opened has no layout
// (ownership), as has a layout this build does not know; an unknown database
// or schema and a malformed path are config, as is a missing database handle
// or a bad configuration.
func testReaderOpenRefusals(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	db := rdDB(t, ctx, cluster)
	keyspace.RegisterDomainIfNotExists(relDomain)
	path, schema := relTenantPaths(freshRun(t), 3)
	ts, err := otelsql.EnsureTenant(ctx, db, path, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	open := func(path, schema string, cfg ReaderConfig) error {
		r, err := OpenReader(ctx, db, path, schema, cfg)
		if err == nil {
			_ = r.Close()
		}
		return err
	}
	err = open(path, schema, ReaderConfig{})
	requireOtelKind(t, err, "ownership")
	if !strings.Contains(err.Error(), "has no layout") {
		t.Fatalf("store without a layout: %v", err)
	}
	requireOtelKind(t, open(path, "T99", ReaderConfig{}), "config")
	requireOtelKind(t, open("/OTEL/R_"+strings.ToUpper(freshRun(t)), "T0", ReaderConfig{}), "config")
	requireOtelKind(t, open("not-a-path", "T0", ReaderConfig{}), "config")
	requireOtelKind(t, open(path, schema, ReaderConfig{K: -1}), "config")
	_, err = OpenReader(ctx, nil, path, schema, ReaderConfig{})
	requireOtelKind(t, err, "config")
	store := &tenantStore{db: db, ts: ts, databasePath: ts.DatabasePath, schema: ts.Schema}
	err = store.run(withExporterTxns(ctx, 10*time.Second), "seed layout", func(s *recordlayer.FDBRecordStore) error {
		return saveStoreMeta(s, otelsql.StoreMeta{Name: otelsql.StoreMetaLayout, Str: proto.String("messages=bogus")})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = open(path, schema, ReaderConfig{})
	requireOtelKind(t, err, "ownership")
	if !strings.Contains(err.Error(), "messages=bogus") {
		t.Fatalf("unknown layout: %v", err)
	}
}

// testReaderTransactions: a Reader transaction is cancelled, not committed,
// so its writes are lost; it carries the Reader's tags; a retryable FDB error
// retries, at most readerAttempts attempts, and any other error does not;
// the Reader's store refuses a write transaction; and every read of a Reader
// runs in such transactions.
func testReaderTransactions(t *testing.T, cluster string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	db := rdDB(t, ctx, cluster)
	txns := &readerTxns{tags: []string{"grafana", "dashboards"}}
	pctx := withExporterTxns(ctx, 5*time.Second)
	key := fdb.Key("reader-probe-" + freshRun(t))
	calls := 0
	err := txns.run(pctx, db, "probe", func(rc *recordlayer.FDBRecordContext) error {
		calls++
		rc.Transaction().Set(key, []byte("written"))
		tags := rc.Transaction().Options().(interface{ Tags() []string }).Tags()
		if !slices.Equal(tags, txns.tags) {
			return errors.New("tags " + strings.Join(tags, ","))
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("probe: %d calls (%v)", calls, err)
	}
	out, err := db.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) { return rtx.Get(key).Get() })
	if err != nil {
		t.Fatal(err)
	}
	if v := out.([]byte); v != nil {
		t.Fatalf("a Reader transaction committed its write: %q", v)
	}
	for _, c := range []struct {
		name  string
		fail  func(call int) error
		calls int
		kind  string
	}{
		{"retryable once", func(call int) error {
			if call == 1 {
				return fdb.Error{Code: 1007}
			}
			return nil
		}, 2, ""},
		{"retryable always", func(int) error { return fdb.Error{Code: 1007} }, readerAttempts, "transient"},
		{"timed out", func(int) error { return fdb.Error{Code: 1031} }, 1, "transient"},
		{"not FDB", func(int) error { return errors.New("not an FDB error") }, 1, "operation"},
	} {
		calls := 0
		err := txns.run(pctx, db, "probe", func(*recordlayer.FDBRecordContext) error {
			calls++
			return c.fail(calls)
		})
		if calls != c.calls {
			t.Fatalf("%s: %d calls, want %d (%v)", c.name, calls, c.calls, err)
		}
		if c.kind == "" {
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		} else {
			requireOtelKind(t, err, c.kind)
		}
	}
	store := &tenantStore{db: db, reader: txns}
	err = store.run(pctx, "write", func(*recordlayer.FDBRecordStore) error {
		t.Error("a Reader's store ran a write transaction")
		return nil
	})
	requireOtelKind(t, err, "operation")

	// A Reader's reads: each attempt's write is lost and its tags are set.
	e := openRelExporter(t, ctx, cluster, func(c *ExporterConfig) { c.EnableMessageDeltas = true })
	trace := exRandomID(t, 16)
	rdExport(t, ctx, e, rdTrace(trace, 2, 0), rdLogs(trace))
	tags := []string{"grafana"}
	r := rdOpen(t, ctx, db, e.Config(), ReaderConfig{TransactionTags: tags})
	var mu sync.Mutex
	observed := 0
	r.store.reader.observe = func(rc *recordlayer.FDBRecordContext) {
		mu.Lock()
		defer mu.Unlock()
		observed++
		rc.Transaction().Set(key, []byte("written by a read"))
		if got := rc.Transaction().Options().(interface{ Tags() []string }).Tags(); !slices.Equal(got, tags) {
			t.Errorf("a Reader read without its tags: %q", got)
		}
	}
	tr := hex.EncodeToString(trace)
	for name, read := range map[string]func() error{
		"trace": func() error { _, _, err := r.TraceOTLP(ctx, tr, TraceBudget{}); return err },
		"query": func() error { _, err := r.Query(ctx, Attribute{"span", "turn", "int", "1"}); return err },
		"logs":  func() error { _, err := r.QueryLogsByTrace(ctx, tr, ""); return err },
		"keys":  func() error { _, _, err := r.AttributeKeys(ctx, "span", "span", "", "", 0); return err },
	} {
		mu.Lock()
		before := observed
		mu.Unlock()
		if err := read(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mu.Lock()
		if observed == before {
			t.Errorf("%s ran outside the Reader's transactions", name)
		}
		mu.Unlock()
	}
	out, err = db.RunRead(ctx, func(rtx fdb.ReadTransaction) (any, error) { return rtx.Get(key).Get() })
	if err != nil || out.([]byte) != nil {
		t.Fatalf("a Reader's read committed its write: %q (%v)", out, err)
	}
}
