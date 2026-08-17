package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// mustRebaseValue is the test spelling of a rebase EXPECTED to succeed.
//
// values.RebaseValue — the error-less wrapper that returned nil on failure — is
// gone (see values/rebase.go for why). A test that wants the FAILURE calls
// values.RebaseValueChecked and asserts on the error; a test that just needs a
// rebased tree wants this, which turns a failure into a loud test failure
// instead of a nil that flows onward looking like a legitimate absence.
func mustRebaseValue(t testing.TB, v values.Value, aliases values.AliasMap) values.Value {
	t.Helper()
	rebased, err := values.RebaseValueChecked(v, aliases)
	if err != nil {
		t.Fatalf("RebaseValueChecked: %v", err)
	}
	return rebased
}
