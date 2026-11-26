// Package allocs isolates allocation assertions from parallel test activity.
package allocs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const childTestEnv = "FDB_GO_ALLOCATION_TEST"

// IsChildProcess reports the exact subprocess invocation made by Run. TestMain
// may use this to avoid starting background services for an allocation-only
// test. An environment marker without the matching test filter is insufficient.
func IsChildProcess() bool {
	return isChildInvocation(os.Getenv(childTestEnv), os.Args[1:])
}

func isChildInvocation(name string, args []string) bool {
	return isTopLevelTest(name) && len(args) == 4 &&
		args[0] == "-test.run=^"+regexp.QuoteMeta(name)+"$" &&
		args[1] == "-test.v" && args[2] == "-test.count=1" && args[3] == "-test.timeout=1m"
}

func isTopLevelTest(name string) bool {
	return strings.HasPrefix(name, "Test") && !strings.Contains(name, "/")
}

// Run executes measure in a fresh copy of the test executable containing only
// this top-level test. Subtests are rejected: their parent setup can start
// concurrent work, and Go splits -test.run patterns on slashes rather than
// matching the full name. Call it from a parallel test, but do not start background work in
// measure: testing.Benchmark reads process-wide MemStats, not per-goroutine
// allocation counters. Parent tests stay parallel; their allocations cannot
// enter the child's measurement. The original assertion and threshold run in
// the child and any failure, timeout, or missing execution marker fails here.
func Run(t *testing.T, measure func()) {
	t.Helper()
	if !isTopLevelTest(t.Name()) {
		t.Fatal("allocation assertions require a top-level test")
	}
	if os.Getenv(childTestEnv) != "" && !IsChildProcess() {
		t.Fatal("invalid allocation child invocation")
	}
	if IsChildProcess() {
		if os.Getenv(childTestEnv) != t.Name() {
			t.Fatalf("allocation child selected %q, entered %q", os.Getenv(childTestEnv), t.Name())
		}
		measure()
		t.Log("ALLOCS_ISOLATED " + strconv.Quote(t.Name()))
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v", "-test.count=1", "-test.timeout=1m")
	cmd.Env = childEnvironment(os.Environ(), t.Name(), t.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated allocation test failed: %v\n%s", err, output)
	}
	if !childCompleted(t.Name(), string(output)) {
		t.Fatalf("allocation subprocess did not execute and pass the requested assertion:\n%s", output)
	}
	t.Logf("isolated allocation assertion:\n%s", output)
}

func childEnvironment(environment []string, name, outputDir string) []string {
	out := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "TEST_SRCDIR", "TEST_WORKSPACE", "TEST_TMPDIR":
			out = append(out, entry)
			continue
		case "COVERAGE_OUTPUT_FILE":
			// rules_go writes this profile directly. The child must not
			// overwrite its parent's profile. COVERAGE_DIR stays shared:
			// rules_go emits uniquely named LCOV files there for Bazel to merge.
			out = append(out, key+"="+filepath.Join(outputDir, "coverage.dat"))
			continue
		case "GOCOVERDIR":
			out = append(out, key+"="+outputDir)
			continue
		}
		// Do not let Bazel shard, re-filter, wrap, or overwrite the parent
		// XML report from this child. Preserve ordinary settings (including
		// runfiles paths) that the test executable may need to initialize.
		if strings.HasPrefix(key, "TEST_") || key == "TESTBRIDGE_TEST_ONLY" ||
			key == "XML_OUTPUT_FILE" || strings.HasPrefix(key, "GO_TEST_") || key == childTestEnv {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GO_TEST_WRAP=0", childTestEnv+"="+name)
}

func childCompleted(name, output string) bool {
	return strings.Contains(output, "=== RUN   "+name+"\n") &&
		strings.Contains(output, "--- PASS: "+name+" (") &&
		strings.Count(output, "ALLOCS_ISOLATED "+strconv.Quote(name)) == 1
}
