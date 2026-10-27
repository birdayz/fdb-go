package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func chManifestWire(t *testing.T, m Manifest) string {
	t.Helper()
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	row, err := json.Marshal(map[string]string{"payload": string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return string(row) + "\n"
}

func chHTTPBackend(t *testing.T, attrs bool, handler http.HandlerFunc) *CHBackend {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	b, err := OpenCH(testConfig(), srv.URL, attrs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

func chIsManifest(sql string) bool { return strings.HasPrefix(sql, "SELECT payload FROM ") }

func assertCHRequest(t *testing.T, r *http.Request, params map[string]string) string {
	t.Helper()
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("method/content type = %s/%q", r.Method, r.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) != 0 {
		t.Errorf("query body = %q, error %v", body, err)
	}
	q := r.URL.Query()
	for k, want := range map[string]string{"use_query_cache": "0", "enable_http_compression": "0", "send_progress_in_http_headers": "0"} {
		if got := q[k]; !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("%s = %q, want singleton %q", k, got, want)
		}
	}
	gotParams := map[string]string{}
	for key, values := range q {
		if strings.HasPrefix(key, "param_") {
			if len(values) != 1 {
				t.Errorf("parameter %s repeated: %q", key, values)
			}
			gotParams[strings.TrimPrefix(key, "param_")] = q.Get(key)
		}
	}
	if !reflect.DeepEqual(gotParams, params) {
		t.Errorf("forwarded params = %#v, want %#v", gotParams, params)
	}
	return q.Get("query")
}

func assertCHBudgets(t *testing.T, sql string, c Config) {
	t.Helper()
	for _, setting := range []string{"use_query_cache=0", "max_execution_time=2", "max_rows_to_read=" + strconv.Itoa(c.MaxCandidates), "max_bytes_to_read=" + strconv.FormatInt(c.MaxBytes, 10), "read_overflow_mode='throw'"} {
		if !strings.Contains(sql, setting) {
			t.Errorf("SQL missing %q: %s", setting, sql)
		}
	}
}

func TestCHPublicHTTPBindingsAndDecoding(t *testing.T) {
	t.Parallel()
	for _, layout := range []struct {
		name  string
		attrs bool
		order string
	}{{"map", false, "trace"}, {"attrs_trace", true, "trace"}, {"attrs_time", true, "time"}} {
		t.Run(layout.name, func(t *testing.T) {
			t.Parallel()
			a := Attribute{"scope'&=+\n雪", "key' OR 1=1 --&?%", "type\"\\+", "value'); DROP DATABASE other;--\n&=+雪"}
			trace := "trace' OR 1=1 --&=+%\n雪"
			encodedMapKey, err := json.Marshal([]string{a.Scope, a.Key, a.Type})
			if err != nil {
				t.Fatal(err)
			}
			var b *CHBackend
			var queryCalls, traceCalls atomic.Int64
			b = chHTTPBackend(t, layout.attrs, func(w http.ResponseWriter, r *http.Request) {
				sql := r.URL.Query().Get("query")
				if chIsManifest(sql) {
					fmt.Fprint(w, chManifestWire(t, manifest(b.cfg, "ready")))
					return
				}
				w.Header().Set("X-ClickHouse-Summary", `{"read_rows":"17","read_bytes":"891","elapsed_ns":"12345"}`)
				if strings.HasPrefix(sql, "SELECT span_id,ts_ns") {
					traceCalls.Add(1)
					assertCHRequest(t, r, map[string]string{"tenant": strconv.Itoa(b.cfg.Tenant), "trace": trace})
					assertCHBudgets(t, sql, b.cfg)
					for _, token := range []string{"trace_id={trace:String}", "tenant={tenant:UInt32}", "ORDER BY ts_ns,span_id", "LIMIT " + strconv.Itoa(b.cfg.MaxCandidates+1), "optimize_use_projections=1"} {
						if !strings.Contains(sql, token) {
							t.Errorf("trace SQL missing %q: %s", token, sql)
						}
					}
					if strings.Contains(sql, trace) {
						t.Error("trace interpolated into SQL")
					}
					fmt.Fprint(w, "{\"span_id\":\"early\",\"ts_ns\":\"1\"}\n{\"span_id\":\"tie_a\",\"ts_ns\":2}\n{\"span_id\":\"tie_z\",\"ts_ns\":\"2\"}\n")
					return
				}
				queryCalls.Add(1)
				assertCHRequest(t, r, map[string]string{"tenant": strconv.Itoa(b.cfg.Tenant), "scope": a.Scope, "key": a.Key, "typ": a.Type, "value": a.Value, "mapkey": string(encodedMapKey)})
				assertCHBudgets(t, sql, b.cfg)
				if !strings.Contains(sql, "LIMIT "+strconv.Itoa(b.cfg.K)+" SETTINGS optimize_read_in_order=1,") {
					t.Errorf("query K/read-order settings missing: %s", sql)
				}
				for _, literal := range []string{a.Scope, a.Key, a.Type, a.Value} {
					if strings.Contains(sql, literal) {
						t.Errorf("literal interpolated into SQL: %q", literal)
					}
				}
				if layout.attrs {
					for _, predicate := range []string{"scope={scope:String}", "k={key:String}", "typ={typ:String}", "v={value:String}"} {
						if !strings.Contains(sql, predicate) {
							t.Errorf("missing typed/scoped predicate %s", predicate)
						}
					}
				} else if !strings.Contains(sql, "mapContains(attrs,{mapkey:String})") || !strings.Contains(sql, "attrs[{mapkey:String}]={value:String}") {
					t.Errorf("map ownership/value predicate missing: %s", sql)
				}
				if layout.attrs && layout.order == "time" {
					if !strings.Contains(sql, "ORDER BY neg_time,rev_trace,span_id LIMIT 1 BY trace_id") {
						t.Errorf("time frontier SQL: %s", sql)
					}
				} else if !strings.Contains(sql, "GROUP BY trace_id ORDER BY latest DESC,trace_id DESC") {
					t.Errorf("trace aggregation/order SQL: %s", sql)
				}
				fmt.Fprint(w, "{\"trace_id\":\"z\",\"latest\":\"10\"}\n{\"trace_id\":\"a\",\"latest\":10}\n{\"trace_id\":\"b\",\"latest\":\"9\"}\n")
			})
			b.cfg.Order = layout.order
			got, err := b.Query(t.Context(), a)
			if err != nil || !reflect.DeepEqual(got.Hits, []Hit{{"z", 10}, {"a", 10}, {"b", 9}}) {
				t.Fatalf("Query decoded %v, error %v", got, err)
			}
			if !got.ServerSummary || got.ServerReadRows != 17 || got.ServerReadBytes != 891 || got.ServerElapsed != 12345 || got.ResponseBytes == 0 {
				t.Fatalf("Query work = %+v", got)
			}
			spans, err := b.Trace(t.Context(), trace)
			if err != nil || !reflect.DeepEqual(spans, []SpanHit{{"early", 1}, {"tie_a", 2}, {"tie_z", 2}}) {
				t.Fatalf("Trace decoded %v, error %v", spans, err)
			}
			if work := b.LastQueryWork(); !work.ServerSummary || work.ServerReadRows != 17 || work.ServerReadBytes != 891 || work.ServerElapsed != 12345 || work.ResponseBytes == 0 {
				t.Fatalf("Trace work = %+v", work)
			}
			if queryCalls.Load() != 1 || traceCalls.Load() != 1 {
				t.Fatalf("public request counts query=%d trace=%d", queryCalls.Load(), traceCalls.Load())
			}
		})
	}
}

func TestCHPublicSummaryAvailability(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, header string
		available    bool
	}{
		{"complete", `{"read_rows":"7","read_bytes":"9","elapsed_ns":"11"}`, true},
		{"numeric", `{"read_rows":7,"read_bytes":9,"elapsed_ns":11}`, true},
		{"zero", `{"read_rows":"0","read_bytes":"0","elapsed_ns":"0"}`, true},
		{"absent", "", false},
		{"empty", `{}`, false},
		{"null", `null`, false},
		{"missing_rows", `{"read_bytes":"9","elapsed_ns":"11"}`, false},
		{"missing_bytes", `{"read_rows":"7","elapsed_ns":"11"}`, false},
		{"missing_elapsed", `{"read_rows":"7","read_bytes":"9"}`, false},
		{"malformed", `{`, false},
		{"bad_number", `{"read_rows":"oops","read_bytes":"9","elapsed_ns":"11"}`, false},
		{"overflow", `{"read_rows":"9223372036854775808","read_bytes":"9","elapsed_ns":"11"}`, false},
		{"negative_rows", `{"read_rows":"-1","read_bytes":"9","elapsed_ns":"11"}`, false},
		{"negative_bytes", `{"read_rows":"7","read_bytes":"-9","elapsed_ns":"11"}`, false},
		{"negative_elapsed", `{"read_rows":"7","read_bytes":"9","elapsed_ns":"-11"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var b *CHBackend
			b = chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
				if chIsManifest(r.URL.Query().Get("query")) {
					fmt.Fprint(w, chManifestWire(t, manifest(b.cfg, "ready")))
					return
				}
				if tc.header != "" {
					w.Header().Set("X-ClickHouse-Summary", tc.header)
				}
			})
			for _, op := range []string{"query", "trace"} {
				var err error
				if op == "query" {
					var result QueryResult
					result, err = b.Query(t.Context(), Attribute{"span", QueryKey, "string", QueryValue})
					if !reflect.DeepEqual(result, b.LastQueryWork()) {
						t.Fatalf("returned and side-channel query work differ: result=%+v work=%+v", result, b.LastQueryWork())
					}
				} else {
					_, err = b.Trace(t.Context(), "trace")
				}
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				work := b.LastQueryWork()
				if work.ServerSummary != tc.available {
					t.Fatalf("%s summary availability = %t, want %t", op, work.ServerSummary, tc.available)
				}
				if tc.available && tc.name != "zero" && (work.ServerReadRows != 7 || work.ServerReadBytes != 9 || work.ServerElapsed != 11) {
					t.Fatalf("%s summary values corrupted: %+v", op, work)
				}
				if (!tc.available || tc.name == "zero") && (work.ServerReadRows != 0 || work.ServerReadBytes != 0 || work.ServerElapsed != 0) {
					t.Fatalf("%s fabricated partial measurements: %+v", op, work)
				}
			}
		})
	}
}

func chPublicOperation(ctx context.Context, b *CHBackend, op string) error {
	switch op {
	case "query":
		_, err := b.Query(ctx, Attribute{"span", QueryKey, "string", QueryValue})
		return err
	case "trace":
		_, err := b.Trace(ctx, "trace")
		return err
	case "audit":
		_, err := b.Audit(ctx)
		return err
	case "drop":
		return b.Drop(ctx)
	default:
		panic(op)
	}
}

func TestCHPublicOwnershipGuards(t *testing.T) {
	t.Parallel()
	mutations := map[string]func(*Manifest){
		"tool": func(m *Manifest) { m.Tool += "_other" }, "run": func(m *Manifest) { m.Run += "_other" },
		"corpus": func(m *Manifest) { m.Corpus += "_other" }, "order": func(m *Manifest) { m.Order = "time" },
		"hash_bytes": func(m *Manifest) { m.HashBytes-- }, "backend": func(m *Manifest) { m.Backend = "ch-attrs" },
		"loading": func(m *Manifest) { m.State = "loading" },
		"missing": nil, "malformed": nil,
	}
	for name, mutate := range mutations {
		for _, op := range []string{"query", "trace", "audit", "drop"} {
			if name == "loading" && op == "drop" {
				continue // Loading is owned and explicitly droppable.
			}
			t.Run(name+"/"+op, func(t *testing.T) {
				t.Parallel()
				var b *CHBackend
				var manifestCalls, otherCalls atomic.Int64
				var sentinelDeleted atomic.Bool
				b = chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
					sql := r.URL.Query().Get("query")
					if !chIsManifest(sql) {
						otherCalls.Add(1)
						if strings.HasPrefix(sql, "DROP DATABASE") {
							sentinelDeleted.Store(true)
						}
						http.Error(w, "guard bypass", http.StatusInternalServerError)
						return
					}
					manifestCalls.Add(1)
					if name == "missing" {
						return
					}
					if name == "malformed" {
						fmt.Fprint(w, `{"payload":"not json"}`)
						return
					}
					m := manifest(b.cfg, "ready")
					mutate(&m)
					fmt.Fprint(w, chManifestWire(t, m))
				})
				if err := chPublicOperation(t.Context(), b, op); err == nil {
					t.Fatal("unowned/not-ready operation authorized")
				}
				if manifestCalls.Load() != 1 || otherCalls.Load() != 0 || sentinelDeleted.Load() {
					t.Fatalf("guard requests manifest=%d other=%d sentinelDeleted=%t", manifestCalls.Load(), otherCalls.Load(), sentinelDeleted.Load())
				}
			})
		}
	}
}

func TestCHLastWorkResetOnFailure(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace"} {
		for _, failure := range []string{"ownership", "http"} {
			t.Run(op+"/"+failure, func(t *testing.T) {
				t.Parallel()
				var fail atomic.Bool
				var b *CHBackend
				b = chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
					if chIsManifest(r.URL.Query().Get("query")) {
						m := manifest(b.cfg, "ready")
						if fail.Load() && failure == "ownership" {
							m.Corpus = "wrong"
						}
						fmt.Fprint(w, chManifestWire(t, m))
						return
					}
					if fail.Load() {
						http.Error(w, "intentional failure", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("X-ClickHouse-Summary", `{"read_rows":"13","read_bytes":"19","elapsed_ns":"23"}`)
					fmt.Fprint(w, "\n")
				})
				if err := chPublicOperation(t.Context(), b, op); err != nil {
					t.Fatal(err)
				}
				if work := b.LastQueryWork(); !work.ServerSummary || work.ServerReadRows != 13 || work.ResponseBytes == 0 {
					t.Fatalf("positive work control: %+v", work)
				}
				fail.Store(true)
				if err := chPublicOperation(t.Context(), b, op); err == nil {
					t.Fatal("failed request succeeded")
				}
				if work := b.LastQueryWork(); !reflect.DeepEqual(work, QueryResult{}) {
					t.Fatalf("failed request retained previous work: %+v", work)
				}
			})
		}
	}
}

func TestCHPublicDecodeFailures(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace"} {
		for name, body := range map[string]string{"json": `{`, "timestamp": `{"trace_id":"x","span_id":"y","latest":"oops","ts_ns":"oops"}`, "missing_timestamp": `{"trace_id":"x","span_id":"y"}`, "overflow": `{"trace_id":"x","span_id":"y","latest":"9223372036854775808","ts_ns":"9223372036854775808"}`} {
			t.Run(op+"/"+name, func(t *testing.T) {
				t.Parallel()
				var b *CHBackend
				b = chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
					if chIsManifest(r.URL.Query().Get("query")) {
						fmt.Fprint(w, chManifestWire(t, manifest(b.cfg, "ready")))
					} else {
						fmt.Fprint(w, body)
					}
				})
				if err := chPublicOperation(t.Context(), b, op); err == nil {
					t.Fatal("malformed result accepted")
				}
			})
		}
	}
}

func TestCHInitialOwnershipRollback(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"database_exists", "owner_table", "initial_manifest", "canceled_owner_table", "canceled_initial_manifest"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			var calls []string
			created, dropped := false, false
			b := chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				sql := r.URL.Query().Get("query")
				calls = append(calls, sql)
				switch {
				case strings.HasPrefix(sql, "CREATE DATABASE"):
					if stage == "database_exists" {
						http.Error(w, "database already exists; sentinel", http.StatusBadRequest)
						return
					}
					created = true
				case strings.HasPrefix(sql, "DROP DATABASE"):
					if !created || r.Context().Err() != nil {
						t.Error("cleanup not restricted to newly created database/fresh context")
					}
					dropped = true
				case strings.HasPrefix(sql, "CREATE TABLE") && strings.Contains(stage, "owner_table"), strings.HasPrefix(sql, "INSERT INTO") && strings.Contains(stage, "initial_manifest"):
					if strings.HasPrefix(stage, "canceled_") {
						cancel()
					}
					http.Error(w, "intentional ownership initialization failure", http.StatusForbidden)
				case strings.HasPrefix(sql, "CREATE TABLE otel_"):
					// The initial manifest failure happens after owner-table creation.
				default:
					t.Errorf("unexpected request after initialization failure: %s", sql)
				}
			})
			if _, err := b.Ingest(ctx); err == nil {
				t.Fatal("initialization failure accepted")
			}
			mu.Lock()
			defer mu.Unlock()
			if stage == "database_exists" {
				if created || dropped || len(calls) != 1 {
					t.Fatalf("preexisting database touched: created=%t dropped=%t calls=%v", created, dropped, calls)
				}
			} else if !created || !dropped || calls[len(calls)-1] != "DROP DATABASE "+b.database+" SYNC" {
				t.Fatalf("new unowned database leaked: created=%t dropped=%t calls=%v", created, dropped, calls)
			}
		})
	}
}

func TestCHPublicCancellation(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"query", "trace"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var requests atomic.Int64
			var b *CHBackend
			b = chHTTPBackend(t, false, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if chIsManifest(r.URL.Query().Get("query")) {
					fmt.Fprint(w, chManifestWire(t, manifest(b.cfg, "ready")))
					return
				}
				cancel()
				<-r.Context().Done()
			})
			b.lastWork = QueryResult{ServerReadRows: 42, ServerSummary: true, ResponseBytes: 7}
			err := chPublicOperation(ctx, b, op)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost through public error: %v", err)
			}
			if requests.Load() != 2 {
				t.Fatalf("cancellation did not reach actual query request: %d", requests.Load())
			}
			if work := b.LastQueryWork(); !reflect.DeepEqual(work, QueryResult{}) {
				t.Fatalf("canceled request retained work: %+v", work)
			}
		})
	}
}

func chSourceRows(t *testing.T, c Config) []string {
	t.Helper()
	var rows []string
	for i := 0; i < c.Count; i++ {
		source, err := json.Marshal(Generate(c, i))
		if err != nil {
			t.Fatal(err)
		}
		row, err := json.Marshal(map[string]any{"ordinal": i, "source": string(source)})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(row))
	}
	return rows
}

func TestCHSourceAuditRejectsCorruption(t *testing.T) {
	t.Parallel()
	for _, problem := range []string{"intact", "missing", "duplicate", "out_of_range", "changed_source", "malformed_source", "malformed_row", "http", "attribute_census"} {
		t.Run(problem, func(t *testing.T) {
			t.Parallel()
			var b *CHBackend
			var sourceCalls, explainCalls atomic.Int64
			b = chHTTPBackend(t, true, func(w http.ResponseWriter, r *http.Request) {
				sql := r.URL.Query().Get("query")
				switch {
				case chIsManifest(sql):
					fmt.Fprint(w, chManifestWire(t, manifest(b.cfg, "ready")))
				case strings.HasPrefix(sql, "SELECT version()"):
					fmt.Fprint(w, "26.10.2.103\n")
				case strings.HasPrefix(sql, "SELECT toString(count())"):
					fmt.Fprintf(w, `{"n":"%d","a":"%d"}`, b.cfg.Count, b.cfg.Count*b.cfg.Attributes)
				case strings.HasPrefix(sql, "SELECT ordinal,source"):
					sourceCalls.Add(1)
					rows := chSourceRows(t, b.cfg)
					switch problem {
					case "missing":
						rows = rows[1:]
					case "duplicate":
						rows[1] = rows[0]
					case "out_of_range":
						rows[0] = `{"ordinal":999999,"source":"{}"}`
					case "changed_source":
						rows[0] = `{"ordinal":0,"source":"{}"}`
					case "malformed_source":
						rows[0] = `{"ordinal":0,"source":"not json"}`
					case "malformed_row":
						rows[0] = `{`
					case "http":
						http.Error(w, "source read failed", http.StatusServiceUnavailable)
						return
					}
					fmt.Fprintln(w, strings.Join(rows, "\n"))
				case strings.Contains(sql, "FROM system.parts "):
					n := b.cfg.Count * b.cfg.Attributes
					if problem == "attribute_census" {
						n--
					}
					fmt.Fprintf(w, "{\"table\":\"spans\",\"rows\":\"%d\",\"disk\":\"10\",\"logical\":\"20\"}\n{\"table\":\"attr_lookup\",\"rows\":\"%d\",\"disk\":\"30\",\"logical\":\"40\"}\n", b.cfg.Count, n)
				case strings.HasPrefix(sql, "EXPLAIN indexes=1 "):
					explainCalls.Add(1)
					if strings.Contains(sql, "SELECT span_id,ts_ns") {
						assertCHBudgets(t, sql, b.cfg)
					}
					fmt.Fprint(w, "ReadFromMergeTree")
				case strings.Contains(sql, "FROM system.projection_parts "):
					fmt.Fprint(w, `{"disk":"1","logical":"2"}`)
				default:
					t.Errorf("unexpected audit SQL: %s", sql)
					http.Error(w, "unexpected SQL", http.StatusBadRequest)
				}
			})
			stats, err := b.Audit(t.Context())
			if sourceCalls.Load() != 1 {
				t.Fatalf("source audit requests = %d", sourceCalls.Load())
			}
			if problem == "intact" {
				if err != nil || explainCalls.Load() != 2 || stats.Rows != int64(b.cfg.Count) || stats.Structures["attr_lookup"].Keys != int64(b.cfg.Count*b.cfg.Attributes) {
					t.Fatalf("positive audit control stats=%+v explain=%d error=%v", stats, explainCalls.Load(), err)
				}
			} else if err == nil {
				t.Fatalf("%s corruption accepted: %+v", problem, stats)
			}
		})
	}
}

func TestCHPublicIngestBatchLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		rowCap     int
		byteRows   int
		byteAdjust int
		wantRows   []int
		oversize   bool
	}{
		{name: "row_cap", rowCap: 2, byteRows: 5, wantRows: []int{2, 2, 1}},
		{name: "byte_cap", rowCap: 5, byteRows: 3, byteAdjust: 1, wantRows: []int{3, 2}},
		{name: "exact_two_row_boundary", rowCap: 5, byteRows: 2, wantRows: []int{2, 2, 1}},
		{name: "below_two_row_boundary", rowCap: 5, byteRows: 2, byteAdjust: -1, wantRows: []int{1, 1, 1, 1, 1}},
		{name: "exact_single_row_boundary", rowCap: 5, byteRows: 1, wantRows: []int{1, 1, 1, 1, 1}},
		{name: "oversized_single_row", rowCap: 5, byteRows: 1, byteAdjust: -1, oversize: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := testConfig()
			c.Count, c.Tenants, c.Attributes, c.MatchEvery = 5, 1, 4, 1
			c.Backend, c.CHBatch = "ch-map", tc.rowCap
			// Equal-width rows make the expected split counts literal, not a
			// second implementation of Ingest's batching loop.
			rowBytes := 0
			for ordinal := 0; ordinal < c.Count; ordinal++ {
				row, err := json.Marshal(chRow(Generate(c, ordinal)))
				if err != nil {
					t.Fatal(err)
				}
				if ordinal == 0 {
					rowBytes = len(row) + 1 // JSONEachRow's newline is transported too.
				} else if len(row)+1 != rowBytes {
					t.Fatalf("fixture row %d has %d bytes, want %d", ordinal, len(row)+1, rowBytes)
				}
			}
			c.CHBatchBytes = tc.byteRows*rowBytes + tc.byteAdjust
			var mu sync.Mutex
			var batchRows, batchBytes, ordinals []int
			var owners []Manifest
			var sequences []int
			var payloadBytes int64
			databaseCreated, schemaCreated, dropped := false, false, false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				sql := r.URL.Query().Get("query")
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/octet-stream" {
					t.Errorf("ingest method/content type = %s/%q", r.Method, r.Header.Get("Content-Type"))
				}
				switch {
				case sql == "CREATE DATABASE otel_"+c.Run:
					databaseCreated = true
				case strings.HasPrefix(sql, "CREATE TABLE otel_"+c.Run+".owner "):
				case strings.HasPrefix(sql, "CREATE TABLE otel_"+c.Run+".spans "):
					schemaCreated = true
				case sql == "INSERT INTO otel_"+c.Run+".owner FORMAT JSONEachRow":
					var row struct {
						Sequence int    `json:"sequence"`
						Payload  string `json:"payload"`
					}
					var owner Manifest
					if err := json.Unmarshal(body, &row); err != nil {
						t.Error(err)
						return
					}
					if err := json.Unmarshal([]byte(row.Payload), &owner); err != nil {
						t.Error(err)
						return
					}
					owners = append(owners, owner)
					sequences = append(sequences, row.Sequence)
					if owner.State == "ready" && len(ordinals) != c.Count {
						t.Errorf("ready published before all source rows: %v", ordinals)
					}
				case sql == "INSERT INTO otel_"+c.Run+".spans SETTINGS async_insert=0 FORMAT JSONEachRow":
					if !schemaCreated || len(owners) != 1 || owners[0].State != "loading" {
						t.Error("source INSERT before schema/loading owner")
					}
					if len(body) == 0 || body[len(body)-1] != '\n' {
						t.Errorf("source payload is not nonempty newline-delimited JSON: %q", body)
					}
					lines := bytes.Split(bytes.TrimSuffix(body, []byte{'\n'}), []byte{'\n'})
					batchRows = append(batchRows, len(lines))
					batchBytes = append(batchBytes, len(body))
					payloadBytes += int64(len(body))
					for _, line := range lines {
						var row struct {
							Ordinal int    `json:"ordinal"`
							Source  string `json:"source"`
						}
						if err := json.Unmarshal(line, &row); err != nil {
							t.Error(err)
							continue
						}
						var source Span
						if err := json.Unmarshal([]byte(row.Source), &source); err != nil {
							t.Error(err)
						} else if row.Ordinal < 0 || row.Ordinal >= c.Count || !equalSpan(source, Generate(c, row.Ordinal)) {
							t.Errorf("wrong source for ordinal %d: %+v", row.Ordinal, source)
						}
						ordinals = append(ordinals, row.Ordinal)
					}
				case strings.HasPrefix(sql, "DROP DATABASE "):
					dropped = true
				default:
					t.Errorf("unexpected ingest SQL: %s", sql)
					http.Error(w, "unexpected SQL", http.StatusBadRequest)
				}
			}))
			t.Cleanup(srv.Close)
			b, err := OpenCH(c, srv.URL, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(b.Close)
			stats, err := b.Ingest(t.Context())
			mu.Lock()
			defer mu.Unlock()
			if !databaseCreated || !schemaCreated || dropped {
				t.Fatalf("ingest did not retain initialized database: created=%t schema=%t dropped=%t", databaseCreated, schemaCreated, dropped)
			}
			if !reflect.DeepEqual(batchRows, tc.wantRows) {
				t.Fatalf("source INSERT row counts = %v, want %v", batchRows, tc.wantRows)
			}
			if tc.oversize {
				var admission *AdmissionError
				if !errors.As(err, &admission) || len(ordinals) != 0 || payloadBytes != 0 || !reflect.DeepEqual(owners, []Manifest{manifest(c, "loading")}) || !reflect.DeepEqual(sequences, []int{1}) {
					t.Fatalf("oversized row must fail before source INSERT and remain loading: error=%v ordinals=%v payload=%d owners=%+v sequences=%v", err, ordinals, payloadBytes, owners, sequences)
				}
				if !reflect.DeepEqual(stats, Statistics{}) {
					t.Fatalf("oversized row reported successful ingest statistics: %+v", stats)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, count := range tc.wantRows {
				if batchBytes[i] != count*rowBytes || batchBytes[i] > c.CHBatchBytes || count > c.CHBatch {
					t.Errorf("batch %d rows=%d bytes=%d; want %d bytes within caps rows=%d bytes=%d", i, count, batchBytes[i], count*rowBytes, c.CHBatch, c.CHBatchBytes)
				}
			}
			if !reflect.DeepEqual(ordinals, []int{0, 1, 2, 3, 4}) {
				t.Fatalf("source INSERTs omitted/duplicated/reordered ordinals: %v", ordinals)
			}
			if payloadBytes != int64(5*rowBytes) || stats.LogicalPayloadBytes != payloadBytes || stats.Rows != 5 || stats.Attributes != 20 {
				t.Fatalf("ingest accounting stats=%+v payload=%d, want %d", stats, payloadBytes, 5*rowBytes)
			}
			if !reflect.DeepEqual(owners, []Manifest{manifest(c, "loading"), manifest(c, "ready")}) || !reflect.DeepEqual(sequences, []int{1, 2}) {
				t.Fatalf("owner lifecycle = %+v sequences=%v", owners, sequences)
			}
		})
	}
}
