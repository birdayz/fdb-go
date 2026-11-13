package otel

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type nativeQueryConn interface {
	Query(context.Context, string, ...any) (driver.Rows, error)
	Close() error
}

type chNativeQueries struct {
	conn nativeQueryConn
	cfg  Config
}

func openCHNativeQueries(c Config, endpoint *url.URL) (*chNativeQueries, error) {
	address, err := nativeAddress(c.CHNativeAddr)
	if err != nil {
		return nil, err
	}
	if _, err := nativeAddress(net.JoinHostPort(endpoint.Hostname(), "1")); err != nil {
		return nil, fail("config", "CH native queries", "native queries require a loopback HTTP administration endpoint")
	}
	auth := clickhouse.Auth{Database: "default", Username: "default"}
	if endpoint.User != nil {
		auth.Username = endpoint.User.Username()
		auth.Password, _ = endpoint.User.Password()
	}
	// Open is lazy: no ping, query, or hidden warmup precedes the timed calls.
	conn, err := clickhouse.Open(&clickhouse.Options{
		Protocol: clickhouse.Native, Addr: []string{address}, Auth: auth,
		DialTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Hour,
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionNone},
		Settings:    clickhouse.Settings{"use_query_cache": 0},
	})
	if err != nil {
		return nil, wrap("operation", "open CH native queries", err)
	}
	return &chNativeQueries{conn: conn, cfg: c}, nil
}

func (q *chNativeQueries) close() error { return q.conn.Close() }

// Progress callbacks need not arrive for fast queries; they are never treated
// as complete read-work metrics. Query IDs support later out-of-clock reconciliation.
func (q *chNativeQueries) rows(ctx context.Context, sql string, params map[string]string, consume func(driver.Rows) error) (meta chQueryMeta, err error) {
	if err := ctx.Err(); err != nil {
		return meta, wrap("canceled", "CH native query", err)
	}
	id, err := newCHQueryID()
	if err != nil {
		return meta, err
	}
	meta = chQueryMeta{queryID: id, transport: "native", metricsSource: "unavailable"}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	queryCtx := clickhouse.Context(bounded, clickhouse.WithQueryID(id), clickhouse.WithParameters(clickhouse.Parameters(params)), clickhouse.WithSettings(clickhouse.Settings{"use_query_cache": 0}))
	rows, err := q.conn.Query(queryCtx, strings.TrimSuffix(sql, " FORMAT JSONEachRow"))
	if err != nil {
		// Query releases its connection on error and may return typed-nil rows.
		// Only a successful Query transfers row ownership to this caller.
		if bounded.Err() != nil {
			err = errors.Join(err, bounded.Err())
		}
		return meta, wrap("operation", "CH native query", err)
	}
	if rows == nil {
		return meta, fail("operation", "CH native query", "driver returned no rows interface")
	}
	defer func() {
		err = errors.Join(err, wrap("operation", "CH native rows close", rows.Close()))
		if bounded.Err() != nil {
			err = errors.Join(err, wrap("canceled", "CH native result", bounded.Err()))
		}
	}()
	for rows.Next() {
		if err := bounded.Err(); err != nil {
			return meta, wrap("canceled", "CH native result", err)
		}
		if err := consume(rows); err != nil {
			return meta, err
		}
	}
	if err := rows.Err(); err != nil {
		return meta, wrap("operation", "CH native rows", err)
	}
	if err := bounded.Err(); err != nil {
		return meta, wrap("canceled", "CH native result", err)
	}
	return meta, nil
}

func (q *chNativeQueries) owner(ctx context.Context, sql string) (string, error) {
	var payload string
	count := 0
	_, err := q.rows(ctx, sql, nil, func(rows driver.Rows) error {
		count++
		if count > 1 {
			return fail("ownership", "manifest", "owner query returned more than one row")
		}
		if err := rows.Scan(&payload); err != nil {
			return wrap("ownership", "native manifest", err)
		}
		if len(payload) > 32<<20 {
			return fail("budget", "CH native owner", "decoded owner exceeds 32MiB cap")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if count != 1 {
		return "", fail("ownership", "manifest", "owner query returned no row")
	}
	return payload, nil
}

func (q *chNativeQueries) hits(ctx context.Context, sql string, params map[string]string) ([]Hit, chQueryMeta, error) {
	out := []Hit{}
	var decoded int64
	meta, err := q.rows(ctx, sql, params, func(rows driver.Rows) error {
		var hit Hit
		if err := rows.Scan(&hit.Trace, &hit.Latest); err != nil {
			return wrap("corruption", "CH native trace result", err)
		}
		decoded += int64(len(hit.Trace)) + 8
		if len(out) >= q.cfg.K || len(out) >= q.cfg.MaxCandidates || decoded > 32<<20 {
			return fail("budget", "CH native result", "result count or decoded bytes exceed budget")
		}
		out = append(out, hit)
		return nil
	})
	return out, meta, err
}

func (q *chNativeQueries) trace(ctx context.Context, sql string, params map[string]string) ([]SpanHit, chQueryMeta, error) {
	out := []SpanHit{}
	var decoded int64
	meta, err := q.rows(ctx, sql, params, func(rows driver.Rows) error {
		var hit SpanHit
		if err := rows.Scan(&hit.ID, &hit.Start); err != nil {
			return wrap("corruption", "CH native span result", err)
		}
		decoded += int64(len(hit.ID)) + 8
		if len(out) >= q.cfg.MaxCandidates || decoded > 32<<20 {
			return fail("budget", "CH native result", "result count or decoded bytes exceed budget")
		}
		out = append(out, hit)
		return nil
	})
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}

func (q *chNativeQueries) spans(ctx context.Context, sql string, params map[string]string) ([]SpanMatch, chQueryMeta, error) {
	out := []SpanMatch{}
	var decoded int64
	meta, err := q.rows(ctx, sql, params, func(rows driver.Rows) error {
		var hit SpanMatch
		if err := rows.Scan(&hit.Trace, &hit.ID, &hit.Start); err != nil {
			return wrap("corruption", "CH native span result", err)
		}
		decoded += int64(len(hit.Trace)+len(hit.ID)) + 8
		if len(out) >= q.cfg.MaxCandidates || decoded > 32<<20 {
			return fail("budget", "CH native result", "result count or decoded bytes exceed budget")
		}
		out = append(out, hit)
		return nil
	})
	if err != nil {
		return nil, meta, err
	}
	return out, meta, nil
}
