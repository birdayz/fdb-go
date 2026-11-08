package plugin

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

func TestMain(m *testing.M) {
	if err := fdb.APIVersion(APIVersion); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// fakeResult is a canned result set.
type fakeResult struct {
	cols  []string
	types []string // DatabaseTypeName per column
	rows  [][]driver.Value
	err   error
}

// fakeSQL is a database/sql connector factory recording DSNs and queries.
type fakeSQL struct {
	mu       sync.Mutex
	dsns     []string
	queries  []string
	answer   func(dsn, query string) fakeResult
	closed   int
	newCalls int
}

func (f *fakeSQL) factory(_ *recordlayer.FDBDatabase, dsn string) (driver.Connector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	f.dsns = append(f.dsns, dsn)
	return &fakeConnector{f: f, dsn: dsn}, nil
}

func (f *fakeSQL) queryLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

type fakeConnector struct {
	f   *fakeSQL
	dsn string
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c: c}, nil }
func (c *fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

type fakeConn struct{ c *fakeConnector }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *fakeConn) Close() error {
	c.c.f.mu.Lock()
	c.c.f.closed++
	c.c.f.mu.Unlock()
	return nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	f := c.c.f
	f.mu.Lock()
	f.queries = append(f.queries, query)
	answer := f.answer
	f.mu.Unlock()
	res := fakeResult{cols: []string{"1"}, types: []string{"BIGINT"}, rows: [][]driver.Value{{int64(1)}}}
	if answer != nil {
		res = answer(c.c.dsn, query)
	}
	if res.err != nil {
		return nil, res.err
	}
	return &fakeRows{res: res}, nil
}

type fakeRows struct {
	res fakeResult
	i   int
}

func (r *fakeRows) Columns() []string { return r.res.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.res.rows) {
		return io.EOF
	}
	copy(dest, r.res.rows[r.i])
	r.i++
	return nil
}
func (r *fakeRows) ColumnTypeDatabaseTypeName(i int) string { return r.res.types[i] }
func (r *fakeRows) ColumnTypeNullable(int) (bool, bool)     { return true, true }
func (r *fakeRows) ColumnTypeScanType(i int) reflect.Type {
	switch r.res.types[i] {
	case "BIGINT":
		return reflect.TypeFor[int64]()
	case "STRING":
		return reflect.TypeFor[string]()
	case "DOUBLE":
		return reflect.TypeFor[float64]()
	case "BINARY":
		return reflect.TypeFor[[]byte]()
	default:
		return reflect.TypeFor[any]()
	}
}

// fakeReader is an otelReader with canned answers and a call log.
type fakeReader struct {
	mu     sync.Mutex
	schema string
	calls  []string
	closed bool

	layout       string
	trace        func(id string) ([]*tracepb.ResourceSpans, otel.TraceReadStats, error)
	sameSpan     otel.SpanQueryPage
	existential  otel.TraceExistentialResult
	queryResult  otel.QueryResult
	timeResult   otel.QueryResult
	logs         []otel.LogRecord
	keys         []string
	values       []string
	truncated    bool
	err          error
	stale        int // TraceOTLP fails stale this many times
	lastSameSpan otel.SameSpanQuery
	lastExist    otel.TraceExistentialQuery
	lastAttr     otel.Attribute
	lastTime     otel.TimeRange
	lastBudget   otel.TraceBudget
	lastLogsArgs []any
	lastAttrArgs []any
}

func (r *fakeReader) record(call string) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
}

func (r *fakeReader) Layout() string { return r.layout }

func (r *fakeReader) TraceOTLP(_ context.Context, id string, b otel.TraceBudget) ([]*tracepb.ResourceSpans, otel.TraceReadStats, error) {
	r.record("TraceOTLP " + id)
	r.lastBudget = b
	r.mu.Lock()
	stale := r.stale > 0
	if stale {
		r.stale--
	}
	r.mu.Unlock()
	if stale {
		return nil, otel.TraceReadStats{}, &otel.StaleMetadataError{OtelError: &otel.OtelError{Kind: "stale", Op: "trace", Message: "metadata changed"}}
	}
	if r.err != nil {
		return nil, otel.TraceReadStats{}, r.err
	}
	if r.trace == nil {
		return nil, otel.TraceReadStats{}, nil
	}
	return r.trace(id)
}

func (r *fakeReader) QuerySameSpan(_ context.Context, q otel.SameSpanQuery) (otel.SpanQueryPage, error) {
	r.record("QuerySameSpan")
	r.lastSameSpan = q
	return r.sameSpan, r.err
}

func (r *fakeReader) QueryTraceExistential(_ context.Context, q otel.TraceExistentialQuery) (otel.TraceExistentialResult, error) {
	r.record("QueryTraceExistential")
	r.lastExist = q
	return r.existential, r.err
}

func (r *fakeReader) Query(_ context.Context, a otel.Attribute) (otel.QueryResult, error) {
	r.record("Query")
	r.lastAttr = a
	return r.queryResult, r.err
}

func (r *fakeReader) Time(_ context.Context, tr otel.TimeRange) (otel.QueryResult, error) {
	r.record("Time")
	r.lastTime = tr
	return r.timeResult, r.err
}

func (r *fakeReader) QueryLogsByTrace(_ context.Context, traceID, spanID string) ([]otel.LogRecord, error) {
	r.record("QueryLogsByTrace")
	r.lastLogsArgs = []any{traceID, spanID}
	return r.logs, r.err
}

func (r *fakeReader) QueryLogsByAttribute(_ context.Context, a otel.Attribute, limit int) ([]otel.LogRecord, error) {
	r.record("QueryLogsByAttribute")
	r.lastLogsArgs = []any{a, limit}
	return r.logs, r.err
}

func (r *fakeReader) AttributeKeys(_ context.Context, signal, scope, typ, prefix string, limit int) ([]string, bool, error) {
	r.record("AttributeKeys")
	r.lastAttrArgs = []any{signal, scope, typ, prefix, limit}
	return r.keys, r.truncated, r.err
}

func (r *fakeReader) AttributeValues(_ context.Context, signal string, a otel.Attribute, prefix string, limit int) ([]string, bool, error) {
	r.record("AttributeValues")
	r.lastAttrArgs = []any{signal, a, prefix, limit}
	return r.values, r.truncated, r.err
}

func (r *fakeReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

func (r *fakeReader) callLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// harness wires a Datasource to fakes.
type harness struct {
	ds        *Datasource
	sql       *fakeSQL
	readers   map[string]*fakeReader
	openErr   error
	readerErr map[string]error
	opens     map[string]int
	fdbOpens  int
	fdbCloses int
	readerNew func(schema string) *fakeReader
	mu        sync.Mutex
}

const testUID = "ds-uid-1"

func instanceSettings(t testing.TB, jsonData map[string]any) backend.DataSourceInstanceSettings {
	t.Helper()
	base := map[string]any{"clusterString": "test:abc@127.0.0.1:4500", "database": "/OTEL/R_TEST", "defaultSchema": "T1"}
	for k, v := range jsonData {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return backend.DataSourceInstanceSettings{UID: testUID, Name: "FDB", Type: PluginID, JSONData: raw}
}

func newHarness(t testing.TB, jsonData map[string]any) *harness {
	t.Helper()
	h := &harness{sql: &fakeSQL{}, readers: map[string]*fakeReader{}, readerErr: map[string]error{}, opens: map[string]int{}}
	d := deps{
		openFDB: func(context.Context, *Settings, backend.DataSourceInstanceSettings) (*fdbHandle, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.openErr != nil {
				return nil, h.openErr
			}
			h.fdbOpens++
			return &fdbHandle{close: func() {
				h.mu.Lock()
				h.fdbCloses++
				h.mu.Unlock()
			}}, nil
		},
		openReader: func(_ context.Context, _ *recordlayer.FDBDatabase, _, schema string, _ otel.ReaderConfig) (otelReader, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.opens[schema]++
			if err := h.readerErr[schema]; err != nil {
				return nil, err
			}
			r, ok := h.readers[schema]
			if !ok {
				if h.readerNew != nil {
					r = h.readerNew(schema)
				} else {
					r = &fakeReader{layout: otel.StoreLayoutDeltas}
				}
				r.schema = schema
				h.readers[schema] = r
			}
			return r, nil
		},
		newConnector: h.sql.factory,
	}
	h.ds = newDatasource(instanceSettings(t, jsonData), d)
	t.Cleanup(h.ds.Dispose)
	return h
}

func (h *harness) reader(schema string) *fakeReader {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.readers[schema]
	if !ok {
		r = &fakeReader{layout: otel.StoreLayoutDeltas, schema: schema}
		h.readers[schema] = r
	}
	return r
}

// query runs one DataQuery through QueryData and returns its response.
func (h *harness) query(t testing.TB, model map[string]any, tr backend.TimeRange) backend.DataResponse {
	t.Helper()
	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	qt, _ := model["queryType"].(string)
	inst := h.ds.inst
	resp, err := h.ds.QueryData(context.Background(), &backend.QueryDataRequest{
		PluginContext: backend.PluginContext{DataSourceInstanceSettings: &inst},
		Queries:       []backend.DataQuery{{RefID: "A", QueryType: qt, JSON: raw, TimeRange: tr}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Responses["A"]
}
