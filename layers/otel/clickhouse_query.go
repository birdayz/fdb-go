package otel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

type chQueryMeta struct {
	queryID, transport, metricsSource string
	summary                           chSummary
	responseBytes                     int64
}

func (m chQueryMeta) work() QueryResult {
	return QueryResult{QueryID: m.queryID, QueryTransport: m.transport, ServerMetricsSource: m.metricsSource, ServerReadRows: m.summary.ReadRows, ServerReadBytes: m.summary.ReadBytes, ServerElapsed: m.summary.Elapsed, ServerSummary: m.summary.Present, ResponseBytes: m.responseBytes}
}

type chQueryExecutor interface {
	owner(context.Context, string) (string, error)
	hits(context.Context, string, map[string]string) ([]Hit, chQueryMeta, error)
	trace(context.Context, string, map[string]string) ([]SpanHit, chQueryMeta, error)
	spans(context.Context, string, map[string]string) ([]SpanMatch, chQueryMeta, error)
	close() error
}

func newCHQueryID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", wrap("operation", "query identity", err)
	}
	return "otel_" + hex.EncodeToString(id[:]), nil
}

type chHTTPQueries struct{ backend *CHBackend }

func (q *chHTTPQueries) close() error { return nil }

func (q *chHTTPQueries) request(ctx context.Context, sql string, params map[string]string) ([]byte, chQueryMeta, error) {
	id, err := newCHQueryID()
	if err != nil {
		return nil, chQueryMeta{}, err
	}
	meta := chQueryMeta{queryID: id, transport: "http", metricsSource: "unavailable"}
	wire, summary, err := q.backend.requestWithSummaryID(ctx, sql, nil, params, id)
	if err != nil {
		return nil, meta, err
	}
	meta.summary, meta.responseBytes = summary, int64(len(wire))
	if summary.Present {
		meta.metricsSource = "http_summary"
	}
	return wire, meta, nil
}

func (q *chHTTPQueries) owner(ctx context.Context, sql string) (string, error) {
	wire, _, err := q.request(ctx, sql, nil)
	if err != nil {
		return "", err
	}
	var row struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(wire), &row); err != nil {
		return "", wrap("ownership", "manifest", err)
	}
	return row.Payload, nil
}

func (q *chHTTPQueries) hits(ctx context.Context, sql string, params map[string]string) ([]Hit, chQueryMeta, error) {
	wire, meta, err := q.request(ctx, sql, params)
	if err != nil {
		return nil, meta, err
	}
	out := []Hit{}
	for _, line := range bytes.Split(bytes.TrimSpace(wire), []byte{'\n'}) {
		if err := ctx.Err(); err != nil {
			return out, meta, wrap("canceled", "query result", err)
		}
		if len(line) == 0 {
			continue
		}
		var row struct {
			Trace  string      `json:"trace_id"`
			Latest json.Number `json:"latest"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return out, meta, wrap("corruption", "query result", err)
		}
		latest, err := strconv.ParseInt(string(row.Latest), 10, 64)
		if err != nil {
			return out, meta, wrap("corruption", "query timestamp", err)
		}
		out = append(out, Hit{Trace: row.Trace, Latest: latest})
	}
	return out, meta, nil
}

func (q *chHTTPQueries) trace(ctx context.Context, sql string, params map[string]string) ([]SpanHit, chQueryMeta, error) {
	wire, meta, err := q.request(ctx, sql, params)
	if err != nil {
		return nil, meta, err
	}
	out := []SpanHit{}
	for _, line := range bytes.Split(bytes.TrimSpace(wire), []byte{'\n'}) {
		if err := ctx.Err(); err != nil {
			return nil, meta, wrap("canceled", "trace result", err)
		}
		if len(line) == 0 {
			continue
		}
		var row struct {
			ID    string      `json:"span_id"`
			Start json.Number `json:"ts_ns"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, meta, wrap("corruption", "trace result", err)
		}
		start, err := strconv.ParseInt(string(row.Start), 10, 64)
		if err != nil {
			return nil, meta, wrap("corruption", "trace timestamp", err)
		}
		out = append(out, SpanHit{ID: row.ID, Start: start})
		if len(out) > q.backend.cfg.MaxCandidates {
			return nil, meta, fail("budget", "CH trace", "trace row limit exceeded")
		}
	}
	return out, meta, nil
}

func (q *chHTTPQueries) spans(ctx context.Context, sql string, params map[string]string) ([]SpanMatch, chQueryMeta, error) {
	wire, meta, err := q.request(ctx, sql, params)
	if err != nil {
		return nil, meta, err
	}
	out := []SpanMatch{}
	for _, line := range bytes.Split(bytes.TrimSpace(wire), []byte{'\n'}) {
		if err := ctx.Err(); err != nil {
			return nil, meta, wrap("canceled", "span result", err)
		}
		if len(line) == 0 {
			continue
		}
		var row struct {
			Trace string      `json:"trace_id"`
			ID    string      `json:"span_id"`
			Start json.Number `json:"ts_ns"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, meta, wrap("corruption", "span result", err)
		}
		start, err := strconv.ParseInt(string(row.Start), 10, 64)
		if err != nil {
			return nil, meta, wrap("corruption", "span timestamp", err)
		}
		out = append(out, SpanMatch{Trace: row.Trace, ID: row.ID, Start: start})
		if len(out) > q.backend.cfg.MaxCandidates {
			return nil, meta, fail("budget", "CH span", "span row limit exceeded")
		}
	}
	return out, meta, nil
}

func (b *CHBackend) readQueryManifest(ctx context.Context) error {
	payload, err := b.queries.owner(ctx, "SELECT payload FROM "+b.database+".owner ORDER BY sequence DESC LIMIT 1 FORMAT JSONEachRow")
	if err != nil {
		return err
	}
	var owner Manifest
	if err := json.Unmarshal([]byte(payload), &owner); err != nil {
		return wrap("ownership", "manifest", err)
	}
	return checkManifest(b.cfg, owner, true)
}
