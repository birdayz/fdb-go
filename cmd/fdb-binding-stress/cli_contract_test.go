package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRunnerCLIFlagAndFilesystemContracts(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"scripted", "api", "directory", "directory_hca"} {
		t.Run(name, func(t *testing.T) {
			f := newCLIContractFixture(t)
			out := filepath.Join(f.root, "out")
			status, output := f.run(t, f.ws, out, "-test-name", name, "-concurrency", "5", "-api-version", "610", "-ops", "37", "-seed-start", "42")
			if status != 0 {
				t.Fatalf("exit=%d: %s", status, output)
			}
			report := readCLIContractReport(t, filepath.Join(out, "report.json"))
			want := workload{name, 37, 5, false, 610}
			if report.Total != 1 || report.Pass != 1 || report.Fail != 0 || len(report.Seeds) != 1 || report.Seeds[0].Seed != 42 || report.Seeds[0].Workload != want {
				t.Fatalf("parsed CLI did not reach executor/report: %+v", report)
			}
			// Independent literal argv: not expected values derived from actual w.
			args := []string{"HARNESS/bindingtester/bindingtester.py", "--cluster-file", "CLUSTER", "--test-name", name, "--api-version", "610", "--num-ops", "37", "--seed", "42", "--timeout", "300", "--concurrency", "5", "--no-tenants"}
			if name == "directory" || name == "directory_hca" {
				args = append(args, "--no-directory-snapshot-ops")
			}
			args = append(args, "pure_go")
			if !reflect.DeepEqual(report.Seeds[0].Command, args) {
				t.Fatalf("forwarded argv=%q want %q", report.Seeds[0].Command, args)
			}
			if data, err := os.ReadFile(f.seedMarker); err != nil || string(data) != "seed invoked\n" {
				t.Fatalf("no executor positive control: %q %v", data, err)
			}
		})
	}
	t.Run("bad_output_parent", func(t *testing.T) {
		f := newCLIContractFixture(t)
		blocked := filepath.Join(f.root, "blocked")
		if err := os.WriteFile(blocked, []byte("sentinel"), 0o644); err != nil {
			t.Fatal(err)
		}
		status, output := f.run(t, f.ws, filepath.Join(blocked, "out"))
		// MkdirAll(parent) reports blocked, whereas deleting its error check lets
		// Mkdir(output) report blocked/out. Both fail; distinguish the guarded phase.
		if status != 1 || !strings.Contains(output, "mkdir "+blocked+": not a directory") {
			t.Fatalf("wrong parent rejection: exit=%d %s", status, output)
		}
		if strings.Contains(output, "mkdir "+filepath.Join(blocked, "out")+":") {
			t.Fatalf("fell through parent failure: %s", output)
		}
		if data, err := os.ReadFile(blocked); err != nil || string(data) != "sentinel" {
			t.Fatalf("parent changed: %q %v", data, err)
		}
		f.assertNoSeed(t)
	})
	t.Run("bad_workspace", func(t *testing.T) {
		f := newCLIContractFixture(t)
		missing := filepath.Join(f.root, "missing-workspace")
		status, output := f.run(t, missing, filepath.Join(f.root, "out"))
		if status != 1 || !strings.Contains(output, "change to workspace: chdir "+missing) {
			t.Fatalf("wrong chdir rejection: exit=%d %s", status, output)
		}
		if _, err := os.Stat(f.bazelLog); !os.IsNotExist(err) {
			t.Fatalf("nested Bazel ran after chdir failed: %v", err)
		}
		f.assertNoSeed(t)
	})
	t.Run("default_output_nanosecond_collision_resistance", func(t *testing.T) {
		f := newCLIContractFixture(t)
		// Real clock, two invocations, no sleep. This observes collision resistance,
		// not guaranteed uniqueness or deterministic same-second execution.
		for range 2 {
			status, output := f.run(t, f.ws, "")
			if status != 0 {
				t.Fatalf("default output: exit=%d %s", status, output)
			}
		}
		entries, err := os.ReadDir(filepath.Join(f.ws, "binding-stress-out"))
		if err != nil || len(entries) != 2 {
			t.Fatalf("expected two distinct outputs: entries=%v err=%v", entries, err)
		}
		pattern := regexp.MustCompile(`^[0-9]{8}-[0-9]{6}\.[0-9]{9}$`)
		for _, entry := range entries {
			if !entry.IsDir() || !pattern.MatchString(entry.Name()) {
				t.Fatalf("default output lacks nanosecond basename: %q", entry.Name())
			}
			report := readCLIContractReport(t, filepath.Join(f.ws, "binding-stress-out", entry.Name(), "report.json"))
			if report.Total != 1 || report.Pass != 1 || report.Fail != 0 || len(report.Seeds) != 1 || !report.Seeds[0].Pass {
				t.Fatalf("empty default-output control: %+v", report)
			}
		}
	})
}

// Called by the ACTUAL TestHarnessLifecycleChild after resetting flag.CommandLine.
func configureCLIContractChild(t *testing.T) bool {
	t.Helper()
	raw := os.Getenv("FDB_STRESS_CLI_ARGS")
	if raw == "" {
		return false
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"binding-stress"}, args...)
	return true
}

type cliContractFixture struct{ root, ws, bin, base, seedMarker, bazelLog string }

func newCLIContractFixture(t *testing.T) cliContractFixture {
	t.Helper()
	root, ws, base, bin := setupFixture(t)
	f := cliContractFixture{root, ws, bin, base, filepath.Join(root, "seed-called"), filepath.Join(root, "bazel-called")}
	archive, err := os.ReadFile("python-fdb.tar")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "bazel-bin/cmd/fdb-binding-stress/python-fdb.tar"), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	// This is the CLI contract, not Python imports or nested build execution.
	// Accept only production's actual preflight invocation in its owned harness.
	probe := `#!/bin/sh
[ "$#" = 2 ] && [ "$1" = -c ] && [ "$2" = "$CLI_PREFLIGHT_PROGRAM" ] || exit 91
case "$PWD" in "$TMPDIR"/fdb-binding-harness-*) ;; *) exit 92;; esac
printf 'fixture preflight client\n'
`
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CLI_BAZEL_LOG\"\nif [ \"$1\" = info ]; then printf '%s\\n' \"$FDB_TEST_OUTPUT_BASE\"; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "bazelisk"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f cliContractFixture) run(t *testing.T, workspace, out string, extra ...string) (int, string) {
	t.Helper()
	args := []string{"-seeds", "1"}
	if out != "" {
		args = append(args, "-out", out)
	}
	args = append(args, extra...)
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestHarnessLifecycleChild$")
	configureTesterProcess(cmd)
	// Context cancellation kills the private group; WaitDelay bounds pipe drain.
	// Do not defer group kill after Wait: its PID may already have been reused.
	// Set cmd.Dir to an existing workspace even for the failed-Chdir test. The
	// production setup must reject the missing env path, not exec itself.
	cmd.Dir = f.ws
	cmd.Env = append(os.Environ(), "PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "BUILD_WORKSPACE_DIRECTORY="+workspace, "FDB_TEST_OUTPUT_BASE="+f.base, "TMPDIR="+f.root, "FDB_VERSION=7.3.77", "FDB_STRESS_LIFECYCLE=cli", "FDB_STRESS_CLI_ARGS="+string(encoded), "FDB_STRESS_CLI_SEED_MARKER="+f.seedMarker, "CLI_BAZEL_LOG="+f.bazelLog, "CLI_PREFLIGHT_PROGRAM="+preflightPythonProgram)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("CLI child exceeded bound: %v\n%s", ctx.Err(), output)
	}
	if err == nil {
		return 0, string(output)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("start child: %v", err)
	}
	return exit.ExitCode(), string(output)
}

func (f cliContractFixture) assertNoSeed(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.seedMarker); !os.IsNotExist(err) {
		t.Fatalf("seed ran after setup rejection: %v", err)
	}
}

func readCLIContractReport(t *testing.T, path string) Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestValidateRunTimedSeedBoundariesAndAcceptedNames(t *testing.T) {
	// No Parallel: validateRun reads process FDB_VERSION; Setenv neutralizes hosts.
	t.Setenv("FDB_VERSION", "7.3.77")
	for _, tc := range []struct {
		name         string
		seeds, start int
		duration     time.Duration
		want         string
	}{
		{"timed_oversized_start", 0, 0x100000000, 20 * time.Minute, "seed-start must be in [1, 2^32-1]"},
		{"timed_max_start", 0, 0xffffffff, 20 * time.Minute, ""},
		{"timed_counted_range_wraps", 2, 0xffffffff, 20 * time.Minute, ""},
		{"counted_range_overflows", 2, 0xffffffff, 0, "seed range exceeds 32 bits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRun(tc.seeds, tc.start, tc.duration, false, workload{"api", 37, 1, false, 730})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation=%v want %q", err, tc.want)
			}
		})
	}
	for _, name := range []string{"api", "scripted", "directory", "directory_hca"} {
		if err := validateRun(1, 42, 0, false, workload{name, 37, 5, false, 610}); err != nil {
			t.Errorf("documented name %s rejected: %v", name, err)
		}
	}
}
