package yamsql

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestExactAssertionValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ body, want string }{
		{"query: SELECT 1\n    rows: []\n    exact_rows: []", "mutually exclusive"},
		{"query: SELECT 1\n    exact_rows: []\n    error_code: '22000'", "incompatible"},
		{"query: SELECT 1\n    column_types: [DOUBLE]\n    error_code: '22000'", "incompatible"},
		{"exec: DELETE FROM t\n    exact_rows: []", "only valid on queries"},
		{"exec: DELETE FROM t\n    rows: []", "exec cannot expect rows"},
		{"query: DELETE FROM t\n    rows: []", "only valid on queries"},
		{"exec: DELETE FROM t\n    column_types: []", "must be non-empty"},
		{"query: SELECT 1\n    error_message: oops", "requires an error code"},
		{"query: SELECT 1\n    error_code: '22000'\n    error: message", "mutually exclusive"},
	} {
		t.Run(tc.body, func(t *testing.T) { t.Parallel(); loadExpectingError(t, "  - "+tc.body+"\n", tc.want) })
	}
	s, err := Load(writeTempYaml(t, strictTestHeader+"  - query: SELECT 1 WHERE FALSE\n    exact_rows: []\n"))
	if err != nil || s.Tests[0].ExactRows == nil {
		t.Fatalf("explicit empty exact set lost: %v", err)
	}
	for _, statement := range []Test{{Exec: "DELETE FROM t", Rows: [][]any{}}, {Query: "DELETE FROM t", Rows: [][]any{}}} {
		direct := &Scenario{SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY(id))", Tests: []Test{statement}}
		if _, err := Run(context.Background(), direct, RunConfig{}); err == nil || (!strings.Contains(err.Error(), "rows") && !strings.Contains(err.Error(), "result assertions")) {
			t.Fatalf("Run ignored empty DML rows: %v", err)
		}
	}
	empty := [][]Scalar{}
	invalid := &Scenario{SchemaTemplate: "CREATE TABLE t (id BIGINT, PRIMARY KEY(id))", Tests: []Test{{Query: "SELECT 1", Rows: [][]any{}, ExactRows: &empty}}}
	if _, err := Run(context.Background(), invalid, RunConfig{}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Run bypassed validation: %v", err)
	}
}

func TestLiveOutcomeEvidence(t *testing.T) {
	t.Parallel()
	s := &Scenario{Name: "live", Tests: []Test{{Query: "SELECT 1", Rows: [][]any{{math.Inf(1)}}}}}
	digest, err := digestScenario(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		r    *Result
		pass bool
	}{
		{"successful", &Result{scenarioDigest: digest, outcomes: []bool{true}}, true},
		{"failed outcome", &Result{scenarioDigest: digest, outcomes: []bool{false}}, true},
		{"short outcomes", &Result{scenarioDigest: digest}, false},
		{"wrong digest", &Result{outcomes: []bool{true}}, false},
		{"aggregate only", &Result{TestsRun: 1, TestsPass: 1}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.r.successfulStatements(s)
			if (err == nil) != tc.pass {
				t.Fatalf("outcomes=%v err=%v", got, err)
			}
			if err == nil {
				if got[0] != tc.r.outcomes[0] {
					t.Fatal("changed recorded outcome")
				}
				got[0] = !got[0]
				if got[0] == tc.r.outcomes[0] {
					t.Fatal("exposed mutable outcomes")
				}
			}
		})
	}
	changed := *s
	changed.Setup = []string{"INSERT INTO t VALUES (1)"}
	r := &Result{scenarioDigest: digest, outcomes: []bool{true}}
	if _, err := r.successfulStatements(&changed); err == nil {
		t.Fatal("stale success after setup change")
	}
}
