package main

import (
	"strings"
	"testing"
)

func TestPlanTraceReportsThePlanAndItsCost(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	err := run([]string{
		"-top", "3",
		"-ddl", "CREATE TABLE t (id BIGINT, a BIGINT, b BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t (a) CREATE INDEX idx_b ON t (b)",
		"-sql", "SELECT * FROM t WHERE a = 1 OR b = 2",
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{"plan (", "UnorderedUnion", "planner trace: ", "memo members by type:", "equivalent group classes"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
	if rows := strings.Count(report, "PLANNING ") + strings.Count(report, "REWRITING "); rows != 3 {
		t.Fatalf("-top 3 printed %d attribution rows:\n%s", rows, report)
	}
}

func TestPlanTraceRejectsMissingInputsAndPlannerErrors(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if err := run([]string{"-sql", "SELECT 1"}, &out); err == nil {
		t.Fatal("missing -ddl accepted")
	}
	out.Reset()
	err := run([]string{"-ddl", "CREATE TABLE t (id BIGINT, PRIMARY KEY (id))", "-sql", "SELECT * FROM missing"}, &out)
	if err == nil || !strings.Contains(out.String(), "planning failed after") {
		t.Fatalf("unknown table: err=%v report=%q", err, out.String())
	}
}
