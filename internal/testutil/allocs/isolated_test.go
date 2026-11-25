package allocs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChildInvocationIsExact(t *testing.T) {
	t.Parallel()
	valid := []string{"-test.run=^TestAllocation$", "-test.v", "-test.count=1", "-test.timeout=1m"}
	for _, test := range []struct {
		name     string
		selected string
		args     []string
		want     bool
	}{
		{"valid", "TestAllocation", valid, true},
		{"no marker", "", valid, false},
		{"not a test", "BenchmarkAllocation", valid, false},
		{"different name", "TestOther", valid, false},
		{"no filter", "TestAllocation", nil, false},
		{"broad filter", "TestAllocation", []string{"-test.run=Test", "-test.v", "-test.count=1", "-test.timeout=1m"}, false},
		{"no verbose output", "TestAllocation", []string{"-test.run=^TestAllocation$", "-test.v=false", "-test.count=1", "-test.timeout=1m"}, false},
		{"zero executions", "TestAllocation", []string{"-test.run=^TestAllocation$", "-test.v", "-test.count=0", "-test.timeout=1m"}, false},
		{"unbounded timeout", "TestAllocation", []string{"-test.run=^TestAllocation$", "-test.v", "-test.count=1", "-test.timeout=0"}, false},
		{"extra flag", "TestAllocation", append(append([]string(nil), valid...), "-test.shuffle=on"), false},
		{"subtest would broaden parent filter", "TestAllocation/a.b", []string{"-test.run=^TestAllocation/a\\.b$", "-test.v", "-test.count=1", "-test.timeout=1m"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isChildInvocation(test.selected, test.args); got != test.want {
				t.Fatalf("child invocation = %v, want %v", got, test.want)
			}
		})
	}
}

func TestChildEnvironmentRemovesParentTestControls(t *testing.T) {
	t.Parallel()
	got := childEnvironment([]string{
		"HOME=/home/test", "RUNFILES_DIR=/runfiles", "TEST_SHARD_INDEX=2",
		"TEST_TOTAL_SHARDS=4", "TESTBRIDGE_TEST_ONLY=TestOther", "XML_OUTPUT_FILE=parent.xml",
		"GO_TEST_WRAP=1", "GO_TEST_RUN_FROM_BAZEL=1", childTestEnv + "=TestOther",
		"TEST_SRCDIR=/runfiles", "TEST_WORKSPACE=project", "TEST_TMPDIR=/sandbox/tmp",
		"COVERAGE_OUTPUT_FILE=/parent/coverage.dat", "GOCOVERDIR=/parent/raw",
		"COVERAGE_DIR=/merged", "COVERAGE_MANIFEST=/manifest",
	}, "TestAllocation", "/child")
	want := []string{
		"HOME=/home/test", "RUNFILES_DIR=/runfiles",
		"TEST_SRCDIR=/runfiles", "TEST_WORKSPACE=project", "TEST_TMPDIR=/sandbox/tmp",
		"COVERAGE_OUTPUT_FILE=" + filepath.Join("/child", "coverage.dat"), "GOCOVERDIR=/child",
		"COVERAGE_DIR=/merged", "COVERAGE_MANIFEST=/manifest",
		"GO_TEST_WRAP=0", childTestEnv + "=TestAllocation",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child environment = %v, want %v", got, want)
	}
}

func TestChildCompletionRejectsEmptyGreens(t *testing.T) {
	t.Parallel()
	name := "TestAllocation"
	run, pass, marker := "=== RUN   "+name+"\n", "--- PASS: "+name+" (0.01s)\n", "ALLOCS_ISOLATED "+strconv.Quote(name)+"\n"
	for _, test := range []struct {
		name, output string
		want         bool
	}{
		{"complete", run + marker + pass, true},
		{"empty", "", false},
		{"empty green", "PASS\n", false},
		{"no run", marker + pass, false},
		{"no assertion", run + pass, false},
		{"no pass", run + marker, false},
		{"repeated assertion", run + marker + marker + pass, false},
		{"wrong test", "=== RUN   TestOther\nALLOCS_ISOLATED \"TestOther\"\n--- PASS: TestOther (0.01s)\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := childCompleted(name, test.output); got != test.want {
				t.Fatalf("completion = %v, want %v", got, test.want)
			}
		})
	}
}

// Go's Benchmark counts allocations in OTHER goroutines too. The handshake
// makes the reproducer deterministic: each measured iteration asks another
// goroutine to allocate a buffer and waits until that allocation has happened.
func TestBenchmarkIncludesOtherGoroutines(t *testing.T) {
	t.Parallel()
	requests, buffers := make(chan struct{}), make(chan []byte)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range requests {
			buffers <- make([]byte, 1024)
		}
	}()
	defer func() { close(requests); <-finished }()
	result := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			requests <- struct{}{}
			runtime.KeepAlive(<-buffers)
		}
	})
	if result.N == 0 || result.AllocsPerOp() < 1 {
		t.Fatalf("benchmark did not count the other goroutine's allocation: N=%d allocs=%d", result.N, result.AllocsPerOp())
	}
}

func TestRunIsolatesAllocationAssertions(t *testing.T) {
	t.Parallel()
	if !IsChildProcess() {
		stop, done := make(chan struct{}), make(chan struct{})
		var allocations atomic.Uint64
		go func() {
			defer close(done)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					// Escape each allocation through a channel rather than relying
					// on compiler decisions about a discarded scratch buffer.
					buffer := make(chan []byte, 1)
					buffer <- make([]byte, 1024)
					runtime.KeepAlive(<-buffer)
					allocations.Add(1)
				}
			}
		}()
		defer func() {
			close(stop)
			<-done
			if allocations.Load() == 0 {
				t.Fatal("parent allocation-noise control never ran")
			}
		}()
	}
	Run(t, func() {
		// The invocation check establishes isolation. The sparse parent noise
		// is a live control, not a quantitative detector: per-operation
		// allocation rounding can hide it in a large no-op benchmark.
		if !IsChildProcess() {
			t.Fatal("allocation assertion ran alongside its parent tests")
		}
		measured := testing.Benchmark(func(b *testing.B) {
			for range b.N {
				runtime.Gosched()
			}
		})
		if measured.N == 0 || measured.AllocsPerOp() != 0 {
			t.Fatalf("isolated no-op benchmark allocated: N=%d allocs=%d", measured.N, measured.AllocsPerOp())
		}
	})
}

// A nested caller must fail before starting another process or measuring
// alongside parent setup. This also pins propagation of a real child failure.
func TestRunRejectsSubtests(t *testing.T) {
	t.Parallel()
	if os.Getenv(childTestEnv) != "" {
		if !IsChildProcess() {
			t.Fatal("invalid subtest-rejection child invocation")
		}
		t.Run("a.b", func(t *testing.T) {
			t.Parallel()
			Run(t, func() { t.Fatal("nested assertion must not run") })
		})
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=1", "-test.timeout=1m")
	cmd.Env = childEnvironment(os.Environ(), t.Name(), t.TempDir())
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "allocation assertions require a top-level test") ||
		!strings.Contains(string(output), "--- FAIL: "+t.Name()+"/a.b (") ||
		strings.Contains(string(output), "nested assertion must not run") || childCompleted(t.Name(), string(output)) {
		t.Fatalf("nested caller did not fail closed: %v\n%s", err, output)
	}
}

func TestRunRejectsInvalidChildInvocation(t *testing.T) {
	t.Parallel()
	if os.Getenv(childTestEnv) != "" {
		Run(t, func() { t.Fatal("invalid child must not measure") })
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	// The unexpected repetition must fail before Run can spawn again.
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+t.Name()+"$", "-test.v", "-test.count=2", "-test.timeout=1m")
	cmd.Env = childEnvironment(os.Environ(), t.Name(), t.TempDir())
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "invalid allocation child invocation") ||
		!strings.Contains(string(output), "--- FAIL: "+t.Name()+" (") ||
		strings.Contains(string(output), "invalid child must not measure") || childCompleted(t.Name(), string(output)) {
		t.Fatalf("malformed child did not fail before respawning: %v\n%s", err, output)
	}
}

func FuzzChildIsolationProtocol(f *testing.F) {
	f.Add("TestAllocation", "PASS\n")
	f.Add("TestAllocation/a.b", "")
	f.Fuzz(func(t *testing.T, name, noise string) {
		t.Parallel()
		args := []string{"-test.run=^" + regexp.QuoteMeta(name) + "$", "-test.v", "-test.count=1", "-test.timeout=1m"}
		if isChildInvocation(name, args) != (strings.HasPrefix(name, "Test") && !strings.Contains(name, "/")) {
			t.Fatal("exact filter and child selector disagree")
		}
		if isChildInvocation(name, append(args, "-test.shuffle=on")) {
			t.Fatal("a broader invocation bypassed normal TestMain setup")
		}
		valid := "=== RUN   " + name + "\nALLOCS_ISOLATED " + strconv.Quote(name) + "\n--- PASS: " + name + " (0.01s)\n"
		if !childCompleted(name, valid) {
			t.Fatal("complete child result rejected")
		}
		// Merely adding arbitrary output never supplies the absent assertion
		// marker: duplicate-marker output must also stay rejected.
		if childCompleted(name, valid+valid+noise) {
			t.Fatal("repeated child execution admitted")
		}
	})
}
