//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSetupGuardProcess(t *testing.T) {
	if os.Getenv("RUNNER_SETUP_GUARD_PROCESS") != "1" {
		return
	}
	harness, err := setup()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("SETUP_HARNESS=" + harness)
}

func TestSetupUsesExactNestedBazelArguments(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws, bin, base := filepath.Join(root, "workspace"), filepath.Join(root, "bin"), filepath.Join(root, "selected")
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(ws, "bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester"), "#!/bin/sh\nexit 0\n", 0o755)
	tar, err := filepath.Abs("python-fdb.tar")
	if err != nil {
		t.Fatal(err)
	}
	// The existing archive runfile is exercised by separate import/content tests.
	if err := os.MkdirAll(filepath.Join(ws, "bazel-bin/cmd/fdb-binding-stress"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tar, filepath.Join(ws, "bazel-bin/cmd/fdb-binding-stress/python-fdb.tar")); err != nil {
		t.Fatal(err)
	}
	oracle := filepath.Join(base, "external/foundationdb+/bindings")
	write(filepath.Join(oracle, "bindingtester/__init__.py"), "sys.path[:0] = []\nimport util\nfrom fdb import LATEST_API_VERSION\n", 0o644)
	write(filepath.Join(oracle, "bindingtester/known_testers.py"), "# fixture\n", 0o644)
	write(filepath.Join(oracle, "bindingtester/bindingtester.py"), "# fixture oracle\n", 0o644)
	write(filepath.Join(oracle, "python/tests/tester.py"), "# fixture python tester\n", 0o644)
	write(filepath.Join(bin, "bazelisk"), `#!/bin/bash
set -euo pipefail
printf '%s\n' "$@" >> "$ARG_LOG"
printf '%s\n' -- >> "$ARG_LOG"
case "$1" in
 build) [[ "$#" = 4 && "$2" = --config=ci && "$3" = //cmd/fdb-stacktester:fdb-stacktester && "$4" = //cmd/fdb-binding-stress:python_fdb_tar ]] ;;
 info) [[ "$#" = 3 && "$2" = --config=ci && "$3" = output_base ]]; printf '%s\n' "$SELECTED_BASE" ;;
 *) exit 91 ;;
esac
`, 0o755)
	// Only the setup/argv contract is under test; Python import has its own real probe tests.
	write(filepath.Join(bin, "python3"), "#!/bin/sh\nprintf 'fixture preflight client\\n'\n", 0o755)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "argv")
	cmd := exec.Command(exe, "-test.run=^TestSetupGuardProcess$", "-test.v")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "RUNNER_SETUP_GUARD_PROCESS=1", "BUILD_WORKSPACE_DIRECTORY="+ws, "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+root, "SELECTED_BASE="+base, "ARG_LOG="+log)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "--config=ci", "//cmd/fdb-stacktester:fdb-stacktester", "//cmd/fdb-binding-stress:python_fdb_tar", "--", "info", "--config=ci", "output_base", "--"}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested Bazel argv=%q want%q", got, want)
	}
	harness := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "SETUP_HARNESS=") {
			harness = strings.TrimPrefix(line, "SETUP_HARNESS=")
		}
	}
	if harness == "" {
		t.Fatalf("setup returned no harness: %s", out)
	}
	for _, file := range []string{"bindingtester/bindingtester.py", "python/tester.py", "fdb/__init__.py"} {
		if _, err := os.Stat(filepath.Join(harness, file)); err != nil {
			t.Fatal(err)
		}
	}
	known, err := os.ReadFile(filepath.Join(harness, "bindingtester/known_testers.py"))
	if err != nil || !strings.Contains(string(known), "testers['pure_go']") || !strings.Contains(string(known), "testers['python']") {
		t.Fatalf("registered testers=%q err=%v", known, err)
	}
}

func TestFindBindingTesterRejectsStaleHomeFallback(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "selected")
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, filepath.Join(base, "external/foundationdb+/bindings/bindingtester"), filepath.Join(home, ".cache/bazel/_bazel_fixture/stale/external/foundationdb+/bindings/bindingtester"), filepath.Join(home, ".cache/bazel/_bazel_fixture/stale/external/foundationdb/bindings/bindingtester")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if dir != bin {
			for _, file := range []string{"__init__.py", "bindingtester.py"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("# real stale fixture\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	stub := "#!/bin/sh\n[ \"$#\" = 3 ] && [ \"$1\" = info ] && [ \"$2\" = --config=ci ] && [ \"$3\" = output_base ] || exit 91\nprintf '%s\\n' \"$SELECTED_BASE\"\n"
	if err := os.WriteFile(filepath.Join(bin, "bazelisk"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SELECTED_BASE", base)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	selected := filepath.Join(base, "external/foundationdb+/bindings/bindingtester")
	if got, err := findBindingTesterDir(); err != nil || got != selected {
		t.Fatalf("selected control=%q %v", got, err)
	}
	if err := os.RemoveAll(selected); err != nil {
		t.Fatal(err)
	}
	if got, err := findBindingTesterDir(); err == nil || got != "" || !strings.Contains(err.Error(), "bindingtester dir not found") {
		t.Fatalf("missing selected oracle accepted stale HOME: %q %v", got, err)
	}
}
