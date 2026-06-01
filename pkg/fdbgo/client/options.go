package client

import (
	"crypto/tls"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

// DialFunc is the custom-dialer signature (alias of transport.DialFunc) so
// callers configuring WithDialFunc don't need to import the transport package.
type DialFunc = transport.DialFunc

// Option configures a database opened via OpenDatabase / OpenDatabaseFromConfig.
// Options compose and are forward-compatible — new knobs don't break callers.
type Option func(*openOptions)

type openOptions struct {
	dialFn    DialFunc
	tlsConfig *tls.Config
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
func WithTLSConfig(cfg *tls.Config) Option {
	return func(o *openOptions) { o.tlsConfig = cfg }
}
