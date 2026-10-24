package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
)

// ErrDockerUnavailable marks a Run failure caused by there being no reachable
// Docker daemon: no Docker host could be discovered, or the one discovered
// refused the connection. It is the only startup failure a suite may answer
// with a skip; see SkipWithoutDocker.
var ErrDockerUnavailable = errors.New("docker unavailable")

// dockerProbeTimeout bounds the daemon ping. A daemon that does not answer in
// this time is hung, not absent, so the probe reports a failure, not a skip.
const dockerProbeTimeout = 15 * time.Second

// SkipWithoutDocker reports whether a suite may skip its FDB tests after Run
// returned err.
//
// Only a machine with no Docker daemon may skip. Every other startup failure (an
// image pull, a timeout, an OOM-killed container, a failed cleanup) happens
// where Docker runs, and skipping it turns a broken fixture into a green run
// that tested nothing. With CI set in the environment it never skips.
func SkipWithoutDocker(err error) bool {
	return skipWithoutDocker(err, os.Getenv("CI") != "")
}

func skipWithoutDocker(err error, ci bool) bool {
	if err == nil || ci {
		return false
	}
	var terminationErr *TerminationError
	if errors.As(err, &terminationErr) {
		return false
	}
	return errors.Is(err, ErrDockerUnavailable)
}

// checkDocker returns an ErrDockerUnavailable error when no Docker daemon is
// reachable, nil when one answers, and any other probe failure as an ordinary
// error.
func checkDocker(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	panicked, err := probeDocker(ctx)
	return classifyDockerProbe(panicked, err, ctx.Err())
}

// probeDocker pings the daemon testcontainers would use. Testcontainers panics
// when it cannot discover any Docker host, so that panic is returned as a value.
func probeDocker(ctx context.Context) (panicked any, err error) {
	defer func() { panicked = recover() }()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	_, err = cli.Ping(ctx)
	return nil, err
}

// classifyDockerProbe turns a probe outcome into checkDocker's result.
//
//   - a discovery panic carrying an error: no Docker host exists, unavailable;
//   - any other panic: a fault, not an absent daemon, so an ordinary error;
//   - a ping that succeeded: nil;
//   - a failed ping after the probe's context ended: a hung daemon, an ordinary
//     error (the Docker client reports a deadline as a connection failure);
//   - a refused or missing socket: unavailable;
//   - anything else: an ordinary error.
func classifyDockerProbe(panicked any, pingErr, ctxErr error) error {
	if panicked != nil {
		if e, ok := panicked.(error); ok {
			return fmt.Errorf("%w: no Docker host found: %w", ErrDockerUnavailable, e)
		}
		return fmt.Errorf("docker probe panicked: %v", panicked)
	}
	if pingErr == nil {
		return nil
	}
	if ctxErr != nil {
		return fmt.Errorf("docker probe: daemon did not answer: %w", pingErr)
	}
	if client.IsErrConnectionFailed(pingErr) {
		return fmt.Errorf("%w: %w", ErrDockerUnavailable, pingErr)
	}
	return fmt.Errorf("docker probe: %w", pingErr)
}
