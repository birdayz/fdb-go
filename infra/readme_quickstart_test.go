package infra

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOnboardingSnippetsPropagateFailures(t *testing.T) {
	t.Parallel()
	type shellCase struct {
		name, failCommand string
		count, exit       int
	}
	for _, doc := range []struct {
		name, path, start, commands string
	}{
		{"readme", "../README.md", "go build -o .tmpbuild/fdb ", `go build -o .tmpbuild/fdb ./cmd/fdb
fdb dev up
go run ./example/sql|<cluster>
go run ./example|<cluster>`},
		{"website_build", "../website/content/docs/getting-started.md", "git clone ", `git clone https://github.com/birdayz/fdb-go.git
go build -o .tmpbuild/fdb ./cmd/fdb
fdb version`},
		{"website_examples", "../website/content/docs/getting-started.md", "FDB_CLUSTER_FILE=", `fdb dev up
go run ./example/sql|<cluster>
go run ./example|<cluster>`},
		{"cli_readme_build", "../cmd/fdb/README.md", "go build -o .tmpbuild/fdb ", `go build -o .tmpbuild/fdb ./cmd/fdb
fdb version`},
		{"cli_readme_cluster", "../cmd/fdb/README.md", "fdb dev up", `fdb dev up
fdb tx read-version`},
		{"cli_build", "../website/content/docs/cli/_index.md", "git clone ", `git clone https://github.com/birdayz/fdb-go.git
go build -o .tmpbuild/fdb ./cmd/fdb
fdb version
fdb --help`},
		{"cli_cluster", "../website/content/docs/cli/_index.md", "fdb dev up", `fdb dev up
fdb tx read-version
fdb dev status`},
		{"protobuf_build", "../website/content/docs/cli/protobuf.md", "git clone ", `git clone https://github.com/birdayz/fdb-go.git
go build -o .tmpbuild/fdb ./cmd/fdb
fdb version`},
		{"protobuf_cluster", "../website/content/docs/cli/protobuf.md", "cluster_file=", `fdb dev up --name fdb-rl-example --context contacts
fdb tx read-version
fdb config view`},
		{"sql_build_and_cluster", "../website/content/docs/cli/sql.md", "git clone ", `git clone https://github.com/birdayz/fdb-go.git
go build -o .tmpbuild/fdb ./cmd/fdb
fdb version
fdb dev up --name fdb-sql-demo --context fdb-sql-demo --port 4511
fdb tx read-version`},
		{"metadata_cluster", "../website/content/docs/cli/metadata.md", "fdb dev up", `fdb dev up
fdb tx read-version`},
		{"reference_cluster", "../website/content/docs/cli/reference.md", "cluster_file=", `fdb dev up
fdb sql --cluster-file <cluster> --database /FRL/myapp`},
	} {
		t.Run(doc.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(doc.path)
			if err != nil {
				t.Fatal(err)
			}
			var snippet string
			matches := 0
			for _, fenced := range strings.Split(string(data), "```sh\n")[1:] {
				block, _, closed := strings.Cut(fenced, "\n```")
				if !strings.HasPrefix(block, doc.start) {
					continue
				}
				if !closed {
					t.Fatal("onboarding snippet has no closing fence")
				}
				snippet = block
				matches++
			}
			if matches != 1 {
				t.Fatalf("found %d onboarding blocks starting %q, want 1", matches, doc.start)
			}
			commands := strings.Split(doc.commands, "\n")
			cases := []shellCase{{name: "success", count: len(commands)}}
			for i, command := range commands {
				name, exit := "", 0
				switch {
				case strings.HasPrefix(command, "go build "):
					name, exit = "build_failure", 42
				case strings.HasPrefix(command, "fdb dev up"):
					name, exit = "startup_failure", 43
				case strings.HasPrefix(command, "go run ./example/sql|"):
					name, exit = "sql_example_failure", 44
				}
				if name != "" {
					call, _, _ := strings.Cut(command, "|")
					cases = append(cases, shellCase{name, call, i + 1, exit})
				}
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					for _, path := range []string{"bin", ".tmpbuild", "fdb-go/.tmpbuild"} {
						if err := os.MkdirAll(filepath.Join(dir, path), 0o700); err != nil {
							t.Fatal(err)
						}
					}
					// The clone and built CLI are fixtures; every command still comes from the source fence.
					for _, path := range []string{"bin/git", "bin/go", "bin/fdb", ".tmpbuild/fdb", "fdb-go/.tmpbuild/fdb"} {
						writeBuildStub(t, filepath.Join(dir, path), `#!/bin/sh
command="${0##*/} $*"
case "$command" in
    'go run ./example/sql'|'go run ./example')
        printf '%s|%s\n' "$command" "${FDB_CLUSTER_FILE-unset}" >> "$CALLS"
        ;;
    *) printf '%s\n' "$command" >> "$CALLS" ;;
esac
if [ "$command" = "$FAIL_COMMAND" ]; then
    exit "$FAIL_EXIT"
fi
case "$command" in
    'fdb dev up'|'fdb dev up '*) printf '%s\n' "$CLUSTER_PATH" ;;
esac
`)
					}
					calls := filepath.Join(dir, "calls")
					clusterPath := filepath.Join(dir, "cluster path", "fdb.cluster")
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, "sh", "-c", snippet)
					cmd.Dir = dir
					// An inherited exported variable would hide a missing export in the snippet.
					for _, entry := range os.Environ() {
						if !strings.HasPrefix(entry, "FDB_CLUSTER_FILE=") {
							cmd.Env = append(cmd.Env, entry)
						}
					}
					cmd.Env = append(cmd.Env, "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "CALLS="+calls, "FAIL_COMMAND="+tc.failCommand, "FAIL_EXIT="+strconv.Itoa(tc.exit), "CLUSTER_PATH="+clusterPath, "FDB_CONFIG="+filepath.Join(dir, "config.yaml"))
					out, err := cmd.CombinedOutput()
					if tc.exit != 0 {
						requireBuildExit(t, err, tc.exit, out)
					} else if err != nil {
						t.Fatalf("onboarding snippet failed: %v: %s", err, out)
					}
					got, err := os.ReadFile(calls)
					if err != nil {
						t.Fatal(err)
					}
					want := strings.ReplaceAll(strings.Join(commands[:tc.count], "\n"), "<cluster>", clusterPath) + "\n"
					if string(got) != want {
						t.Fatalf("onboarding commands=%q, want %q", got, want)
					}
				})
			}
		})
	}
}
