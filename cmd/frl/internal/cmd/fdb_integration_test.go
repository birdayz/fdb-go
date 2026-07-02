// End-to-end test for the `frl fdb up` chaining contract (RFC-174 §3.1,
// owner addition): stdout carries exactly the cluster-file path so
// `frl <cmd> --cluster-file $(frl fdb up)` works with zero config.
// Drives the real docker CLI (same prerequisite as the command itself);
// uses a non-default port so it can't collide with a developer's
// default frl-fdb instance or a parallel CI job on the same host.
package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runCmdSplit is runCmd with separate stdout/stderr capture — needed to
// assert the fdb up stdout contract (runCmd merges the two streams).
func runCmdSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestIntegration_FdbUp_StdoutChainsIntoClusterFileFlag(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("FDB not available (no Docker)")
	}
	// Unique name + non-default port: safe next to the developer's own
	// frl-fdb container and other CI jobs on a shared host network.
	name := fmt.Sprintf("frl-e2e-%d", os.Getpid())
	const port = 47501

	tmp := t.TempDir()
	t.Setenv("FRL_CONFIG", filepath.Join(tmp, "config.yaml"))

	stdout, stderr, err := runCmdSplit(t, "fdb", "up",
		"--name", name, "--context", name, "--port", strconv.Itoa(port))
	t.Cleanup(func() {
		_, _, _ = runCmdSplit(t, "fdb", "down", "--name", name)
	})
	if err != nil {
		t.Fatalf("fdb up: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// The contract: stdout is exactly one line — the cluster-file path.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("fdb up stdout must be exactly the cluster-file path, got %d lines:\n%s", len(lines), stdout)
	}
	clusterFile := lines[0]
	if _, statErr := os.Stat(clusterFile); statErr != nil {
		t.Fatalf("stdout %q is not an existing cluster file: %v", clusterFile, statErr)
	}
	// Progress chatter went to stderr, not stdout.
	if !strings.Contains(stderr, "ready") {
		t.Errorf("expected progress on stderr, got:\n%s", stderr)
	}

	// The chain: a fresh config-free invocation against the path.
	// (Point FRL_CONFIG somewhere empty to prove --cluster-file alone
	// is sufficient.)
	t.Setenv("FRL_CONFIG", filepath.Join(tmp, "empty-config.yaml"))
	out, _, err := runCmdSplit(t, "tx", "read-version", "--cluster-file", clusterFile)
	if err != nil {
		t.Fatalf("tx read-version --cluster-file: %v\noutput: %s", err, out)
	}
	if _, convErr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); convErr != nil {
		t.Errorf("read-version output not an integer: %q", out)
	}
}
