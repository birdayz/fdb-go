package otel

import (
	"context"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/layers/otel/otelsql"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Harness of the Exporter ports: the exporter_helpers_test.go helpers
// over the read API both exporters share.

// relTestConfig is a fresh run's tenant 11 on cluster. D2: the relational
// store refuses Brotli, the v1 default.
func relTestConfig(t *testing.T, cluster string, mutate func(*ExporterConfig)) ExporterConfig {
	t.Helper()
	cfg := DefaultExporterConfig()
	cfg.ClusterFile, cfg.Run, cfg.Tenant = cluster, freshRun(t), 11
	cfg.RecordCompression = "zlib-1"
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// startRelExporter opens cfg's exporter and shuts it down at cleanup.
func startRelExporter(t *testing.T, ctx context.Context, cfg ExporterConfig, parts relParts) *Exporter {
	t.Helper()
	e, err := newExporter(ctx, cfg, parts)
	if err != nil {
		t.Fatal(err)
	}
	if e.Config() != cfg {
		t.Fatalf("Config() = %+v, want %+v", e.Config(), cfg)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return e
}

// openRelExporter is openTestExporter for the relational exporter.
func openRelExporter(t *testing.T, ctx context.Context, cluster string, mutate func(*ExporterConfig)) *Exporter {
	t.Helper()
	return startRelExporter(t, ctx, relTestConfig(t, cluster, mutate), relParts{})
}

// relReads is the read API of both exporters.
type relReads interface {
	Query(context.Context, Attribute) (QueryResult, error)
	Trace(context.Context, string) ([]SpanHit, error)
	Span(context.Context, string) ([]SpanMatch, error)
	Time(context.Context, TimeRange) (QueryResult, error)
	LoadSpanOTLP(context.Context, string, string) (*tracepb.ResourceSpans, error)
}

func reHits(t *testing.T, ctx context.Context, e relReads, a Attribute, want ...Hit) QueryResult {
	t.Helper()
	got, err := e.Query(ctx, a)
	if err != nil {
		t.Fatalf("query %+v: %v", a, err)
	}
	if got.Hits == nil {
		t.Fatalf("query %+v returned nil hits", a)
	}
	if len(want) == 0 {
		want = []Hit{}
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("query %+v\n got %+v\nwant %+v", a, got.Hits, want)
	}
	return got
}

func reTrace(t *testing.T, ctx context.Context, e relReads, traceID string, want ...SpanHit) {
	t.Helper()
	got, err := e.Trace(ctx, traceID)
	if err != nil {
		t.Fatalf("trace %s: %v", traceID, err)
	}
	if len(want) == 0 {
		want = []SpanHit{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace %s\n got %+v\nwant %+v", traceID, got, want)
	}
}

func reSpan(t *testing.T, ctx context.Context, e relReads, spanID string, want ...SpanMatch) {
	t.Helper()
	got, err := e.Span(ctx, spanID)
	if err != nil {
		t.Fatalf("span %s: %v", spanID, err)
	}
	if len(want) == 0 {
		want = []SpanMatch{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("span %s\n got %+v\nwant %+v", spanID, got, want)
	}
}

func reTime(t *testing.T, ctx context.Context, e relReads, r TimeRange, want ...Hit) {
	t.Helper()
	got, err := e.Time(ctx, r)
	if err != nil {
		t.Fatalf("time %+v: %v", r, err)
	}
	if len(want) == 0 {
		want = []Hit{}
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Fatalf("time %+v\n got %+v\nwant %+v", r, got.Hits, want)
	}
}

// reRecorded checks LoadSpanOTLP against the SDK's own view of every span.
func reRecorded(t *testing.T, ctx context.Context, e relReads, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, ro := range spans {
		want := ResourceSpansFromReadOnly([]sdktrace.ReadOnlySpan{ro})
		sc := ro.SpanContext()
		got, err := e.LoadSpanOTLP(ctx, sc.TraceID().String(), sc.SpanID().String())
		if err != nil {
			t.Fatalf("load %s/%s (%s): %v", sc.TraceID(), sc.SpanID(), ro.Name(), err)
		}
		requireProtoEqual(t, "LoadSpanOTLP "+ro.Name(), got, want[0])
	}
}

// typedAttr is the typed equality (D1) of an exported attribute: v1's
// CanonicalAttribute turned bools and doubles into strings.
func typedAttr(t *testing.T, scope, key string, v *commonpb.AnyValue) Attribute {
	t.Helper()
	a, ok := qrTyped(scope, exKV(key, v))
	if !ok {
		t.Fatalf("%s %s has no typed equality", scope, key)
	}
	return a
}

// reMessages is an in-memory relMessageHooks: it numbers elements 1.1, 1.2,
// ... in persist order, keeps their bytes for reconstruct and packs
// references as relMessageStore does, placeholders at the largest ID.
type reMessages struct {
	mu         sync.Mutex
	calls      []string
	persistErr error
	next       int64
	raw        map[messageID][]byte
	closed     bool
}

func (m *reMessages) record(call string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
}

func (m *reMessages) persist(_ context.Context, d *messageDecomposition) (MessageStoreStats, error) {
	m.record("persist")
	if m.persistErr != nil {
		return MessageStoreStats{Transactions: 1}, m.persistErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw == nil {
		m.raw = map[messageID][]byte{}
	}
	for _, el := range d.elements {
		m.next++
		el.id = messageID{1, m.next}
		m.raw[el.id] = el.raw
	}
	return MessageStoreStats{Written: int64(len(d.elements)), Transactions: 1}, nil
}

func (m *reMessages) refChunks(uses []messageUse) ([][]byte, error) {
	refs := make([]otelsql.MessageRef, len(uses))
	for i, u := range uses {
		id := u.element.id
		if !id.valid() {
			id = messageID{math.MaxInt64, math.MaxInt64}
		}
		refs[i] = otelsql.MessageRef{Writer: id.writer, Seq: id.seq, Attribute: u.attribute, Newest: u.newest, Ordinals: u.ordinals}
	}
	return otelsql.PackRefChunks(refs)
}

func (m *reMessages) reconstruct(ctx context.Context, _ *recordlayer.FDBRecordStore, rs *tracepb.ResourceSpans, _ *queryWork) error {
	m.record("reconstruct")
	p, err := planReconstruct(ctx, []*tracepb.ResourceSpans{rs})
	if err != nil {
		return err
	}
	m.mu.Lock()
	raws := make(map[messageID][]byte, len(p.refs))
	for _, id := range p.refs {
		raws[id] = m.raw[id]
	}
	m.mu.Unlock()
	out, err := p.apply(raws)
	if err != nil {
		return err
	}
	proto.Reset(rs)
	proto.Merge(rs, out[0])
	return nil
}

func (m *reMessages) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
}

// reMessageParts supplies hooks to the exporters it opens.
func reMessageParts(hooks *reMessages) relParts {
	return relParts{messages: func(*tenantStore, ExporterConfig) (relMessageHooks, error) { return hooks, nil }}
}

type delayedBootstrapTransactor struct {
	*retryInjector
	once    sync.Once
	delayed atomic.Bool
}

func (d *delayedBootstrapTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.TransactCtx(context.Background(), fn)
}

func (d *delayedBootstrapTransactor) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return d.retryInjector.TransactCtx(ctx, func(tx fdb.WritableTransaction) (any, error) {
		d.once.Do(func() {
			timer := time.NewTimer(benchTxnTimeout + 100*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				d.delayed.Store(true)
			case <-ctx.Done():
			}
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return fn(tx)
	})
}
