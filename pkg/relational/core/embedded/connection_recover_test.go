package embedded

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// TestRecoveredPanicError pins the database/sql boundary-recover contract (P0.2):
// a panic that escapes planning/execution becomes a generic internal error — never
// a process crash — and the panic value (which may carry schema/row data) is logged
// SERIOUS server-side but NOT leaked to the caller. See TODO-production.md P0.3.
func TestRecoveredPanicError(t *testing.T) {
	t.Parallel()
	var logged string
	orig := seriousLog
	seriousLog = func(msg string, attrs ...any) { logged += fmt.Sprintf("%s %v", msg, attrs) }
	t.Cleanup(func() { seriousLog = orig })

	err := recoveredPanicError("sensitive: secret_table.ssn")

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if apiErr.Code != api.ErrCodeInternalError {
		t.Errorf("code = %q, want %q", apiErr.Code, api.ErrCodeInternalError)
	}
	if strings.Contains(err.Error(), "secret_table") {
		t.Errorf("caller-visible error must not leak the panic value: %q", err.Error())
	}
	if !strings.Contains(logged, "secret_table") {
		t.Errorf("the panic value must be logged server-side (SERIOUS); got %q", logged)
	}
}
