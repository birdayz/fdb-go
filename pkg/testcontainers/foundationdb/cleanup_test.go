package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

type cleanupReporter struct {
	helpers int
	errors  []string
}

func (r *cleanupReporter) Helper() { r.helpers++ }
func (r *cleanupReporter) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

type cleanupContainer struct {
	testcontainers.Container
	id     string
	err    error
	ctx    context.Context
	ctxErr error
	calls  int
}

func (c *cleanupContainer) GetContainerID() string { return c.id }
func (c *cleanupContainer) Terminate(ctx context.Context, _ ...testcontainers.TerminateOption) error {
	c.ctx = ctx
	c.ctxErr = ctx.Err()
	c.calls++
	return c.err
}

func TestTerminateT(t *testing.T) {
	t.Parallel()
	failure := errors.New("Docker stop failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "failure", err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &cleanupContainer{id: "fixture-123", err: tc.err}
			r := &cleanupReporter{}
			TerminateT(r, ctx, &Container{Container: c})
			if c.calls != 1 || c.ctx != ctx {
				t.Fatalf("Terminate calls = %d, context = %v; want one call with original context", c.calls, c.ctx)
			}
			if r.helpers != 1 {
				t.Fatalf("Helper calls = %d, want 1", r.helpers)
			}
			if tc.err == nil {
				if len(r.errors) != 0 {
					t.Fatalf("successful cleanup reported errors: %v", r.errors)
				}
			} else if len(r.errors) != 1 || r.errors[0] != "terminate container fixture-123: Docker stop failed" {
				t.Fatalf("cleanup errors = %v, want container ID and Docker error", r.errors)
			}
		})
	}
}

func TestTeardownUsesFreshTerminateTimeout(t *testing.T) {
	t.Parallel()
	failure := errors.New("Docker removal failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "failure", err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &cleanupContainer{id: "fixture-123", err: tc.err}
			var callerErr, err error
			var start, end time.Time
			t.Run("cleanup", func(t *testing.T) {
				// t.Context is cancelled before Cleanup callbacks run, as in fixture teardown.
				t.Cleanup(func() {
					callerErr = t.Context().Err()
					start = time.Now()
					err = Teardown(&Container{Container: c})
					end = time.Now()
				})
			})
			if callerErr == nil {
				t.Fatal("caller context was still live; the cancelled-caller case did not run")
			}
			if c.calls != 1 || c.ctxErr != nil {
				t.Fatalf("Terminate calls = %d, context error = %v; want one call with a live context", c.calls, c.ctxErr)
			}
			deadline, bounded := c.ctx.Deadline()
			if !bounded || deadline.Before(start.Add(TerminateTimeout)) || deadline.After(end.Add(TerminateTimeout)) || deadline.Sub(start) <= 30*time.Second {
				t.Fatalf("teardown deadline = %v after start (bounded %t), want TerminateTimeout %v, above the 30s Docker removal overran", deadline.Sub(start), bounded, TerminateTimeout)
			}
			if c.ctx.Err() == nil {
				t.Fatal("Teardown did not release its context")
			}
			var terminationErr *TerminationError
			if tc.err == nil {
				if err != nil {
					t.Fatalf("successful teardown = %v", err)
				}
			} else if !errors.As(err, &terminationErr) || terminationErr.ContainerID != "fixture-123" || !errors.Is(err, failure) {
				t.Fatalf("Teardown = %v, want TerminationError for fixture-123 wrapping the removal failure", err)
			}
		})
	}
}

func TestTerminatePreservesError(t *testing.T) {
	t.Parallel()
	failure := errors.New("already removed")
	c := &cleanupContainer{id: "removed-123", err: failure}
	err := Terminate(context.Background(), &Container{Container: c})
	if !errors.Is(err, failure) || err.Error() != "terminate container removed-123: already removed" {
		t.Fatalf("Terminate = %v, want wrapped failure with container ID", err)
	}
}

func TestClusterTerminateReportsAllFailures(t *testing.T) {
	t.Parallel()
	first := errors.New("first stop failed")
	second := errors.New("second stop failed")
	for _, tc := range []struct {
		name string
		errs []error
	}{
		{name: "empty"},
		{name: "success", errs: []error{nil, nil}},
		{name: "failures", errs: []error{first, nil, second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cluster := &Cluster{}
			var containers []*cleanupContainer
			for i, err := range tc.errs {
				c := &cleanupContainer{id: fmt.Sprintf("replica-%d", i), err: err}
				containers = append(containers, c)
				cluster.Replicas = append(cluster.Replicas, &Container{Container: c})
			}
			err := cluster.Terminate(ctx)
			for _, c := range containers {
				if c.calls != 1 || c.ctx != ctx {
					t.Errorf("%s: calls = %d, context = %v; want one call with original context", c.id, c.calls, c.ctx)
				}
			}
			if tc.name != "failures" {
				if err != nil {
					t.Fatalf("successful cleanup = %v", err)
				}
				return
			}
			if !errors.Is(err, first) || !errors.Is(err, second) {
				t.Fatalf("cluster cleanup lost errors: %v", err)
			}
			want := "terminate container replica-0: first stop failed\nterminate container replica-2: second stop failed"
			if err.Error() != want {
				t.Fatalf("cluster cleanup = %q, want %q", err.Error(), want)
			}
			r := &cleanupReporter{}
			TerminateT(r, ctx, cluster)
			want = "terminate container replica-0,replica-1,replica-2: " + want
			if len(r.errors) != 1 || r.errors[0] != want {
				t.Fatalf("cluster reporter = %v, want %q", r.errors, want)
			}
		})
	}
}
