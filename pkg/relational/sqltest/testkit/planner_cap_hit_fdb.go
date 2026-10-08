package testkit

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/relational/api"
)

// assertPlannerCapHit pins the properties that distinguish an exhausted
// planning budget from every other planner failure: the cap sentinel survives
// as a cause, the SQLSTATE is the class-54 program-limit code (Go-only — Java's
// SQL layer never enables these caps, so there is no Java code to port), the
// user-visible message is an actionable "too complex" verdict rather than
// either "could not plan query" or the internal sentinel's wording, and the
// budget numbers reach the caller.
func AssertPlannerCapHit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("planning converged within the task cap; this statement no longer exercises " +
			"the cap — widen the join rather than deleting this test")
	}
	if !errors.Is(err, cascades.ErrPlannerCapHit) {
		t.Fatalf("planning failed for a different reason than the task cap: %v", err)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *api.Error, got %T (%v)", err, err)
	}
	if apiErr.Code != api.ErrCodePlanComplexityLimitReached {
		t.Fatalf("code = %q, want %q", apiErr.Code, api.ErrCodePlanComplexityLimitReached)
	}
	// The user gets an actionable verdict, not the internal sentinel's wording
	// (which names a planner config field). The sentinel stays in the cause,
	// which is what the errors.Is check above rides on.
	if !strings.Contains(apiErr.Message, "too complex to plan") {
		t.Fatalf("message = %q, want the user-facing budget verdict", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, "MaxTasks") {
		t.Fatalf("message leaks the planner config field name: %q", apiErr.Message)
	}
	if _, ok := apiErr.Context["max_task_count"]; !ok {
		t.Fatalf("budget context did not survive to the driver: %v", apiErr.Context)
	}
	if _, ok := apiErr.Context["task_count"]; !ok {
		t.Fatalf("budget context did not survive to the driver: %v", apiErr.Context)
	}
}
