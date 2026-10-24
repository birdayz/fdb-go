package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// refusedPing returns the error the Docker client gives for a socket nobody
// listens on: the error a machine without a Docker daemon produces.
func refusedPing(t *testing.T) error {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+filepath.Join(t.TempDir(), "docker.sock")),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	if err == nil || !client.IsErrConnectionFailed(err) {
		t.Fatalf("ping of a missing socket = %v, want a connection failure", err)
	}
	return err
}

func TestClassifyDockerProbe(t *testing.T) {
	t.Parallel()
	refused := refusedPing(t)
	for _, tc := range []struct {
		name        string
		panicked    any
		pingErr     error
		ctxErr      error
		wantErr     bool
		unavailable bool
	}{
		{name: "daemon answers"},
		{name: "no Docker host discovered", panicked: errors.New("socket not found"), wantErr: true, unavailable: true},
		{name: "non-error panic", panicked: "nil map write", wantErr: true},
		{name: "socket refuses the connection", pingErr: refused, wantErr: true, unavailable: true},
		{name: "daemon did not answer in time", pingErr: refused, ctxErr: context.DeadlineExceeded, wantErr: true},
		{name: "daemon answers with an error", pingErr: errors.New("Error response from daemon: 500"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyDockerProbe(tc.panicked, tc.pingErr, tc.ctxErr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("classifyDockerProbe = %v, want error %t", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrDockerUnavailable); got != tc.unavailable {
				t.Fatalf("errors.Is(%v, ErrDockerUnavailable) = %t, want %t", err, got, tc.unavailable)
			}
		})
	}
}

func TestSkipWithoutDocker(t *testing.T) {
	t.Parallel()
	noDocker := fmt.Errorf("%w: no Docker host found", ErrDockerUnavailable)
	for _, tc := range []struct {
		name string
		err  error
		ci   bool
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{name: "no Docker", err: noDocker, want: true},
		{name: "no Docker, wrapped by the caller", err: fmt.Errorf("start FDB: %w", noDocker), want: true},
		{name: "no Docker in CI", err: noDocker, ci: true, want: false},
		{name: "failed cleanup", err: &TerminationError{ContainerID: "abc", Err: errors.New("daemon gone")}, want: false},
		{name: "failed cleanup wrapping no Docker", err: &TerminationError{ContainerID: "abc", Err: noDocker}, want: false},
		{name: "startup timeout", err: fmt.Errorf("container startup stopped: %w", context.DeadlineExceeded), want: false},
		{name: "image pull failure", err: errors.New("pull foundationdb/foundationdb:7.3.77: toomanyrequests"), want: false},
		{name: "OOM-killed container", err: errors.New("container abc is not running"), want: false},
		{name: "hung daemon", err: classifyDockerProbe(nil, errors.New("deadline"), context.DeadlineExceeded), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := skipWithoutDocker(tc.err, tc.ci); got != tc.want {
				t.Fatalf("skipWithoutDocker(%v, ci=%t) = %t, want %t", tc.err, tc.ci, got, tc.want)
			}
		})
	}
}

// Run must answer a missing daemon with ErrDockerUnavailable before it starts
// any container, not with the testcontainers discovery panic.
func TestCheckDockerOnThisHost(t *testing.T) {
	t.Parallel()
	err := checkDocker(context.Background())
	if err != nil && !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("checkDocker on this host = %v: neither reachable nor absent", err)
	}
}
