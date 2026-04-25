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

// TestPlanFunc_Execute pins delegation to the wrapped closure.
func TestPlanFunc_Execute(t *testing.T) {
	t.Parallel()
	want := Result{RowsAffected: 7}
	p := &PlanFunc{ExecFn: func(ctx context.Context) (Result, error) {
		return want, nil
	}}
	got, err := p.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: unexpected err %v", err)
	}
	if got != want {
		t.Fatalf("Execute: got %v, want %v", got, want)
	}
}

// TestPlanFunc_IsUpdate pins the nil-UpdateFn fallback (false) plus
// the wrapped-closure delegation in both directions.
func TestPlanFunc_IsUpdate(t *testing.T) {
	t.Parallel()
	p := &PlanFunc{}
	if p.IsUpdate() {
		t.Fatal("nil UpdateFn should return false")
	}
	p = &PlanFunc{UpdateFn: func() bool { return true }}
	if !p.IsUpdate() {
		t.Fatal("UpdateFn returning true should propagate")
	}
	p = &PlanFunc{UpdateFn: func() bool { return false }}
	if p.IsUpdate() {
		t.Fatal("UpdateFn returning false should propagate")
	}
}

// TestMultiPlan_Execute_AggregatesRowsAffected pins that Execute
// sums RowsAffected across the batch and short-circuits on error.
func TestMultiPlan_Execute_AggregatesRowsAffected(t *testing.T) {
	t.Parallel()
	mk := func(n int64) *PlanFunc {
		return &PlanFunc{ExecFn: func(_ context.Context) (Result, error) {
			return Result{RowsAffected: n}, nil
		}}
	}
	m := &MultiPlan{Plans: []Plan{mk(1), mk(2), mk(3)}}
	got, err := m.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: unexpected err %v", err)
	}
	if got.RowsAffected != 6 {
		t.Fatalf("Execute: total %d, want 6", got.RowsAffected)
	}
	if got.Rows != nil {
		t.Fatalf("Execute: Rows should be nil for multi-Exec, got %v", got.Rows)
	}
}

// TestMultiPlan_Execute_ShortCircuitsOnError pins that the first
// child's error propagates and subsequent children are NOT executed.
func TestMultiPlan_Execute_ShortCircuitsOnError(t *testing.T) {
	t.Parallel()
	called := 0
	wantErr := context.DeadlineExceeded
	first := &PlanFunc{ExecFn: func(_ context.Context) (Result, error) {
		called++
		return Result{}, wantErr
	}}
	second := &PlanFunc{ExecFn: func(_ context.Context) (Result, error) {
		called++
		return Result{RowsAffected: 99}, nil
	}}
	m := &MultiPlan{Plans: []Plan{first, second}}
	_, err := m.Execute(context.Background())
	if err != wantErr {
		t.Fatalf("err: got %v, want %v", err, wantErr)
	}
	if called != 1 {
		t.Fatalf("expected only first child to run (called=%d)", called)
	}
}

// TestMultiPlan_IsUpdate_AllUpdate pins that an all-update batch
// reports IsUpdate=true.
func TestMultiPlan_IsUpdate_AllUpdate(t *testing.T) {
	t.Parallel()
	mk := func(b bool) *PlanFunc {
		return &PlanFunc{UpdateFn: func() bool { return b }}
	}
	m := &MultiPlan{Plans: []Plan{mk(true), mk(true), mk(true)}}
	if !m.IsUpdate() {
		t.Fatal("all-update batch: expected IsUpdate=true")
	}
}

// TestMultiPlan_IsUpdate_MixedIsNotUpdate pins that a mixed batch
// (any non-update child) reports IsUpdate=false. Used by the driver
// to decide between driver.Rows vs driver.Result.
func TestMultiPlan_IsUpdate_MixedIsNotUpdate(t *testing.T) {
	t.Parallel()
	mk := func(b bool) *PlanFunc {
		return &PlanFunc{UpdateFn: func() bool { return b }}
	}
	m := &MultiPlan{Plans: []Plan{mk(true), mk(false), mk(true)}}
	if m.IsUpdate() {
		t.Fatal("mixed batch: expected IsUpdate=false")
	}
}

// TestMultiPlan_IsUpdate_Empty pins that an empty batch is
// vacuously all-update (matches Java's batch with no statements).
func TestMultiPlan_IsUpdate_Empty(t *testing.T) {
	t.Parallel()
	if !(&MultiPlan{}).IsUpdate() {
		t.Fatal("empty MultiPlan: expected IsUpdate=true (vacuously)")
	}
}

// Static assertion: *PlanFunc satisfies Plan (exercises the new
// Explain method on the interface).
var (
	_ Plan = (*PlanFunc)(nil)
	_ Plan = (*MultiPlan)(nil)
)
