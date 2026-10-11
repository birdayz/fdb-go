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
					files := map[string]string{
						"calls": "",
						"go": `#!/bin/sh
printf 'go|%s|%s\n' "$*" "$FDB_CLUSTER_FILE" >> "$QUICKSTART_LOG"
case "$*" in
  'build -o frl ./cmd/frl') exit "$QUICKSTART_BUILD_STATUS" ;;
  'run ./example/sql') exit "$QUICKSTART_SQL_STATUS" ;;
  'run ./example') exit 0 ;;
  *) exit 99 ;;
esac
`,
						"frl": `#!/bin/sh
printf 'frl|%s|%s\n' "$*" "$FDB_CLUSTER_FILE" >> "$QUICKSTART_LOG"
[ "$*" = 'fdb up' ] || exit 99
[ "$QUICKSTART_UP_STATUS" -eq 0 ] || exit "$QUICKSTART_UP_STATUS"
printf '%s\n' "$QUICKSTART_CLUSTER"
`,
					}
					for name, content := range files {
						if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
							t.Fatal(err)
						}
					}

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
