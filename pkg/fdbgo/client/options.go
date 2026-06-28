package client

import (
	"crypto/tls"
	"log/slog"

	oteltrace "go.opentelemetry.io/otel/trace"

	"fdb.dev/pkg/fdbgo/transport"
)

// DialFunc is the custom-dialer signature (alias of transport.DialFunc) so
// callers configuring WithDialFunc don't need to import the transport package.
type DialFunc = transport.DialFunc

// Option configures a database opened via OpenDatabase / OpenDatabaseFromConfig.
// Options compose and are forward-compatible — new knobs don't break callers.
type Option func(*openOptions)

type openOptions struct {
	dialFn            DialFunc
	tlsConfig         *tls.Config
	logger            *slog.Logger
	clusterFilePath   string           // internal: set by OpenDatabase for cluster-file persistence (RFC-111)
	rangeByteCeiling  int64            // opt-in GetRange materialization ceiling (RFC-115 §2); 0 = unlimited (default)
	tracingSampleRate float64          // distributed-trace sample rate (RFC-115 §4); 0.0 = unsampled (default, matches C++ TRACING_SAMPLE_RATE)
	tracer            oteltrace.Tracer // OpenTelemetry export backend (RFC-115 §4 Layer 2); nil → noop (no telemetry)
	apiVersion        int              // selected FDB API version (RFC-149); required — OpenDatabase rejects an unset version
}

func applyOptions(opts []Option) openOptions {
	var o openOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithDialFunc overrides the dialer used for every connection. nil (the default)
// uses a standard net.Dialer. Useful for fault injection, custom networking, or
// traffic shaping in tests.
func WithDialFunc(fn DialFunc) Option {
	return func(o *openOptions) { o.dialFn = fn }
}

// WithTLSConfig connects to the cluster over TLS using the given standard
// *crypto/tls.Config — bring any config: in-memory certs, rotation via
// GetClientCertificate, custom VerifyPeerCertificate, cipher/version policy.
//
// It takes precedence over the FDB_TLS_* environment / cluster-file ":tls"
// resolution and enables TLS even when the cluster string lacks ":tls". When not
// supplied, a ":tls" cluster string falls back to the FDB_TLS_* env convenience
// layer (matching the C++ client). A non-nil config is the only "use TLS" signal
// — there is no separate boolean, so TLS can never be silently downgraded.
//
// WithTLSConfig(nil) is a no-op (it does NOT force plaintext): a ":tls" cluster
// string + FDB_TLS_* env still enables TLS. To stay plaintext, use a cluster
// string without ":tls".
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *openOptions) { o.tlsConfig = cfg }
}

// withClusterFilePath records the on-disk cluster-file path so coordinator-set
// changes can be persisted back to it (RFC-111). Internal — set by OpenDatabase.
// OpenDatabaseFromConfig leaves it empty (memory-only, no persistence).
func withClusterFilePath(path string) Option {
	return func(o *openOptions) { o.clusterFilePath = path }
}

// WithRangeByteCeiling bounds how many bytes a single GetRange may materialize
// into memory before it fails with a *RangeMaterializationLimitError, instead of
// OOM-ing the process on a runaway unbounded scan. n ≤ 0 (the default) means
// UNLIMITED — matching libfdb_c, whose GetSliceWithError equivalent also
// materializes a range unbounded and never returns a "too big" error. This is a
// Go-only OPT-IN OOM safety valve, off by default so the default facade behavior
// stays oracle-matching; an operator sets a ceiling (e.g. 256<<20) as a
// last-resort guard. The bounded, streaming Iterator() honors StreamingMode and
// is the right tool for large result sets; this ceiling is the backstop for code
// that calls GetSliceWithError on an unexpectedly huge range. The cap bounds
// total materialized key+value bytes; a single read may overshoot it by at most
// one reply (~80 KB) before the check fires.
func WithRangeByteCeiling(n int64) Option {
	return func(o *openOptions) { o.rangeByteCeiling = n }
}

// WithTracingSampleRate sets the fraction (0.0–1.0) of transactions whose trace span
// is flagged SAMPLED. The default 0.0 matches C++ FLOW_KNOBS->TRACING_SAMPLE_RATE: every
// transaction still carries a real, randomly-generated SpanContext on every request
// (wire-faithful with C++), but flagged unsampled so collectors drop it. Raise it to
// emit sampled spans for a fraction of transactions. RFC-115 §4.
func WithTracingSampleRate(rate float64) Option {
	return func(o *openOptions) { o.tracingSampleRate = rate }
}

// WithTracer sets the OpenTelemetry tracer used to EXPORT client-side trace spans
// (the C++ ITracer analog — NoopTracer default, pluggable backend). The pure-Go
// client always GENERATES + propagates a SpanContext on the wire (RFC-115 §4 Layer 1)
// regardless of this; WithTracer adds the export half. Pass any
// go.opentelemetry.io/otel/trace.Tracer (from your OTLP/Jaeger/Datadog TracerProvider);
// the client emits a "Transaction" span plus per-operation child spans (getValue,
// getRange, commit, GRV, …), seeded with the same traceID it puts on the wire so
// FDB server-side spans land in the same trace. nil (the default) → an internal no-op
// tracer: zero telemetry, zero allocation on the hot path, no OTEL SDK pulled in.
// Spans are recorded only for sampled transactions (see WithTracingSampleRate).
//
// CAVEAT (span lifetime): the per-transaction "Transaction" span ends on commit
// success, OnError retry, Reset, or Cancel. Database.Transact/TransactCtx always hit
// one of these, so they are safe. A RAW Database.CreateTransaction() handle that you
// read from (starting the span) and then ABANDON without committing/Reset/Cancel will
// LEAK that span (it is never ended) — only matters with a real tracer AND a sampled
// transaction. Always Reset() or Cancel() a raw handle you don't commit.
func WithTracer(t oteltrace.Tracer) Option {
	return func(o *openOptions) { o.tracer = t }
}

// WithAPIVersion records the selected FDB API version; required — OpenDatabase
// rejects an unset version, mirroring fdb_select_api_version. Gates
// version-dependent wire behaviour, e.g. the Min→MinV2/And→AndV2 atomic upgrade
// at >=510.
func WithAPIVersion(v int) Option {
	return func(o *openOptions) { o.apiVersion = v }
}

// WithLogger sets the per-handle logger for the client's operational events
// (transaction retries, commit_unknown_result — RFC-097). nil (the default)
// uses slog.Default(), so zero-config apps keep the standard integration
// point (slog.SetDefault); the per-handle option exists for multi-tenant
// hosts and for tests, which must never mutate process-global state.
func WithLogger(logger *slog.Logger) Option {
	return func(o *openOptions) { o.logger = logger }
}
