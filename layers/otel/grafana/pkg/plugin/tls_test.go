package plugin

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
	"math/big"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

type pki struct {
	caPEM                string
	server, rogue        *x509.Certificate
	clientPEM, clientKey string
}

var (
	pkiOnce sync.Once
	pkiVal  pki
	pkiErr  error
)

// testPKI is a CA, a server certificate it signs (CN fdb-server), a client
// key pair, and a rogue self-signed server certificate.
func testPKI(t testing.TB) pki {
	t.Helper()
	pkiOnce.Do(func() { pkiVal, pkiErr = makePKI() })
	if pkiErr != nil {
		t.Fatal(pkiErr)
	}
	return pkiVal
}

func makePKI() (pki, error) {
	newKey := func() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }
	tmpl := func(cn string, serial int64, ca bool) *x509.Certificate {
		c := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		if ca {
			c.IsCA, c.BasicConstraintsValid = true, true
			c.KeyUsage |= x509.KeyUsageCertSign
		}
		return c
	}
	caKey, err := newKey()
	if err != nil {
		return pki{}, err
	}
	caTmpl := tmpl("test-ca", 1, true)
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return pki{}, err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	sign := func(cn string, serial int64, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
		k, err := newKey()
		if err != nil {
			return nil, nil, nil, err
		}
		c := tmpl(cn, serial, false)
		if parent == nil {
			parent, parentKey = c, k
		}
		der, err := x509.CreateCertificate(rand.Reader, c, parent, &k.PublicKey, parentKey)
		if err != nil {
			return nil, nil, nil, err
		}
		cert, err := x509.ParseCertificate(der)
		return cert, k, der, err
	}
	server, _, _, err := sign("fdb-server", 2, caCert, caKey)
	if err != nil {
		return pki{}, err
	}
	_, clientKey, clientDER, err := sign("grafana", 3, caCert, caKey)
	if err != nil {
		return pki{}, err
	}
	rogue, _, _, err := sign("fdb-server", 4, nil, nil)
	if err != nil {
		return pki{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return pki{}, err
	}
	return pki{
		caPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		server:    server,
		rogue:     rogue,
		clientPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})),
		clientKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
	}, nil
}

func TestBuildTLSConfigModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{tlsModeOff, tlsModeCluster} {
		cfg, err := buildTLSConfig(&Settings{TLSMode: mode})
		if err != nil || cfg != nil {
			t.Fatalf("%s: want no explicit config, got %v, %v", mode, cfg, err)
		}
	}
}

func TestBuildTLSConfigVerifiesPeers(t *testing.T) {
	t.Parallel()
	p := testPKI(t)
	cfg, err := buildTLSConfig(&Settings{TLSMode: tlsModeConfig, TLSCACert: p.caPEM, TLSClientCert: p.clientPEM, TLSClientKey: p.clientKey})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("want host-name check off and the policy hook on: %+v", cfg)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("client certificate missing")
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{p.server}}); err != nil {
		t.Fatalf("CA-signed peer refused: %v", err)
	}
	var pe *transport.PeerVerifyError
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{p.rogue}}); !errors.As(err, &pe) {
		t.Fatalf("self-signed peer accepted or wrong error: %v", err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{}); !errors.As(err, &pe) {
		t.Fatalf("peer without certificate accepted: %v", err)
	}
}

func TestBuildTLSConfigVerifyPeersPolicy(t *testing.T) {
	t.Parallel()
	p := testPKI(t)
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{p.server}}
	match, err := buildTLSConfig(&Settings{TLSMode: tlsModeConfig, TLSCACert: p.caPEM, TLSVerifyPeers: "S.CN=fdb-server"})
	if err != nil {
		t.Fatal(err)
	}
	if err := match.VerifyConnection(state); err != nil {
		t.Fatalf("matching CN refused: %v", err)
	}
	other, err := buildTLSConfig(&Settings{TLSMode: tlsModeConfig, TLSCACert: p.caPEM, TLSVerifyPeers: "S.CN=other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.VerifyConnection(state); err == nil {
		t.Fatal("peer outside the verify-peers rule accepted")
	}
}

func TestBuildTLSConfigRefusals(t *testing.T) {
	t.Parallel()
	p := testPKI(t)
	cases := map[string]*Settings{
		"tlsCACert":      {TLSMode: tlsModeConfig, TLSCACert: "not pem"},
		"tlsVerifyPeers": {TLSMode: tlsModeConfig, TLSCACert: p.caPEM, TLSVerifyPeers: "Check.Bogus=1"},
		"tlsClientCert":  {TLSMode: tlsModeConfig, TLSCACert: p.caPEM, TLSClientCert: p.clientPEM, TLSClientKey: "junk"},
	}
	for field, s := range cases {
		_, err := buildTLSConfig(s)
		var se *SettingsError
		if !errors.As(err, &se) || se.Field != field {
			t.Errorf("%s: got %v", field, err)
		}
	}
}

// A ":tls" cluster with tlsMode off is refused before any dial.
func TestOpenFDBRefusesTLSClusterWhenOff(t *testing.T) {
	t.Parallel()
	s, err := ParseSettings(instanceSettings(t, map[string]any{"clusterString": "test:abc@127.0.0.1:4500:tls"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = openFDB(ctx, s, instanceSettings(t, nil))
	var se *SettingsError
	if !errors.As(err, &se) || se.Field != "tlsMode" {
		t.Fatalf("got %v", err)
	}
}

// PDC enabled on the datasource with no proxy configured in Grafana is
// refused rather than dialed directly.
func TestOpenFDBRefusesMissingPDC(t *testing.T) {
	t.Parallel()
	inst := instanceSettings(t, map[string]any{"enableSecureSocksProxy": true})
	s, err := ParseSettings(inst)
	if err != nil {
		t.Fatal(err)
	}
	if !s.SecureSocksProxy {
		t.Fatal("enableSecureSocksProxy not parsed")
	}
	_, err = openFDB(context.Background(), s, inst)
	if !errors.Is(err, errPDCUnavailable) {
		t.Fatalf("want errPDCUnavailable, got %v", err)
	}
}
