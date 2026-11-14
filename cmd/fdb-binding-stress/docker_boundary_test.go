//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	foundationdb "fdb.dev/pkg/testcontainers/foundationdb"
)

type runnerDockerTB interface {
	Helper()
	Skipf(string, ...any)
	Fatalf(string, ...any)
}

func requireRunnerDocker(t runnerDockerTB, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if os.Getenv("FDB_REQUIRE_DOCKER") == "" && foundationdb.SkipWithoutDocker(err) {
		t.Skipf("FDB not available (no Docker): %v", err)
		return
	}
	t.Fatalf("FDB container startup failed: %v", err)
}

func requireRunnerDockerAvailable(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := startSeedFDB(ctx, "foundationdb/foundationdb:7.3.77")
	requireRunnerDocker(t, err)
	if err := foundationdb.Terminate(ctx, container); err != nil {
		t.Fatalf("Docker availability fixture cleanup failed: %v", err)
	}
}

type runnerDockerRecorder struct {
	skips, fatals int
	message       string
}

func (*runnerDockerRecorder) Helper() {}
func (r *runnerDockerRecorder) Skipf(format string, args ...any) {
	r.skips++
	r.message = fmt.Sprintf(format, args...)
}

func (r *runnerDockerRecorder) Fatalf(format string, args ...any) {
	r.fatals++
	r.message = fmt.Sprintf(format, args...)
}

func TestRunnerDockerBoundary(t *testing.T) {
	unavailable := &foundationdb.DockerUnavailableError{Err: errors.New("injected absent daemon")}
	for _, tc := range []struct {
		name, ci, require string
		err               error
		skips, fatals     int
	}{
		{name: "available"},
		{name: "local_absent", err: unavailable, skips: 1},
		{name: "wrapped_local_absent", err: fmt.Errorf("start: %w", unavailable), skips: 1},
		{name: "CI_absent", ci: "true", err: unavailable, fatals: 1},
		{name: "required_absent", require: "1", err: unavailable, fatals: 1},
		{name: "ordinary_start_failure", err: errors.New("injected image failure"), fatals: 1},
		{name: "startup_timeout", err: context.DeadlineExceeded, fatals: 1},
		{name: "cleanup_failure", err: &foundationdb.TerminationError{ContainerID: "fixture", Err: unavailable}, fatals: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CI", tc.ci)
			t.Setenv("FDB_REQUIRE_DOCKER", tc.require)
			recorder := &runnerDockerRecorder{}
			requireRunnerDocker(recorder, tc.err)
			if recorder.skips != tc.skips || recorder.fatals != tc.fatals {
				t.Fatalf("boundary skips=%d fatals=%d message=%q", recorder.skips, recorder.fatals, recorder.message)
			}
			if tc.err != nil && !strings.Contains(recorder.message, tc.err.Error()) {
				t.Fatalf("boundary lost diagnostic: %q", recorder.message)
			}
		})
	}
}
