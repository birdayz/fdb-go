package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The profiling tests start the process-wide CPU profiler and execution
// tracer, so they must not run in parallel with each other.

func profileArgs(dir string) []string {
	return []string{
		"--cpuprofile=" + filepath.Join(dir, "cpu.pprof"),
		"--memprofile=" + filepath.Join(dir, "heap.pprof"),
		"--blockprofile=" + filepath.Join(dir, "block.pprof"),
		"--mutexprofile=" + filepath.Join(dir, "mutex.pprof"),
		"--trace=" + filepath.Join(dir, "trace.out"),
	}
}

// requireProfiles checks that every profile file was written: pprof profiles
// are gzipped protocol buffers, an execution trace starts with its header.
func requireProfiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"cpu.pprof", "heap.pprof", "heap.pprof.start", "block.pprof", "mutex.pprof"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
			t.Fatalf("%s is not a gzipped profile (%d bytes, %v)", name, len(b), err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "trace.out"))
	if err != nil || !bytes.HasPrefix(b, []byte("go 1.")) {
		t.Fatalf("trace.out is not an execution trace (%d bytes, %v)", len(b), err)
	}
}

func TestExecuteProfilesWholeRun(t *testing.T) {
	dir := t.TempDir()
	backend := &fakeBackend{t: t}
	var stdout, stderr bytes.Buffer
	args := append(cliArgs("ingest"), profileArgs(dir)...)
	if code := execute(args, &stdout, &stderr, backend.open); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if e := decodeEvidence(t, stdout.Bytes()); e.Error != "" {
		t.Fatalf("evidence error %q", e.Error)
	}
	requireProfiles(t, dir)
}

func TestExecuteProfileWindowNeverOpened(t *testing.T) {
	dir := t.TempDir()
	backend := &fakeBackend{t: t}
	var stdout, stderr bytes.Buffer
	args := append(cliArgs("ingest"), "--cpuprofile="+filepath.Join(dir, "cpu.pprof"), "--profile-start=1h")
	if code := execute(args, &stdout, &stderr, backend.open); code != 1 {
		t.Fatalf("exit=%d, want 1 (stderr %q)", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "profiling window never opened") {
		t.Fatalf("stderr %q", &stderr)
	}
	// The run's evidence is still complete.
	if e := decodeEvidence(t, stdout.Bytes()); e.Error != "" || len(backend.calls) == 0 {
		t.Fatalf("evidence %+v calls %v", e, backend.calls)
	}
}

func TestExecuteProfileFlagsRejectedBeforeRun(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.pprof")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string][]string{
		"negative start":    {"--cpuprofile=" + filepath.Join(dir, "a.pprof"), "--profile-start=-1s"},
		"negative duration": {"--cpuprofile=" + filepath.Join(dir, "b.pprof"), "--profile-duration=-1s"},
		"zero block rate":   {"--blockprofile=" + filepath.Join(dir, "c.pprof"), "--blockprofilerate=0"},
		"zero mutex rate":   {"--mutexprofile=" + filepath.Join(dir, "d.pprof"), "--mutexprofilefraction=0"},
		"existing file":     {"--cpuprofile=" + existing, "--memprofile=" + filepath.Join(dir, "partial.pprof")},
		"missing directory": {"--trace=" + filepath.Join(dir, "missing", "trace.out")},
		"existing output":   {"--output=" + existing, "--cpuprofile=" + filepath.Join(dir, "e.pprof")},
	} {
		t.Run(name, func(t *testing.T) {
			backend := &fakeBackend{t: t}
			var stdout, stderr bytes.Buffer
			if code := execute(append(cliArgs("ingest"), extra...), &stdout, &stderr, backend.open); code != 2 || stderr.Len() == 0 {
				t.Fatalf("exit=%d stderr=%q", code, &stderr)
			}
			if len(backend.calls) != 0 || stdout.Len() != 0 {
				t.Fatalf("rejected profiling flags still ran: calls %v", backend.calls)
			}
		})
	}
	if b, err := os.ReadFile(existing); err != nil || string(b) != "keep" {
		t.Fatalf("existing file changed: %q %v", b, err)
	}
	// A rejected run leaves no partial profile behind.
	for _, name := range []string{"a.pprof", "b.pprof", "c.pprof", "d.pprof", "e.pprof", "partial.pprof", "partial.pprof.start"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s created by a rejected run: %v", name, err)
		}
	}
}

// A bounded window closes on its own; ending the run later changes nothing.
func TestProfilerWindowDuration(t *testing.T) {
	dir := t.TempDir()
	cfg := profileFlags{blockRate: 10000, mutexFraction: 100, start: 10 * time.Millisecond, duration: 20 * time.Millisecond}
	cfg.cpu, cfg.mem, cfg.block, cfg.mutex, cfg.trace = filepath.Join(dir, "cpu.pprof"), filepath.Join(dir, "heap.pprof"), filepath.Join(dir, "block.pprof"), filepath.Join(dir, "mutex.pprof"), filepath.Join(dir, "trace.out")
	p, err := newProfiler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.begin()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("window did not close after --profile-duration")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	requireProfiles(t, dir)
}

// collector-bench profiles its steady phase by default: the window opens at
// --warmup, so a run that ends sooner reports it.
func TestExecuteCollectorBenchProfileStartsAfterWarmup(t *testing.T) {
	stubCollectorBench(t, nil)
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := collectorBenchArgs("--duration=10m", "--warmup=45s", "--cpuprofile="+filepath.Join(dir, "cpu.pprof"))
	if code := execute(args, &stdout, &stderr, nil); code != 1 || !strings.Contains(stderr.String(), "--profile-start 45s") {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	// An explicit start overrides the default.
	stdout.Reset()
	stderr.Reset()
	dir = t.TempDir()
	args = collectorBenchArgs("--duration=10m", "--warmup=45s", "--profile-start=0s", "--cpuprofile="+filepath.Join(dir, "cpu.pprof"))
	if code := execute(args, &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, &stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cpu.pprof")); err != nil || len(b) < 2 || b[0] != 0x1f {
		t.Fatalf("cpu profile: %d bytes, %v", len(b), err)
	}
}
