//go:build bazelrunfiles

package conformance_test

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

// TestDivergenceHolds pins divergenceHolds — the RFC-082 regression-lock
// predicate that decides whether an annotated corpus entry's cross-engine
// divergence still holds exactly as documented. It is a pure function (no FDB),
// so this is a plain unit test that runs alongside the Ginkgo suite.
//
// The headline case: a
// DivergenceJavaIntermittentGoCorrect entry documents that BOTH engines
// succeed and only Java's ROW ORDER is intermittent — so if Java starts
// DETERMINISTICALLY ERRORING (a new, worse divergence) the lock must FAIL, not
// pass merely because Go's rows still match. Every direction also asserts BOTH
// the Java premise and Go's pinned behaviour (the whole point of the
// assert-both-sides rewrite), so a divergence can never silently launder a Go
// bug or a stale Java premise.
func TestDivergenceHolds(t *testing.T) {
	t.Parallel()

	rows := func(rs [][]any) plandiff.RunResult {
		return plandiff.RunResult{Engine: "x", Rows: plandiff.RowSet{Rows: rs}}
	}
	fail := func(msg string) plandiff.RunResult {
		return plandiff.RunResult{Engine: "x", Err: errors.New(msg)}
	}
	// javaFail models Java's ENGINE rejecting the query — a *plandiff.JavaError,
	// which is what the HTTP client actually produces for a server-side
	// exception. The distinction is load-bearing: divergenceHolds treats a
	// non-JavaError as an infra failure (the call never reached the engine), so
	// a fixture that used a plain error here would exercise the infra branch
	// while claiming to test engine behaviour.
	javaFail := func(class, msg string) plandiff.RunResult {
		return plandiff.RunResult{Engine: "x", Err: &plandiff.JavaError{ExceptionClass: class, Message: msg}}
	}
	abc := [][]any{{int64(1)}, {int64(2)}}
	other := [][]any{{int64(9)}}

	cases := []struct {
		name string
		div  *plandiff.Divergence
		java plandiff.RunResult
		go_  plandiff.RunResult
		want bool
	}{
		// --- JavaIntermittentGoCorrect: the intermittent-row-order axis. ---
		{
			name: "intermittent: Java succeeds + Go pinned rows → holds",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaIntermittentGoCorrect, GoExpectedRows: abc},
			java: rows([][]any{{int64(2)}, {int64(1)}}), // different ORDER, still success
			go_:  rows(abc),
			want: true,
		},
		{
			name: "intermittent: Java now DETERMINISTICALLY ERRORS → must NOT hold (regression)",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaIntermittentGoCorrect, GoExpectedRows: abc},
			java: javaFail("RecordCoreException", "could not plan query"),
			go_:  rows(abc),
			want: false,
		},
		{
			name: "intermittent: Go errors → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaIntermittentGoCorrect, GoExpectedRows: abc},
			java: rows(abc),
			go_:  fail("go boom"),
			want: false,
		},
		{
			name: "intermittent: Go rows drifted → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaIntermittentGoCorrect, GoExpectedRows: abc},
			java: rows(abc),
			go_:  rows(other),
			want: false,
		},
		// --- JavaErrorsGoCorrect: Java must actually error. ---
		{
			name: "java-errors: Java errors + Go pinned → holds",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaErrorsGoCorrect, GoExpectedRows: abc},
			java: javaFail("NullPointerException", "NPE"),
			go_:  rows(abc),
			want: true,
		},
		{
			name: "java-errors: Java now SUCCEEDS → must NOT hold (divergence gone)",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaErrorsGoCorrect, GoExpectedRows: abc},
			java: rows(abc),
			go_:  rows(abc),
			want: false,
		},
		// --- JavaWrongRowsGoCorrect: Java must succeed AND still be wrong. ---
		{
			name: "java-wrong-rows: Java wrong + Go correct → holds",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaWrongRowsGoCorrect, GoExpectedRows: abc},
			java: rows(other),
			go_:  rows(abc),
			want: true,
		},
		{
			name: "java-wrong-rows: Java now MATCHES Go (fixed) → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaWrongRowsGoCorrect, GoExpectedRows: abc},
			java: rows(abc),
			go_:  rows(abc),
			want: false,
		},
		{
			name: "java-wrong-rows: Java errors → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaWrongRowsGoCorrect, GoExpectedRows: abc},
			java: javaFail("RelationalException", "boom"),
			go_:  rows(abc),
			want: false,
		},
		// --- BothErrorMessagesDrift: both must error; Go substring must match. ---
		{
			name: "both-error: both error + Go substring matches → holds",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceBothErrorMessagesDrift, GoErrorContains: "type mismatch"},
			java: javaFail("RelationalException", "java says nope"),
			go_:  fail("operand type mismatch detected"),
			want: true,
		},
		{
			name: "both-error: Java now succeeds → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceBothErrorMessagesDrift, GoErrorContains: "type mismatch"},
			java: rows(abc),
			go_:  fail("operand type mismatch detected"),
			want: false,
		},
		{
			name: "both-error: Go wording drifted → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceBothErrorMessagesDrift, GoErrorContains: "type mismatch"},
			java: javaFail("RelationalException", "java says nope"),
			go_:  fail("totally different error"),
			want: false,
		},
		// --- JavaSucceedsGoRejects: Java must succeed; Go must reject with substring. ---
		{
			name: "java-succeeds-go-rejects: Java OK + Go rejects with substring → holds",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaSucceedsGoRejects, GoErrorContains: "not supported"},
			java: rows(abc),
			go_:  fail("feature not supported yet"),
			want: true,
		},
		{
			name: "java-succeeds-go-rejects: Java errors → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaSucceedsGoRejects, GoErrorContains: "not supported"},
			java: javaFail("RelationalException", "java also failed"),
			go_:  fail("feature not supported yet"),
			want: false,
		},
		{
			name: "java-succeeds-go-rejects: Go now succeeds → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaSucceedsGoRejects, GoErrorContains: "not supported"},
			java: rows(abc),
			go_:  rows(abc),
			want: false,
		},
		// --- INFRA: a starved or unreachable server is not engine behaviour. ---
		// A DivergenceJavaIntermittentGoCorrect entry annotates "Java always
		// SUCCEEDS, only its row order wanders", so a Java-side error trips its
		// premise check. Measured on this repo: running the full bazel suite
		// concurrently with another Docker-spinning job starved the conformance
		// server into a DeadlineExceededException on `union_all_basic`, and the
		// lock announced it as "Go's behaviour no longer matches the pinned
		// divergence" — pointing at the engine when the cause was the host. The
		// same target passed in isolation.
		{
			name: "infra: Java deadline exceeded → must NOT hold (but as INFRA, not a semantic drift)",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaIntermittentGoCorrect, GoExpectedRows: abc},
			java: javaFail("DeadlineExceededException", "deadline exceeded"),
			go_:  rows(abc),
			want: false,
		},
		{
			name: "infra: transport failure (not a JavaError) → must NOT hold",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceJavaErrorsGoCorrect, GoExpectedRows: abc},
			java: fail("Post \"http://127.0.0.1:1/invoke\": connection refused"),
			go_:  rows(abc),
			want: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, detail := divergenceHolds(tc.div, tc.java, tc.go_)
			if got != tc.want {
				t.Fatalf("divergenceHolds = %v (detail %q), want %v", got, detail, tc.want)
			}
		})
	}
}

// TestJavaInfraFailureLabelsTheCause pins the LABEL, which is the whole point
// of the classifier. Both infra shapes must stay RED — a sick server may never
// pass — but must be reported as infrastructure, never as engine behaviour.
// Asserting only the boolean would let the misleading diagnosis back in
// unnoticed, since the boolean was already correct when the bug was live.
func TestJavaInfraFailureLabelsTheCause(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		err       error
		wantInfra bool
		wantIn    string
	}{
		{
			name:      "server-side deadline is infra",
			err:       &plandiff.JavaError{ExceptionClass: "DeadlineExceededException", Message: "deadline exceeded"},
			wantInfra: true,
			wantIn:    "exceeded its deadline (INFRA, not engine behaviour",
		},
		{
			name:      "transport failure is infra",
			err:       errors.New("connection refused"),
			wantInfra: true,
			wantIn:    "conformance-server call failed (INFRA, not engine behaviour",
		},
		{
			name:      "a genuine engine exception is NOT infra",
			err:       &plandiff.JavaError{ExceptionClass: "RelationalException", Message: "syntax error"},
			wantInfra: false,
		},
		{
			name: "an engine exception that merely MENTIONS a deadline is NOT infra",
			err: &plandiff.JavaError{
				ExceptionClass: "RelationalException",
				Message:        "deadline exceeded while planning",
			},
			wantInfra: false,
		},
		{name: "no error is not infra", err: nil, wantInfra: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			infra, detail := javaInfraFailure(tc.err)
			if infra != tc.wantInfra {
				t.Fatalf("javaInfraFailure(%v) = %v, want %v (detail %q)", tc.err, infra, tc.wantInfra, detail)
			}
			if tc.wantIn != "" && !strings.Contains(detail, tc.wantIn) {
				t.Fatalf("detail %q does not name the cause; want it to contain %q", detail, tc.wantIn)
			}
		})
	}
}
