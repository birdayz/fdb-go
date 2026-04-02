// Package bindingtester runs the official FDB binding tester against our
// pure Go stacktester. Fully Bazel-native via testcontainers.
//
// Architecture:
//
//	Docker network (shared)
//	├── FDB container (foundationdb:7.3.75) — the database
//	└── Tester container (custom image: Python + fdb + bindingtester.py + our binary)
//	    └── runs: bindingtester.py --tester-binary /usr/local/bin/fdb-stacktester
//
// Run: bazelisk test //cmd/fdb-stacktester/bindingtester:bindingtester_test --test_output=streamed
package bindingtester

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestBindingTester(t *testing.T) {
	if testing.Short() {
		t.Skip("binding tester requires Docker")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Start FDB container.
	fdbContainer, err := tcfdb.Run(ctx, "", tcfdb.WithVersion("7.3.75"))
	if err != nil {
		t.Fatalf("start FDB: %v", err)
	}
	defer fdbContainer.Terminate(ctx)

	// Initialize database.
	if err := fdbContainer.InitializeDatabase(ctx); err != nil {
		t.Fatalf("init DB: %v", err)
	}

	clusterFile := fdbContainer.InternalClusterFile()
	networkName := fdbContainer.NetworkName()
	t.Logf("FDB cluster: %s (network: %s)", clusterFile, networkName)

	// 2. Build the Docker context with everything the tester needs.
	dockerCtx, err := buildDockerContext(t)
	if err != nil {
		t.Fatalf("build docker context: %v", err)
	}

	// 3. Start tester container on the same Docker network.
	testerContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    dockerCtx,
				Dockerfile: "Dockerfile",
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

	// Write cluster file inside tester container.
	err = testerContainer.CopyToContainer(ctx, []byte(clusterFile), "/etc/foundationdb/fdb.cluster", 0644)
	if err != nil {
		t.Fatalf("copy cluster file: %v", err)
	}

	// 4. Run bindingtester.py.
	numOps := 100
	exitCode, output, err := testerContainer.Exec(ctx, []string{
		"python3", "/opt/fdb/bindings/bindingtester/bindingtester.py",
		"--cluster-file", "/etc/foundationdb/fdb.cluster",
		"--test-name", "api",
		"--num-ops", fmt.Sprintf("%d", numOps),
		"--tester-binary", "/usr/local/bin/fdb-stacktester",
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

// buildDockerContext assembles a temp dir with: Dockerfile, bindingtester.py,
// Python fdb binding, and our stacktester binary.
func buildDockerContext(t *testing.T) (string, error) {
	t.Helper()

	dir, err := os.MkdirTemp("", "fdb-binding-tester-*")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	fdbSrc := findFDBSource()
	if fdbSrc == "" {
		return "", fmt.Errorf("cannot find FDB source; run 'bazelisk build //...' first")
	}

	// Dockerfile.
	repoRoot := getRepoRoot()
	copyFile(t, filepath.Join(repoRoot, "cmd/fdb-stacktester/bindingtester/Dockerfile"), filepath.Join(dir, "Dockerfile"))

	// bindingtester Python files.
	copyDir(t, filepath.Join(fdbSrc, "bindings/bindingtester"), filepath.Join(dir, "bindingtester"))

	// Python fdb binding + generated apiversion.py.
	copyDir(t, filepath.Join(fdbSrc, "bindings/python"), filepath.Join(dir, "python"))
	os.WriteFile(filepath.Join(dir, "python/fdb/apiversion.py"), []byte("LATEST_API_VERSION = 730\n"), 0644)

	// Build and copy stacktester binary (linux/amd64).
	testerBin, err := buildStacktester()
	if err != nil {
		return "", fmt.Errorf("build stacktester: %v", err)
	}
	copyFile(t, testerBin, filepath.Join(dir, "fdb-stacktester"))

	return dir, nil
}

func findFDBSource() string {
	out, err := exec.Command("bazelisk", "info", "output_base").Output()
	if err == nil {
		p := filepath.Join(strings.TrimSpace(string(out)), "external/foundationdb+")
		if _, err := os.Stat(filepath.Join(p, "bindings/bindingtester")); err == nil {
			return p
		}
	}
	return ""
}

func getRepoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func buildStacktester() (string, error) {
	cmd := exec.Command("bazelisk", "build", "//cmd/fdb-stacktester")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	out, err := exec.Command("bazelisk", "info", "bazel-bin").Output()
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(string(out)), "cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester"), nil
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	os.WriteFile(dst, data, 0755)
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := exec.Command("cp", "-r", src, dst).Run(); err != nil {
		t.Fatalf("cp -r %s %s: %v", src, dst, err)
	}
}
