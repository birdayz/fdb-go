package foundationdb

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// TestWithProcessCount_Single verifies that the default (1 process) works.
func TestWithProcessCount_Single(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "",
		WithStorageEngine("ssd"),
		WithDirectIP(),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	count, err := container.countProcesses(ctx)
	if err != nil {
		t.Fatalf("count processes: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 fdbserver process, got %d", count)
	}

	// Verify database is available.
	output, err := container.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		t.Fatalf("fdbcli: %v", err)
	}
	if !strings.Contains(output, "available") && !strings.Contains(output, "Healthy") {
		t.Fatalf("expected available/Healthy, got: %s", output)
	}
}

// TestWithProcessCount_Three verifies that 3 fdbserver processes start
// and all join the cluster. Validates via both OS-level ps and FDB-level
// status to confirm processes are visible to the cluster coordinator.
func TestWithProcessCount_Three(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := Run(ctx, "",
		WithStorageEngine("ssd"),
		WithDirectIP(),
		WithProcessCount(3),
		WithRedundancyMode("double"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// OS-level: verify 3 fdbserver processes are running.
	osCount, err := container.countProcesses(ctx)
	if err != nil {
		t.Fatalf("count processes: %v", err)
	}
	if osCount != 3 {
		t.Fatalf("OS: expected 3 fdbserver processes, got %d", osCount)
	}

	// FDB-level: verify cluster sees all 3 processes via status.
	output, err := container.FDBCLIExec(ctx, "status minimal")
	if err != nil {
		t.Fatalf("fdbcli: %v", err)
	}
	if !strings.Contains(output, "available") && !strings.Contains(output, "Healthy") {
		t.Fatalf("expected available/Healthy, got: %s", output)
	}

	// Parse "status details" to count FDB-visible processes.
	details, err := container.FDBCLIExec(ctx, "status details")
	if err != nil {
		t.Fatalf("status details: %v", err)
	}
	// Count lines matching process entries (IP:port pattern in process list).
	fdbProcesses := 0
	for _, line := range strings.Split(details, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, container.containerIP+":") {
			fdbProcesses++
		}
	}
	if fdbProcesses < 3 {
		t.Errorf("FDB status shows %d processes, expected 3", fdbProcesses)
	}
	t.Logf("OS processes: %d, FDB-visible processes: %d", osCount, fdbProcesses)
	t.Logf("status details:\n%s", details)
}

// TestWithKnob_AppliedToProcess verifies that knobs appear in fdbserver args.
func TestWithKnob_AppliedToProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := Run(ctx, "",
		WithStorageEngine("ssd"),
		WithDirectIP(),
		WithKnob("min_shard_bytes", "40000"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// Check that the knob appears in the fdbserver process args.
	_, reader, err := container.Exec(ctx, []string{"ps", "aux"}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	out, _ := io.ReadAll(reader)
	if !strings.Contains(string(out), "knob_min_shard_bytes") {
		t.Fatalf("knob not found in process args:\n%s", out)
	}
	t.Logf("knob found in fdbserver args")
}

// TestWithKnob_AppliedToAllProcesses verifies that knobs are applied
// to additional processes started by WithProcessCount.
func TestWithKnob_AppliedToAllProcesses(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := Run(ctx, "",
		WithStorageEngine("ssd"),
		WithDirectIP(),
		WithProcessCount(2),
		WithRedundancyMode("double"),
		WithKnob("min_shard_bytes", "40000"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer container.Terminate(ctx)

	// Poll the per-process knob check instead of taking a single snapshot. The
	// secondary fdbserver is started UNSUPERVISED (a backgrounded shell in
	// startAdditionalProcesses, not fdbmonitor), and Run() confirms the process count
	// only ONCE before issuing `configure double` (which triggers a cluster recovery).
	// On a contended coverage box the secondary can be momentarily absent during that
	// recovery window, so a one-shot pgrep flaked (nightly coverage). Poll until both
	// processes report the knob; if it never settles within the deadline, THAT is a
	// real "secondary process died" bug and fails loudly.
	deadline := time.Now().Add(30 * time.Second)
	var knobCount int
	var lastOut []byte
	for {
		// pgrep for actual fdbserver PIDs, then read /proc/PID/cmdline for the knob.
		_, reader, err := container.Exec(ctx, []string{
			"/bin/bash", "-c",
			`pgrep fdbserver | while read pid; do tr '\0' ' ' < /proc/$pid/cmdline; echo; done`,
		},
			tcexec.Multiplexed())
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		lastOut, _ = io.ReadAll(reader)
		knobCount = 0
		for _, line := range strings.Split(strings.TrimSpace(string(lastOut)), "\n") {
			if strings.Contains(line, "knob_min_shard_bytes") {
				knobCount++
			}
		}
		if knobCount == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 fdbserver processes with knob within 30s, last saw %d:\n%s", knobCount, lastOut)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("knob applied to all %d processes", knobCount)
}

// TestWithProcessCount_Invalid verifies validation.
func TestWithProcessCount_Invalid(t *testing.T) {
	t.Parallel()

	opt := WithProcessCount(0)
	cfg := defaultOptions()
	if err := opt.apply(&cfg); err == nil {
		t.Error("expected error for processCount=0")
	}

	opt = WithProcessCount(11)
	cfg = defaultOptions()
	if err := opt.apply(&cfg); err == nil {
		t.Error("expected error for processCount=11")
	}
}

// TestWithKnob_InvalidName verifies knob name validation.
func TestWithKnob_InvalidName(t *testing.T) {
	t.Parallel()

	opt := WithKnob("invalid-name", "100")
	cfg := defaultOptions()
	if err := opt.apply(&cfg); err == nil {
		t.Error("expected error for hyphenated knob name")
	}

	opt = WithKnob("name; rm -rf /", "100")
	cfg = defaultOptions()
	if err := opt.apply(&cfg); err == nil {
		t.Error("expected error for shell injection in knob name")
	}
}
