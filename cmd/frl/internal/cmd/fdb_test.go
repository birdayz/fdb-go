package cmd

import (
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
