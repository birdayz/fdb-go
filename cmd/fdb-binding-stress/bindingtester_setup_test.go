package main

import (
	"archive/tar"
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
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
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
	for _, mode := range []string{"success", "failure"} {
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
			if mode == "failure" {
				wantStatus, wantPass, wantFail = 1, 0, 1
			}
			if status != wantStatus {
				t.Fatalf("exit = %d, want %d: %s", status, wantStatus, output)
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
			left, err := filepath.Glob(filepath.Join(root, "fdb-binding-harness-*"))
			if err != nil || len(left) != 0 {
				t.Fatalf("completed campaign leaked harness: %v, %v", left, err)
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
	os.Args = []string{"binding-stress", "-seeds", "1", "-out", os.Getenv("FDB_STRESS_TEST_OUT")}
	os.Exit(run(func(seed, _ int, _, _, _, _ string) SeedResult {
		return SeedResult{Seed: seed, Pass: mode == "success", FDBAlive: true, Error: "controlled seed verdict"}
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
