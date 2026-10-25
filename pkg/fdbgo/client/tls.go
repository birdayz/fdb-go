package client

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

// defaultTLSConfigDir is FDB's default config directory on Linux
// (C++ platform::getDefaultConfigPath()). cert.pem / key.pem found here are used
// only when the corresponding FDB_TLS_* env var is unset. A variable only so the
// tls_roots_unit_test binary can point openTLSConfig at a temp dir.
var defaultTLSConfigDir = "/etc/foundationdb"

// errNoCertificates is the cause when the CA file holds no PEM certificate.
var errNoCertificates = errors.New("no certificates found")

// TLSConfigError reports a TLS setting that cannot be used. It is tls_error (2107):
// errors.As finds a *wire.FDBError with that code, and the cause.
type TLSConfigError struct {
	Setting string // the variable or file at fault
	Err     error
}

func (e *TLSConfigError) Error() string {
	return fmt.Sprintf("tls_error: %s: %v", e.Setting, e.Err)
}

func (e *TLSConfigError) Unwrap() []error {
	return []error{&wire.FDBError{Code: 2107}, e.Err}
}

// lookupEnvFunc reads an environment variable as os.LookupEnv does; tests inject one.
type lookupEnvFunc func(string) (string, bool)

// resolveTLSConfig builds the TLS configuration of a ":tls" cluster file the way
// libfdb_c resolves its own (flow/TLSConfig.actor.cpp):
//   - FDB_TLS_CERTIFICATE_FILE / FDB_TLS_KEY_FILE / FDB_TLS_CA_FILE, each falling back
//     to <configDir>/cert.pem and key.pem when unset (a set but empty variable means
//     no file); the CA has no default;
//   - FDB_TLS_PASSWORD decrypts an encrypted PKCS#8 key;
//   - FDB_TLS_VERIFY_PEERS (default Check.Valid=1) decides which peers to accept,
//     against the CA file alone, with no host-name check.
//
// A setting that cannot be used is a *TLSConfigError.
func resolveTLSConfig(configDir string, lookupEnv lookupEnvFunc) (*tls.Config, error) {
	certPath := resolveTLSPath(lookupEnv, "FDB_TLS_CERTIFICATE_FILE", configDir, "cert.pem")
	keyPath := resolveTLSPath(lookupEnv, "FDB_TLS_KEY_FILE", configDir, "key.pem")
	caPath := resolveTLSPath(lookupEnv, "FDB_TLS_CA_FILE", "", "")
	password, _ := lookupEnv("FDB_TLS_PASSWORD")

	policy := transport.DefaultVerifyPolicy
	if s, ok := lookupEnv("FDB_TLS_VERIFY_PEERS"); ok {
		p, err := transport.ParseVerifyPeers(s)
		if err != nil {
			return nil, &TLSConfigError{Setting: "FDB_TLS_VERIFY_PEERS", Err: err}
		}
		policy = p
	}

	// libfdb_c trusts only the configured CA (no default verify paths).
	roots := x509.NewCertPool()
	cfg := &tls.Config{
		RootCAs:            roots,
		InsecureSkipVerify: true, // the policy below replaces the stdlib chain and host check
		VerifyConnection:   policy.VerifyConnection(roots),
	}
	if certPath != "" || keyPath != "" {
		if certPath == "" || keyPath == "" {
			return nil, &TLSConfigError{
				Setting: "FDB_TLS_CERTIFICATE_FILE/FDB_TLS_KEY_FILE",
				Err:     fmt.Errorf("client cert and key must both be set (cert=%q, key=%q)", certPath, keyPath),
			}
		}
		cert, err := loadKeyPair(certPath, keyPath, password)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if caPath != "" {
		caBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, &TLSConfigError{Setting: caPath, Err: err}
		}
		if !roots.AppendCertsFromPEM(caBytes) {
			return nil, &TLSConfigError{Setting: caPath, Err: errNoCertificates}
		}
	}
	return cfg, nil
}

func loadKeyPair(certPath, keyPath, password string) (tls.Certificate, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, &TLSConfigError{Setting: certPath, Err: err}
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, &TLSConfigError{Setting: keyPath, Err: err}
	}
	if keyPEM, err = decryptKeyPEM(keyPEM, password); err != nil {
		return tls.Certificate{}, &TLSConfigError{Setting: keyPath, Err: err}
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, &TLSConfigError{Setting: certPath + ", " + keyPath, Err: err}
	}
	return cert, nil
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
		cfg, err := resolveTLSConfig(defaultTLSConfigDir, os.LookupEnv)
		if err != nil {
			return nil, fmt.Errorf("resolve TLS config: %w", err)
		}
		return cfg, nil
	}
	return nil, nil
}

// resolveTLSPath returns the env var if set, even to "", else <configDir>/<defaultFile>
// when that exists (when both configDir and defaultFile are non-empty), else "".
func resolveTLSPath(lookupEnv lookupEnvFunc, envVar, configDir, defaultFile string) string {
	if v, ok := lookupEnv(envVar); ok {
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
