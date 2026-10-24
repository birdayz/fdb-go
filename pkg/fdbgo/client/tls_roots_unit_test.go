package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// systemRootCert is the server certificate signed by the CA TestMain puts into the
// system roots, and systemRootsFile that CA.
var (
	systemRootCert  tls.Certificate
	systemRootsFile string
)

// TestMain installs a fresh CA as the process's system roots before anything loads
// them; x509 reads SSL_CERT_FILE once per process. It also gives openTLSConfig a
// default directory holding cert.pem and key.pem.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fdb-tls-roots")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	caPEM, cert, err := systemRootPKI()
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "roots.pem"), caPEM, 0o600)
	}
	if err == nil {
		err = writeDefaultKeyPair(dir, cert)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	systemRootsFile = filepath.Join(dir, "roots.pem")
	os.Setenv("SSL_CERT_FILE", systemRootsFile)
	os.Setenv("SSL_CERT_DIR", dir)
	systemRootCert = cert
	defaultTLSConfigDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func writeDefaultKeyPair(dir string, cert tls.Certificate) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
}

// startSystemRootServer serves the FDB ConnectPacket exchange behind TLS with
// systemRootCert and returns a dial through transport.Dial.
func startSystemRootServer(t *testing.T) func(cfg *tls.Config) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{systemRootCert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var buf [transport.ConnectPacketSize]byte
				if _, err := io.ReadFull(c, buf[:]); err != nil {
					return
				}
				sp := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 1}
				_, _ = c.Write(sp.Marshal())
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return func(cfg *tls.Config) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := transport.Dial(ctx, ln.Addr().String(), cfg, nil)
		if c != nil {
			c.Close()
		}
		return err
	}
}

func systemRootPKI() ([]byte, tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "system-root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// With no CA file the environment config trusts no root, as libfdb_c, which loads
// no default verify paths; a server the system roots vouch for is refused.
func TestResolveTLSConfigIgnoresSystemRoots(t *testing.T) {
	dial := startSystemRootServer(t)
	// The control: Go's own verification trusts the system roots.
	if err := dial(&tls.Config{}); err != nil {
		t.Fatalf("stdlib config with system roots: %v", err)
	}
	cfg, err := resolveTLSConfig(t.TempDir(), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	var pe *transport.PeerVerifyError
	var unknownCA x509.UnknownAuthorityError
	if err := dial(cfg); !errors.As(err, &pe) || !errors.As(err, &unknownCA) {
		t.Fatalf("environment config with no CA file: %v; want unknown authority for a system-root server", err)
	}
	// Nil roots in the policy trust nothing either, not the system pool.
	leaf, err := x509.ParseCertificate(systemRootCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.DefaultVerifyPolicy.Verify([]*x509.Certificate{leaf}, nil); !errors.As(err, &pe) || !errors.As(err, &unknownCA) {
		t.Fatalf("policy with nil roots: %v; want unknown authority", err)
	}
}

// openTLSConfig reads the process environment: an unset path variable falls back to
// the default directory and a set but empty one does not; the CA file and the
// verify-peers policy decide the handshake.
func TestOpenTLSConfigReadsEnvironment(t *testing.T) {
	for _, v := range []string{"FDB_TLS_CERTIFICATE_FILE", "FDB_TLS_KEY_FILE", "FDB_TLS_CA_FILE", "FDB_TLS_PASSWORD", "FDB_TLS_VERIFY_PEERS"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	open := func() *tls.Config {
		t.Helper()
		cfg, err := openTLSConfig(openOptions{}, &ClusterFile{UseTLS: true})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if cfg := open(); len(cfg.Certificates) != 1 {
		t.Fatalf("unset variables: %d certificates; want the default cert.pem and key.pem", len(cfg.Certificates))
	}
	t.Setenv("FDB_TLS_CERTIFICATE_FILE", "")
	t.Setenv("FDB_TLS_KEY_FILE", "")
	if cfg := open(); len(cfg.Certificates) != 0 {
		t.Fatalf("empty variables: %d certificates; want none", len(cfg.Certificates))
	}

	dial := startSystemRootServer(t)
	t.Setenv("FDB_TLS_CA_FILE", systemRootsFile)
	if err := dial(open()); err != nil {
		t.Fatalf("CA file from the environment: %v", err)
	}
	t.Setenv("FDB_TLS_VERIFY_PEERS", "S.CN=nobody")
	var pe *transport.PeerVerifyError
	if err := dial(open()); !errors.As(err, &pe) {
		t.Fatalf("FDB_TLS_VERIFY_PEERS from the environment: %v; want a PeerVerifyError", err)
	}
}
