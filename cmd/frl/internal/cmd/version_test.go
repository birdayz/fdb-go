package cmd

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionCmd_Text(t *testing.T) {
	t.Parallel()
	c := newVersionCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "frl ") {
		t.Errorf("expected output starting with 'frl ', got: %q", got)
	}
}

func TestVersionCmd_Short(t *testing.T) {
	t.Parallel()
	c := newVersionCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--short"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out.String())
	// --short strips the parenthesised suffix and the 'frl ' prefix.
	if strings.Contains(got, " ") || strings.Contains(got, "(") {
		t.Errorf("--short output should be a bare version string, got: %q", got)
	}
	if got == "" {
		t.Errorf("--short output is empty")
	}
}

func TestVersionCmd_JSON(t *testing.T) {
	t.Parallel()
	c := newVersionCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var v versionInfo
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("decode JSON: %v\nraw:\n%s", err, out.String())
	}
	if v.Version == "" {
		t.Errorf("version field empty:\n%s", out.String())
	}
	if v.GoVersion == "" {
		t.Errorf("go_version field empty:\n%s", out.String())
	}
}

// TestRootVersionFlagMatchesSubcommand verifies `frl --version` and
// `frl version` render the same string. Before the fix they diverged:
// the flag path used cobra's default template ("unknown (built from
// source)") while the subcommand used runtime/debug.ReadBuildInfo().
// Two versions of the truth — this test keeps them one.
func TestRootVersionFlagMatchesSubcommand(t *testing.T) {
	t.Parallel()

	runRoot := func(args ...string) string {
		r := NewRoot()
		var buf bytes.Buffer
		r.SetOut(&buf)
		r.SetErr(&buf)
		r.SetArgs(args)
		if err := r.Execute(); err != nil {
			t.Fatalf("root %v: %v\n%s", args, err, buf.String())
		}
		return strings.TrimSpace(buf.String())
	}

	flag := runRoot("--version")
	sub := runRoot("version")
	if flag != sub {
		t.Errorf("flag vs subcommand drift:\n --version → %q\n version   → %q",
			flag, sub)
	}
}

// TestSettingValue_FoundAndMissing exercises both branches of the
// debug.BuildSetting scan helper. The "unknown" fallback is what turns
// into "unknown" in `frl version` when a build-info key the CLI expects
// (GOOS/GOARCH) is absent — important for Bazel-built binaries where
// rules_go strips most settings.
func TestSettingValue_FoundAndMissing(t *testing.T) {
	t.Parallel()
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "linux"},
			{Key: "GOARCH", Value: "amd64"},
		},
	}
	if got := settingValue(info, "GOOS"); got != "linux" {
		t.Errorf("GOOS = %q; want linux", got)
	}
	if got := settingValue(info, "GOARCH"); got != "amd64" {
		t.Errorf("GOARCH = %q; want amd64", got)
	}
	// Missing key → the "unknown" fallback that drives the "unknown"
	// banner text in Bazel-built binaries.
	if got := settingValue(info, "vcs.revision"); got != "unknown" {
		t.Errorf("missing key = %q; want unknown", got)
	}
}

func TestVersionCmd_InvalidOutput(t *testing.T) {
	t.Parallel()
	c := newVersionCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"-o", "yaml"})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error on unsupported --output")
	}
}
