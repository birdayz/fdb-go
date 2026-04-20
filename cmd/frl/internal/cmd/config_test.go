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
	if !strings.Contains(err.Error(), "current_context is empty") {
		t.Errorf("error = %v; want mention of 'current_context is empty'", err)
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
