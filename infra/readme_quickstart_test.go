package infra

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadmeQuickstartPropagatesFailures(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var snippet string
	matches := 0
	for _, fenced := range strings.Split(string(readme), "```sh\n")[1:] {
		block, _, closed := strings.Cut(fenced, "\n```")
		if !strings.HasPrefix(block, "go build -o .tmpbuild/fdb ") {
			continue
		}
		if !closed {
			t.Fatal("README quickstart has no closing fence")
		}
		snippet = block
		matches++
	}
	if matches != 1 {
		t.Fatalf("found %d README quickstart blocks, want 1", matches)
	}
	for _, tc := range []struct {
		name, buildExit, startupExit string
	}{
		{"build_failure", "42", "0"},
		{"startup_failure", "0", "43"},
		{"success", "0", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, ".tmpbuild"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeBuildStub(t, filepath.Join(dir, "go"), `#!/bin/sh
case "$*" in
    'build -o .tmpbuild/fdb ./cmd/fdb')
        printf '%s\n' "go $*" >> "$CALLS"
        exit "$BUILD_EXIT"
        ;;
    'run ./example/sql'|'run ./example')
        printf '%s|%s\n' "go $*" "${FDB_CLUSTER_FILE-unset}" >> "$CALLS"
        ;;
    *) exit 91 ;;
esac
`)
			writeBuildStub(t, filepath.Join(dir, ".tmpbuild", "fdb"), `#!/bin/sh
[ "$*" = 'dev up' ] || exit 92
printf '%s\n' 'fdb dev up' >> "$CALLS"
if [ "$STARTUP_EXIT" -ne 0 ]; then
    printf '%s\n' 'startup failed' >&2
    exit "$STARTUP_EXIT"
fi
printf '%s\n' "$CLUSTER_PATH"
`)
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
			cmd.Env = append(cmd.Env, "PATH="+dir+":"+os.Getenv("PATH"), "CALLS="+calls, "BUILD_EXIT="+tc.buildExit, "STARTUP_EXIT="+tc.startupExit, "CLUSTER_PATH="+clusterPath)
			out, err := cmd.CombinedOutput()
			want := "go build -o .tmpbuild/fdb ./cmd/fdb\n"
			if tc.buildExit != "0" {
				requireBuildExit(t, err, 42, out)
			} else {
				want += "fdb dev up\n"
				if tc.startupExit != "0" {
					requireBuildExit(t, err, 43, out)
				} else {
					if err != nil {
						t.Fatalf("quickstart failed: %v: %s", err, out)
					}
					want += "go run ./example/sql|" + clusterPath + "\ngo run ./example|" + clusterPath + "\n"
				}
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatalf("quickstart commands=%q, want %q", got, want)
			}
		})
	}
}
