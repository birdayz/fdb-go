package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CHBackend compares immutable unique-span data only: MV insertion does not
// implement corrections, deletes or deduplication of replayed OTLP requests.
type CHBackend struct {
	cfg       Config
	endpoint  *url.URL
	client    *http.Client
	transport *http.Transport
	database  string
	attrs     bool
	lastWork  QueryResult
}

func OpenCH(c Config, endpoint string, attrs bool) (*CHBackend, error) {
	return OpenCHContext(context.Background(), c, endpoint, attrs)
}

func OpenCHContext(ctx context.Context, c Config, endpoint string, attrs bool) (*CHBackend, error) {
	if err := ctx.Err(); err != nil {
		return nil, wrap("canceled", "open CH", err)
	}
	c.Backend = "ch-map"
	if attrs {
		c.Backend = "ch-attrs"
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, wrap("config", "open CH", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fail("config", "open CH", "CH URL must be explicit http(s) endpoint without query/fragment")
	}
	tr := &http.Transport{MaxIdleConns: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	return &CHBackend{cfg: c, endpoint: u, client: &http.Client{Transport: tr, Timeout: 30 * time.Second}, transport: tr, database: "otel_" + c.Run, attrs: attrs}, nil
}

// LastQueryWork is metrics only; results are never cached. CLI calls are sequential.
func (b *CHBackend) LastQueryWork() QueryResult { return b.lastWork }
func (b *CHBackend) Close()                     { b.transport.CloseIdleConnections() }

type chSummary struct {
	ReadRows, ReadBytes, Elapsed int64
	Present                      bool
}

func parseSummary(raw string) (chSummary, error) {
	var values map[string]json.RawMessage
	if raw == "" {
		return chSummary{}, fail("http", "summary", "missing X-ClickHouse-Summary")
	}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return chSummary{}, err
	}
	read := func(k string) (int64, error) {
		v, ok := values[k]
		if !ok {
			return 0, failf("http", "summary", "missing %s", k)
		}
		n, err := strconv.ParseInt(strings.Trim(string(v), "\""), 10, 64)
		if err != nil {
			return 0, wrap("http", "summary", err)
		}
		if n < 0 {
			return 0, failf("http", "summary", "negative %s", k)
		}
		return n, nil
	}
	rows, err := read("read_rows")
	if err != nil {
		return chSummary{}, err
	}
	n, err := read("read_bytes")
	if err != nil {
		return chSummary{}, err
	}
	elapsed, err := read("elapsed_ns")
	return chSummary{rows, n, elapsed, err == nil}, err
}

func (b *CHBackend) request(ctx context.Context, sql string, payload []byte, params map[string]string) ([]byte, error) {
	wire, _, err := b.requestWithSummary(ctx, sql, payload, params)
	return wire, err
}

func (b *CHBackend) requestWithSummary(ctx context.Context, sql string, payload []byte, params map[string]string) ([]byte, chSummary, error) {
	u := *b.endpoint
	q := u.Query()
	q.Set("query", sql)
	q.Set("use_query_cache", "0")
	q.Set("enable_http_compression", "0")
	q.Set("send_progress_in_http_headers", "0")
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, chSummary{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, chSummary{}, wrap("http", "request", err)
	}
	defer resp.Body.Close()
	wire, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, chSummary{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, chSummary{}, failf("http", "request", "ClickHouse HTTP status %d: %.4096s", resp.StatusCode, wire)
	}
	summary, summaryErr := parseSummary(resp.Header.Get("X-ClickHouse-Summary"))
	if summaryErr != nil {
		summary = chSummary{}
	}
	return wire, summary, nil
}

func (b *CHBackend) exec(ctx context.Context, sql string) error {
	_, err := b.request(ctx, sql, nil, nil)
	return err
}

func (b *CHBackend) readManifest(ctx context.Context, ready bool) error {
	wire, err := b.request(ctx, "SELECT payload FROM "+b.database+".owner ORDER BY sequence DESC LIMIT 1 FORMAT JSONEachRow", nil, nil)
	if err != nil {
		return err
	}
	var row struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(wire), &row); err != nil {
		return wrap("ownership", "manifest", err)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(row.Payload), &m); err != nil {
		return wrap("ownership", "manifest", err)
	}
	return checkManifest(b.cfg, m, ready)
}

func (b *CHBackend) putManifest(ctx context.Context, state string, sequence int) error {
	wire, _ := json.Marshal(manifest(b.cfg, state))
	row, _ := json.Marshal(map[string]any{"sequence": sequence, "payload": string(wire)})
	_, err := b.request(ctx, "INSERT INTO "+b.database+".owner FORMAT JSONEachRow", row, nil)
	return err
}

type chSpan struct {
	Ordinal uint64            `json:"ordinal"`
	Tenant  uint32            `json:"tenant"`
	Service string            `json:"service"`
	Day     string            `json:"day"`
	Start   int64             `json:"ts_ns"`
	Trace   string            `json:"trace_id"`
	ID      string            `json:"span_id"`
	Attrs   map[string]string `json:"attrs"`
	Terms   [][4]string       `json:"terms"`
	Source  string            `json:"source"`
}

func chRow(s Span) chSpan {
	row := chSpan{uint64(s.Ordinal), uint32(s.Tenant), fmt.Sprintf("service_%d", s.Ordinal%100), time.Unix(0, s.Start).UTC().Format("2006-01-02"), s.Start, s.Trace, s.ID, map[string]string{}, nil, ""}
	for _, a := range s.Attributes {
		row.Attrs[mapKey(a)] = a.Value
		row.Terms = append(row.Terms, [4]string{a.Scope, a.Key, a.Type, a.Value})
	}
	source, _ := json.Marshal(s)
	row.Source = string(source)
	return row
}

func (b *CHBackend) Ingest(ctx context.Context) (stats Statistics, retErr error) {
	// No IF NOT EXISTS: never append into a previous run or assume ownership.
	if err := b.exec(ctx, "CREATE DATABASE "+b.database); err != nil {
		return Statistics{}, err
	}
	initialized := false
	defer func() {
		if initialized {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.exec(cleanup, "DROP DATABASE "+b.database+" SYNC"); err != nil {
			retErr = errors.Join(retErr, wrap("http", "initialization cleanup", err))
		}
	}()
	if err := b.exec(ctx, "CREATE TABLE "+b.database+".owner (sequence UInt32,payload String) ENGINE=TinyLog"); err != nil {
		return Statistics{}, err
	}
	if err := b.putManifest(ctx, "loading", 1); err != nil {
		return Statistics{}, err
	}
	initialized = true
	schema := "CREATE TABLE " + b.database + `.spans (ordinal UInt64,tenant UInt32,service LowCardinality(String),day Date,ts_ns Int64,trace_id String,span_id String,attrs Map(String,String) CODEC(ZSTD(1)),terms Array(Tuple(String,String,String,String)) CODEC(ZSTD(1)),source String CODEC(ZSTD(1)), INDEX trace_bf trace_id TYPE bloom_filter(0.01) GRANULARITY 1, INDEX attrs_k mapKeys(attrs) TYPE bloom_filter(0.01) GRANULARITY 1, INDEX attrs_v mapValues(attrs) TYPE bloom_filter(0.01) GRANULARITY 1, PROJECTION by_trace (SELECT tenant,trace_id,span_id,ts_ns ORDER BY (tenant,trace_id,span_id))) ENGINE=MergeTree PARTITION BY day ORDER BY (tenant,service,ts_ns,trace_id,span_id)`
	if err := b.exec(ctx, schema); err != nil {
		return Statistics{}, err
	}
	if b.attrs {
		order := "tenant,scope,k,typ,v,trace_id,span_id"
		if b.cfg.Order == "time" {
			order = "tenant,scope,k,typ,v,neg_time,rev_trace,span_id"
		}
		ddl := "CREATE TABLE " + b.database + ".attr_lookup (tenant UInt32,day Date,scope LowCardinality(String),k String,typ LowCardinality(String),v String,ts_ns Int64,neg_time Int64,trace_id String,rev_trace String,span_id String) ENGINE=MergeTree PARTITION BY day ORDER BY (" + order + ")"
		if err := b.exec(ctx, ddl); err != nil {
			return Statistics{}, err
		}
		mv := "CREATE MATERIALIZED VIEW " + b.database + ".attr_mv TO " + b.database + ".attr_lookup AS SELECT tenant,day,a.1 AS scope,a.2 AS k,a.3 AS typ,a.4 AS v,ts_ns,-ts_ns AS neg_time,trace_id,translate(trace_id,'0123456789abcdef','fedcba9876543210') AS rev_trace,span_id FROM " + b.database + ".spans ARRAY JOIN terms AS a"
		if err := b.exec(ctx, mv); err != nil {
			return Statistics{}, err
		}
	}
	var logical int64
	for lo := 0; lo < b.cfg.Count; {
		var buf bytes.Buffer
		hi := lo
		for hi < b.cfg.Count && hi-lo < b.cfg.CHBatch {
			row := chRow(Generate(b.cfg, hi))
			wire, err := json.Marshal(row)
			if err != nil {
				return Statistics{}, err
			}
			if buf.Len() > 0 && buf.Len()+len(wire)+1 > b.cfg.CHBatchBytes {
				break
			}
			if len(wire)+1 > b.cfg.CHBatchBytes {
				return Statistics{}, fail("admission", "CH ingest", "source row exceeds batch cap")
			}
			buf.Write(wire)
			buf.WriteByte('\n')
			hi++
		}
		if _, err := b.request(ctx, "INSERT INTO "+b.database+".spans SETTINGS async_insert=0 FORMAT JSONEachRow", buf.Bytes(), nil); err != nil {
			return Statistics{}, wrap("http", "ingest batch "+strconv.Itoa(lo), err)
		}
		logical += int64(buf.Len())
		lo = hi
	}
	if err := b.putManifest(ctx, "ready", 2); err != nil {
		return Statistics{}, err
	}
	return Statistics{Rows: int64(b.cfg.Count), Attributes: int64(b.cfg.Count * b.cfg.Attributes), LogicalPayloadBytes: logical, Details: map[string]string{"ingest": "bulk synchronous HTTP JSONEachRow, independent CH batch/byte caps; NOT matched real-time acknowledgment batching", "payload_bytes": "JSON transport bytes including duplicate terms/map/source; not comparable serialized-record bytes"}}, nil
}

func (b *CHBackend) readWorkSettings() string {
	return "use_query_cache=0,max_execution_time=2,max_rows_to_read=" + strconv.Itoa(b.cfg.MaxCandidates) + ",max_bytes_to_read=" + strconv.FormatInt(b.cfg.MaxBytes, 10) + ",read_overflow_mode='throw'"
}

func (b *CHBackend) traceSQL(trace string) (string, map[string]string) {
	return "SELECT span_id,ts_ns FROM " + b.database + ".spans WHERE tenant={tenant:UInt32} AND trace_id={trace:String} ORDER BY ts_ns,span_id LIMIT " + strconv.Itoa(b.cfg.MaxCandidates+1) + " SETTINGS optimize_use_projections=1," + b.readWorkSettings() + " FORMAT JSONEachRow", map[string]string{"tenant": strconv.Itoa(b.cfg.Tenant), "trace": trace}
}

func (b *CHBackend) querySQL(a Attribute) (string, map[string]string) {
	params := map[string]string{"tenant": strconv.Itoa(b.cfg.Tenant), "scope": a.Scope, "key": a.Key, "typ": a.Type, "value": a.Value, "mapkey": mapKey(a)}
	where := "tenant={tenant:UInt32} AND mapContains(attrs,{mapkey:String}) AND attrs[{mapkey:String}]={value:String}"
	table := "spans"
	if b.attrs {
		table = "attr_lookup"
		where = "tenant={tenant:UInt32} AND scope={scope:String} AND k={key:String} AND typ={typ:String} AND v={value:String}"
	}
	// GROUP BY max is the exact trace-level oracle, including repeated matching
	// spans. CH is free to exploit the declared sorting key; plans are recorded.
	sql := "SELECT trace_id,max(ts_ns) AS latest FROM " + b.database + "." + table + " WHERE " + where + " GROUP BY trace_id ORDER BY latest DESC,trace_id DESC LIMIT " + strconv.Itoa(b.cfg.K)
	if b.attrs && b.cfg.Order == "time" {
		sql = "SELECT trace_id,ts_ns AS latest FROM " + b.database + ".attr_lookup WHERE " + where + " ORDER BY neg_time,rev_trace,span_id LIMIT 1 BY trace_id LIMIT " + strconv.Itoa(b.cfg.K)
	}
	sql += " SETTINGS optimize_read_in_order=1," + b.readWorkSettings() + " FORMAT JSONEachRow"
	return sql, params
}

func (b *CHBackend) Query(ctx context.Context, a Attribute) (QueryResult, error) {
	b.lastWork = QueryResult{}
	if err := b.readManifest(ctx, true); err != nil {
		return QueryResult{}, err
	}
	sql, params := b.querySQL(a)
	wire, summary, err := b.requestWithSummary(ctx, sql, nil, params)
	if err != nil {
		return QueryResult{}, err
	}
	out := QueryResult{Hits: []Hit{}, ServerReadRows: summary.ReadRows, ServerReadBytes: summary.ReadBytes, ServerElapsed: summary.Elapsed, ResponseBytes: int64(len(wire)), ServerSummary: summary.Present}
	b.lastWork = out
	for _, line := range bytes.Split(bytes.TrimSpace(wire), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r struct {
			Trace  string      `json:"trace_id"`
			Latest json.Number `json:"latest"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return out, wrap("corruption", "query result", err)
		}
		n, err := strconv.ParseInt(string(r.Latest), 10, 64)
		if err != nil {
			return out, err
		}
		out.Hits = append(out.Hits, Hit{r.Trace, n})
	}
	return out, nil
}

func (b *CHBackend) Trace(ctx context.Context, trace string) ([]SpanHit, error) {
	b.lastWork = QueryResult{}
	if err := b.readManifest(ctx, true); err != nil {
		return nil, err
	}
	sql, params := b.traceSQL(trace)
	wire, summary, err := b.requestWithSummary(ctx, sql, nil, params)
	if err != nil {
		return nil, err
	}
	b.lastWork = QueryResult{ServerReadRows: summary.ReadRows, ServerReadBytes: summary.ReadBytes, ServerElapsed: summary.Elapsed, ResponseBytes: int64(len(wire)), ServerSummary: summary.Present}
	out := []SpanHit{}
	for _, line := range bytes.Split(bytes.TrimSpace(wire), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r struct {
			ID    string      `json:"span_id"`
			Start json.Number `json:"ts_ns"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, wrap("corruption", "trace result", err)
		}
		n, err := strconv.ParseInt(string(r.Start), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, SpanHit{r.ID, n})
	}
	if len(out) > b.cfg.MaxCandidates {
		return nil, fail("budget", "CH trace", "trace row limit exceeded")
	}
	return out, nil
}

func (b *CHBackend) Drop(ctx context.Context) error {
	if err := b.readManifest(ctx, false); err != nil {
		return err
	}
	return b.exec(ctx, "DROP DATABASE "+b.database+" SYNC")
}

func (b *CHBackend) auditSources(ctx context.Context) error {
	u := *b.endpoint
	q := u.Query()
	q.Set("query", "SELECT ordinal,source FROM "+b.database+".spans FORMAT JSONEachRow")
	q.Set("use_query_cache", "0")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: b.transport, Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return wrap("http", "source audit", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failf("http", "source audit", "HTTP status %d", resp.StatusCode)
	}
	seen := newOrdinalSet(b.cfg.Count)
	decoder := json.NewDecoder(resp.Body)
	for {
		var row struct {
			Ordinal json.Number `json:"ordinal"`
			Source  string      `json:"source"`
		}
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		ordinal, err := strconv.Atoi(string(row.Ordinal))
		if err != nil {
			return err
		}
		if err := seen.add(ordinal, b.cfg.Count); err != nil {
			return err
		}
		var source Span
		if err := json.Unmarshal([]byte(row.Source), &source); err != nil {
			return err
		}
		if !equalSpan(source, Generate(b.cfg, ordinal)) {
			return failf("audit", "source scan", "CH source mismatch at ordinal %d", ordinal)
		}
	}
	if seen.count != b.cfg.Count {
		return failf("audit", "source census", "CH source audit saw %d unique ordinals, want %d", seen.count, b.cfg.Count)
	}
	return nil
}

func (b *CHBackend) Audit(ctx context.Context) (Statistics, error) {
	if err := b.readManifest(ctx, true); err != nil {
		return Statistics{}, err
	}
	stats := Statistics{Structures: map[string]Size{}, Details: map[string]string{"storage": "system.parts active bytes; source + every MV destination included", "semantic_class": "immutable unique span corpus; no replication/durability equivalence claimed"}}
	wire, err := b.request(ctx, "SELECT version() FORMAT TabSeparated", nil, nil)
	if err != nil {
		return stats, err
	}
	stats.Details["server_version"] = strings.TrimSpace(string(wire))
	wire, err = b.request(ctx, "SELECT toString(count()) AS n,toString(sum(length(terms))) AS a FROM "+b.database+".spans FORMAT JSONEachRow", nil, nil)
	if err != nil {
		return stats, err
	}
	var counts struct {
		N string `json:"n"`
		A string `json:"a"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(wire), &counts); err != nil {
		return stats, err
	}
	stats.Rows, err = strconv.ParseInt(counts.N, 10, 64)
	if err != nil {
		return stats, err
	}
	stats.Attributes, err = strconv.ParseInt(counts.A, 10, 64)
	if err != nil {
		return stats, err
	}
	if stats.Rows != int64(b.cfg.Count) || stats.Attributes != int64(b.cfg.Count*b.cfg.Attributes) {
		return stats, fail("audit", "source census", "CH row/attribute audit mismatch")
	}
	// One unordered streaming source scan: no ordinal predicate on a different
	// sorting key, no OFFSET, and no whole-corpus response allocation.
	if err := b.auditSources(ctx); err != nil {
		return stats, err
	}
	wire, err = b.request(ctx, "SELECT table,toString(sum(rows)) AS rows,toString(sum(bytes_on_disk)) AS disk,toString(sum(data_uncompressed_bytes)) AS logical FROM system.parts WHERE database={db:String} AND active GROUP BY table FORMAT JSONEachRow", nil, map[string]string{"db": b.database})
	if err != nil {
		return stats, err
	}
	for _, line := range bytes.Split(bytes.TrimSpace(wire), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r struct{ Table, Rows, Disk, Logical string }
		if err := json.Unmarshal(line, &r); err != nil {
			return stats, err
		}
		n, e := strconv.ParseInt(r.Rows, 10, 64)
		if e != nil {
			return stats, e
		}
		disk, e := strconv.ParseInt(r.Disk, 10, 64)
		if e != nil {
			return stats, e
		}
		logical, e := strconv.ParseInt(r.Logical, 10, 64)
		if e != nil {
			return stats, e
		}
		stats.Structures[r.Table] = Size{Keys: n, ValueBytes: logical, PhysicalBytes: disk}
	}
	sql, params := b.querySQL(Attribute{"span", QueryKey, "string", QueryValue})
	sql = strings.TrimSuffix(sql, " FORMAT JSONEachRow")
	plan, err := b.request(ctx, "EXPLAIN indexes=1 "+sql, nil, params)
	if err != nil {
		return stats, err
	}
	stats.Details["rare_query_explain"] = string(plan)
	traceSQL, traceParams := b.traceSQL(Generate(b.cfg, 2*b.cfg.Tenant).Trace)
	tracePlan, err := b.request(ctx, "EXPLAIN indexes=1 "+strings.TrimSuffix(traceSQL, " FORMAT JSONEachRow"), nil, traceParams)
	if err != nil {
		return stats, err
	}
	stats.Details["trace_query_explain"] = string(tracePlan)
	projectionBytes, err := b.request(ctx, "SELECT toString(sum(bytes_on_disk)) AS disk,toString(sum(data_uncompressed_bytes)) AS logical FROM system.projection_parts WHERE database={db:String} AND active FORMAT JSONEachRow", nil, map[string]string{"db": b.database})
	if err != nil {
		return stats, err
	}
	stats.Details["projection_parts_bytes"] = strings.TrimSpace(string(projectionBytes))
	stats.Details["projection_accounting"] = "by_trace created before loading; report projection_parts separately and verify whether parent system.parts already includes these bytes before summing"
	if b.attrs && stats.Structures["attr_lookup"].Keys != stats.Attributes {
		return stats, fail("audit", "posting census", "CH attribute destination row census mismatch")
	}
	return stats, nil
}
