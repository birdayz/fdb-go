package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const bindingTesterImports = `import sys
sys.path[:0] = ['../python']
import util
from fdb import LATEST_API_VERSION
FDB_API_VERSION = LATEST_API_VERSION
`

func TestPrepareBindingTesterDoesNotPatchPinnedSymlinkInputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pinned := filepath.Join(root, "pinned.py")
	if err := os.WriteFile(pinned, []byte(bindingTesterImports), 0o644); err != nil {
		t.Fatal(err)
	}
	forest := filepath.Join(root, "runfiles")
	if err := os.Mkdir(forest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pinned, filepath.Join(forest, "__init__.py")); err != nil {
		t.Fatal(err)
	}
	var previous string
	for range 3 {
		dir, err := prepareBindingTester(forest, root)
		if err != nil {
			t.Fatal(err)
		}
		if dir == previous {
			t.Fatal("setup reused another invocation's harness")
		}
		previous = dir
		initPy := filepath.Join(dir, "bindingtester", "__init__.py")
		info, err := os.Lstat(initPy)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("copied input is not a regular file: %v, %v", info, err)
		}
		data, err := os.ReadFile(initPy)
		if err != nil {
			t.Fatal(err)
		}
		want := "import sys\n# Use the installed FDB Python client.\nfrom bindingtester import util\nLATEST_API_VERSION = 730\nFDB_API_VERSION = LATEST_API_VERSION\n"
		if string(data) != want {
			t.Fatalf("patched imports = %q, want %q", data, want)
		}
		if out, err := exec.Command("python3", "-m", "py_compile", initPy).CombinedOutput(); err != nil {
			t.Fatalf("patched Python does not compile: %s: %v", out, err)
		}
		unchanged, err := os.ReadFile(pinned)
		if err != nil || string(unchanged) != bindingTesterImports {
			t.Fatalf("setup modified the pinned input: %q, %v", unchanged, err)
		}
	}
}

func TestBindingTesterImportPatchIsIdempotentAndRejectsCorruption(t *testing.T) {
	t.Parallel()
	patched, err := patchBindingTesterImports(bindingTesterImports)
	if err != nil {
		t.Fatal(err)
	}
	again, err := patchBindingTesterImports(patched)
	if err != nil || again != patched {
		t.Fatalf("second patch changed imports: %q, %v", again, err)
	}
	for _, source := range []string{
		strings.ReplaceAll(bindingTesterImports, "import util", "from bindingtester from bindingtester import util"),
		bindingTesterImports + "import util\n",
		strings.ReplaceAll(bindingTesterImports, "from fdb import LATEST_API_VERSION", ""),
		strings.ReplaceAll(bindingTesterImports, "sys.path[:0] = ['../python']", ""),
	} {
		if _, err := patchBindingTesterImports(source); err == nil {
			t.Fatalf("corrupt imports accepted: %q", source)
		}
	}
}

func TestPrepareBindingTesterRemovesFailedPrivateCopies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "__init__.py"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{source, filepath.Join(source, "absent"), t.TempDir()} {
		if dir, err := prepareBindingTester(src, root); err == nil || dir != "" {
			t.Fatalf("invalid source accepted: %q, %v", dir, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed setup left private copies: %v, %v", entries, err)
		}
	}
}

func TestPreparedPinnedBindingTesterImportsPinnedClient(t *testing.T) {
	t.Parallel()
	source := filepath.Join(os.Getenv("TEST_SRCDIR"), "foundationdb+", "bindings", "bindingtester")
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatalf("pinned tester not staged: %v", err)
	}
	root := t.TempDir()
	forest := filepath.Join(root, "forest")
	pinned := map[string][sha256.Size]byte{}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(forest, rel)
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !strings.HasSuffix(path, ".py") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		pinned[path] = sha256.Sum256(data)
		return os.Symlink(path, dst)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) == 0 {
		t.Fatal("no pinned tester Python inputs staged")
	}
	clientTar, err := filepath.Abs("python-fdb.tar")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		dir, err := prepareBindingTester(forest, root)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("tar", "xf", clientTar, "-C", dir).CombinedOutput(); err != nil {
			t.Fatalf("unpack pinned client: %s: %v", out, err)
		}
		cmd := exec.Command("python3", "-S", "-c", `import os, pathlib
import bindingtester
from bindingtester import util
import fdb
root = pathlib.Path(os.getcwd())
assert pathlib.Path(bindingtester.__file__).is_relative_to(root)
assert pathlib.Path(util.__file__).is_relative_to(root)
assert pathlib.Path(fdb.__file__).is_relative_to(root)
assert bindingtester.FDB_API_VERSION == 730
assert callable(fdb.api_version)
print("standalone bindingtester imports verified")
`)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PYTHONPATH="+dir, "PYTHONDONTWRITEBYTECODE=1")
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "standalone bindingtester imports verified") {
			t.Fatalf("prepared pinned tester startup: %s: %v", out, err)
		}
		for path, before := range pinned {
			data, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(data) != before {
				t.Fatalf("pinned tester input changed: %s: %v", path, err)
			}
		}
	}
}

func setupFixture(t *testing.T) (root, ws, base, bin string) {
	t.Helper()
	root = t.TempDir()
	ws = filepath.Join(root, "workspace")
	base = filepath.Join(root, "output-base")
	bin = filepath.Join(root, "bin")
	for _, dir := range []string{
		bin,
		filepath.Join(ws, "bazel-bin", "cmd", "fdb-stacktester", "fdb-stacktester_"),
		filepath.Join(ws, "bazel-bin", "cmd", "fdb-binding-stress"),
		filepath.Join(base, "external", "foundationdb+", "bindings", "bindingtester"),
		filepath.Join(base, "external", "foundationdb+", "bindings", "python", "tests"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(bin, "git"): "#!/bin/sh\n[ \"$*\" = 'rev-parse HEAD' ] || exit 1\nprintf '%s\\n' '0123456789abcdef0123456789abcdef01234567'\n",
		filepath.Join(base, "external", "foundationdb+", "bindings", "bindingtester", "known_testers.py"): "# lifecycle fixture\n",
		filepath.Join(base, "external", "foundationdb+", "bindings", "python", "tests", "tester.py"):      "# lifecycle oracle fixture\n",
		filepath.Join(bin, "bazelisk"): "#!/bin/sh\nif [ \"$1\" = info ]; then printf '%s\\n' \"$FDB_TEST_OUTPUT_BASE\"; fi\n",
		filepath.Join(ws, "bazel-bin", "cmd", "fdb-stacktester", "fdb-stacktester_", "fdb-stacktester"): "stub",
		filepath.Join(base, "external", "foundationdb+", "bindings", "bindingtester", "__init__.py"):    bindingTesterImports,
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, ws, base, bin
}

func TestRunRemovesHarnessOnSuccessAndFailedSeed(t *testing.T) {
	t.Parallel()
	client, err := os.ReadFile("python-fdb.tar")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "failure", "report_write_failure", "report_rename_failure"} {
		t.Run(mode, func(t *testing.T) {
			root, ws, base, bin := setupFixture(t)
			clientTar := filepath.Join(ws, "bazel-bin", "cmd", "fdb-binding-stress", "python-fdb.tar")
			if err := os.WriteFile(clientTar, client, 0o644); err != nil {
				t.Fatal(err)
			}
			outDir := filepath.Join(root, "out")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessLifecycleChild$")
			cmd.Env = append(
				os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"BUILD_WORKSPACE_DIRECTORY="+ws,
				"FDB_TEST_OUTPUT_BASE="+base,
				"TMPDIR="+root,
				"FDB_STRESS_LIFECYCLE="+mode,
				"FDB_STRESS_TEST_OUT="+outDir,
			)
			output, err := cmd.CombinedOutput()
			status := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					status = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			wantStatus, wantPass, wantFail := 0, 1, 0
			if mode != "success" {
				wantStatus = 1
			}
			if mode == "failure" {
				wantPass, wantFail = 0, 1
			}
			if status != wantStatus {
				t.Fatalf("exit = %d, want %d: %s", status, wantStatus, output)
			}
			harness := ""
			for _, line := range strings.Split(string(output), "\n") {
				if strings.HasPrefix(line, "Harness: ") {
					harness = strings.TrimPrefix(line, "Harness: ")
				}
			}
			if harness == "" || !strings.HasPrefix(harness, root+string(os.PathSeparator)) {
				t.Fatalf("missing owned harness under fixture root: %s", output)
			}
			if _, err := os.Stat(harness); !os.IsNotExist(err) {
				t.Fatalf("completed campaign leaked owned harness %s: %v", harness, err)
			}
			left, err := filepath.Glob(filepath.Join(root, "fdb-binding-harness-*"))
			if err != nil || len(left) != 0 {
				t.Fatalf("completed campaign leaked harness: %v, %v", left, err)
			}
			if strings.HasPrefix(mode, "report_") {
				blocked := filepath.Join(outDir, "report.json.tmp")
				if mode == "report_rename_failure" {
					blocked = filepath.Join(outDir, "report.json")
				}
				data, err := os.ReadFile(filepath.Join(blocked, "sentinel"))
				if err != nil || string(data) != "prior evidence" || !strings.Contains(string(output), blocked) {
					t.Fatalf("lost evidence rejection: sentinel=%q err=%v output=%s", data, err, output)
				}
				if mode == "report_write_failure" {
					data, err := os.ReadFile(filepath.Join(outDir, "report.json"))
					if err != nil || string(data) != "prior report" {
						t.Fatalf("overwrote prior report: %q %v", data, err)
					}
				} else {
					data, err := os.ReadFile(filepath.Join(outDir, "report.json.tmp"))
					var report Report
					if err != nil || json.Unmarshal(data, &report) != nil || report.Total != 1 || report.Pass != 1 || report.Fail != 0 || report.FDBDeaths != 0 || len(report.Seeds) != 1 || !report.Seeds[0].Pass {
						t.Fatalf("lost completed report evidence: %s %v", data, err)
					}
				}
				logs, err := filepath.Glob(filepath.Join(outDir, "case-*/tester.log"))
				if err != nil || len(logs) != 1 {
					t.Fatalf("missing completed seed evidence: %v %v", logs, err)
				}
				data, err = os.ReadFile(logs[0])
				if err != nil || string(data) != "controlled seed completed\n" {
					t.Fatalf("fixture did not execute completed seed: %q %v", data, err)
				}
				return
			}
			data, err := os.ReadFile(filepath.Join(outDir, "report.json"))
			if err != nil {
				t.Fatalf("no completed campaign report: %v: %s", err, output)
			}
			var report Report
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			if report.Total != 1 || report.Pass != wantPass || report.Fail != wantFail {
				t.Fatalf("campaign population/status = %+v", report)
			}
		})
	}
}

func TestHarnessLifecycleChild(t *testing.T) {
	mode := os.Getenv("FDB_STRESS_LIFECYCLE")
	if mode == "" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("binding-stress-lifecycle", flag.ExitOnError)
	if !configureCLIContractChild(t) {
		os.Args = []string{"binding-stress", "-seeds", "1", "-out", os.Getenv("FDB_STRESS_TEST_OUT")}
	}
	os.Exit(run(func(_ context.Context, seed int, w workload, harness, dir string) SeedResult {
		if fi, err := os.Stat(harness); err != nil || !fi.IsDir() {
			t.Fatalf("runner did not own live harness: %v", err)
		}
		if mode == "cli" {
			if err := os.WriteFile(os.Getenv("FDB_STRESS_CLI_SEED_MARKER"), []byte("seed invoked\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return SeedResult{Seed: seed, Workload: w, Pass: true, FDBAlive: true, Command: testerArgs(seed, w, "CLUSTER", "HARNESS")}
		}
		if strings.HasPrefix(mode, "report_") {
			if err := os.WriteFile(filepath.Join(dir, "tester.log"), []byte("controlled seed completed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			output := filepath.Dir(dir)
			blocked := filepath.Join(output, "report.json.tmp")
			if mode == "report_rename_failure" {
				blocked = filepath.Join(output, "report.json")
				if err := os.Remove(blocked); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(blocked, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blocked, "sentinel"), []byte("prior evidence"), 0o644); err != nil {
				t.Fatal(err)
			}
			if mode == "report_write_failure" {
				if err := os.WriteFile(filepath.Join(output, "report.json"), []byte("prior report"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			return SeedResult{Seed: seed, Workload: w, Pass: true, FDBAlive: true}
		}
		return SeedResult{Seed: seed, Workload: w, Pass: mode == "success", FDBAlive: true, Error: "controlled seed verdict"}
	}))
}

func TestSetupCleansHarnessAfterClientInitializationFailure(t *testing.T) {
	root, ws, base, bin := setupFixture(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILD_WORKSPACE_DIRECTORY", ws)
	t.Setenv("FDB_TEST_OUTPUT_BASE", base)
	t.Setenv("TMPDIR", root)
	clientTar := filepath.Join(ws, "bazel-bin", "cmd", "fdb-binding-stress", "python-fdb.tar")
	for _, preflightFailure := range []bool{false, true} {
		if preflightFailure {
			f, err := os.Create(clientTar)
			if err != nil {
				t.Fatal(err)
			}
			w := tar.NewWriter(f)
			data := "raise RuntimeError('broken pinned client')\n"
			if err := w.WriteHeader(&tar.Header{Name: "fdb/__init__.py", Mode: 0o644, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(data)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(clientTar, []byte("invalid tar"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := setup()
		want := "unpack python fdb"
		if preflightFailure {
			want = "broken pinned client"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("setup error = %v, want %q", err, want)
		}
		left, err := filepath.Glob(filepath.Join(root, "fdb-binding-harness-*"))
		if err != nil || len(left) != 0 {
			t.Fatalf("failed setup leaked harness: %v, %v", left, err)
		}
	}
}
