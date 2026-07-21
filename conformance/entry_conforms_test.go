//go:build bazelrunfiles

package conformance_test

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

// TestEntryConforms_JavaErrorClassification pins the harness's Java-error
// classification arms. The dimensional gap that let a CI transport hiccup
// read as an engine regression: ANY Java-side error used to classify as
// "Java errored but Go succeeded" with NO error text — an HTTP timeout to
// the conformance server (a plain wrapped error, not *plandiff.JavaError)
// was indistinguishable from a genuine UnableToPlanException, and the CI
// log carried nothing to tell them apart. Now: (a) a non-JavaError is
// reported as INFRA, never as a divergence; (b) a genuine JavaError's
// class+message ride in the detail so the response (annotate vs fix vs
// infra) is decidable from the log alone.
func TestEntryConforms_JavaErrorClassification(t *testing.T) {
	t.Parallel()

	okRows := plandiff.RunResult{Engine: "go"}

	t.Run("transport error is INFRA, never a divergence", func(t *testing.T) {
		t.Parallel()
		java := plandiff.RunResult{Engine: "java", Err: fmt.Errorf("plandiff: HTTP POST: context deadline exceeded")}
		ok, detail := entryConforms(java, okRows)
		if ok {
			t.Fatal("an infra failure must stay RED (a sick server must not silently pass)")
		}
		if !strings.Contains(detail, "INFRA") {
			t.Fatalf("detail must name the failure as infra, got: %s", detail)
		}
		if strings.Contains(detail, "Java errored but Go succeeded") {
			t.Fatalf("an infra failure must never classify as a cross-engine divergence, got: %s", detail)
		}
	})

	t.Run("genuine Java error carries class and message", func(t *testing.T) {
		t.Parallel()
		java := plandiff.RunResult{Engine: "java", Err: &plandiff.JavaError{
			Message:        "Cascades planner could not plan query",
			ExceptionClass: "UnableToPlanException",
		}}
		ok, detail := entryConforms(java, okRows)
		if ok {
			t.Fatal("Java-errored/Go-succeeded must stay a divergence")
		}
		if !strings.Contains(detail, "Java errored but Go succeeded") {
			t.Fatalf("genuine Java error must classify as the divergence, got: %s", detail)
		}
		if !strings.Contains(detail, "UnableToPlanException") || !strings.Contains(detail, "could not plan") {
			t.Fatalf("detail must carry the Java exception class + message, got: %s", detail)
		}
	})
}
