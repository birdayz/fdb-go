package otel

import (
	"errors"
	"testing"
)

func TestRelTenantPaths(t *testing.T) {
	db, schema := relTenantPaths("run_abc_01", 42)
	if db != "/OTEL/R_RUN_ABC_01" || schema != "T42" {
		t.Fatalf("paths %q %q", db, schema)
	}
}

// Every arm of the budget: under, at and over each bound, and the work a
// result reports.
func TestQueryWorkCharge(t *testing.T) {
	budgetErr := func(err error) bool {
		var b *QueryBudgetError
		return errors.As(err, &b)
	}
	w := newQueryWork(Config{MaxCandidates: 2, MaxBytes: 100})
	if err := w.charge("q", 60); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	if err := w.charge("q", 40); err != nil {
		t.Fatalf("a charge reaching both budgets exactly failed: %v", err)
	}
	if err := w.charge("q", 0); !budgetErr(err) {
		t.Fatalf("a third candidate over a budget of two: %v", err)
	}

	w = newQueryWork(Config{MaxCandidates: 10, MaxBytes: 100})
	if err := w.charge("q", 101); !budgetErr(err) {
		t.Fatalf("101 bytes over a budget of 100: %v", err)
	}
	var r QueryResult
	w.result(&r)
	if r.Candidates != 1 || r.Bytes != 101 {
		t.Fatalf("result work %d candidates, %d bytes", r.Candidates, r.Bytes)
	}
}
