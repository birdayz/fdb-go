//go:build bazelrunfiles

package conformance_test

import (
	"errors"
	"testing"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

// TestDivergenceHolds pins divergenceHolds — the RFC-082 regression-lock
// predicate that decides whether an annotated corpus entry's cross-engine
// divergence still holds exactly as documented. It is a pure function (no FDB),
// so this is a plain unit test that runs alongside the Ginkgo suite.
//
// The headline case is the codex-caught regression: a
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
	abc := [][]any{{int64(1)}, {int64(2)}}
	other := [][]any{{int64(9)}}

	cases := []struct {
		name string
		div  *plandiff.Divergence
		java plandiff.RunResult
		go_  plandiff.RunResult
		want bool
	}{
		// --- JavaIntermittentGoCorrect: the codex-caught axis. ---
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
			java: fail("could not plan query"),
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
			java: fail("NPE"),
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
			java: fail("boom"),
			go_:  rows(abc),
			want: false,
		},
		// --- BothErrorMessagesDrift: both must error; Go substring must match. ---
		{
			name: "both-error: both error + Go substring matches → holds",
			div:  &plandiff.Divergence{Direction: plandiff.DivergenceBothErrorMessagesDrift, GoErrorContains: "type mismatch"},
			java: fail("java says nope"),
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
			java: fail("java says nope"),
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
			java: fail("java also failed"),
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
