package plugin

import (
	"context"
	"database/sql/driver"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/recordlayer"
)

// otelReader is the read-only OTEL_V1 facade (*otel.Reader) the plugin uses;
// tests substitute a fake.
type otelReader interface {
	Layout() string
	TraceOTLP(ctx context.Context, traceID string, b otel.TraceBudget) ([]*tracepb.ResourceSpans, otel.TraceReadStats, error)
	QuerySameSpan(ctx context.Context, q otel.SameSpanQuery) (otel.SpanQueryPage, error)
	QueryTraceExistential(ctx context.Context, q otel.TraceExistentialQuery) (otel.TraceExistentialResult, error)
	Query(ctx context.Context, a otel.Attribute) (otel.QueryResult, error)
	Time(ctx context.Context, tr otel.TimeRange) (otel.QueryResult, error)
	QueryLogsByTrace(ctx context.Context, traceID, spanID string) ([]otel.LogRecord, error)
	QueryLogsByAttribute(ctx context.Context, a otel.Attribute, limit int) ([]otel.LogRecord, error)
	AttributeKeys(ctx context.Context, signal, scope, typ, prefix string, limit int) ([]string, bool, error)
	AttributeValues(ctx context.Context, signal string, a otel.Attribute, prefix string, limit int) ([]string, bool, error)
	Close() error
}

var _ otelReader = (*otel.Reader)(nil)

// readerOpener opens a reader on one tenant schema (otel.OpenReader).
type readerOpener func(ctx context.Context, db *recordlayer.FDBDatabase, database, schema string, cfg otel.ReaderConfig) (otelReader, error)

// connectorFactory builds a database/sql connector over the instance's FDB
// database (sqldriver.NewConnector).
type connectorFactory func(db *recordlayer.FDBDatabase, dsn string) (driver.Connector, error)
