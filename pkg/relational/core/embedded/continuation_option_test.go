package embedded

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestOptContinuation_RejectsLoudly pins the RFC-181 C2 decision: Go SQL
// statement tokens are ENGINE-PRIVATE and no resume entry point exists,
// so a caller-supplied CONTINUATION option must fail the statement
// loudly — never be silently ignored (which would re-run from row 1
// while the caller believes they resumed: duplicate rows both within Go
// and for any Java-minted token handed across engines).
func TestOptContinuation_RejectsLoudly(t *testing.T) {
	t.Parallel()
	conn := &EmbeddedConnection{}
	conn.SetOptions(api.NewOptionsBuilder().
		Set(api.OptContinuation, []byte{0x01}).
		Build())
	p := &cascadesPlan{conn: conn}
	_, err := p.Execute(context.Background())
	if err == nil {
		t.Fatal("a CONTINUATION option must reject loudly, not be silently ignored")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedOperation {
		t.Fatalf("want ErrCodeUnsupportedOperation, got %v", err)
	}
	if !strings.Contains(err.Error(), "engine-private") {
		t.Fatalf("error must state the engine-private decision, got %q", err.Error())
	}
}
