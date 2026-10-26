package foundationdb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/testcontainers/testcontainers-go"
)

// Upstream's parent PID/create-time hash collides across PID-namespaced actions.
// Each process must get its own session, including siblings with the same parent.
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

// The isolated binary sets RYUK_RECONNECTION_TIMEOUT=1s. Ryuk must survive an
// idle gap after the last container terminates so a later Run can still start.
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

func runReaperTestChild(t *testing.T, env ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), append(env, "FDB_REAPER_TEST_CHILD=1")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reaper subprocess: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// Configuration and heldReaper are process-global, so disabled mode needs a
// fresh process that has not already acquired the normal lifetime connection.
func TestReaperDisabled(t *testing.T) {
	if os.Getenv("FDB_REAPER_TEST_CHILD") != "1" {
		runReaperTestChild(t, "TESTCONTAINERS_RYUK_DISABLED=true")
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	first, err := Run(ctx, "")
	if err != nil {
		if SkipWithoutDocker(err) {
			t.Skipf("FDB not available (no Docker): %v", err)
		}
		t.Fatalf("Run with Ryuk disabled: %v", err)
	}
	TerminateT(t, ctx, first)
	cluster, err := RunCluster(ctx, 1)
	if err != nil {
		t.Fatalf("RunCluster with Ryuk disabled: %v", err)
	}
	TerminateT(t, ctx, cluster)
	if heldReaper.conn != nil {
		t.Fatal("Ryuk disabled but a lifetime connection was acquired")
	}
	cli, err := tcDockerClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.ContainerInspect(ctx, "reaper_"+testcontainers.SessionID()); !cerrdefs.IsNotFound(err) {
		t.Fatalf("Ryuk disabled: reaper inspect returned %v; want not found", err)
	}
}

// Reserve Ryuk's name without its labels to force a real reaper-creation error.
// Removing that obstruction must let the same process initialize and retain Ryuk.
func TestReaperInitializationRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if os.Getenv("FDB_REAPER_TEST_CHILD") != "1" {
		// Warm the image through the upstream provider, not the initializer under test.
		if err := tcProber.check(ctx); err != nil {
			if SkipWithoutDocker(err) {
				t.Skipf("FDB not available (no Docker): %v", err)
			}
			t.Fatal(err)
		}
		warm, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{Image: defaultImage + ":" + fdbVersion()},
		})
		testcontainers.CleanupContainer(t, warm)
		if err != nil {
			t.Fatal(err)
		}
		runReaperTestChild(t)
		return
	}
	cli, err := tcDockerClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	networksBefore, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	existingNetworks := make(map[string]bool, len(networksBefore))
	for _, nw := range networksBefore {
		existingNetworks[nw.ID] = true
	}
	name := "reaper_" + testcontainers.SessionID()
	blocker, err := cli.ContainerCreate(ctx, &container.Config{Image: defaultImage + ":" + fdbVersion()}, nil, nil, nil, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cli.ContainerRemove(cleanupCtx, blocker.ID, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			t.Errorf("remove reaper-name blocker: %v", err)
		}
	}()
	failed, err := Run(ctx, "")
	if failed != nil {
		TerminateT(t, ctx, failed)
	}
	if failed != nil || err == nil || !strings.HasPrefix(err.Error(), "hold reaper:") || !cerrdefs.IsConflict(err) {
		t.Fatalf("Run with occupied reaper name: container=%v, err=%v; want held-reaper name conflict", failed, err)
	}
	if heldReaper.conn != nil {
		t.Fatal("failed reaper initialization retained a connection")
	}
	networksAfter, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, nw := range networksAfter {
		if !existingNetworks[nw.ID] && nw.Labels["org.testcontainers.sessionId"] == testcontainers.SessionID() {
			if err := cli.NetworkRemove(ctx, nw.ID); err != nil {
				t.Errorf("remove leaked network %q: %v", nw.Name, err)
			}
			t.Fatalf("failed reaper initialization leaked network %q", nw.Name)
		}
	}
	if err := cli.ContainerRemove(ctx, blocker.ID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	recovered, err := Run(ctx, "")
	if err != nil {
		t.Fatalf("Run after removing reaper-name conflict: %v", err)
	}
	TerminateT(t, ctx, recovered)
	if heldReaper.conn == nil {
		t.Fatal("successful retry did not acquire a lifetime connection")
	}
	time.Sleep(4 * time.Second)
	info, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatalf("inspect retained reaper after recovery: %v", err)
	}
	if !info.State.Running {
		t.Fatalf("reaper after recovery is %s", info.State.Status)
	}
	// Check the selected bridge even when another session already created reaper_default.
	if _, ok := info.NetworkSettings.Networks[testcontainers.Bridge]; !ok {
		t.Fatalf("reaper after recovery uses networks %v; want %s", info.NetworkSettings.Networks, testcontainers.Bridge)
	}
}
