package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
// returned err: only when no Docker daemon is reachable, and never with CI set
// (.bazelrc forwards CI to tests under --config=ci).
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
	p := probeDocker(ctx, newClient)
	return classifyDockerProbe(p, ctx.Err(), dialDocker)
}

// probe is one probe outcome: the discovery panic, or the ping and its host.
type probe struct {
	panicked any
	pingErr  error
	host     string
}

// probeDocker pings the daemon. Testcontainers' discovery panics, with every
// candidate host's error joined, when none answers; the panic becomes a value.
func probeDocker(ctx context.Context, newClient dockerClientFunc) (p probe) {
	defer func() { p.panicked = recover() }()
	cli, err := newClient(ctx)
	if err != nil {
		return probe{pingErr: err}
	}
	defer cli.Close()
	_, err = cli.Ping(ctx)
	return probe{pingErr: err, host: cli.DaemonHost()}
}

// classifyDockerProbe turns a probe outcome into checkDocker's result: no
// daemon only if every cause is a host that does not exist or refuses a dial.
// A probe that outlived its context is a hung daemon, checked first because the
// Docker client reports a deadline as a connection failure.
func classifyDockerProbe(p probe, ctxErr error, dial func(host string) error) error {
	if p.panicked == nil && p.pingErr == nil {
		return nil
	}
	cause, host := p.pingErr, p.host
	if p.panicked != nil {
		e, ok := p.panicked.(error)
		if !ok {
			return fmt.Errorf("docker probe panicked: %v", p.panicked)
		}
		cause, host = e, ""
	}
	switch {
	case ctxErr != nil:
		return fmt.Errorf("docker probe: daemon did not answer: %w", cause)
	case onlyNoDaemon(cause, host, dial):
		return &DockerUnavailableError{Err: cause}
	case p.panicked != nil:
		return fmt.Errorf("docker probe panicked: %w", cause)
	default:
		return fmt.Errorf("docker probe: %w", cause)
	}
}

// Testcontainers' internal discovery errors for a candidate host that does not
// exist (core.ErrSocketNotFound and the rootless-socket errors).
var (
	noHostTexts  = []string{"socket not found", "rootless Docker not found"}
	noHostPrefix = "checked path: "
)

// cannotConnect starts the Docker client's error for a failed dial, a timeout
// included, so the host is dialed again to tell an absent daemon from a stuck one.
const cannotConnect = "Cannot connect to the Docker daemon at "

// checkHost matches how discovery names the host each joined error is about.
var checkHost = regexp.MustCompile(`^check host ("(?:[^"\\]|\\.)*"): `)

// onlyNoDaemon reports whether every cause in err says no daemon is there: a
// candidate host that does not exist, or one whose dial finds no socket or is
// refused. host is the daemon a ping error is about; discovery's joined errors
// each name their own.
func onlyNoDaemon(err error, host string, dial func(string) error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		for _, e := range errs {
			if !onlyNoDaemon(e, host, dial) {
				return false
			}
		}
		return len(errs) > 0
	}
	text := err.Error()
	if slices.Contains(noHostTexts, text) || strings.HasPrefix(text, noHostPrefix) {
		return true
	}
	if m := checkHost.FindStringSubmatch(text); m != nil {
		h, uerr := strconv.Unquote(m[1])
		if uerr != nil {
			return false
		}
		host = h
	}
	if host == "" || !client.IsErrConnectionFailed(err) || !strings.Contains(text, cannotConnect) {
		return false
	}
	derr := dial(host)
	return errors.Is(derr, syscall.ENOENT) || errors.Is(derr, syscall.ECONNREFUSED)
}

// dialDocker opens and closes a connection to a Docker host given as a URL.
func dialDocker(host string) error {
	u, err := url.Parse(host)
	if err != nil {
		return err
	}
	var network, addr string
	switch u.Scheme {
	case "unix":
		network, addr = "unix", u.Path
	case "tcp":
		network, addr = "tcp", u.Host
	default:
		return fmt.Errorf("docker host %q: scheme %q not dialed", host, u.Scheme)
	}
	conn, err := net.DialTimeout(network, addr, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}
