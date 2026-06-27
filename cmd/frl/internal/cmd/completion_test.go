package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// writeMetaFileWithIndexes builds a fixture with two VALUE indexes
// (Order$price, Customer$name) so index-name completion has something
// to return. Companion to writeDemoMetaFile (no indexes) / buildDemoMetaData.
func writeMetaFileWithIndexes(t *testing.T) string {
	t.Helper()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	b.AddIndex("Customer", recordlayer.NewIndex("Customer$name", recordlayer.Field("name")))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	path := filepath.Join(t.TempDir(), "meta.pb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := recordlayer.WriteRecordMetaData(md, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

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

func TestCompletion_TypeFlagPullsFromMetadata(t *testing.T) {
	// Build a context pointing at the demo metadata the other tests use.
	md := buildDemoMetaData(t)
	metaPath := writeDemoMetaFile(t, 0)
	_ = md // ensures buildDemoMetaData is called for its side-effect of keeping the fixture fresh

	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`current_context: local
contexts:
  - name: local
    cluster_file: /tmp/fake.cluster
    keyspace_path: /test
    metadata:
      meta_file: %s
`, metaPath)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	got := runCompletion(t, "record", "scan", "--type", "")
	want := map[string]bool{"Order": true, "Customer": true, "TypedRecord": true}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3: %v", len(got), got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected type candidate %q", c)
		}
	}
}

func TestCompletion_TypeFlagSilentOnBadConfig(t *testing.T) {
	t.Setenv("FRL_CONFIG", "/definitely/does/not/exist.yaml")
	got := runCompletion(t, "record", "scan", "--type", "")
	if len(got) != 0 {
		t.Errorf("expected silent empty completions on bad config, got: %v", got)
	}
}

// TestCompletion_IndexDescribeSilentOnBadConfig — the index-name
// completer must return zero candidates silently when metadata can't
// load (bad config, missing file, etc.). Shells print completion errors
// as if they were real candidates, so a visible error string here would
// show up as a weird tab-complete suggestion.
func TestCompletion_IndexDescribeSilentOnBadConfig(t *testing.T) {
	t.Setenv("FRL_CONFIG", "/definitely/does/not/exist.yaml")
	got := runCompletion(t, "index", "describe", "")
	if len(got) != 0 {
		t.Errorf("expected silent empty completions on bad config, got: %v", got)
	}
}

// TestCompletion_MetaTypesDescribeSilentOnBadConfig — sibling of
// TestCompletion_IndexDescribeSilentOnBadConfig for recordTypeNameCompletion.
// Both go through loadMetaForCompletion, and both need to fail silent.
func TestCompletion_MetaTypesDescribeSilentOnBadConfig(t *testing.T) {
	t.Setenv("FRL_CONFIG", "/definitely/does/not/exist.yaml")
	got := runCompletion(t, "meta", "types", "describe", "")
	if len(got) != 0 {
		t.Errorf("expected silent empty completions on bad config, got: %v", got)
	}
}

func TestCompletion_UseContextPositional(t *testing.T) {
	writeTestConfig(t, "prod")
	got := runCompletion(t, "config", "use-context", "")
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

func TestCompletion_IndexDescribePositional(t *testing.T) {
	metaPath := writeMetaFileWithIndexes(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`current_context: local
contexts:
  - name: local
    cluster_file: /tmp/fake
    keyspace_path: /test
    metadata:
      meta_file: %s
`, metaPath)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	got := runCompletion(t, "index", "describe", "")
	want := map[string]bool{"Customer$name": true, "Order$price": true}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2: %v", len(got), got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected index candidate %q", c)
		}
	}
}

func TestCompletion_MetaTypesDescribePositional(t *testing.T) {
	metaPath := writeDemoMetaFile(t, 0)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := fmt.Sprintf(`current_context: local
contexts:
  - name: local
    cluster_file: /tmp/fake
    keyspace_path: /test
    metadata:
      meta_file: %s
`, metaPath)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("FRL_CONFIG", path)

	got := runCompletion(t, "meta", "types", "describe", "")
	want := map[string]bool{"Order": true, "Customer": true, "TypedRecord": true}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3: %v", len(got), got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected type candidate %q", c)
		}
	}
}

// TestCompletion_StoreDumpSubspace exercises the --subspace value
// completer wired on `store dump`. It must offer every known subspace
// label and nothing else — typos at the flag slot silently kill the
// filter, so the tab-complete list is the operator's guardrail.
func TestCompletion_StoreDumpSubspace(t *testing.T) {
	// No config needed — knownSubspaceLabels() is a pure function of the
	// compiled-in subspaceLabel map. Not using writeTestConfig/t.Setenv
	// here means the test can stay t.Parallel-able.
	t.Parallel()
	got := runCompletion(t, "store", "dump", "--subspace", "")
	if len(got) != len(subspaceLabel) {
		t.Fatalf("got %d candidates, want %d (len(subspaceLabel)): %v",
			len(got), len(subspaceLabel), got)
	}
	haveSet := make(map[string]bool, len(got))
	for _, c := range got {
		haveSet[c] = true
	}
	for _, name := range subspaceLabel {
		if !haveSet[name] {
			t.Errorf("completion missing label %q; got %v", name, got)
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
