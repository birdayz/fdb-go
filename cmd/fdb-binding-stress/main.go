// fdb-binding-stress runs the FDB binding tester across many seeds and reports results.
//
// Each seed gets a fresh FDB Docker container (via testcontainers). All output — FDB
// trace logs, docker logs, tester stdout — is saved per-seed into a timestamped run
// directory. A JSON report summarizes the run for machine consumption.
//
// Usage:
//
//	fdb-binding-stress -seeds 100 -ops 1000                  # 100 seeds
//	fdb-binding-stress -duration 2h -ops 1000                # run for 2 hours
//	fdb-binding-stress -seeds 1 -ops 1000 -seed-start 146    # replay a default-mode seed
//
// For other modes, copy every workload setting from report.json; see docs/client-correctness.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
)

const perSeedTimeout = 5 * time.Minute

// SeedResult is the per-seed result written to the JSON report.
type SeedResult struct {
	Workload workload `json:"workload"`
	Command  []string `json:"command"`
	Seed     int      `json:"seed"`
	Pass     bool     `json:"pass"`
	FDBAlive bool     `json:"fdb_alive"`
	Duration float64  `json:"duration_s"`
	Error    string   `json:"error,omitempty"`
}

// Report is the top-level JSON report.
type Report struct {
	Revision       string       `json:"revision"`
	FDBVersion     string       `json:"fdb_version"`
	Scheduled      bool         `json:"scheduled"`
	CompleteCycles int          `json:"complete_cycles"`
	Error          string       `json:"error,omitempty"`
	StartedAt      string       `json:"started_at"`
	Finished       string       `json:"finished_at"`
	Elapsed        string       `json:"elapsed"`
	Ops            int          `json:"ops_per_seed"`
	Total          int          `json:"total_seeds"`
	Pass           int          `json:"pass"`
	Fail           int          `json:"fail"`
	FDBDeaths      int          `json:"fdb_deaths"`
	Seeds          []SeedResult `json:"seeds"`
}

func main() {
	seeds := flag.Int("seeds", 100, "number of seeds (unless -duration is set)")
	ops := flag.Int("ops", 1000, "operations per seed")
	duration := flag.Duration("duration", 0, "wall-time budget; overrides -seeds")
	seedStart := flag.Int("seed-start", 1, "first exploration seed")
	outDir := flag.String("out", "", "output directory (must not exist)")
	testName := flag.String("test-name", "api", "scripted, api, directory, or directory_hca")
	concurrency := flag.Int("concurrency", 1, "concurrent tester count")
	compare := flag.Bool("compare-python", false, "compare results with the official Python binding")
	apiVersion := flag.Int("api-version", 730, "FDB API version")
	scheduled := flag.Bool("scheduled", false, "replay regression corpus then cycle all correctness modes")
	flag.Parse()
	w := workload{Test: *testName, Ops: *ops, Concurrency: *concurrency, Compare: *compare, APIVersion: *apiVersion}
	if err := validateRun(*seeds, *seedStart, *duration, *scheduled, w); err != nil {
		log.Fatal(err)
	}
	btRunDir := setup()
	if *outDir == "" {
		*outDir = filepath.Join("binding-stress-out", time.Now().Format("20060102-150405.000000000"))
	}
	if err := os.MkdirAll(filepath.Dir(*outDir), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.Mkdir(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := executeRun(*outDir, *seeds, *seedStart, *duration, *scheduled, w, btRunDir); err != nil {
		os.RemoveAll(btRunDir)
		log.Print(err)
		os.Exit(1)
	}
	os.RemoveAll(btRunDir)
}

func runSeed(parent context.Context, seed int, w workload, btRunDir, seedDir string) SeedResult {
	return runSeedWithDeps(parent, seed, w, btRunDir, seedDir, defaultRunnerDeps(realRunSeed))
}

func realRunSeed(ctx context.Context, seed int, w workload, btRunDir, seedDir string) (r SeedResult) {
	r = SeedResult{Seed: seed, Workload: w}

	// Start FDB via testcontainers. WithDirectIP avoids Docker DNAT which
	// confuses FDB's canonicalRemotePort tracking in the C binding (used by
	// the Python tester), triggering assertion spam at FlowTransport.actor.cpp.
	container, err := foundationdb.Run(
		ctx, "foundationdb/foundationdb:"+testedFDBVersion,
		foundationdb.WithDirectIP(),
		foundationdb.WithStorageEngine("memory"),
	)
	if err != nil {
		r.Error = "container start failed: " + err.Error()
		return r
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		collectLogs(cleanupCtx, container, seedDir)
		if err := foundationdb.Terminate(cleanupCtx, container); err != nil {
			fmt.Fprintf(os.Stderr, "seed %d: %v\n", seed, err)
			r.Pass = false
			if r.Error != "" {
				r.Error += "; "
			}
			r.Error += err.Error()
		}
	}()

	// Get cluster file with direct IP for the Python tester.
	clusterContent, err := container.ClusterFile(ctx)
	if err != nil {
		r.Error = "get cluster file: " + err.Error()
		return r
	}

	clusterFile, err := filepath.Abs(filepath.Join(seedDir, "fdb.cluster"))
	if err != nil {
		r.Error = "abs cluster file path: " + err.Error()
		return r
	}
	if err := os.WriteFile(clusterFile, []byte(clusterContent), 0o644); err != nil {
		r.Error = "write cluster file: " + err.Error()
		return r
	}

	// Run bindingtester.
	args := testerArgs(seed, w, clusterFile, btRunDir)
	r.Command = append([]string{"python3"}, args...)
	cmd := exec.CommandContext(ctx, "python3", args...)
	configureTesterProcess(cmd)
	defer killTesterGroup(cmd)
	cmd.Dir = btRunDir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+btRunDir)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()

	output := buf.String()
	if writeErr := os.WriteFile(filepath.Join(seedDir, "tester.log"), []byte(output), 0o644); writeErr != nil {
		r.Error = "save tester output: " + writeErr.Error()
		return r
	}

	r.FDBAlive = isFDBAlive(ctx, container)
	r.Pass = successfulVerdict(output, err) && r.FDBAlive

	if !r.Pass && r.Error == "" {
		r.Error = summarizeFailure(output, err)
		if !r.FDBAlive {
			r.Error += "; FDB liveness check failed"
		}
	}

	return r
}

// The Python harness spawns both bindings. Killing just its PID leaves those
// children holding output pipes open and can make CommandContext wait forever.
func configureTesterProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killTesterGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
}

func killTesterGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// maxTailLines bounds how much of the tester's output a failure summary carries.
// A Python traceback's cause is on its last line; three lines also catch the
// frame that raised it without turning the console into a log dump.
const maxTailLines = 3

// summarizeFailure turns a failed seed into one console-readable line.
//
// The tester's own verdict line wins when it exists. Otherwise the tail of its
// output is reported, because the interesting failures are the ones where the
// tester never reached a verdict: reporting only the exec error there says
// "exit status 1" and nothing else. That is not a hypothetical — a fleet-wide
// missing Python fdb module was reported as 50 identical "exit status 1" lines,
// with the traceback naming the cause sitting unread in the per-seed log.
func summarizeFailure(output string, runErr error) string {
	var tail []string
	verdict := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Last verdict wins: the tester prints one per test it ran.
		if strings.Contains(line, "incorrect result") {
			verdict = line
		}
		tail = append(tail, line)
		if len(tail) > maxTailLines {
			tail = tail[1:]
		}
	}
	if verdict != "" {
		return verdict
	}

	summary := strings.Join(tail, " | ")
	switch {
	case summary != "" && runErr != nil:
		return runErr.Error() + ": " + summary
	case summary != "":
		return summary
	case runErr != nil:
		return runErr.Error()
	default:
		return "no result line in output"
	}
}

func collectLogs(ctx context.Context, container *foundationdb.Container, seedDir string) {
	containerID := container.GetContainerID()

	// FDB trace logs.
	exec.CommandContext(ctx, "docker", "cp",
		containerID+":/var/fdb/logs", filepath.Join(seedDir, "fdb-logs")).Run()

	// Docker container logs.
	logReader, err := container.Logs(ctx)
	if err == nil {
		data, _ := io.ReadAll(logReader)
		logReader.Close()
		os.WriteFile(filepath.Join(seedDir, "docker.log"), data, 0o644)
	}
}

func isFDBAlive(ctx context.Context, container *foundationdb.Container) bool {
	output, err := container.FDBCLIExec(ctx, "status json")
	return databaseAvailable(output, err)
}

func databaseAvailable(output string, err error) bool {
	if err != nil {
		return false
	}
	var status struct {
		Client struct {
			DatabaseStatus struct {
				Available bool `json:"available"`
			} `json:"database_status"`
		} `json:"client"`
	}
	return json.Unmarshal([]byte(output), &status) == nil && status.Client.DatabaseStatus.Available
}

func setup() (btRunDir string) {
	// When run via `bazelisk run`, cwd is the sandbox. Use workspace dir.
	wsDir := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if wsDir != "" {
		if err := os.Chdir(wsDir); err != nil {
			log.Fatalf("change to workspace: %v", err)
		}
	}

	fmt.Println("Building stacktester...")
	cmd := exec.Command("bazelisk", "build", "--config=ci",
		"//cmd/fdb-stacktester:fdb-stacktester",
		"//cmd/fdb-binding-stress:python_fdb_tar")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("bazelisk build failed: %v", err)
	}

	stacktester, err := filepath.Abs("bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester")
	if err != nil {
		log.Fatalf("resolve stacktester path: %v", err)
	}
	// Resolve symlinks to get the stable cache path. The bazel-bin/ symlink
	// gets recreated on every Bazel invocation, so if a concurrent build runs
	// (e.g., pre-commit hook), the symlink points to a new directory and the
	// old path breaks. EvalSymlinks resolves to the actual file in the Bazel
	// cache which survives across builds.
	stacktester, err = filepath.EvalSymlinks(stacktester)
	if err != nil {
		log.Fatalf("resolve stacktester symlinks: %v", err)
	}
	if _, err := os.Stat(stacktester); err != nil {
		log.Fatalf("stacktester not found at %s", stacktester)
	}

	btRunDir, err = os.MkdirTemp("", "fdb-binding-harness-")
	if err != nil {
		log.Fatalf("create private harness directory: %v", err)
	}
	btDir, err := findBindingTesterDir()
	if err != nil {
		log.Fatalf("find bindingtester: %v", err)
	}

	os.MkdirAll(filepath.Join(btRunDir, "bindingtester"), 0o755)
	cp := exec.Command("cp", "-r", btDir+"/.", filepath.Join(btRunDir, "bindingtester/"))
	if out, err := cp.CombinedOutput(); err != nil {
		log.Fatalf("copy bindingtester: %s: %v", out, err)
	}

	// Patch Python imports for standalone use.
	initPy := filepath.Join(btRunDir, "bindingtester/__init__.py")
	data, _ := os.ReadFile(initPy)
	s := string(data)
	s = strings.ReplaceAll(s, "sys.path[:0]", "# patched")
	s = strings.ReplaceAll(s, "import util", "from bindingtester import util")
	s = strings.ReplaceAll(s, "from fdb import LATEST_API_VERSION", "LATEST_API_VERSION = 730")
	os.WriteFile(initPy, []byte(s), 0o644)

	// The oracle stack machine is upstream test code, not part of the Python sdist.
	pythonTests := filepath.Join(filepath.Dir(btDir), "python", "tests")
	if out, err := exec.Command("cp", "-r", pythonTests, filepath.Join(btRunDir, "python")).CombinedOutput(); err != nil {
		log.Fatalf("copy Python oracle: %s: %v", out, err)
	}
	known := filepath.Join(btRunDir, "bindingtester", "known_testers.py")
	f, err := os.OpenFile(known, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		log.Fatal(err)
	}
	_, err = fmt.Fprintf(f, "\ntesters['python'] = Tester('python', 'python3 %s', 2040, 23, 730, types=ALL_TYPES, tenants_enabled=True)\ntesters['pure_go'] = Tester('pure_go', '%s', 2040, 510, 730, types=ALL_TYPES)\n", filepath.Join(btRunDir, "python", "tester.py"), stacktester)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		log.Fatal(err)
	}

	installPythonFDB(btRunDir)
	preflightPythonFDB(btRunDir)

	fmt.Fprintf(os.Stderr, "Stacktester: %s\nHarness: %s\n\n", stacktester, btRunDir)
	return btRunDir
}

// installPythonFDB unpacks the pinned FDB Python client into the harness dir,
// which is the PYTHONPATH the tester runs under. The client comes from Bazel
// rather than from the host so the harness runs identically on a fresh CI box
// and a developer laptop, and so the binding under test is version-locked to
// the pinned C client instead of to whatever `pip install foundationdb` last
// left behind.
func installPythonFDB(btRunDir string) {
	tar, err := filepath.Abs("bazel-bin/cmd/fdb-binding-stress/python-fdb.tar")
	if err != nil {
		log.Fatalf("resolve python fdb tar path: %v", err)
	}
	// See the stacktester resolution above for why the symlink is resolved.
	tar, err = filepath.EvalSymlinks(tar)
	if err != nil {
		log.Fatalf("resolve python fdb tar symlinks: %v", err)
	}

	// Refuse to mix a partial install with the pinned archive.
	if err := os.RemoveAll(filepath.Join(btRunDir, "fdb")); err != nil {
		log.Fatalf("remove stale python fdb: %v", err)
	}
	untar := exec.Command("tar", "xf", tar, "-C", btRunDir)
	if out, err := untar.CombinedOutput(); err != nil {
		log.Fatalf("unpack python fdb (%s): %s: %v", tar, out, err)
	}
}

// preflightPythonProgram is the probe preflightPythonFDB runs in the tester's Python.
const preflightPythonProgram = `
import fdb
if not getattr(fdb, "__file__", None):
    raise SystemExit(
        "'import fdb' resolved to an implicit namespace package (__file__ is None): "
        "the directory exists but has no importable __init__.py, so the package is empty")
fdb.api_version
print(fdb.__file__)
`

// preflightPythonFDB fails the run before any container starts if the tester
// cannot import its client. Without this the import error surfaces once per
// seed, as an opaque exec status, after minutes of spinning up FDB containers
// that never get used.
//
// A bare `import fdb` is not enough to establish that, and believing it was cost
// a fleet-wide lane. When `fdb/` unpacks without an importable `__init__.py`,
// Python does not fail: it resolves the name to an implicit namespace package,
// so the import succeeds, `__file__` is None, and the package is empty. The
// preflight therefore has to touch something the real client defines —
// api_version is the first thing the tester itself calls — and reject the
// file-less namespace case explicitly.
func preflightPythonFDB(btRunDir string) {
	cmd := exec.Command("python3", "-c", preflightPythonProgram)
	cmd.Dir = btRunDir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+btRunDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("the binding tester's Python client is not importable — every seed "+
			"would fail with an opaque exec status.\npython3 -c 'import fdb' under %s said:\n%s\n"+
			"(the client is unpacked from //cmd/fdb-binding-stress:python_fdb_tar; it also "+
			"needs libfdb_c on the system loader path)", btRunDir, out)
	}
	fmt.Fprintf(os.Stderr, "Python FDB client: %s", out)
}

func findBindingTesterDir() (string, error) {
	cmd := exec.Command("bazelisk", "info", "--config=ci", "output_base")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bazelisk info: %w", err)
	}
	base := strings.TrimSpace(string(out))
	dir := filepath.Join(base, "external/foundationdb+/bindings/bindingtester")
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, nil
	}
	return "", fmt.Errorf("bindingtester dir not found (run bazelisk build first)")
}

func writeJSON(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		log.Fatal(err)
	}
}
