package sqlbackend

import (
	"context"

	"fdb.dev/layers/otel"
	"fdb.dev/layers/otel/otelsql"
)

// The direct query path is the Exporter's: otel.SpanReader over the query
// tenant's store, one Record Layer transaction per query through the same
// OTEL_V1 indexes SQL's plans use (EXPLAIN: a covering reverse scan of
// SPANS_STR_ATTR / SPANS_INT_ATTR / SPANS_TIME, a covering SPANS_SPAN_ID scan,
// a primary-key scan for a trace), with no catalog read, no planning and no
// per-page store reopen.

// directReader resolves the query tenant's store once.
func (b *Backend) directReader(ctx context.Context) (*otel.SpanReader, error) {
	if b.reader != nil {
		return b.reader, nil
	}
	ts, err := otelsql.OpenTenantStore(ctx, b.db, b.dbPath, tenantSchema(b.cfg.Tenant), b.readSer)
	if err != nil {
		return nil, err
	}
	b.reader = otel.NewSpanReader(b.db, ts, b.cfg)
	return b.reader, nil
}

func (b *Backend) directQuery(ctx context.Context, a otel.Attribute) (otel.QueryResult, error) {
	b.lastWork = otel.QueryResult{}
	r, err := b.directReader(ctx)
	if err != nil {
		return otel.QueryResult{}, err
	}
	out, err := r.Query(ctx, a)
	b.lastWork = otel.QueryResult{Candidates: out.Candidates, Bytes: out.Bytes}
	return out, err
}

func (b *Backend) directTime(ctx context.Context, tr otel.TimeRange) (otel.QueryResult, error) {
	b.lastWork = otel.QueryResult{}
	r, err := b.directReader(ctx)
	if err != nil {
		return otel.QueryResult{}, err
	}
	out, err := r.Time(ctx, tr)
	b.lastWork = otel.QueryResult{Candidates: out.Candidates, Bytes: out.Bytes}
	return out, err
}

func (b *Backend) directSpan(ctx context.Context, span string) ([]otel.SpanMatch, error) {
	b.lastWork = otel.QueryResult{}
	r, err := b.directReader(ctx)
	if err != nil {
		return nil, err
	}
	out, work, err := r.Span(ctx, span)
	b.lastWork = work
	return out, err
}

func (b *Backend) directTrace(ctx context.Context, trace string) ([]otel.SpanHit, error) {
	b.lastWork = otel.QueryResult{}
	r, err := b.directReader(ctx)
	if err != nil {
		return nil, err
	}
	out, work, err := r.Trace(ctx, trace)
	b.lastWork = work
	return out, err
}
