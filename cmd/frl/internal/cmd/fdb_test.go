package cmd

import (
	"fmt"
	"testing"

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
