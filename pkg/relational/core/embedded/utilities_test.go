package embedded

// Direct unit tests for the pure helpers in utilities.go that lack
// dedicated coverage today: stripBytesWrapper + decodeBase64.
// Per RFC-025 §"Strong unit-test coverage per package": these are
// trivially testable in isolation but only had implicit coverage
// via integration tests for bytes-literal handling.
//
// Note: TestValuesEqual + TestRowKey already exist in
// embedded_test.go and cover the other utilities. This file
// completes the coverage for the file.

import (
	"strings"
	"testing"
)

// ----- stripBytesWrapper -------------------------------------------------

// TestStripBytesWrapper_Hex pins the standard hex-literal shape:
// `x'deadbeef'` strips to `deadbeef`. Case-insensitive on the
// prefix to accept x / X.
func TestStripBytesWrapper_Hex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase x", "x'deadbeef'", "deadbeef"},
		{"uppercase X", "X'DEADBEEF'", "DEADBEEF"},
		{"empty payload", "x''", ""},
		{"hex digits only", "x'00'", "00"},
		{"missing closing quote", "x'abc", "abc"}, // documents lenient strip
		{"no quote pair", "x'abc'", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripBytesWrapper(tc.in, "x"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStripBytesWrapper_Base64 pins the b64'...' literal shape used
// by the base64-bytes-literal path. Case-insensitive prefix accepts
// b64 / B64.
func TestStripBytesWrapper_Base64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase b64", "b64'aGVsbG8='", "aGVsbG8="},
		{"uppercase B64", "B64'AGVMBG8='", "AGVMBG8="},
		{"empty payload", "b64''", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripBytesWrapper(tc.in, "b64"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStripBytesWrapper_PrefixMismatch pins the boundary where the
// prefix doesn't match: only the surrounding quotes are stripped.
// Documents the "strip what we can" lenient behaviour rather than
// asserting on a return-error contract (helper has no error path).
func TestStripBytesWrapper_PrefixMismatch(t *testing.T) {
	t.Parallel()
	got := stripBytesWrapper("y'abc'", "x")
	// Prefix doesn't match — full text minus the trailing/leading quote
	// pair is returned. Today: leading "y'" is not stripped because
	// only quote-strip runs on the whole text. Trailing "'" gets
	// stripped.
	if !strings.Contains(got, "abc") {
		t.Fatalf("expected payload to be present, got %q", got)
	}
}

// ----- decodeBase64 ------------------------------------------------------

// TestDecodeBase64_RoundTrip pins the encoder/decoder symmetry that
// the bytes-literal SQL path depends on. base64StdStrict matches
// Java's Base64.getDecoder().
func TestDecodeBase64_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []byte
	}{
		{"hello", "aGVsbG8=", []byte("hello")},
		{"empty", "", []byte{}},
		{"binary with padding", "AAECAw==", []byte{0, 1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeBase64(tc.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !bytesEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDecodeBase64_StrictRejection pins strict mode: unpadded or
// non-base64 input returns an error rather than producing best-effort
// output. SQL semantics require strict input — silent acceptance of
// malformed base64 would mask user errors.
//
// Notes on coverage scope: Go's encoding/base64 Strict() does NOT
// reject embedded newlines and accepts any length-multiple-of-4
// input as long as the characters are valid + padding is correct.
// "Strict" here means reject the URL-safe alternative + reject
// trailing-bit non-zero in padded inputs. Cases below stay within
// what's actually checked.
func TestDecodeBase64_StrictRejection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"unpadded", "aGVsbG8"},                       // missing trailing =
		{"invalid char", "!!!"},                       // non-base64 char
		{"url-safe alt rejected", "aGVsbG8-aGVsbG8="}, // - is URL-safe; strict rejects
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeBase64(tc.in); err == nil {
				t.Fatalf("expected error for invalid input %q, got nil", tc.in)
			}
		})
	}
}

// bytesEqual is a local byte-slice equality helper (not in the
// embedded package; use bytes.Equal in non-test code). Lets the
// test file avoid importing bytes for one assertion.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
