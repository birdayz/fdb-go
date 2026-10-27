package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type fakeCHNativeRows struct {
	driver.Rows
	values                               [][]any
	index                                int
	scanError, terminalError, closeError error
	onScan                               func()
	closed                               atomic.Int64
	scanned                              atomic.Int64
}

func (r *fakeCHNativeRows) Next() bool {
	if r.index >= len(r.values) {
		return false
	}
	r.index++
	return true
}

func (r *fakeCHNativeRows) Scan(dest ...any) error {
	r.scanned.Add(1)
	if r.onScan != nil {
		r.onScan()
	}
	if r.scanError != nil {
		return r.scanError
	}
	if r.index < 1 || r.index > len(r.values) {
		return errors.New("Scan without a current native row")
	}
	row := r.values[r.index-1]
	if len(dest) != len(row) {
		return fmt.Errorf("native Scan columns=%d, want %d", len(dest), len(row))
	}
	for i, value := range row {
		pointer := reflect.ValueOf(dest[i])
		if pointer.Kind() != reflect.Pointer || pointer.IsNil() {
			return fmt.Errorf("native Scan destination %d is not a pointer", i)
		}
		source := reflect.ValueOf(value)
		if !source.IsValid() || !source.Type().AssignableTo(pointer.Elem().Type()) {
			return fmt.Errorf("native Scan column %d: %T is not assignable to %T", i, value, dest[i])
		}
		pointer.Elem().Set(source)
	}
	return nil
}

func (r *fakeCHNativeRows) Err() error   { return r.terminalError }
func (r *fakeCHNativeRows) Close() error { r.closed.Add(1); return r.closeError }

type nativeOptionSnapshot struct {
	id         string
	parameters map[string]string
	settings   map[string]string
}

// v2.43.0 exposes QueryOption callbacks but no option getters. Inspect only the
// pinned QueryOptions value, never private context structure or unsafe memory.
func nativeOptionsForTest(ctx context.Context) (nativeOptionSnapshot, error) {
	out := nativeOptionSnapshot{parameters: map[string]string{}, settings: map[string]string{}}
	var inspectionError error
	clickhouse.Context(ctx, func(options *clickhouse.QueryOptions) error {
		value := reflect.ValueOf(options).Elem()
		id, parameters, settings := value.FieldByName("queryID"), value.FieldByName("parameters"), value.FieldByName("settings")
		if !id.IsValid() || id.Kind() != reflect.String || !parameters.IsValid() || parameters.Kind() != reflect.Map || parameters.Type().Key().Kind() != reflect.String || parameters.Type().Elem().Kind() != reflect.String || !settings.IsValid() || settings.Kind() != reflect.Map || settings.Type().Key().Kind() != reflect.String {
			inspectionError = errors.New("pinned clickhouse-go QueryOptions fields changed; inspect the new driver API")
			return nil
		}
		out.id = id.String()
		params := parameters.MapRange()
		for params.Next() {
			out.parameters[params.Key().String()] = params.Value().String()
		}
		entries := settings.MapRange()
		for entries.Next() {
			entry := entries.Value()
			for entry.Kind() == reflect.Interface {
				entry = entry.Elem()
			}
			switch entry.Kind() {
			case reflect.String:
				out.settings[entries.Key().String()] = entry.String()
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				out.settings[entries.Key().String()] = strconv.FormatInt(entry.Int(), 10)
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				out.settings[entries.Key().String()] = strconv.FormatUint(entry.Uint(), 10)
			case reflect.Bool:
				out.settings[entries.Key().String()] = strconv.FormatBool(entry.Bool())
			default:
				inspectionError = fmt.Errorf("unsupported native setting kind %s", entry.Kind())
			}
		}
		return nil
	})
	return out, inspectionError
}

type (
	nativeTestMarker struct{}
	fakeCHNativeCall struct {
		sql         string
		options     nativeOptionSnapshot
		deadline    time.Time
		hasDeadline bool
		marker      any
		args        int
	}
)

type fakeCHNativeConn struct {
	mu         sync.Mutex
	calls      []fakeCHNativeCall
	query      func(int, context.Context, string) (driver.Rows, error)
	closeError error
	closed     atomic.Int64
}

func (c *fakeCHNativeConn) Query(ctx context.Context, sql string, args ...any) (driver.Rows, error) {
	options, err := nativeOptionsForTest(ctx)
	if err != nil {
		return nil, err
	}
	deadline, bounded := ctx.Deadline()
	c.mu.Lock()
	index := len(c.calls)
	c.calls = append(c.calls, fakeCHNativeCall{sql: sql, options: options, deadline: deadline, hasDeadline: bounded, marker: ctx.Value(nativeTestMarker{}), args: len(args)})
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.query(index, ctx, sql)
}

func (c *fakeCHNativeConn) Close() error { c.closed.Add(1); return c.closeError }
func (c *fakeCHNativeConn) snapshot() []fakeCHNativeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeCHNativeCall(nil), c.calls...)
}

func nativeCHBackendForTest(t *testing.T) (*CHBackend, *fakeCHNativeConn, *fakeCHNativeRows) {
	t.Helper()
	b := chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("native ownership/data query reached HTTP administration endpoint: %s", r.URL.Query().Get("query"))
		http.Error(w, "native query used HTTP", http.StatusBadRequest)
	})
	b.cfg.CHQueryTransport, b.cfg.CHNativeAddr = "native", "127.0.0.1:9000"
	payload, err := json.Marshal(manifest(b.cfg, "ready"))
	if err != nil {
		t.Fatal(err)
	}
	owner := &fakeCHNativeRows{values: [][]any{{string(payload)}}}
	conn := &fakeCHNativeConn{}
	b.queries = &chNativeQueries{conn: conn, cfg: b.cfg}
	return b, conn, owner
}

func assertNativeWork(t *testing.T, work QueryResult, id string) {
	t.Helper()
	if id == "" || work.QueryID != id || work.QueryTransport != "native" || work.ServerMetricsSource != "unavailable" || work.ServerSummary || work.ServerReadRows != 0 || work.ServerReadBytes != 0 || work.ServerElapsed != 0 || work.ResponseBytes != 0 || work.Candidates != 0 || work.Bytes != 0 {
		t.Fatalf("native query fabricated metrics or lost provenance: got=%+v want ID=%q", work, id)
	}
}

func assertNativeCalls(t *testing.T, calls []fakeCHNativeCall, expectedParams map[string]string) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("native owner/data calls=%d, want 2: %+v", len(calls), calls)
	}
	ids := map[string]bool{}
	for i, call := range calls {
		if call.options.id == "" || ids[call.options.id] {
			t.Fatalf("missing/reused native query ID: %+v", calls)
		}
		ids[call.options.id] = true
		if strings.Contains(call.sql, "FORMAT ") || call.args != 0 {
			t.Fatalf("native SQL retained HTTP format or positional args: %+v", call)
		}
		if !call.hasDeadline || time.Until(call.deadline) <= 0 || time.Until(call.deadline) > 31*time.Second {
			t.Fatalf("native call lacks bounded 30s context: %+v", call)
		}
		if cache, ok := call.options.settings["use_query_cache"]; !ok || cache != "0" && cache != "false" {
			t.Fatalf("native query cache not explicitly disabled: %+v", call.options)
		}
		if i == 0 && (len(call.options.parameters) != 0 || !strings.Contains(call.sql, ".owner ")) {
			t.Fatalf("native owner SQL/options: %+v", call)
		}
	}
	if !reflect.DeepEqual(calls[1].options.parameters, expectedParams) {
		t.Fatalf("native bound parameters=%#v, want %#v", calls[1].options.parameters, expectedParams)
	}
}

func TestCHNativePublicTypedQueries(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			b, conn, owner := nativeCHBackendForTest(t)
			traceA, traceZ := strings.Repeat("a", 32), strings.Repeat("f", 32)
			spanA, spanZ := strings.Repeat("a", 16), strings.Repeat("f", 16)
			attribute := Attribute{"scope'&=+\n雪", "key' OR 1=1 --&?%", "type\\+", "value'); DROP DATABASE other;--\n&=+雪"}
			id := "id' OR 1=1 --&=+%\n雪"
			data := &fakeCHNativeRows{}
			params := map[string]string{"tenant": strconv.Itoa(b.cfg.Tenant)}
			switch op {
			case "query":
				data.values = [][]any{{traceZ, int64(9)}, {traceA, int64(9)}}
				key, err := json.Marshal([]string{attribute.Scope, attribute.Key, attribute.Type})
				if err != nil {
					t.Fatal(err)
				}
				params["scope"], params["key"], params["typ"], params["value"], params["mapkey"] = attribute.Scope, attribute.Key, attribute.Type, attribute.Value, string(key)
			case "trace":
				data.values = [][]any{{spanA, int64(4)}, {spanZ, int64(9)}}
				params["trace"] = id
			case "span":
				data.values = [][]any{{traceA, spanA, int64(4)}, {traceZ, spanA, int64(9)}}
				params["span"] = id
			case "time":
				data.values = [][]any{{traceZ, int64(9)}, {traceA, int64(9)}}
				params["start"], params["end"] = "-1", "100"
			}
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index == 0 {
					return owner, nil
				}
				return data, nil
			}
			ctx := context.WithValue(t.Context(), nativeTestMarker{}, "preserved")
			var returned QueryResult
			switch op {
			case "query":
				var err error
				returned, err = b.Query(ctx, attribute)
				if err != nil || !reflect.DeepEqual(returned.Hits, []Hit{{Trace: traceZ, Latest: 9}, {Trace: traceA, Latest: 9}}) {
					t.Fatalf("native Query=%+v error=%v", returned, err)
				}
			case "trace":
				got, err := b.Trace(ctx, id)
				if err != nil || !reflect.DeepEqual(got, []SpanHit{{ID: spanA, Start: 4}, {ID: spanZ, Start: 9}}) {
					t.Fatalf("native Trace=%+v error=%v", got, err)
				}
			case "span":
				b.cfg.K = 1
				b.queries.(*chNativeQueries).cfg = b.cfg
				got, err := b.Span(ctx, id)
				if err != nil || !reflect.DeepEqual(got, []SpanMatch{{Trace: traceA, ID: spanA, Start: 4}, {Trace: traceZ, ID: spanA, Start: 9}}) {
					t.Fatalf("native standalone Span truncated matches at K=1: %+v error=%v", got, err)
				}
			case "time":
				var err error
				returned, err = b.Time(ctx, TimeRange{Start: -1, End: 100})
				if err != nil || !reflect.DeepEqual(returned.Hits, []Hit{{Trace: traceZ, Latest: 9}, {Trace: traceA, Latest: 9}}) {
					t.Fatalf("native Time=%+v error=%v", returned, err)
				}
			}
			calls := conn.snapshot()
			assertNativeCalls(t, calls, params)
			for _, call := range calls {
				if call.marker != "preserved" {
					t.Fatalf("native query discarded parent context value: %+v", call)
				}
			}
			assertCHBudgets(t, calls[1].sql, b.cfg)
			for _, literal := range []string{attribute.Scope, attribute.Key, attribute.Type, attribute.Value, id} {
				if strings.Contains(calls[1].sql, literal) {
					t.Errorf("native query interpolated literal %q", literal)
				}
			}
			assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
			if op == "query" || op == "time" {
				assertNativeWork(t, returned, calls[1].options.id)
			}
			if owner.closed.Load() != 1 || data.closed.Load() != 1 || owner.scanned.Load() != 1 || data.scanned.Load() != 2 {
				t.Fatalf("native row lifecycle owner close/scan=%d/%d data close/scan=%d/%d", owner.closed.Load(), owner.scanned.Load(), data.closed.Load(), data.scanned.Load())
			}
		})
	}
}

func nativePublicResult(ctx context.Context, b *CHBackend, op string) (QueryResult, error) {
	if op == "query" {
		return b.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
	}
	if op == "time" {
		return b.Time(ctx, TimeRange{Start: 0, End: 1})
	}
	return QueryResult{}, chPublicOperation(ctx, b, op)
}

func nativeDataForOperation(op string) *fakeCHNativeRows {
	trace, id := strings.Repeat("a", 32), strings.Repeat("a", 16)
	switch op {
	case "trace":
		return &fakeCHNativeRows{values: [][]any{{id, int64(1)}}}
	case "span":
		return &fakeCHNativeRows{values: [][]any{{trace, id, int64(1)}}}
	default:
		return &fakeCHNativeRows{values: [][]any{{trace, int64(1)}}}
	}
}

func nativeTwoRowsForOperation(op string) *fakeCHNativeRows {
	traceA, traceZ := strings.Repeat("a", 32), strings.Repeat("f", 32)
	spanA, spanZ := strings.Repeat("a", 16), strings.Repeat("f", 16)
	switch op {
	case "trace":
		return &fakeCHNativeRows{values: [][]any{{spanA, int64(1)}, {spanZ, int64(2)}}}
	case "span":
		return &fakeCHNativeRows{values: [][]any{{traceA, spanA, int64(1)}, {traceZ, spanA, int64(2)}}}
	default:
		return &fakeCHNativeRows{values: [][]any{{traceZ, int64(2)}, {traceA, int64(1)}}}
	}
}

func TestCHNativePublicDataFailuresKeepCurrentID(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		for _, stage := range []string{"query", "query_with_rows", "scan", "terminal", "close", "scan_and_close", "terminal_and_close"} {
			t.Run(op+"/"+stage, func(t *testing.T) {
				t.Parallel()
				b, conn, owner := nativeCHBackendForTest(t)
				data := nativeDataForOperation(op)
				cause, closeCause := errors.New("controlled native data failure"), errors.New("controlled native close failure")
				if strings.Contains(stage, "scan") {
					data.scanError = cause
				}
				if strings.Contains(stage, "terminal") {
					data.terminalError = cause
				}
				if stage == "close" {
					data.closeError = cause
				} else if strings.Contains(stage, "and_close") || stage == "query_with_rows" {
					data.closeError = closeCause
				}
				conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
					if index == 0 {
						return owner, nil
					}
					if stage == "query" {
						return nil, cause
					}
					if stage == "query_with_rows" {
						return data, cause
					}
					return data, nil
				}
				b.lastWork = QueryResult{QueryID: "stale", QueryTransport: "http", ServerMetricsSource: "stale", ServerSummary: true, ServerReadRows: 7, ServerReadBytes: 13, ServerElapsed: 17, ResponseBytes: 19, Candidates: 23, Bytes: 29}
				returned, err := nativePublicResult(t.Context(), b, op)
				if !errors.Is(err, cause) || strings.Contains(stage, "and_close") && !errors.Is(err, closeCause) {
					t.Fatalf("native %s/%s lost underlying failure(s): %v", op, stage, err)
				}
				calls := conn.snapshot()
				if len(calls) != 2 {
					t.Fatalf("native failure calls=%d, want owner+data", len(calls))
				}
				assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
				if op == "query" || op == "time" {
					assertNativeWork(t, returned, calls[1].options.id)
				}
				wantClosed := int64(1)
				if stage == "query" {
					wantClosed = 0
				}
				if stage == "query_with_rows" {
					wantClosed = 0
				}
				if owner.closed.Load() != 1 || data.closed.Load() != wantClosed {
					t.Fatalf("native failure leaked/double-closed rows: owner=%d data=%d want=%d", owner.closed.Load(), data.closed.Load(), wantClosed)
				}
				if stage == "query" || stage == "query_with_rows" {
					if data.index != 0 || data.scanned.Load() != 0 || errors.Is(err, closeCause) {
						t.Fatalf("Query-error rows were inspected or closed: next=%d scan=%d error=%v", data.index, data.scanned.Load(), err)
					}
				}
			})
		}
	}
}

type queryErrorNativeRows struct{ driver.Rows }

func (*queryErrorNativeRows) Next() bool        { panic("Next called on Query-error rows") }
func (*queryErrorNativeRows) Scan(...any) error { panic("Scan called on Query-error rows") }
func (*queryErrorNativeRows) Err() error        { panic("Err called on Query-error rows") }
func (*queryErrorNativeRows) Close() error      { panic("Close called on Query-error rows") }

func TestCHNativeQueryErrorTypedNilRowsAreNeverUsed(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		for _, phase := range []string{"owner", "data"} {
			t.Run(op+"/"+phase, func(t *testing.T) {
				t.Parallel()
				defer func() {
					if fault := recover(); fault != nil {
						t.Fatalf("Query-error rows must never be inspected or closed: %v", fault)
					}
				}()
				b, conn, owner := nativeCHBackendForTest(t)
				cause := errors.New("native Query returned typed-nil rows and an error")
				var pointer *queryErrorNativeRows
				var rejected driver.Rows = pointer
				if !reflect.ValueOf(rejected).IsNil() {
					t.Fatal("regression did not construct a typed-nil rows interface")
				}
				conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
					if phase == "data" && index == 0 {
						return owner, nil
					}
					return rejected, cause
				}
				b.lastWork = QueryResult{QueryID: "stale", QueryTransport: "http", ServerMetricsSource: "stale", ServerSummary: true, ServerReadRows: 7, ResponseBytes: 19}
				returned, err := nativePublicResult(t.Context(), b, op)
				if !errors.Is(err, cause) {
					t.Fatalf("typed-nil Query error lost cause: %v", err)
				}
				calls := conn.snapshot()
				if phase == "owner" {
					if len(calls) != 1 || owner.closed.Load() != 0 || !reflect.DeepEqual(b.LastQueryWork(), QueryResult{}) || !reflect.DeepEqual(returned, QueryResult{}) {
						t.Fatalf("typed-nil ownership error executed data or retained work: calls=%d closes=%d work=%+v returned=%+v", len(calls), owner.closed.Load(), b.LastQueryWork(), returned)
					}
					return
				}
				if len(calls) != 2 || owner.closed.Load() != 1 || owner.scanned.Load() != 1 {
					t.Fatalf("typed-nil data regression lacks successful ownership: calls=%d closes=%d scans=%d", len(calls), owner.closed.Load(), owner.scanned.Load())
				}
				assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
				if op == "query" || op == "time" {
					assertNativeWork(t, returned, calls[1].options.id)
				}
			})
		}
	}
}

func TestCHNativeOwnerFailuresHaveNoDataProvenance(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		for _, stage := range []string{"query", "scan", "terminal", "close", "missing", "duplicate", "malformed", "loading", "wrong_owner"} {
			t.Run(op+"/"+stage, func(t *testing.T) {
				t.Parallel()
				b, conn, owner := nativeCHBackendForTest(t)
				cause := errors.New("controlled native owner failure")
				switch stage {
				case "scan":
					owner.scanError = cause
				case "terminal":
					owner.terminalError = cause
				case "close":
					owner.closeError = cause
				case "missing":
					owner.values = nil
				case "duplicate":
					owner.values = append(owner.values, owner.values[0])
				case "malformed":
					owner.values = [][]any{{"not JSON"}}
				case "loading", "wrong_owner":
					m := manifest(b.cfg, "loading")
					if stage == "wrong_owner" {
						m = manifest(b.cfg, "ready")
						m.Corpus = "other"
					}
					payload, err := json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
					owner.values = [][]any{{string(payload)}}
				}
				conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
					if index != 0 {
						return nil, errors.New("native data executed after failed ownership")
					}
					if stage == "query" {
						return nil, cause
					}
					return owner, nil
				}
				b.lastWork = QueryResult{QueryID: "stale", ServerReadRows: 42, ServerSummary: true}
				returned, err := nativePublicResult(t.Context(), b, op)
				if err == nil {
					t.Fatalf("native %s owner %s failure authorized data", op, stage)
				}
				if stage == "query" || stage == "scan" || stage == "terminal" || stage == "close" {
					if !errors.Is(err, cause) {
						t.Fatalf("owner failure lost cause: %v", err)
					}
				}
				if len(conn.snapshot()) != 1 || !reflect.DeepEqual(b.LastQueryWork(), QueryResult{}) || !reflect.DeepEqual(returned, QueryResult{}) {
					t.Fatalf("owner failure leaked data query/provenance: calls=%+v work=%+v returned=%+v", conn.snapshot(), b.LastQueryWork(), returned)
				}
				wantClosed := int64(1)
				if stage == "query" {
					wantClosed = 0
				}
				if owner.closed.Load() != wantClosed {
					t.Fatalf("owner failure close count=%d want=%d", owner.closed.Load(), wantClosed)
				}
			})
		}
	}
}

func TestCHNativePublicCancellation(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		for _, phase := range []string{"before_owner", "data_query", "data_scan"} {
			t.Run(op+"/"+phase, func(t *testing.T) {
				t.Parallel()
				b, conn, owner := nativeCHBackendForTest(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered := make(chan struct{})
				data := nativeDataForOperation(op)
				if phase == "data_scan" {
					data.onScan = cancel
				}
				conn.query = func(index int, queryCtx context.Context, _ string) (driver.Rows, error) {
					if index == 0 {
						return owner, nil
					}
					if phase == "data_query" {
						close(entered)
						<-queryCtx.Done()
						return nil, queryCtx.Err()
					}
					return data, nil
				}
				b.lastWork = QueryResult{QueryID: "stale", ServerReadRows: 42, ServerSummary: true}
				if phase == "before_owner" {
					cancel()
				}
				result := make(chan error, 1)
				go func() { result <- chPublicOperation(ctx, b, op) }()
				if phase == "data_query" {
					select {
					case <-entered:
						cancel()
					case err := <-result:
						t.Fatalf("native query returned before in-flight cancellation: %v", err)
					case <-time.After(2 * time.Second):
						t.Fatal("native data query never entered fake driver")
					}
				}
				select {
				case err := <-result:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("native cancellation lost cause: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("native query ignored caller cancellation")
				}
				calls := conn.snapshot()
				if phase == "before_owner" {
					if len(calls) != 0 || !reflect.DeepEqual(b.LastQueryWork(), QueryResult{}) {
						t.Fatalf("pre-canceled query executed or retained work: calls=%+v work=%+v", calls, b.LastQueryWork())
					}
					return
				}
				if len(calls) != 2 || owner.closed.Load() != 1 {
					t.Fatalf("canceled owner/data lifecycle: calls=%+v owner closes=%d", calls, owner.closed.Load())
				}
				assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
				wantClosed := int64(0)
				if phase == "data_scan" {
					wantClosed = 1
				}
				if data.closed.Load() != wantClosed {
					t.Fatalf("canceled data rows close count=%d want=%d", data.closed.Load(), wantClosed)
				}
			})
		}
	}
}

func TestCHNativeEmptyAndInvalidTime(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			b, conn, owner := nativeCHBackendForTest(t)
			data := &fakeCHNativeRows{}
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index == 0 {
					return owner, nil
				}
				return data, nil
			}
			switch op {
			case "query":
				got, err := b.Query(t.Context(), Attribute{"span", QueryKey, "string", "absent"})
				if err != nil || got.Hits == nil || len(got.Hits) != 0 {
					t.Fatalf("empty native Query=%+v error=%v", got, err)
				}
			case "trace":
				got, err := b.Trace(t.Context(), "absent")
				if err != nil || got == nil || len(got) != 0 {
					t.Fatalf("empty native Trace=%+v error=%v", got, err)
				}
			case "span":
				got, err := b.Span(t.Context(), "absent")
				if err != nil || got == nil || len(got) != 0 {
					t.Fatalf("empty native Span=%+v error=%v", got, err)
				}
			case "time":
				got, err := b.Time(t.Context(), TimeRange{Start: 3, End: 3})
				if err != nil || got.Hits == nil || len(got.Hits) != 0 {
					t.Fatalf("empty native Time=%+v error=%v", got, err)
				}
			}
			calls := conn.snapshot()
			if len(calls) != 2 || owner.closed.Load() != 1 || data.closed.Load() != 1 {
				t.Fatalf("empty native result leaked row ownership: calls=%d closes=%d/%d", len(calls), owner.closed.Load(), data.closed.Load())
			}
			assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
			_, err := b.Time(t.Context(), TimeRange{Start: 2, End: 1})
			var config *ConfigError
			if !errors.As(err, &config) || len(conn.snapshot()) != 2 || !reflect.DeepEqual(b.LastQueryWork(), QueryResult{}) {
				t.Fatalf("invalid native Time range made query or retained provenance: error=%v calls=%d work=%+v", err, len(conn.snapshot()), b.LastQueryWork())
			}
		})
	}
}

func TestCHNativeCandidateBudget(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			b, conn, owner := nativeCHBackendForTest(t)
			b.cfg.MaxCandidates = 1
			b.queries.(*chNativeQueries).cfg = b.cfg
			data := nativeTwoRowsForOperation(op)
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index == 0 {
					return owner, nil
				}
				return data, nil
			}
			err := chPublicOperation(t.Context(), b, op)
			var budget *QueryBudgetError
			if !errors.As(err, &budget) {
				t.Fatalf("native %s candidate exhaustion returned success/wrong error: %v", op, err)
			}
			calls := conn.snapshot()
			if len(calls) != 2 || data.closed.Load() != 1 {
				t.Fatalf("native candidate budget leaked rows: calls=%d closes=%d", len(calls), data.closed.Load())
			}
			assertCHBudgets(t, calls[1].sql, b.cfg)
			assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
		})
	}
}

func TestCHNativeHitLimit(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "time"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			b, conn, owner := nativeCHBackendForTest(t)
			b.cfg.K = 1
			b.queries.(*chNativeQueries).cfg = b.cfg
			data := nativeTwoRowsForOperation(op)
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index == 0 {
					return owner, nil
				}
				return data, nil
			}
			_, err := nativePublicResult(t.Context(), b, op)
			var budget *QueryBudgetError
			if !errors.As(err, &budget) || data.closed.Load() != 1 {
				t.Fatalf("native %s retained more than K=1 hits or leaked rows: error=%v closes=%d", op, err, data.closed.Load())
			}
			calls := conn.snapshot()
			if len(calls) != 2 {
				t.Fatalf("native hit-limit call population=%d", len(calls))
			}
			assertNativeWork(t, b.LastQueryWork(), calls[1].options.id)
		})
	}
}

func TestCHNativePreservesShorterParentDeadline(t *testing.T) {
	t.Parallel()
	b, conn, owner := nativeCHBackendForTest(t)
	data := nativeDataForOperation("query")
	conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
		if index == 0 {
			return owner, nil
		}
		return data, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := chPublicOperation(ctx, b, "query"); err != nil {
		t.Fatal(err)
	}
	calls := conn.snapshot()
	if len(calls) != 2 {
		t.Fatalf("native parent-deadline call population=%d", len(calls))
	}
	for _, call := range calls {
		if !call.hasDeadline || !call.deadline.Equal(deadline) {
			t.Fatalf("native request extended parent deadline: got=%s want=%s", call.deadline, deadline)
		}
	}
}

func TestCHNativeSuccessThenFailureUsesFreshID(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			b, conn, owner := nativeCHBackendForTest(t)
			cause := errors.New("second native data request failed")
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index%2 == 0 {
					return &fakeCHNativeRows{values: owner.values}, nil
				}
				if index == 1 {
					return nativeDataForOperation(op), nil
				}
				return nil, cause
			}
			if err := chPublicOperation(t.Context(), b, op); err != nil {
				t.Fatal(err)
			}
			first := b.LastQueryWork().QueryID
			if err := chPublicOperation(t.Context(), b, op); !errors.Is(err, cause) {
				t.Fatalf("second native request: %v", err)
			}
			calls := conn.snapshot()
			if len(calls) != 4 {
				t.Fatalf("native success/failure population=%d", len(calls))
			}
			seen := map[string]bool{}
			for _, call := range calls {
				if call.options.id == "" || seen[call.options.id] {
					t.Fatalf("owner/data requests reused query ID: %+v", calls)
				}
				seen[call.options.id] = true
			}
			if first == "" || first == b.LastQueryWork().QueryID {
				t.Fatalf("failed native request retained prior success ID: first=%q work=%+v", first, b.LastQueryWork())
			}
			assertNativeWork(t, b.LastQueryWork(), calls[3].options.id)
		})
	}
}

func lowerCHPercentEncoding(value string) string {
	wire := []byte(value)
	for i := 0; i+2 < len(wire); i++ {
		if wire[i] != '%' {
			continue
		}
		for j := i + 1; j <= i+2; j++ {
			if wire[j] >= 'A' && wire[j] <= 'F' {
				wire[j] += 'a' - 'A'
			}
		}
		i += 2
	}
	return string(wire)
}

func TestCHNativePublicErrorsRedactCredentials(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace", "span", "time"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			b, conn, owner := nativeCHBackendForTest(t)
			username, password := "user@name+雪", "p:/@%+雪"
			b.endpoint.User = url.UserPassword(username, password)
			encodedUser := url.User(username).String()
			encodedPassword := strings.TrimPrefix(url.UserPassword("fixed", password).String(), "fixed:")
			endpoint := b.endpoint.String()
			cause := errors.New("native dial sentinel")
			wrapped := fmt.Errorf("native user=%s password=%s endpoint=%s: %w", username, password, endpoint, &url.Error{Op: "query", URL: lowerCHPercentEncoding(endpoint), Err: cause})
			conn.query = func(index int, _ context.Context, _ string) (driver.Rows, error) {
				if index == 0 {
					return owner, nil
				}
				return nil, wrapped
			}
			err := chPublicOperation(t.Context(), b, op)
			if !errors.Is(err, cause) {
				t.Fatalf("credential sanitization lost native cause: %v", err)
			}
			secrets := []string{username, password, encodedUser, encodedPassword, lowerCHPercentEncoding(encodedUser), lowerCHPercentEncoding(encodedPassword), b.endpoint.User.String(), lowerCHPercentEncoding(b.endpoint.User.String())}
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err)} {
				for _, secret := range secrets {
					if strings.Contains(rendered, secret) {
						t.Fatalf("native public error leaked raw/escaped credentials: %q", rendered)
					}
				}
			}
			var transport *url.Error
			if !errors.As(err, &transport) {
				t.Fatalf("credential sanitization lost typed URL error: %T", err)
			}
			sanitized, parseErr := url.Parse(transport.URL)
			if parseErr != nil || sanitized.User != nil || sanitized.Host != b.endpoint.Host {
				t.Fatalf("typed URL error retained credentials or lost endpoint: URL=%q parse=%v", transport.URL, parseErr)
			}
			calls := conn.snapshot()
			if len(calls) != 2 {
				t.Fatalf("native credential-error call population=%d", len(calls))
			}
			work := b.LastQueryWork()
			assertNativeWork(t, work, calls[1].options.id)
			for _, secret := range secrets {
				if strings.Contains(work.QueryID+work.QueryTransport+work.ServerMetricsSource, secret) {
					t.Fatal("query provenance contains endpoint credentials")
				}
			}
		})
	}
}

func TestCHNativeCloseDelegatesPoolError(t *testing.T) {
	t.Parallel()
	b, conn, _ := nativeCHBackendForTest(t)
	cause := errors.New("native pool close failed")
	conn.closeError = cause
	b.Close()
	if conn.closed.Load() != 1 || len(conn.snapshot()) != 0 {
		t.Fatalf("Backend.Close did not close the lazy native pool: closes=%d queries=%d", conn.closed.Load(), len(conn.snapshot()))
	}
	if err := b.queries.close(); !errors.Is(err, cause) {
		t.Fatalf("executor close lost pool error: %v", err)
	}
}
