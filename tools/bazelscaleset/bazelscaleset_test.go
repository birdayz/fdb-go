package main

import (
	"testing"
	"time"
)

func baseValidConfig() *config {
	return &config{
		url:               "https://github.com/birdayz/fdb-go",
		name:              "gh-runner-fdb",
		runnerGroup:       "default",
		maxRunners:        1,
		minRunners:        0,
		runnerDir:         "/home/runner/actions-runner",
		workBase:          "/mnt/ci-data/bazelwork",
		pollTimeout:       2 * time.Minute,
		jobStartTimeout:   5 * time.Minute,
		appClientID:       "Iv1.deadbeef",
		appInstallationID: 12345,
		appPrivateKey:     "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*config)
		wantErr bool
	}{
		{"valid app creds", func(*config) {}, false},
		{"valid token instead of app", func(c *config) {
			c.appClientID, c.appInstallationID, c.appPrivateKey = "", 0, ""
			c.token = "ghp_token"
		}, false},
		{"empty url", func(c *config) { c.url = "" }, true},
		{"relative url", func(c *config) { c.url = "github.com/x/y" }, true},
		{"empty name", func(c *config) { c.name = "" }, true},
		{"maxRunners zero", func(c *config) { c.maxRunners = 0 }, true},
		{"maxRunners negative", func(c *config) { c.maxRunners = -1 }, true},
		{"maxRunners above one rejected", func(c *config) { c.maxRunners = 2 }, true},
		{"minRunners negative", func(c *config) { c.minRunners = -1 }, true},
		{"minRunners exceeds max", func(c *config) { c.maxRunners, c.minRunners = 1, 2 }, true},
		{"minRunners equals max ok", func(c *config) { c.maxRunners, c.minRunners = 1, 1 }, false},
		{"empty runnerDir", func(c *config) { c.runnerDir = "" }, true},
		{"empty workBase", func(c *config) { c.workBase = "" }, true},
		{"pollTimeout too small", func(c *config) { c.pollTimeout = 30 * time.Second }, true},
		{"pollTimeout at floor ok", func(c *config) { c.pollTimeout = 60 * time.Second }, false},
		{"jobStartTimeout negative", func(c *config) { c.jobStartTimeout = -1 }, true},
		{"jobStartTimeout zero disables ok", func(c *config) { c.jobStartTimeout = 0 }, false},
		{"no creds at all", func(c *config) {
			c.appClientID, c.appInstallationID, c.appPrivateKey, c.token = "", 0, "", ""
		}, true},
		{"partial app creds (missing key) and no token", func(c *config) {
			c.appPrivateKey = ""
		}, true},
		{"partial app creds rescued by token", func(c *config) {
			c.appPrivateKey = ""
			c.token = "ghp_token"
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := baseValidConfig()
			tt.mutate(c)
			err := c.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigLabels(t *testing.T) {
	t.Parallel()

	c := &config{name: "gh-runner-fdb"}
	if got := c.labels(); len(got) != 1 || got[0].Name != "gh-runner-fdb" {
		t.Fatalf("default labels = %+v, want single label from name", got)
	}

	c.labelList = []string{"a", "b"}
	got := c.labels()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("explicit labels = %+v, want [a b]", got)
	}
}

func TestConfigNewClientPrefersApp(t *testing.T) {
	t.Parallel()

	// With both app creds and a token, the App path is taken (preferred).
	c := baseValidConfig()
	c.token = "ghp_token"
	if _, err := c.newClient(); err != nil {
		t.Fatalf("newClient with app creds: %v", err)
	}

	// Token-only also constructs a client.
	c2 := baseValidConfig()
	c2.appClientID, c2.appInstallationID, c2.appPrivateKey = "", 0, ""
	c2.token = "ghp_token"
	if _, err := c2.newClient(); err != nil {
		t.Fatalf("newClient with token: %v", err)
	}
}

func TestEnvHelpers(t *testing.T) {
	// Not parallel: uses t.Setenv.
	if got := envOr("BAZELSCALESET_TEST_UNSET", "def"); got != "def" {
		t.Fatalf("envOr unset = %q, want def", got)
	}
	t.Setenv("BAZELSCALESET_TEST_STR", "val")
	if got := envOr("BAZELSCALESET_TEST_STR", "def"); got != "val" {
		t.Fatalf("envOr set = %q, want val", got)
	}

	if got := envIntOr("BAZELSCALESET_TEST_UNSET_INT", 7); got != 7 {
		t.Fatalf("envIntOr unset = %d, want 7", got)
	}
	t.Setenv("BAZELSCALESET_TEST_INT", "42")
	if got := envIntOr("BAZELSCALESET_TEST_INT", 7); got != 42 {
		t.Fatalf("envIntOr set = %d, want 42", got)
	}
	t.Setenv("BAZELSCALESET_TEST_INT_BAD", "notanint")
	if got := envIntOr("BAZELSCALESET_TEST_INT_BAD", 7); got != 7 {
		t.Fatalf("envIntOr invalid = %d, want fallback 7", got)
	}

	if got := envBoolOr("BAZELSCALESET_TEST_UNSET_BOOL", true); got != true {
		t.Fatalf("envBoolOr unset = %v, want true", got)
	}
	t.Setenv("BAZELSCALESET_TEST_BOOL", "false")
	if got := envBoolOr("BAZELSCALESET_TEST_BOOL", true); got != false {
		t.Fatalf("envBoolOr set = %v, want false", got)
	}

	t.Setenv("BAZELSCALESET_TEST_DUR", "90s")
	if got := envDurOr("BAZELSCALESET_TEST_DUR", time.Minute); got != 90*time.Second {
		t.Fatalf("envDurOr set = %v, want 90s", got)
	}
	t.Setenv("BAZELSCALESET_TEST_DUR_BAD", "nope")
	if got := envDurOr("BAZELSCALESET_TEST_DUR_BAD", time.Minute); got != time.Minute {
		t.Fatalf("envDurOr invalid = %v, want fallback 1m", got)
	}
}

func TestSlotPool(t *testing.T) {
	t.Parallel()

	if _, err := newSlotPool(t.TempDir(), 0); err == nil {
		t.Fatal("newSlotPool(0) should error")
	}

	dir := t.TempDir()
	p, err := newSlotPool(dir, 3)
	if err != nil {
		t.Fatalf("newSlotPool: %v", err)
	}
	if p.size() != 3 {
		t.Fatalf("size = %d, want 3", p.size())
	}

	// Take all three; they must be distinct.
	seen := map[int]bool{}
	var taken []*slot
	for range 3 {
		s := p.take()
		if s == nil {
			t.Fatal("take returned nil while slots free")
		}
		if seen[s.index] {
			t.Fatalf("take returned duplicate slot %d", s.index)
		}
		seen[s.index] = true
		taken = append(taken, s)
	}

	// Pool is now exhausted.
	if p.take() != nil {
		t.Fatal("take should return nil when exhausted")
	}

	// Give one back, take it again.
	p.give(taken[0])
	if s := p.take(); s == nil || s.index != taken[0].index {
		t.Fatalf("after give, take = %v, want slot %d", s, taken[0].index)
	}
}
