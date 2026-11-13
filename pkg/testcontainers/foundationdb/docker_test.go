package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// socketClient returns a Docker client for a unix socket at path.
func socketClient(t *testing.T, path string) *testcontainers.DockerClient {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost("unix://"+path), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	return &testcontainers.DockerClient{Client: cli}
}

// missingSocket is a socket path nothing listens on: a machine without a daemon.
func missingSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "docker.sock")
}

// refusedPing returns the error the Docker client gives for a socket nobody
// listens on, the error a machine without a Docker daemon produces.
func refusedPing(t *testing.T) error {
	t.Helper()
	cli := socketClient(t, missingSocket(t))
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := cli.Ping(ctx)
	if err == nil || !client.IsErrConnectionFailed(err) {
		t.Fatalf("ping of a missing socket = %v, want a connection failure", err)
	}
	return err
}

// fakeDaemon serves h on a unix socket and returns its path.
func fakeDaemon(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	// A short directory: a unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dkr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return path
}

// discoveryFailure is the value testcontainers panics with when no candidate
// Docker host answers: one wrapped `docker info` failure per host, joined.
func discoveryFailure(failures ...[2]any) error {
	errs := make([]error, len(failures))
	for i, f := range failures {
		errs[i] = fmt.Errorf("check host %q: %w", f[0].(string), fmt.Errorf("docker info: %w", f[1].(error)))
	}
	return errors.Join(errs...)
}

func at(host string, err error) [2]any { return [2]any{host, err} }

// dialAnswers fakes dialDocker with one outcome per host.
func dialAnswers(outcomes map[string]error) func(string) error {
	return func(host string) error {
		if err, ok := outcomes[host]; ok {
			return err
		}
		return fmt.Errorf("unexpected dial of %q", host)
	}
}

func TestClassifyDockerProbe(t *testing.T) {
	t.Parallel()
	refused := refusedPing(t)
	const a, b = "unix:///a.sock", "unix:///b.sock"
	gone := dialAnswers(map[string]error{a: syscall.ENOENT, b: syscall.ECONNREFUSED})
	listening := dialAnswers(map[string]error{a: nil, b: syscall.ECONNREFUSED})
	stuck := dialAnswers(map[string]error{a: syscall.EAGAIN})
	daemon500 := errors.New("Error response from daemon: 500")
	for _, tc := range []struct {
		name        string
		p           probe
		ctxErr      error
		dial        func(string) error
		wantErr     bool
		unavailable bool
	}{
		{name: "daemon answers", p: probe{host: a}, dial: gone},
		{name: "no candidate host", p: probe{panicked: errors.New("socket not found")}, dial: gone, wantErr: true, unavailable: true},
		{name: "every candidate gone", p: probe{panicked: discoveryFailure(at(a, refused), at(b, refused))}, dial: gone, wantErr: true, unavailable: true},
		{name: "refused default socket, no rootless one", p: probe{panicked: errors.Join(
			discoveryFailure(at(a, refused)), errors.New("rootless Docker not found"))}, dial: gone, wantErr: true, unavailable: true},
		{name: "refused default socket, rootless path checked", p: probe{panicked: errors.Join(
			discoveryFailure(at(a, refused)), errors.New("checked path: $XDG_RUNTIME_DIR"))}, dial: gone, wantErr: true, unavailable: true},
		{name: "a candidate still listens", p: probe{panicked: discoveryFailure(at(a, refused), at(b, refused))}, dial: listening, wantErr: true},
		{name: "a candidate is stuck", p: probe{panicked: discoveryFailure(at(a, refused))}, dial: stuck, wantErr: true},
		{name: "a candidate erred otherwise", p: probe{panicked: discoveryFailure(at(a, refused), at(b, daemon500))}, dial: gone, wantErr: true},
		{name: "discovery error without a host", p: probe{panicked: fmt.Errorf("docker info: %w", refused)}, dial: gone, wantErr: true},
		{name: "discovery outlived the probe", p: probe{panicked: discoveryFailure(at(a, refused))}, ctxErr: context.DeadlineExceeded, dial: gone, wantErr: true},
		{name: "non-error panic", p: probe{panicked: "nil map write"}, dial: gone, wantErr: true},
		{name: "ping refused, socket gone", p: probe{pingErr: refused, host: a}, dial: gone, wantErr: true, unavailable: true},
		{name: "ping refused, socket listens", p: probe{pingErr: refused, host: a}, dial: listening, wantErr: true},
		{name: "ping refused, no host known", p: probe{pingErr: refused}, dial: gone, wantErr: true},
		{name: "ping outlived the probe", p: probe{pingErr: refused, host: a}, ctxErr: context.DeadlineExceeded, dial: gone, wantErr: true},
		{name: "daemon answers with an error", p: probe{pingErr: daemon500, host: a}, dial: gone, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyDockerProbe(tc.p, tc.ctxErr, tc.dial)
			if (err != nil) != tc.wantErr {
				t.Fatalf("classifyDockerProbe = %v, want error %t", err, tc.wantErr)
			}
			var unavailable *DockerUnavailableError
			if got := errors.As(err, &unavailable); got != tc.unavailable {
				t.Fatalf("classifyDockerProbe = %v: DockerUnavailableError %t, want %t", err, got, tc.unavailable)
			}
		})
	}
}

func TestDialDocker(t *testing.T) {
	t.Parallel()
	listening := fakeDaemon(t, func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []struct {
		host string
		want error
	}{
		{"unix://" + missingSocket(t), syscall.ENOENT},
		{"unix://" + listening, nil},
		{"tcp://127.0.0.1:1", syscall.ECONNREFUSED},
	} {
		if err := dialDocker(tc.host); !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Errorf("dialDocker(%q) = %v, want %v", tc.host, err, tc.want)
		}
	}
	for _, host := range []string{"npipe:////./pipe/docker_engine", "::bad"} {
		if err := dialDocker(host); err == nil {
			t.Errorf("dialDocker(%q) = nil, want an error", host)
		}
	}
}

// Run's probe end to end, through the client constructor it calls: a fake
// constructor stands in for testcontainers' discovery and the daemon behind it.
func TestRunProbesDockerFirst(t *testing.T) {
	t.Parallel()
	refused := refusedPing(t)
	missing := "unix://" + missingSocket(t)
	hung := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	erroring := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	resetting := resettingSocket(t)
	// Answers 500 should the test run as root, which the mode does not stop.
	denied := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if err := os.Chmod(denied, 0); err != nil {
		t.Fatal(err)
	}
	via := func(path string) dockerClientFunc {
		return func(context.Context) (*testcontainers.DockerClient, error) { return socketClient(t, path), nil }
	}
	for _, tc := range []struct {
		name        string
		newClient   dockerClientFunc
		timeout     time.Duration
		unavailable bool
	}{
		{
			name:        "discovery finds no host",
			newClient:   func(context.Context) (*testcontainers.DockerClient, error) { panic(errors.New("socket not found")) },
			unavailable: true,
		},
		{
			name: "discovery is refused everywhere",
			newClient: func(context.Context) (*testcontainers.DockerClient, error) {
				panic(discoveryFailure(at(missing, refused)))
			},
			unavailable: true,
		},
		{
			name: "discovery outlives the probe",
			newClient: func(ctx context.Context) (*testcontainers.DockerClient, error) {
				<-ctx.Done()
				panic(discoveryFailure(at(missing, refused)))
			},
			timeout: 200 * time.Millisecond,
		},
		{
			name: "runtime panic in discovery",
			newClient: func(context.Context) (*testcontainers.DockerClient, error) {
				var v any = 1
				return nil, fmt.Errorf("unreachable: %s", v.(string))
			},
		},
		{
			name:      "constructor error",
			newClient: func(context.Context) (*testcontainers.DockerClient, error) { return nil, errors.New("bad TLS config") },
		},
		{name: "socket refuses the ping", newClient: via(strings.TrimPrefix(missing, "unix://")), unavailable: true},
		{name: "daemon hangs", newClient: via(hung), timeout: 200 * time.Millisecond},
		{name: "daemon answers with an error", newClient: via(erroring)},
		{name: "daemon resets the connection", newClient: via(resetting)},
		{name: "socket permission denied", newClient: via(denied)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			c, err := run(ctx, &dockerProber{newClient: tc.newClient}, "")
			if c != nil || err == nil {
				t.Fatalf("run = (%v, %v), want no container and an error", c, err)
			}
			var unavailable *DockerUnavailableError
			if got := errors.As(err, &unavailable); got != tc.unavailable {
				t.Fatalf("run = %v: DockerUnavailableError %t, want %t", err, got, tc.unavailable)
			}
			if got := skipWithoutDocker(err, false); got != tc.unavailable {
				t.Fatalf("skipWithoutDocker(%v) = %t, want %t", err, got, tc.unavailable)
			}
		})
	}
}

// resettingSocket accepts connections and closes them at once: a daemon that is
// there but failing.
func resettingSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dkr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "r.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return path
}

func TestCheckDockerAnsweringDaemon(t *testing.T) {
	t.Parallel()
	ok := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Api-Version", "1.43")
		_, _ = w.Write([]byte("OK"))
	})
	prober := &dockerProber{newClient: func(context.Context) (*testcontainers.DockerClient, error) {
		return socketClient(t, ok), nil
	}}
	if err := prober.check(context.Background()); err != nil {
		t.Fatalf("check against an answering daemon = %v", err)
	}
}

// After a discovery panic testcontainers' clients ping a default host discovery
// never chose, so the prober answers later checks with the panic's verdict.
func TestProberKeepsDiscoveryVerdict(t *testing.T) {
	t.Parallel()
	ok := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Api-Version", "1.43")
		_, _ = w.Write([]byte("OK"))
	})
	calls := 0
	prober := &dockerProber{newClient: func(context.Context) (*testcontainers.DockerClient, error) {
		calls++
		if calls == 1 {
			panic(errors.New("socket not found"))
		}
		return socketClient(t, ok), nil
	}}
	first := prober.check(context.Background())
	second := prober.check(context.Background())
	var unavailable *DockerUnavailableError
	if !errors.As(first, &unavailable) || second != first || calls != 1 {
		t.Fatalf("checks = (%v, %v) after %d client builds, want one discovery verdict twice", first, second, calls)
	}
}

func TestSkipWithoutDocker(t *testing.T) {
	t.Parallel()
	noDocker := &DockerUnavailableError{Err: errors.New("socket not found")}
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := skipWithoutDocker(tc.err, tc.ci); got != tc.want {
				t.Fatalf("skipWithoutDocker(%v, ci=%t) = %t, want %t", tc.err, tc.ci, got, tc.want)
			}
		})
	}
}

// The exported decision reads CI from the environment. Not parallel: t.Setenv.
func TestSkipWithoutDockerReadsCI(t *testing.T) {
	noDocker := &DockerUnavailableError{Err: errors.New("socket not found")}
	t.Setenv("CI", "")
	if !SkipWithoutDocker(noDocker) {
		t.Fatal("SkipWithoutDocker = false without CI, want true")
	}
	t.Setenv("CI", "true")
	if SkipWithoutDocker(noDocker) {
		t.Fatal("SkipWithoutDocker = true with CI set, want false")
	}
}

const noDockerChildEnv = "FDB_TC_NO_DOCKER_CHILD"

// testcontainersVersion is the release whose discovery errors noHostTexts and
// checkHost were read from; a bump must re-read internal/core/docker_host.go.
const testcontainersVersion = "github.com/testcontainers/testcontainers-go v0.41.0"

func TestTestcontainersVersionPinned(t *testing.T) {
	t.Parallel()
	gomod, err := os.ReadFile("../../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gomod), "\t"+testcontainersVersion+"\n") {
		t.Fatalf("go.mod no longer requires %s: re-check noHostTexts and checkHost against the new "+
			"release's Docker-host discovery errors, then update testcontainersVersion", testcontainersVersion)
	}
}

// The real Run and RunCluster in a container with no Docker socket: discovery
// panics, and every later call must keep that verdict.
func TestRunWithoutDocker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := tcProber.check(ctx); err != nil {
		if SkipWithoutDocker(err) {
			t.Skipf("no Docker to run the child in: %v", err)
		}
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      defaultImage + ":" + fdbVersion(),
			Entrypoint: []string{"/child", "-test.run=^TestRunWithoutDockerChild$", "-test.v"},
			Env:        map[string]string{noDockerChildEnv: "1", "HOME": "/tmp"},
			Files:      []testcontainers.ContainerFile{{HostFilePath: self, ContainerFilePath: "/child", FileMode: 0o755}},
			WaitingFor: wait.ForExit().WithExitTimeout(time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ctr.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := ctr.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	out, err := io.ReadAll(logs)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExitCode != 0 {
		t.Fatalf("child exited %d:\n%s", state.ExitCode, out)
	}
	for _, want := range []string{"Run 0: unavailable", "Run 1: unavailable", "RunCluster: unavailable"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("child output lacks %q:\n%s", want, out)
		}
	}
}

func TestRunWithoutDockerChild(t *testing.T) {
	if os.Getenv(noDockerChildEnv) == "" {
		t.Skip("runs only as TestRunWithoutDocker's child")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	assertDiscoveryVerdict := func(what string, started bool, err error) {
		t.Helper()
		var unavailable *DockerUnavailableError
		if started || !errors.As(err, &unavailable) || !SkipWithoutDocker(err) ||
			!strings.HasPrefix(err.Error(), "docker unavailable: docker host discovery: ") {
			t.Fatalf("%s: started %t, err %v; want the skippable discovery verdict", what, started, err)
		}
		t.Logf("%s: unavailable: %v", what, err)
	}
	for i := range 2 {
		c, err := Run(ctx, "")
		assertDiscoveryVerdict(fmt.Sprintf("Run %d", i), c != nil, err)
	}
	cl, err := RunCluster(ctx, 1)
	assertDiscoveryVerdict("RunCluster", cl != nil, err)
}
