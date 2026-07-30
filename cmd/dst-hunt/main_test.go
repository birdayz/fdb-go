package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/simfdb/hunt"
)

// TestRecorderWritesFinding proves the durable recording pipeline: a failing report becomes a
// well-formed findings.jsonl line with the seed, profile, violations, and a replay command —
// so an overnight run genuinely records every bug it finds, not just counts them.
func TestRecorderWritesFinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec, err := newRecorder(dir, "unit")
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}

	// A tiny lightweight profile so finding()'s internal Shrink/FaultDependent probes are cheap
	// (they run real hunts on the seed; the seed won't actually reproduce, which is fine — the
	// synthetic report's fields are what gets recorded).
	p := hunt.Profile{Name: "value", Cfg: hunt.Config{Metadata: hunt.ValueMetadata, NumOps: 4, MaxPKs: 6}}
	rep := &hunt.Report{Seed: 123, Ops: 3, FaultsFired: 2, Violations: []string{"synthetic: index drift at op 3"}}

	rec.finding(p, 123, p.Cfg, rep)
	rec.heartbeat(time.Now(), 10, 1, 7, 0, 10, "test")
	rec.close()

	data, err := os.ReadFile(filepath.Join(dir, "findings.jsonl"))
	if err != nil {
		t.Fatalf("read findings.jsonl: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("findings.jsonl: got %d lines, want 1", len(lines))
	}
	var f findingLine
	if err := json.Unmarshal(lines[0], &f); err != nil {
		t.Fatalf("unmarshal finding: %v\nline: %s", err, lines[0])
	}
	if f.Seed != 123 || f.Profile != "value" || f.FailedAtOp != 3 || f.FaultsFired != 2 {
		t.Fatalf("finding fields wrong: %+v", f)
	}
	if len(f.Violations) != 1 || f.Violations[0] != "synthetic: index drift at op 3" {
		t.Fatalf("finding violations wrong: %v", f.Violations)
	}
	if !strings.Contains(f.Reproduce, "HUNT_SEED=123") {
		t.Fatalf("finding reproduce missing seed: %q", f.Reproduce)
	}

	prog, err := os.ReadFile(filepath.Join(dir, "progress.jsonl"))
	if err != nil || len(bytes.TrimSpace(prog)) == 0 {
		t.Fatalf("progress.jsonl empty or unreadable: %v", err)
	}
	var pl progressLine
	if err := json.Unmarshal(bytes.TrimSpace(prog), &pl); err != nil {
		t.Fatalf("unmarshal progress: %v", err)
	}
	if pl.Checked != 10 || pl.Bugs != 1 || pl.Faults != 7 {
		t.Fatalf("progress fields wrong: %+v", pl)
	}
}

// TestProtectConvertsPanic pins the panic→error boundary: a panicking seed must become a
// recorded message, never crash the worker.
func TestProtectConvertsPanic(t *testing.T) {
	t.Parallel()
	if msg := protect(func() {}); msg != "" {
		t.Fatalf("clean run returned message: %q", msg)
	}
	msg := protect(func() { panic("boom") })
	if !strings.Contains(msg, "boom") || !strings.Contains(msg, "panic:") {
		t.Fatalf("panic not captured: %q", msg)
	}
}
