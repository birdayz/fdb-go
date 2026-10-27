package otel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
)

func TestCHTransportExecutionIdentityAndAddress(t *testing.T) {
	t.Parallel()
	base := testConfig()
	if base.CHQueryTransport != "http" || base.CHNativeAddr != "" {
		t.Fatalf("HTTP transport default changed: %+v", base)
	}
	for _, address := range []string{"127.0.0.1:9000", "127.9.8.7:1", "localhost:65535", "[::1]:9000", "[::ffff:127.0.0.1]:9000"} {
		for _, transport := range []string{"http", "native"} {
			c := base
			c.CHQueryTransport, c.CHNativeAddr = transport, address
			if err := c.Validate(); err != nil {
				t.Fatalf("valid %s/%s: %v", transport, address, err)
			}
			if corpusIdentity(c) != corpusIdentity(base) || checkManifest(c, manifest(base, "ready"), true) != nil {
				t.Fatal("query transport changed corpus or owned layout identity")
			}
		}
	}
	if got, err := nativeAddress("localhost:09000"); err != nil || got != "127.0.0.1:9000" {
		t.Fatalf("localhost must not require DNS: %q %v", got, err)
	}
	for _, transport := range []string{"", "other", "HTTP"} {
		c := base
		c.CHQueryTransport = transport
		var config *ConfigError
		if err := c.Validate(); !errors.As(err, &config) {
			t.Fatalf("invalid transport accepted: %q %v", transport, err)
		}
	}
	for _, address := range []string{"", ":9000", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:port", "0.0.0.0:9000", "192.0.2.1:9000", "example.invalid:9000", "[::1%lo]:9000", "http://127.0.0.1:9000", "127.0.0.1:9000/path", "user:do-not-echo@127.0.0.1:9000"} {
		c := base
		c.CHQueryTransport, c.CHNativeAddr = "native", address
		var config *ConfigError
		err := c.Validate()
		if !errors.As(err, &config) || strings.Contains(err.Error(), "do-not-echo") {
			t.Fatalf("invalid native address accepted or echoed: %v", err)
		}
	}
}

func TestCHNativeOpenIsLazyAndAdminLoopbackOnly(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.CHQueryTransport, c.CHNativeAddr = "native", "127.0.0.1:1"
	b, err := OpenCHContext(t.Context(), c, "http://127.0.0.1:1", false)
	if err != nil {
		t.Fatalf("constructing native pool attempted a hidden ping/query: %v", err)
	}
	b.Close()
	var config *ConfigError
	if _, err := OpenCHContext(t.Context(), c, "http://192.0.2.1:8123", false); !errors.As(err, &config) {
		t.Fatalf("native transport accepted remote admin endpoint: %v", err)
	}
	if _, err := OpenCHContext(t.Context(), c, "http://operator:do-not-echo%zz@127.0.0.1:8123", false); !errors.As(err, &config) || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("malformed URL leaked userinfo: %v", err)
	}
}

func assertCHSafeError(t *testing.T, err error, forbidden []string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected diagnostic error")
	}
	for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				t.Fatalf("credential escaped redaction (%q): %s", token, text)
			}
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if child != nil {
				assertCHSafeError(t, child, forbidden)
			}
		}
	case interface{ Unwrap() error }:
		if child := wrapped.Unwrap(); child != nil {
			assertCHSafeError(t, child, forbidden)
		}
	}
}

func TestCHCredentialRedactionPreservesSafeCauses(t *testing.T) {
	t.Parallel()
	endpoint, err := url.Parse("http://operator%40example:do%2Fnot%3Aecho@127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	user, password := endpoint.User.Username(), "do/not:echo"
	forbidden := []string{user, password, endpoint.User.String(), "operator%40example", "do%2Fnot%3Aecho", "do%2fnot%3aecho", "do%2fnot%3Aecho"}
	cause := errors.New("transport sentinel")
	address := "http://operator%40example:do%2fnot%3aecho@127.0.0.1:9000/query"
	unsafe := fmt.Errorf("outer %s / operator%%40example:do%%2fnot%%3Aecho: %w", endpoint.User.String(), &url.Error{Op: "query", URL: address, Err: fmt.Errorf("auth %s: %w", password, cause)})
	clean := redactCHError(unsafe, endpoint)
	assertCHSafeError(t, clean, forbidden)
	if !errors.Is(clean, cause) {
		t.Fatal("redaction lost sentinel cause")
	}
	var request *url.Error
	if !errors.As(clean, &request) {
		t.Fatal("redaction lost url.Error type")
	}
	parsed, err := url.Parse(request.URL)
	if err != nil || parsed.User != nil {
		t.Fatalf("wrapped url.Error retained userinfo: %v %v", request, err)
	}
	for _, kind := range []string{"config", "ownership", "admission", "budget", "corruption", "http", "audit", "oracle", "operation"} {
		wrapped := typedError(kind, user, password, unsafe)
		clean := redactCHError(wrapped, endpoint)
		assertCHSafeError(t, clean, forbidden)
		var typed *OtelError
		if !errors.As(clean, &typed) || typed.Kind != kind || !errors.Is(clean, cause) {
			t.Fatalf("redaction lost typed %s/cause: %v", kind, clean)
		}
	}
	multi := redactCHError(errors.Join(unsafe, fmt.Errorf("second %s: %w", user, context.Canceled)), endpoint)
	assertCHSafeError(t, multi, forbidden)
	if !errors.Is(multi, cause) || !errors.Is(multi, context.Canceled) {
		t.Fatal("redaction lost joined causes")
	}
	if redactCHError(nil, endpoint) != nil {
		t.Fatal("nil error became a diagnostic")
	}
}

func TestCHCredentialRedactionInvalidUTF8(t *testing.T) {
	t.Parallel()
	user := string([]byte{0xff})
	endpoint := &url.URL{Scheme: "http", Host: "127.0.0.1:8123", User: url.UserPassword(user, "do-not-echo")}
	cause := errors.New("invalid byte " + user + " and do-not-echo")
	clean := redactCHError(cause, endpoint)
	assertCHSafeError(t, clean, []string{user, "do-not-echo", "%FF"})
	if !errors.Is(clean, cause) {
		t.Fatal("invalid UTF8 redaction lost cause identity")
	}
}

func TestCHNativeExceptionRedactsNestedDiagnostics(t *testing.T) {
	t.Parallel()
	endpoint, err := url.Parse("http://operator:do-not-echo@127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	unsafe := &clickhouse.Exception{Code: 241, Name: "operator", Message: "do-not-echo", StackTrace: endpoint.String(), Nested: []clickhouse.Exception{{Code: 242, Name: "operator", Message: "do-not-echo", StackTrace: endpoint.String()}}}
	clean := redactCHError(unsafe, endpoint)
	assertCHSafeError(t, clean, []string{"operator", "do-not-echo"})
	var exception *clickhouse.Exception
	if !errors.As(clean, &exception) || exception.Code != 241 || len(exception.Nested) != 1 || exception.Nested[0].Code != 242 {
		t.Fatalf("native exception lost code/type: %v", clean)
	}
	for _, e := range append([]clickhouse.Exception{*exception}, exception.Nested...) {
		for _, text := range []string{e.Name, e.Message, e.StackTrace} {
			if strings.Contains(text, "operator") || strings.Contains(text, "do-not-echo") {
				t.Fatalf("nested native exception leaked auth: %+v", e)
			}
		}
	}
}
