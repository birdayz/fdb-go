package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// mustRebaseValue is the test spelling of a rebase EXPECTED to succeed; the
// error-less values.RebaseValue wrapper is gone (see values/rebase.go).
func mustRebaseValue(t testing.TB, v values.Value, aliases values.AliasMap) values.Value {
	t.Helper()
	rebased, err := values.RebaseValueChecked(v, aliases)
	if err != nil {
		t.Fatalf("RebaseValueChecked: %v", err)
	}
	return rebased
}
