package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

func TestFDBQuickstartShell(t *testing.T) {
	t.Parallel()
	for _, doc := range []struct {
		path     string
		examples []string
	}{
		{"README.md", []string{"./example/sql", "./example"}},
		{"website/content/docs/getting-started.md", []string{"./example/sql"}},
	} {
		t.Run(doc.path, func(t *testing.T) {
			t.Parallel()
			block := fdbQuickstartBlock(t, doc.path)
			for _, scenario := range []struct {
				name        string
				buildStatus int
				upStatus    int
				sqlStatus   int
			}{
				{name: "success"},
				{name: "build_failure", buildStatus: 17},
				{name: "startup_failure", upStatus: 23},
				{name: "sql_failure", sqlStatus: 31},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					logPath := filepath.Join(dir, "calls")
					newCluster := filepath.Join(dir, "disposable cluster.cluster")
					oldCluster := filepath.Join(dir, "real cluster.cluster")
					fdbQuickstartFixtures(t, dir)

					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					// No errexit: pasted commands must stop themselves before touching another cluster.
					command := exec.CommandContext(ctx, "sh", "-c", block)
					command.Dir = dir
					command.Env = append(os.Environ(),
						"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
						"FDB_CLUSTER_FILE="+oldCluster,
						"QUICKSTART_LOG="+logPath,
						"QUICKSTART_CLUSTER="+newCluster,
						fmt.Sprintf("QUICKSTART_BUILD_STATUS=%d", scenario.buildStatus),
						fmt.Sprintf("QUICKSTART_UP_STATUS=%d", scenario.upStatus),
						fmt.Sprintf("QUICKSTART_SQL_STATUS=%d", scenario.sqlStatus),
					)
					output, err := command.CombinedOutput()
					wantStatus := scenario.buildStatus + scenario.upStatus + scenario.sqlStatus
					if wantStatus == 0 {
						if err != nil {
							t.Errorf("quickstart failed: %v\n%s", err, output)
						}
					} else {
						var exitErr *exec.ExitError
						if !errors.As(err, &exitErr) || exitErr.ExitCode() != wantStatus {
							t.Errorf("quickstart error = %v, want exit %d\n%s", err, wantStatus, output)
						}
					}

					wantCalls := "go|build -o frl ./cmd/frl|" + oldCluster + "\n"
					if scenario.buildStatus == 0 {
						wantCalls += "frl|fdb up|" + oldCluster + "\n"
						if scenario.upStatus == 0 {
							for _, example := range doc.examples {
								wantCalls += "go|run " + example + "|" + newCluster + "\n"
								if scenario.sqlStatus != 0 {
									break
								}
							}
						}
					}
					calls, readErr := os.ReadFile(logPath)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if string(calls) != wantCalls {
						t.Errorf("quickstart calls:\n%s\nwant:\n%s", calls, wantCalls)
					}
				})
			}
		})
	}
}

func TestFDBQuickstartFixturesUseRunfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fdbQuickstartFixtures(t, dir)
	for _, name := range []string{"go", "frl"} {
		path, err := runfiles.Rlocation("_main/cmd/frl/internal/cmd/testdata/quickstart/" + name + ".sh")
		if err != nil {
			t.Fatal(err)
		}
		fixture, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		executable, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		// Copies reintroduce writable executable descriptors during parallel forks.
		if !os.SameFile(fixture, executable) {
			t.Errorf("%s: executable must reuse the runfile inode, not a writable copy", name)
		}
	}
}

func fdbQuickstartFixtures(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "calls"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Concurrent forks can retain script writers until exec, causing ETXTBSY
	// even after WriteFile returns. Reuse runfiles without opening them for write.
	for _, name := range []string{"go", "frl"} {
		path, err := runfiles.Rlocation("_main/cmd/frl/internal/cmd/testdata/quickstart/" + name + ".sh")
		if err != nil {
			t.Fatal(err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func fdbQuickstartBlock(t *testing.T, doc string) string {
	t.Helper()
	path, err := runfiles.Rlocation("_main/" + doc)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	for _, afterFence := range strings.Split(string(content), "```sh\n")[1:] {
		block, _, closed := strings.Cut(afterFence, "\n```")
		if closed && strings.Contains(block, "./frl fdb up") {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("%s: found %d quickstart shell blocks, want 1", doc, len(blocks))
	}
	// zsh without INTERACTIVE_COMMENTS runs pasted `#` lines as commands; keep one && chain.
	lines := strings.Split(blocks[0], "\n")
	for i, line := range lines {
		last := i == len(lines)-1
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") ||
			strings.HasSuffix(line, "&&") == last {
			t.Fatalf("%s: quickstart line %d %q: want a non-comment command, chained with && except the last", doc, i+1, line)
		}
	}
	return blocks[0]
}
