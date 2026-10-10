package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	configv1 "fdb.dev/cmd/frl/gen/frl/config/v1"
)

func TestSetContext_AddsAndActivates(t *testing.T) {
	t.Parallel()
	cfg := &configv1.Config{}
	setContext(cfg, "frl-fdb", "/home/u/.frl/frl-fdb.cluster", "/dev")

	if got := cfg.GetCurrentContext(); got != "frl-fdb" {
		t.Fatalf("current_context = %q, want frl-fdb", got)
	}
	if len(cfg.GetContexts()) != 1 {
		t.Fatalf("contexts = %d, want 1", len(cfg.GetContexts()))
	}
	c := cfg.GetContexts()[0]
	if c.GetClusterFile() != "/home/u/.frl/frl-fdb.cluster" || c.GetKeyspacePath() != "/dev" {
		t.Fatalf("context = %+v", c)
	}
}

func TestSetContext_UpdatesExistingNoDup(t *testing.T) {
	t.Parallel()
	cfg := &configv1.Config{
		CurrentContext: "prod",
		Contexts: []*configv1.Context{
			{Name: "prod", ClusterFile: "/etc/fdb/prod.cluster", KeyspacePath: "/app"},
			{Name: "frl-fdb", ClusterFile: "/old.cluster", KeyspacePath: "/old"},
		},
	}
	setContext(cfg, "frl-fdb", "/new.cluster", "/dev")

	if len(cfg.GetContexts()) != 2 {
		t.Fatalf("contexts = %d, want 2 (no dup)", len(cfg.GetContexts()))
	}
	if got := cfg.GetCurrentContext(); got != "frl-fdb" {
		t.Fatalf("current_context = %q, want frl-fdb", got)
	}
	for _, c := range cfg.GetContexts() {
		if c.GetName() == "frl-fdb" {
			if c.GetClusterFile() != "/new.cluster" || c.GetKeyspacePath() != "/dev" {
				t.Fatalf("frl-fdb not updated: %+v", c)
			}
		}
		if c.GetName() == "prod" && c.GetClusterFile() != "/etc/fdb/prod.cluster" {
			t.Fatalf("prod context clobbered: %+v", c)
		}
	}
}

func TestFdbCommandWiring(t *testing.T) {
	t.Parallel()
	root := NewRoot()
	fdb, _, err := root.Find([]string{"fdb"})
	if err != nil || fdb.Name() != "fdb" {
		t.Fatalf("fdb command not wired: %v", err)
	}
	for _, sub := range []string{"up", "down", "status"} {
		if c, _, err := root.Find([]string{"fdb", sub}); err != nil || c.Name() != sub {
			t.Fatalf("fdb %s not wired: %v", sub, err)
		}
	}
}

// Regression (FDB C++ dev review, RFC-174 C4): `configure new` is not
// idempotent — after a half-acknowledged success, fdbcli reports
// "Database already exists" with a nonzero exit on every retry. The
// retry loop must treat that as success, not fail a healthy cluster.
func TestConfigureNewOutcome(t *testing.T) {
	t.Parallel()
	someErr := fmt.Errorf("exit status 1")
	cases := []struct {
		name    string
		output  string
		err     error
		wantNil bool
	}{
		{"clean success", "Database created", nil, true},
		{"already exists is success", "ERROR: Database already exists! To recreate the database, use the configure command with the \"new\" option.", someErr, true},
		{"real failure propagates", "ERROR: Unable to connect to cluster", someErr, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := configureNewOutcome(tc.output, tc.err)
			if (got == nil) != tc.wantNil {
				t.Errorf("configureNewOutcome(%q, %v) = %v; wantNil=%t", tc.output, tc.err, got, tc.wantNil)
			}
		})
	}
}

// The published port must not expose unauthenticated FDB on host LAN interfaces.
func TestFdbRunArgs_LoopbackPublishNotHostNetwork(t *testing.T) {
	t.Parallel()
	args := fdbRunArgs("frl-fdb", "img:tag", 4689)
	want := []string{
		"run", "-d", "--name", "frl-fdb", "--network", "bridge",
		"--publish", "127.0.0.1:4689:4689",
		"--env", "FDB_NETWORKING_MODE=host",
		"--env", "FDB_PORT=4689",
		"--env", "FDB_CLUSTER_FILE_CONTENTS=docker:docker@127.0.0.1:4689",
		"img:tag",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("fdbRunArgs =\n  %q\nwant\n  %q", args, want)
	}
	if got := fdbClusterString(4500); got != "docker:docker@127.0.0.1:4500" {
		t.Fatalf("fdbClusterString(4500) = %q", got)
	}
}

func TestFdbUpRejectsInvalidPort(t *testing.T) {
	t.Parallel()
	for _, port := range []int{-1, 0, 65536} {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			t.Parallel()
			cmd := newFdbUpCmd()
			cmd.SetArgs([]string{"--port", strconv.Itoa(port)})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "port must be between") {
				t.Fatalf("invalid port %d: %v", port, err)
			}
		})
	}
}

func TestFdbCommandsRespectCanceledContext(t *testing.T) {
	t.Parallel()
	for _, subcommand := range []string{"up", "down", "status"} {
		t.Run(subcommand, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			root := NewRoot()
			root.SetArgs([]string{"fdb", subcommand})
			if err := root.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled fdb %s: %v", subcommand, err)
			}
		})
	}
}

func TestRunDockerRespectsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := runDocker(ctx, "version"); !errors.Is(err, context.Canceled) || out != "" {
		t.Fatalf("canceled Docker operation: output=%q, error=%v", out, err)
	}
}

func TestFDBRetryCancellation(t *testing.T) {
	t.Parallel()
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("callback_success_%t", success), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- retry(ctx, 3, time.Hour, func() error {
					cancel()
					if success {
						return nil
					}
					return errors.New("retryable failure")
				})
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled retry: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled retry did not stop its backoff")
			}
		})
	}
}

func TestFDBRetryBudget(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	calls := 0
	if err := retry(ctx, 3, 0, func() error {
		calls++
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
		t.Fatalf("expired retry budget: calls=%d, error=%v", calls, err)
	}
	wantErr := errors.New("retryable failure")
	if err := retry(context.Background(), 3, 0, func() error {
		calls++
		return wantErr
	}); !errors.Is(err, wantErr) || calls != 3 {
		t.Fatalf("attempt budget: calls=%d, error=%v", calls, err)
	}
}

func TestFdbUpRequiresLocalDockerEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		endpoint string
		local    bool
	}{
		{"unix:///var/run/docker.sock", true},
		{"unix:///Users/dev/.docker/run/docker.sock", true},
		{"npipe:////./pipe/docker_engine", true},
		{"ssh://remote", false},
		{"tcp://192.0.2.1:2376", false},
		{"tcp://localhost:2375", false},
		{"", false},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			t.Parallel()
			if err := validateLocalDockerEndpoint(tc.endpoint); (err == nil) != tc.local {
				t.Fatalf("validateLocalDockerEndpoint(%q) = %v; local=%t", tc.endpoint, err, tc.local)
			}
		})
	}
}
