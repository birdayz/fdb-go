package foundationdb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
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
func discoveryFailure(causes ...error) error {
	errs := make([]error, len(causes))
	for i, c := range causes {
		errs[i] = fmt.Errorf("check host %q: %w", fmt.Sprintf("unix:///run/%d.sock", i), fmt.Errorf("docker info: %w", c))
	}
	return errors.Join(errs...)
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
		{name: "no candidate host", panicked: errors.New("socket not found"), wantErr: true, unavailable: true},
		{name: "every candidate refused", panicked: discoveryFailure(refused, refused), wantErr: true, unavailable: true},
		{name: "one candidate erred otherwise", panicked: discoveryFailure(refused, errors.New("Error response from daemon: 500")), wantErr: true},
		{name: "discovery outlived the probe", panicked: discoveryFailure(refused), ctxErr: context.DeadlineExceeded, wantErr: true},
		{name: "refused default socket, no rootless one", panicked: errors.Join(
			discoveryFailure(refused), errors.New("rootless Docker not found")), wantErr: true, unavailable: true},
		{name: "refused default socket, rootless path checked", panicked: errors.Join(
			discoveryFailure(refused), errors.New("checked path: $XDG_RUNTIME_DIR")), wantErr: true, unavailable: true},
		{name: "non-error panic", panicked: "nil map write", wantErr: true},
		{name: "socket refuses the connection", pingErr: refused, wantErr: true, unavailable: true},
		{name: "ping outlived the probe", pingErr: refused, ctxErr: context.DeadlineExceeded, wantErr: true},
		{name: "daemon answers with an error", pingErr: errors.New("Error response from daemon: 500"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyDockerProbe(tc.panicked, tc.pingErr, tc.ctxErr)
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

// Run's probe end to end, through the client constructor it calls: a fake
// constructor stands in for testcontainers' discovery and the daemon behind it.
func TestRunProbesDockerFirst(t *testing.T) {
	t.Parallel()
	refused := refusedPing(t)
	missing := missingSocket(t)
	hung := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	erroring := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
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
			name:        "discovery is refused everywhere",
			newClient:   func(context.Context) (*testcontainers.DockerClient, error) { panic(discoveryFailure(refused)) },
			unavailable: true,
		},
		{
			name: "discovery outlives the probe",
			newClient: func(ctx context.Context) (*testcontainers.DockerClient, error) {
				<-ctx.Done()
				panic(discoveryFailure(refused))
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
		{
			name: "socket refuses the ping",
			newClient: func(context.Context) (*testcontainers.DockerClient, error) {
				return socketClient(t, missing), nil
			},
			unavailable: true,
		},
		{
			name: "daemon hangs",
			newClient: func(context.Context) (*testcontainers.DockerClient, error) {
				return socketClient(t, hung), nil
			},
			timeout: 200 * time.Millisecond,
		},
		{
			name: "daemon answers with an error",
			newClient: func(context.Context) (*testcontainers.DockerClient, error) {
				return socketClient(t, erroring), nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			c, err := run(ctx, tc.newClient, "")
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

func TestCheckDockerAnsweringDaemon(t *testing.T) {
	t.Parallel()
	ok := fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Api-Version", "1.43")
		_, _ = w.Write([]byte("OK"))
	})
	err := checkDocker(context.Background(), func(context.Context) (*testcontainers.DockerClient, error) {
		return socketClient(t, ok), nil
	})
	if err != nil {
		t.Fatalf("checkDocker against an answering daemon = %v", err)
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

// The real Run on a host whose Docker socket is masked: testcontainers'
// discovery panics on the first call and the probe's ping is refused on later
// ones, and Run must answer both with a DockerUnavailableError. The child runs
// in a user and mount namespace with /dev/null bound over the socket, so the
// host's daemon stays untouched.
func TestRunWithoutDocker(t *testing.T) {
	t.Parallel()
	const sock = "/var/run/docker.sock"
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("no %s to mask: %v", sock, err)
	}
	if err := exec.Command("unshare", "-r", "-m", "true").Run(); err != nil {
		t.Skipf("unprivileged namespaces unavailable: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-r", "-m", "sh", "-c",
		`mount --bind /dev/null "$1" && exec "$2" -test.run='^TestRunWithoutDockerChild$' -test.v`,
		"sh", sock, self)
	cmd.Env = append(os.Environ(), noDockerChildEnv+"=1", "DOCKER_HOST=", "CI=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	for _, want := range []string{"attempt 0: unavailable", "attempt 1: unavailable"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("child output lacks %q:\n%s", want, out)
		}
	}
}

func TestRunWithoutDockerChild(t *testing.T) {
	if os.Getenv(noDockerChildEnv) == "" {
		t.Skip("runs only as TestRunWithoutDocker's child")
	}
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := Run(ctx, "")
		cancel()
		var unavailable *DockerUnavailableError
		if c != nil || !errors.As(err, &unavailable) || !SkipWithoutDocker(err) {
			t.Fatalf("attempt %d: Run = (%v, %v), want a DockerUnavailableError a suite may skip", i, c, err)
		}
		t.Logf("attempt %d: unavailable: %v", i, err)
	}
}
