package foundationdb

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestIsTransientContainerErr pins the OOM-death signature detection. The recurring CI flake is a Docker
// OOM-kill of the FDB container during InitializeDatabase under --local_test_jobs concurrency, which
// surfaces as "container <id> is not running" from the configure exec; that (and other death signatures)
// must be retryable, while deterministic config errors must fail fast.
func TestIsTransientContainerErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// The exact CI signature observed on #426/#431: configure exec against an OOM-killed container.
		{"container not running (OOM mid-init)", errors.New("initialize database: configure new (attempt 6/6): exec fdbcli: container exec create: Error response from daemon: container abc123 is not running (output: )"), true},
		{"container exited 137", errors.New("wait: container exited with code 137"), true},
		{"failed to start", errors.New("generic container: failed to start"), true},
		{"wrapped transient", errors.New("initialize database: " + "container def456 is not running"), true},
		{"config/option error (deterministic)", errors.New("apply option: bad redundancy mode"), false},
		{"malformed cluster file", errors.New(`malformed cluster file: "garbage"`), false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientContainerErr(tt.err); got != tt.want {
				t.Errorf("isTransientContainerErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestRetryContainerStart pins the retry policy without real Docker (fake attempt + zero backoff): retry a
// transient death, fail fast on a deterministic error, bound to maxAttempts, and stop on ctx cancellation.
func TestRetryContainerStart(t *testing.T) {
	t.Parallel()
	noBackoff := func(int) time.Duration { return 0 }
	transient := errors.New("container xyz is not running")
	permanent := errors.New("apply option: bad config")

	t.Run("retries a transient death then succeeds", func(t *testing.T) {
		t.Parallel()
		calls := 0
		c, err := retryContainerStart(context.Background(), 3, noBackoff, func() (*Container, error) {
			calls++
			if calls < 3 {
				return nil, transient
			}
			return &Container{}, nil
		})
		if err != nil {
			t.Fatalf("expected success after retries, got %v", err)
		}
		if c == nil || calls != 3 {
			t.Fatalf("expected 3 attempts and a container, got calls=%d c=%v", calls, c)
		}
	})

	t.Run("does not retry a deterministic error", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := retryContainerStart(context.Background(), 3, noBackoff, func() (*Container, error) {
			calls++
			return nil, permanent
		})
		if !errors.Is(err, permanent) {
			t.Errorf("expected the permanent error unwrapped, got %v", err)
		}
		if calls != 1 {
			t.Errorf("deterministic error must fail fast (1 attempt), got %d", calls)
		}
	})

	t.Run("fails after maxAttempts of sustained transient death", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := retryContainerStart(context.Background(), 3, noBackoff, func() (*Container, error) {
			calls++
			return nil, transient
		})
		if err == nil {
			t.Fatal("expected failure after exhausting attempts")
		}
		if calls != 3 {
			t.Errorf("expected exactly maxAttempts=3 attempts, got %d", calls)
		}
		if !errors.Is(err, transient) {
			t.Errorf("final error must wrap the last transient error, got %v", err)
		}
	})

	t.Run("stops on ctx cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		// Long backoff so the test can't pass by exhausting attempts; cancellation must short-circuit.
		_, err := retryContainerStart(ctx, 5, func(int) time.Duration { return time.Hour }, func() (*Container, error) {
			calls++
			cancel() // cancel during the attempt; the loop must observe ctx.Err() and stop
			return nil, transient
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		if calls != 1 {
			t.Errorf("expected to stop after 1 attempt on cancellation, got %d", calls)
		}
	})
}
