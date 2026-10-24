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
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
)

// DockerUnavailableError reports that no Docker daemon is reachable, the one
// startup failure a suite may skip on (see SkipWithoutDocker).
type DockerUnavailableError struct {
	Err error
}

func (e *DockerUnavailableError) Error() string { return "docker unavailable: " + e.Err.Error() }

func (e *DockerUnavailableError) Unwrap() error { return e.Err }

// dockerProbeTimeout bounds the probe; a daemon slower than this is hung, not absent.
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

// dockerClientFunc builds the client the probe pings.
type dockerClientFunc func(context.Context) (*testcontainers.DockerClient, error)

func tcDockerClient(ctx context.Context) (*testcontainers.DockerClient, error) {
	return testcontainers.NewDockerClientWithOpts(ctx)
}

// dockerProber keeps a discovery panic's verdict: testcontainers caches discovery
// per process, so later clients would ping a default host it never chose.
type dockerProber struct {
	newClient dockerClientFunc
	mu        sync.Mutex
	discovery error
}

// tcProber is the prober Run and RunCluster use.
var tcProber = &dockerProber{newClient: tcDockerClient}

// check returns a *DockerUnavailableError when no Docker daemon is reachable,
// nil when one answers, and any other probe failure as an ordinary error.
func (d *dockerProber) check(ctx context.Context) error {
	d.mu.Lock()
	discovery := d.discovery
	d.mu.Unlock()
	if discovery != nil {
		return discovery
	}
	ctx, cancel := context.WithTimeout(ctx, dockerProbeTimeout)
	defer cancel()
	p := probeDocker(ctx, d.newClient)
	err := classifyDockerProbe(p, ctx.Err(), dialDocker)
	if p.panicked != nil {
		d.mu.Lock()
		d.discovery = err
		d.mu.Unlock()
	}
	return err
}

// probe is one probe outcome: the discovery panic, or the ping and its host.
type probe struct {
	panicked any
	pingErr  error
	host     string
}

// probeDocker pings the daemon; testcontainers' discovery panics, with every
// candidate host's error joined, when none answers.
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

// classifyDockerProbe: no daemon only if every cause is a host that does not
// exist or refuses a dial. A probe past its deadline is a hung daemon.
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
	case onlyNoDaemon(cause, host, dial) && p.panicked != nil:
		return &DockerUnavailableError{Err: fmt.Errorf("docker host discovery: %w", cause)}
	case onlyNoDaemon(cause, host, dial):
		return &DockerUnavailableError{Err: fmt.Errorf("ping %s: %w", host, cause)}
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

// cannotConnect is the Docker client's error for any failed dial, timeouts
// included, so the host is dialed again to tell absent from stuck.
const cannotConnect = "Cannot connect to the Docker daemon at "

// checkHost matches how discovery names the host each joined error is about.
var checkHost = regexp.MustCompile(`^check host ("(?:[^"\\]|\\.)*"): `)

// onlyNoDaemon reports whether every cause in err is a host that does not exist
// or whose socket is missing or refuses a dial; host is the pinged daemon's.
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
