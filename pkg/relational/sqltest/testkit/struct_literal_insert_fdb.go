package testkit

import (
	"strings"
	"testing"
)

// requireErrContains asserts the statement failed with a message carrying
// want — used where the Java-verbatim WORDING is the contract.
func RequireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, statement succeeded", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}
