package transport

// Real TLS tests for the dial path (RFC-051). These use crypto/tls with
// in-process generated certs — NOT mocks: an actual mutual-TLS handshake is
// negotiated and the FDB ConnectPacket handshake rides inside the tunnel,
// exercising the exact Dial -> upgradeTLS production path (TLS is a transparent
// net.Conn wrapper). The client side is configured with a standard *tls.Config,
// the idiomatic API. No Docker.
//
// Note: there is no "TLS requested but no config" test — that failure mode is
// gone by construction. A non-nil *tls.Config is the only "use TLS" signal; nil
// means plaintext. There is no boolean to disagree with it, so a connection can
// never be silently downgraded.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// --- tiny in-test PKI ---------------------------------------------------------

func genCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
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
	cert, _ := x509.ParseCertificate(der)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// genLeaf signs a leaf cert with the CA. server leafs get an IP SAN of 127.0.0.1
// + ServerAuth; client leafs get ClientAuth. Returns cert+key PEM.
func genLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, server bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// clientTLSConfig builds the standard *tls.Config a caller would pass to Dial:
// RootCAs to verify the server, plus a client cert for mutual auth (nil PEM →
// no client cert).
func clientTLSConfig(t *testing.T, caPEM, cliCertPEM, cliKeyPEM []byte) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("bad CA PEM")
	}
	cfg := &tls.Config{RootCAs: pool}
	if cliCertPEM != nil {
		cert, err := tls.X509KeyPair(cliCertPEM, cliKeyPEM)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg
}

// startFDBTLSServer starts a TLS listener that requires+verifies a client cert
// (mutual auth), then speaks the server side of the FDB ConnectPacket handshake
// inside the tunnel. Returns its address.
func startFDBTLSServer(t *testing.T, serverCertPEM, serverKeyPEM, caPEM []byte) string {
	t.Helper()
	cert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // proves mutual auth
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var buf [ConnectPacketSize]byte
				if _, err := io.ReadFull(c, buf[:]); err != nil {
					return // handshake / client-cert verification failed
				}
				sp := ConnectPacket{ProtocolVersion: ProtocolVersion73, ConnectionID: 0x42}
				if _, err := c.Write(sp.Marshal()); err != nil {
					return
				}
				io.Copy(io.Discard, c) // drain until the client closes
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// TestDial_RealMutualTLS: Dial negotiates real mutual TLS with a standard
// *tls.Config and completes the FDB ConnectPacket exchange inside the tunnel,
// returning a working Conn. Proves the wiring end-to-end on the production path.
func TestDial_RealMutualTLS(t *testing.T) {
	t.Parallel()
	ca, caKey, caPEM := genCA(t)
	srvCert, srvKey := genLeaf(t, ca, caKey, true)
	cliCert, cliKey := genLeaf(t, ca, caKey, false)
	addr := startFDBTLSServer(t, srvCert, srvKey, caPEM)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, clientTLSConfig(t, caPEM, cliCert, cliKey), nil)
	if err != nil {
		t.Fatalf("Dial over real mutual TLS failed: %v", err)
	}
	defer c.Close()
	if c.PeerProtocolVersion() != ProtocolVersion73 {
		t.Errorf("peer protocol: got %#x, want %#x", c.PeerProtocolVersion(), ProtocolVersion73)
	}
}

// TestDial_WrongCARejected: a client trusting the wrong CA must fail to verify
// the server — no working connection.
func TestDial_WrongCARejected(t *testing.T) {
	t.Parallel()
	ca, caKey, caPEM := genCA(t)
	srvCert, srvKey := genLeaf(t, ca, caKey, true)
	cliCert, cliKey := genLeaf(t, ca, caKey, false)
	addr := startFDBTLSServer(t, srvCert, srvKey, caPEM)

	_, _, otherCAPEM := genCA(t) // a DIFFERENT CA
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, clientTLSConfig(t, otherCAPEM, cliCert, cliKey), nil)
	if err == nil {
		c.Close()
		t.Fatal("expected TLS verification failure with wrong CA, got a connection")
	}
}

// TestDial_MissingClientCertRejected: the server requires a client cert (mutual
// auth). A client with no cert must be rejected — proves mutual auth is actually
// enforced, not just advertised.
func TestDial_MissingClientCertRejected(t *testing.T) {
	t.Parallel()
	ca, caKey, caPEM := genCA(t)
	srvCert, srvKey := genLeaf(t, ca, caKey, true)
	addr := startFDBTLSServer(t, srvCert, srvKey, caPEM)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, clientTLSConfig(t, caPEM, nil, nil), nil) // CA only, no client cert
	if err == nil {
		c.Close()
		t.Fatal("expected mutual-auth failure (no client cert), got a connection")
	}
}
