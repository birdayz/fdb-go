package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestConfig drops a fixed two-context YAML at path + points
// FRL_CONFIG at it. Returns the path for convenience.
func writeTestConfig(t *testing.T, currentContext string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `current_context: ` + currentContext + `
contexts:
  - name: local
    cluster_file: /tmp/local.cluster
    keyspace_path: /dev
  - name: prod
    cluster_file: /etc/fdb/prod.cluster
    keyspace_path: /myapp/prod
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)
	return path
}

func TestConfigCurrentContext_PrintsName(t *testing.T) {
	// Not parallel: t.Setenv forbids Parallel.
	writeTestConfig(t, "prod")
	c := newConfigCurrentContextCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(out.String()) != "prod" {
		t.Errorf("got %q, want prod", strings.TrimSpace(out.String()))
	}
}

func TestConfigCurrentContext_JSON(t *testing.T) {
	writeTestConfig(t, "prod")
	c := newConfigCurrentContextCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var obj map[string]string
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if obj["name"] != "prod" {
		t.Errorf("name = %q, want prod", obj["name"])
	}
}

func TestConfigCurrentContext_ErrorsWhenEmpty(t *testing.T) {
	writeTestConfig(t, "")
	c := newConfigCurrentContextCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error when current_context is empty")
	}
	if !strings.Contains(err.Error(), "no current context set") {
		t.Errorf("error = %v; want mention of 'no current context set'", err)
	}
}

func TestConfigGetContexts_MarksActive(t *testing.T) {
	writeTestConfig(t, "prod")
	c := newConfigGetContextsCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "  local") || !strings.Contains(got, "* prod") {
		t.Errorf("expected '  local' and '* prod' in output:\n%s", got)
	}
	// Preservation of config-file order (local before prod).
	if strings.Index(got, "local") > strings.Index(got, "prod") {
		t.Errorf("contexts not in config-file order:\n%s", got)
	}
}

func TestConfigGetContexts_JSONArray(t *testing.T) {
	writeTestConfig(t, "prod")
	c := newConfigGetContextsCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d:\n%s", len(rows), out.String())
	}
	// Order preserved (local before prod).
	if rows[0]["name"] != "local" || rows[1]["name"] != "prod" {
		t.Errorf("expected [local, prod], got: %v", rows)
	}
	// Active marker.
	if rows[0]["active"] != false || rows[1]["active"] != true {
		t.Errorf("expected prod active, local inactive: %v", rows)
	}
}

func TestConfigInit_CreatesStarter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("FRL_CONFIG", path)

	c := newConfigInitCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s", err, out.String())
	}
	// File must exist, contain the header comment, and be non-empty.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("init wrote empty file")
	}
	if !strings.Contains(string(data), "frl CLI configuration") {
		t.Errorf("starter missing expected header comment:\n%s", data)
	}
	// Stdout hints the next step.
	if !strings.Contains(out.String(), "use-context") {
		t.Errorf("stdout missing next-step hint:\n%s", out.String())
	}
}

// TestConfigInit_OutputIsParseable is the regression guard for the
// "init writes a template that view then can't parse" bug. protoyaml
// is strict about sequence fields — a bare `contexts:` reads as a null
// scalar, which triggers `expected sequence, got scalar` from the very
// next `frl config view`. The template must include `contexts: []` so
// a fresh install survives any subsequent read.
func TestConfigInit_OutputIsParseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("FRL_CONFIG", path)

	if err := newConfigInitCmd().Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	// get-contexts triggers a full Load() of the written file and is thus
	// the minimal smoke test that the template parses at all.
	c := newConfigGetContextsCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("post-init get-contexts: %v\nout:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "no contexts configured") {
		t.Errorf("post-init get-contexts should report empty:\n%s", out.String())
	}
}

// TestConfigView_YAMLRoundTripsSnakeCase verifies `config view` emits
// snake_case keys matching the on-disk YAML format. Without
// UseProtoNames, protoyaml renders `clusterFile` / `keyspacePath` /
// `metaFile` — if an operator pipes `view` output back into
// config.yaml, the loader rejects it with "unknown field". This test
// locks in the round-trippable contract.
func TestConfigView_YAMLRoundTripsSnakeCase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `current_context: prod
contexts:
  - name: prod
    cluster_file: /etc/fdb/prod.cluster
    keyspace_path: /myapp/prod
    metadata:
      meta_file: /etc/myapp/meta.pb
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	c := newConfigViewCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"cluster_file:", "keyspace_path:", "meta_file:"} {
		if !strings.Contains(got, want) {
			t.Errorf("YAML output missing snake_case key %q (got camelCase?):\n%s",
				want, got)
		}
	}
	for _, avoid := range []string{"clusterFile:", "keyspacePath:", "metaFile:"} {
		if strings.Contains(got, avoid) {
			t.Errorf("YAML output contains camelCase key %q — round-trip broken:\n%s",
				avoid, got)
		}
	}
}

// TestConfigView_JSONUsesProtoNames ensures -o json also emits snake_case
// keys so jq / yq pipelines can be swapped 1:1. Regression guard against
// one format drifting from the other.
func TestConfigView_JSONUsesProtoNames(t *testing.T) {
	writeTestConfig(t, "prod")
	c := newConfigViewCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s", err, out.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if _, ok := obj["cluster_file"]; !ok {
		t.Errorf("expected cluster_file key; got: %v", obj)
	}
	if _, ok := obj["clusterFile"]; ok {
		t.Errorf("got camelCase clusterFile — drift from yaml output: %v", obj)
	}
}

func TestConfigInit_RefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("seed existing config: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	c := newConfigInitCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	err := c.Execute()
	if err == nil {
		t.Fatal("expected refusal on existing file")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v; want 'refusing to overwrite'", err)
	}
	// Existing file untouched.
	data, _ := os.ReadFile(path)
	if string(data) != "existing: true\n" {
		t.Errorf("existing file was modified:\n%s", data)
	}
}

func TestConfigInit_ForceOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	c := newConfigInitCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--force"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute with --force: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "frl CLI configuration") {
		t.Errorf("--force didn't replace file contents:\n%s", data)
	}
}

func TestConfigPath_HonoursEnv(t *testing.T) {
	t.Setenv("FRL_CONFIG", "/tmp/explicit.yaml")
	c := newConfigPathCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(out.String()) != "/tmp/explicit.yaml" {
		t.Errorf("got %q, want /tmp/explicit.yaml", strings.TrimSpace(out.String()))
	}
}

func TestConfigUseContext_MissingFileHint(t *testing.T) {
	t.Setenv("FRL_CONFIG", "/tmp/definitely-does-not-exist-"+t.Name()+".yaml")
	c := newConfigUseContextCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"prod"})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error for use-context against an empty/missing config")
	}
	// Error should name the file + suggest editing it (the "no contexts" hint).
	if !strings.Contains(err.Error(), "no contexts configured") {
		t.Errorf("error = %v; want mention of 'no contexts configured'", err)
	}
	if !strings.Contains(err.Error(), "definitely-does-not-exist") {
		t.Errorf("error = %v; want the missing path surfaced", err)
	}
}

func TestConfigUseContext_TypoHintListsAvailable(t *testing.T) {
	writeTestConfig(t, "local")
	c := newConfigUseContextCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"pord"}) // typo of prod
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent context")
	}
	if !strings.Contains(err.Error(), "available contexts") {
		t.Errorf("error = %v; want 'available contexts' suggestion", err)
	}
	// Both known names should be listed.
	if !strings.Contains(err.Error(), "prod") || !strings.Contains(err.Error(), "local") {
		t.Errorf("error = %v; want both 'prod' and 'local' in suggestion", err)
	}
}

// TestConfigView_MissingContextHint locks in the error shape when
// config.yaml is empty / absent. The message must:
//   - name the effective config path so the operator can see where
//     they need to write
//   - suggest `--context <name>` as the escape hatch for CI scripts
//     that don't want to touch the on-disk config
func TestConfigView_MissingContextHint(t *testing.T) {
	t.Setenv("FRL_CONFIG", "/tmp/definitely-not-real-"+t.Name()+".yaml")

	c := newConfigViewCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error on missing config, got nil")
	}
	for _, want := range []string{"definitely-not-real", "--context"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v; missing %q", err, want)
		}
	}
}

// TestConfigView_InvalidOutputRejected smokes the output validator on
// the view command — view accepts yaml|json, anything else should bail
// before touching the config file.
func TestConfigView_InvalidOutputRejected(t *testing.T) {
	writeTestConfig(t, "prod")
	c := newConfigViewCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"-o", "text"}) // view doesn't support text
	err := c.Execute()
	if err == nil {
		t.Fatal("expected error for -o text")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error = %v; want 'invalid --output'", err)
	}
}

// TestConfigUseContext_PersistsAndConfirms — happy path for the
// write side of use-context: switching from "local" to "prod" must
// persist the change (subsequent ResolveContext returns "prod") AND
// emit a confirmation line to stdout so shell scripts can log the
// switch. Before this, only the two error paths had coverage.
func TestConfigUseContext_PersistsAndConfirms(t *testing.T) {
	path := writeTestConfig(t, "local")

	c := newConfigUseContextCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"prod"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Switched to context \"prod\"") {
		t.Errorf("stdout missing confirmation line:\n%s", out.String())
	}

	// Reload from disk and verify persistence — cleanest way is to run
	// current-context and expect "prod".
	cc := newConfigCurrentContextCmd()
	var ccOut bytes.Buffer
	cc.SetOut(&ccOut)
	cc.SetErr(&ccOut)
	if err := cc.Execute(); err != nil {
		t.Fatalf("current-context after switch: %v", err)
	}
	if strings.TrimSpace(ccOut.String()) != "prod" {
		t.Errorf("persisted current_context = %q, want prod\nfile: %s",
			strings.TrimSpace(ccOut.String()), path)
	}
}

func TestConfigGetContexts_Empty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	c := newConfigGetContextsCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "no contexts configured") {
		t.Errorf("expected 'no contexts configured' in output:\n%s", out.String())
	}
}
