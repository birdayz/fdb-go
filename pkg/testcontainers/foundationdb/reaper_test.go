package foundationdb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/testcontainers/testcontainers-go"
)

// The patched testcontainers bootstrap gives a Bazel test action a random
// session; upstream's (ppid, parent create time) hash collides across
// concurrent PID-namespaced actions.
func TestSessionPerBazelTest(t *testing.T) {
	if os.Getenv("FDB_REAPER_SESSION_CHILD") == "1" {
		fmt.Print(testcontainers.SessionID())
		os.Exit(0)
	}
	t.Parallel()
	if os.Getenv("TEST_TMPDIR") == "" {
		t.Fatal("TEST_TMPDIR unset: this target must run under bazel test")
	}
	seen := map[string]bool{testcontainers.SessionID(): true}
	// Siblings have the same parent PID and parent creation time, exactly the
	// upstream collision. Distinct processes must get distinct sessions.
	for range 4 {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSessionPerBazelTest$")
		cmd.Env = append(os.Environ(), "FDB_REAPER_SESSION_CHILD=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("session subprocess: %v", err)
		}
		id := string(out)
		if seen[id] {
			t.Fatalf("sibling processes share SessionID() %q", id)
		}
		seen[id] = true
	}
}

// With RYUK_RECONNECTION_TIMEOUT=1s (set by the BUILD target), a reaper whose
// last connection closed is gone a second later. The held connection must keep
// it running after the process's last container terminates, and a later Run
// must start.
func TestReaperOutlivesTheLastContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	first, err := Run(ctx, "")
	if err != nil {
		if SkipWithoutDocker(err) {
			t.Skipf("FDB not available (no Docker): %v", err)
		}
		t.Fatal(err)
	}
	TerminateT(t, ctx, first)

	time.Sleep(4 * time.Second)
	cli, err := tcDockerClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	reaper := "reaper_" + testcontainers.SessionID()
	info, err := cli.ContainerInspect(ctx, reaper)
	switch {
	case cerrdefs.IsNotFound(err):
		t.Fatalf("reaper %s is gone 4 s after the last container terminated", reaper)
	case err != nil:
		t.Fatal(err)
	case !info.State.Running:
		t.Fatalf("reaper %s is %s 4 s after the last container terminated", reaper, info.State.Status)
	}

	second, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run after the last container terminated: %v", err)
	}
	TerminateT(t, ctx, second)
}
