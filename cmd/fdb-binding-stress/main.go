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
//	fdb-binding-stress -seeds 1 -ops 1000 -seed-start 146    # reproduce one seed
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	foundationdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

const perSeedTimeout = 5 * time.Minute

// SeedResult is the per-seed result written to the JSON report.
type SeedResult struct {
	Seed     int     `json:"seed"`
	Pass     bool    `json:"pass"`
	FDBAlive bool    `json:"fdb_alive"`
	Duration float64 `json:"duration_s"`
	Error    string  `json:"error,omitempty"`
}

// Report is the top-level JSON report.
type Report struct {
	StartedAt string       `json:"started_at"`
	Finished  string       `json:"finished_at"`
	Elapsed   string       `json:"elapsed"`
	Ops       int          `json:"ops_per_seed"`
	Total     int          `json:"total_seeds"`
	Pass      int          `json:"pass"`
	Fail      int          `json:"fail"`
	FDBDeaths int          `json:"fdb_deaths"`
	Seeds     []SeedResult `json:"seeds"`
}

func main() {
	seeds := flag.Int("seeds", 100, "number of seeds to run (0 = use -duration)")
	ops := flag.Int("ops", 1000, "operations per seed")
	duration := flag.Duration("duration", 0, "run for this long (e.g. 2h); overrides -seeds")
	seedStart := flag.Int("seed-start", 1, "first seed number")
	outDir := flag.String("out", "", "output directory (default: binding-stress-out/<timestamp>)")
	testName := flag.String("test-name", "api", "binding tester test name (api, directory, directory_hca)")
	flag.Parse()

	stacktester, btRunDir := setup()

	// Create timestamped output directory.
	if *outDir == "" {
		*outDir = filepath.Join("binding-stress-out", time.Now().Format("20060102-150405"))
	}
	os.MkdirAll(*outDir, 0o755)

	report := Report{
		StartedAt: time.Now().Format(time.RFC3339),
		Ops:       *ops,
	}

	startTime := time.Now()

	if *duration > 0 {
		fmt.Printf("binding-stress: timed run, %d ops/seed, duration=%s\n", *ops, *duration)
	} else {
		fmt.Printf("binding-stress: %d seeds × %d ops, start=%d\n", *seeds, *ops, *seedStart)
	}
	fmt.Printf("Output: %s/\n\n", *outDir)

	pass, fail, dead := 0, 0, 0
	seed := *seedStart - 1

	for {
		seed++
		if *duration > 0 {
			if time.Since(startTime) >= *duration {
				break
			}
		} else if seed >= *seedStart+*seeds {
			break
		}

		seedDir := filepath.Join(*outDir, fmt.Sprintf("seed-%d", seed))
		os.MkdirAll(seedDir, 0o755)

		t0 := time.Now()
		r := runSeed(seed, *ops, *testName, stacktester, btRunDir, seedDir)
		r.Duration = time.Since(t0).Seconds()

		report.Seeds = append(report.Seeds, r)

		if r.Pass {
			pass++
			if !r.FDBAlive {
				dead++
				fmt.Printf("WARN seed %d: PASS but FDB=DEAD (%.1fs)\n", seed, r.Duration)
			}
		} else {
			fail++
			if !r.FDBAlive {
				dead++
			}
			alive := "ALIVE"
			if !r.FDBAlive {
				alive = "DEAD"
			}
			errMsg := r.Error
			if errMsg == "" {
				errMsg = "TIMEOUT/HANG"
			}
			fmt.Printf("FAIL seed %d: %s (FDB=%s, %.1fs)\n", seed, errMsg, alive, r.Duration)
		}

		if seed%10 == 0 {
			elapsed := time.Since(startTime)
			if *duration > 0 {
				remaining := *duration - elapsed
				fmt.Printf("--- seed %d (pass=%d fail=%d dead=%d) %s remaining ---\n",
					seed, pass, fail, dead, remaining.Truncate(time.Second))
			} else {
				fmt.Printf("--- seed %d/%d (pass=%d fail=%d dead=%d) %s elapsed ---\n",
					seed, *seedStart+*seeds-1, pass, fail, dead, elapsed.Truncate(time.Second))
			}
		}

		// Write JSON report after every seed (crash-safe).
		report.Total = pass + fail
		report.Pass = pass
		report.Fail = fail
		report.FDBDeaths = dead
		report.Finished = time.Now().Format(time.RFC3339)
		report.Elapsed = time.Since(startTime).Truncate(time.Second).String()
		writeJSON(filepath.Join(*outDir, "report.json"), report)
	}

	// Final summary.
	total := pass + fail
	fmt.Printf("\nFinished: %s (elapsed: %s)\n", time.Now().Format(time.RFC3339),
		time.Since(startTime).Truncate(time.Second))
	fmt.Println("=========================================")
	fmt.Printf("binding-stress: %d/%d pass, %d fail, %d FDB deaths (%d ops/seed)\n",
		pass, total, fail, dead, *ops)
	if fail > 0 {
		var failSeeds []string
		for _, r := range report.Seeds {
			if !r.Pass {
				failSeeds = append(failSeeds, fmt.Sprintf("%d", r.Seed))
			}
		}
		fmt.Printf("Failed seeds: %s\n", strings.Join(failSeeds, ", "))
	}
	fmt.Printf("Output: %s/\n", *outDir)
	fmt.Println("=========================================")

	if fail > 0 {
		os.Exit(1)
	}
}

func runSeed(seed, ops int, testName, stacktester, btRunDir, seedDir string) SeedResult {
	r := SeedResult{Seed: seed}

	// Start FDB via testcontainers. WithDirectIP avoids Docker DNAT which
	// confuses FDB's canonicalRemotePort tracking in the C binding (used by
	// the Python tester), triggering assertion spam at FlowTransport.actor.cpp.
	ctx, cancel := context.WithTimeout(context.Background(), perSeedTimeout)
	defer cancel()

	container, err := foundationdb.Run(ctx, "",
		foundationdb.WithDirectIP(),
		foundationdb.WithStorageEngine("memory"),
	)
	if err != nil {
		r.Error = "container start failed: " + err.Error()
		return r
	}
	defer func() {
		collectLogs(ctx, container, seedDir)
		container.Terminate(ctx)
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
	cmd := exec.CommandContext(ctx, "python3",
		filepath.Join(btRunDir, "bindingtester/bindingtester.py"),
		"--cluster-file", clusterFile,
		"--test-name", testName,
		"--api-version", "730",
		"--num-ops", fmt.Sprintf("%d", ops),
		"--seed", fmt.Sprintf("%d", seed),
		"--timeout", fmt.Sprintf("%d", int(perSeedTimeout.Seconds())),
		"--no-threads",
		"--no-tenants",
		stacktester,
	)
	cmd.Dir = btRunDir
	cmd.Env = append(os.Environ(), "PYTHONPATH="+btRunDir)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()

	output := buf.String()
	os.WriteFile(filepath.Join(seedDir, "tester.log"), []byte(output), 0o644)

	r.FDBAlive = isFDBAlive(ctx, container)
	r.Pass = err == nil && strings.Contains(output, "had 0 incorrect result(s) and 0 error(s)")

	if !r.Pass && r.Error == "" {
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "incorrect result") {
				r.Error = strings.TrimSpace(line)
			}
		}
		if r.Error == "" {
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Error = "no result line in output"
			}
		}
	}

	return r
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
	_, err := container.FDBCLIExec(ctx, "status")
	return err == nil
}

func setup() (stacktester string, btRunDir string) {
	// When run via `bazelisk run`, cwd is the sandbox. Use workspace dir.
	wsDir := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if wsDir != "" {
		os.Chdir(wsDir)
	}

	fmt.Println("Building stacktester...")
	cmd := exec.Command("bazelisk", "build", "//cmd/fdb-stacktester:fdb-stacktester")
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

	btRunDir = "/tmp/bt-run"
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

	fmt.Fprintf(os.Stderr, "Stacktester: %s\nHarness: %s\n\n", stacktester, btRunDir)
	return stacktester, btRunDir
}

func findBindingTesterDir() (string, error) {
	cmd := exec.Command("bazelisk", "info", "output_base")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bazelisk info: %w", err)
	}
	base := strings.TrimSpace(string(out))
	dir := filepath.Join(base, "external/foundationdb+/bindings/bindingtester")
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, nil
	}
	matches, _ := filepath.Glob(os.Getenv("HOME") + "/.cache/bazel/_bazel_*/*/external/foundationdb+/bindings/bindingtester")
	if len(matches) > 0 {
		return matches[0], nil
	}
	return "", fmt.Errorf("bindingtester dir not found (run bazelisk build first)")
}

func writeJSON(path string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	os.WriteFile(path, data, 0o644)
}
