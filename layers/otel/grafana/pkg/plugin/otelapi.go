package plugin

import (
	"context"
	"database/sql/driver"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"fdb.dev/layers/otel"
	"fdb.dev/pkg/recordlayer"
)

// STORE_META("layout") values (otel.StoreLayout*).
const (
	layoutMessagesNone     = "messages=none"
	layoutMessagesDeltas   = "messages=deltas"
	layoutMessagesFullText = "messages=fulltext"
)

// readerConfig mirrors otel.ReaderConfig.
type readerConfig struct {
	MaxCandidates      int
	MaxBytes           int64
	K                  int
	TransactionTimeout time.Duration
	TransactionTags    []string
}

// traceBudget mirrors otel.TraceBudget.
type traceBudget struct {
	MaxSpans int
	MaxBytes int64
}

// traceReadStats mirrors otel.TraceReadStats. Reason is "" or a comma-joined
// subset of max_spans, max_bytes and messages.
type traceReadStats struct {
	Spans     int
	Bytes     int64
	Truncated bool
	Reason    string
}

// otelReader is the read-only OTEL_V1 facade (*otel.Reader) the plugin uses.
type otelReader interface {
	Layout() string
	TraceOTLP(ctx context.Context, traceID string, b traceBudget) ([]*tracepb.ResourceSpans, traceReadStats, error)
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

// readerOpener opens a reader on one tenant schema (otel.OpenReader).
type readerOpener func(ctx context.Context, db *recordlayer.FDBDatabase, database, schema string, cfg readerConfig) (otelReader, error)

// connectorFactory builds a database/sql connector over the instance's FDB
// database (sqldriver.NewConnector).
type connectorFactory func(db *recordlayer.FDBDatabase, dsn string) (driver.Connector, error)
