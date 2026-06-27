//go:build bazelrunfiles

// Package bindingtester runs the official FDB binding tester against our
// pure Go stacktester. Fully Bazel-native via testcontainers + data deps.
//
// Architecture:
//
//	Docker network (shared)
//	├── FDB container (foundationdb:7.3.77) — the database
//	└── Tester container (custom image: Python + fdb + bindingtester.py + our binary)
//	    └── runs: bindingtester.py --tester-binary /usr/local/bin/fdb-stacktester
//
// Run:
//
//	bazelisk test //cmd/fdb-stacktester/bindingtester --test_output=streamed
package bindingtester

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestBindingTester(t *testing.T) {
	if testing.Short() {
		t.Skip("binding tester requires Docker")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// 1. Create shared network + start FDB container.
	nw, err := tcfdb.CreateNetwork(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	defer nw.Remove(ctx)

	fdbContainer, err := tcfdb.Run(ctx, "", tcfdb.WithNetwork(nw))
	if err != nil {
		t.Fatalf("start FDB: %v", err)
	}
	defer fdbContainer.Terminate(ctx)

	clusterFile := fdbContainer.InternalClusterFile()
	networkName := fdbContainer.NetworkName()
	t.Logf("FDB cluster: %s (network: %s)", clusterFile, networkName)

	// 2. Build Docker context from Bazel runfiles.
	dockerCtx, err := buildDockerContext(t)
	if err != nil {
		t.Fatalf("build docker context: %v", err)
	}

	// 3. Start tester container on same Docker network.
	testerContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    dockerCtx,
				Dockerfile: "Dockerfile",
				BuildArgs:  map[string]*string{"FDB_VERSION": strPtr(fdbVersion())},
			},
			Networks: []string{networkName},
			Cmd:      []string{"sleep", "infinity"},
			WaitingFor: wait.ForExec([]string{"true"}).
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start tester container: %v", err)
	}
	defer testerContainer.Terminate(ctx)

	err = testerContainer.CopyToContainer(ctx, []byte(clusterFile), "/etc/foundationdb/fdb.cluster", 0o644)
	if err != nil {
		t.Fatalf("copy cluster file: %v", err)
	}

	// 4. Run bindingtester.py.
	numOps := 100
	exitCode, output, err := testerContainer.Exec(ctx, []string{
		"python3", "/opt/fdb/bindings/bindingtester/bindingtester.py",
		"--cluster-file", "/etc/foundationdb/fdb.cluster",
		"--test-name", "api",
		"--api-version", "730",
		"--num-ops", fmt.Sprintf("%d", numOps),
		"--timeout", "600",
		"--no-threads",
		"--no-tenants",
		"/usr/local/bin/fdb-stacktester",
	})
	if err != nil {
		t.Fatalf("exec bindingtester: %v", err)
	}

	outBytes, _ := io.ReadAll(output)
	t.Logf("bindingtester output:\n%s", string(outBytes))

	if exitCode != 0 {
		t.Fatalf("bindingtester exited with code %d", exitCode)
	}

	t.Log("binding tester PASSED")
}

func buildDockerContext(t *testing.T) (string, error) {
	t.Helper()

	dir, err := os.MkdirTemp("", "fdb-binding-tester-*")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	runfiles := findRunfilesDir()
	if runfiles == "" {
		return "", fmt.Errorf("cannot find Bazel runfiles directory (TEST_SRCDIR not set)")
	}
	t.Logf("runfiles: %s", runfiles)

	ws := "_main"

	// Dockerfile.
	src := filepath.Join(runfiles, ws, "cmd/fdb-stacktester/bindingtester/Dockerfile")
	if err := copyFile(src, filepath.Join(dir, "Dockerfile")); err != nil {
		return "", fmt.Errorf("Dockerfile: %w", err)
	}

	// bindingtester Python files (@foundationdb+).
	if err := copyDirFollow(filepath.Join(runfiles, "foundationdb+", "bindings/bindingtester"), filepath.Join(dir, "bindingtester")); err != nil {
		return "", fmt.Errorf("bindingtester: %w", err)
	}

	// Python fdb binding (@foundationdb+).
	if err := copyDirFollow(filepath.Join(runfiles, "foundationdb+", "bindings/python"), filepath.Join(dir, "python")); err != nil {
		return "", fmt.Errorf("python fdb: %w", err)
	}

	// Generate apiversion.py (cmake template → constant).
	if err := os.WriteFile(filepath.Join(dir, "python/fdb/apiversion.py"), []byte("LATEST_API_VERSION = 730\n"), 0o644); err != nil {
		return "", fmt.Errorf("write apiversion.py: %w", err)
	}

	// stacktester binary (//cmd/fdb-stacktester).
	bin := filepath.Join(runfiles, ws, "cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester")
	if err := copyFile(bin, filepath.Join(dir, "fdb-stacktester")); err != nil {
		return "", fmt.Errorf("stacktester binary: %w", err)
	}
	os.Chmod(filepath.Join(dir, "fdb-stacktester"), 0o755)

	return dir, nil
}

func findRunfilesDir() string {
	if d := os.Getenv("TEST_SRCDIR"); d != "" {
		return d
	}
	exe, _ := os.Executable()
	rf := exe + ".runfiles"
	if fi, err := os.Stat(rf); err == nil && fi.IsDir() {
		return rf
	}
	return ""
}

func fdbVersion() string {
	if v := os.Getenv("FDB_VERSION"); v != "" {
		return v
	}
	return "7.3.77"
}

func strPtr(s string) *string { return &s }

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	return os.WriteFile(dst, data, 0o755)
}

// copyDirFollow copies a directory, following symlinks (Bazel runfiles are symlink forests).
func copyDirFollow(src, dst string) error {
	cmd := exec.Command("cp", "-rL", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp -rL %s %s: %s: %w", src, dst, out, err)
	}
	return nil
}
