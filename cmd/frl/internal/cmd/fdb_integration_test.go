// End-to-end test for the `frl fdb up` chaining contract (RFC-174 §3.1,
// owner addition): stdout carries exactly the cluster-file path so
// `frl <cmd> --cluster-file $(frl fdb up)` works with zero config.
// Drives the real docker CLI (same prerequisite as the command itself);
// leases a non-ephemeral port to exclude automatic outbound port allocation
// and coordinate competing instances of this fixture.
package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

// runCmdSplit is runCmd with separate stdout/stderr capture — needed to
// assert the fdb up stdout contract (runCmd merges the two streams).
func runCmdSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRoot()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root.SetContext(ctx)
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
		volume, inspectErr := runDocker(context.Background(), "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/var/fdb/data"}}{{.Name}}{{end}}{{end}}`, name)
		out, errOut, err := runCmdSplit(t, "fdb", "down", "--name", name)
		if err != nil {
			t.Errorf("fdb down: %v\nstdout: %s\nstderr: %s", err, out, errOut)
			return
		}
		if inspectErr != nil {
			t.Errorf("inspect anonymous data volume: %v", inspectErr)
			return
		}
		if volume = strings.TrimSpace(volume); volume != "" {
			remaining, err := runDocker(context.Background(), "volume", "ls", "--filter", "name="+volume, "--format", "{{.Name}}")
			if err != nil || strings.TrimSpace(remaining) != "" {
				t.Errorf("fdb down left anonymous data volume %q: %q (error %v)", volume, remaining, err)
			}
		}
	})
	// Keep the UDP lease until after container teardown; FDB needs the TCP socket.
	if err := lease.handoffTCP(); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCmdSplit(t, "fdb", "up",
		"--name", name, "--context", name, "--port", strconv.Itoa(port))
	if err != nil {
		logs, logErr := runDocker(t.Context(), "logs", name)
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
	// The cluster must name loopback, not the image's default container
	// address (a bridge IP a macOS host cannot reach).
	cluster, err := os.ReadFile(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(cluster)), fmt.Sprintf("docker:docker@127.0.0.1:%d", port); got != want {
		t.Fatalf("cluster file = %q, want %q", got, want)
	}
	// The container's own cluster file is the same string, so fdbcli inside
	// it and clients outside agree on the coordinator.
	inContainer, err := runDocker(t.Context(), "exec", name, "cat", "/var/fdb/fdb.cluster")
	if err != nil || strings.TrimSpace(inContainer) != strings.TrimSpace(string(cluster)) {
		t.Fatalf("container cluster file = %q (err %v), want %q", inContainer, err, cluster)
	}
	// Docker must publish only on host loopback, without host networking.
	netMode, err := runDocker(t.Context(), "inspect", "-f", "{{.HostConfig.NetworkMode}}", name)
	if err != nil || strings.TrimSpace(netMode) != "bridge" {
		t.Fatalf("container network mode = %q (err %v), want bridge", netMode, err)
	}
	bindings, err := runDocker(t.Context(), "port", name)
	if err != nil {
		t.Fatalf("docker port: %v\n%s", err, bindings)
	}
	if got, want := strings.TrimSpace(bindings), fmt.Sprintf("%d/tcp -> 127.0.0.1:%d", port, port); got != want {
		t.Fatalf("port bindings = %q, want exactly %q", got, want)
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

	// A coordinator connection alone does not prove advertised storage and
	// commit-proxy addresses are reachable from outside the container.
	db, err := openDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := recordlayer.NewFDBDatabase(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := fdb.Key("frl-network-check")
	_, err = rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		rtx.Transaction().Set(key, []byte("host round trip"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("host commit through published port: %v", err)
	}
	value, err := rec.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		return rtx.Transaction().Get(key).Get()
	})
	if err != nil {
		t.Fatalf("host storage read through published port: %v", err)
	}
	if got := string(value.([]byte)); got != "host round trip" {
		t.Fatalf("host storage read = %q", got)
	}

	// Observe the exec'd process before cancellation, not just a canceled launch.
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	childDone := make(chan error, 1)
	go func() {
		_, err := runDocker(childCtx, "exec", name, "sh", "-c", "touch /tmp/frl-cancel-started; exec sleep 30")
		childDone <- err
	}()
	if err := retry(ctx, 50, 100*time.Millisecond, func() error {
		_, err := runDocker(ctx, "exec", name, "test", "-f", "/tmp/frl-cancel-started")
		return err
	}); err != nil {
		t.Fatalf("wait for Docker child to start: %v", err)
	}
	cancelChild()
	select {
	case err := <-childDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("running Docker child cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("running Docker child ignored cancellation")
	}
}
