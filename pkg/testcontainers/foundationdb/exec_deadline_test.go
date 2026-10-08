package foundationdb

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestExecOutputEndsAtTheContextDeadline: testcontainers reads a multiplexed
// exec stream to its end without watching ctx, so an fdbcli that never exits
// (a configure against a cluster that never becomes available) held a
// container start for the package's whole test timeout. A command outliving
// ctx must not outlive the call.
func TestExecOutputEndsAtTheContextDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := Run(ctx, "")
	if err != nil {
		t.Skipf("FDB not available (no Docker): %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = container.Terminate(cleanupCtx)
	}()

	short, shortCancel := context.WithTimeout(ctx, 2*time.Second)
	defer shortCancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := container.execOutput(short, "sleep", "sleep", "600")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a command killed at the deadline reported success")
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Fatalf("returned %v after a 2s deadline", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("execOutput still running 30s past a 2s deadline")
	}

	// A command that finishes in time is unaffected.
	out, err := container.execOutput(ctx, "echo", "echo", "ok")
	if err != nil || out == "" {
		t.Fatalf("echo: %q, %v", out, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a finished command reported the deadline")
	}
}
