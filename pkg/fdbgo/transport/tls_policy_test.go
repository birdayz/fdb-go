package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"
)

// The verify-peers grammar of TLSConfig.actor.cpp:384-611.
func TestParseVerifyPeers(t *testing.T) {
	t.Parallel()
	cn := asn1.ObjectIdentifier{2, 5, 4, 3}
	o := asn1.ObjectIdentifier{2, 5, 4, 10}
	ok := []struct {
		in    string
		rules []verifyRule
	}{
		{"Check.Valid=1", []verifyRule{{verifyCert: true}}},
		{"Check.Valid=0", []verifyRule{{}}},
		{"Check.Unexpired=0", []verifyRule{{verifyCert: true}}},
		{"Check.Valid=0,CN=a", []verifyRule{{subject: []criterion{{oid: cn, value: "a"}}}}},
		{"", []verifyRule{{verifyCert: true}}},
		{"CN=a", []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a"}}}}},
		{"S.CN=a,", []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a"}}}}},
		{"Subject.CN>=a", []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a", match: matchPrefix}}}}},
		{"I.O<=x", []verifyRule{{verifyCert: true, issuer: []criterion{{oid: o, value: "x", match: matchSuffix}}}}},
		{"Root.CN=r", []verifyRule{{verifyCert: true, root: []criterion{{oid: cn, value: "r"}}}}},
		{"subjectAltName=DNS:x", []verifyRule{{verifyCert: true, subject: []criterion{{value: "DNS:x"}}}}},
		{`CN=a\,b\41\ `, []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a,bA "}}}}},
		{"CN=a=b#c|O=x", []verifyRule{
			{verifyCert: true, subject: []criterion{{oid: cn, value: "a=b#c"}}},
			{verifyCert: true, subject: []criterion{{oid: o, value: "x"}}},
		}},
		{"CN=a|", []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a"}}}, {verifyCert: true}}},
		{"|CN=a", []verifyRule{{verifyCert: true}, {verifyCert: true, subject: []criterion{{oid: cn, value: "a"}}}}},
		{"CN=a,CN=b", []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a"}, {oid: cn, value: "b"}}}}},
		{"CN=a,CN=a", []verifyRule{{verifyCert: true, subject: []criterion{{oid: cn, value: "a"}}}}},
	}
	for _, tc := range ok {
		p, err := ParseVerifyPeers(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !equalRules(p.rules, tc.rules) {
			t.Errorf("%q: rules %+v, want %+v", tc.in, p.rules, tc.rules)
		}
	}
	for _, in := range []string{
		"Check.Valid=2", "Check.Valid=", "Check.Valid=11", "Check.Valid>=1", "Check.Other=1",
		"X.CN=a", "cn=a", "E=a", "S.Check.Valid=1", "CN", "=a", "CN=",
		"CN=#a", "CN= a", "CN=a ", "CN=a ,O=b", "CN=a,,O=b", "CN=a+b", "CN=a;b",
		`CN=a\zz`, `CN=a\`, `CN=a\4`, `CN=a\|b|O=x`, `CN=a\\|O=x`,
	} {
		if p, err := ParseVerifyPeers(in); err == nil {
			t.Errorf("%q parsed as %+v; want an error", in, p.rules)
		} else if pe := (*VerifyPeersError)(nil); !errors.As(err, &pe) {
			t.Errorf("%q: %T is not a VerifyPeersError", in, err)
		}
	}
}

func equalRules(a, b []verifyRule) bool {
	if len(a) != len(b) {
		return false
	}
	eq := func(x, y []criterion) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !x[i].oid.Equal(y[i].oid) || x[i].value != y[i].value || x[i].match != y[i].match {
				return false
			}
		}
		return true
	}
	for i := range a {
		if a[i].verifyCert != b[i].verifyCert || !eq(a[i].subject, b[i].subject) ||
			!eq(a[i].issuer, b[i].issuer) || !eq(a[i].root, b[i].root) {
			return false
		}
	}
	return true
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// issue signs tmpl for key with ca, or self-signs it when ca is nil.
func issue(t *testing.T, tmpl *x509.Certificate, key *ecdsa.PrivateKey, ca *testCA) *x509.Certificate {
	t.Helper()
	parent, signer := tmpl, key
	if ca != nil {
		parent, signer = ca.cert, ca.key
	}
	if tmpl.SerialNumber == nil {
		tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	}
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newCAWith(t *testing.T, name pkix.Name, key *ecdsa.PrivateKey, parent *testCA) *testCA {
	t.Helper()
	tmpl := &x509.Certificate{
		Subject: name, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	return &testCA{cert: issue(t, tmpl, key, parent), key: key}
}

func serverLeaf(t *testing.T, ca *testCA, edit func(*x509.Certificate)) *x509.Certificate {
	t.Helper()
	spiffe, _ := url.Parse("spiffe://fdb/server-a")
	tmpl := &x509.Certificate{
		Subject: pkix.Name{
			CommonName: "server-a", Organization: []string{"fdb"},
			OrganizationalUnit: []string{"ops", "dev"}, Country: []string{"US"},
		},
		DNSNames:       []string{"a.fdb.test"},
		EmailAddresses: []string{"ops@fdb.test"},
		URIs:           []*url.URL{spiffe},
		IPAddresses:    []net.IP{net.ParseIP("10.0.0.1")},
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if edit != nil {
		edit(tmpl)
	}
	return issue(t, tmpl, newTestKey(t), ca)
}

// The policy accepts or rejects a peer as TLSPolicy::verify_peer does.
func TestVerifyPolicyMatching(t *testing.T) {
	t.Parallel()
	ca := newCAWith(t, pkix.Name{CommonName: "root-ca", Organization: []string{"FDB"}}, newTestKey(t), nil)
	other := newCAWith(t, pkix.Name{CommonName: "other-ca"}, newTestKey(t), nil)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	leaf := serverLeaf(t, ca, nil)
	cafe := serverLeaf(t, ca, func(c *x509.Certificate) { c.Subject.CommonName = "café" })
	clientOnly := serverLeaf(t, ca, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} })
	anyOnly := serverLeaf(t, ca, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny} })
	anyAndServer := serverLeaf(t, ca, func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny, x509.ExtKeyUsageServerAuth}
	})
	expired := serverLeaf(t, ca, func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	})
	unknown := serverLeaf(t, other, nil)
	rawURI := serverLeaf(t, ca, func(c *x509.Certificate) {
		c.DNSNames, c.EmailAddresses, c.URIs, c.IPAddresses = nil, nil, nil, nil
		c.ExtraExtensions = []pkix.Extension{{Id: oidSubjectAltName, Value: rawSANExtension(t, "SPIFFE://fdb/raw")}}
	})
	if got := rawURI.URIs[0].String(); got != "spiffe://fdb/raw" {
		t.Fatalf("parsed URI %q: the case below needs String() to differ from the raw SAN", got)
	}

	cases := []struct {
		policy string
		peer   *x509.Certificate
		accept bool
	}{
		{"Check.Valid=1", leaf, true},
		{"CN=server-a", leaf, true},
		{"CN=server-b", leaf, false},
		{"CN>=server", leaf, true},
		{"CN<=-a", leaf, true},
		{"CN<=-b", leaf, false},
		{"O=fdb", leaf, true},
		{"OU=ops", leaf, false}, // OU occurs twice
		{"C=US,O=fdb", leaf, true},
		{"CN=server-a,O=other", leaf, false},
		{"CN=server-a,CN=x", leaf, false},
		{"CN=server-b|CN=server-a", leaf, true},
		{`CN=caf\E9`, cafe, true},     // the criterion's bytes are Latin-1
		{`CN=caf\C3\A9`, cafe, false}, // ... so raw UTF-8 bytes are widened again
		{"CN=café", cafe, false},
		{"I.CN=root-ca", leaf, true},
		{"I.CN=other-ca", leaf, false},
		{"R.O=FDB", leaf, true},
		{"I.subjectAltName=DNS:a.fdb.test", leaf, true}, // the leaf's own SAN
		{"R.subjectAltName=DNS:a.fdb.test", leaf, false},
		{"subjectAltName=DNS:a.fdb.test", leaf, true},
		{"subjectAltName>=DNS:a.fdb", leaf, true},
		{"subjectAltName>=DNS:", leaf, false}, // an empty criterion matches nothing
		{"subjectAltName=DNS", leaf, false},
		{"subjectAltName=EMAIL:ops@fdb.test", leaf, true},
		{"subjectAltName=DNS:ops@fdb.test", leaf, true}, // falls through from EMAIL
		{"subjectAltName=EMAIL:a.fdb.test", leaf, false},
		{"subjectAltName=URI:spiffe://fdb/server-a", leaf, true},
		{"subjectAltName=IP:spiffe://fdb/server-a", leaf, true}, // falls through from URI
		{"subjectAltName=IP:10.0.0.1", leaf, false},             // an iPAddress never matches
		{"subjectAltName=URI:SPIFFE://fdb/raw", rawURI, true},   // the URI as written ...
		{"subjectAltName=URI:spiffe://fdb/raw", rawURI, false},  // ... not URL.String()
		{"Check.Valid=1", unknown, false},
		{"Check.Valid=0", unknown, true},
		{"CN=nobody|Check.Valid=0", unknown, true},
		{"Check.Valid=1", clientOnly, false},
		{"Check.Unexpired=0", expired, false},
	}
	for _, tc := range cases {
		p, err := ParseVerifyPeers(tc.policy)
		if err != nil {
			t.Fatalf("%q: %v", tc.policy, err)
		}
		err = p.Verify([]*x509.Certificate{tc.peer}, roots)
		if accepted := err == nil; accepted != tc.accept {
			t.Errorf("%q on %s: accepted %t (%v), want %t", tc.policy, tc.peer.Subject.CommonName, accepted, err, tc.accept)
		}
		var pe *PeerVerifyError
		if err != nil && !errors.As(err, &pe) {
			t.Errorf("%q: %T is not a PeerVerifyError", tc.policy, err)
		}
	}
	if err := DefaultVerifyPolicy.Verify([]*x509.Certificate{leaf}, roots); err != nil {
		t.Errorf("default policy: %v", err)
	}
	if err := DefaultVerifyPolicy.Verify(nil, roots); err == nil {
		t.Error("default policy accepted no certificate")
	}

	// Each rejection names its cause.
	var unknownCA x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	for _, tc := range []struct {
		name  string
		peer  *x509.Certificate
		roots *x509.CertPool
		cause func(error) bool
	}{
		{"unknown CA", unknown, roots, func(err error) bool { return errors.As(err, &unknownCA) }},
		{"ClientAuth only", clientOnly, roots, func(err error) bool {
			return errors.As(err, &invalid) && invalid.Reason == x509.IncompatibleUsage
		}},
		{"expired", expired, roots, func(err error) bool { return errors.As(err, &invalid) && invalid.Reason == x509.Expired }},
		// OpenSSL's ssl_server purpose rejects anyExtendedKeyUsage without serverAuth.
		{"anyExtendedKeyUsage only", anyOnly, roots, func(err error) bool { return errors.Is(err, errAnyEKUOnly) }},
	} {
		err := DefaultVerifyPolicy.Verify([]*x509.Certificate{tc.peer}, tc.roots)
		var pe *PeerVerifyError
		if !errors.As(err, &pe) || !tc.cause(err) {
			t.Errorf("%s: %v; want a PeerVerifyError with its chain error", tc.name, err)
		}
	}
	if err := DefaultVerifyPolicy.Verify([]*x509.Certificate{anyAndServer}, roots); err != nil {
		t.Errorf("anyExtendedKeyUsage with serverAuth: %v", err)
	}
	mismatch, _ := ParseVerifyPeers("CN=server-b")
	err := mismatch.Verify([]*x509.Certificate{leaf}, roots)
	if pe := (*PeerVerifyError)(nil); !errors.As(err, &pe) || pe.Err != nil {
		t.Errorf("rule mismatch: %v; want a PeerVerifyError without a chain error", err)
	}
}

// rawSANExtension encodes a subjectAltName with one uniformResourceIdentifier, as written.
func rawSANExtension(t *testing.T, uri string) []byte {
	t.Helper()
	der, err := asn1.Marshal([]asn1.RawValue{{Class: asn1.ClassContextSpecific, Tag: sanURI, Bytes: []byte(uri)}})
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// A peer with two valid chains is accepted when a root rule matches either one.
func TestVerifyPolicyAnyChain(t *testing.T) {
	t.Parallel()
	rootA := newCAWith(t, pkix.Name{CommonName: "root-a"}, newTestKey(t), nil)
	rootB := newCAWith(t, pkix.Name{CommonName: "root-b"}, newTestKey(t), nil)
	interKey := newTestKey(t)
	interName := pkix.Name{CommonName: "intermediate"}
	viaA := newCAWith(t, interName, interKey, rootA)
	viaB := newCAWith(t, interName, interKey, rootB)
	leaf := serverLeaf(t, viaA, nil)
	roots := x509.NewCertPool()
	roots.AddCert(rootA.cert)
	roots.AddCert(rootB.cert)
	peer := []*x509.Certificate{leaf, viaA.cert, viaB.cert}
	// I. reads the leaf's issuer (the intermediate), R. the chain's last certificate.
	for policy, accept := range map[string]bool{
		"R.CN=root-a": true, "R.CN=root-b": true, "R.CN=root-c": false,
		"I.CN=intermediate": true, "I.CN=root-a": false,
	} {
		p, err := ParseVerifyPeers(policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Verify(peer, roots); (err == nil) != accept {
			t.Errorf("%q: %v, want accepted %t", policy, err, accept)
		}
	}
}

// The environment policy reads no host name: through upgradeTLS a policy config
// accepts a certificate for another host, which a stdlib config or a mismatched
// ServerName rejects.
func TestUpgradeTLSPolicyHasNoHostName(t *testing.T) {
	t.Parallel()
	ca := newCAWith(t, pkix.Name{CommonName: "root-ca"}, newTestKey(t), nil)
	key := newTestKey(t)
	leaf := issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "server"},
		IPAddresses: []net.IP{net.ParseIP("10.9.9.9")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, key, ca)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}},
	})
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
				_ = c.(*tls.Conn).Handshake()
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	addr := net.JoinHostPort("localhost", port)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	dial := func(cfg *tls.Config) error {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		tc, err := upgradeTLS(conn, addr, cfg)
		if tc != nil {
			tc.Close()
		}
		return err
	}
	var hostErr x509.HostnameError
	policy := &tls.Config{RootCAs: roots, InsecureSkipVerify: true, VerifyConnection: DefaultVerifyPolicy.VerifyConnection(roots)}
	if err := dial(policy); err != nil {
		t.Errorf("policy config: %v; want the handshake", err)
	}
	if err := dial(&tls.Config{RootCAs: roots}); !errors.As(err, &hostErr) {
		t.Errorf("stdlib config: %v; want a host-name error", err)
	}
	if err := dial(&tls.Config{RootCAs: roots, ServerName: "wrong.test"}); !errors.As(err, &hostErr) {
		t.Errorf("mismatched ServerName: %v; want a host-name error", err)
	}
}
