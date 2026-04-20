package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// runCompletion drives cobra's built-in `__complete` subcommand on a
// freshly-constructed root tree. Returns the tab-completion candidates
// as a slice (empty on error).
func runCompletion(t *testing.T, args ...string) []string {
	t.Helper()
	root := NewRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	// __complete expects the last argument to be the current word being
	// completed — usually empty string for "give me everything".
	fullArgs := append([]string{"__complete"}, args...)
	root.SetArgs(fullArgs)
	if err := root.Execute(); err != nil {
		t.Fatalf("__complete %v: %v\n%s", args, err, buf.String())
	}
	// Output is one candidate per line plus a trailing ":<directive>"
	// sentinel line. Split on newlines, drop the directive + empty.
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" || strings.HasPrefix(line, ":") ||
			strings.HasPrefix(line, "Completion") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestCompletion_ContextFlagPullsFromConfig(t *testing.T) {
	writeTestConfig(t, "prod")
	got := runCompletion(t, "store", "info", "--context", "")
	want := map[string]bool{"local": true, "prod": true}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2: %v", len(got), got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected context candidate %q", c)
		}
	}
}

func TestCompletion_OutputFlagDefaultsTextJSON(t *testing.T) {
	writeTestConfig(t, "prod")
	got := runCompletion(t, "store", "info", "-o", "")
	want := []string{"text", "json"}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %v", len(got), len(want), got)
	}
	for i, c := range got {
		if c != want[i] {
			t.Errorf("candidate[%d] = %q, want %q", i, c, want[i])
		}
	}
}

func TestCompletion_MetaGetOutputIsYAMLNotText(t *testing.T) {
	writeTestConfig(t, "prod")
	got := runCompletion(t, "meta", "get", "-o", "")
	// meta get is the only command where -o accepts yaml instead of text
	// (proto-to-text doesn't exist, json is default).
	if len(got) != 2 || got[0] != "json" || got[1] != "yaml" {
		t.Errorf("meta get -o completions = %v; want [json yaml]", got)
	}
}
