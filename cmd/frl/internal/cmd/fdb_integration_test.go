// End-to-end test for the `frl fdb up` chaining contract (RFC-174 §3.1,
// owner addition): stdout carries exactly the cluster-file path so
// `frl <cmd> --cluster-file $(frl fdb up)` works with zero config.
// Drives the real docker CLI (same prerequisite as the command itself);
// leases a non-ephemeral port to exclude automatic outbound port allocation
// and coordinate competing instances of this fixture.
package cmd

import (
	"bytes"
	"fmt"
	"math/rand/v2"
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
	ephemeral, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		t.Fatalf("read host-network ephemeral port range: %v", err)
	}
	first, last, err := fdbFixtureEphemeralRange(string(ephemeral))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireFDBFixturePort(fdbFixturePortCandidates(rand.IntN(fdbFixturePortCount)), first, last)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.close)
	port := lease.port

	tmp := t.TempDir()
	name := fdbFixtureContainerName(tmp)
	t.Setenv("FRL_CONFIG", filepath.Join(tmp, "config.yaml"))
	t.Cleanup(func() {
		out, errOut, err := runCmdSplit(t, "fdb", "down", "--name", name)
		if err != nil {
			t.Errorf("fdb down: %v\nstdout: %s\nstderr: %s", err, out, errOut)
		}
	})
	// Keep the UDP lease until after container teardown; FDB needs the TCP socket.
	if err := lease.handoffTCP(); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCmdSplit(t, "fdb", "up",
		"--name", name, "--context", name, "--port", strconv.Itoa(port))
	if err != nil {
		logs, logErr := runDocker("logs", name)
		t.Fatalf("fdb up: %v\nstdout: %s\nstderr: %s\ncontainer logs (error %v):\n%s", err, stdout, stderr, logErr, logs)
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
	// Host networking must publish loopback, not the image's default
	// hostname-derived address. The first hostname address can be IPv6,
	// which the container-mode entrypoint writes without FDB's brackets.
	cluster, err := os.ReadFile(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(cluster)), fmt.Sprintf("@127.0.0.1:%d", port)) {
		t.Fatalf("host-network cluster must advertise loopback on port %d, got %q", port, cluster)
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
