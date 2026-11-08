package plugin

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/relational/api"
)

func sqlModel(tenant, rawSQL string, format int) map[string]any {
	m := map[string]any{"queryType": queryTypeSQL, "rawSql": rawSQL, "format": format}
	if tenant != "" {
		m["tenant"] = tenant
	}
	return m
}

func TestSQLQueryRunsReadOnlyPerTenant(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"allowedSchemas": []string{"T*"}})
	h.sql.answer = func(dsn, query string) fakeResult {
		return fakeResult{
			cols:  []string{"time", "TRACE_ID", "n"},
			types: []string{"BIGINT", "BINARY", "BIGINT"},
			rows:  [][]driver.Value{{int64(1_700_000_000_000_000_000), []byte{0xab}, int64(3)}},
		}
	}
	tr := backend.TimeRange{From: time.Unix(1_700_000_000, 0), To: time.Unix(1_700_000_060, 0)}
	r := h.query(t, sqlModel("", "SELECT $__time(start_ns), trace_id, n FROM spans WHERE $__timeFilter(start_ns)", 1), tr)
	f := onlyFrame(t, r)
	if q := h.sql.queryLog(); len(q) != 1 || q[0] != `SELECT start_ns AS "time", trace_id, n FROM spans WHERE start_ns >= 1700000000000000000 AND start_ns < 1700000060000000000` {
		t.Fatalf("executed %q", q)
	}
	if len(h.sql.dsns) != 1 || !strings.Contains(h.sql.dsns[0], "schema=T1") || !strings.Contains(h.sql.dsns[0], "read_only=true") {
		t.Fatalf("dsns %v", h.sql.dsns)
	}
	if f.Fields[0].Type() != data.FieldTypeNullableTime || *f.Fields[1].At(0).(*string) != "ab" {
		t.Fatalf("converters not applied: %s %v", f.Fields[0].Type(), f.Fields[1].At(0))
	}
	links := f.Fields[1].Config.Links
	if len(links) != 1 || links[0].Internal.Query.(map[string]any)["queryType"] != queryTypeTrace || links[0].Internal.Query.(map[string]any)["tenant"] != "T1" {
		t.Fatalf("trace_id link %+v", links)
	}
	if f.Meta.ExecutedQueryString == "" {
		t.Fatal("executed SQL not reported")
	}

	// A second tenant gets its own pool; the first is reused.
	h.query(t, sqlModel("T7", "SELECT 1", 1), tr)
	h.query(t, sqlModel("T1", "SELECT 1", 1), tr)
	if len(h.sql.dsns) != 2 || !strings.Contains(h.sql.dsns[1], "schema=T7") {
		t.Fatalf("dsns %v", h.sql.dsns)
	}
	if h.fdbOpens != 1 {
		t.Fatalf("FDB opened %d times; one database per instance", h.fdbOpens)
	}
}

func TestSQLRefusesTenantBeforeConnecting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"allowedSchemas": []string{"T1"}})
	for _, tenant := range []string{"T2", "$tenant", "T1; DROP", "t1"} {
		r := h.query(t, sqlModel(tenant, "SELECT 1", 1), backend.TimeRange{})
		var te *TenantError
		if r.Error == nil || !strings.Contains(r.Error.Error(), "refused") || errors.As(r.Error, &te) && te.Tenant != tenant {
			t.Errorf("%s: %v", tenant, r.Error)
		}
	}
	if h.sql.newCalls != 0 || h.fdbOpens != 0 {
		t.Fatalf("a refused tenant reached FDB: %d connectors, %d opens", h.sql.newCalls, h.fdbOpens)
	}
}

func TestSQLNoDefaultSchema(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"defaultSchema": nil})
	if r := h.query(t, sqlModel("", "SELECT 1", 1), backend.TimeRange{}); r.Error == nil || !strings.Contains(r.Error.Error(), "no default schema") {
		t.Fatalf("got %v", r.Error)
	}
}

func TestSQLErrorCarriesSQLSTATE(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sql.answer = func(string, string) fakeResult {
		return fakeResult{err: api.NewError(api.ErrorCode("0A000"), "read-only connection refuses INSERT")}
	}
	r := h.query(t, sqlModel("", "INSERT INTO t VALUES (1)", 1), backend.TimeRange{})
	if r.Error == nil || !strings.Contains(r.Error.Error(), "0A000") {
		t.Fatalf("got %v", r.Error)
	}
}

func TestSQLConnectionArgsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	m := sqlModel("", "SELECT 1", 1)
	m["connectionArgs"] = map[string]any{"schema": "T9"}
	if r := h.query(t, m, backend.TimeRange{}); r.Error == nil {
		t.Fatal("connectionArgs bypassed the tenant")
	}
	for _, dsn := range h.sql.dsns {
		if strings.Contains(dsn, "T9") {
			t.Fatalf("connected to %s", dsn)
		}
	}
}

func TestSQLRowLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"sqlMaxRows": 2})
	h.sql.answer = func(string, string) fakeResult {
		return fakeResult{cols: []string{"n"}, types: []string{"BIGINT"}, rows: [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}}}
	}
	f := onlyFrame(t, h.query(t, sqlModel("", "SELECT n FROM t", 1), backend.TimeRange{}))
	if n, _ := f.RowLen(); n != 2 || len(f.Meta.Notices) != 1 {
		t.Fatalf("%d rows, notices %+v", n, f.Meta.Notices)
	}
	if !strings.Contains(h.sql.dsns[0], "max_rows=2") {
		t.Fatalf("dsn %s", h.sql.dsns[0])
	}
}

// The LRU bounds tenant pools: past capacity the least recently used pool
// closes, and a pool in use closes only when its query finishes.
func TestTenantPoolsAreBounded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for i := range tenantPoolCapacity + 1 {
		if r := h.query(t, sqlModel(fmt.Sprintf("T%d", i), "SELECT 1", 1), backend.TimeRange{}); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	keys := h.ds.sqlPools.keys()
	if len(keys) != tenantPoolCapacity || keys[0] != fmt.Sprintf("T%d", tenantPoolCapacity) {
		t.Fatalf("resident pools %v", keys)
	}
	for _, k := range keys {
		if k == "T0" {
			t.Fatal("T0 still resident")
		}
	}
	// T0 reopens on demand with a fresh connector.
	h.query(t, sqlModel("T0", "SELECT 1", 1), backend.TimeRange{})
	if h.sql.newCalls != tenantPoolCapacity+2 {
		t.Fatalf("%d connectors", h.sql.newCalls)
	}
}

func TestTraceQueryFrameAndLinks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"traceMaxSpans": 3, "traceMaxBytes": 4096})
	r := h.reader("T1")
	r.trace = func(id string) ([]*tracepb.ResourceSpans, traceReadStats, error) {
		return fullTrace(), traceReadStats{Spans: 3, Bytes: 4097, Truncated: true, Reason: "max_bytes,messages"}, nil
	}
	f := onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange))
	if r.lastBudget != (traceBudget{MaxSpans: 3, MaxBytes: 4096}) {
		t.Fatalf("budget %+v", r.lastBudget)
	}
	if f.RefID != "A" || f.Meta.PreferredVisualization != data.VisTypeTrace {
		t.Fatalf("frame %q %+v", f.RefID, f.Meta)
	}
	if len(f.Meta.Notices) != 1 || !strings.Contains(f.Meta.Notices[0].Text, "4096") || !strings.Contains(f.Meta.Notices[0].Text, "manifests") {
		t.Fatalf("notices %+v", f.Meta.Notices)
	}
	span, _ := f.FieldByName("spanID")
	if len(span.Config.Links) != 2 {
		t.Fatalf("span links %+v", span.Config.Links)
	}
	perSpan := span.Config.Links[0].Internal
	perTrace := span.Config.Links[1].Internal.Query.(map[string]any)
	q := perSpan.Query.(map[string]any)
	if perSpan.DatasourceUID != testUID || q["queryType"] != queryTypeLogs || q["traceId"] != rowTraceID || q["spanId"] != valueRaw || q["tenant"] != "T1" {
		t.Fatalf("span → logs link %+v", perSpan)
	}
	if _, ok := perTrace["spanId"]; ok || perTrace["traceId"] != rowTraceID {
		t.Fatalf("trace → logs link %+v", perTrace)
	}
	raw, err := json.Marshal(f)
	if err != nil || !strings.Contains(string(raw), `"datasourceUid":"ds-uid-1"`) {
		t.Fatalf("links not serialized: %v", err)
	}
}

func TestTraceQueryNotFoundAndErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	f := onlyFrame(t, h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange))
	if n, _ := f.RowLen(); n != 0 || len(f.Meta.Notices) != 1 || !strings.Contains(f.Meta.Notices[0].Text, "not found") {
		t.Fatalf("not found: %d rows %+v", n, f.Meta.Notices)
	}
	h.reader("T1").err = fdb.Error{Code: 1007}
	r := h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "FDB error 1007") {
		t.Fatalf("got %v", r.Error)
	}
	if r := h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": "nope"}, searchRange); r.Error == nil {
		t.Fatal("bad trace ID accepted")
	}
}

func TestOTelModes(t *testing.T) {
	t.Parallel()
	off := newHarness(t, map[string]any{"otelMode": "off"})
	r := off.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "otelMode off") || len(off.readers) != 0 {
		t.Fatalf("otelMode off: %v", r.Error)
	}
	on := newHarness(t, nil)
	on.readerErr["T1"] = &otel.OwnershipError{OtelError: &otel.OtelError{Kind: "ownership", Op: "open", Message: "no STORE_META layout"}}
	r = on.query(t, map[string]any{"queryType": queryTypeLogs, "traceId": goldenTraceID}, searchRange)
	if r.Error == nil || !strings.Contains(r.Error.Error(), "not an OTEL_V1 store") {
		t.Fatalf("non-OTel tenant: %v", r.Error)
	}
}

func TestUnknownQueryType(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if r := h.query(t, map[string]any{"queryType": "traceql"}, searchRange); r.Error == nil {
		t.Fatal("unknown query type accepted")
	}
}

func TestSettingsErrorSurfacesOnQueries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{"database": nil})
	r := h.query(t, sqlModel("", "SELECT 1", 1), backend.TimeRange{})
	if r.Error == nil || !strings.Contains(r.Error.Error(), "database") {
		t.Fatalf("got %v", r.Error)
	}
	if h.fdbOpens != 0 {
		t.Fatal("opened FDB with invalid settings")
	}
}

// Dispose closes the FDB database only after in-flight requests finish, and
// refuses requests that arrive later.
func TestDisposeWaitsForInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	started, unblock := make(chan struct{}), make(chan struct{})
	r := h.reader("T1")
	var once sync.Once
	r.trace = func(string) ([]*tracepb.ResourceSpans, traceReadStats, error) {
		once.Do(func() { close(started) })
		<-unblock
		return nil, traceReadStats{}, nil
	}
	done := make(chan backend.DataResponse)
	go func() {
		done <- h.query(t, map[string]any{"queryType": queryTypeTrace, "traceId": goldenTraceID}, searchRange)
	}()
	<-started
	h.ds.Dispose()
	h.mu.Lock()
	closes := h.fdbCloses
	h.mu.Unlock()
	if closes != 0 || r.closed {
		t.Fatal("closed while a query was running")
	}
	if _, err := h.ds.QueryData(context.Background(), &backend.QueryDataRequest{}); !errors.Is(err, errDisposed) {
		t.Fatalf("after Dispose: %v", err)
	}
	close(unblock)
	if res := <-done; res.Error != nil {
		t.Fatalf("in-flight query failed: %v", res.Error)
	}
	h.mu.Lock()
	closes = h.fdbCloses
	h.mu.Unlock()
	if closes != 1 || !r.closed {
		t.Fatalf("after the last request: %d FDB closes, reader closed %v", closes, r.closed)
	}
}
