package foundationdb

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/testcontainers/testcontainers-go"
)

// The patched testcontainers bootstrap gives a Bazel test action a random
// session; upstream's (ppid, parent create time) hash collides across
// concurrent PID-namespaced actions.
func TestSessionPerBazelTest(t *testing.T) {
	t.Parallel()
	if os.Getenv("TEST_TMPDIR") == "" {
		t.Fatal("TEST_TMPDIR unset: this target must run under bazel test")
	}
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if id := testcontainers.SessionID(); !uuid.MatchString(id) {
		t.Fatalf("SessionID() = %q: the upstream parent-process hash, not a per-test session", id)
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
