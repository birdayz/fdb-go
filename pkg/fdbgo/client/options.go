package client

import (
	"crypto/tls"
	"log/slog"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

// DialFunc is the custom-dialer signature (alias of transport.DialFunc) so
// callers configuring WithDialFunc don't need to import the transport package.
type DialFunc = transport.DialFunc

// Option configures a database opened via OpenDatabase / OpenDatabaseFromConfig.
// Options compose and are forward-compatible — new knobs don't break callers.
type Option func(*openOptions)

type openOptions struct {
	dialFn          DialFunc
	tlsConfig       *tls.Config
	logger          *slog.Logger
	clusterFilePath string // internal: set by OpenDatabase for cluster-file persistence (RFC-111)
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

// WithLogger sets the per-handle logger for the client's operational events
// (transaction retries, commit_unknown_result — RFC-097). nil (the default)
// uses slog.Default(), so zero-config apps keep the standard integration
// point (slog.SetDefault); the per-handle option exists for multi-tenant
// hosts and for tests, which must never mutate process-global state.
func WithLogger(logger *slog.Logger) Option {
	return func(o *openOptions) { o.logger = logger }
}
