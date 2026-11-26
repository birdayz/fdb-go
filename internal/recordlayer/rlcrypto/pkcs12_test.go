package rlcrypto

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // the PKCS #12 KDF vectors are SHA-1
	"testing"
)

// TestPKCS12KDF pins RFC 7292 appendix B.2 with the vectors
// golang.org/x/crypto/pkcs12 checks (pbkdf_test.go): one long key, and one
// whose intermediate I_j gains a leading zero byte, the case a big-integer
// implementation of step 6C gets wrong.
func TestPKCS12KDF(t *testing.T) {
	t.Parallel()
	got := PKCS12KDF(sha1.New, 1, BMPPassword("sesame"), bytes.Repeat([]byte{0xff}, 8), 2048, 24)
	want := []byte("\x7c\xd9\xfd\x3e\x2b\x3b\xe7\x69\x1a\x44\xe3\xbe\xf0\xf9\xea\x0f\xb9\xb8\x97\xd4\xe3\x25\xd9\xd1")
	if !bytes.Equal(got, want) {
		t.Errorf("long key: got %x, want %x", got, want)
	}
	got = PKCS12KDF(sha1.New, 1, []byte{0, 0}, []byte("\xf3\x7e\x05\xb5\x18\x32\x4b\x4b"), 2048, 24)
	want = []byte("\x00\xf7\x59\xff\x47\xd1\x4d\xd0\x36\x65\xd5\x94\x3c\xb3\xc4\xa3\x9a\x25\x55\xc0\x2a\xed\x66\xe1")
	if !bytes.Equal(got, want) {
		t.Errorf("leading zero: got %x, want %x", got, want)
	}
}

// TestBMPPassword pins the JDK's PKCS12KeyStore.derive encoding: UTF-16BE
// with a two-byte terminator, the empty password being the terminator alone,
// and only "\x00" empty. The JVM spec's empty-password key store is the
// end-to-end check of the middle case against the JDK.
func TestBMPPassword(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want []byte
	}{
		{"", []byte{0, 0}},
		{"\x00", nil},
		{"ab", []byte{0, 'a', 0, 'b', 0, 0}},
		{"é𝄞", []byte{0x00, 0xe9, 0xd8, 0x34, 0xdd, 0x1e, 0, 0}},
	} {
		if got := BMPPassword(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("BMPPassword(%q) = %x, want %x", c.in, got, c.want)
		}
	}
}
