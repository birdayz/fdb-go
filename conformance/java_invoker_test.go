//go:build bazelrunfiles

package conformance_test

import (
	"net/http"
	"testing"

	"github.com/onsi/ginkgo/v2"
)

func init() {
	isolatedSpecFailed = func() bool {
		return ginkgo.CurrentSpecReport().Failed()
	}
}

// TestJavaHTTPClient_DisablesKeepAlives pins the flake fix: the client that
// talks to the pooled Java server must NOT reuse keep-alive connections, or a
// stale server-closed socket surfaces as a spurious `POST … : EOF` under
// full-suite load. Pure config assertion — no server needed.
func TestJavaHTTPClient_DisablesKeepAlives(t *testing.T) {
	t.Parallel()
	c := newJavaHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("java HTTP client Transport = %T, want *http.Transport carrying the keep-alive setting", c.Transport)
	}
	if !tr.DisableKeepAlives {
		t.Fatal("java HTTP client must set DisableKeepAlives=true — reusing a pooled connection the " +
			"server has since closed causes the intermittent `POST /invoke: EOF` conformance flake")
	}
}

// A client-wide timeout would cut a step short of its caller's deadline: the
// target's planner overran a fixed two minutes on CI inside a spec allowed
// twenty. Invoke bounds a step by the caller's deadline, or a default.
func TestJavaHTTPClient_LeavesTheDeadlineToTheCaller(t *testing.T) {
	t.Parallel()
	if c := newJavaHTTPClient(); c.Timeout != 0 {
		t.Fatalf("java HTTP client Timeout = %v, want none", c.Timeout)
	}
}
