package yamsql_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// The scenario name is the only string that ties a corpus failure back to
// the scenario that produced it. Go writes a parallel subtest's messages
// where they happen but buffers the "--- FAIL: <subtest>" result lines and
// flushes them in one block after every subtest finishes, so the failure
// text and the only marker naming it end up ~1000 lines apart in the log.
// A CI failure was once reported as a message-less FAIL because the
// message was there but `grep <scenario-name>` could not find it: the
// pass line carried the name and the failure lines did not.
//
// These tests pin the property, not the wording.

const reportScenarioName = "aggregate_distinct_count"

// TestScenarioReportNamesTheScenario walks every error field of
// scenarioOutcome reflectively and requires each one, in isolation, to
// produce a failure whose every line names the scenario. Reflection is the
// point: a failure path added later shows up as a new error field, and an
// unrendered field fails here rather than silently shipping an unfindable
// message.
func TestScenarioReportNamesTheScenario(t *testing.T) {
	t.Parallel()

	errType := reflect.TypeOf((*error)(nil)).Elem()
	outType := reflect.TypeOf(scenarioOutcome{})

	var errFields []string
	for i := range outType.NumField() {
		if f := outType.Field(i); f.Type == errType {
			errFields = append(errFields, f.Name)
		}
	}
	if len(errFields) == 0 {
		t.Fatal("scenarioOutcome exposes no error fields — the reflective sweep would " +
			"pass vacuously; it must enumerate every failure path")
	}

	for _, field := range errFields {
		t.Run(field, func(t *testing.T) {
			t.Parallel()

			sentinel := errors.New("sentinel-" + field)
			out := reflect.New(outType).Elem()
			out.FieldByName("Path").SetString("testdata/" + reportScenarioName + ".yaml")
			out.FieldByName(field).Set(reflect.ValueOf(sentinel))

			lines, failed := scenarioReport(reportScenarioName, out.Interface().(scenarioOutcome))
			if !failed {
				t.Fatalf("%s set but scenarioReport reported success — the failure path is unrendered", field)
			}
			if len(lines) == 0 {
				t.Fatalf("%s produced no output at all", field)
			}
			assertAllLinesNamed(t, lines)
			if !strings.Contains(strings.Join(lines, "\n"), sentinel.Error()) {
				t.Errorf("%s: underlying error %q dropped from the report:\n%s",
					field, sentinel, strings.Join(lines, "\n"))
			}
		})
	}
}

// TestScenarioReportNamesAssertionFailures covers the one failure path that
// is not an error field: individual test-case mismatches, which are the
// most common corpus failure and the ones a reader greps for by name.
func TestScenarioReportNamesAssertionFailures(t *testing.T) {
	t.Parallel()

	lines, failed := scenarioReport(reportScenarioName, scenarioOutcome{
		TestsRun:  2,
		TestsPass: 0,
		TestsFail: 2,
		Failures: []yamsql.Failure{
			{Index: 0, Query: "SELECT COUNT(DISTINCT category) FROM t", Message: "expected error 0AF00, got nil"},
			{Index: 1, Query: "SELECT category FROM t", Message: "row 0 mismatch"},
		},
	})
	if !failed {
		t.Fatal("TestsFail > 0 reported as success")
	}
	// One line per mismatch plus the summary; every one of them has to be
	// findable, not just the summary.
	if len(lines) != 3 {
		t.Fatalf("expected 2 mismatch lines + 1 summary, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	assertAllLinesNamed(t, lines)
}

// TestScenarioReportPassLineNamesTheScenario pins the passing format too.
// It is what the failure lines were made to match, and the corpus log is
// read by grepping it.
func TestScenarioReportPassLineNamesTheScenario(t *testing.T) {
	t.Parallel()

	lines, failed := scenarioReport(reportScenarioName, scenarioOutcome{TestsRun: 2, TestsPass: 2})
	if failed {
		t.Fatal("clean outcome reported as failure")
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly one pass line, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	assertAllLinesNamed(t, lines)
	if !strings.Contains(lines[0], "2/2 passed") {
		t.Errorf("pass line lost its counts: %q", lines[0])
	}
}

// TestScenarioReportDeadlineNamesItsBudget pins the second half of the
// misdiagnosis: a bare "context deadline exceeded" names neither the
// budget that expired nor how long it took, so it reads as a hung query
// rather than a starved machine.
func TestScenarioReportDeadlineNamesItsBudget(t *testing.T) {
	t.Parallel()

	lines, failed := scenarioReport(reportScenarioName, scenarioOutcome{
		SetupErr: errors.New("CREATE DATABASE: context deadline exceeded"),
		Elapsed:  140 * time.Second,
		Expired:  true,
	})
	if !failed {
		t.Fatal("expired setup reported as success")
	}
	assertAllLinesNamed(t, lines)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, scenarioBudget.String()) {
		t.Errorf("deadline failure does not name the %s budget it exhausted:\n%s", scenarioBudget, joined)
	}
	if !strings.Contains(joined, "2m20s") {
		t.Errorf("deadline failure does not say how long it took:\n%s", joined)
	}

	// The note is for expired scenarios only — attaching it to every
	// failure would make it noise and teach readers to skip it.
	clean, _ := scenarioReport(reportScenarioName, scenarioOutcome{
		SetupErr: errors.New("CREATE DATABASE: syntax error"),
	})
	if strings.Contains(strings.Join(clean, "\n"), "per-scenario budget") {
		t.Errorf("budget note attached to a failure that did not time out:\n%s", strings.Join(clean, "\n"))
	}
}

func assertAllLinesNamed(t *testing.T, lines []string) {
	t.Helper()
	want := "scenario " + reportScenarioName
	for i, line := range lines {
		if !strings.Contains(line, want) {
			t.Errorf("line %d is unfindable by `grep %s` — it must carry the scenario name:\n\t%s",
				i, reportScenarioName, line)
		}
	}
}
