package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the shipped executable and setup, not a fabricated runner report.
// Only nested Bazel discovery is substituted: every build input is a declared runfile.
func TestRunnerAssemblesPinnedHarnessAndPersistsRealResults(t *testing.T) {
	runfiles := os.Getenv("TEST_SRCDIR")
	workspace := os.Getenv("TEST_WORKSPACE")
	binary := filepath.Join(runfiles, workspace, "cmd/fdb-binding-stress/fdb-binding-stress_/fdb-binding-stress")
	stacktester := filepath.Join(runfiles, workspace, "cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester")
	pythonTar := filepath.Join(runfiles, workspace, "cmd/fdb-binding-stress/python-fdb.tar")
	nativeFiles, err := filepath.Glob(filepath.Join(runfiles, "*libfdb_c_linux_x86_64/file/libfdb_c.so"))
	if err != nil || len(nativeFiles) != 1 {
		t.Fatalf("native oracle population=%v err=%v", nativeFiles, err)
	}
	native := filepath.Dir(nativeFiles[0])
	for _, tc := range []struct {
		name, mode, output   string
		compare, pass, alive bool
	}{
		{"default_output", "api", "", true, true, true},
		{"relative_output", "directory", "relative/results", true, true, true},
		{"tester_failure", "api", "results", true, false, true},
		{"dead_fdb", "api", "results", false, false, false},
		{"scheduled", "api", "results", false, true, true},
		{"insufficient_budget", "api", "results", false, false, true},
		{"existing_output", "api", "results", false, false, true},
		{"version_override", "api", "results", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ws := filepath.Join(root, "workspace")
			initial := filepath.Join(root, "runfiles-cwd")
			tools := filepath.Join(root, "tools")
			for _, dir := range []string{ws, initial, tools, filepath.Join(ws, "bazel-bin/cmd/fdb-stacktester/fdb-stacktester_"), filepath.Join(ws, "bazel-bin/cmd/fdb-binding-stress")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			command := func(name string, args ...string) string {
				t.Helper()
				cmd := exec.Command(name, args...)
				cmd.Dir = ws
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v %s", name, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			command("git", "init", "-q")
			command("git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture", "--allow-empty")
			revision := command("git", "rev-parse", "HEAD")
			selectedTester := stacktester
			if tc.name == "tester_failure" {
				selectedTester = filepath.Join(tools, "broken-tester")
				if err := os.WriteFile(selectedTester, []byte("#!/bin/sh\necho deliberate-tester-failure >&2\nexit 9\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for dst, src := range map[string]string{
				"bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester": selectedTester,
				"bazel-bin/cmd/fdb-binding-stress/python-fdb.tar":                pythonTar,
			} {
				if err := os.Symlink(src, filepath.Join(ws, dst)); err != nil {
					t.Fatal(err)
				}
			}
			base := root
			if err := os.Mkdir(filepath.Join(base, "external"), 0o755); err != nil {
				t.Fatal(err)
			}
			upstream := filepath.Join(runfiles, "foundationdb+")
			if tc.name == "dead_fdb" {
				// A controlled harness verdict kills its actual case container before
				// claiming success; the runner must independently reject that claim.
				copy := exec.Command("cp", "-rL", upstream, filepath.Join(root, "dead-oracle"))
				if out, err := copy.CombinedOutput(); err != nil {
					t.Fatalf("copy oracle: %v %s", err, out)
				}
				upstream = filepath.Join(root, "dead-oracle")
				fixture := `import json, subprocess, sys
cluster = open(sys.argv[sys.argv.index('--cluster-file') + 1]).read()
ip = cluster.split('@')[1].split(':')[0]
ids = subprocess.check_output(['docker', 'ps', '-q'], text=True).split()
found = False
for cid in ids:
    info = json.loads(subprocess.check_output(['docker', 'inspect', cid]))[0]
    if any(n.get('IPAddress') == ip for n in info['NetworkSettings']['Networks'].values()):
        subprocess.check_call(['docker', 'kill', cid])
        found = True
        break
assert found, 'case container not found'
print('Test had 0 incorrect result(s) and 0 error(s)')
`
				if err := os.WriteFile(filepath.Join(upstream, "bindings/bindingtester/bindingtester.py"), []byte(fixture), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(upstream, filepath.Join(base, "external/foundationdb+")); err != nil {
				t.Fatal(err)
			}
			bazel := "#!/bin/sh\ncase \"$1\" in build) exit 0;; info) printf '%s\\n' \"$FIXTURE_OUTPUT_BASE\";; *) exit 2;; esac\n"
			if err := os.WriteFile(filepath.Join(tools, "bazelisk"), []byte(bazel), 0o755); err != nil {
				t.Fatal(err)
			}
			args := []string{"-seeds", "1", "-seed-start", "146", "-ops", "20", "-test-name", tc.mode}
			if tc.compare {
				args = append(args, "-compare-python")
			}
			if tc.output != "" {
				args = append(args, "-out", tc.output)
			}
			if tc.name == "insufficient_budget" {
				args = append(args, "-scheduled", "-duration", "1ns")
			}
			if tc.name == "existing_output" {
				command("mkdir", "-p", tc.output)
				for _, file := range []string{"report.json", "sentinel.log"} {
					if err := os.WriteFile(filepath.Join(ws, tc.output, file), []byte("prior evidence"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			program := binary
			if tc.name == "scheduled" {
				program, err = os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				args = []string{"-test.run=^TestScheduledRunnerProcess$"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, program, args...)
			configureTesterProcess(cmd)
			defer killTesterGroup(cmd)
			cmd.Dir = initial
			cmd.Env = append(os.Environ(), "BUILD_WORKSPACE_DIRECTORY="+ws, "FIXTURE_OUTPUT_BASE="+base, "PATH="+tools+":"+os.Getenv("PATH"), "LD_LIBRARY_PATH="+native+":"+os.Getenv("LD_LIBRARY_PATH"))
			if tc.name == "scheduled" {
				cmd.Env = append(cmd.Env, "SCHEDULED_RUNNER_FIXTURE=1")
			}
			if tc.name == "version_override" {
				cmd.Env = append(cmd.Env, "FDB_VERSION=7.3.63")
			}
			out, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.pass {
				t.Fatalf("pass=%v err=%v output=%s", tc.pass, runErr, out)
			}
			if tc.name == "existing_output" {
				for _, file := range []string{"report.json", "sentinel.log"} {
					data, err := os.ReadFile(filepath.Join(ws, tc.output, file))
					if err != nil || string(data) != "prior evidence" {
						t.Fatalf("overwrote %s: %q %v", file, data, err)
					}
				}
				return
			}
			if tc.name == "version_override" {
				if !strings.Contains(string(out), "requires FDB_VERSION=7.3.77") {
					t.Fatalf("override failure: %s", out)
				}
				return
			}
			pattern := filepath.Join(ws, tc.output, "report.json")
			if tc.output == "" {
				pattern = filepath.Join(ws, "binding-stress-out/*/report.json")
			}
			paths, err := filepath.Glob(pattern)
			if err != nil || len(paths) != 1 {
				t.Fatalf("report population=%v err=%v output=%s", paths, err, out)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			var report Report
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			if tc.name == "insufficient_budget" {
				if !report.Scheduled || report.Total != 0 || len(report.Seeds) != 0 || report.CompleteCycles != 0 || !strings.Contains(report.Error, "budget exhausted") {
					t.Fatalf("vacuous scheduled run accepted: %s", data)
				}
				return
			}
			if tc.name == "scheduled" {
				if report.Revision != revision || report.FDBVersion != "7.3.77" || !report.Scheduled || report.Total != 19 || report.Pass != 19 || report.Fail != 0 || report.FDBDeaths != 0 || len(report.Seeds) != 19 || report.CompleteCycles != 1 || report.Error != "" {
					t.Fatalf("scheduled population failed: %s", data)
				}
				want := regressionCases()
				for _, mode := range correctnessModes(20) {
					want = append(want, seedCase{146, mode})
				}
				for i, result := range report.Seeds {
					if result.Seed != want[i].Seed || result.Workload != want[i].Workload || !result.Pass || !result.FDBAlive || len(result.Command) == 0 {
						t.Fatalf("scheduled case %d: %+v want %+v", i, result, want[i])
					}
				}
				return
			}
			if report.Revision != revision || report.FDBVersion != "7.3.77" || report.Total != 1 || len(report.Seeds) != 1 || report.CompleteCycles != 1 || report.Scheduled {
				t.Fatalf("wrong report: %s", data)
			}
			r := report.Seeds[0]
			if r.Seed != 146 || r.Workload != (workload{tc.mode, 20, 1, tc.compare, 730}) || r.Pass != tc.pass || r.FDBAlive != tc.alive || len(r.Command) == 0 || r.Command[len(r.Command)-1] != "pure_go" {
				t.Fatalf("wrong case: %s", data)
			}
			if tc.pass {
				if report.Pass != 1 || report.Fail != 0 || report.FDBDeaths != 0 || report.Error != "" {
					t.Fatalf("wrong successful counters: %s", data)
				}
			} else if report.Pass != 0 || report.Fail != 1 || report.FDBDeaths != boolInt(!tc.alive) || report.Error == "" || r.Error == "" {
				t.Fatalf("failure lost: %s", data)
			}
			logs, _ := filepath.Glob(filepath.Join(filepath.Dir(paths[0]), "case-*/tester.log"))
			if len(logs) != 1 {
				t.Fatalf("missing case evidence: %v", logs)
			}
			log, err := os.ReadFile(logs[0])
			if err != nil || len(log) == 0 {
				t.Fatalf("empty case log: %v", err)
			}
			if tc.pass && !successfulVerdict(string(log), nil) {
				t.Fatalf("no actual pinned harness success: %s", log)
			}
			if _, err := os.Stat(filepath.Join(initial, "binding-stress-out")); !os.IsNotExist(err) {
				t.Fatalf("output escaped into initial runfiles cwd: %v", err)
			}
		})
	}
}

// Count-bound the real scheduled executor in a subprocess, avoiding a clock-based
// test cutoff while still running all corpus cases and one full mode cycle.
func TestScheduledRunnerProcess(t *testing.T) {
	if os.Getenv("SCHEDULED_RUNNER_FIXTURE") != "1" {
		return
	}
	harness := setup()
	defer os.RemoveAll(harness)
	if err := os.Mkdir("results", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := executeRun("results", 1, 146, 0, true, workload{"api", 20, 1, false, 730}, harness); err != nil {
		t.Fatal(err)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
