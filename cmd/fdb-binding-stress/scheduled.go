package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A seed is reproducible only together with its generator settings and pinned harness.
type workload struct {
	Test        string `json:"test_name"`
	Ops         int    `json:"ops"`
	Concurrency int    `json:"concurrency"`
	Compare     bool   `json:"compare_python"`
	APIVersion  int    `json:"api_version"`
}

type seedCase struct {
	Seed     int
	Workload workload
}

func correctnessModes(ops int) []workload {
	return []workload{
		{"scripted", ops, 1, false, 730},
		{"api", ops, 1, true, 730},
		{"api", ops, 5, false, 730},
		{"directory", ops, 1, true, 730},
		{"directory_hca", 100, 5, false, 730},
	}
}

// Borrowed Rust regression seeds, not bugs found in this client; provenance is in
// docs/client-correctness.md. Use API 730 except the original HCA API 610 case.
func regressionCases() []seedCase {
	modes := correctnessModes(1000)
	cases := []seedCase{{3534790651, modes[1]}, {3864917676, modes[1]}, {3153055325, modes[2]}}
	for _, seed := range []int{2095856910, 2483220251, 241550211, 1508488514, 4203526853, 4142326254, 4275610547, 1951034301, 1700644945, 584458794} {
		cases = append(cases, seedCase{seed, modes[3]})
	}
	hca := modes[4]
	hca.APIVersion = 610
	return append(cases, seedCase{3179331380, hca})
}

const testedFDBVersion = "7.3.77"

func validateFDBVersion(version string) error {
	if version != "" && version != testedFDBVersion {
		return fmt.Errorf("binding correctness requires FDB_VERSION=%s, got %q", testedFDBVersion, version)
	}
	return nil
}

func validateRun(seeds, start int, duration time.Duration, scheduled bool, w workload) error {
	if err := validateFDBVersion(os.Getenv("FDB_VERSION")); err != nil {
		return err
	}
	if seeds <= 0 && duration == 0 {
		return fmt.Errorf("a positive seed count or duration is required")
	}
	if start < 1 || int64(start) > 0xffffffff || duration < 0 {
		return fmt.Errorf("seed-start must be in [1, 2^32-1] and duration nonnegative")
	}
	if duration == 0 && int64(start)+int64(seeds)-1 > 0xffffffff {
		return fmt.Errorf("seed range exceeds 32 bits")
	}
	if scheduled && duration <= 0 {
		return fmt.Errorf("scheduled mode requires a positive duration")
	}
	if w.Ops <= 0 || w.Concurrency < 1 || (w.Compare && w.Concurrency != 1) {
		return fmt.Errorf("positive ops/concurrency required; comparison cannot run concurrently")
	}
	switch w.Test {
	case "api", "scripted", "directory", "directory_hca":
	default:
		return fmt.Errorf("unknown test name %q", w.Test)
	}
	if w.APIVersion < 510 || w.APIVersion > 730 {
		return fmt.Errorf("API version must be between 510 and 730")
	}
	return nil
}

func testerArgs(seed int, w workload, cluster, harness string) []string {
	args := []string{
		filepath.Join(harness, "bindingtester/bindingtester.py"),
		"--cluster-file", cluster, "--test-name", w.Test, "--api-version", strconv.Itoa(w.APIVersion),
		"--num-ops", strconv.Itoa(w.Ops), "--seed", strconv.Itoa(seed),
		"--timeout", strconv.Itoa(int(perSeedTimeout.Seconds())), "--concurrency", strconv.Itoa(w.Concurrency), "--no-tenants",
	}
	if w.Test == "directory" || w.Test == "directory_hca" {
		args = append(args, "--no-directory-snapshot-ops")
	}
	if w.Compare {
		args = append(args, "--compare", "python")
	}
	return append(args, "pure_go")
}

func successfulVerdict(output string, err error) bool {
	if err != nil {
		return false
	}
	found := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "incorrect result(s)") {
			if !strings.Contains(line, "had 0 incorrect result(s) and 0 error(s)") {
				return false
			}
			found = true
		}
	}
	return found
}

func runVerdict(r Report, scheduled bool, regressionsDone bool) error {
	if r.Total == 0 {
		return fmt.Errorf("no completed cases")
	}
	if r.Fail > 0 || r.FDBDeaths > 0 {
		return fmt.Errorf("%d failures, %d FDB deaths", r.Fail, r.FDBDeaths)
	}
	if scheduled && (!regressionsDone || r.CompleteCycles == 0) {
		return fmt.Errorf("budget exhausted before regression corpus and one complete exploration cycle")
	}
	return nil
}

func executeRun(out string, seeds, start int, duration time.Duration, scheduled bool, w workload, harness string) error {
	return executeRunContext(context.Background(), out, seeds, start, duration, scheduled, w, harness)
}

func executeRunContext(parent context.Context, out string, seeds, start int, duration time.Duration, scheduled bool, w workload, harness string) error {
	revision, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("record revision: %w", err)
	}
	r := Report{Revision: strings.TrimSpace(string(revision)), FDBVersion: testedFDBVersion, Scheduled: scheduled, StartedAt: time.Now().Format(time.RFC3339), Ops: w.Ops}
	started := time.Now()
	ctx := parent
	cancel := func() {}
	if duration > 0 {
		ctx, cancel = context.WithTimeout(parent, duration)
	}
	defer cancel()
	persist := func() {
		r.Finished = time.Now().Format(time.RFC3339)
		r.Elapsed = time.Since(started).String()
		writeJSON(filepath.Join(out, "report.json"), r)
	}
	persist()
	run := func(seed int, mode workload) error {
		dir := filepath.Join(out, fmt.Sprintf("case-%05d-%s-seed-%d", r.Total, mode.Test, seed))
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		t0 := time.Now()
		result := runSeed(ctx, seed, mode, harness, dir)
		result.Duration = time.Since(t0).Seconds()
		r.Seeds = append(r.Seeds, result)
		r.Total++
		if result.Pass {
			r.Pass++
		} else {
			r.Fail++
		}
		if !result.FDBAlive {
			r.FDBDeaths++
		}
		fmt.Printf("%s seed=%d concurrency=%d compare=%v pass=%v (%.1fs) %s\n", mode.Test, seed, mode.Concurrency, mode.Compare, result.Pass, result.Duration, result.Error)
		persist()
		return nil
	}
	// Do not start work we cannot give its full per-case timeout. The outer
	// deadline still bounds all work, including startup and cleanup overhead.
	available := func() bool {
		return duration == 0 || time.Until(started.Add(duration)) >= perSeedTimeout+30*time.Second
	}
	cycles, regressionsDone, planErr := runPlan(seeds, start, duration > 0, scheduled, w, available, run)
	r.CompleteCycles = cycles
	err = planErr
	if err == nil {
		err = runVerdict(r, scheduled, regressionsDone)
	}
	if err != nil {
		r.Error = err.Error()
	}
	persist()
	fmt.Printf("binding-stress: %d/%d passed, cycles=%d; report=%s\n", r.Pass, r.Total, r.CompleteCycles, filepath.Join(out, "report.json"))
	return err
}

// Keep population selection independent of Docker and the clock so budget
// exhaustion, corpus replay and seed wraparound can be tested deterministically.
func runPlan(seeds, start int, timed, scheduled bool, w workload, available func() bool, run func(int, workload) error) (cycles int, regressionsDone bool, err error) {
	regressionsDone = !scheduled
	if scheduled {
		for _, c := range regressionCases() {
			if !available() {
				return cycles, false, nil
			}
			if err := run(c.Seed, c.Workload); err != nil {
				return cycles, false, err
			}
		}
		regressionsDone = true
	}
	modes := []workload{w}
	if scheduled {
		modes = correctnessModes(w.Ops)
	}
	for n := 0; available() && (timed || n < seeds); n++ {
		seed := int((int64(start)-1+int64(n))%0xffffffff) + 1
		for _, mode := range modes {
			if !available() {
				return cycles, regressionsDone, nil
			}
			if err := run(seed, mode); err != nil {
				return cycles, regressionsDone, err
			}
		}
		cycles++
	}
	return cycles, regressionsDone, nil
}
