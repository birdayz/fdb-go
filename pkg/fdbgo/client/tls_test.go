package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseClusterString_TLS pins the ":tls" coordinator-suffix parsing, faithful
// to C++ NetworkAddress::parse (flow/network.cpp): strip "(fromHostname)" then a
// trailing ":tls" when len>4, set ClusterFile.UseTLS, reject mixed.
func TestParseClusterString_TLS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		wantTLS   bool
		wantCoord []string
		wantErr   bool
	}{
		{name: "plain single", in: "d:i@127.0.0.1:4500", wantTLS: false, wantCoord: []string{"127.0.0.1:4500"}},
		{name: "tls single", in: "d:i@127.0.0.1:4500:tls", wantTLS: true, wantCoord: []string{"127.0.0.1:4500"}},
		{name: "tls multi uniform", in: "d:i@10.0.0.1:4500:tls,10.0.0.2:4500:tls", wantTLS: true, wantCoord: []string{"10.0.0.1:4500", "10.0.0.2:4500"}},
		{name: "plain multi", in: "d:i@10.0.0.1:4500,10.0.0.2:4500", wantTLS: false, wantCoord: []string{"10.0.0.1:4500", "10.0.0.2:4500"}},
		{name: "mixed rejected", in: "d:i@10.0.0.1:4500:tls,10.0.0.2:4500", wantErr: true},
		{name: "mixed rejected rev", in: "d:i@10.0.0.1:4500,10.0.0.2:4500:tls", wantErr: true},
		{name: "ipv6 tls", in: "d:i@[::1]:4500:tls", wantTLS: true, wantCoord: []string{"[::1]:4500"}},
		{name: "hostname tls", in: "d:i@myhost:4500:tls", wantTLS: true, wantCoord: []string{"myhost:4500"}},
		{name: "fromHostname then tls", in: "d:i@myhost:4500:tls(fromHostname)", wantTLS: true, wantCoord: []string{"myhost:4500"}},
		{name: "bare tls invalid", in: "d:i@a:tls", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cf, err := ParseClusterString(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got cf=%+v", tc.in, cf)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseClusterString(%q): %v", tc.in, err)
			}
			if cf.UseTLS != tc.wantTLS {
				t.Errorf("UseTLS: got %v, want %v", cf.UseTLS, tc.wantTLS)
			}
			if len(cf.Coordinators) != len(tc.wantCoord) {
				t.Fatalf("coordinators: got %v, want %v", cf.Coordinators, tc.wantCoord)
			}
			for i := range tc.wantCoord {
				if cf.Coordinators[i] != tc.wantCoord[i] {
					t.Errorf("coordinator[%d]: got %q, want %q", i, cf.Coordinators[i], tc.wantCoord[i])
				}
			}
		})
	}
}

// TestResolveTLSPath pins the C++ per-field precedence: env var wins, else
// <configDir>/<defaultFile> if it exists, else "".
func TestResolveTLSPath(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", "/env/cert.pem")
		if got := resolveTLSPath("FDB_TLS_CERTIFICATE_FILE", "/ignored", "cert.pem"); got != "/env/cert.pem" {
			t.Errorf("env should win: got %q", got)
		}
	})
	t.Run("default dir if file exists", func(t *testing.T) {
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", "")
		dir := t.TempDir()
		p := filepath.Join(dir, "cert.pem")
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := resolveTLSPath("FDB_TLS_CERTIFICATE_FILE", dir, "cert.pem"); got != p {
			t.Errorf("default-dir resolution: got %q, want %q", got, p)
		}
	})
	t.Run("empty when nothing", func(t *testing.T) {
		t.Setenv("FDB_TLS_CA_FILE", "")
		if got := resolveTLSPath("FDB_TLS_CA_FILE", t.TempDir(), "ca.pem"); got != "" {
			t.Errorf("nothing present → empty, got %q", got)
		}
		// CA has no default file at all (configDir/defaultFile empty).
		if got := resolveTLSPath("FDB_TLS_CA_FILE", "", ""); got != "" {
			t.Errorf("no default → empty, got %q", got)
		}
	})
}

// TestResolveTLSConfig loads real files into a *tls.Config and checks the error
// paths. The default-dir stat is reached only via this path (callers gate on a
// TLS cluster), so a plaintext open never touches /etc/foundationdb.
func TestResolveTLSConfig(t *testing.T) {
	caPEM, certPEM, keyPEM := testCertPEM(t)

	t.Run("CA only from env → RootCAs set, no client cert", func(t *testing.T) {
		caFile := writeTLSTemp(t, "ca.pem", caPEM)
		t.Setenv("FDB_TLS_CA_FILE", caFile)
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", "")
		t.Setenv("FDB_TLS_KEY_FILE", "")
		cfg, err := resolveTLSConfig(t.TempDir()) // empty default dir
		if err != nil {
			t.Fatalf("resolveTLSConfig: %v", err)
		}
		if cfg == nil || cfg.RootCAs == nil {
			t.Fatalf("RootCAs should be set, got %+v", cfg)
		}
		if len(cfg.Certificates) != 0 {
			t.Errorf("no client cert expected, got %d", len(cfg.Certificates))
		}
	})

	t.Run("cert+key+CA → mutual config", func(t *testing.T) {
		t.Setenv("FDB_TLS_CA_FILE", writeTLSTemp(t, "ca.pem", caPEM))
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", writeTLSTemp(t, "cert.pem", certPEM))
		t.Setenv("FDB_TLS_KEY_FILE", writeTLSTemp(t, "key.pem", keyPEM))
		cfg, err := resolveTLSConfig(t.TempDir())
		if err != nil {
			t.Fatalf("resolveTLSConfig: %v", err)
		}
		if cfg.RootCAs == nil || len(cfg.Certificates) != 1 {
			t.Errorf("expected RootCAs + 1 client cert, got %+v", cfg)
		}
	})

	t.Run("cert without key → error", func(t *testing.T) {
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", writeTLSTemp(t, "cert.pem", certPEM))
		t.Setenv("FDB_TLS_KEY_FILE", "")
		t.Setenv("FDB_TLS_CA_FILE", "")
		if _, err := resolveTLSConfig(t.TempDir()); err == nil {
			t.Error("cert without key must error")
		}
	})

	t.Run("unreadable CA → error", func(t *testing.T) {
		t.Setenv("FDB_TLS_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", "")
		t.Setenv("FDB_TLS_KEY_FILE", "")
		if _, err := resolveTLSConfig(t.TempDir()); err == nil {
			t.Error("missing CA file must error, not silently run without it")
		}
	})

	t.Run("nothing configured → empty non-nil config", func(t *testing.T) {
		t.Setenv("FDB_TLS_CA_FILE", "")
		t.Setenv("FDB_TLS_CERTIFICATE_FILE", "")
		t.Setenv("FDB_TLS_KEY_FILE", "")
		cfg, err := resolveTLSConfig(t.TempDir())
		if err != nil || cfg == nil {
			t.Fatalf("want empty non-nil config, got cfg=%v err=%v", cfg, err)
		}
		if cfg.RootCAs != nil || len(cfg.Certificates) != 0 {
			t.Errorf("expected empty config, got %+v", cfg)
		}
	})
}

// --- tiny in-test PKI for the loader tests -----------------------------------

func testCertPEM(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	// Self-signed: it is its own CA, and a valid cert+key pair.
	return pemCert, pemCert, pemKey
}

func writeTLSTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
