package client

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io/fs"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
)

func fixtureDER(t *testing.T, name string) []byte {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(encryptedKeyFixtures[name])
	if err != nil || len(der) == 0 {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return der
}

func encryptedPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der})
}

// requireTLSError asserts err is a *TLSConfigError that reads as tls_error (2107).
func requireTLSError(t *testing.T, what string, err error) {
	t.Helper()
	var cfgErr *TLSConfigError
	var fdbErr *wire.FDBError
	if !errors.As(err, &cfgErr) || !errors.As(err, &fdbErr) || fdbErr.Code != 2107 {
		t.Fatalf("%s: %v; want a TLSConfigError with code 2107", what, err)
	}
}

// pbes2Fixtures are the fixtures the client decrypts.
var pbes2Fixtures = []string{"aes256-sha256", "aes128-sha1", "aes128-sha224", "aes128-sha384", "aes192-sha512", "des3"}

// refusedSchemes maps each refused fixture to the step that refuses it.
var refusedSchemes = map[string]string{"scrypt": "derivation", "pbes1": "encryption", "pkcs12-pbe": "encryption"}

// requireKeyScheme asserts err names the refused scheme step part.
func requireKeyScheme(t *testing.T, what string, err error, part string) {
	t.Helper()
	var scheme *keySchemeError
	if !errors.As(err, &scheme) || scheme.Part != part {
		t.Fatalf("%s: %v; want the %s refused", what, err, part)
	}
}

// OpenSSL-written PBES2 keys decrypt to the key they were made from; scrypt, PBES1,
// PKCS#12 PBE and a wrong password do not.
func TestDecryptPKCS8Fixtures(t *testing.T) {
	t.Parallel()
	want, err := base64.StdEncoding.DecodeString(fixturePublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range pbes2Fixtures {
		keyPEM, err := decryptKeyPEM(encryptedPEM(fixtureDER(t, name)), fixtureKeyPassword)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		block, _ := pem.Decode(keyPEM)
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pub, err := x509.MarshalPKIXPublicKey(key.(crypto.Signer).Public())
		if err != nil || !bytes.Equal(pub, want) {
			t.Fatalf("%s: decrypted to another key (%v)", name, err)
		}
		if _, err := decryptKeyPEM(encryptedPEM(fixtureDER(t, name)), "wrong"); !errors.Is(err, errKeyPassword) {
			t.Errorf("%s with a wrong password: %v; want %v", name, err, errKeyPassword)
		}
	}
	for name, part := range refusedSchemes {
		_, err := decryptKeyPEM(encryptedPEM(fixtureDER(t, name)), fixtureKeyPassword)
		requireKeyScheme(t, name, err, part)
	}
}

// fixtureCertPEM is a certificate for the fixture key, signed by a fresh CA.
func fixtureCertPEM(t *testing.T) []byte {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(fixturePublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	cert, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})
}

// FDB_TLS_PASSWORD decrypts the key file; an unusable key is tls_error.
func TestResolveTLSConfigKeys(t *testing.T) {
	t.Parallel()
	certFile := writeTLSTemp(t, "cert.pem", fixtureCertPEM(t))
	resolve := func(keyPEM []byte, password string, set bool) (*tls.Config, error) {
		env := map[string]string{
			"FDB_TLS_CERTIFICATE_FILE": certFile,
			"FDB_TLS_KEY_FILE":         writeTLSTemp(t, "key.pem", keyPEM),
		}
		if set {
			env["FDB_TLS_PASSWORD"] = password
		}
		return resolveTLSConfig(t.TempDir(), fakeEnv(env))
	}
	for _, name := range pbes2Fixtures {
		cfg, err := resolve(encryptedPEM(fixtureDER(t, name)), fixtureKeyPassword, true)
		if err != nil || len(cfg.Certificates) != 1 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err := resolve(encryptedPEM(fixtureDER(t, "aes256-sha256")), "wrong", true)
	requireTLSError(t, "wrong password", err)
	if !errors.Is(err, errKeyPassword) {
		t.Fatalf("wrong password: %v; want %v", err, errKeyPassword)
	}
	_, err = resolve(encryptedPEM(fixtureDER(t, "aes256-sha256")), "", false)
	requireTLSError(t, "no password", err)
	if !errors.Is(err, errKeyPassword) {
		t.Fatalf("no password: %v; want %v", err, errKeyPassword)
	}
	for name, part := range refusedSchemes {
		_, err := resolve(encryptedPEM(fixtureDER(t, name)), fixtureKeyPassword, true)
		requireTLSError(t, name, err)
		requireKeyScheme(t, name, err, part)
	}
	legacy := pem.EncodeToMemory(&pem.Block{
		Type:    "EC PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00112233445566778899AABBCCDDEEFF"},
		Bytes:   make([]byte, 128),
	})
	_, err = resolve(legacy, fixtureKeyPassword, true)
	requireTLSError(t, "legacy encrypted PEM", err)
	requireKeyScheme(t, "legacy encrypted PEM", err, "legacy PEM encryption")

	// An unencrypted key ignores the password, as OpenSSL does.
	_, certPEM, keyPEM := testCertPEM(t)
	env := fakeEnv(map[string]string{
		"FDB_TLS_CERTIFICATE_FILE": writeTLSTemp(t, "plain-cert.pem", certPEM),
		"FDB_TLS_KEY_FILE":         writeTLSTemp(t, "plain-key.pem", keyPEM),
		"FDB_TLS_PASSWORD":         "unused",
	})
	if cfg, err := resolveTLSConfig(t.TempDir(), env); err != nil || len(cfg.Certificates) != 1 {
		t.Fatalf("unencrypted key with a password set: %v", err)
	}
}

// A path variable set to "" means no file and no default, as in C++.
func TestResolveTLSConfigEmptyPathVariables(t *testing.T) {
	t.Parallel()
	caPEM, certPEM, keyPEM := testCertPEM(t)
	dir := t.TempDir()
	for name, data := range map[string][]byte{"cert.pem": certPEM, "key.pem": keyPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if cfg, err := resolveTLSConfig(dir, fakeEnv(nil)); err != nil || len(cfg.Certificates) != 1 {
		t.Fatalf("defaults: %v", err)
	}
	empty := fakeEnv(map[string]string{"FDB_TLS_CERTIFICATE_FILE": "", "FDB_TLS_KEY_FILE": "", "FDB_TLS_CA_FILE": ""})
	if cfg, err := resolveTLSConfig(dir, empty); err != nil || len(cfg.Certificates) != 0 || !cfg.RootCAs.Equal(x509.NewCertPool()) {
		t.Fatalf("empty variables: %+v, %v; want no certificate and no CA", cfg, err)
	}
	ca := writeTLSTemp(t, "ca.pem", caPEM)
	keyOnly := fakeEnv(map[string]string{"FDB_TLS_CERTIFICATE_FILE": "", "FDB_TLS_CA_FILE": ca})
	_, err := resolveTLSConfig(dir, keyOnly)
	requireTLSError(t, "key without certificate", err)
	var cfgErr *TLSConfigError
	if errors.As(err, &cfgErr); cfgErr.Setting != "FDB_TLS_CERTIFICATE_FILE/FDB_TLS_KEY_FILE" {
		t.Fatalf("an empty certificate variable with the default key: %v; want the cert/key pairing error", err)
	}
}

// Each unusable file is tls_error naming the file; a missing one unwraps to fs.ErrNotExist.
func TestResolveTLSConfigFileErrors(t *testing.T) {
	t.Parallel()
	caPEM, certPEM, keyPEM := testCertPEM(t)
	_, _, otherKeyPEM := testCertPEM(t)
	ca := writeTLSTemp(t, "ca.pem", caPEM)
	cert := writeTLSTemp(t, "cert.pem", certPEM)
	key := writeTLSTemp(t, "key.pem", keyPEM)
	missing := filepath.Join(t.TempDir(), "missing.pem")
	emptyFile := writeTLSTemp(t, "empty.pem", nil)
	otherKey := writeTLSTemp(t, "other-key.pem", otherKeyPEM)
	for _, tc := range []struct {
		name, cert, key, ca string
		setting             string
		cause               error // nil: crypto/tls's own pairing error
	}{
		{"empty CA file", cert, key, emptyFile, emptyFile, errNoCertificates},
		{"missing certificate file", missing, key, ca, missing, fs.ErrNotExist},
		{"missing key file", cert, missing, ca, missing, fs.ErrNotExist},
		{"empty certificate file", emptyFile, key, ca, emptyFile + ", " + key, nil},
		{"key of another certificate", cert, otherKey, ca, cert + ", " + otherKey, nil},
	} {
		_, err := resolveTLSConfig(t.TempDir(), fakeEnv(map[string]string{
			"FDB_TLS_CERTIFICATE_FILE": tc.cert, "FDB_TLS_KEY_FILE": tc.key, "FDB_TLS_CA_FILE": tc.ca,
		}))
		requireTLSError(t, tc.name, err)
		var cfgErr *TLSConfigError
		if errors.As(err, &cfgErr); cfgErr.Setting != tc.setting || (tc.cause != nil && !errors.Is(err, tc.cause)) {
			t.Errorf("%s: %v; want setting %q and cause %v", tc.name, err, tc.setting, tc.cause)
		}
	}
}

// testPKI is a CA and the PEM of a client certificate and key it signed.
type testPKI struct {
	ca            *x509.Certificate
	caKey         *ecdsa.PrivateKey
	caPEM         []byte
	clientCertPEM []byte
	clientKeyPEM  []byte
	clientCAs     *x509.CertPool
}

func newTestPKI(t *testing.T, name string) *testPKI {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	p := &testPKI{ca: ca, caKey: caKey, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}
	cli, cliKey := p.leaf(t, "client", x509.ExtKeyUsageClientAuth)
	p.clientCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cli.Certificate[0]})
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cliKey)
	p.clientKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	p.clientCAs = x509.NewCertPool()
	p.clientCAs.AddCert(ca)
	return p
}

// leaf issues a certificate named cn for an address the tests never dial.
func (p *testPKI) leaf(t *testing.T, cn string, usage x509.ExtKeyUsage) (tls.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("10.9.9.9")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, key
}

// The environment config completes mutual TLS with a server whose certificate names
// another host, and FDB_TLS_VERIFY_PEERS decides which servers it accepts.
func TestResolveTLSConfigMutualTLS(t *testing.T) {
	t.Parallel()
	pki := newTestPKI(t, "cluster-ca")
	server, _ := pki.leaf(t, "server-a", x509.ExtKeyUsageServerAuth)
	addr := startTLSFDBServer(t, server, pki.clientCAs)
	other := newTestPKI(t, "other-ca")
	otherServer, _ := other.leaf(t, "server-a", x509.ExtKeyUsageServerAuth)
	otherAddr := startTLSFDBServer(t, otherServer, nil)
	env := map[string]string{
		"FDB_TLS_CA_FILE":          writeTLSTemp(t, "ca.pem", pki.caPEM),
		"FDB_TLS_CERTIFICATE_FILE": writeTLSTemp(t, "cert.pem", pki.clientCertPEM),
		"FDB_TLS_KEY_FILE":         writeTLSTemp(t, "key.pem", pki.clientKeyPEM),
	}
	dial := func(addr string, verifyPeers *string) error {
		t.Helper()
		e := maps.Clone(env)
		if verifyPeers != nil {
			e["FDB_TLS_VERIFY_PEERS"] = *verifyPeers
		}
		cfg, err := resolveTLSConfig(t.TempDir(), fakeEnv(e))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := transport.Dial(ctx, addr, cfg, nil)
		if c != nil {
			c.Close()
		}
		return err
	}
	match, mismatch := "S.CN=server-a,I.CN=cluster-ca", "S.CN=server-b"
	if err := dial(addr, nil); err != nil {
		t.Fatalf("default policy: %v", err)
	}
	if err := dial(addr, &match); err != nil {
		t.Fatalf("%q: %v", match, err)
	}
	var pe *transport.PeerVerifyError
	if err := dial(addr, &mismatch); !errors.As(err, &pe) || pe.Err != nil {
		t.Fatalf("%q: %v; want a rule mismatch", mismatch, err)
	}
	var unknownCA x509.UnknownAuthorityError
	if err := dial(otherAddr, nil); !errors.As(err, &pe) || !errors.As(err, &unknownCA) {
		t.Fatalf("server of another CA: %v; want unknown authority", err)
	}
	_, err := resolveTLSConfig(t.TempDir(), fakeEnv(map[string]string{"FDB_TLS_VERIFY_PEERS": "Check.Valid=2"}))
	requireTLSError(t, "unparseable FDB_TLS_VERIFY_PEERS", err)
	var cfgErr *TLSConfigError
	var parseErr *transport.VerifyPeersError
	if errors.As(err, &cfgErr); cfgErr.Setting != "FDB_TLS_VERIFY_PEERS" || !errors.As(err, &parseErr) {
		t.Fatalf("unparseable FDB_TLS_VERIFY_PEERS: %v; want the setting and a VerifyPeersError", err)
	}
}

// rewriteScheme re-encodes the aes256-sha256 fixture with its PBKDF2 PRF or its
// cipher replaced by oid.
func rewriteScheme(t *testing.T, prf bool, oid asn1.ObjectIdentifier) []byte {
	t.Helper()
	var info encryptedPrivateKeyInfo
	if _, err := asn1.Unmarshal(fixtureDER(t, "aes256-sha256"), &info); err != nil {
		t.Fatal(err)
	}
	var params pbes2Params
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil {
		t.Fatal(err)
	}
	if prf {
		var kdf pbkdf2Params
		if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
			t.Fatal(err)
		}
		kdf.PRF.Algorithm = oid
		b, err := asn1.Marshal(kdf)
		if err != nil {
			t.Fatal(err)
		}
		params.KeyDerivationFunc.Parameters = asn1.RawValue{FullBytes: b}
	} else {
		params.EncryptionScheme.Algorithm = oid
	}
	b, err := asn1.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	info.Algorithm.Parameters = asn1.RawValue{FullBytes: b}
	der, err := asn1.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// A PBES2 key with a PRF or cipher outside the supported set is refused by name.
func TestDecryptPKCS8UnsupportedPRFAndCipher(t *testing.T) {
	t.Parallel()
	hmacMD5 := asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 6}
	rc2CBC := asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 2}
	_, err := decryptKeyPEM(encryptedPEM(rewriteScheme(t, true, hmacMD5)), fixtureKeyPassword)
	requireKeyScheme(t, "HMAC-MD5 PRF", err, "PBKDF2 PRF")
	_, err = decryptKeyPEM(encryptedPEM(rewriteScheme(t, false, rc2CBC)), fixtureKeyPassword)
	requireKeyScheme(t, "RC2-CBC cipher", err, "cipher")
}
