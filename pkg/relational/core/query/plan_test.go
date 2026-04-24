package query

import (
	"context"
	"testing"
)

func TestPlanFunc_Explain(t *testing.T) {
	t.Parallel()

	// Zero value: nil ExplainFn returns "".
	p := &PlanFunc{}
	if got := p.Explain(); got != "" {
		t.Fatalf("nil ExplainFn: want empty, got %q", got)
	}

	// Set ExplainFn: delegates.
	p = &PlanFunc{ExplainFn: func() string { return "select *" }}
	if got := p.Explain(); got != "select *" {
		t.Fatalf("ExplainFn: got %q", got)
	}
}

func TestMultiPlan_Explain(t *testing.T) {
	t.Parallel()

	ok := func(text string) *PlanFunc {
		return &PlanFunc{
			ExecFn:    func(_ context.Context) (Result, error) { return Result{}, nil },
			UpdateFn:  func() bool { return true },
			ExplainFn: func() string { return text },
		}
	}

	// Empty: empty string.
	if got := (&MultiPlan{}).Explain(); got != "" {
		t.Fatalf("empty MultiPlan.Explain: got %q", got)
	}

	// Single child: that child's explanation.
	m := &MultiPlan{Plans: []Plan{ok("A")}}
	if got := m.Explain(); got != "A" {
		t.Fatalf("single child: got %q, want \"A\"", got)
	}

	// Multiple: joined by ';\n'.
	m = &MultiPlan{Plans: []Plan{ok("A"), ok("B"), ok("C")}}
	if got := m.Explain(); got != "A;\nB;\nC" {
		t.Fatalf("multiple children: got %q", got)
	}
}

// Static assertion: *PlanFunc satisfies Plan (exercises the new
// Explain method on the interface).
var (
	_ Plan = (*PlanFunc)(nil)
	_ Plan = (*MultiPlan)(nil)
)
