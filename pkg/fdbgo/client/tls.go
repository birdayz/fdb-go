package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
)

// defaultTLSConfigDir is FDB's default config directory on Linux
// (C++ platform::getDefaultConfigPath()). cert.pem / key.pem found here are used
// only when the corresponding FDB_TLS_* env var is unset. Passed explicitly into
// resolveTLSConfig (tests inject a temp dir) — never mutated globally.
const defaultTLSConfigDir = "/etc/foundationdb"

// resolveTLSConfig builds a *crypto/tls.Config from the FDB_TLS_* environment,
// the way the C++ client resolves its TLS files (flow/TLSConfig.actor.cpp), per
// field: FDB_TLS_CERTIFICATE_FILE / FDB_TLS_KEY_FILE / FDB_TLS_CA_FILE, falling
// back to <configDir>/cert.pem and <configDir>/key.pem when those env vars are
// unset and the files exist (CA has no default).
//
// This is the env/cluster-file convenience layer: it merely *produces* a
// standard *tls.Config. Callers who want full control pass their own
// *tls.Config (WithTLSConfig), which takes precedence over this.
//
// Returns a non-nil config (possibly empty — empty attempts a real handshake and
// fails closed against a private FDB CA, never plaintext). Errors if a file that
// IS configured can't be loaded/parsed (don't silently run with half a config).
// getenv is injected (production passes os.Getenv) so the resolution is testable
// without mutating process environment — the tests stay parallel-safe.
func resolveTLSConfig(configDir string, getenv func(string) string) (*tls.Config, error) {
	certPath := resolveTLSPath(getenv, "FDB_TLS_CERTIFICATE_FILE", configDir, "cert.pem")
	keyPath := resolveTLSPath(getenv, "FDB_TLS_KEY_FILE", configDir, "key.pem")
	caPath := resolveTLSPath(getenv, "FDB_TLS_CA_FILE", "", "")

	cfg := &tls.Config{}
	if certPath != "" || keyPath != "" {
		if certPath == "" || keyPath == "" {
			return nil, fmt.Errorf("TLS client cert and key must both be set (cert=%q, key=%q)", certPath, keyPath)
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load TLS client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if caPath != "" {
		caBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("load TLS CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("parse TLS CA %q: no certificates found", caPath)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// openTLSConfig applies the database-open precedence (RFC-051):
//
//	WithTLSConfig(explicit)  →  use it (and enable TLS even without a ":tls" cluster)
//	cluster ":tls" suffix     →  FDB_TLS_* env / default config dir
//	neither                   →  nil (plaintext)
//
// A non-nil result is the sole "use TLS" signal downstream. Pulled out of
// OpenDatabaseFromConfig so the precedence — especially the explicit-wins
// invariant — is unit-testable without bootstrapping a cluster.
func openTLSConfig(o openOptions, cf *ClusterFile) (*tls.Config, error) {
	if o.tlsConfig != nil {
		return o.tlsConfig, nil
	}
	if cf.UseTLS {
		cfg, err := resolveTLSConfig(defaultTLSConfigDir, os.Getenv)
		if err != nil {
			return nil, fmt.Errorf("resolve TLS config: %w", err)
		}
		return cfg, nil
	}
	return nil, nil
}

// resolveTLSPath returns the env var if set, else <configDir>/<defaultFile> when
// that exists (when both configDir and defaultFile are non-empty), else "".
func resolveTLSPath(getenv func(string) string, envVar, configDir, defaultFile string) string {
	if v := getenv(envVar); v != "" {
		return v
	}
	if configDir != "" && defaultFile != "" {
		p := filepath.Join(configDir, defaultFile)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
