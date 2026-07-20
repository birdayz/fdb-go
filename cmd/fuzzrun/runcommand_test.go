package main

import (
	"io"
	"strings"
	"testing"
)

// The exit status must survive to classify: it is what separates a test failure
// (retryable if the shape matches) from a kill, a timeout, or a build error.
func TestRunCommand_ReportsExitStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		argv     []string
		wantCode int
	}{
		"success":            {[]string{"sh", "-c", "exit 0"}, 0},
		"go test failure":    {[]string{"sh", "-c", "exit 1"}, 1},
		"bazel tests failed": {[]string{"sh", "-c", "exit 3"}, 3},
		"timeout kill":       {[]string{"sh", "-c", "exit 124"}, 124},
		// Killed by a signal: ExitCode() is -1, which classify must reject.
		"killed by signal": {[]string{"sh", "-c", "kill -9 $$"}, -1},
	}
	for name, tc := range cases {
		r := runCommand(tc.argv, io.Discard)
		if r.exitCode != tc.wantCode {
			t.Errorf("%s: exitCode = %d, want %d (err=%v)", name, r.exitCode, tc.wantCode, r.err)
		}
		if name == "success" && !r.ok() {
			t.Errorf("%s: ok() = false, want true", name)
		}
	}
}

func TestRunCommand_CapturesAndStreamsOutput(t *testing.T) {
	t.Parallel()
	var streamed strings.Builder
	r := runCommand([]string{"sh", "-c", "echo out; echo err 1>&2"}, &streamed)
	if r.exitCode != 0 {
		t.Fatalf("exitCode = %d, err = %v", r.exitCode, r.err)
	}
	// Both streams must be captured for classification...
	for _, want := range []string{"out", "err"} {
		if !strings.Contains(r.output, want) {
			t.Errorf("captured output missing %q; got %q", want, r.output)
		}
		// ...and streamed live, so a job killed mid-run still has the log.
		if !strings.Contains(streamed.String(), want) {
			t.Errorf("streamed output missing %q; got %q", want, streamed.String())
		}
	}
}

// A command that cannot be started must surface a reason rather than an empty body.
func TestRunCommand_SpawnFailure(t *testing.T) {
	t.Parallel()
	r := runCommand([]string{"definitely-not-a-real-binary-xyzzy"}, io.Discard)
	if r.exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", r.exitCode)
	}
	if r.err == nil {
		t.Fatal("err = nil, want a spawn error")
	}
	if r.ok() {
		t.Error("ok() = true for a command that never started")
	}
	if classify(r) != verdictRealFailure {
		t.Error("a spawn failure must never be classified as retryable")
	}
}
