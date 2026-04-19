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
