package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
)

// DockerUnavailableError reports that no Docker daemon is reachable: none could
// be discovered, or every one discovered refused the connection. It is the only
// startup failure a suite may answer with a skip; see SkipWithoutDocker.
type DockerUnavailableError struct {
	Err error
}

func (e *DockerUnavailableError) Error() string { return "docker unavailable: " + e.Err.Error() }

func (e *DockerUnavailableError) Unwrap() error { return e.Err }

// dockerProbeTimeout bounds the daemon probe. A daemon that does not answer in
// this time is hung, not absent, so the probe reports a failure, not a skip.
const dockerProbeTimeout = 15 * time.Second

// SkipWithoutDocker reports whether a suite may skip its FDB tests after Run
// returned err.
//
// Only a machine with no Docker daemon may skip. Every other startup failure (an
// image pull, a timeout, an OOM-killed container, a failed cleanup) happens
// where Docker runs, and skipping it turns a broken fixture into a green run
// that tested nothing. With CI set in the environment it never skips; Bazel
// tests see CI because .bazelrc's ci config forwards it.
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
	var unavailable *DockerUnavailableError
	return errors.As(err, &unavailable)
}

// dockerClientFunc builds the client the probe pings. Run passes
// tcDockerClient; tests pass fakes.
type dockerClientFunc func(context.Context) (*testcontainers.DockerClient, error)

// tcDockerClient builds the client testcontainers itself would use.
func tcDockerClient(ctx context.Context) (*testcontainers.DockerClient, error) {
	return testcontainers.NewDockerClientWithOpts(ctx)
}

// checkDocker returns a *DockerUnavailableError when no Docker daemon is
// reachable, nil when one answers, and any other probe failure as an ordinary
// error.
func checkDocker(ctx context.Context, newClient dockerClientFunc) error {
	ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	panicked, err := probeDocker(ctx, newClient)
	return classifyDockerProbe(panicked, err, ctx.Err())
}

// probeDocker pings the daemon. Testcontainers panics, with the error of every
// candidate host it tried, when its Docker-host discovery finds none that
// answers; that panic is returned as a value.
func probeDocker(ctx context.Context, newClient dockerClientFunc) (panicked any, err error) {
	defer func() { panicked = recover() }()
	cli, err := newClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	_, err = cli.Ping(ctx)
	return nil, err
}

// classifyDockerProbe turns a probe outcome into checkDocker's result.
//
//   - the probe's context ended: a hung daemon, an ordinary error, checked first
//     because the Docker client reports a deadline as a connection failure;
//   - a panic whose error says only that no daemon is there: unavailable;
//   - any other panic: a fault, an ordinary error;
//   - a ping that succeeded: nil;
//   - a ping refused or with no socket: unavailable;
//   - any other ping failure: an ordinary error.
func classifyDockerProbe(panicked any, pingErr, ctxErr error) error {
	if panicked == nil && pingErr == nil {
		return nil
	}
	cause := pingErr
	if panicked != nil {
		e, ok := panicked.(error)
		if !ok {
			return fmt.Errorf("docker probe panicked: %v", panicked)
		}
		cause = e
	}
	switch {
	case ctxErr != nil:
		return fmt.Errorf("docker probe: daemon did not answer: %w", cause)
	case onlyNoDaemon(cause):
		return &DockerUnavailableError{Err: cause}
	case panicked != nil:
		return fmt.Errorf("docker probe panicked: %w", cause)
	default:
		return fmt.Errorf("docker probe: %w", cause)
	}
}

// noHostTexts are testcontainers' internal discovery errors that say a candidate
// Docker host does not exist: core.ErrSocketNotFound when there is no candidate
// at all, and the rootless-socket errors ("rootless Docker not found", "checked
// path: ...") it joins in beside the refused default socket.
var noHostTexts = []string{"socket not found", "rootless Docker not found"}

const noHostPrefix = "checked path: "

// onlyNoDaemon reports whether err says nothing but that no Docker daemon is
// there: every joined cause is a refused or missing socket, or a candidate host
// that does not exist.
func onlyNoDaemon(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		for _, e := range errs {
			if !onlyNoDaemon(e) {
				return false
			}
		}
		return len(errs) > 0
	}
	text := err.Error()
	return slices.Contains(noHostTexts, text) || strings.HasPrefix(text, noHostPrefix) ||
		client.IsErrConnectionFailed(err)
}
