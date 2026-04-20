package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestKeyspaceResolve_PrintsHex exercises the full command (minus cobra's
// root parsing) by invoking Execute on the subcommand directly with a
// captured stdout. Validates that the bytes match the string-tuple we
// document in the command's Long description.
func TestKeyspaceResolve_PrintsHex(t *testing.T) {
	t.Parallel()
	c := newKeyspaceResolveCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"/myapp/prod/orders"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out.String())
	// Tuple(string("myapp"), string("prod"), string("orders")) packs as:
	//   0x02 myapp   0x00 0x02 prod   0x00 0x02 orders   0x00
	// We only assert the prefix bytes and the segment bytes appear so
	// the test is resilient to any future tuple-layer prefix tweaks.
	for _, want := range []string{"6d79617070", "70726f64", "6f7264657273"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing hex segment %q", got, want)
		}
	}
}

func TestKeyspaceResolve_JSON(t *testing.T) {
	t.Parallel()
	c := newKeyspaceResolveCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"/myapp/prod", "-o", "json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v\nraw:\n%s", err, out.String())
	}
	if obj["path"] != "/myapp/prod" {
		t.Errorf("path = %v; want /myapp/prod", obj["path"])
	}
	hex, _ := obj["prefix_hex"].(string)
	if len(hex) == 0 || !strings.Contains(hex, "6d79617070") {
		t.Errorf("prefix_hex missing 'myapp' bytes: %q", hex)
	}
	if _, ok := obj["prefix_len"].(float64); !ok {
		t.Errorf("prefix_len missing or not a number: %v", obj["prefix_len"])
	}
}

func TestKeyspaceResolve_RejectsEmpty(t *testing.T) {
	t.Parallel()
	c := newKeyspaceResolveCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"/"})
	if err := c.Execute(); err == nil {
		t.Fatal("expected error for empty tuple, got nil")
	}
}
